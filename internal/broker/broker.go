// Package broker manages stdio MCP backend subprocess lifecycle.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/observability"
	"github.com/subconscious-labs/lazy-mcp/internal/policy"
	"github.com/subconscious-labs/lazy-mcp/internal/session"
)

// ToolAnnotations mirrors the MCP tools/list annotations. All fields
// are hints, not guarantees; clients must not rely on them for safety.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint bool   `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool   `json:"openWorldHint,omitempty"`
}

// ToolInfo is a normalized tool descriptor from a backend.
type ToolInfo struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Annotations ToolAnnotations
}

// Backend is a running backend session (stdio or http).
type Backend interface {
	Ensure(ctx context.Context) error
	ListTools(ctx context.Context) ([]ToolInfo, error)
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
	Stop()
	Alive() bool
	LastToolCall() time.Time
	LastActivity() time.Time
	SetNotificationHandler(NotificationHandler)
}

// Broker manages backends keyed by server name.
type Broker struct {
	cfg    *config.Holder
	policy *policy.Policy
	log    *logx.Logger

	mu   sync.Mutex
	pool map[string]Backend

	circuitsMu sync.Mutex
	circuits   map[string]*circuit

	notifMu      sync.RWMutex
	notifHandler NotificationHandler
}

// New creates a Broker.
func New(cfg *config.Holder, pol *policy.Policy, log *logx.Logger) *Broker {
	return &Broker{
		cfg:      cfg,
		policy:   pol,
		log:      log,
		pool:     make(map[string]Backend),
		circuits: make(map[string]*circuit),
	}
}

// Ensure returns a ready Backend for serverName, starting it if necessary.
// The handshake (initialize) is bounded by proxy.initialize_timeout.
func (b *Broker) Ensure(ctx context.Context, serverName string) (Backend, error) {
	cfg := b.cfg.Get()

	srvcfg, ok := cfg.Server(serverName)
	if !ok {
		return nil, fmt.Errorf("broker: server %q not found in config", serverName)
	}
	if !srvcfg.Enabled {
		return nil, fmt.Errorf("broker: server %q is disabled", serverName)
	}

	b.mu.Lock()
	proc, exists := b.pool[serverName]
	if !exists {
		var nb Backend
		if srvcfg.Transport == "http" {
			nb = newHTTPBackend(srvcfg, b.log)
		} else {
			nb = newProcessBackend(srvcfg, b.log)
		}
		nb.SetNotificationHandler(b.getNotificationHandler())
		proc = nb
		b.pool[serverName] = proc
	}
	b.mu.Unlock()

	startCtx, cancel := context.WithTimeout(ctx, cfg.Proxy.InitializeTimeout)
	defer cancel()

	if err := proc.Ensure(startCtx); err != nil {
		return nil, fmt.Errorf("broker: start %q: %w", serverName, err)
	}
	return proc, nil
}

// CallTool ensures the backend is running then calls a tool. Applies the
// circuit breaker and the configured exec_timeout, and records metrics.
func (b *Broker) CallTool(ctx context.Context, serverName, tool string, args map[string]any) (string, error) {
	start := time.Now()
	result, err := b.doCallTool(ctx, serverName, tool, args)
	observability.ObserveCall(serverName, tool, callStatus(err), time.Since(start))
	return result, err
}

func (b *Broker) doCallTool(ctx context.Context, serverName, tool string, args map[string]any) (string, error) {
	origCtx := ctx

	sess, _ := session.FromContext(ctx)
	user := ""
	if sess != nil {
		user = sess.User
	}
	if err := b.policy.Check(user, serverName, tool); err != nil {
		return "", err
	}

	if err := b.gate(serverName); err != nil {
		return "", err
	}

	back, err := b.Ensure(ctx, serverName)
	if err != nil {
		b.classify(origCtx, serverName, err)
		return "", err
	}

	callCtx, cancel := b.withExecTimeout(ctx)
	defer cancel()

	result, err := back.CallTool(callCtx, tool, args)
	b.classify(origCtx, serverName, err)
	return result, err
}

// callStatus maps an error to a metrics status label.
func callStatus(err error) string {
	if err == nil {
		return observability.StatusOK
	}
	switch {
	case errors.Is(err, ErrToolError):
		return observability.StatusToolError
	case errors.Is(err, ErrRPCClientError):
		return observability.StatusClientError
	case errors.Is(err, ErrCircuitOpen):
		return observability.StatusCircuitOpen
	case errors.Is(err, context.DeadlineExceeded):
		return observability.StatusTimeout
	case errors.Is(err, context.Canceled):
		return observability.StatusCanceled
	}
	return observability.StatusError
}

// ListTools ensures the backend is running then lists tools.
func (b *Broker) ListTools(ctx context.Context, serverName string) ([]ToolInfo, error) {
	origCtx := ctx

	if err := b.gate(serverName); err != nil {
		return nil, err
	}

	back, err := b.Ensure(ctx, serverName)
	if err != nil {
		b.classify(origCtx, serverName, err)
		return nil, err
	}

	callCtx, cancel := b.withExecTimeout(ctx)
	defer cancel()

	tools, err := back.ListTools(callCtx)
	b.classify(origCtx, serverName, err)
	return tools, err
}

// withExecTimeout wraps ctx with proxy.exec_timeout if configured.
func (b *Broker) withExecTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := b.cfg.Get().Proxy.ExecTimeout
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// gate checks whether a call to serverName is currently allowed.
func (b *Broker) gate(serverName string) error {
	c := b.circuitFor(serverName)
	if c == nil {
		return nil
	}
	return c.allow()
}

