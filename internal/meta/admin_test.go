package meta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/session"
)

func newAdminHandlers(reloadFn func() ([]string, error)) *Handlers {
	holder := config.NewHolder(&config.Config{
		Proxy: config.ProxyConfig{CursorTTL: time.Minute},
		Servers: []config.ServerConfig{
			{Name: "a", Enabled: true},
			{Name: "b", Enabled: true},
			{Name: "c", Enabled: false},
		},
	})
	return &Handlers{
		cfg:      holder,
		log:      logx.New(io.Discard),
		pager:    NewPageStore(time.Minute),
		reloadFn: reloadFn,
	}
}

func adminCtx() context.Context {
	return session.WithContext(context.Background(),
		session.New("s1", "admin", "admin", nil))
}

func agentCtx() context.Context {
	return session.WithContext(context.Background(),
		session.New("s2", "elsa", "agent", []string{"a"}))
}

func TestReload_AdminOnly(t *testing.T) {
	called := false
	h := newAdminHandlers(func() ([]string, error) {
		called = true
		return nil, nil
	})

	_, err := h.Reload(agentCtx(), nil)
	if err == nil || !strings.Contains(err.Error(), "admin") {
		t.Fatalf("agent should be rejected: %v", err)
	}
	if called {
		t.Fatal("reloadFn must not be called for agent")
	}
}

func TestReload_Succeeds(t *testing.T) {
	h := newAdminHandlers(func() ([]string, error) {
		return []string{"dup warning"}, nil
	})

	out, err := h.Reload(adminCtx(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["ok"] != true {
		t.Fatalf("ok: %v", parsed["ok"])
	}
	if parsed["servers"] != float64(3) {
		t.Fatalf("servers: %v", parsed["servers"])
	}
	if parsed["enabled"] != float64(2) {
		t.Fatalf("enabled: %v", parsed["enabled"])
	}
	names := parsed["names"].([]any)
	if len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names: %v", names)
	}
	warns := parsed["warnings"].([]any)
	if len(warns) != 1 || warns[0] != "dup warning" {
		t.Fatalf("warnings: %v", warns)
	}
}

func TestReload_ErrorPropagates(t *testing.T) {
	h := newAdminHandlers(func() ([]string, error) {
		return nil, errors.New("parse error")
	})
	_, err := h.Reload(adminCtx(), nil)
	if err == nil || !strings.Contains(err.Error(), "parse error") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestReload_NotAvailable(t *testing.T) {
	h := newAdminHandlers(nil)
	_, err := h.Reload(adminCtx(), nil)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected not-available error, got %v", err)
	}
}

func TestReload_Cooldown(t *testing.T) {
	calls := 0
	h := newAdminHandlers(func() ([]string, error) {
		calls++
		return nil, nil
	})

	if _, err := h.Reload(adminCtx(), nil); err != nil {
		t.Fatalf("first call should succeed: %v", err)
	}
	_, err := h.Reload(adminCtx(), nil)
	if err == nil || !strings.Contains(err.Error(), "too soon") {
		t.Fatalf("second call within cooldown should fail, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("reloadFn called %d times, want 1", calls)
	}
}

func TestReload_CooldownExpires(t *testing.T) {
	calls := 0
	h := newAdminHandlers(func() ([]string, error) {
		calls++
		return nil, nil
	})
	// Backdate lastReload to simulate cooldown expiry without sleeping.
	h.reloadMu.Lock()
	h.lastReload = time.Now().Add(-reloadCooldown - time.Second)
	h.reloadMu.Unlock()

	if _, err := h.Reload(adminCtx(), nil); err != nil {
		t.Fatalf("reload should succeed after cooldown: %v", err)
	}
	if calls != 1 {
		t.Fatalf("reloadFn calls: %d", calls)
	}
}
