// Package policy enforces per-server tool allow/deny rules, the
// dangerous_tools gate, and per-(user, server, tool) rate limiting.
package policy

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
)

// Policy answers "may this user call this (server, tool) right now?".
// It reads config through a Holder, so SIGHUP changes take effect
// without rebuilding the object.
type Policy struct {
	cfg *config.Holder

	mu       sync.Mutex
	buckets  map[string]*bucket
	paramsID string // identifies current (perMinute, burst); limiter reset on change
}

// New creates a Policy bound to cfg.
func New(cfg *config.Holder) *Policy {
	return &Policy{
		cfg:     cfg,
		buckets: make(map[string]*bucket),
	}
}

// Visible reports whether tool from serverName may be indexed and
// shown to user. Empty user (stdio) or user="local" is treated as
// admin and sees everything enabled.
//
// Note: rate limiting is intentionally NOT considered here. An
// indexed tool is always visible; only calls are throttled.
func (p *Policy) Visible(user, serverName, tool string) bool {
	cfg := p.cfg.Get()
	srv, ok := cfg.Server(serverName)
	if !ok || !srv.Enabled {
		return false
	}

	// Deny rules apply to everyone, including admin. Otherwise "denied"
	// would mean "denied for agents only", which defeats the point.
	if contains(srv.DeniedTools, tool) {
		return false
	}
	if cfg.Proxy.DenyDangerous && contains(srv.DangerousTools, tool) {
		return false
	}

	// Admin (or stdio-local) bypasses the allowlist; an agent must match it.
	if user == "" || user == "local" {
		return true
	}
	if len(srv.AllowedTools) > 0 && !contains(srv.AllowedTools, tool) {
		return false
	}
	return true
}

// Authorize verifies that user may call (serverName, tool) but does
// NOT consume a rate-limit token. Use this on the request path where
// multiple layers check the same call, and in DescribeTool.
func (p *Policy) Authorize(user, serverName, tool string) error {
	if !p.Visible(user, serverName, tool) {
		return fmt.Errorf("policy: tool %s/%s is not permitted for user %q",
			serverName, tool, user)
	}
	return nil
}

// Check verifies visibility AND consumes a token from the rate-limit
// bucket. Called exactly once per actual tool execution.
func (p *Policy) Check(user, serverName, tool string) error {
	if err := p.Authorize(user, serverName, tool); err != nil {
		return err
	}
	rl := p.cfg.Get().Proxy.RateLimit
	if !rl.Enabled {
		return nil
	}
	key := user + "\x00" + serverName + "/" + tool
	if err := p.consume(key, rl.PerMinute, rl.Burst); err != nil {
		return fmt.Errorf("policy: %s/%s: %w", serverName, tool, err)
	}
	return nil
}

// Invalidate resets the rate limiter state. Called on SIGHUP so that
// changes to per_minute/burst take effect immediately.
func (p *Policy) Invalidate() {
	p.mu.Lock()
	p.buckets = make(map[string]*bucket)
	p.paramsID = ""
	p.mu.Unlock()
}

func (p *Policy) consume(key string, perMinute, burst int) error {
	paramsID := fmt.Sprintf("%d/%d", perMinute, burst)

	p.mu.Lock()
	if p.paramsID != paramsID {
		p.buckets = make(map[string]*bucket)
		p.paramsID = paramsID
	}
	b, ok := p.buckets[key]
	if !ok {
		b = newBucket(float64(perMinute)/60.0, float64(burst))
		p.buckets[key] = b
	}
	p.mu.Unlock()

	if !b.allow() {
		return fmt.Errorf("rate limit exceeded (%d/min, burst %d)", perMinute, burst)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// bucket is a minimal token bucket.
type bucket struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	rate     float64 // tokens per second
	capacity float64
}

func newBucket(rate, capacity float64) *bucket {
	return &bucket{
		tokens:   capacity,
		last:     time.Now(),
		rate:     rate,
		capacity: capacity,
	}
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.rate)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
