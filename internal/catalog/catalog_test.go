package catalog

import (
	"io"
	"testing"

	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

func testCatalogWithEntries(entries []ToolEntry) *Catalog {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{ToolPrefix: "meta"},
	}
	c := New(cfg, nil, logx.New(io.Discard))
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
