package catalog

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

// benchEntry is a realistic ToolEntry used by all benchmarks.
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

func benchCatalog(n int) *Catalog {
	cfg := &config.Config{
		Proxy: config.ProxyConfig{
			ToolPrefix:   "meta",
			ToolCacheTTL: 30 * time.Minute,
		},
	}
	c := New(config.NewHolder(cfg), nil, logx.New(io.Discard))
	c.entries = make([]ToolEntry, n)
	for i := 0; i < n; i++ {
		c.entries[i] = benchEntry(i)
	}
	return c
}

func BenchmarkSearch_SingleWord(b *testing.B) {
	c := benchCatalog(500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("tool_42", 8)
	}
}

func BenchmarkSearch_MultiWord(b *testing.B) {
	c := benchCatalog(500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("execute command remote", 8)
	}
}

func BenchmarkSearch_NoMatch(b *testing.B) {
	c := benchCatalog(500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("zzznotexist", 8)
	}
}

func BenchmarkSearch_Empty(b *testing.B) {
	c := benchCatalog(500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Search("", 8)
	}
}

func BenchmarkExtractInputHint(b *testing.B) {
	schema := []byte(`{"type":"object","properties":{"host":{"type":"string"},"command":{"type":"string"},"timeout":{"type":"integer"}},"required":["host","command"]}`)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = extractInputHint(schema)
	}
}

func BenchmarkScoreWord(b *testing.B) {
	e := benchEntry(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = scoreWord("command", e)
	}
}
