package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
)

// fakeHandlers is a controllable MetaHandlers implementation for tests.
type fakeHandlers struct {
	searchFn       func(ctx context.Context, args map[string]any) (string, error)
	callFn         func(ctx context.Context, args map[string]any) (string, error)
	describeToolFn func(ctx context.Context, args map[string]any) (string, error)
	listServersFn  func(ctx context.Context, args map[string]any) (string, error)
	whoAmIFn       func(ctx context.Context, args map[string]any) (string, error)
	reloadFn       func(ctx context.Context, args map[string]any) (string, error)
}

func (f *fakeHandlers) Search(ctx context.Context, args map[string]any) (string, error) {
	if f.searchFn == nil {
		return "", errors.New("searchFn not set")
	}
	return f.searchFn(ctx, args)
}

func (f *fakeHandlers) Call(ctx context.Context, args map[string]any) (string, error) {
	if f.callFn == nil {
		return "", errors.New("callFn not set")
	}
	return f.callFn(ctx, args)
}

func (f *fakeHandlers) DescribeTool(ctx context.Context, args map[string]any) (string, error) {
	if f.describeToolFn == nil {
		return "", errors.New("describeToolFn not set")
	}
	return f.describeToolFn(ctx, args)
}

func (f *fakeHandlers) ListServers(ctx context.Context, args map[string]any) (string, error) {
	if f.listServersFn == nil {
		return "", errors.New("listServersFn not set")
	}
	return f.listServersFn(ctx, args)
}

func (f *fakeHandlers) WhoAmI(ctx context.Context, args map[string]any) (string, error) {
	if f.whoAmIFn == nil {
		return "", errors.New("whoAmIFn not set")
	}
	return f.whoAmIFn(ctx, args)
}

func (f *fakeHandlers) Reload(ctx context.Context, args map[string]any) (string, error) {
	if f.reloadFn == nil {
		return "reload-ok", nil
	}
	return f.reloadFn(ctx, args)
}

func testServer(t *testing.T, prefix string, h MetaHandlers) *Server {
	t.Helper()
	cfg := &config.Config{Proxy: config.ProxyConfig{ToolPrefix: prefix}}
	if h == nil {
		h = &fakeHandlers{}
	}
	return newServer(config.NewHolder(cfg), h, logx.New(io.Discard))
}

// runOnce feeds a raw JSONL request stream through Run and returns the
// decoded response records in order. Notifications produce no records.
func runOnce(t *testing.T, srv *Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	if err := srv.Run(context.Background(), in, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var records []map[string]any
	dec := json.NewDecoder(&out)
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode response: %v\n%s", err, out.String())
		}
		records = append(records, rec)
	}
	return records
}

// runOnceRaw is like runOnce but returns the raw JSON response lines,
// so tests can assert on exact bytes (e.g. large integer IDs).
func runOnceRaw(t *testing.T, srv *Server, lines ...string) []string {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	if err := srv.Run(context.Background(), in, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var raws []string
	sc := bufio.NewScanner(&out)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			raws = append(raws, line)
		}
	}
	return raws
}

// ---- protocol ----

func TestRun_Initialize(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec["id"] != float64(1) {
		t.Fatalf("id: %v", rec["id"])
	}
	if rec["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc: %v", rec["jsonrpc"])
	}
	result, ok := rec["result"].(map[string]any)
	if !ok {
		t.Fatalf("result: %T", rec["result"])
	}
	if result["protocolVersion"] != "2025-11-25" {
		t.Fatalf("protocolVersion: %v", result["protocolVersion"])
	}
	caps, ok := result["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing")
	}
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("capabilities.tools missing")
	}
	if _, ok := caps["logging"]; !ok {
		t.Fatalf("capabilities.logging missing — clients may drop notifications/message")
	}
	if _, ok := result["serverInfo"]; !ok {
		t.Fatalf("serverInfo missing")
	}
}

func TestRun_NotificationsInitialized_NoResponse(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
	)
	if len(recs) != 1 {
		t.Fatalf("notifications must not produce a response, got %d records", len(recs))
	}
}

func TestRun_ToolsList_Prefix(t *testing.T) {
	srv := testServer(t, "xp", nil)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	result := recs[0]["result"].(map[string]any)
	tools := result["tools"].([]any)

	// Exactly four meta-tools, all prefixed with "xp_".
	want := map[string]bool{
		"xp_search":        false,
		"xp_call":          false,
		"xp_describe_tool": false,
		"xp_list_servers":  false,
		"xp_whoami":        false,
		"xp_reload":        false,
	}
	for _, raw := range tools {
		obj := raw.(map[string]any)
		name, _ := obj["name"].(string)
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected tool name %q", name)
			continue
		}
		want[name] = true
		if _, ok := obj["inputSchema"]; !ok {
			t.Errorf("tool %q has no inputSchema", name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q missing from manifest", name)
		}
	}
}

