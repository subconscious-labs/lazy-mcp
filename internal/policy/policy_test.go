package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
)

func newTestPolicy(cfg *config.Config) *Policy {
	return New(config.NewHolder(cfg))
}

func cfgWithServers(servers ...config.ServerConfig) *config.Config {
	return &config.Config{
		Proxy:   config.ProxyConfig{},
		Servers: servers,
	}
}

func TestVisible_DefaultAllowsAll(t *testing.T) {
	p := newTestPolicy(cfgWithServers(config.ServerConfig{Name: "s", Enabled: true}))
	if !p.Visible("", "s", "anything") {
		t.Fatalf("admin should see anything")
	}
}

func TestVisible_AllowlistAdmin(t *testing.T) {
	p := newTestPolicy(cfgWithServers(config.ServerConfig{
		Name:         "s",
		Enabled:      true,
		AllowedTools: []string{"a", "b"},
	}))
	if !p.Visible("", "s", "c") {
		t.Fatalf("admin bypasses allowlist")
	}
}

func TestVisible_Denylist(t *testing.T) {
	p := newTestPolicy(cfgWithServers(config.ServerConfig{
		Name:        "s",
		Enabled:     true,
		DeniedTools: []string{"drop_all"},
	}))
	if p.Visible("", "s", "drop_all") {
		t.Fatalf("denied tool should not be visible even for admin")
	}
	if !p.Visible("", "s", "safe_tool") {
		t.Fatalf("other tools should be visible")
	}
}

func TestVisible_DenylistBeatsAllowlist(t *testing.T) {
	p := newTestPolicy(cfgWithServers(config.ServerConfig{
		Name:         "s",
		Enabled:      true,
		AllowedTools: []string{"a", "b"},
		DeniedTools:  []string{"b"},
	}))
	if !p.Visible("", "s", "a") {
		t.Fatalf("a should be visible")
	}
	if p.Visible("", "s", "b") {
		t.Fatalf("b is in both lists, deny should win")
	}
}

func TestVisible_DenyDangerous(t *testing.T) {
	p := newTestPolicy(&config.Config{
		Proxy: config.ProxyConfig{DenyDangerous: true},
		Servers: []config.ServerConfig{{
			Name:           "s",
			Enabled:        true,
			DangerousTools: []string{"delete_all"},
		}},
	})
	if p.Visible("", "s", "delete_all") {
		t.Fatalf("dangerous tool hidden with deny_dangerous=true")
	}
	if !p.Visible("", "s", "safe_tool") {
		t.Fatalf("safe tool visible")
	}
}

func TestVisible_DenyDangerousDisabled(t *testing.T) {
	p := newTestPolicy(&config.Config{
		Proxy: config.ProxyConfig{DenyDangerous: false},
		Servers: []config.ServerConfig{{
			Name:           "s",
			Enabled:        true,
			DangerousTools: []string{"delete_all"},
		}},
	})
	if !p.Visible("", "s", "delete_all") {
		t.Fatalf("dangerous tool visible when deny_dangerous=false")
	}
}

func TestVisible_DisabledServer(t *testing.T) {
	p := newTestPolicy(cfgWithServers(config.ServerConfig{Name: "s", Enabled: false}))
	if p.Visible("", "s", "a") {
		t.Fatalf("disabled server not visible")
	}
}

func TestVisible_UnknownServer(t *testing.T) {
	p := newTestPolicy(cfgWithServers())
	if p.Visible("", "nope", "a") {
		t.Fatalf("unknown server not visible")
	}
}

func TestVisible_PerUserAllowlist(t *testing.T) {
	p := newTestPolicy(cfgWithServers(config.ServerConfig{
		Name:         "s",
		Enabled:      true,
		AllowedTools: []string{"a"},
	}))
	if !p.Visible("", "s", "b") {
		t.Fatalf("admin sees b outside allowlist")
	}
	if !p.Visible("elsa", "s", "a") {
		t.Fatalf("agent sees a")
	}
	if p.Visible("elsa", "s", "b") {
		t.Fatalf("agent must not see b")
	}
}

