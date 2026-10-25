package broker

import (
	"context"
	"testing"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
)

func TestApplyConfig_RemovesDisabledServer(t *testing.T) {
	srvA := testServer("a", nil)
	srvB := testServer("b", nil)

	b := newTestBroker(t, []config.ServerConfig{srvA, srvB}, brokerOpts{})

	// Both running.
	if _, err := b.ListTools(bg(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ListTools(bg(), "b"); err != nil {
		t.Fatal(err)
	}

	// New config: b disabled.
	srvBDisabled := srvB
	srvBDisabled.Enabled = false
	newCfg := &config.Config{
		Proxy:   b.cfg.Get().Proxy,
		Servers: []config.ServerConfig{srvA, srvBDisabled},
	}
	oldCfg := b.cfg.Get()
	b.cfg.Swap(newCfg)
	b.ApplyConfig(oldCfg, newCfg)

	if b.Status("a").Running != true {
		t.Fatalf("a should still be running")
	}
	if b.Status("b").Running {
		t.Fatalf("b should be stopped after disable")
	}
}

func TestApplyConfig_RestartsOnCommandChange(t *testing.T) {
	srvA := testServer("a", map[string]string{"MOCK_TOOLS": "one"})
	b := newTestBroker(t, []config.ServerConfig{srvA}, brokerOpts{})

	if _, err := b.ListTools(bg(), "a"); err != nil {
		t.Fatal(err)
	}

	srvA2 := srvA
	srvA2.Env = map[string]string{"MOCK_TOOLS": "two"}
	newCfg := &config.Config{
		Proxy:   b.cfg.Get().Proxy,
		Servers: []config.ServerConfig{srvA2},
	}
	oldCfg := b.cfg.Get()
	b.cfg.Swap(newCfg)
	b.ApplyConfig(oldCfg, newCfg)

	// Next ListTools must reflect new env (lazy restart).
	tools, err := b.ListTools(bg(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "two" {
		t.Fatalf("expected single 'two' tool, got %+v", tools)
	}
}

// bg is a tiny helper to avoid importing context in every call site.
func bg() context.Context { return context.Background() }

func TestApplyConfig_NoChangeKeepsRunning(t *testing.T) {
	srv := testServer("a", nil)
	b := newTestBroker(t, []config.ServerConfig{srv}, brokerOpts{})

	if _, err := b.ListTools(bg(), "a"); err != nil {
		t.Fatal(err)
	}

	oldCfg := b.cfg.Get()
	newCfg := &config.Config{
		Proxy:   oldCfg.Proxy,
		Servers: []config.ServerConfig{srv}, // identical
	}
	b.cfg.Swap(newCfg)
	b.ApplyConfig(oldCfg, newCfg)

	if !b.Status("a").Running {
		t.Fatalf("unchanged server should keep running")
	}
}

func TestApplyConfig_RemovesServer(t *testing.T) {
	srvA := testServer("a", nil)
	srvB := testServer("b", nil)
	b := newTestBroker(t, []config.ServerConfig{srvA, srvB}, brokerOpts{})

	if _, err := b.ListTools(bg(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ListTools(bg(), "b"); err != nil {
		t.Fatal(err)
	}

	// New config omits b entirely.
	oldCfg := b.cfg.Get()
	newCfg := &config.Config{
		Proxy:   oldCfg.Proxy,
		Servers: []config.ServerConfig{srvA},
	}
	b.cfg.Swap(newCfg)
	b.ApplyConfig(oldCfg, newCfg)

	if !b.Status("a").Running {
		t.Fatalf("a should still be running")
	}
	if b.Status("b").Running {
		t.Fatalf("b should be stopped after removal")
	}
}

func TestApplyConfig_ReenabledServerStartsLazily(t *testing.T) {
	srv := testServer("a", nil)
	srv.Enabled = false
	b := newTestBroker(t, []config.ServerConfig{srv}, brokerOpts{})

	// Not running: disabled.
	if b.Status("a").Running {
		t.Fatalf("disabled server should not be running")
	}

	// Flip enabled=true via ApplyConfig.
	srvEnabled := srv
	srvEnabled.Enabled = true
	oldCfg := b.cfg.Get()
	newCfg := &config.Config{
		Proxy:   oldCfg.Proxy,
		Servers: []config.ServerConfig{srvEnabled},
	}
	b.cfg.Swap(newCfg)
	b.ApplyConfig(oldCfg, newCfg)

	// Still not running — lazy start happens on Ensure.
	if b.Status("a").Running {
		t.Fatalf("re-enabled server should not start until first call")
	}
	if _, err := b.ListTools(bg(), "a"); err != nil {
		t.Fatalf("ListTools after re-enable: %v", err)
	}
	if !b.Status("a").Running {
		t.Fatalf("server should be running after first call")
	}
}

func TestServerRestartRequired(t *testing.T) {
	base := config.ServerConfig{
		Name:      "x",
		Transport: "stdio",
		Command:   "/bin/true",
		Args:      []string{"a", "b"},
		Env:       map[string]string{"K": "V"},
	}

	tests := []struct {
		name   string
		modify func(*config.ServerConfig)
		want   bool
	}{
		{"identical", func(_ *config.ServerConfig) {}, false},
		{"command changed", func(s *config.ServerConfig) { s.Command = "/bin/false" }, true},
		{"args changed", func(s *config.ServerConfig) { s.Args = []string{"a", "c"} }, true},
		{"args order", func(s *config.ServerConfig) { s.Args = []string{"b", "a"} }, true},
		{"args length", func(s *config.ServerConfig) { s.Args = []string{"a"} }, true},
		{"env value", func(s *config.ServerConfig) { s.Env = map[string]string{"K": "W"} }, true},
		{"env new key", func(s *config.ServerConfig) { s.Env = map[string]string{"K": "V", "K2": "W"} }, true},
		{"description only", func(s *config.ServerConfig) { s.Description = "new" }, false},
		{"tags only", func(s *config.ServerConfig) { s.Tags = []string{"x"} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := base
			tt.modify(&other)
			if got := serverRestartRequired(base, other); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}