func TestRun_UnknownMethod(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":1,"method":"bogus"}`)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record")
	}
	errObj := recs[0]["error"].(map[string]any)
	if errObj["code"] != float64(-32601) {
		t.Fatalf("code: %v", errObj["code"])
	}
	if !strings.Contains(errObj["message"].(string), "method not found") {
		t.Fatalf("message: %v", errObj["message"])
	}
}

// ---- tools/call dispatch ----

func TestRun_ToolsCall_Success(t *testing.T) {
	gotArgs := map[string]any{}
	h := &fakeHandlers{
		callFn: func(_ context.Context, args map[string]any) (string, error) {
			for k, v := range args {
				gotArgs[k] = v
			}
			return "called-ok", nil
		},
	}
	srv := testServer(t, "meta", h)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"meta_call","arguments":{"tool_ref":"git/x"}}}`)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record")
	}
	result := recs[0]["result"].(map[string]any)
	content := result["content"].([]any)
	item := content[0].(map[string]any)
	if item["text"] != "called-ok" {
		t.Fatalf("text: %v", item["text"])
	}
	if _, ok := result["isError"]; ok {
		t.Fatalf("isError should not be set on success")
	}
	if gotArgs["tool_ref"] != "git/x" {
		t.Fatalf("handler did not receive args: %v", gotArgs)
	}
}

func TestRun_ToolsCall_HandlerError(t *testing.T) {
	h := &fakeHandlers{
		callFn: func(_ context.Context, _ map[string]any) (string, error) {
			return "", errors.New("boom")
		},
	}
	srv := testServer(t, "meta", h)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"meta_call","arguments":{}}}`)
	result := recs[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("isError: %v", result["isError"])
	}
	content := result["content"].([]any)
	item := content[0].(map[string]any)
	if !strings.Contains(item["text"].(string), "boom") {
		t.Fatalf("error text: %v", item["text"])
	}
}

func TestRun_ToolsCall_UnknownMetaTool(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"meta_nope","arguments":{}}}`)
	result := recs[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("isError should be true")
	}
	content := result["content"].([]any)
	item := content[0].(map[string]any)
	if !strings.Contains(item["text"].(string), "unknown meta-tool") {
		t.Fatalf("text: %v", item["text"])
	}
}

func TestRun_ToolsCall_InvalidParams(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":"not-an-object"}`)
	errObj := recs[0]["error"].(map[string]any)
	if errObj["code"] != float64(-32602) {
		t.Fatalf("code: %v", errObj["code"])
	}
}