func TestCheck_RejectsDenied(t *testing.T) {
	p := newTestPolicy(cfgWithServers(config.ServerConfig{
		Name:        "s",
		Enabled:     true,
		DeniedTools: []string{"x"},
	}))
	err := p.Check("", "s", "x")
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected not permitted, got %v", err)
	}
}

func TestCheck_RateLimit(t *testing.T) {
	p := newTestPolicy(&config.Config{
		Proxy: config.ProxyConfig{
			RateLimit: config.RateLimitConfig{Enabled: true, PerMinute: 60, Burst: 2},
		},
		Servers: []config.ServerConfig{{Name: "s", Enabled: true}},
	})
	if err := p.Check("", "s", "tool"); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if err := p.Check("", "s", "tool"); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if err := p.Check("", "s", "tool"); err == nil {
		t.Fatalf("call 3 should be limited")
	}
	time.Sleep(1100 * time.Millisecond)
	if err := p.Check("", "s", "tool"); err != nil {
		t.Fatalf("after refill: %v", err)
	}
}

func TestCheck_RateLimitPerTool(t *testing.T) {
	p := newTestPolicy(&config.Config{
		Proxy: config.ProxyConfig{
			RateLimit: config.RateLimitConfig{Enabled: true, PerMinute: 60, Burst: 1},
		},
		Servers: []config.ServerConfig{{Name: "s", Enabled: true}},
	})
	if err := p.Check("", "s", "a"); err != nil {
		t.Fatal(err)
	}
	if err := p.Check("", "s", "b"); err != nil {
		t.Fatalf("b independent: %v", err)
	}
	if err := p.Check("", "s", "a"); err == nil {
		t.Fatalf("a should be limited")
	}
}

func TestCheck_RateLimitPerUser(t *testing.T) {
	p := newTestPolicy(&config.Config{
		Proxy: config.ProxyConfig{
			RateLimit: config.RateLimitConfig{Enabled: true, PerMinute: 60, Burst: 1},
		},
		Servers: []config.ServerConfig{{Name: "s", Enabled: true}},
	})
	if err := p.Check("elsa", "s", "a"); err != nil {
		t.Fatal(err)
	}
	if err := p.Check("elsa", "s", "a"); err == nil {
		t.Fatalf("elsa limited")
	}
	if err := p.Check("bob", "s", "a"); err != nil {
		t.Fatalf("bob independent: %v", err)
	}
}

func TestCheck_RateLimitDisabled(t *testing.T) {
	p := newTestPolicy(cfgWithServers(config.ServerConfig{Name: "s", Enabled: true}))
	for i := 0; i < 100; i++ {
		if err := p.Check("", "s", "tool"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestCheck_InvalidateResetsBuckets(t *testing.T) {
	p := newTestPolicy(&config.Config{
		Proxy: config.ProxyConfig{
			RateLimit: config.RateLimitConfig{Enabled: true, PerMinute: 60, Burst: 1},
		},
		Servers: []config.ServerConfig{{Name: "s", Enabled: true}},
	})
	_ = p.Check("", "s", "a")
	if err := p.Check("", "s", "a"); err == nil {
		t.Fatalf("expected limited")
	}
	p.Invalidate()
	if err := p.Check("", "s", "a"); err != nil {
		t.Fatalf("after Invalidate: %v", err)
	}
}

func TestAuthorize_DoesNotConsume(t *testing.T) {
	p := newTestPolicy(&config.Config{
		Proxy: config.ProxyConfig{
			RateLimit: config.RateLimitConfig{Enabled: true, PerMinute: 60, Burst: 1},
		},
		Servers: []config.ServerConfig{{Name: "s", Enabled: true}},
	})

	// 100 Authorize calls — no tokens consumed.
	for i := 0; i < 100; i++ {
		if err := p.Authorize("", "s", "a"); err != nil {
			t.Fatalf("Authorize %d: %v", i, err)
		}
	}

	// One Check consumes the only token.
	if err := p.Check("", "s", "a"); err != nil {
		t.Fatalf("first Check: %v", err)
	}
	// Second Check hits the limit.
	if err := p.Check("", "s", "a"); err == nil {
		t.Fatal("expected rate limit on second Check")
	}
}
