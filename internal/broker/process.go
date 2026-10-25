package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/observability"
)

// ErrToolError indicates the tool itself returned isError:true.
// The backend is healthy — this is a business-level result, not a
// transport failure.
var ErrToolError = errors.New("tool returned error")

// ErrRPCClientError indicates the backend rejected the request itself
// (parse error, invalid params, unknown method). The backend is healthy;
// this is a caller-side mistake, not a transport failure.
var ErrRPCClientError = errors.New("rpc client error")

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// NotificationHandler receives MCP server→client notifications from a
// downstream backend. method is e.g. "notifications/message".
type NotificationHandler func(server, method string, params json.RawMessage)

type backendProcess struct {
	cfg config.ServerConfig
	log *logx.Logger

	// generation is incremented on every successful start.
	generation int

	lifecycleMu sync.Mutex // serializes start/stop
	mu          sync.Mutex // guards pending, scanErr, ready, proc, lastUsed
	writeMu     sync.Mutex // guards stdin, enc

	proc  *exec.Cmd
	stdin io.WriteCloser
	enc   *json.Encoder

	pending     map[int64]chan *rpcMessage
	scanErr     error
	ready       bool
	activeCalls int

	nextID atomic.Int64

	lastToolCall time.Time
	startedAt    time.Time

	notifHandler NotificationHandler // guarded by mu
}

func newProcessBackend(cfg config.ServerConfig, log *logx.Logger) *backendProcess {
	return &backendProcess{cfg: cfg, log: log}
}

// Ensure starts the process if it's not running, or restarts it if it died.
// Safe to call concurrently; serialized by lifecycleMu.
func (p *backendProcess) Ensure(ctx context.Context) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	alive := p.ready && p.scanErr == nil
	p.mu.Unlock()
	if alive {
		return nil
	}

	// dead or never started — clean up any leftovers and start fresh
	p.mu.Lock()
	gen := p.generation
	p.mu.Unlock()
	p.killGenLocked(gen)
	return p.start(ctx)
}

func (p *backendProcess) start(ctx context.Context) error {
	cmd := exec.Command(p.cfg.Command, p.cfg.Args...)
	cmd.Env = buildEnv(p.cfg)
	cmd.Stderr = os.Stderr // backend stderr flows through

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec %q: %w", p.cfg.Command, err)
	}
	observability.IncBackendStart(p.cfg.Name)

	p.writeMu.Lock()
	p.stdin = stdin
	p.enc = json.NewEncoder(stdin)
	p.writeMu.Unlock()

	p.mu.Lock()
	p.proc = cmd
	p.pending = make(map[int64]chan *rpcMessage)
	p.scanErr = nil
	p.startedAt = time.Now()
	p.generation++
	// NOTE: lastToolCall is intentionally NOT touched here — it must
	// reflect tools/call only, not process starts. Idle-reaper uses
	// LastActivity() which takes max(lastToolCall, startedAt).
	p.mu.Unlock()

	p.log.Info("backend started", "server", p.cfg.Name, "pid", cmd.Process.Pid)

	// Single reader goroutine demuxes responses by id.
	go p.readLoop(stdout)

	// MCP initialize handshake. The caller (Broker.Ensure) bounds ctx with
	// proxy.initialize_timeout; we do not impose our own limit here so the
	// timeout is uniform across the whole startup path.
	if _, err := p.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "lazy-mcp",
			"version": "0.1.0",
		},
	}); err != nil {
		p.killGenLocked(p.generation)
		return fmt.Errorf("initialize: %w", err)
	}

	if err := p.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	}); err != nil {
		p.killGenLocked(p.generation)
		return fmt.Errorf("initialized notification: %w", err)
	}

	p.mu.Lock()
	p.ready = true
	p.mu.Unlock()

	p.log.Info("backend ready", "server", p.cfg.Name)
	return nil
}

