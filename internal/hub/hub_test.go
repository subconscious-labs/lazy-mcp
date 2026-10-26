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

func doMCP(t *testing.T, base, token, sid, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", base+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSession_BoundToPrincipal(t *testing.T) {
	cfg := basicCfg()
	cfg.Auth = config.AuthConfig{
		Enabled: true,
		Users: []config.AuthUserConfig{
			{Name: "admin", TokenSHA256: tokenHash("admin-tok"), Role: "admin"},
			{Name: "elsa", TokenSHA256: tokenHash("elsa-tok"), Role: "agent",
				AllowedServers: []string{"git"}},
		},
	}
	_, ts := newTestHub(t, cfg)

	// Admin creates a session.
	respA := doMCP(t, ts.URL, "admin-tok", "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	adminSID := respA.Header.Get("Mcp-Session-Id")
	respA.Body.Close()
	if adminSID == "" {
		t.Fatal("no session")
	}

	// Elsa tries to use admin's session ID.
	respB := doMCP(t, ts.URL, "elsa-tok", adminSID, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	bodyB, _ := io.ReadAll(respB.Body)
	respB.Body.Close()
	if respB.StatusCode != http.StatusNotFound {
		t.Fatalf("elsa using admin session: got %d, want 404; body=%s",
			respB.StatusCode, bodyB)
	}

	// Elsa tries to delete admin's session.
	reqD, _ := http.NewRequest("DELETE", ts.URL+"/mcp", nil)
	reqD.Header.Set("Authorization", "Bearer elsa-tok")
	reqD.Header.Set("Mcp-Session-Id", adminSID)
	respD, _ := http.DefaultClient.Do(reqD)
	respD.Body.Close()
	if respD.StatusCode != http.StatusNotFound {
		t.Fatalf("elsa deleting admin session: got %d, want 404", respD.StatusCode)
	}

	// Elsa tries to open SSE on admin's session.
	reqS, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
	reqS.Header.Set("Authorization", "Bearer elsa-tok")
	reqS.Header.Set("Mcp-Session-Id", adminSID)
	reqS.Header.Set("Accept", "text/event-stream")
	respS, _ := http.DefaultClient.Do(reqS)
	respS.Body.Close()
	if respS.StatusCode != http.StatusNotFound {
		t.Fatalf("elsa sse on admin session: got %d, want 404", respS.StatusCode)
	}

	// Admin's session is untouched.
	respC := doMCP(t, ts.URL, "admin-tok", adminSID, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	respC.Body.Close()
	if respC.StatusCode != http.StatusOK {
		t.Fatalf("admin session after attack: got %d, want 200", respC.StatusCode)
	}
}

func readSSEData(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return ""
		}
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		}
	}
}

func TestBroadcast_RespectsACL(t *testing.T) {
	cfg := basicCfg()
	// Настраиваем два сервера
	cfg.Servers = []config.ServerConfig{
		{Name: "public", Enabled: true},
		{Name: "secret", Enabled: true},
	}
	cfg.Auth = config.AuthConfig{
		Enabled: true,
		Users: []config.AuthUserConfig{
			{Name: "admin", TokenSHA256: tokenHash("admin-tok"), Role: "admin"},
			{Name: "elsa", TokenSHA256: tokenHash("elsa-tok"), Role: "agent", AllowedServers: []string{"public"}},
		},
	}
	h, ts := newTestHub(t, cfg)

	openSSE := func(token string) (*bufio.Reader, *http.Response) {
		resp := doMCP(t, ts.URL, token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
		sid := resp.Header.Get("Mcp-Session-Id")
		resp.Body.Close()

		req, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Mcp-Session-Id", sid)
		req.Header.Set("Accept", "text/event-stream")
		respSSE, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return bufio.NewReader(respSSE.Body), respSSE
	}

	rAdmin, respAdmin := openSSE("admin-tok")
	defer respAdmin.Body.Close()
	rElsa, respElsa := openSSE("elsa-tok")
	defer respElsa.Body.Close()

	time.Sleep(50 * time.Millisecond)

	// Отправляем событие от сервера secret
	h.OnDownstreamNotification("secret", "notifications/message", json.RawMessage(`{"level":"info","data":"secret payload"}`))
	// Отправляем событие от сервера public
	h.OnDownstreamNotification("public", "notifications/message", json.RawMessage(`{"level":"info","data":"public payload"}`))

	// Админ должен получить оба (порядок не гарантирован)
	gotAdmin1 := readSSEData(t, rAdmin)
	gotAdmin2 := readSSEData(t, rAdmin)
	if !strings.Contains(gotAdmin1+gotAdmin2, "secret payload") || !strings.Contains(gotAdmin1+gotAdmin2, "public payload") {
		t.Fatalf("admin should receive both, got: %s | %s", gotAdmin1, gotAdmin2)
	}

	// Elsa должна получить только public payload (событие от secret для неё отфильтровано)
	gotElsa := readSSEData(t, rElsa)
	if !strings.Contains(gotElsa, "public payload") {
		t.Fatalf("elsa should receive public, got: %s", gotElsa)
	}
}

func TestPost_DoesNotCreateSessionOnInvalidJSON(t *testing.T) {
	h, ts := newTestHub(t, basicCfg())

	// Отправляем мусор без session ID
	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`not json`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", resp.StatusCode)
	}

	// Проверяем, что сессия не создалась
	h.mu.RLock()
	count := len(h.sessions)
	h.mu.RUnlock()
	if count != 0 {
		t.Fatalf("session created for invalid payload: %d", count)
	}
}

