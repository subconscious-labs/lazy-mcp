// Package catalog builds and queries a compact tool index across all backends.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nikita-popov/lazy-mcp/internal/broker"
	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
	"github.com/nikita-popov/lazy-mcp/internal/observability"
)

// ToolEntry is a compact catalog entry for one tool.
type ToolEntry struct {
	Ref         string // "server/toolname"
	Server      string
	Tool        string
	Summary     string
	Tags        []string
	InputHint   string // compact one-liner from inputSchema
	InputSchema []byte // full schema for describe_tool
	Dangerous   bool

	// Precomputed lowercase views for fast scoring.
	toolLower    string
	refLower     string
	serverLower  string
	summaryLower string
	tagsLower    []string
}

func (e *ToolEntry) computeLower() {
	e.toolLower = strings.ToLower(e.Tool)
	e.refLower = strings.ToLower(e.Ref)
	e.serverLower = strings.ToLower(e.Server)
	e.summaryLower = strings.ToLower(e.Summary)
	e.tagsLower = make([]string, len(e.Tags))
	for i, t := range e.Tags {
		e.tagsLower[i] = strings.ToLower(t)
	}
}

// SearchHit is a ranked result from Search.
type SearchHit struct {
	Entry ToolEntry
	Score float64
}

// SearchWarning describes a query term that matched server metadata but no
// catalog entry — usually because the server's catalog is empty or stale.
type SearchWarning struct {
	Server string   `json:"server"`
	Terms  []string `json:"terms"`
	Reason string   `json:"reason"`
}

// SearchResult bundles hits and warnings.
type SearchResult struct {
	Hits     []SearchHit
	Warnings []SearchWarning
	Total    int // total matches before limit was applied
}

// Catalog holds all indexed tools.
type Catalog struct {
	cfg         *config.Holder
	brok        *broker.Broker
	log         *logx.Logger
	mu          sync.RWMutex
	entries     []ToolEntry
	lastRefresh map[string]time.Time
}

// New creates a Catalog.
func New(cfg *config.Holder, brok *broker.Broker, log *logx.Logger) *Catalog {
	return &Catalog{
		cfg:         cfg,
		brok:        brok,
		log:         log,
		lastRefresh: make(map[string]time.Time),
	}
}

// WarmUp fetches tools from all enabled servers.
func (c *Catalog) WarmUp(ctx context.Context) error {
	var lastErr error
	for _, srv := range c.cfg.Get().EnabledServers() {
		if err := c.RefreshServer(ctx, srv.Name); err != nil {
			c.log.Warn("warmup refresh failed", "server", srv.Name, "err", err)
			lastErr = err
		}
	}
	return lastErr
}

// RefreshServer fetches tools/list from a backend and updates the index.
func (c *Catalog) RefreshServer(ctx context.Context, serverName string) error {
	srvcfg, ok := c.cfg.Get().Server(serverName)
	if !ok {
		return fmt.Errorf("catalog: server %q not found", serverName)
	}

	tools, err := c.brok.ListTools(ctx, serverName)
	if err != nil {
		return fmt.Errorf("catalog: list tools %q: %w", serverName, err)
	}

	dangerSet := make(map[string]bool)
	for _, d := range srvcfg.DangerousTools {
		dangerSet[d] = true
	}

	var entries []ToolEntry
	for _, t := range tools {
		entry := ToolEntry{
			Ref:         serverName + "/" + t.Name,
			Server:      serverName,
			Tool:        t.Name,
			Summary:     firstNonEmpty(t.Description, srvcfg.Description),
			Tags:        srvcfg.Tags,
			InputHint:   extractInputHint(t.InputSchema),
			InputSchema: t.InputSchema,
			Dangerous:   dangerSet[t.Name],
		}
		entry.computeLower()
		entries = append(entries, entry)
	}

	c.mu.Lock()
	// remove old entries for this server
	var kept []ToolEntry
	for _, e := range c.entries {
		if e.Server != serverName {
			kept = append(kept, e)
		}
	}
	c.entries = append(kept, entries...)
	c.lastRefresh[serverName] = time.Now()
	c.mu.Unlock()

	observability.SetCatalogSize(serverName, len(entries))

	c.log.Info("catalog refreshed", "server", serverName, "tools", len(entries))
	return nil
}

