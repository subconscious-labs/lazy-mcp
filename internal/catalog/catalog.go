// Package catalog builds and queries a compact tool index across all backends.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/subconscious-labs/lazy-mcp/internal/broker"
	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/observability"
	"github.com/subconscious-labs/lazy-mcp/internal/policy"
)

// ToolEntry is a compact catalog entry for one tool.
type ToolEntry struct {
	Ref         string
	Server      string
	Tool        string
	Summary     string // capped at proxy.max_summary_len
	Description string // full description (for describe_tool)
	Tags        []string
	InputHint   string
	InputSchema []byte
	Dangerous   bool
	Annotations broker.ToolAnnotations

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
	Entry *ToolEntry
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
	Total    int
}

// Catalog holds all indexed tools.
type Catalog struct {
	cfg         *config.Holder
	brok        *broker.Broker
	policy      *policy.Policy
	log         *logx.Logger
	mu          sync.RWMutex
	entries     []ToolEntry
	lastRefresh map[string]time.Time
}

// New creates a Catalog.
func New(cfg *config.Holder, brok *broker.Broker, pol *policy.Policy, log *logx.Logger) *Catalog {
	return &Catalog{
		cfg:         cfg,
		brok:        brok,
		policy:      pol,
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
//
// The index is filtered by policy with user="admin": per-user views are
// computed at read time in Search, so we keep one index that is as
// complete as the global rules allow.
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
	skipped := 0
	for _, t := range tools {
		if !c.policy.Visible("", serverName, t.Name) {
			skipped++
			continue
		}
		desc := firstNonEmpty(t.Description, srvcfg.Description)
		summary := desc
		if maxLen := c.cfg.Get().Proxy.MaxSummaryLen; maxLen > 0 && len(summary) > maxLen {
			summary = truncateUTF8(summary, maxLen) + "…"
		}
		entry := ToolEntry{
			Ref:         serverName + "/" + t.Name,
			Server:      serverName,
			Tool:        t.Name,
			Summary:     summary,
			Description: desc,
			Tags:        srvcfg.Tags,
			InputHint:   extractInputHint(t.InputSchema),
			InputSchema: t.InputSchema,
			Dangerous:   dangerSet[t.Name],
			Annotations: t.Annotations,
		}
		entry.computeLower()
		entries = append(entries, entry)
	}

	c.mu.Lock()
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

	c.log.Info("catalog refreshed",
		"server", serverName,
		"tools", len(entries),
		"skipped", skipped)
	return nil
}

// ServerStale reports whether the cached tool list for serverName is older
// than proxy.tool_cache_ttl.
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

// InvalidateServer drops the refresh timestamp for serverName so the
// next Search or DescribeTool re-fetches tools/list.
func (c *Catalog) InvalidateServer(serverName string) {
	c.mu.Lock()
	_, had := c.lastRefresh[serverName]
	delete(c.lastRefresh, serverName)
	c.mu.Unlock()
	if had {
		c.log.Info("catalog invalidated", "server", serverName)
	}
}

// Invalidate clears all refresh timestamps so the next Search reloads
// every server. Called on SIGHUP when policy rules may have changed.
func (c *Catalog) Invalidate() {
	c.mu.Lock()
	c.lastRefresh = make(map[string]time.Time)
	c.mu.Unlock()
}

// Search returns scored hits for query plus warnings for query terms
// that matched nothing. Semantics:
//
//   - All words matched somewhere → hits as before, no warnings.
//   - Some words matched, some did not → hits from matched words only;
//     unmatched terms are reported as warnings so the caller knows the
//     query was partial.
//   - No word matched anywhere → empty result, no warnings.
//
// Score in each hit is a per-word average in [0, 1]; a higher score
// means the entry matched the query more strongly, not that it ranks
// higher in absolute terms.
func (c *Catalog) Search(query string, limit int, filter func(*ToolEntry) bool) SearchResult {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return c.allHits(limit, filter)
	}
	words := strings.Fields(q)
	if len(words) == 0 {
		return c.allHits(limit, filter)
	}

	c.mu.RLock()
	entries := c.entries
	c.mu.RUnlock()

	perWordMatched := make([]bool, len(words))
	// Cap the initial hint: a catalog with 1200 entries and a typical
	// query that matches ~150 should not pre-allocate 1200 SearchHits.
	capHint := len(entries)
	if capHint > 256 {
		capHint = 256
	}
	hits := make([]SearchHit, 0, capHint)
	denom := float64(len(words))

	// Iterate over entries once, applying filter inline (no copy) and
	// scoring each entry.
	for i := range entries {
		e := &entries[i]
		if filter != nil && !filter(e) {
			continue
		}
		var sum float64
		for j, w := range words {
			s := scoreWord(w, e)
			if s > 0 {
				perWordMatched[j] = true
			}
			sum += s
		}
		if sum > 0 {
			hits = append(hits, SearchHit{Entry: e, Score: sum / denom})
		}
	}

	// Unmatched words: check server metadata (no entry copy).
	serverTerms := map[string][]string{}
	var unmatched []string
	for j, w := range words {
		if perWordMatched[j] {
			continue
		}
		matched := false
		for _, srv := range c.cfg.Get().EnabledServers() {
			probe := ToolEntry{Server: srv.Name}
			if filter != nil && !filter(&probe) {
				continue
			}
			if wordMatchesServerMeta(w, srv.Name, srv.Description, srv.Tags) {
				serverTerms[srv.Name] = append(serverTerms[srv.Name], w)
				matched = true
				break
			}
		}
		if !matched {
			unmatched = append(unmatched, w)
		}
	}

	if len(hits) == 0 && len(serverTerms) == 0 {
		return SearchResult{}
	}

	// O(n log n): insertion sort was O(n²) and dominated runtime when
	// several hundred entries matched. Tie-break on Ref for a
	// deterministic order that does not depend on catalog insertion.
	slices.SortFunc(hits, func(a, b SearchHit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		default:
			return strings.Compare(a.Entry.Ref, b.Entry.Ref)
		}
	})

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
	if len(unmatched) > 0 {
		warnings = append(warnings, SearchWarning{
			Terms:  unmatched,
			Reason: "terms not found in any catalog entry; try the query without them",
		})
	}

	return SearchResult{Hits: hits, Warnings: warnings, Total: total}
}

// allHits returns all entries with uniform score, for the empty-query case.
func (c *Catalog) allHits(limit int, filter func(*ToolEntry) bool) SearchResult {
	c.mu.RLock()
	entries := c.entries
	c.mu.RUnlock()
	hits := make([]SearchHit, 0, len(entries))
	for i := range entries {
		e := &entries[i]
		if filter != nil && !filter(e) {
			continue
		}
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

// InjectEntries replaces the catalog contents. Intended for tests
// that need deterministic entries without a running broker.
func (c *Catalog) InjectEntries(entries []ToolEntry) {
	c.mu.Lock()
	c.entries = entries
	c.mu.Unlock()
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

// truncateUTF8 caps s to at most limit bytes without splitting a UTF-8 rune.
func truncateUTF8(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
