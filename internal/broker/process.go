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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/observability"
)

var ErrToolError = errors.New("tool returned error")
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

type NotificationHandler func(server, method string, params json.RawMessage)

// processRun holds the per-generation state of one started subprocess.
type processRun struct {
	gen     int
	proc    *exec.Cmd
	stdin   io.WriteCloser
	enc     *json.Encoder
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[int64]chan *rpcMessage
	ready   bool
	scanErr error
	closed  bool
}

func (r *processRun) markClosed() {
	r.mu.Lock()
	r.closed = true
	r.ready = false
	if r.scanErr == nil {
		r.scanErr = fmt.Errorf("run closed")
	}
	for id, ch := range r.pending {
		delete(r.pending, id)
		close(ch)
	}
	r.mu.Unlock()
}

type backendProcess struct {
	cfg    config.ServerConfig
	log    *logx.Logger
	maxRaw int // hard cap for a single inbound frame

	lifecycleMu sync.Mutex
	mu          sync.Mutex
	current     *processRun
	genCounter  atomic.Int64

	nextID       atomic.Int64
	lastToolCall time.Time
	startedAt    time.Time
	activeCalls  int
	notifHandler NotificationHandler
}

func newProcessBackend(cfg config.ServerConfig, maxRaw int, log *logx.Logger) *backendProcess {
	return &backendProcess{cfg: cfg, log: log, maxRaw: maxRaw}
}

func (p *backendProcess) SetNotificationHandler(h NotificationHandler) {
	p.mu.Lock()
	p.notifHandler = h
	p.mu.Unlock()
}

func (p *backendProcess) Ensure(ctx context.Context) error {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	run := p.current
	p.mu.Unlock()

	if run != nil {
		run.mu.Lock()
		alive := run.ready && run.scanErr == nil
		run.mu.Unlock()
		if alive {
			return nil
		}
		p.killGenLocked(run.gen)
	}
	return p.start(ctx)
}

func (p *backendProcess) start(ctx context.Context) error {
	cmd := exec.Command(p.cfg.Command, p.cfg.Args...)
	cmd.Env = buildEnv(p.cfg)
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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

	gen := int(p.genCounter.Add(1))
	run := &processRun{
		gen:     gen,
		proc:    cmd,
		stdin:   stdin,
		enc:     json.NewEncoder(stdin),
		pending: make(map[int64]chan *rpcMessage),
	}

	p.mu.Lock()
	p.current = run
	// NOTE: lastToolCall is intentionally NOT touched here — it must
	// reflect tools/call only, not process starts.
	p.startedAt = time.Now()
	p.mu.Unlock()

	p.log.Info("backend started", "server", p.cfg.Name, "pid", cmd.Process.Pid, "gen", gen)

	go p.readLoop(run, stdout)

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if _, err := p.requestRun(initCtx, run, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "lazy-mcp", "version": "0.1.0"},
	}); err != nil {
		p.killGenLocked(gen)
		return fmt.Errorf("initialize: %w", err)
	}

	if err := p.writeRun(run, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	}); err != nil {
		p.killGenLocked(gen)
		return fmt.Errorf("initialized notification: %w", err)
	}

	run.mu.Lock()
	run.ready = true
	run.mu.Unlock()

	p.log.Info("backend ready", "server", p.cfg.Name, "gen", gen)
	return nil
}

func (p *backendProcess) readLoop(run *processRun, r io.Reader) {
	sc := bufio.NewScanner(r)
	// The buffer must be able to hold a single frame up to maxRaw.
	// bufio requires max >= initial, so we start at maxRaw directly.
	maxFrame := p.maxRaw
	if maxFrame < 64*1024 {
		maxFrame = 64 * 1024
	}
	sc.Buffer(make([]byte, 0, 64*1024), maxFrame)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.ID == nil {
			if msg.Method != "" {
				p.dispatchNotification(msg.Method, msg.Params)
			}
			continue
		}
		run.mu.Lock()
		ch, ok := run.pending[*msg.ID]
		if ok {
			delete(run.pending, *msg.ID)
		}
		run.mu.Unlock()
		if ok {
			ch <- &msg
		}
	}

	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	p.log.Warn("backend stream closed", "server", p.cfg.Name, "gen", run.gen, "err", err)

	run.mu.Lock()
	run.scanErr = err
	run.ready = false
	for id, ch := range run.pending {
		delete(run.pending, id)
		close(ch)
	}
	run.mu.Unlock()
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

func (p *backendProcess) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	run := p.current
	p.mu.Unlock()
	if run == nil {
		return nil, fmt.Errorf("backend not started")
	}
	return p.requestRun(ctx, run, method, params)
}

