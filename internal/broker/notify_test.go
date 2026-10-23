package broker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
)

func TestBroker_DispatchesNotifications(t *testing.T) {
	var mu sync.Mutex
	var seen []struct {
		server, method string
		params         string
	}
	h := func(server, method string, params json.RawMessage) {
		mu.Lock()
		seen = append(seen, struct {
			server, method string
			params         string
		}{server, method, string(params)})
		mu.Unlock()
	}

	b := newTestBroker(t, []config.ServerConfig{
		testServer("mock", map[string]string{"MOCK_NOTIFY_MESSAGE": "hello"}),
	}, brokerOpts{})
	b.SetNotificationHandler(h)

	if _, err := b.CallTool(context.Background(), "mock", "echo", nil); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatalf("no notifications received")
	}
	first := seen[0]
	if first.server != "mock" {
		t.Fatalf("server: %q", first.server)
	}
	if first.method != "notifications/message" {
		t.Fatalf("method: %q", first.method)
	}
	var p struct {
		Level string          `json:"level"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(first.params), &p); err != nil {
		t.Fatalf("params: %v", err)
	}
	if p.Level != "info" {
		t.Fatalf("level: %q", p.Level)
	}
	if string(p.Data) != `"hello"` {
		t.Fatalf("data: %q", p.Data)
	}
}

func TestBroker_SetNotificationHandler_PropagatesToRunning(t *testing.T) {
	b := newTestBroker(t, []config.ServerConfig{testServer("mock", nil)}, brokerOpts{})

	if _, err := b.ListTools(context.Background(), "mock"); err != nil {
		t.Fatal(err)
	}

	var hits int
	var mu sync.Mutex
	b.SetNotificationHandler(func(_ string, _ string, _ json.RawMessage) {
		mu.Lock()
		hits++
		mu.Unlock()
	})

	// No error expected: just ensuring the propagation code runs.
	mu.Lock()
	_ = hits
	mu.Unlock()
}
