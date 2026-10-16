// Package meta implements the meta-tool handlers: search, call, describe_tool, list_servers.
package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
// args: {"query": "...", "limit": N}
// Response: {"hits": [...], "warnings": [...]}
func (h *Handlers) Search(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	limit := h.cfg.Proxy.MaxSearchResults
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = int(l)
	}

	for _, srv := range h.cfg.EnabledServers() {
		if h.cat.ServerStale(srv.Name) {
			if err := h.cat.RefreshServer(ctx, srv.Name); err != nil {
				h.log.Warn("search: refresh failed", "server", srv.Name, "err", err)
			}
		}
	}

	result := h.cat.Search(query, limit)

	type hitOut struct {
		Ref       string   `json:"tool_ref"`
		Server    string   `json:"server"`
		Tool      string   `json:"tool"`
		Summary   string   `json:"summary"`
		Tags      []string `json:"tags"`
		InputHint string   `json:"input_hint,omitempty"`
		Dangerous bool     `json:"dangerous,omitempty"`
	}
	hits := make([]hitOut, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, hitOut{
			Ref:       hit.Entry.Ref,
			Server:    hit.Entry.Server,
			Tool:      hit.Entry.Tool,
			Summary:   hit.Entry.Summary,
			Tags:      hit.Entry.Tags,
			InputHint: hit.Entry.InputHint,
			Dangerous: hit.Entry.Dangerous,
		})
	}

	envelope := map[string]any{"hits": hits}
	if len(result.Warnings) > 0 {
		envelope["warnings"] = result.Warnings
	}
	b, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// callResult is the envelope returned by Call.
type callResult struct {
	Result     any    `json:"result"`
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

	return h.buildResponse(raw, pageSize)
}

// callCursorNext fetches the next page for an existing cursor.
func (h *Handlers) callCursorNext(cursor string) (string, error) {
	page, nextCursor, hasMore, ok := h.pager.Next(cursor)
	if !ok {
		return "", fmt.Errorf("call: cursor %q not found or expired", cursor)
	}

	out := callResult{
		Result:     wrapResult(page),
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
func (h *Handlers) buildResponse(raw string, pageSize int) (string, error) {
	maxBytes := h.cfg.Proxy.MaxResultBytes
	// Safety net: if raw exceeds the hard limit, and pagination either disabled
	// or set too coarse to help, truncate with a hint.
	if maxBytes > 0 && len(raw) > maxBytes && (pageSize <= 0 || pageSize > maxBytes) {
		truncated := truncateBytes(raw, maxBytes)
		out := callResult{
			Result:    wrapResult(truncated),
			Truncated: true,
			Hint:      fmt.Sprintf("result truncated at %d bytes; use page_size <= %d to paginate", maxBytes, maxBytes),
		}
		b, _ := json.MarshalIndent(out, "", "  ")
		return string(b), nil
	}
	// Normal pagination via PageStore.
	firstPage, cursor, hasMore := h.pager.Store(raw, pageSize)
	out := callResult{
		Result:     wrapResult(firstPage),
		NextCursor: cursor,
		HasMore:    hasMore,
	}
	if hasMore {
		out.Page = 1
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

// ListServers returns configured backends and their runtime status.
// args: {"include_disabled": bool}
//
// By default only enabled servers are listed. Pass include_disabled:true
// to see the full registry (disabled entries have circuit:"disabled" and
// running:false).
func (h *Handlers) ListServers(_ context.Context, args map[string]any) (string, error) {
	includeDisabled, _ := args["include_disabled"].(bool)

	servers := h.cfg.EnabledServers()
	if includeDisabled {
		servers = h.cfg.Servers
	}

	type srvOut struct {
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		Tags         []string `json:"tags"`
		Enabled      bool     `json:"enabled"`
		Autostart    bool     `json:"autostart"`
		Running      bool     `json:"running"`
		Circuit      string   `json:"circuit"`
		LastToolCall string   `json:"last_tool_call,omitempty"`
		LastError    string   `json:"last_error,omitempty"`
	}
	out := make([]srvOut, 0, len(servers))
	for _, s := range servers {
		item := srvOut{
			Name:        s.Name,
			Description: s.Description,
			Tags:        s.Tags,
			Enabled:     s.Enabled,
			Autostart:   s.Autostart,
		}
		if !s.Enabled {
			item.Circuit = "disabled"
			out = append(out, item)
			continue
		}
		st := h.brok.Status(s.Name)
		item.Running = st.Running
		item.Circuit = st.Circuit
		item.LastError = st.LastError
		if !st.LastToolCall.IsZero() {
			item.LastToolCall = st.LastToolCall.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
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
func truncateBytes(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… [truncated]"
}

// wrapResult returns raw as json.RawMessage when it looks like a JSON
// object or array, and as a plain string otherwise. This lets models
// consume structured tool output directly instead of unescaping a
// stringified JSON blob.
func wrapResult(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed)
		}
	}
	return raw
}
