// Package meta implements the meta-tool handlers: search, call, describe_tool, list_servers.
package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/nikita-popov/lazy-mcp/internal/broker"
	"github.com/nikita-popov/lazy-mcp/internal/catalog"
	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

// Handlers holds dependencies for meta-tool execution.
type Handlers struct {
	cfg   *config.Config
	brok  *broker.Broker
	cat   *catalog.Catalog
	log   *logx.Logger
	pager *PageStore
}

// New creates Handlers.
func New(cfg *config.Config, brok *broker.Broker, cat *catalog.Catalog, log *logx.Logger) *Handlers {
	return &Handlers{
		cfg:   cfg,
		brok:  brok,
		cat:   cat,
		log:   log,
		pager: NewPageStore(cfg.Proxy.CursorTTL),
	}
}

// Search finds tools matching query.
// args: {"query": "...", "tags": ["..."], "limit": N}
func (h *Handlers) Search(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	limit := h.cfg.Proxy.MaxSearchResults
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = int(l)
	}

	// Refresh only the servers whose cached tool list is stale (or missing).
	// On a warm catalog this is a no-op.
	for _, srv := range h.cfg.EnabledServers() {
		if h.cat.ServerStale(srv.Name) {
			if err := h.cat.RefreshServer(ctx, srv.Name); err != nil {
				h.log.Warn("search: refresh failed", "server", srv.Name, "err", err)
			}
		}
	}

	hits := h.cat.Search(query, limit)
	type hitOut struct {
		Ref       string   `json:"tool_ref"`
		Server    string   `json:"server"`
		Tool      string   `json:"tool"`
		Summary   string   `json:"summary"`
		Tags      []string `json:"tags"`
		InputHint string   `json:"input_hint,omitempty"`
		Dangerous bool     `json:"dangerous,omitempty"`
	}
	var out []hitOut
	for _, h := range hits {
		out = append(out, hitOut{
			Ref:       h.Entry.Ref,
			Server:    h.Entry.Server,
			Tool:      h.Entry.Tool,
			Summary:   h.Entry.Summary,
			Tags:      h.Entry.Tags,
			InputHint: h.Entry.InputHint,
			Dangerous: h.Entry.Dangerous,
		})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// callResult is the envelope returned by Call.
type callResult struct {
	Result     string `json:"result"`
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more,omitempty"`
	Page       int    `json:"page,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// Call routes a tool call to the correct backend with optional pagination.
//
// First call:
//
//	args: {"tool_ref": "server/tool", "arguments": {...}, "page_size": N}
//	  OR  {"server": "name", "tool": "name", "arguments": {...}, "page_size": N}
//
// Subsequent pages:
//
//	args: {"cursor": "<token>"}
func (h *Handlers) Call(ctx context.Context, args map[string]any) (string, error) {
	// --- Cursor continuation path ---
	if cursor, ok := args["cursor"].(string); ok && cursor != "" {
		return h.callCursorNext(cursor)
	}

	// --- First call path ---
	server, tool, err := h.resolveTarget(args)
	if err != nil {
		return "", err
	}

	toolArgs := extractArguments(args)
	pageSize := h.cfg.Proxy.DefaultPageSize
	if ps, ok := args["page_size"].(float64); ok && int(ps) > 0 {
		pageSize = int(ps)
	}

	h.log.Info("call", "server", server, "tool", tool, "page_size", pageSize)

	// Lazy refresh catalog if needed.
	ref := server + "/" + tool
	if _, ok := h.cat.Describe(ref); !ok {
		if err := h.cat.RefreshServer(ctx, server); err != nil {
			h.log.Warn("call: catalog refresh", "server", server, "err", err)
		}
	}

	raw, err := h.brok.CallTool(ctx, server, tool, toolArgs)
	if err != nil {
		return "", fmt.Errorf("call %s/%s: %w", server, tool, err)
	}

	return h.buildResponse(raw, pageSize, 1)
}

// callCursorNext fetches the next page for an existing cursor.
func (h *Handlers) callCursorNext(cursor string) (string, error) {
	page, nextCursor, hasMore, ok := h.pager.Next(cursor)
	if !ok {
		return "", fmt.Errorf("call: cursor %q not found or expired", cursor)
	}

	out := callResult{
		Result:     page,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// buildResponse applies max_result_bytes safety net and pagination.
func (h *Handlers) buildResponse(raw string, pageSize, pageNum int) (string, error) {
	maxBytes := h.cfg.Proxy.MaxResultBytes

	// Safety net: if raw exceeds hard limit AND pageSize is larger, truncate with hint.
	if maxBytes > 0 && pageSize > maxBytes && len(raw) > maxBytes {
		truncated := truncateBytes(raw, maxBytes)
		out := callResult{
			Result:    truncated,
			Truncated: true,
			Hint:      fmt.Sprintf("result truncated at %d bytes; use page_size <= %d to paginate", maxBytes, maxBytes),
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		return string(b), nil
	}

	// Normal pagination via PageStore.
	firstPage, cursor, hasMore := h.pager.Store(raw, pageSize)
	out := callResult{
		Result:     firstPage,
		NextCursor: cursor,
		HasMore:    hasMore,
		Page:       pageNum,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// resolveTarget extracts server+tool from tool_ref or explicit fields.
func (h *Handlers) resolveTarget(args map[string]any) (server, tool string, err error) {
	if ref, ok := args["tool_ref"].(string); ok && ref != "" {
		return splitRef(ref)
	}
	server, _ = args["server"].(string)
	tool, _ = args["tool"].(string)
	if server == "" || tool == "" {
		return "", "", fmt.Errorf("call: provide tool_ref or server+tool")
	}
	return server, tool, nil
}

func extractArguments(args map[string]any) map[string]any {
	if a, ok := args["arguments"].(map[string]any); ok {
		return a
	}
	return map[string]any{}
}

// DescribeTool returns full schema for a tool ref.
// args: {"tool_ref": "server/tool"}
func (h *Handlers) DescribeTool(ctx context.Context, args map[string]any) (string, error) {
	ref, _ := args["tool_ref"].(string)
	if ref == "" {
		return "", fmt.Errorf("describe_tool: tool_ref required")
	}

	server, tool, err := splitRef(ref)
	if err != nil {
		return "", err
	}
	if _, ok := h.cat.Describe(ref); !ok {
		if err := h.cat.RefreshServer(ctx, server); err != nil {
			return "", fmt.Errorf("describe_tool: refresh %q: %w", server, err)
		}
	}

	entry, ok := h.cat.Describe(ref)
	if !ok {
		return "", fmt.Errorf("describe_tool: tool %q not found", ref)
	}

	out := map[string]any{
		"tool_ref":     ref,
		"server":       server,
		"tool":         tool,
		"summary":      entry.Summary,
		"tags":         entry.Tags,
		"input_schema": json.RawMessage(entry.InputSchema),
		"dangerous":    entry.Dangerous,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ListServers returns configured backends and their status.
func (h *Handlers) ListServers(_ context.Context, _ map[string]any) (string, error) {
	type srvOut struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Enabled     bool     `json:"enabled"`
		Autostart   bool     `json:"autostart"`
	}
	var out []srvOut
	for _, s := range h.cfg.EnabledServers() {
		out = append(out, srvOut{
			Name:        s.Name,
			Description: s.Description,
			Tags:        s.Tags,
			Enabled:     s.Enabled,
			Autostart:   s.Autostart,
		})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func splitRef(ref string) (server, tool string, err error) {
	for i, c := range ref {
		if c == '/' {
			return ref[:i], ref[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("invalid tool_ref %q: expected server/tool", ref)
}

// truncateBytes caps s to at most max bytes without splitting a UTF-8 rune.
// max <= 0 means no limit.
func truncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	// Back off to a rune boundary so we don't emit a broken codepoint.
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… [truncated]"
}