func TestSession_DeleteRemovesFromMap(t *testing.T) {
	h, ts := newTestHub(t, basicCfg())

	resp := doMCP(t, ts.URL, "", "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	reqD, _ := http.NewRequest("DELETE", ts.URL+"/mcp", nil)
	reqD.Header.Set("Mcp-Session-Id", sid)
	respD, _ := http.DefaultClient.Do(reqD)
	respD.Body.Close()
	if respD.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", respD.StatusCode)
	}

	h.mu.RLock()
	_, stillThere := h.sessions[sid]
	h.mu.RUnlock()
	if stillThere {
		t.Fatal("DELETE returned 204 but session remains in map")
	}
}

func TestRefreshSessionsAfterReload_Revokes(t *testing.T) {
	cfg := basicCfg()
	cfg.Servers = []config.ServerConfig{
		{Name: "secret", Enabled: true},
		{Name: "public", Enabled: true},
	}
	cfg.Auth = config.AuthConfig{
		Enabled: true,
		Users: []config.AuthUserConfig{
			{Name: "alice", TokenSHA256: tokenHash("alice-tok"), Role: "agent",
				AllowedServers: []string{"secret", "public"}},
		},
	}
	h, ts := newTestHub(t, cfg)

	// Alice opens SSE with access to [secret, public].
	resp := doMCP(t, ts.URL, "alice-tok", "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer alice-tok")
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Accept", "text/event-stream")
	respSSE, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer respSSE.Body.Close()
	reader := bufio.NewReader(respSSE.Body)

	time.Sleep(50 * time.Millisecond)

	// Reload: alice loses access to secret.
	oldCfg := h.cfg.Get()
	newCfg := &config.Config{
		Proxy:   oldCfg.Proxy,
		Servers: oldCfg.Servers,
		Auth: config.AuthConfig{
			Enabled: true,
			Users: []config.AuthUserConfig{
				{Name: "alice", TokenSHA256: tokenHash("alice-tok"), Role: "agent",
					AllowedServers: []string{"public"}},
			},
		},
	}
	h.cfg.Swap(newCfg)
	h.RefreshSessionsAfterReload()

	// Broadcast from secret — alice must NOT receive it.
	h.OnDownstreamNotification("secret", "notifications/message",
		json.RawMessage(`{"level":"info","data":"secret payload"}`))
	// Broadcast from public — alice MUST receive it.
	h.OnDownstreamNotification("public", "notifications/message",
		json.RawMessage(`{"level":"info","data":"public payload"}`))

	// Read one message: it must be the public one.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if strings.Contains(data, "secret payload") {
			t.Fatalf("alice received forbidden event after revocation: %s", data)
		}
		if strings.Contains(data, "public payload") {
			return // success
		}
	}
	t.Fatalf("public payload not received")
}

func TestRefreshSessionsAfterReload_ClosesRemovedUser(t *testing.T) {
	cfg := basicCfg()
	cfg.Auth = config.AuthConfig{
		Enabled: true,
		Users: []config.AuthUserConfig{
			{Name: "alice", TokenSHA256: tokenHash("alice-tok"), Role: "admin"},
		},
	}
	h, ts := newTestHub(t, cfg)

	resp := doMCP(t, ts.URL, "alice-tok", "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	// Reload: alice is removed.
	newCfg := &config.Config{
		Proxy:   cfg.Proxy,
		Servers: cfg.Servers,
		Auth: config.AuthConfig{
			Enabled: true,
			Users:   []config.AuthUserConfig{},
		},
	}
	h.cfg.Swap(newCfg)
	h.RefreshSessionsAfterReload()

	h.mu.RLock()
	_, stillThere := h.sessions[sid]
	h.mu.RUnlock()
	if stillThere {
		t.Fatal("session for removed user should have been closed")
	}
}
