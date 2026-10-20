package catalog

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
)

func testCatalogWithEntries(entries []ToolEntry) *Catalog {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{ToolPrefix: "meta"},
	}
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))
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
	r := c.Search("list", 0)
	if r.Total != 3 || len(r.Hits) != 3 {
		t.Fatalf("no-limit: total=%d hits=%d, want 3/3", r.Total, len(r.Hits))
	}

	// Limit smaller than matches: hits capped, total intact.
	r = c.Search("list", 2)
	if r.Total != 3 {
		t.Fatalf("total: got %d want 3", r.Total)
	}
	if len(r.Hits) != 2 {
		t.Fatalf("hits: got %d want 2", len(r.Hits))
	}

	// Limit larger than matches: no truncation.
	r = c.Search("list", 100)
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

	r := c.Search("", 1)
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

	r := c.Search("zzznotexist", 0)
	if r.Total != 0 || len(r.Hits) != 0 {
		t.Fatalf("expected empty, got total=%d hits=%d", r.Total, len(r.Hits))
	}
}

func TestServerStale(t *testing.T) {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{ToolCacheTTL: 100 * time.Millisecond},
	}
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))

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
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))
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
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))
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
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))
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
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))
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
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))
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
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))
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
