// Package broker manages stdio MCP backend subprocess lifecycle.
package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

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
// The configured exec_timeout is applied only to the call itself,
// not to backend startup (that has its own bounded handshake).
func (b *Broker) CallTool(ctx context.Context, serverName, tool string, args map[string]any) (string, error) {
	back, err := b.Ensure(ctx, serverName)
	if err != nil {
		return "", err
	}
	ctx, cancel := b.withExecTimeout(ctx)
	defer cancel()
	return back.CallTool(ctx, tool, args)
}

// ListTools ensures the backend is running then lists tools.
func (b *Broker) ListTools(ctx context.Context, serverName string) ([]ToolInfo, error) {
	back, err := b.Ensure(ctx, serverName)
	if err != nil {
		return nil, err
	}
	ctx, cancel := b.withExecTimeout(ctx)
	defer cancel()
	return back.ListTools(ctx)
}

// withExecTimeout wraps ctx with proxy.exec_timeout if configured.
func (b *Broker) withExecTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if b.cfg.Proxy.ExecTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, b.cfg.Proxy.ExecTimeout)
}

// StopAll stops all running backends and clears the pool.
func (b *Broker) StopAll() {
	b.mu.Lock()
	procs := b.pool
	b.pool = make(map[string]*backendProcess)
	b.mu.Unlock()

	for name, proc := range procs {
		b.log.Info("stopping backend", "server", name)
		proc.Stop()
	}
}

// StartIdleReaper launches a goroutine that periodically stops backends
// idle longer than proxy.idle_ttl. The goroutine exits when ctx is done.
// No-op if idle_ttl <= 0.
func (b *Broker) StartIdleReaper(ctx context.Context) {
	ttl := b.cfg.Proxy.IdleTTL
	if ttl <= 0 {
		b.log.Info("idle reaper disabled", "idle_ttl", ttl.String())
		return
	}

	tick := ttl / 2
	if tick < 10*time.Second {
		tick = 10 * time.Second
	}
	if tick > 5*time.Minute {
		tick = 5 * time.Minute
	}

	b.log.Info("idle reaper started", "idle_ttl", ttl.String(), "tick", tick.String())
	go b.reapLoop(ctx, ttl, tick)
}

func (b *Broker) reapLoop(ctx context.Context, ttl, tick time.Duration) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.reapIdle(ttl)
		}
	}
}

func (b *Broker) reapIdle(ttl time.Duration) {
	b.mu.Lock()
	procs := make(map[string]*backendProcess, len(b.pool))
	for name, proc := range b.pool {
		procs[name] = proc
	}
	b.mu.Unlock()

	now := time.Now()
	for name, proc := range procs {
		last := proc.LastUsed()
		if now.Sub(last) < ttl {
			continue
		}
		if proc.StopIfIdle(ttl) {
			b.log.Info("reaped idle backend",
				"server", name,
				"idle_for", time.Since(last).String())
		}
	}
}