// ServerStale reports whether the cached tool list for serverName is older
// than proxy.tool_cache_ttl. Servers that were never refreshed count as stale.
// ttl <= 0 disables staleness entirely (cache never expires).
func (c *Catalog) ServerStale(serverName string) bool {
	ttl := c.cfg.Get().Proxy.ToolCacheTTL
	if ttl <= 0 {
		return false
	}
	c.mu.RLock()
	t, ok := c.lastRefresh[serverName]
	c.mu.RUnlock()
	if !ok {
		return true
	}
	return time.Since(t) > ttl
}

type matchKind int

const (
	matchNone matchKind = iota
	matchCatalog
	matchServer
)

// Search returns scored hits for query plus warnings for terms that matched
// only server metadata (e.g. a broken server's description or tags).
//
// Semantics: a word that matches nothing anywhere (neither catalog nor server
// metadata) disqualifies the whole query. A word that matches only server
// metadata contributes no score but does not disqualify; instead it yields
// a warning about that server.
func (c *Catalog) Search(query string, limit int) SearchResult {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return c.allHits(limit)
	}
	words := strings.Fields(q)
	if len(words) == 0 {
		return c.allHits(limit)
	}

	c.mu.RLock()
	entries := c.entries
	c.mu.RUnlock()

	matches := make([]matchKind, len(words))
	serverTerms := map[string][]string{} // server name -> terms matched

	for i, w := range words {
		for _, e := range entries {
			if wordMatchesEntry(w, e) {
				matches[i] = matchCatalog
				break
			}
		}
		if matches[i] == matchCatalog {
			continue
		}
		for _, srv := range c.cfg.Get().EnabledServers() {
			if wordMatchesServerMeta(w, srv.Name, srv.Description, srv.Tags) {
				matches[i] = matchServer
				serverTerms[srv.Name] = append(serverTerms[srv.Name], w)
				break
			}
		}
	}

	// Any word matched nowhere → empty result.
	for _, m := range matches {
		if m == matchNone {
			return SearchResult{}
		}
	}

	var hits []SearchHit
	for _, e := range entries {
		var s float64
		for _, w := range words {
			s += scoreWord(w, e)
		}
		if s > 0 {
			hits = append(hits, SearchHit{Entry: e, Score: s})
		}
	}
	// insertion sort for small N
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].Score > hits[j-1].Score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}

	total := len(hits)
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}

	var warnings []SearchWarning
	for srv, terms := range serverTerms {
		warnings = append(warnings, SearchWarning{
			Server: srv,
			Terms:  terms,
			Reason: "matched server metadata but no tools are indexed (catalog unavailable or empty)",
		})
	}

	return SearchResult{Hits: hits, Warnings: warnings, Total: total}
}

// allHits returns all entries with uniform score, for the empty-query case.
func (c *Catalog) allHits(limit int) SearchResult {
	c.mu.RLock()
	entries := c.entries
	c.mu.RUnlock()
	hits := make([]SearchHit, 0, len(entries))
	for _, e := range entries {
		hits = append(hits, SearchHit{Entry: e, Score: 1})
	}
	total := len(hits)
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return SearchResult{Hits: hits, Total: total}
}

// Describe returns a ToolEntry by ref ("server/tool").
func (c *Catalog) Describe(ref string) (ToolEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.entries {
		if e.Ref == ref {
			return e, true
		}
	}
	return ToolEntry{}, false
}

// Entries returns a snapshot of all indexed entries.
func (c *Catalog) Entries() []ToolEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ToolEntry, len(c.entries))
	copy(out, c.entries)
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// extractInputHint builds a compact one-liner from a JSON Schema.
func extractInputHint(schema []byte) string {
	if len(schema) == 0 {
		return ""
	}
	var s struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return ""
	}
	var parts []string
	for name, prop := range s.Properties {
		t := prop.Type
		if t == "" {
			t = "any"
		}
		parts = append(parts, name+":"+t)
	}
	return strings.Join(parts, ", ")
}

// ApplyConfig removes entries and refresh records for servers that are
// no longer enabled in cfg.
func (c *Catalog) ApplyConfig(cfg *config.Config) {
	enabled := make(map[string]bool, len(cfg.Servers))
	for _, s := range cfg.EnabledServers() {
		enabled[s.Name] = true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var kept []ToolEntry
	for _, e := range c.entries {
		if enabled[e.Server] {
			kept = append(kept, e)
		}
	}
	removed := len(c.entries) - len(kept)
	c.entries = kept

	for name := range c.lastRefresh {
		if !enabled[name] {
			delete(c.lastRefresh, name)
		}
	}
	if removed > 0 {
		c.log.Info("reload: dropped catalog entries", "count", removed)
	}
}
