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

	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

// ErrToolError indicates the tool itself returned isError:true.
// The backend is healthy — this is a business-level result, not a
// transport failure.
var ErrToolError = errors.New("tool returned error")

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type backendProcess struct {
	cfg config.ServerConfig
	log *logx.Logger

	lifecycleMu sync.Mutex // serializes start/stop
	mu          sync.Mutex // guards pending, scanErr, ready, proc, lastUsed
	writeMu     sync.Mutex // guards stdin, enc

	proc  *exec.Cmd
	stdin io.WriteCloser
	enc   *json.Encoder

	pending map[int64]chan *rpcMessage
	scanErr error
	ready   bool

	nextID atomic.Int64

	lastUsed time.Time
}

func newBackendProcess(cfg config.ServerConfig, log *logx.Logger) *backendProcess {
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
	p.kill()
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
		return fmt.Errorf("start %q: %w", p.cfg.Command, err)
	}

	p.writeMu.Lock()
	p.stdin = stdin
	p.enc = json.NewEncoder(stdin)
	p.writeMu.Unlock()

	p.mu.Lock()
	p.proc = cmd
	p.pending = make(map[int64]chan *rpcMessage)
	p.scanErr = nil
	p.lastUsed = time.Now()
	p.mu.Unlock()

	p.log.Info("backend started", "server", p.cfg.Name, "pid", cmd.Process.Pid)

	// Single reader goroutine demuxes responses by id.
	go p.readLoop(stdout)

	// MCP initialize handshake, bounded by ctx (caller may add timeout).
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := p.request(initCtx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "lazy-mcp",
			"version": "0.1.0",
		},
	}); err != nil {
		p.kill()
		return fmt.Errorf("initialize: %w", err)
	}

	if err := p.write(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	}); err != nil {
		p.kill()
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
			continue // notification, no waiter
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
	if p.pending == nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("backend not started")
	}
	p.pending[id] = ch
	p.lastUsed = time.Now()
	p.mu.Unlock()

	if err := p.write(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, err
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("backend closed during request")
		}
		if msg.Error != nil {
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
	res, err := p.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}

	type rawTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	type listResult struct {
		Tools []rawTool `json:"tools"`
	}
	var lr listResult
	if err := json.Unmarshal(res, &lr); err != nil {
		return nil, fmt.Errorf("parse tools/list: %w", err)
	}
	tools := make([]ToolInfo, 0, len(lr.Tools))
	for _, t := range lr.Tools {
		tools = append(tools, ToolInfo{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return tools, nil
}

// CallTool calls tools/call on the backend.
func (p *backendProcess) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	res, err := p.request(ctx, "tools/call", map[string]any{
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
		return "", fmt.Errorf("parse tools/call result: %w", err)
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
	if ttl <= 0 {
		return false
	}
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	if !p.ready || p.scanErr != nil {
		p.mu.Unlock()
		return false
	}
	idle := time.Since(p.lastUsed)
	if idle < ttl {
		p.mu.Unlock()
		return false
	}
	p.mu.Unlock()

	p.kill()
	return true
}

// LastUsed returns the time of the last request sent to this backend.
// Used by the idle reaper (TODO 1.5).
func (p *backendProcess) LastUsed() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastUsed
}

// kill forcibly terminates the process. Safe to call multiple times.
func (p *backendProcess) kill() {
	p.writeMu.Lock()
	if p.stdin != nil {
		_ = p.stdin.Close()
		p.stdin = nil
	}
	p.enc = nil
	p.writeMu.Unlock()

	p.mu.Lock()
	proc := p.proc
	p.proc = nil
	p.ready = false
	if p.scanErr == nil {
		p.scanErr = fmt.Errorf("stopped")
	}
	for id, ch := range p.pending {
		delete(p.pending, id)
		close(ch)
	}
	p.mu.Unlock()

	if proc != nil && proc.Process != nil {
		_ = proc.Process.Kill()
		_ = proc.Wait()
	}
}

// Stop terminates the backend.
func (p *backendProcess) Stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.kill()
	p.log.Info("backend stopped", "server", p.cfg.Name)
}

func buildEnv(cfg config.ServerConfig) []string {
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	return env
}
