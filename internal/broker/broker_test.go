package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

// mockMCPPath is set by TestMain; absolute path to the compiled mock server.
var mockMCPPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "lazy-mcp-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	mockMCPPath = filepath.Join(tmp, "mockmcp")
	build := exec.Command("go", "build", "-o", mockMCPPath, "./testdata/mockmcp")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build mockmcp:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// ---- helpers ----

// testServer builds a ServerConfig that runs the mock with the given env.
func testServer(name string, env map[string]string) config.ServerConfig {
	return config.ServerConfig{
		Name:      name,
		Transport: "stdio",
		Command:   mockMCPPath,
		Env:       env,
		Enabled:   true,
	}
}

// testBroker builds a Broker with tight timeouts suitable for tests.
// circuitThreshold / cooldown are overridable via opts.
type brokerOpts struct {
	execTimeout      time.Duration
	initTimeout      time.Duration
	circuitThreshold int
	circuitCooldown  time.Duration
	circuitDisabled  bool
}

func newTestBroker(t *testing.T, servers []config.ServerConfig, opts brokerOpts) *Broker {
	t.Helper()
	if opts.execTimeout == 0 {
		opts.execTimeout = 2 * time.Second
	}
	if opts.initTimeout == 0 {
		opts.initTimeout = 2 * time.Second
	}
	if opts.circuitThreshold == 0 {
		opts.circuitThreshold = 3
	}
	if opts.circuitCooldown == 0 {
		opts.circuitCooldown = 200 * time.Millisecond
	}

	cb := config.CircuitBreakerConfig{
		FailureThreshold: opts.circuitThreshold,
		Cooldown:         opts.circuitCooldown,
	}
	if opts.circuitDisabled {
		f := false
		cb.Enabled = &f
	}

	cfg := &config.Config{
		Proxy: config.ProxyConfig{
			ToolPrefix:        "meta",
			ExecTimeout:       opts.execTimeout,
			InitializeTimeout: opts.initTimeout,
			IdleTTL:           time.Hour,
			ToolCacheTTL:      time.Hour,
			CursorTTL:         time.Minute,
			ShutdownTimeout:   time.Second,
			CircuitBreaker:    cb,
		},
		Servers: servers,
	}

	b := New(cfg, logx.New(io.Discard))
	t.Cleanup(b.StopAll)
	return b
}

// ---- lifecycle ----

func TestEnsure_StartsAndListsTools(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", map[string]string{"MOCK_TOOLS": "alpha,beta"}),
	}, brokerOpts{})

	ctx := context.Background()
	tools, err := b.ListTools(ctx, "mock")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "alpha" || tools[1].Name != "beta" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
	if !b.Status("mock").Running {
		t.Fatalf("backend should be running after ListTools")
	}
}

func TestEnsure_DisabledRejected(t *testing.T) {
	srv := testServer("mock", nil)
	srv.Enabled = false
	b := newTestBroker(t, []config.ServerConfig{srv}, brokerOpts{})

	_, err := b.ListTools(context.Background(), "mock")
	if err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("expected 'is disabled' error, got %v", err)
	}
}

func TestEnsure_UnknownServer(t *testing.T) {
	b := newTestBroker(t, nil, brokerOpts{})
	_, err := b.ListTools(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "not found in config") {
		t.Fatalf("expected 'not found' error, got %v", err)
	}
}

func TestEnsure_InitializeTimeout(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("slow", map[string]string{
			"MOCK_INIT_DELAY": "2s",
		}),
	}, brokerOpts{initTimeout: 100 * time.Millisecond})

	_, err := b.ListTools(context.Background(), "slow")
	if err == nil {
		t.Fatal("expected initialize timeout")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func TestEnsure_RestartsAfterDeath(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", nil),
	}, brokerOpts{})

	ctx := context.Background()
	if _, err := b.ListTools(ctx, "mock"); err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Kill the underlying process from underneath the broker.
	b.mu.Lock()
	proc := b.pool["mock"]
	b.mu.Unlock()
	proc.kill()

	// Wait for readLoop to notice EOF and mark not ready.
	time.Sleep(50 * time.Millisecond)

	if b.Status("mock").Running {
		t.Fatalf("backend should not be running after kill")
	}

	// Next call should transparently restart.
	if _, err := b.ListTools(ctx, "mock"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if !b.Status("mock").Running {
		t.Fatalf("backend should be running after restart")
	}
}

// ---- call semantics ----

func TestCallTool_Success(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", nil),
	}, brokerOpts{})

	out, err := b.CallTool(context.Background(), "mock", "echo", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(out, "called echo") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestCallTool_ToolErrorIsSentinel(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", map[string]string{"MOCK_CALL_ERROR": "boom"}),
	}, brokerOpts{})

	_, err := b.CallTool(context.Background(), "mock", "echo", nil)
	if err == nil {
		t.Fatal("expected tool error")
	}
	if !errors.Is(err, ErrToolError) {
		t.Fatalf("expected ErrToolError, got %v", err)
	}
}

func TestCallTool_ExecTimeout(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("slow", map[string]string{"MOCK_CALL_DELAY": "1s"}),
	}, brokerOpts{execTimeout: 100 * time.Millisecond})

	_, err := b.CallTool(context.Background(), "slow", "echo", nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestCallTool_Concurrent(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", nil),
	}, brokerOpts{})

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := b.CallTool(context.Background(), "mock", "echo",
				map[string]any{"i": i})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}
}