// readLoop reads JSON-RPC frames from stdout and dispatches them to waiters.
func (p *backendProcess) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // tolerate large schemas

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // skip non-JSON / debug lines
		}
		if msg.ID == nil {
			if msg.Method != "" {
				p.dispatchNotification(msg.Method, msg.Params)
			}
			continue // notification: no waiter
		}
		p.mu.Lock()
		ch, ok := p.pending[*msg.ID]
		if ok {
			delete(p.pending, *msg.ID)
		}
		p.mu.Unlock()

		if ok {
			ch <- &msg // buffered (cap 1), never blocks
		}
	}

	err := sc.Err()
	if err == nil {
		err = io.EOF
	}

	p.mu.Lock()
	p.scanErr = err
	p.ready = false
	for id, ch := range p.pending {
		delete(p.pending, id)
		close(ch)
	}
	p.mu.Unlock()

	p.log.Warn("backend stream closed", "server", p.cfg.Name, "err", err)
}

// request sends a JSON-RPC request and waits for the matching response or ctx.
func (p *backendProcess) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	ch := make(chan *rpcMessage, 1)

	p.mu.Lock()
	if p.scanErr != nil {
		err := p.scanErr
		p.mu.Unlock()
		return nil, fmt.Errorf("backend dead: %w", err)
	}
	p.pending[id] = ch
	p.activeCalls++
	gen := p.generation
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.activeCalls--
		p.mu.Unlock()
	}()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- p.write(req)
	}()

	select {
	case err := <-writeErr:
		if err != nil {
			p.mu.Lock()
			delete(p.pending, id)
			p.mu.Unlock()
			return nil, err
		}
	case <-ctx.Done():
		// Write is blocked. Kill the process to unblock it and clean up.
		go p.killGen(gen)
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, ctx.Err()
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("backend closed during request")
		}
		if msg.Error != nil {
			if msg.Error.Code >= -32700 && msg.Error.Code <= -32602 {
				return nil, fmt.Errorf("%w: code %d: %s",
					ErrRPCClientError, msg.Error.Code, msg.Error.Message)
			}
			return nil, fmt.Errorf("rpc error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		return msg.Result, nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *backendProcess) write(v any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.enc == nil {
		return fmt.Errorf("backend not started")
	}
	return p.enc.Encode(v)
}

// ListTools calls tools/list on the backend.
func (p *backendProcess) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var allTools []ToolInfo
	cursor := ""
	seenCursors := make(map[string]bool)

	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}

		res, err := p.request(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}

		type rawTool struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
			Annotations ToolAnnotations `json:"annotations,omitempty"`
		}
		type listResult struct {
			Tools      []rawTool `json:"tools"`
			NextCursor string    `json:"nextCursor,omitempty"`
		}
		var lr listResult
		if err := json.Unmarshal(res, &lr); err != nil {
			return nil, fmt.Errorf("parse tools/list: %w", err)
		}

		for _, t := range lr.Tools {
			allTools = append(allTools, ToolInfo(t))
		}

		if lr.NextCursor == "" {
			break // конец пагинации
		}

		// Защита от бесконечного цикла, если сервер зациклит курсор
		if seenCursors[lr.NextCursor] {
			p.log.Warn("tools/list returned duplicate cursor, stopping pagination",
				"server", p.cfg.Name, "cursor", lr.NextCursor)
			break
		}
		seenCursors[lr.NextCursor] = true
		cursor = lr.NextCursor
	}

	return allTools, nil
}

