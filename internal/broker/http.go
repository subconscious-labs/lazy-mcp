package broker

import (
	"bytes"
	"context"
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
)

var errSessionNotFound = errors.New("session not found")

type httpBackend struct {
	cfg         config.ServerConfig
	log         *logx.Logger
	http        *http.Client
	maxRaw      int
	initTimeout time.Duration

	handshakeMu sync.Mutex // serializes Ensure/doHandshake

	mu                sync.Mutex
	initialized       bool // true after a successful handshake
	sessionID         string
	negotiatedVersion string // set after downstream initialize
	nextID            int64
	notifHandler      NotificationHandler
	dead              bool
	lastToolCall      time.Time
	startedAt         time.Time

	sseCancel context.CancelFunc
}

func newHTTPBackend(cfg config.ServerConfig, maxRaw int, initTimeout time.Duration, log *logx.Logger) *httpBackend {
	if initTimeout <= 0 {
		initTimeout = 10 * time.Second
	}
	return &httpBackend{
		cfg:         cfg,
		log:         log,
		maxRaw:      maxRaw,
		initTimeout: initTimeout,
		http:        &http.Client{Timeout: 0},
	}
}

func (h *httpBackend) SetNotificationHandler(nh NotificationHandler) {
	h.mu.Lock()
	h.notifHandler = nh
	h.mu.Unlock()
}

func (h *httpBackend) Alive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.initialized && !h.dead
}

func (h *httpBackend) LastToolCall() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastToolCall
}

func (h *httpBackend) LastActivity() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastToolCall.After(h.startedAt) {
		return h.lastToolCall
	}
	return h.startedAt
}

// Ensure returns nil if the backend is already initialized. Concurrent
// Ensure calls do exactly one handshake because handshakeMu serializes
// the entire path.
func (h *httpBackend) Ensure(ctx context.Context) error {
	h.handshakeMu.Lock()
	defer h.handshakeMu.Unlock()

	h.mu.Lock()
	ready := h.initialized && !h.dead
	h.mu.Unlock()
	if ready {
		return nil
	}
	return h.doHandshake(ctx)
}

func (h *httpBackend) doHandshake(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.initTimeout)
		defer cancel()
	}

	h.mu.Lock()
	h.initialized = false
	h.sessionID = ""
	h.negotiatedVersion = ""
	h.dead = false
	h.mu.Unlock()

	resp, err := h.requestRaw(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "lazy-mcp", "version": "0.2.2"},
	})
	if err != nil {
		h.mu.Lock()
		h.dead = true
		h.mu.Unlock()
		return fmt.Errorf("initialize: %w", err)
	}

	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(resp, &initRes); err != nil {
		h.mu.Lock()
		h.dead = true
		h.mu.Unlock()
		return fmt.Errorf("decode initialize response: %w", err)
	}

	h.mu.Lock()
	if initRes.ProtocolVersion != "" {
		h.negotiatedVersion = initRes.ProtocolVersion
	}
	h.mu.Unlock()

	if err := h.notify(ctx, "notifications/initialized", nil); err != nil {
		h.mu.Lock()
		h.dead = true
		h.mu.Unlock()
		return fmt.Errorf("initialized notification: %w", err)
	}

	sseCtx, sseCancel := context.WithCancel(context.Background())
	h.mu.Lock()
	prevCancel := h.sseCancel
	h.sseCancel = sseCancel
	h.initialized = true
	h.mu.Unlock()
	if prevCancel != nil {
		prevCancel()
	}

	go h.readSSE(sseCtx)
	h.log.Info("http backend ready", "server", h.cfg.Name)
	return nil
}

// requestRaw sends a single JSON-RPC request and returns the result.
// It does NOT call Ensure; callers either already ensured or are
// inside the handshake itself.
func (h *httpBackend) requestRaw(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := h.nextIDAdd()
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}

	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h.cfg.Headers {
		req.Header.Set(k, v)
	}

	h.mu.Lock()
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	if h.negotiatedVersion != "" {
		req.Header.Set("MCP-Protocol-Version", h.negotiatedVersion)
	}
	h.mu.Unlock()

	res, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if sid := res.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.sessionID = sid
		h.mu.Unlock()
	}

	// A single response body cannot exceed maxRaw; wrap with a
	// limit+1 reader so we can detect an oversized response without
	// buffering the whole thing.
	limited := io.LimitReader(res.Body, int64(h.maxRaw)+1)

	if res.StatusCode == http.StatusNotFound {
		h.mu.Lock()
		hadSession := h.sessionID != ""
		h.sessionID = ""
		h.initialized = false
		h.mu.Unlock()
		raw, _ := io.ReadAll(limited)
		if hadSession {
			return nil, errSessionNotFound
		}
		return nil, fmt.Errorf("http 404: %s", string(raw))
	}
	if res.StatusCode >= 400 {
		raw, _ := io.ReadAll(limited)
		return nil, fmt.Errorf("http %d: %s", res.StatusCode, string(raw))
	}

	ct := res.Header.Get("Content-Type")
	var raw []byte
	if strings.HasPrefix(ct, "application/json") {
		raw, err = io.ReadAll(limited)
		if err != nil {
			return nil, err
		}
	} else if strings.HasPrefix(ct, "text/event-stream") {
		// SSE parser already enforces a per-event limit via
		// bufio.Scanner; pass through the limited reader.
		return h.parseSSEResponse(limited, id)
	} else {
		raw, _ = io.ReadAll(limited)
		return nil, fmt.Errorf("unexpected content-type: %s; body: %s", ct, string(raw))
	}

	if len(raw) > h.maxRaw {
		return nil, fmt.Errorf("response exceeds max_raw_result_bytes (%d)", h.maxRaw)
	}

	return h.parseJSONBytes(raw, id)
}

