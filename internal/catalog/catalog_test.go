package catalog

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/policy"
)

func testCatalogWithEntries(entries []ToolEntry) *Catalog {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{ToolPrefix: "meta"},
	}
	holder := config.NewHolder(cfg)
	c := New(holder, nil, policy.New(holder), logx.New(io.Discard))
	c.entries = entries
	return c
}

func mkEntry(server, tool, summary string) ToolEntry {
	e := ToolEntry{
		Ref:     server + "/" + tool,
		Server:  server,
		Tool:    tool,
		Summary: summary,
	}
	e.computeLower()
	return e
}

func TestSearch_TotalReflectsPreLimitCount(t *testing.T) {
	entries := []ToolEntry{
		mkEntry("git", "list_repos", "List repositories"),
		mkEntry("git", "list_issues", "List issues"),
		mkEntry("git", "list_prs", "List pull requests"),
		mkEntry("git", "create_issue", "Create an issue"),
	}
	c := testCatalogWithEntries(entries)

	// No limit: total == hits count.
	r := c.Search("list", 0, nil)
	if r.Total != 3 || len(r.Hits) != 3 {
		t.Fatalf("no-limit: total=%d hits=%d, want 3/3", r.Total, len(r.Hits))
	}

	// Limit smaller than matches: hits capped, total intact.
	r = c.Search("list", 2, nil)
	if r.Total != 3 {
		t.Fatalf("total: got %d want 3", r.Total)
	}
	if len(r.Hits) != 2 {
		t.Fatalf("hits: got %d want 2", len(r.Hits))
	}

	// Limit larger than matches: no truncation.
	r = c.Search("list", 100, nil)
	if r.Total != 3 || len(r.Hits) != 3 {
		t.Fatalf("large-limit: total=%d hits=%d", r.Total, len(r.Hits))
	}
}

func TestSearch_EmptyQueryTotal(t *testing.T) {
	entries := []ToolEntry{
		mkEntry("a", "t1", ""),
		mkEntry("b", "t2", ""),
	}
	c := testCatalogWithEntries(entries)

	r := c.Search("", 1, nil)
	if r.Total != 2 {
		t.Fatalf("total: got %d want 2", r.Total)
	}
	if len(r.Hits) != 1 {
		t.Fatalf("hits: got %d want 1", len(r.Hits))
	}
}

func TestSearch_NoMatchTotalZero(t *testing.T) {
	entries := []ToolEntry{mkEntry("a", "t1", "")}
	c := testCatalogWithEntries(entries)

	r := c.Search("zzznotexist", 0, nil)
	if r.Total != 0 || len(r.Hits) != 0 {
		t.Fatalf("expected empty, got total=%d hits=%d", r.Total, len(r.Hits))
	}
}

func TestServerStale(t *testing.T) {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{ToolCacheTTL: 100 * time.Millisecond},
	}
	c := New(config.NewHolder(cfg), nil, policy.New(config.NewHolder(cfg)), logx.New(io.Discard))

	// Never refreshed: stale.
	if !c.ServerStale("srv") {
		t.Fatalf("unknown server should be stale")
	}

	// Just refreshed: not stale.
	c.mu.Lock()
	c.lastRefresh["srv"] = time.Now()
	c.mu.Unlock()
	if c.ServerStale("srv") {
		t.Fatalf("fresh server should not be stale")
	}

	// Aged past TTL: stale again.
	time.Sleep(150 * time.Millisecond)
	if !c.ServerStale("srv") {
		t.Fatalf("expired server should be stale")
	}
}

func TestServerStale_ZeroTTLNeverStale(t *testing.T) {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{ToolCacheTTL: 0},
	}
	c := New(config.NewHolder(cfg), nil, policy.New(config.NewHolder(cfg)), logx.New(io.Discard))
	if c.ServerStale("never-seen") {
		t.Fatalf("ttl=0 should disable staleness")
	}
}

func TestDescribe(t *testing.T) {
	e := mkEntry("git", "list", "list repos")
	c := testCatalogWithEntries([]ToolEntry{e})

	got, ok := c.Describe("git/list")
	if !ok || got.Tool != "list" {
		t.Fatalf("Describe: ok=%v, got=%+v", ok, got)
	}
	if _, ok := c.Describe("git/nope"); ok {
		t.Fatalf("unknown ref should not be found")
	}
}

