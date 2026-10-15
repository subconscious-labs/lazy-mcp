package meta

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nikita-popov/lazy-mcp/internal/config"
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
	return &Handlers{
		cfg: &config.Config{
			Proxy: config.ProxyConfig{
				MaxResultBytes: 4096,
				CursorTTL:      time.Minute,
			},
		},
		pager: NewPageStore(time.Minute),
	}
}

func TestBuildResponse_NoPageWhenSinglePage(t *testing.T) {
	h := newTestHandlers()
	out, err := h.buildResponse("short result", 0, 1)
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
	if got := parsed["result"]; got != "short result" {
		t.Fatalf("result: got %v", got)
	}
}

func TestBuildResponse_PageWhenPaginating(t *testing.T) {
	h := newTestHandlers()
	big := strings.Repeat("x", 10000)
	out, err := h.buildResponse(big, 1000, 1)
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
}

func TestBuildResponse_EmbedsJSONResult(t *testing.T) {
	h := newTestHandlers()
	out, err := h.buildResponse(`{"status":"ok","n":3}`, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	// result must be an embedded object, not a stringified one.
	nested, ok := parsed["result"].(map[string]any)
	if !ok {
		t.Fatalf("result should be an object, got %T: %v", parsed["result"], parsed["result"])
	}
	if nested["status"] != "ok" {
		t.Fatalf("unexpected nested: %v", nested)
	}
}

func TestBuildResponse_TruncatedEmbedsJSON(t *testing.T) {
	h := newTestHandlers()
	// Valid JSON longer than 4096 bytes.
	body := `{"items":[` + strings.Repeat(`"x",`, 2000) + `"y"]}`
	if len(body) <= h.cfg.Proxy.MaxResultBytes {
		t.Fatalf("test body too short: %d", len(body))
	}
	out, err := h.buildResponse(body, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["truncated"] != true {
		t.Fatalf("truncated: got %v, want true", parsed["truncated"])
	}
	// Truncated body is invalid JSON (cut mid-string), so result must be string.
	if _, isStr := parsed["result"].(string); !isStr {
		t.Fatalf("truncated result should fall back to string, got %T", parsed["result"])
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
	first, cur, more := ps.Store("short", 100)
	if first != "short" || cur != "" || more {
		t.Fatalf("got %q, %q, %v", first, cur, more)
	}
}

func TestPageStore_MultiPage(t *testing.T) {
	ps := NewPageStore(time.Minute)
	content := strings.Repeat("line\n", 100)

	first, cur, more := ps.Store(content, 120)
	if !more || cur == "" {
		t.Fatalf("expected multi-page, got more=%v cur=%q", more, cur)
	}
	if len(first) == 0 {
		t.Fatal("first page empty")
	}

	total := len(first)
	for cur != "" {
		page, next, more, ok := ps.Next(cur)
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

func TestPageStore_UnknownCursor(t *testing.T) {
	ps := NewPageStore(time.Minute)
	_, _, _, ok := ps.Next("nope")
	if ok {
		t.Fatal("unknown cursor should be rejected")
	}
}
