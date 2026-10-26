// Package session carries per-connection state: user identity, role
// and lifecycle. Stored in context so handlers can look up "who is
// this call from" without threading extra parameters everywhere.
package session

import (
	"context"
	"sync"
	"time"
)

// Session is a single logical client.
type Session struct {
	ID   string
	User string // immutable after creation

	mu             sync.Mutex
	role           string   // guarded by mu
	allowedServers []string // guarded by mu
	lastSeen       time.Time

	createdAt time.Time
}

// New creates a Session. The caller's slice is copied so that the
// caller can safely reuse or mutate it afterwards.
func New(id, user, role string, allowed []string) *Session {
	now := time.Now()
	cp := append([]string(nil), allowed...)
	return &Session{
		ID:             id,
		User:           user,
		role:           role,
		allowedServers: cp,
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

// Role returns the current effective role.
func (s *Session) Role() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.role
}

// AllowedServers returns a copy of the current allow-list.
func (s *Session) AllowedServers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.allowedServers))
	copy(out, s.allowedServers)
	return out
}

// UpdatePermissions replaces the cached role and allowed servers.
// Called by the hub when a request authenticates or when the config
// reloads, to reflect the current auth state.
func (s *Session) UpdatePermissions(role string, allowed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.role = role
	s.allowedServers = append(s.allowedServers[:0], allowed...)
}

// CanSeeServer reports whether this user is currently allowed to talk
// to server. Reads under the lock so it is safe against concurrent
// UpdatePermissions.
func (s *Session) CanSeeServer(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.role == "admin" {
		return true
	}
	for _, a := range s.allowedServers {
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
