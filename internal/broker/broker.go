// Package broker manages stdio MCP backend subprocess lifecycle.
package broker

import (
	"context"
	"encoding/json"
	"errors"
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

	circuitsMu sync.Mutex
	circuits   map[string]*circuit
}

// New creates a Broker.
func New(cfg *config.Config, log *logx.Logger) *Broker {
	return &Broker{
		cfg:      cfg,
		log:      log,
		pool:     make(map[string]*backendProcess),
		circuits: make(map[string]*circuit),
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
// Applies the circuit breaker and the configured exec_timeout.
func (b *Broker) CallTool(ctx context.Context, serverName, tool string, args map[string]any) (string, error) {
	origCtx := ctx

	if err := b.gate(serverName); err != nil {
		return "", err
	}

	back, err := b.Ensure(ctx, serverName)
	if err != nil {
		b.classify(serverName, err, origCtx)
		return "", err
	}

	callCtx, cancel := b.withExecTimeout(ctx)
	defer cancel()

	result, err := back.CallTool(callCtx, tool, args)
	b.classify(serverName, err, origCtx)
	return result, err
}

// ListTools ensures the backend is running then lists tools.
func (b *Broker) ListTools(ctx context.Context, serverName string) ([]ToolInfo, error) {
	origCtx := ctx

	if err := b.gate(serverName); err != nil {
		return nil, err
	}

	back, err := b.Ensure(ctx, serverName)
	if err != nil {
		b.classify(serverName, err, origCtx)
		return nil, err
	}

	callCtx, cancel := b.withExecTimeout(ctx)
	defer cancel()

	tools, err := back.ListTools(callCtx)
	b.classify(serverName, err, origCtx)
	return tools, err
}

// gate checks whether a call to serverName is currently allowed.
// Returns ErrCircuitOpen (wrapped) if the circuit is open.
func (b *Broker) gate(serverName string) error {
	c := b.circuitFor(serverName)
	if c == nil {
		return nil
	}
	return c.allow()
}

// classify updates the circuit for serverName based on the outcome of a call.
// origCtx is the caller's context before any timeout wrapper was applied —
// used to distinguish client cancellation from our own exec_timeout.
func (b *Broker) classify(serverName string, err error, origCtx context.Context) {
	c := b.circuitFor(serverName)
	if c == nil {
		return
	}

	// Successful round-trip. This includes tools that returned isError:true
	// (that's a business error, not a backend failure) and errors wrapped in
	// ToolError by the process layer.
	if err == nil || errors.Is(err, ErrToolError) {
		prevState, _ := c.snapshot()
		c.recordSuccess()
		if prevState != circuitClosed {
			b.log.Info("circuit closed", "server", serverName)
		}
		return
	}

	// If the caller's own context was cancelled or its deadline expired,
	// don't blame the backend.
	if origCtx.Err() != nil {
		return
	}

	prevState, prevFails := c.snapshot()
	c.recordFailure()
	newState, _ := c.snapshot()
	if newState != prevState {
		b.log.Warn("circuit opened",
			"server", serverName,
			"consecutive_failures", prevFails+1,
			"cooldown", b.cfg.Proxy.CircuitBreaker.Cooldown.String())
	} else if newState == circuitOpen {
		b.log.Warn("circuit failure",
			"server", serverName,
			"consecutive_failures", prevFails+1)
	}
}

// circuitFor returns the circuit for serverName, or nil if disabled.
func (b *Broker) circuitFor(serverName string) *circuit {
	if !b.cfg.Proxy.CircuitBreaker.IsEnabled() {
		return nil
	}
	b.circuitsMu.Lock()
	defer b.circuitsMu.Unlock()
	c, ok := b.circuits[serverName]
	if !ok {
		c = newCircuit(
			b.cfg.Proxy.CircuitBreaker.FailureThreshold,
			b.cfg.Proxy.CircuitBreaker.Cooldown,
		)
		b.circuits[serverName] = c
	}
	return c
}

// CircuitState returns the current circuit state for a server ("closed",
// "open", "half-open", or "disabled"). For use by status endpoints.
func (b *Broker) CircuitState(serverName string) string {
	c := b.circuitFor(serverName)
	if c == nil {
		return "disabled"
	}
	s, _ := c.snapshot()
	return s.String()
}

// ServerStatus summarizes runtime state of a single backend.
type ServerStatus struct {
	Name     string
	Running  bool
	Circuit  string
	LastUsed time.Time // zero if never used
}

// Status returns a snapshot of runtime state for serverName.
func (b *Broker) Status(serverName string) ServerStatus {
	b.mu.Lock()
	proc := b.pool[serverName]
	b.mu.Unlock()

	st := ServerStatus{
		Name:    serverName,
		Circuit: b.CircuitState(serverName),
	}
	if proc != nil {
		st.Running = proc.Alive()
		st.LastUsed = proc.LastUsed()
	}
	return st
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