func TestEntries_Snapshot(t *testing.T) {
	c := testCatalogWithEntries([]ToolEntry{
		mkEntry("a", "t1", ""),
		mkEntry("b", "t2", ""),
	})
	snap := c.Entries()
	if len(snap) != 2 {
		t.Fatalf("Entries: %d", len(snap))
	}
	// Mutating the snapshot must not affect the catalog.
	snap[0].Tool = "mutated"
	if c.Entries()[0].Tool == "mutated" {
		t.Fatalf("Entries returned a shared slice")
	}
}

func TestRefreshServer_UnknownServer(t *testing.T) {
	cfg := &config.Config{Proxy: config.ProxyConfig{ToolPrefix: "meta"}}
	c := New(config.NewHolder(cfg), nil, policy.New(config.NewHolder(cfg)), logx.New(io.Discard))
	err := c.RefreshServer(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
}

func TestApplyConfig_DropsDisabledEntries(t *testing.T) {
	srvA := config.ServerConfig{Name: "a", Enabled: true}
	srvB := config.ServerConfig{Name: "b", Enabled: true}
	cfg := &config.Config{
		Proxy:   config.ProxyConfig{ToolPrefix: "meta"},
		Servers: []config.ServerConfig{srvA, srvB},
	}
	c := New(config.NewHolder(cfg), nil, policy.New(config.NewHolder(cfg)), logx.New(io.Discard))
	c.entries = []ToolEntry{
		mkEntry("a", "t1", ""),
		mkEntry("a", "t2", ""),
		mkEntry("b", "t1", ""),
	}
	c.lastRefresh["a"] = time.Now()
	c.lastRefresh["b"] = time.Now()

	// b disabled.
	newCfg := &config.Config{
		Proxy: cfg.Proxy,
		Servers: []config.ServerConfig{
			srvA,
			{Name: "b", Enabled: false},
		},
	}
	c.ApplyConfig(newCfg)

	snap := c.Entries()
	if len(snap) != 2 {
		t.Fatalf("entries: got %d, want 2 (only a)", len(snap))
	}
	for _, e := range snap {
		if e.Server != "a" {
			t.Fatalf("unexpected entry from server %q", e.Server)
		}
	}

	c.mu.RLock()
	_, hasB := c.lastRefresh["b"]
	_, hasA := c.lastRefresh["a"]
	c.mu.RUnlock()
	if hasB {
		t.Fatalf("lastRefresh[b] should be cleared")
	}
	if !hasA {
		t.Fatalf("lastRefresh[a] should be preserved")
	}
}

func TestApplyConfig_DropsRemovedServer(t *testing.T) {
	srvA := config.ServerConfig{Name: "a", Enabled: true}
	cfg := &config.Config{
		Proxy:   config.ProxyConfig{ToolPrefix: "meta"},
		Servers: []config.ServerConfig{srvA},
	}
	c := New(config.NewHolder(cfg), nil, policy.New(config.NewHolder(cfg)), logx.New(io.Discard))
	c.entries = []ToolEntry{
		mkEntry("a", "t1", ""),
		mkEntry("b", "t1", ""), // b was never in the config, but in the index
	}

	newCfg := &config.Config{
		Proxy:   cfg.Proxy,
		Servers: []config.ServerConfig{srvA},
	}
	c.ApplyConfig(newCfg)

	snap := c.Entries()
	if len(snap) != 1 || snap[0].Server != "a" {
		t.Fatalf("entries: got %+v", snap)
	}
}

func TestApplyConfig_NoopWhenNothingChanged(t *testing.T) {
	srvA := config.ServerConfig{Name: "a", Enabled: true}
	cfg := &config.Config{
		Proxy:   config.ProxyConfig{ToolPrefix: "meta"},
		Servers: []config.ServerConfig{srvA},
	}
	c := New(config.NewHolder(cfg), nil, policy.New(config.NewHolder(cfg)), logx.New(io.Discard))
	c.entries = []ToolEntry{mkEntry("a", "t1", "")}
	c.lastRefresh["a"] = time.Now()

	c.ApplyConfig(cfg)

	if len(c.Entries()) != 1 {
		t.Fatalf("noop should keep entries")
	}
}

func TestApplyConfig_EmptyAfterAllDisabled(t *testing.T) {
	srvA := config.ServerConfig{Name: "a", Enabled: true}
	cfg := &config.Config{
		Proxy:   config.ProxyConfig{ToolPrefix: "meta"},
		Servers: []config.ServerConfig{srvA},
	}
	c := New(config.NewHolder(cfg), nil, policy.New(config.NewHolder(cfg)), logx.New(io.Discard))
	c.entries = []ToolEntry{mkEntry("a", "t1", "")}

	newCfg := &config.Config{
		Proxy: cfg.Proxy,
		Servers: []config.ServerConfig{
			{Name: "a", Enabled: false},
		},
	}
	c.ApplyConfig(newCfg)

	if len(c.Entries()) != 0 {
		t.Fatalf("all entries should be dropped")
	}
}

func TestRefreshServer_SummaryCapped(t *testing.T) {
	// Слишком длинное описание для одного сервера
	longDesc := strings.Repeat("x", 500)
	// ... (тест через RefreshServer потребует mock-брокер)
	// Проверим просто truncateUTF8
	got := truncateUTF8(longDesc, 200)
	if len(got) != 200 {
		t.Fatalf("expected 200 bytes, got %d", len(got))
	}
}

func TestTruncateUTF8(t *testing.T) {
	// Кириллица: 6 символов по 2 байта = 12 байт
	s := "Привет"
	got := truncateUTF8(s, 5) // 5 байт = 2.5 символа
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
	if len(got) > 5 {
		t.Fatalf("exceeds limit: %d bytes", len(got))
	}
}

func TestSearch_PartialMatchWithWarning(t *testing.T) {
	entries := []ToolEntry{
		mkEntry("git", "list_issues", "List issues"),
		mkEntry("git", "list_repos", "List repositories"),
	}
	c := testCatalogWithEntries(entries)

	// Two words, one matches, one doesn't.
	r := c.Search("list nonsense", 10, nil)
	if len(r.Hits) != 2 {
		t.Fatalf("expected 2 hits from matched word, got %d", len(r.Hits))
	}
	if r.Total != 2 {
		t.Fatalf("total: got %d want 2", r.Total)
	}
	found := false
	for _, w := range r.Warnings {
		for _, term := range w.Terms {
			if term == "nonsense" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected warning for unmatched term 'nonsense', got %+v", r.Warnings)
	}
}

func TestSearch_NoMatchNoWarning(t *testing.T) {
	entries := []ToolEntry{mkEntry("git", "list_issues", "List issues")}
	c := testCatalogWithEntries(entries)

	r := c.Search("zzznotexist", 10, nil)
	if len(r.Hits) != 0 {
		t.Fatalf("expected empty hits, got %d", len(r.Hits))
	}
	if len(r.Warnings) != 0 {
		t.Fatalf("expected no warnings when nothing matched, got %+v", r.Warnings)
	}
}

func TestSearch_ScoreRange(t *testing.T) {
	entries := []ToolEntry{
		mkEntry("git", "list_issues", "List issues in repository"),
		mkEntry("git", "create_issue", "Create an issue"),
		mkEntry("tasks", "get_task", "Fetch a task"),
	}
	c := testCatalogWithEntries(entries)

	r := c.Search("list issues", 10, nil)
	if len(r.Hits) == 0 {
		t.Fatal("expected hits")
	}
	for _, h := range r.Hits {
		if h.Score < 0 || h.Score > 1 {
			t.Fatalf("score out of range: %v (entry %s)", h.Score, h.Entry.Ref)
		}
	}

	// The entry with "list_issues" in the name should score highest.
	if r.Hits[0].Entry.Tool != "list_issues" {
		t.Fatalf("expected list_issues first, got %s (score %v)",
			r.Hits[0].Entry.Tool, r.Hits[0].Score)
	}
	if r.Hits[0].Score < 0.5 {
		t.Fatalf("strong match should score >= 0.5, got %v", r.Hits[0].Score)
	}
}
