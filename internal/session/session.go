// Package session carries per-connection state: user identity, role
// and lifecycle. Stored in context so handlers can look up "who is
// this call from" without threading extra parameters everywhere.
package session

import (
	"context"
	"sync"
	"time"
)

// Session is a single logical client. In stdio mode there is exactly
// one (the local user); in HTTP mode there is one per Mcp-Session-Id.
type Session struct {
	ID             string
	User           string
	Role           string   // "agent" | "admin"
	AllowedServers []string // meaningful for role=agent

	createdAt time.Time

	mu       sync.Mutex
	lastSeen time.Time
}

// New creates a Session.
func New(id, user, role string, allowed []string) *Session {
	now := time.Now()
	return &Session{
		ID:             id,
		User:           user,
		Role:           role,
		AllowedServers: allowed,
		createdAt:      now,
		lastSeen:       now,
	}
}

// LocalAdmin is the synthetic session used in stdio mode.
func LocalAdmin() *Session {
	return New("stdio", "local", "admin", nil)
}

// Touch updates last-seen time.
func (s *Session) Touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// LastSeen returns the last activity time.
func (s *Session) LastSeen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen
}

// Age returns the time since the session was created.
func (s *Session) Age() time.Duration { return time.Since(s.createdAt) }

// CanSeeServer reports whether this user is allowed to talk to server.
func (s *Session) CanSeeServer(name string) bool {
	if s.Role == "admin" {
		return true
	}
	for _, a := range s.AllowedServers {
		if a == name {
			return true
		}
	}
	return false
}

type ctxKey struct{}

// WithContext stores s in ctx.
func WithContext(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the session, if any.
func FromContext(ctx context.Context) (*Session, bool) {
	s, ok := ctx.Value(ctxKey{}).(*Session)
	return s, ok
}
