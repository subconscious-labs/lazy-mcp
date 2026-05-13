package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

type backendProcess struct {
	cfg    config.ServerConfig
	log    *logx.Logger
	mu     sync.Mutex
	proc   *exec.Cmd
	enc    *json.Encoder
	scanner *bufio.Scanner
	nextID atomic.Int64
	ready  bool
}

func newBackendProcess(cfg config.ServerConfig, log *logx.Logger) *backendProcess {
	return &backendProcess{cfg: cfg, log: log}
}

// Ensure starts the process and runs initialize if not already ready.
func (p *backendProcess) Ensure(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ready {
		return nil
	}
	return p.start(ctx)
}

func (p *backendProcess) start(ctx context.Context) error {
	env := os.Environ()
	for k, v := range p.cfg.Env {
		env = append(env, k+"="+v)
	}

	cmd := exec.CommandContext(ctx, p.cfg.Command, p.cfg.Args...)
	cmd.Env = env
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

	p.proc = cmd
	p.enc = json.NewEncoder(stdin)
	p.scanner = bufio.NewScanner(stdout)
	p.log.Info("backend started", "server", p.cfg.Name, "pid", cmd.Process.Pid)

	// MCP initialize handshake
	if err := p.initialize(ctx); err != nil {
		p.kill()
		return fmt.Errorf("initialize: %w", err)
	}
	p.ready = true
	p.log.Info("backend ready", "server", p.cfg.Name)
	return nil
}

func (p *backendProcess) initialize(ctx context.Context) error {
	id := p.nextID.Add(1)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":   map[string]any{},
			"clientInfo": map[string]any{
				"name":    "lazy-mcp",
				"version": "0.1.0",
			},
		},
	}
	if err := p.enc.Encode(req); err != nil {
		return err
	}
	if _, err := p.readResponse(id); err != nil {
		return err
	}
	// notifications/initialized
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  map[string]any{},
	}
	return p.enc.Encode(notif)
}

// ListTools calls tools/list on the backend.
func (p *backendProcess) ListTools(ctx context.Context) ([]ToolInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	id := p.nextID.Add(1)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	if err := p.enc.Encode(req); err != nil {
		return nil, err
	}
	res, err := p.readResponse(id)
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
	b, _ := json.Marshal(res)
	if err := json.Unmarshal(b, &lr); err != nil {
		return nil, fmt.Errorf("parse tools/list: %w", err)
	}
	var tools []ToolInfo
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
	p.mu.Lock()
	defer p.mu.Unlock()

	id := p.nextID.Add(1)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	if err := p.enc.Encode(req); err != nil {
		return "", err
	}
	res, err := p.readResponse(id)
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
	b, _ := json.Marshal(res)
	if err := json.Unmarshal(b, &cr); err != nil {
		return "", fmt.Errorf("parse tools/call result: %w", err)
	}
	var parts []string
	for _, c := range cr.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	result := strings.Join(parts, "\n")
	if cr.IsError {
		return "", fmt.Errorf("tool error: %s", result)
	}
	return result, nil
}

// readResponse reads lines until it finds the JSON-RPC response matching id.
func (p *backendProcess) readResponse(id int64) (any, error) {
	for p.scanner.Scan() {
		line := strings.TrimSpace(p.scanner.Text())
		if line == "" {
			continue
		}
		var msg struct {
			JSONRPC string  `json:"jsonrpc"`
			ID      *int64  `json:"id"`
			Result  any     `json:"result"`
			Error   *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // skip non-JSON lines (e.g. debug output)
		}
		if msg.ID == nil || *msg.ID != id {
			continue // not our response
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		return msg.Result, nil
	}
	if err := p.scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner: %w", err)
	}
	return nil, fmt.Errorf("backend closed stream")
}

func (p *backendProcess) kill() {
	if p.proc != nil && p.proc.Process != nil {
		p.proc.Process.Kill() //nolint:errcheck
	}
	p.ready = false
}

func (p *backendProcess) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kill()
}
