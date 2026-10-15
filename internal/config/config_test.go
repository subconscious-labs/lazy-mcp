package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults_EmptyProxy(t *testing.T) {
	cfg := &Config{}
	cfg.defaults()

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"ToolPrefix", cfg.Proxy.ToolPrefix, "meta"},
		{"MaxSearchResults", cfg.Proxy.MaxSearchResults, 8},
		{"IdleTTL", cfg.Proxy.IdleTTL, 10 * time.Minute},
		{"ToolCacheTTL", cfg.Proxy.ToolCacheTTL, 30 * time.Minute},
		{"ExecTimeout", cfg.Proxy.ExecTimeout, 60 * time.Second},
		{"MaxResultBytes", cfg.Proxy.MaxResultBytes, 262144},
		{"CursorTTL", cfg.Proxy.CursorTTL, 5 * time.Minute},
		{"ShutdownTimeout", cfg.Proxy.ShutdownTimeout, 5 * time.Second},
		{"InitializeTimeout", cfg.Proxy.InitializeTimeout, 10 * time.Second},
		{"CircuitBreaker.FailureThreshold", cfg.Proxy.CircuitBreaker.FailureThreshold, 5},
		{"CircuitBreaker.Cooldown", cfg.Proxy.CircuitBreaker.Cooldown, 30 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestCircuitBreaker_DefaultEnabled(t *testing.T) {
	cfg := &Config{}
	cfg.defaults()
	if !cfg.Proxy.CircuitBreaker.IsEnabled() {
		t.Fatalf("circuit breaker should be enabled by default")
	}
}

func TestCircuitBreaker_ExplicitDisabled(t *testing.T) {
	disabled := false
	cfg := &Config{}
	cfg.Proxy.CircuitBreaker.Enabled = &disabled
	cfg.defaults()
	if cfg.Proxy.CircuitBreaker.IsEnabled() {
		t.Fatalf("explicit enabled=false should disable circuit breaker")
	}
}

func TestEnabledServers(t *testing.T) {
	cfg := &Config{
		Servers: []ServerConfig{
			{Name: "a", Enabled: true},
			{Name: "b", Enabled: false},
			{Name: "c", Enabled: true},
		},
	}
	got := cfg.EnabledServers()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "c" {
		t.Fatalf("unexpected enabled list: %+v", got)
	}
}

func TestServer_Lookup(t *testing.T) {
	cfg := &Config{
		Servers: []ServerConfig{
			{Name: "a", Enabled: true},
			{Name: "b", Enabled: false},
		},
	}
	if s, ok := cfg.Server("a"); !ok || s.Name != "a" {
		t.Fatalf("server a not found")
	}
	if _, ok := cfg.Server("nope"); ok {
		t.Fatalf("nope should not be found")
	}
}

func TestLoad_YAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
version: 1
proxy:
  tool_prefix: "x"
  max_search_results: 3
servers:
  - name: foo
    transport: stdio
    command: /bin/true
    enabled: true
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxy.ToolPrefix != "x" {
		t.Errorf("tool_prefix: got %q", cfg.Proxy.ToolPrefix)
	}
	if cfg.Proxy.MaxSearchResults != 3 {
		t.Errorf("max_search_results: got %d", cfg.Proxy.MaxSearchResults)
	}
	// Non-overridden defaults still applied.
	if cfg.Proxy.IdleTTL != 10*time.Minute {
		t.Errorf("idle_ttl default not applied: %v", cfg.Proxy.IdleTTL)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "foo" {
		t.Errorf("servers: %+v", cfg.Servers)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load("/nonexistent/path.yaml"); err == nil {
		t.Fatal("expected error for missing file")
	}
}