// CallTool calls tools/call on the backend.
func (p *backendProcess) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	p.mu.Lock()
	p.lastToolCall = time.Now()
	p.mu.Unlock()
	res, err := p.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return "", err
	}

	type contentItem struct {
		Type     string          `json:"type"`
		Text     string          `json:"text,omitempty"`
		MimeType string          `json:"mimeType,omitempty"`
		Data     string          `json:"data,omitempty"`
		Resource json.RawMessage `json:"resource,omitempty"`
	}
	type callResult struct {
		Content           []contentItem   `json:"content"`
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	}
	var cr callResult
	if err := json.Unmarshal(res, &cr); err != nil {
		return "", fmt.Errorf("parse tools/call result: %w", err)
	}

	if cr.IsError {
		var parts []string
		for _, c := range cr.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
		return "", fmt.Errorf("%w: %s", ErrToolError, strings.Join(parts, "\n"))
	}

	// If the server provided structured content, prefer it: it is
	// machine-readable and preserves the full MCP result shape.
	if len(cr.StructuredContent) > 0 && string(cr.StructuredContent) != "null" {
		return string(cr.StructuredContent), nil
	}

	// Otherwise, join text blocks. Non-text blocks (image, audio,
	// resource) are reported explicitly so the caller knows data is
	// missing rather than receiving a silently truncated result.
	var parts []string
	var nonText []string
	for _, c := range cr.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "audio":
			nonText = append(nonText, fmt.Sprintf("[%s content omitted: mimeType=%s]", c.Type, c.MimeType))
		case "resource":
			nonText = append(nonText, "[resource content omitted]")
		default:
			nonText = append(nonText, fmt.Sprintf("[%s content omitted]", c.Type))
		}
	}
	if len(nonText) > 0 {
		parts = append(parts, nonText...)
	}
	result := strings.Join(parts, "\n")
	return result, nil
}

// Alive reports whether the backend is currently healthy.
func (p *backendProcess) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ready && p.scanErr == nil
}

// StopIfIdle stops the backend if it hasn't been used for at least ttl.
// Returns true if the backend was stopped. ttl <= 0 is a no-op.
func (p *backendProcess) StopIfIdle(ttl time.Duration) bool {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	// Если есть незавершенные вызовы, процесс не считается idle.
	if !p.ready || p.scanErr != nil || p.activeCalls > 0 {
		p.mu.Unlock()
		return false
	}
	idle := time.Since(p.lastToolCall)
	if idle < ttl {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()

	p.killGenLocked(p.generation)
	return true
}

// LastToolCall returns the time of the last tools/call sent to this backend.
// Catalog refreshes (tools/list) do NOT count.
func (p *backendProcess) LastToolCall() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastToolCall
}

// LastActivity returns the most recent of startedAt and lastToolCall.
// A freshly-started process that has never been called counts as active
// since startedAt, so the reaper does not kill it immediately.
func (p *backendProcess) LastActivity() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastToolCall.After(p.startedAt) {
		return p.lastToolCall
	}
	return p.startedAt
}

// killGen cleans up a specific generation. It holds lifecycleMu
// to serialize with Ensure/start. If the generation has already
// advanced, it does nothing.
func (p *backendProcess) killGen(gen int) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.killGenLocked(gen)
}

// killGenLocked is killGen without acquiring lifecycleMu.
func (p *backendProcess) killGenLocked(gen int) {
	p.mu.Lock()
	if p.generation != gen {
		p.mu.Unlock()
		return
	}

	stdin := p.stdin
	proc := p.proc
	p.proc = nil
	p.stdin = nil
	p.ready = false
	if p.scanErr == nil {
		p.scanErr = fmt.Errorf("stopped")
	}
	for id, ch := range p.pending {
		delete(p.pending, id)
		close(ch)
	}
	p.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}

	p.writeMu.Lock()
	if p.generation == gen {
		p.enc = nil
	}
	p.writeMu.Unlock()

	if proc != nil && proc.Process != nil {
		_ = proc.Process.Kill()
		_ = proc.Wait()
	}
}

// Stop terminates the backend.
func (p *backendProcess) Stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	gen := p.generation
	p.mu.Unlock()

	p.killGenLocked(gen)
	p.log.Info("backend stopped", "server", p.cfg.Name)
}

func (p *backendProcess) SetNotificationHandler(h NotificationHandler) {
	p.mu.Lock()
	p.notifHandler = h
	p.mu.Unlock()
}

func (p *backendProcess) dispatchNotification(method string, params json.RawMessage) {
	p.mu.Lock()
	h := p.notifHandler
	p.mu.Unlock()
	if h == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			p.log.Warn("notification handler panic",
				"server", p.cfg.Name, "method", method, "recover", r)
		}
	}()
	h(p.cfg.Name, method, params)
}

func buildEnv(cfg config.ServerConfig) []string {
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	return env
}
