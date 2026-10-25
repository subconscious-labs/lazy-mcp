package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/subconscious-labs/lazy-mcp/internal/broker"
	"github.com/subconscious-labs/lazy-mcp/internal/catalog"
	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/policy"
	"github.com/subconscious-labs/lazy-mcp/internal/session"
)

func TestWrapResult(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantRaw bool   // expect json.RawMessage
		want    string // expected value (string body or raw JSON)
	}{
		{"plain string", "hello world", false, "hello world"},
		{"empty", "", false, ""},
		{"object", `{"a":1}`, true, `{"a":1}`},
		{"array", `[1,2,3]`, true, `[1,2,3]`},
		{"object with whitespace", "  {\"a\":1}  ", true, `{"a":1}`},
		{"invalid json", `{not valid}`, false, `{not valid}`},
		{"truncated json", `{broken`, false, `{broken`},
		{"starts with brace but text", "{oops", false, "{oops"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapResult(tt.in)
			if tt.wantRaw {
				raw, ok := got.(json.RawMessage)
				if !ok {
					t.Fatalf("got %T, want json.RawMessage", got)
				}
				if !bytes.Equal(raw, []byte(tt.want)) {
					t.Fatalf("got %q, want %q", raw, tt.want)
				}
				return
			}
			s, ok := got.(string)
			if !ok {
				t.Fatalf("got %T, want string", got)
			}
			if s != tt.want {
				t.Fatalf("got %q, want %q", s, tt.want)
			}
		})
	}
}

func newTestHandlers() *Handlers {
	holder := config.NewHolder(&config.Config{
		Proxy: config.ProxyConfig{
			MaxResultBytes: 4096,
			CursorTTL:      time.Minute,
		},
	})
	return &Handlers{
		cfg:    holder,
		log:    logx.New(io.Discard),
		policy: policy.New(holder),
		pager:  NewPageStore(time.Minute),
		// reloadFn nil: meta_reload returns "not available" in
		// handlers built this way. Tests that need reload build
		// Handlers explicitly.
	}
}

func TestBuildResponse_NoPageWhenSinglePage(t *testing.T) {
	h := newTestHandlers()
	out, err := h.buildResponse("short result", 0, "", "srv", "tool")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if _, ok := parsed["page"]; ok {
		t.Fatalf("page should be omitted when not paginating: %s", out)
	}
	if _, ok := parsed["has_more"]; ok {
		t.Fatalf("has_more should be omitted when false: %s", out)
	}
	if _, ok := parsed["hint"]; ok {
		t.Fatalf("hint should be omitted when no auto-paging: %s", out)
	}
	if got := parsed["result"]; got != "short result" {
		t.Fatalf("result: got %v", got)
	}
}