func (p *backendProcess) requestRun(ctx context.Context, run *processRun, method string, params any) (json.RawMessage, error) {
	id := p.nextID.Add(1)
	ch := make(chan *rpcMessage, 1)

	run.mu.Lock()
	if run.scanErr != nil {
		err := run.scanErr
		run.mu.Unlock()
		return nil, fmt.Errorf("backend dead: %w", err)
	}
	if run.closed {
		run.mu.Unlock()
		return nil, fmt.Errorf("run closed")
	}
	run.pending[id] = ch
	run.mu.Unlock()

	p.mu.Lock()
	p.activeCalls++
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
	go func() { writeErr <- p.writeRun(run, req) }()

	select {
	case err := <-writeErr:
		if err != nil {
			run.mu.Lock()
			delete(run.pending, id)
			run.mu.Unlock()
			return nil, err
		}
	case <-ctx.Done():
		go p.killGen(run.gen)
		run.mu.Lock()
		delete(run.pending, id)
		run.mu.Unlock()
		return nil, ctx.Err()
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("backend closed during request")
		}
		if msg.Error != nil {
			return nil, classifyRPCError(&rpcErrorShape{
				Code:    msg.Error.Code,
				Message: msg.Error.Message,
			})
		}
		return msg.Result, nil
	case <-ctx.Done():
		run.mu.Lock()
		delete(run.pending, id)
		run.mu.Unlock()
		return nil, ctx.Err()
	}
}

// writeRun writes a JSON-RPC message to a specific run. It is safe to
// call on a run that gets closed concurrently: after close it returns
// an error instead of touching a different run's encoder.
func (p *backendProcess) writeRun(run *processRun, v any) error {
	run.mu.Lock()
	if run.closed {
		run.mu.Unlock()
		return fmt.Errorf("run closed")
	}
	run.mu.Unlock()

	run.writeMu.Lock()
	defer run.writeMu.Unlock()

	run.mu.Lock()
	if run.closed || run.enc == nil {
		run.mu.Unlock()
		return fmt.Errorf("run closed")
	}
	run.mu.Unlock()

	return run.enc.Encode(v)
}

func (p *backendProcess) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""
	seen := make(map[string]bool)
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := p.request(ctx, "tools/list", params)
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
			p.log.Warn("tools/list duplicate cursor, stopping",
				"server", p.cfg.Name, "cursor", next)
			return all, nil
		}
		seen[next] = true
		cursor = next
	}
}

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
	return decodeCallToolResult(res)
}

func (p *backendProcess) Alive() bool {
	p.mu.Lock()
	run := p.current
	p.mu.Unlock()
	if run == nil {
		return false
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.ready && run.scanErr == nil
}

func (p *backendProcess) LastToolCall() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastToolCall
}

func (p *backendProcess) LastActivity() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastToolCall.After(p.startedAt) {
		return p.lastToolCall
	}
	return p.startedAt
}

func (p *backendProcess) StopIfIdle(ttl time.Duration) bool {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	run := p.current
	calls := p.activeCalls
	last := p.lastToolCall
	if last.IsZero() {
		last = p.startedAt
	}
	p.mu.Unlock()

	if run == nil || calls > 0 {
		return false
	}
	run.mu.Lock()
	alive := run.ready && run.scanErr == nil
	run.mu.Unlock()
	if !alive {
		return false
	}
	if time.Since(last) < ttl {
		return false
	}
	p.killGenLocked(run.gen)
	return true
}

func (p *backendProcess) killGen(gen int) {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.killGenLocked(gen)
}

func (p *backendProcess) killGenLocked(gen int) {
	p.mu.Lock()
	run := p.current
	if run == nil || run.gen != gen {
		p.mu.Unlock()
		return
	}
	p.current = nil
	p.mu.Unlock()

	run.markClosed()

	// Close stdin to unblock any pending Encode.
	if run.stdin != nil {
		_ = run.stdin.Close()
	}

	if run.proc != nil && run.proc.Process != nil {
		pid := run.proc.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGTERM)

		// Wait for the leader, but always SIGKILL the whole group at
		// the end so that children which ignored SIGTERM (or were
		// reparented after the leader exited) don't survive us.
		done := make(chan struct{})
		go func() {
			_ = run.proc.Wait()
			close(done)
		}()
		select {
		case <-done:
			// Leader is gone; still make sure the group is empty.
		case <-time.After(2 * time.Second):
			// Leader ignored SIGTERM; escalate below.
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}
}

func (p *backendProcess) Stop() {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()

	p.mu.Lock()
	run := p.current
	p.mu.Unlock()
	if run == nil {
		return
	}
	p.killGenLocked(run.gen)
	p.log.Info("backend stopped", "server", p.cfg.Name)
}

func buildEnv(cfg config.ServerConfig) []string {
	env := os.Environ()
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	return env
}
