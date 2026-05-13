// Package catalog builds and queries a compact tool index across all backends.
package catalog

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nikita-popov/lazy-mcp/internal/broker"
	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

// ToolEntry is a compact catalog entry for one tool.
type ToolEntry struct {
	Ref         string   // "server/toolname"
	Server      string
	Tool        string
	Summary     string
	Tags        []string
	InputHint   string // compact one-liner from inputSchema
	InputSchema []byte // full schema for describe_tool
	Dangerous   bool
}

// SearchHit is a ranked result from Search.
type SearchHit struct {
	Entry ToolEntry
	Score float64
}

// Catalog holds all indexed tools.
type Catalog struct {
	cfg    *config.Config
	brok   *broker.Broker
	log    *logx.Logger
	mu     sync.RWMutex
	entries []ToolEntry
}

// New creates a Catalog.
func New(cfg *config.Config, brok *broker.Broker, log *logx.Logger) *Catalog {
	return &Catalog{cfg: cfg, brok: brok, log: log}
}

// WarmUp fetches tools from all enabled servers.
func (c *Catalog) WarmUp(ctx context.Context) error {
	var lastErr error
	for _, srv := range c.cfg.EnabledServers() {
		if err := c.RefreshServer(ctx, srv.Name); err != nil {
			c.log.Warn("warmup refresh failed", "server", srv.Name, "err", err)
			lastErr = err
		}
	}
	return lastErr
}

// RefreshServer fetches tools/list from a backend and updates the index.
func (c *Catalog) RefreshServer(ctx context.Context, serverName string) error {
	srvcfg, ok := c.cfg.Server(serverName)
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
		entries = append(entries, ToolEntry{
			Ref:         serverName + "/" + t.Name,
			Server:      serverName,
			Tool:        t.Name,
			Summary:     firstNonEmpty(t.Description, srvcfg.Description),
			Tags:        srvcfg.Tags,
			InputHint:   extractInputHint(t.InputSchema),
			InputSchema: t.InputSchema,
			Dangerous:   dangerSet[t.Name],
		})
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
	c.mu.Unlock()

	c.log.Info("catalog refreshed", "server", serverName, "tools", len(entries))
	return nil
}

// Search returns top-N scored hits for query.
func (c *Catalog) Search(query string, limit int) []SearchHit {
	c.mu.RLock()
	entries := c.entries
	c.mu.RUnlock()

	q := strings.ToLower(strings.TrimSpace(query))
	var hits []SearchHit
	for _, e := range entries {
		s := score(q, e)
		if s > 0 {
			hits = append(hits, SearchHit{Entry: e, Score: s})
		}
	}
	// simple insertion sort for small N
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].Score > hits[j-1].Score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
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
	if err := jsonUnmarshal(schema, &s); err != nil {
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
