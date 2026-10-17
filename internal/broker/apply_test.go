package broker

import (
	"context"
	"testing"

	"github.com/nikita-popov/lazy-mcp/internal/config"
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