// request ensures the backend is ready, then sends the request. A
// downstream 404 for a request that carried a session ID means the
// session expired; we clear local state, reinitialize once, and retry.
func (h *httpBackend) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := h.Ensure(ctx); err != nil {
		return nil, err
	}
	res, err := h.requestRaw(ctx, method, params)
	if errors.Is(err, errSessionNotFound) {
		if err := h.Ensure(ctx); err != nil {
			return nil, err
		}
		return h.requestRaw(ctx, method, params)
	}
	return res, err
}

func (h *httpBackend) parseJSONBytes(raw []byte, id int64) (json.RawMessage, error) {
	var resp struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.ID != id {
		return nil, fmt.Errorf("id mismatch: got %d, want %d", resp.ID, id)
	}
	if resp.Error != nil {
		return nil, classifyRPCError(&rpcErrorShape{Code: resp.Error.Code, Message: resp.Error.Message})
	}
	return resp.Result, nil
}

func (h *httpBackend) parseSSEResponse(r io.Reader, id int64) (json.RawMessage, error) {
	s := newSSEScanner(r, h.maxRaw)
	for {
		data, err := s.next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("stream closed before response")
		}
		if err != nil {
			return nil, err
		}
		var resp struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcErrorShape  `json:"error,omitempty"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		if resp.ID != id {
			continue
		}
		if resp.Error != nil {
			return nil, classifyRPCError(resp.Error)
		}
		return resp.Result, nil
	}
}

func (h *httpBackend) notify(ctx context.Context, method string, params any) error {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		msg["params"] = params
	}
	body, _ := json.Marshal(msg)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	h.mu.Lock()
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	headers := h.cfg.Headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if h.negotiatedVersion != "" {
		req.Header.Set("MCP-Protocol-Version", h.negotiatedVersion)
	}
	h.mu.Unlock()

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := h.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("http %d", res.StatusCode)
	}
	return nil
}

func (h *httpBackend) nextIDAdd() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	return h.nextID
}

func (h *httpBackend) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	seen := make(map[string]bool)
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := h.request(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		page, next, err := decodeToolsListPage(res)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			return all, nil
		}
		if seen[next] {
			h.log.Warn("tools/list duplicate cursor, stopping",
				"server", h.cfg.Name, "cursor", next)
			return all, nil
		}
		seen[next] = true
		cursor = next
	}
}

func (h *httpBackend) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	res, err := h.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}
	result, err := decodeCallToolResult(res)
	if err != nil {
		return "", err
	}
	h.mu.Lock()
	h.lastToolCall = time.Now()
	h.mu.Unlock()
	return result, nil
}

func (h *httpBackend) Stop() {
	h.mu.Lock()
	if h.sseCancel != nil {
		h.sseCancel()
		h.sseCancel = nil
	}
	if h.sessionID != "" {
		// Best-effort DELETE with a short timeout and Body.Close.
		// A leaked response body keeps the underlying TCP connection
		// alive (and blocks connection-pool reuse), so always close it.
		req, _ := http.NewRequest(http.MethodDelete, h.cfg.URL, nil)
		req.Header.Set("Mcp-Session-Id", h.sessionID)
		req.Header.Set("MCP-Protocol-Version", h.negotiatedVersion)
		for k, v := range h.cfg.Headers {
			req.Header.Set(k, v)
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req = req.WithContext(ctx)
			res, err := h.http.Do(req)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1024))
			_ = res.Body.Close()
		}()
	}
	h.sessionID = ""
	h.initialized = false
	h.dead = true
	h.mu.Unlock()
	h.log.Info("http backend stopped", "server", h.cfg.Name)
}

func (h *httpBackend) StopIfIdle(ttl time.Duration) bool {
	h.mu.Lock()
	if h.dead || !h.initialized {
		h.mu.Unlock()
		return false
	}
	last := h.lastToolCall
	if last.IsZero() {
		last = h.startedAt
	}
	idle := time.Since(last)
	// activeCalls для httpBackend не считаем; SSE не мешает idle,
	// а в момент вызова StopIfIdle никто извне не держит запрос.
	h.mu.Unlock()

	if idle < ttl {
		return false
	}
	h.Stop()
	return true
}

// readSSE читает фоновый поток нотификаций.
func (h *httpBackend) readSSE(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range h.cfg.Headers {
		req.Header.Set(k, v)
	}
	h.mu.Lock()
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	if h.negotiatedVersion != "" {
		req.Header.Set("MCP-Protocol-Version", h.negotiatedVersion)
	}
	h.mu.Unlock()

	res, err := h.http.Do(req)
	if err != nil {
		h.log.Warn("sse connect failed", "server", h.cfg.Name, "err", err)
		return
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		h.log.Warn("sse unexpected status", "server", h.cfg.Name, "code", res.StatusCode)
		return
	}

	s := newSSEScanner(res.Body, h.maxRaw)
	for {
		data, err := s.next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			h.log.Warn("sse read failed", "server", h.cfg.Name, "err", err)
			return
		}
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if !strings.HasPrefix(msg.Method, "notifications/") {
			continue
		}
		h.mu.Lock()
		nh := h.notifHandler
		h.mu.Unlock()
		if nh != nil {
			nh(h.cfg.Name, msg.Method, msg.Params)
		}
	}
}
