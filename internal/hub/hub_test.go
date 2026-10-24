package hub

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/mcp"
	"github.com/subconscious-labs/lazy-mcp/internal/policy"
)

func tokenHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newTestHub(t *testing.T, cfg *config.Config) (*Hub, *httptest.Server) {
	t.Helper()
	holder := config.NewHolder(cfg)
	pol := policy.New(holder)
	srv := mcp.NewServer(holder, nil, nil, pol, logx.New(io.Discard), nil)
	h := New(holder, srv, logx.New(io.Discard))
	ts := httptest.NewServer(h.authMiddleware(http.HandlerFunc(h.handleMCP)))
	t.Cleanup(ts.Close)
	return h, ts
}

func basicCfg() *config.Config {
	return &config.Config{
		Proxy: config.ProxyConfig{
			ToolPrefix: "meta",
			HTTP: config.HTTPConfig{
				AllowedOrigins: []string{"http://localhost:*"},
				SessionTTL:     time.Minute,
			},
		},
	}
}

func TestAuth_Required(t *testing.T) {
	cfg := basicCfg()
	cfg.Auth = config.AuthConfig{
		Enabled: true,
		Users: []config.AuthUserConfig{
			{Name: "agent1", TokenSHA256: tokenHash("sekret"), Role: "agent"},
		},
	}
	_, ts := newTestHub(t, cfg)

	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", resp.StatusCode)
	}

	req2, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req2.Header.Set("Authorization", "Bearer sekret")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("valid token: got %d, want 200", resp2.StatusCode)
	}
	if got := resp2.Header.Get("Mcp-Session-Id"); got == "" {
		t.Fatalf("missing Mcp-Session-Id on new session")
	}
}

func TestAuth_BadToken(t *testing.T) {
	cfg := basicCfg()
	cfg.Auth = config.AuthConfig{
		Enabled: true,
		Users: []config.AuthUserConfig{
			{Name: "agent1", TokenSHA256: tokenHash("right"), Role: "agent"},
		},
	}
	_, ts := newTestHub(t, cfg)

	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", resp.StatusCode)
	}
}

func TestSession_Lifecycle(t *testing.T) {
	_, ts := newTestHub(t, basicCfg())

	// Create session.
	req, _ := http.NewRequest("POST", ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no session id")
	}

	// Second POST with that id: should pass.
	req2, _ := http.NewRequest("POST", ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req2.Header.Set("Mcp-Session-Id", sid)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: got %d", resp2.StatusCode)
	}

	// DELETE it.
	reqD, _ := http.NewRequest("DELETE", ts.URL+"/mcp", nil)
	reqD.Header.Set("Mcp-Session-Id", sid)
	respD, _ := http.DefaultClient.Do(reqD)
	respD.Body.Close()
	if respD.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", respD.StatusCode)
	}

	// Reusing the id now: 404.
	req3, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{}`))
	req3.Header.Set("Mcp-Session-Id", sid)
	resp3, _ := http.DefaultClient.Do(req3)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("reused session: got %d, want 404", resp3.StatusCode)
	}
}

func TestSSE_RequiresSession(t *testing.T) {
	_, ts := newTestHub(t, basicCfg())
	req, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET without session: got %d, want 400", resp.StatusCode)
	}
}

func TestBroadcast_DeliveredToAllSessions(t *testing.T) {
	h, ts := newTestHub(t, basicCfg())

	// Create two sessions.
	mkSession := func() string {
		req, _ := http.NewRequest("POST", ts.URL+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.Header.Get("Mcp-Session-Id")
	}
	sid1 := mkSession()
	sid2 := mkSession()

	// Open SSE on both.
	openSSE := func(sid string) (*http.Response, *bufio.Reader) {
		req, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
		req.Header.Set("Mcp-Session-Id", sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, bufio.NewReader(resp.Body)
	}
	resp1, r1 := openSSE(sid1)
	defer resp1.Body.Close()
	resp2, r2 := openSSE(sid2)
	defer resp2.Body.Close()

	time.Sleep(50 * time.Millisecond)

	// Broadcast a notification.
	h.OnDownstreamNotification("mock", "notifications/message",
		json.RawMessage(`{"level":"info","data":"ping"}`))

	readData := func(r *bufio.Reader) string {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return ""
			}
			if strings.HasPrefix(line, "data: ") {
				return strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
			}
		}
	}

	got1 := readData(r1)
	got2 := readData(r2)
	for i, got := range []string{got1, got2} {
		if got == "" {
			t.Fatalf("session %d: no data", i+1)
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(got), &msg); err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
		if msg["method"] != "notifications/message" {
			t.Fatalf("session %d: method=%v", i+1, msg["method"])
		}
		params := msg["params"].(map[string]any)
		if params["logger"] != "event" {
			t.Fatalf("session %d: logger=%v", i+1, params["logger"])
		}
		data, ok := params["data"].(map[string]any)
		if !ok {
			t.Fatalf("session %d: data is not an object: %T", i+1, params["data"])
		}
		if data["source"] != "mock" {
			t.Fatalf("session %d: source=%v", i+1, data["source"])
		}
		if data["payload"] != "ping" {
			t.Fatalf("session %d: payload=%v", i+1, data["payload"])
		}
	}
}

func TestOrigin_Rejected(t *testing.T) {
	cfg := basicCfg()
	cfg.Proxy.HTTP.AllowedOrigins = []string{"https://app.example.com"}
	_, ts := newTestHub(t, cfg)

	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Origin", "https://evil.example.com")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad origin: got %d, want 403", resp.StatusCode)
	}

	req2, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{}`))
	req2.Header.Set("Origin", "https://app.example.com")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusForbidden {
		t.Fatalf("allowed origin got rejected")
	}
}
