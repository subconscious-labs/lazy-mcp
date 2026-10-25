// Package hub serves MCP over HTTP (POST/GET/DELETE /mcp) with
// per-session auth and SSE-based delivery of downstream notifications.
package hub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/mcp"
	"github.com/subconscious-labs/lazy-mcp/internal/observability"
	"github.com/subconscious-labs/lazy-mcp/internal/session"
)

// Hub is the HTTP MCP frontend.
type Hub struct {
	cfg *config.Holder
	mcp *mcp.Server
	log *logx.Logger

	httpSrv *http.Server

	mu       sync.RWMutex
	sessions map[string]*sessState
}

type sessState struct {
	sess    *session.Session
	notifCh chan []byte
	done    chan struct{}
	once    sync.Once
}

func (s *sessState) close() {
	s.once.Do(func() { close(s.done) })
}

func (h *Hub) metricsAuthFailures() {
	observability.Default().IncAuthFailures()
}

// New builds a Hub. It binds to cfg.Proxy.HTTP.Addr on Run.
func New(cfg *config.Holder, srv *mcp.Server, log *logx.Logger) *Hub {
	h := &Hub{
		cfg:      cfg,
		mcp:      srv,
		log:      log,
		sessions: make(map[string]*sessState),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", h.handleHealth)
	mux.Handle("/mcp", h.authMiddleware(http.HandlerFunc(h.handleMCP)))
	h.httpSrv = &http.Server{
		Addr:              cfg.Get().Proxy.HTTP.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return h
}

// Run blocks until ctx is cancelled or the server fails.
func (h *Hub) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		h.log.Info("hub listening", "addr", h.httpSrv.Addr)
		err := h.httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	go h.reapLoop(ctx)

	select {
	case <-ctx.Done():
		return h.shutdown()
	case err := <-errCh:
		_ = h.shutdown()
		return err
	}
}

func (h *Hub) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.httpSrv.Shutdown(ctx)

	h.mu.Lock()
	states := make([]*sessState, 0, len(h.sessions))
	for _, st := range h.sessions {
		states = append(states, st)
	}
	h.sessions = make(map[string]*sessState)
	h.mu.Unlock()

	for _, st := range states {
		st.close()
	}
	return nil
}

func (h *Hub) reapLoop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			ttl := h.cfg.Get().Proxy.HTTP.SessionTTL
			if ttl <= 0 {
				continue
			}
			cutoff := time.Now().Add(-ttl)
			h.mu.Lock()
			var dead []*sessState
			for id, st := range h.sessions {
				if st.sess.LastSeen().Before(cutoff) {
					delete(h.sessions, id)
					dead = append(dead, st)
				}
			}
			active := len(h.sessions)
			h.mu.Unlock()
			observability.Default().SetSessionsActive(active)
			for _, st := range dead {
				h.log.Info("hub: reaping session", "id", st.sess.ID, "user", st.sess.User)
				st.close()
			}
		}
	}
}

// ---- HTTP handlers ----