// ---- last_tool_call semantics ----

func TestLastToolCall_NotBumpedByListTools(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", nil),
	}, brokerOpts{})

	ctx := context.Background()
	if _, err := b.ListTools(ctx, "mock"); err != nil {
		t.Fatal(err)
	}
	// Repeated ListTools must not move last_tool_call.
	for i := 0; i < 3; i++ {
		_, _ = b.ListTools(ctx, "mock")
	}
	st := b.Status("mock")
	if !st.LastToolCall.IsZero() {
		t.Fatalf("ListTools should not bump last_tool_call, got %v", st.LastToolCall)
	}
}

func TestLastToolCall_BumpedByCall(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", nil),
	}, brokerOpts{})

	ctx := context.Background()
	before := time.Now()
	if _, err := b.CallTool(ctx, "mock", "echo", nil); err != nil {
		t.Fatal(err)
	}
	st := b.Status("mock")
	if st.LastToolCall.IsZero() {
		t.Fatal("last_tool_call should be set after CallTool")
	}
	if st.LastToolCall.Before(before) {
		t.Fatalf("last_tool_call %v should be after test start %v", st.LastToolCall, before)
	}
}

func TestLastToolCall_BumpedByToolError(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", map[string]string{"MOCK_CALL_ERROR": "boom"}),
	}, brokerOpts{})

	ctx := context.Background()
	_, _ = b.CallTool(ctx, "mock", "echo", nil)
	st := b.Status("mock")
	if st.LastToolCall.IsZero() {
		t.Fatal("tool-level error should still bump last_tool_call")
	}
}

// ---- circuit breaker ----

func TestCircuit_TripsAfterThreshold(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("dead", map[string]string{"MOCK_DIE_ON_INIT": "1"}),
	}, brokerOpts{circuitThreshold: 3})

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, _ = b.ListTools(ctx, "dead")
	}
	if got := b.CircuitState("dead"); got != "open" {
		t.Fatalf("circuit: want open, got %s", got)
	}
}

func TestCircuit_OpenRejectsImmediately(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("dead", map[string]string{"MOCK_DIE_ON_INIT": "1"}),
	}, brokerOpts{circuitThreshold: 2, circuitCooldown: time.Hour})

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, _ = b.ListTools(ctx, "dead")
	}

	start := time.Now()
	_, err := b.CallTool(ctx, "dead", "echo", nil)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("open circuit should reject immediately, took %v", elapsed)
	}
}

func TestCircuit_HalfOpenProbe(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("dead", map[string]string{"MOCK_DIE_ON_INIT": "1"}),
	}, brokerOpts{circuitThreshold: 2, circuitCooldown: 100 * time.Millisecond})

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		_, _ = b.ListTools(ctx, "dead")
	}
	if got := b.CircuitState("dead"); got != "open" {
		t.Fatalf("want open, got %s", got)
	}

	time.Sleep(150 * time.Millisecond) // past cooldown

	// Probe goes through, fails, circuit re-opens.
	_, _ = b.CallTool(ctx, "dead", "echo", nil)

	if got := b.CircuitState("dead"); got != "open" {
		t.Fatalf("after failed probe: want open, got %s", got)
	}
}

func TestCircuit_ToolErrorDoesNotTrip(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", map[string]string{"MOCK_CALL_ERROR": "business error"}),
	}, brokerOpts{circuitThreshold: 2})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _ = b.CallTool(ctx, "mock", "echo", nil)
	}
	if got := b.CircuitState("mock"); got != "closed" {
		t.Fatalf("tool-level errors should not trip circuit, got %s", got)
	}
}

func TestCircuit_Disabled(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("dead", map[string]string{"MOCK_DIE_ON_INIT": "1"}),
	}, brokerOpts{circuitDisabled: true})

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, _ = b.ListTools(ctx, "dead")
	}
	if got := b.CircuitState("dead"); got != "disabled" {
		t.Fatalf("want disabled, got %s", got)
	}
}

// ---- idle reaper ----

func TestIdleReaper_StopsIdleBackend(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", nil),
	}, brokerOpts{})

	ctx := context.Background()
	if _, err := b.CallTool(ctx, "mock", "echo", nil); err != nil {
		t.Fatal(err)
	}
	if !b.Status("mock").Running {
		t.Fatal("backend should be running")
	}

	// Rewind last activity to simulate idle.
	b.mu.Lock()
	proc := b.pool["mock"]
	b.mu.Unlock()
	proc.mu.Lock()
	proc.lastToolCall = time.Now().Add(-time.Hour)
	proc.startedAt = time.Now().Add(-time.Hour)
	proc.mu.Unlock()

	b.reapIdle(30 * time.Minute)

	if b.Status("mock").Running {
		t.Fatal("idle backend should have been stopped")
	}

	// Next call restarts it transparently.
	if _, err := b.CallTool(ctx, "mock", "echo", nil); err != nil {
		t.Fatalf("restart after reap: %v", err)
	}
	if !b.Status("mock").Running {
		t.Fatal("backend should be running after restart")
	}
}

func TestIdleReaper_FreshBackendNotKilled(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", nil),
	}, brokerOpts{})

	ctx := context.Background()
	if _, err := b.ListTools(ctx, "mock"); err != nil {
		t.Fatal(err)
	}

	// Fresh process, no calls, reaper should respect startedAt.
	b.reapIdle(30 * time.Minute)

	if !b.Status("mock").Running {
		t.Fatal("freshly started backend should not be killed")
	}
}
