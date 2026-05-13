// Package broker manages stdio MCP backend subprocess lifecycle.
package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

// ToolInfo is a normalized tool descriptor from a backend.
type ToolInfo struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Backend is a running backend session.
type Backend interface {
	ListTools(ctx context.Context) ([]ToolInfo, error)
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
	Stop()
}

// Broker manages backends keyed by server name.
type Broker struct {
	cfg  *config.Config
	log  *logx.Logger
	mu   sync.Mutex
	pool map[string]*backendProcess
}

// New creates a Broker.
func New(cfg *config.Config, log *logx.Logger) *Broker {
	return &Broker{
		cfg:  cfg,
		log:  log,
		pool: make(map[string]*backendProcess),
	}
}

// Ensure returns a ready Backend for serverName, starting it if necessary.
func (b *Broker) Ensure(ctx context.Context, serverName string) (Backend, error) {
	srvcfg, ok := b.cfg.Server(serverName)
	if !ok {
		return nil, fmt.Errorf("broker: server %q not found in config", serverName)
	}

	b.mu.Lock()
	proc, exists := b.pool[serverName]
	if !exists {
		proc = newBackendProcess(srvcfg, b.log)
		b.pool[serverName] = proc
	}
	b.mu.Unlock()

	if err := proc.Ensure(ctx); err != nil {
		return nil, fmt.Errorf("broker: start %q: %w", serverName, err)
	}
	return proc, nil
}

// CallTool ensures the backend is running then calls a tool.
func (b *Broker) CallTool(ctx context.Context, serverName, tool string, args map[string]any) (string, error) {
	back, err := b.Ensure(ctx, serverName)
	if err != nil {
		return "", err
	}
	return back.CallTool(ctx, tool, args)
}

// ListTools ensures the backend is running then lists tools.
func (b *Broker) ListTools(ctx context.Context, serverName string) ([]ToolInfo, error) {
	back, err := b.Ensure(ctx, serverName)
	if err != nil {
		return nil, err
	}
	return back.ListTools(ctx)
}

// StopAll stops all running backends.
func (b *Broker) StopAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, proc := range b.pool {
		b.log.Info("stopping backend", "server", name)
		proc.Stop()
	}
}