func (h *Hub) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (h *Hub) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !h.originOK(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.handleSSE(w, r)
	case http.MethodPost:
		h.handlePost(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) handlePost(w http.ResponseWriter, r *http.Request) {
	sess, st := h.sessionForPost(w, r)
	if sess == nil {
		return
	}
	defer sess.Touch()
	st.sess.Touch()

	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	ctx := session.WithContext(r.Context(), sess)
	resp, err := h.mcp.Handle(ctx, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if resp == nil {
		// Notification: no response body.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Hub) handleSSE(w http.ResponseWriter, r *http.Request) {
	st := h.sessionForPrincipal(w, r)
	if st == nil {
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keep := time.NewTicker(30 * time.Second)
	defer keep.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-st.done:
			return
		case msg := <-st.notifCh:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			flusher.Flush()
			st.sess.Touch()
		case <-keep.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *Hub) handleDelete(w http.ResponseWriter, r *http.Request) {
	st := h.sessionForPrincipal(w, r)
	if st == nil {
		return
	}

	h.mu.Lock()
	delete(h.sessions, st.sess.ID)
	active := len(h.sessions)
	h.mu.Unlock()
	observability.Default().SetSessionsActive(active)

	st.close()
	w.WriteHeader(http.StatusNoContent)
}

// sessionForPrincipal returns the session identified by Mcp-Session-Id
// only if it belongs to the currently authenticated principal. Returns
// nil and writes an HTTP error otherwise.
//
// A session ID is an addressing token, not an authentication token:
// knowing it must not grant someone else's role.
func (h *Hub) sessionForPrincipal(w http.ResponseWriter, r *http.Request) *sessState {
	id := r.Header.Get("Mcp-Session-Id")
	if id == "" {
		http.Error(w, "Mcp-Session-Id required", http.StatusBadRequest)
		return nil
	}
	u := userFromContext(r.Context())
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil
	}

	h.mu.RLock()
	st := h.sessions[id]
	h.mu.RUnlock()
	if st == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return nil
	}

	// Bind session to the authenticated principal.
	if st.sess.User != u.Name {
		// Don't leak existence: 404 for someone else's session.
		http.Error(w, "session not found", http.StatusNotFound)
		return nil
	}
	return st
}

// sessionForPost returns the existing session from Mcp-Session-Id or
// creates a new one and echoes the id back in the response header.
func (h *Hub) sessionForPost(w http.ResponseWriter, r *http.Request) (*session.Session, *sessState) {
	if id := r.Header.Get("Mcp-Session-Id"); id != "" {
		st := h.sessionForPrincipal(w, r)
		if st == nil {
			return nil, nil
		}
		return st.sess, st
	}

	u := userFromContext(r.Context())
	role := "agent"
	user := "anonymous"
	var allowed []string
	if u != nil {
		user = u.Name
		role = u.Role
		allowed = u.AllowedServers
	}

	sess := session.New(newSessionID(), user, role, allowed)
	st := &sessState{
		sess:    sess,
		notifCh: make(chan []byte, 64),
		done:    make(chan struct{}),
	}
	h.mu.Lock()
	h.sessions[sess.ID] = st
	active := len(h.sessions)
	h.mu.Unlock()

	obs := observability.Default()
	obs.IncSessionCreated()
	obs.SetSessionsActive(active)

	w.Header().Set("Mcp-Session-Id", sess.ID)
	h.log.Info("hub: new session", "id", sess.ID, "user", user, "role", role)
	return sess, st
}

func (h *Hub) originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client
	}
	for _, a := range h.cfg.Get().Proxy.HTTP.AllowedOrigins {
		if strings.HasSuffix(a, "*") {
			if strings.HasPrefix(origin, strings.TrimSuffix(a, "*")) {
				return true
			}
			continue
		}
		if a == origin {
			return true
		}
	}
	return false
}

// ---- Downstream notifications → SSE ----

// OnDownstreamNotification is the callback installed on the broker. It
// maps MCP notifications from downstream servers into client-facing
// notifications/message events with logger="event" and
// data={source, payload}. Downstreams differ in what they put in
// params.data (object, string, nested), so the payload is forwarded
// verbatim; the client is expected to normalise.
func (h *Hub) OnDownstreamNotification(server, method string, params json.RawMessage) {
	switch method {
	case "notifications/message":
		var p struct {
			Level string          `json:"level"`
			Data  json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			h.log.Warn("hub: bad notification/message", "server", server, "err", err)
			return
		}
		if p.Level == "" {
			p.Level = "info"
		}
		var payload any
		if len(p.Data) > 0 {
			payload = p.Data
		}
		h.broadcastNotification(p.Level, "event", server, map[string]any{
			"source":  server,
			"payload": payload,
		})

	case "notifications/tools/list_changed":
		h.broadcastNotification("info", "event", server, map[string]any{
			"source":  server,
			"payload": map[string]any{"event": "catalog_changed"},
		})

	case "notifications/resources/updated":
		h.broadcastNotification("info", "event", server, map[string]any{
			"source":  server,
			"payload": map[string]any{"event": "resource_updated"},
		})

	default:
		h.log.Debug("hub: unknown downstream notification", "server", server, "method", method)
	}
}

// broadcastNotification sends a notifications/message event to all SSE
// subscribers. `data` may be any JSON-marshalable value (object, string,
// json.RawMessage); nil becomes JSON null.
func (h *Hub) broadcastNotification(level, logger, source string, data any) {
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/message",
		"params": map[string]any{
			"level":  level,
			"logger": logger,
			"data":   data,
		},
	})
	if err != nil {
		h.log.Warn("hub: marshal notification", "err", err)
		return
	}
	h.log.Info("hub: notification", "logger", logger, "bytes", len(msg))
	h.broadcast(source, msg)
}

func (h *Hub) broadcast(source string, msg []byte) {
	h.mu.RLock()
	states := make([]*sessState, 0, len(h.sessions))
	for _, st := range h.sessions {
		// Filter out sessions that don't have access to the source server.
		if source != "" && !st.sess.CanSeeServer(source) {
			continue
		}
		states = append(states, st)
	}
	h.mu.RUnlock()

	if len(states) == 0 {
		h.log.Debug("hub: notification has no eligible subscribers", "source", source)
		return
	}
	observability.Default().IncNotificationsSent()

	for _, st := range states {
		select {
		case <-st.done:
			continue
		default:
		}
		select {
		case st.notifCh <- msg:
		case <-st.done:
		default:
			// Slow consumer: drop this notification.
			h.log.Warn("hub: dropping notification for slow session", "id", st.sess.ID)
		}
	}
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
