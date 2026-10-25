package broker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
)

type httpBackend struct {
	cfg  config.ServerConfig
	log  *logx.Logger
	http *http.Client

	mu           sync.Mutex
	sessionID    string
	nextID       int64
	notifHandler NotificationHandler
	dead         bool
	lastToolCall time.Time
	startedAt    time.Time

	sseCancel context.CancelFunc
}

func newHTTPBackend(cfg config.ServerConfig, log *logx.Logger) *httpBackend {
	return &httpBackend{
		cfg: cfg,
		log: log,
		http: &http.Client{
			Timeout: 0, // стрим не должен таймаутиться, таймауты задаются через ctx
		},
		startedAt: time.Now(),
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
	return !h.dead
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

// Ensure инициализирует HTTP-сессию (initialize + запуск SSE).
func (h *httpBackend) Ensure(ctx context.Context) error {
	h.mu.Lock()
	if h.sessionID != "" && !h.dead {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	// 1. Initialize
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	resp, err := h.request(initCtx, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "lazy-mcp", "version": "0.2.2"},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(resp, &initRes); err != nil {
		return fmt.Errorf("decode initialize response: %w", err)
	}

	// 2. Notifications initialized
	if err := h.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}

	// 3. Start SSE loop
	sseCtx, sseCancel := context.WithCancel(context.Background())
	h.mu.Lock()
	h.sseCancel = sseCancel
	h.dead = false
	h.mu.Unlock()

	go h.readSSE(sseCtx)

	h.log.Info("http backend ready", "server", h.cfg.Name)
	return nil
}

// request отправляет JSON-RPC запрос.
func (h *httpBackend) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
	h.mu.Unlock()

	res, err := h.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	// Если сервер вернул новую сессию, запоминаем её.
	if sid := res.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.sessionID = sid
		h.mu.Unlock()
	}

	if res.StatusCode >= 400 {
		raw, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("http %d: %s", res.StatusCode, string(raw))
	}

	// Читаем ответ. Он может быть как application/json, так и text/event-stream.
	ct := res.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		return h.parseJSONResponse(res.Body, id)
	}
	if strings.HasPrefix(ct, "text/event-stream") {
		return h.parseSSEResponse(res.Body, id)
	}

	raw, _ := io.ReadAll(res.Body)
	return nil, fmt.Errorf("unexpected content-type: %s; body: %s", ct, string(raw))
}

func (h *httpBackend) parseJSONResponse(r io.Reader, id int64) (json.RawMessage, error) {
	var resp struct {
		ID     int64           `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, err
	}
	if resp.ID != id {
		return nil, fmt.Errorf("id mismatch: got %d, want %d", resp.ID, id)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

func (h *httpBackend) parseSSEResponse(r io.Reader, id int64) (json.RawMessage, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var resp struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			continue
		}
		if resp.ID == id {
			if resp.Error != nil {
				return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("stream closed before response")
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
	if err := h.Ensure(ctx); err != nil {
		return nil, err
	}

	res, err := h.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}

	type rawTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
		Annotations ToolAnnotations `json:"annotations,omitempty"`
	}
	var lr struct {
		Tools []rawTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &lr); err != nil {
		return nil, fmt.Errorf("parse tools/list: %w", err)
	}

	tools := make([]ToolInfo, 0, len(lr.Tools))
	for _, t := range lr.Tools {
		tools = append(tools, ToolInfo(t))
	}
	return tools, nil
}

func (h *httpBackend) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if err := h.Ensure(ctx); err != nil {
		return "", err
	}

	res, err := h.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}

	type contentItem struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	type callResult struct {
		Content []contentItem `json:"content"`
		IsError bool          `json:"isError"`
	}
	var cr callResult
	if err := json.Unmarshal(res, &cr); err != nil {
		return "", fmt.Errorf("parse tools/call: %w", err)
	}

	parts := make([]string, 0, len(cr.Content))
	for _, c := range cr.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	result := strings.Join(parts, "\n")
	if cr.IsError {
		return "", fmt.Errorf("%w: %s", ErrToolError, result)
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
		// Best-effort DELETE
		req, _ := http.NewRequest(http.MethodDelete, h.cfg.URL, nil)
		req.Header.Set("Mcp-Session-Id", h.sessionID)
		for k, v := range h.cfg.Headers {
			req.Header.Set(k, v)
		}
		go func() {
			_, _ = h.http.Do(req)
		}()
	}
	h.sessionID = ""
	h.dead = true
	h.mu.Unlock()
	h.log.Info("http backend stopped", "server", h.cfg.Name)
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
	h.mu.Unlock()

	res, err := h.http.Do(req)
	if err != nil {
		h.log.Warn("sse connect failed", "server", h.cfg.Name, "err", err)
		return
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		h.log.Warn("sse unexpected status", "server", h.cfg.Name, "code", res.StatusCode)
		return
	}

	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			continue
		}
		if strings.HasPrefix(msg.Method, "notifications/") {
			h.mu.Lock()
			nh := h.notifHandler
			h.mu.Unlock()
			if nh != nil {
				nh(h.cfg.Name, msg.Method, msg.Params)
			}
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		h.log.Warn("sse stream closed", "server", h.cfg.Name, "err", err)
	}
}
