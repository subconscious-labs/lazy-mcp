package catalog

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/policy"
)

func benchCatalog(n int) *Catalog {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{
			ToolPrefix:   "meta",
			ToolCacheTTL: 30 * time.Minute,
		},
	}
	holder := config.NewHolder(cfg)
	c := New(holder, nil, policy.New(holder), logx.New(io.Discard))
	c.entries = make([]ToolEntry, n)
	for i := 0; i < n; i++ {
		e := benchEntry(i)
		e.computeLower()
		c.entries[i] = e
	}
	return c
}

// realisticCorpus builds a catalog that mimics a real deployment: 12
// servers with ~100 tools each, mixed English/Russian summaries and
// tags. Used to benchmark against a production-sized index instead of
// a synthetic 500-entry one.
func realisticCorpus(n int) *Catalog {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{
			ToolPrefix:   "meta",
			ToolCacheTTL: 30 * time.Minute,
		},
	}
	holder := config.NewHolder(cfg)
	c := New(holder, nil, policy.New(holder), logx.New(io.Discard))

	servers := []string{"git", "tasks", "ssh", "metrics", "mempalace", "dav", "notes", "ntfy", "xmpp", "tts", "deepseek", "time"}
	verbs := []string{"list", "get", "create", "update", "delete", "search", "query", "fetch", "send", "read"}
	nouns := []string{"issue", "repository", "task", "project", "alert", "metric", "note", "message", "contact", "calendar", "event", "user", "label", "comment"}
	tags := []string{"search", "read", "write", "admin", "memory", "поиск", "задачи", "уведомления", "календарь"}

	c.entries = make([]ToolEntry, 0, n)
	for i := 0; i < n; i++ {
		srv := servers[i%len(servers)]
		verb := verbs[i%len(verbs)]
		noun := nouns[(i/len(verbs))%len(nouns)]
		name := fmt.Sprintf("%s_%s", verb, noun)
		entry := ToolEntry{
			Ref:         srv + "/" + name,
			Server:      srv,
			Tool:        name,
			Summary:     fmt.Sprintf("%s %s via %s API. Returns a list of %s objects.", verb, noun, srv, noun),
			Tags:        []string{srv, tags[i%len(tags)]},
			InputHint:   "id:integer, limit:integer",
			InputSchema: []byte(`{"type":"object","properties":{"id":{"type":"integer"},"limit":{"type":"integer"}}}`),
		}
		entry.computeLower()
		c.entries = append(c.entries, entry)
	}
	return c
}

func benchCatalogRealistic() *Catalog { return realisticCorpus(1200) }

func BenchmarkSearch_Realistic_SingleWord(b *testing.B) {
	c := benchCatalogRealistic()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("task", 8, nil)
	}
}

func BenchmarkSearch_Realistic_MultiWord(b *testing.B) {
	c := benchCatalogRealistic()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("create issue repository", 8, nil)
	}
}

func BenchmarkSearch_Realistic_WithUnmatched(b *testing.B) {
	c := benchCatalogRealistic()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("create nonsense", 8, nil)
	}
}

func BenchmarkSearch_Realistic_NoMatch(b *testing.B) {
	c := benchCatalogRealistic()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("zzznotexist", 8, nil)
	}
}

func BenchmarkSearch_Realistic_WithFilter(b *testing.B) {
	c := benchCatalogRealistic()
	filter := func(e *ToolEntry) bool { return e.Server == "git" || e.Server == "tasks" }
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("issue", 8, filter)
	}
}

func BenchmarkSearch_Realistic_Top3(b *testing.B) {
	c := benchCatalogRealistic()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("task", 3, nil)
	}
}

// Legacy smaller benchmarks kept for regression comparison.
func benchEntry(i int) ToolEntry {
	return ToolEntry{
		Ref:         fmt.Sprintf("server%d/tool_%d", i%5, i),
		Server:      fmt.Sprintf("server%d", i%5),
		Tool:        fmt.Sprintf("tool_%d", i),
		Summary:     "Execute a command on a remote host via SSH and return the output",
		Tags:        []string{"ssh", "exec", "remote"},
		InputHint:   "host:string, command:string",
		InputSchema: []byte(`{"type":"object","properties":{"host":{"type":"string"},"command":{"type":"string"}},"required":["host","command"]}`),
	}
}

func BenchmarkSearch_SingleWord(b *testing.B) {
	c := benchCatalog(500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("tool_42", 8, nil)
	}
}

func BenchmarkScoreWord(b *testing.B) {
	e := benchEntry(0)
	e.computeLower()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = scoreWord("command", &e)
	}
}