func TestRun_Dispatch_AllMetaTools(t *testing.T) {
	cases := []struct {
		name     string
		handler  *fakeHandlers
		wantText string
	}{
		{
			name:     "search",
			handler:  &fakeHandlers{searchFn: func(context.Context, map[string]any) (string, error) { return "search-ok", nil }},
			wantText: "search-ok",
		},
		{
			name:     "call",
			handler:  &fakeHandlers{callFn: func(context.Context, map[string]any) (string, error) { return "call-ok", nil }},
			wantText: "call-ok",
		},
		{
			name:     "describe_tool",
			handler:  &fakeHandlers{describeToolFn: func(context.Context, map[string]any) (string, error) { return "describe-ok", nil }},
			wantText: "describe-ok",
		},
		{
			name:     "list_servers",
			handler:  &fakeHandlers{listServersFn: func(context.Context, map[string]any) (string, error) { return "list-ok", nil }},
			wantText: "list-ok",
		},
		{
			name:     "whoami",
			handler:  &fakeHandlers{whoAmIFn: func(context.Context, map[string]any) (string, error) { return "whoami-ok", nil }},
			wantText: "whoami-ok",
		},
		{
			name:     "reload",
			handler:  &fakeHandlers{reloadFn: func(context.Context, map[string]any) (string, error) { return "reload-ok", nil }},
			wantText: "reload-ok",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := testServer(t, "meta", c.handler)
			req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"meta_` + c.name + `","arguments":{}}}`
			recs := runOnce(t, srv, req)
			result := recs[0]["result"].(map[string]any)
			content := result["content"].([]any)
			if content[0].(map[string]any)["text"] != c.wantText {
				t.Fatalf("text: %v", content[0].(map[string]any)["text"])
			}
		})
	}
}

// ---- input robustness ----

func TestRun_SkipsBlankAndInvalidLines(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv,
		``,
		`not json at all`,
		`{"jsonrpc":"2.0","id":42,"method":"initialize"}`,
	)
	// Two responses: one -32700 for the invalid line, one valid
	// initialize. Blank lines produce nothing.
	if len(recs) != 2 {
		t.Fatalf("expected 2 records (parse error + initialize), got %d", len(recs))
	}

	// First: parse error with null id.
	errObj, ok := recs[0]["error"].(map[string]any)
	if !ok {
		t.Fatalf("first record should be an error: %v", recs[0])
	}
	if errObj["code"] != float64(-32700) {
		t.Fatalf("expected -32700, got %v", errObj["code"])
	}
	if recs[0]["id"] != nil {
		t.Fatalf("parse error should have null id, got %v", recs[0]["id"])
	}

	// Second: the valid request.
	if recs[1]["id"] != float64(42) {
		t.Fatalf("id: %v", recs[1]["id"])
	}
	if _, ok := recs[1]["result"]; !ok {
		t.Fatalf("expected result on second record: %v", recs[1])
	}
}

func TestRun_EncodeError(t *testing.T) {
	srv := testServer(t, "meta", nil)
	fail := &failingWriter{}
	err := srv.Run(
		context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n"),
		fail,
	)
	if err == nil {
		t.Fatal("expected encode error")
	}
	if !strings.Contains(err.Error(), "encode response") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write(_ []byte) (int, error) { return 0, errors.New("nope") }

// ---- lifecycle ----

func TestRun_CtxCancel(t *testing.T) {
	srv := testServer(t, "meta", nil)

	// io.Pipe blocks the reader goroutine until the writer closes; the
	// ctx cancellation must make Run return without waiting for EOF.
	pr, pw := io.Pipe()
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx, pr, io.Discard) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}
}

func TestRun_EmptyInput(t *testing.T) {
	srv := testServer(t, "meta", nil)
	var out bytes.Buffer
	if err := srv.Run(context.Background(), strings.NewReader(""), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected no output, got %q", out.String())
	}
}

func TestRun_Ping(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record")
	}
	result := recs[0]["result"].(map[string]any)
	if len(result) != 0 {
		t.Fatalf("ping should return empty object, got %v", result)
	}
}

func TestRun_LoggingSetLevel(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":1,"method":"logging/setLevel","params":{"level":"debug"}}`)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record")
	}
	if _, ok := recs[0]["error"]; ok {
		t.Fatalf("setLevel should not error: %v", recs[0]["error"])
	}
}

func TestRun_ParseError(t *testing.T) {
	srv := testServer(t, "meta", nil)
	// Отправляем строку, которая не является JSON
	recs := runOnce(t, srv, `{broken json`)
	if len(recs) != 1 {
		t.Fatalf("expected 1 record")
	}
	errObj := recs[0]["error"].(map[string]any)
	if errObj["code"] != float64(-32700) {
		t.Fatalf("expected -32700, got %v", errObj["code"])
	}
	// Для parse error id должен быть null
	if recs[0]["id"] != nil {
		t.Fatalf("expected null id, got %v", recs[0]["id"])
	}
}

func TestRun_LargeIntegerID(t *testing.T) {
	srv := testServer(t, "meta", nil)
	// 2^53 + 1 — число, которое float64 не представляет точно.
	// Проверяем, что сервер возвращает ID в точности как получил.
	raws := runOnceRaw(t, srv,
		`{"jsonrpc":"2.0","id":9007199254740993,"method":"ping"}`)
	if len(raws) != 1 {
		t.Fatalf("expected 1 response, got %d", len(raws))
	}
	if !strings.Contains(raws[0], `"id":9007199254740993`) {
		t.Fatalf("large id not preserved exactly: %s", raws[0])
	}
}

func TestRun_ToolsCallWithoutID_IsDropped(t *testing.T) {
	srv := testServer(t, "meta", nil)
	// tools/call is not a notification; without id it must be dropped
	// silently (no response, no handler side effects we can observe).
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"meta_list_servers"}}`)
	if len(recs) != 0 {
		t.Fatalf("expected no response, got %d", len(recs))
	}
}

func TestRun_InvalidEnvelopeReturnsError(t *testing.T) {
	srv := testServer(t, "meta", nil)
	recs := runOnce(t, srv, `{"jsonrpc":"2.0","id":{},"method":"tools/list"}`)
	if len(recs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(recs))
	}
	errObj := recs[0]["error"].(map[string]any)
	if errObj["code"] != float64(-32600) {
		t.Fatalf("expected -32600, got %v", errObj["code"])
	}
}