// classify updates the circuit for serverName based on the outcome of a call.
func (b *Broker) classify(ctx context.Context, serverName string, err error) {
	c := b.circuitFor(serverName)
	if c == nil {
		return
	}

	if err == nil || errors.Is(err, ErrToolError) || errors.Is(err, ErrRPCClientError) {
		prevState, _ := c.snapshot()
		c.recordSuccess()
		observability.SetCircuit(serverName, observability.CircuitClosed)
		if prevState != circuitClosed {
			b.log.Info("circuit closed", "server", serverName)
		}
		return
	}

	if ctx.Err() != nil {
		return
	}

	prevState, prevFails := c.snapshot()
	c.recordFailure(err)
	newState, _ := c.snapshot()
	observability.SetCircuit(serverName, observability.CircuitCode(newState.String()))
	if newState != prevState {
		cooldown := b.cfg.Get().Proxy.CircuitBreaker.Cooldown
		b.log.Warn("circuit opened",
			"server", serverName,
			"consecutive_failures", prevFails+1,
			"cooldown", cooldown.String())
	} else if newState == circuitOpen {
		b.log.Warn("circuit failure",
			"server", serverName,
			"consecutive_failures", prevFails+1)
	}
}

// circuitFor returns the circuit for serverName, or nil if disabled.
func (b *Broker) circuitFor(serverName string) *circuit {
	cb := b.cfg.Get().Proxy.CircuitBreaker
	if !cb.IsEnabled() {
		observability.SetCircuit(serverName, observability.CircuitDisabled)
		return nil
	}
	b.circuitsMu.Lock()
	defer b.circuitsMu.Unlock()
	c, ok := b.circuits[serverName]
	if !ok {
		c = newCircuit(cb.FailureThreshold, cb.Cooldown)
		b.circuits[serverName] = c
		observability.SetCircuit(serverName, observability.CircuitClosed)
	}
	return c
}

// CircuitState returns the current circuit state for a server
// ("closed", "open", "half-open", or "disabled").
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
	Name         string
	Running      bool
	Circuit      string
	LastToolCall time.Time
	LastError    string
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
	if c := b.circuitFor(serverName); c != nil {
		if e := c.lastError(); e != nil {
			st.LastError = e.Error()
		}
	}
	if proc != nil {
		st.Running = proc.Alive()
		st.LastToolCall = proc.LastToolCall()
	}
	return st
}

// ApplyConfig reconciles the running pool with a new configuration.
// Servers removed, disabled, or whose command/args/env changed are stopped.
// Added servers and re-enabled servers start lazily on next Ensure.
func (b *Broker) ApplyConfig(oldCfg, newCfg *config.Config) {
	oldServers := make(map[string]config.ServerConfig, len(oldCfg.Servers))
	for _, s := range oldCfg.Servers {
		oldServers[s.Name] = s
	}
	newServers := make(map[string]config.ServerConfig, len(newCfg.Servers))
	for _, s := range newCfg.Servers {
		newServers[s.Name] = s
	}

	for name, oldSrv := range oldServers {
		newSrv, stillExists := newServers[name]
		if !stillExists || !newSrv.Enabled {
			b.stopServer(name, "removed or disabled")
			continue
		}
		if serverRestartRequired(oldSrv, newSrv) {
			b.stopServer(name, "command/args/env changed")
		}
	}
}

func serverRestartRequired(a, c config.ServerConfig) bool {
	if a.Transport != c.Transport {
		return true
	}
	switch a.Transport {
	case "stdio":
		if a.Command != c.Command {
			return true
		}
		if !slices.Equal(a.Args, c.Args) {
			return true
		}
		if !maps.Equal(a.Env, c.Env) {
			return true
		}
	case "http":
		if a.URL != c.URL {
			return true
		}
		if !maps.Equal(a.Headers, c.Headers) {
			return true
		}
	}
	return false
}

func (b *Broker) stopServer(name, reason string) {
	b.mu.Lock()
	proc, exists := b.pool[name]
	if exists {
		delete(b.pool, name)
	}
	b.mu.Unlock()
	if !exists {
		return
	}
	b.log.Info("reload: stopping backend", "server", name, "reason", reason)
	proc.Stop()
}

// StartIdleReaper launches a goroutine that periodically stops backends
// idle longer than proxy.idle_ttl. No-op if idle_ttl <= 0.
func (b *Broker) StartIdleReaper(ctx context.Context) {
	ttl := b.cfg.Get().Proxy.IdleTTL
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
	procs := make(map[string]Backend, len(b.pool))
	for name, proc := range b.pool {
		procs[name] = proc
	}
	b.mu.Unlock()

	now := time.Now()
	for name, proc := range procs {
		last := proc.LastActivity()
		if now.Sub(last) < ttl {
			continue
		}
		if proc.Alive() {
			b.log.Info("reaped idle backend", "server", name, "idle_for", time.Since(last).String())
			proc.Stop()
		}
	}
}

// StopAll stops all running backends and clears the pool.
func (b *Broker) StopAll() {
	b.mu.Lock()
	procs := b.pool
	b.pool = make(map[string]Backend)
	b.mu.Unlock()

	for name, proc := range procs {
		b.log.Info("stopping backend", "server", name)
		proc.Stop()
	}
}

// SetNotificationHandler installs h for all downstream notifications.
// Propagates to already-running backends and to backends started later.
func (b *Broker) SetNotificationHandler(h NotificationHandler) {
	b.notifMu.Lock()
	b.notifHandler = h
	b.notifMu.Unlock()

	b.mu.Lock()
	procs := make([]Backend, 0, len(b.pool))
	for _, p := range b.pool {
		procs = append(procs, p)
	}
	b.mu.Unlock()
	for _, p := range procs {
		p.SetNotificationHandler(h)
	}
}

func (b *Broker) getNotificationHandler() NotificationHandler {
	b.notifMu.RLock()
	defer b.notifMu.RUnlock()
	return b.notifHandler
}