func TestBuildResponse_PageWhenPaginating(t *testing.T) {
	h := newTestHandlers()
	big := strings.Repeat("x", 10000)
	out, err := h.buildResponse(big, 1000, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["page"] != float64(1) {
		t.Fatalf("page: got %v, want 1", parsed["page"])
	}
	if parsed["has_more"] != true {
		t.Fatalf("has_more: got %v, want true", parsed["has_more"])
	}
	if parsed["next_cursor"] == "" {
		t.Fatalf("next_cursor should be set")
	}
	if _, ok := parsed["hint"]; ok {
		t.Fatalf("hint should be absent when user requested pagination: %s", out)
	}
}

func TestBuildResponse_AutoPaginateOnMaxBytes(t *testing.T) {
	h := newTestHandlers() // MaxResultBytes: 4096
	// Result larger than MaxResultBytes, no page_size requested.
	body := strings.Repeat("line\n", 2000) // 10000 bytes
	out, err := h.buildResponse(body, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["has_more"] != true {
		t.Fatalf("expected has_more: %s", out)
	}
	if parsed["next_cursor"] == "" {
		t.Fatalf("expected next_cursor: %s", out)
	}
	if parsed["page"] != float64(1) {
		t.Fatalf("page: got %v want 1", parsed["page"])
	}
	hint, _ := parsed["hint"].(string)
	if !strings.Contains(hint, "max_result_bytes") {
		t.Fatalf("hint should mention max_result_bytes, got: %q", hint)
	}
}

func TestBuildResponse_NoAutoPageWhenFits(t *testing.T) {
	h := newTestHandlers()
	small := strings.Repeat("x", 100) // < MaxResultBytes 4096
	out, err := h.buildResponse(small, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed["has_more"]; ok {
		t.Fatalf("has_more should be absent when result fits: %s", out)
	}
	if _, ok := parsed["hint"]; ok {
		t.Fatalf("hint should be absent when no auto-paging: %s", out)
	}
}

func TestBuildResponse_EmbedsJSONResult(t *testing.T) {
	h := newTestHandlers()
	out, err := h.buildResponse(`{"status":"ok","n":3}`, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	nested, ok := parsed["result"].(map[string]any)
	if !ok {
		t.Fatalf("result should be an object, got %T: %v", parsed["result"], parsed["result"])
	}
	if nested["status"] != "ok" {
		t.Fatalf("unexpected nested: %v", nested)
	}
}

// --- pre-existing tests, unchanged ---

func TestTruncateBytes_ShortStringUntouched(t *testing.T) {
	s := "hello"
	if got := truncateBytes(s, 100); got != s {
		t.Fatalf("want %q, got %q", s, got)
	}
}

func TestTruncateBytes_ZeroLimitNoLimit(t *testing.T) {
	s := strings.Repeat("x", 1000)
	if got := truncateBytes(s, 0); got != s {
		t.Fatalf("limit=0 should be no-op")
	}
}

func TestTruncateBytes_CutsAtRuneBoundary(t *testing.T) {
	s := "Привет"
	got := truncateBytes(s, 5)
	body := strings.TrimSuffix(got, "\n… [truncated]")
	if !utf8.ValidString(body) {
		t.Fatalf("output is not valid UTF-8: %q", got)
	}
	if len(body) > 5 {
		t.Fatalf("body exceeds limit: %d bytes", len(body))
	}
}

func TestSplitRef(t *testing.T) {
	tests := []struct {
		in           string
		server, tool string
		wantErr      bool
	}{
		{"ssh/ssh_exec", "ssh", "ssh_exec", false},
		{"a/b/c", "a", "b/c", false},
		{"no-slash", "", "", true},
		{"", "", "", true},
	}
	for _, tt := range tests {
		s, tool, err := splitRef(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("splitRef(%q) err=%v, wantErr=%v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && (s != tt.server || tool != tt.tool) {
			t.Errorf("splitRef(%q) = %q, %q; want %q, %q", tt.in, s, tool, tt.server, tt.tool)
		}
	}
}

func TestPageStore_SinglePageNoCursor(t *testing.T) {
	ps := NewPageStore(time.Minute)
	first, cur, more := ps.Store("short", 100, "", "", "")
	if first != "short" || cur != "" || more {
		t.Fatalf("got %q, %q, %v", first, cur, more)
	}
}

func TestPageStore_MultiPage(t *testing.T) {
	ps := NewPageStore(time.Minute)
	content := strings.Repeat("line\n", 100)

	first, cur, more := ps.Store(content, 120, "", "", "")
	if !more || cur == "" {
		t.Fatalf("expected multi-page, got more=%v cur=%q", more, cur)
	}
	if len(first) == 0 {
		t.Fatal("first page empty")
	}

	total := len(first)
	for cur != "" {
		page, next, more, _, _, _, ok := ps.Next(cur)
		if !ok {
			t.Fatal("cursor rejected")
		}
		total += len(page)
		if !more {
			cur = ""
		} else {
			cur = next
		}
	}
	if total != len(content) {
		t.Fatalf("reassembled length %d, want %d", total, len(content))
	}
}

func TestPageStore_IdempotentCursor(t *testing.T) {
	ps := NewPageStore(time.Minute)
	content := strings.Repeat("line\n", 100) // 500 bytes
	_, cur, more := ps.Store(content, 120, "", "", "")
	if !more {
		t.Fatal("expected multi-page")
	}

	// Fetch page 2 twice — must yield the same bytes.
	page1, next1, _, _, _, _, ok := ps.Next(cur)
	if !ok {
		t.Fatal("first Next failed")
	}
	page2, next2, _, _, _, _, ok := ps.Next(cur)
	if !ok {
		t.Fatal("second Next failed")
	}
	if page1 != page2 {
		t.Fatalf("cursor not idempotent:\nfirst=%q\nsecond=%q", page1, page2)
	}
	if next1 != next2 {
		t.Fatalf("next_cursor differs across identical requests: %q vs %q", next1, next2)
	}
}

func TestPageStore_NextByArbitraryIndex(t *testing.T) {
	ps := NewPageStore(time.Minute)
	content := strings.Repeat("line\n", 100)
	_, cur, _ := ps.Store(content, 120, "", "", "")

	// Extract session id from cur and jump to page 3 directly.
	id, _, err := parseCursor(cur)
	if err != nil {
		t.Fatal(err)
	}
	page3, _, _, _, _, _, ok := ps.Next(cursorFor(id, 3))
	if !ok {
		t.Fatal("direct page 3 fetch failed")
	}
	if page3 == "" {
		t.Fatal("page 3 empty")
	}
}

func TestParseCursor_RejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "nosep", "abc.def", "abc."} {
		if _, _, err := parseCursor(s); err == nil {
			t.Errorf("parseCursor(%q) should fail", s)
		}
	}
}

func TestPageStore_UnknownCursor(t *testing.T) {
	ps := NewPageStore(time.Minute)
	_, _, _, _, _, _, ok := ps.Next("nope")
	if ok {
		t.Fatal("unknown cursor should be rejected")
	}
}

func TestSplitRef_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string // substring of the expected error
	}{
		{"no slash", "no-slash", "expected server/tool"},
		{"empty", "", "expected server/tool"},
		{"empty server", "/tool", "server part is empty"},
		{"empty tool", "server/", "tool part is empty"},
		{"newline in server", "ser\nver/tool", "control character"},
		{"newline in tool", "server/to\nol", "control character"},
		{"nul in tool", "server/tool\x00", "control character"},
		{"leading space", " server/tool", "whitespace"},
		{"trailing space", "server/tool ", "whitespace"},
		{"server too long", strings.Repeat("a", 300) + "/tool", "exceeds 256 bytes"},
		{"tool too long", "server/" + strings.Repeat("a", 300), "exceeds 256 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := splitRef(tt.in)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestSplitRef_AcceptsValid(t *testing.T) {
	tests := []struct {
		in           string
		server, tool string
	}{
		{"ssh/ssh_exec", "ssh", "ssh_exec"},
		{"git/list_my_repositories", "git", "list_my_repositories"},
		{"a/b/c", "a", "b/c"}, // tool names may contain '/'
		{"server/tool-with-dash", "server", "tool-with-dash"},
		{"server/tool.with.dots", "server", "tool.with.dots"},
	}
	for _, tt := range tests {
		s, tool, err := splitRef(tt.in)
		if err != nil {
			t.Errorf("splitRef(%q) err=%v", tt.in, err)
			continue
		}
		if s != tt.server || tool != tt.tool {
			t.Errorf("splitRef(%q) = %q, %q; want %q, %q",
				tt.in, s, tool, tt.server, tt.tool)
		}
	}
}

func TestCall_RejectsOversizedArguments(t *testing.T) {
	holder := config.NewHolder(&config.Config{
		Proxy: config.ProxyConfig{MaxArgumentsBytes: 128},
		Servers: []config.ServerConfig{
			{Name: "server", Enabled: true},
		},
	})
	h := &Handlers{
		cfg:    holder,
		policy: policy.New(holder),
	}
	big := strings.Repeat("x", 200)
	_, err := h.Call(context.Background(), map[string]any{
		"tool_ref":  "server/tool",
		"arguments": map[string]any{"payload": big},
	})
	if err == nil {
		t.Fatal("expected error for oversized arguments")
	}
	if !strings.Contains(err.Error(), "arguments too large") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCall_DenyDestructive(t *testing.T) {
	holder := config.NewHolder(&config.Config{
		Proxy: config.ProxyConfig{DenyDestructive: true},
		Servers: []config.ServerConfig{
			{Name: "server", Enabled: true},
		},
	})
	pol := policy.New(holder)
	cat := catalog.New(holder, nil, pol, logx.New(io.Discard))
	cat.InjectEntries([]catalog.ToolEntry{
		{
			Ref:         "server/danger",
			Server:      "server",
			Tool:        "danger",
			Annotations: broker.ToolAnnotations{DestructiveHint: true},
		},
	})
	h := &Handlers{
		cfg:    holder,
		cat:    cat,
		policy: pol,
	}
	_, err := h.Call(context.Background(), map[string]any{
		"tool_ref":  "server/danger",
		"arguments": map[string]any{},
	})
	if err == nil || !strings.Contains(err.Error(), "destructive") {
		t.Fatalf("expected destructive error, got %v", err)
	}
}

func TestListServers_DoesNotMutateConfig(t *testing.T) {
	holder := config.NewHolder(&config.Config{
		Servers: []config.ServerConfig{
			{Name: "a", Enabled: true},
			{Name: "b", Enabled: true},
			{Name: "c", Enabled: true},
		},
	})
	h := &Handlers{cfg: holder, log: logx.New(io.Discard)}

	ctx := session.WithContext(context.Background(),
		session.New("s1", "elsa", "agent", []string{"b"}))

	// Запрос с include_disabled=true (именно он ломал слайс)
	_, err := h.ListServers(ctx, map[string]any{"include_disabled": true})
	if err != nil {
		t.Fatal(err)
	}

	// Проверяем, что в конфиге всё ещё a, b, c, а не b, b, c
	cfgServers := holder.Get().Servers
	if len(cfgServers) != 3 {
		t.Fatalf("config mutated: len=%d", len(cfgServers))
	}
	if cfgServers[0].Name != "a" || cfgServers[1].Name != "b" || cfgServers[2].Name != "c" {
		t.Fatalf("config mutated: %v", cfgServers)
	}
}

func TestCursor_BoundToUser(t *testing.T) {
	holder := config.NewHolder(&config.Config{
		Proxy: config.ProxyConfig{MaxResultBytes: 1024},
		Servers: []config.ServerConfig{
			{Name: "srv", Enabled: true},
		},
	})
	pol := policy.New(holder)
	h := &Handlers{
		cfg:    holder,
		policy: pol,
		pager:  NewPageStore(time.Minute),
	}

	// Создаём страницу от имени userA
	out, err := h.buildResponse(strings.Repeat("x", 2000), 1000, "userA", "srv", "tool")
	if err != nil {
		t.Fatal(err)
	}
	var resp callResult
	json.Unmarshal([]byte(out), &resp)

	// userB пытается продолжить
	_, err = h.callCursorNext(context.Background(), resp.NextCursor, "userB", nil)
	if err == nil || !strings.Contains(err.Error(), "not found or expired") {
		t.Fatalf("userB should not be able to use userA's cursor, got: %v", err)
	}
}
