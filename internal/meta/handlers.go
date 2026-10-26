// Package meta implements the meta-tool handlers: search, call, describe_tool, list_servers.
package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/subconscious-labs/lazy-mcp/internal/broker"
	"github.com/subconscious-labs/lazy-mcp/internal/catalog"
	"github.com/subconscious-labs/lazy-mcp/internal/config"
	"github.com/subconscious-labs/lazy-mcp/internal/logx"
	"github.com/subconscious-labs/lazy-mcp/internal/policy"
	"github.com/subconscious-labs/lazy-mcp/internal/session"
)

// Handlers holds dependencies for meta-tool execution.
type Handlers struct {
	cfg    *config.Holder
	brok   *broker.Broker
	cat    *catalog.Catalog
	policy *policy.Policy
	log    *logx.Logger
	pager  *PageStore

	// reloadFn is injected by main. nil means meta_reload is not
	// available in this build (e.g., unit tests without a config file).
	reloadFn   func() ([]string, error)
	reloadMu   sync.Mutex
	lastReload time.Time
}

// New creates Handlers.
func New(cfg *config.Holder, brok *broker.Broker, cat *catalog.Catalog, pol *policy.Policy, log *logx.Logger, reloadFn func() ([]string, error)) *Handlers {
	return &Handlers{
		cfg:      cfg,
		brok:     brok,
		cat:      cat,
		policy:   pol,
		log:      log,
		pager:    NewPageStore(cfg.Get().Proxy.CursorTTL),
		reloadFn: reloadFn,
	}
}

// sessionUser returns the session user ("" means admin/local).
func sessionUser(ctx context.Context) (string, *session.Session) {
	sess, _ := session.FromContext(ctx)
	if sess == nil {
		return "", nil
	}
	return sess.User, sess
}

// Search finds tools matching query.
// args: {"query": "...", "limit": N}
// Response: {"hits": [...], "total": N, "has_more": bool, "warnings": [...]}
func (h *Handlers) Search(ctx context.Context, args map[string]any) (string, error) {
	user, sess := sessionUser(ctx)

	query, _ := args["query"].(string)
	limit := h.cfg.Get().Proxy.MaxSearchResults
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = int(l)
	}

	for _, srv := range h.cfg.Get().EnabledServers() {
		if sess != nil && !sess.CanSeeServer(srv.Name) {
			continue
		}
		if h.cat.ServerStale(srv.Name) {
			if err := h.cat.RefreshServer(ctx, srv.Name); err != nil {
				h.log.Warn("search: refresh failed", "server", srv.Name, "err", err)
			}
		}
	}

	visible := func(e catalog.ToolEntry) bool {
		if sess == nil {
			return true
		}
		if e.Server != "" && !sess.CanSeeServer(e.Server) {
			return false
		}
		if e.Tool == "" {
			// Server-only probe from Catalog's warning path.
			return sess.CanSeeServer(e.Server)
		}
		return h.policy.Visible(user, e.Server, e.Tool)
	}

	result := h.cat.Search(query, limit, visible)

	type hitOut struct {
		Ref         string   `json:"tool_ref"`
		Server      string   `json:"server"`
		Tool        string   `json:"tool"`
		Summary     string   `json:"summary"`
		Tags        []string `json:"tags"`
		InputHint   string   `json:"input_hint,omitempty"`
		Dangerous   bool     `json:"dangerous,omitempty"`
		Destructive bool     `json:"destructive,omitempty"`
	}
	hits := make([]hitOut, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, hitOut{
			Ref:         hit.Entry.Ref,
			Server:      hit.Entry.Server,
			Tool:        hit.Entry.Tool,
			Summary:     hit.Entry.Summary,
			Tags:        hit.Entry.Tags,
			InputHint:   hit.Entry.InputHint,
			Dangerous:   hit.Entry.Dangerous,
			Destructive: hit.Entry.Annotations.DestructiveHint,
		})
	}

	envelope := map[string]any{
		"hits":     hits,
		"total":    result.Total,
		"has_more": len(result.Hits) < result.Total,
	}
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
	Hint       string `json:"hint,omitempty"`
}

// Call routes a tool call to the correct backend with optional pagination.
func (h *Handlers) Call(ctx context.Context, args map[string]any) (string, error) {
	user, sess := sessionUser(ctx)

	// --- Cursor continuation path ---
	if cursor, ok := args["cursor"].(string); ok && cursor != "" {
		return h.callCursorNext(ctx, cursor, user, sess)
	}

	// --- First call path ---
	server, tool, err := h.resolveTarget(args)
	if err != nil {
		return "", err
	}
	if sess != nil && !sess.CanSeeServer(server) {
		return "", fmt.Errorf("call: server %q is not permitted for user %q", server, user)
	}
	if err := h.policy.Authorize(user, server, tool); err != nil {
		return "", err
	}

	toolArgs := extractArguments(args)

	if limit := h.cfg.Get().Proxy.MaxArgumentsBytes; limit > 0 {
		b, err := json.Marshal(toolArgs)
		if err != nil {
			return "", fmt.Errorf("call: marshal arguments: %w", err)
		}
		if len(b) > limit {
			return "", fmt.Errorf(
				"call: arguments too large (%d bytes, limit %d); set proxy.max_arguments_bytes to raise",
				len(b), limit)
		}
	}

	pageSize := h.cfg.Get().Proxy.DefaultPageSize
	if ps, ok := args["page_size"].(float64); ok && int(ps) > 0 {
		pageSize = int(ps)
	}

	h.log.Info("call", "server", server, "tool", tool, "page_size", pageSize, "user", user)

	// Refresh the catalog if the entry is missing OR stale. When
	// deny_destructive is enabled, a failed refresh is fatal: we cannot
	// trust stale annotations to say destructive=false.
	ref := server + "/" + tool
	_, found := h.cat.Describe(ref)
	if !found || h.cat.ServerStale(server) {
		if err := h.cat.RefreshServer(ctx, server); err != nil {
			if h.cfg.Get().Proxy.DenyDestructive {
				return "", fmt.Errorf("call: cannot verify destructive hint for %s: %w", ref, err)
			}
			h.log.Warn("call: catalog refresh", "server", server, "err", err)
		}
	}

	// deny_destructive: reject tools that self-report as destructive.
	if h.cfg.Get().Proxy.DenyDestructive {
		entry, ok := h.cat.Describe(ref)
		if !ok {
			return "", fmt.Errorf("call: tool %s not found in catalog; cannot verify destructive hint (deny_destructive is enabled)", ref)
		}
		if entry.Annotations.DestructiveHint {
			return "", fmt.Errorf("call: tool %s/%s is destructive and deny_destructive is set", server, tool)
		}
	}

	raw, err := h.brok.CallTool(ctx, server, tool, toolArgs)
	if err != nil {
		return "", fmt.Errorf("call %s/%s: %w", server, tool, err)
	}

	return h.buildResponse(raw, pageSize, user, server, tool)
}

// callCursorNext fetches the next page for an existing cursor.
func (h *Handlers) callCursorNext(_ context.Context, cursor, user string, sess *session.Session) (string, error) {
	page, nextCursor, hasMore, server, tool, ok := h.pager.Next(cursor, user)
	if !ok {
		// Do not leak existence of someone else's cursor.
		return "", fmt.Errorf("call: cursor %q not found or expired", cursor)
	}

	if sess != nil && !sess.CanSeeServer(server) {
		return "", fmt.Errorf("call: server %q is not permitted for user %q", server, user)
	}
	if err := h.policy.Authorize(user, server, tool); err != nil {
		return "", err
	}

	out := callResult{
		Result:     page, // always a string fragment
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// buildResponse applies the max_result_bytes safety net and pagination.
func (h *Handlers) buildResponse(raw string, pageSize int, user, server, tool string) (string, error) {
	if maxRaw := h.cfg.Get().Proxy.MaxRawResultBytes; maxRaw > 0 && len(raw) > maxRaw {
		return "", fmt.Errorf(
			"call: result too large (%d bytes, limit %d); raise proxy.max_raw_result_bytes to accept larger results",
			len(raw), maxRaw)
	}
	maxBytes := h.cfg.Get().Proxy.MaxResultBytes
	autoPaged := false

	if maxBytes > 0 && pageSize > maxBytes {
		pageSize = maxBytes
	}
	if maxBytes > 0 && pageSize <= 0 && len(raw) > maxBytes {
		pageSize = maxBytes
		autoPaged = true
	}

	firstPage, cursor, hasMore := h.pager.Store(raw, pageSize, user, server, tool)

	// When the result fits in a single page (or pagination is
	// disabled), parse it as JSON if possible. When it's a byte
	// fragment of a larger document, keep it as a string so the
	// caller never sees a chunk that happens to look like a
	// standalone JSON value.
	var resultValue any = firstPage
	if !hasMore {
		resultValue = wrapResult(firstPage)
	}

	out := callResult{
		Result:     resultValue,
		NextCursor: cursor,
		HasMore:    hasMore,
	}
	if hasMore {
		out.Page = 1
	}
	if autoPaged {
		out.Hint = fmt.Sprintf(
			"result exceeds max_result_bytes (%d); paginated automatically — pass next_cursor to continue",
			maxBytes)
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
func (h *Handlers) DescribeTool(ctx context.Context, args map[string]any) (string, error) {
	ref, _ := args["tool_ref"].(string)
	if ref == "" {
		return "", fmt.Errorf("describe_tool: tool_ref required")
	}

	server, tool, err := splitRef(ref)
	if err != nil {
		return "", err
	}

	user, sess := sessionUser(ctx)
	if sess != nil && !sess.CanSeeServer(server) {
		return "", fmt.Errorf("describe_tool: server %q is not permitted for user %q", server, user)
	}
	if err := h.policy.Authorize(user, server, tool); err != nil {
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
		"description":  entry.Description,
		"tags":         entry.Tags,
		"input_schema": json.RawMessage(entry.InputSchema),
		"dangerous":    entry.Dangerous,
		"annotations":  entry.Annotations,
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ListServers returns configured backends and their runtime status.
// args: {"include_disabled": bool}
func (h *Handlers) ListServers(ctx context.Context, args map[string]any) (string, error) {
	includeDisabled, _ := args["include_disabled"].(bool)

	servers := h.cfg.Get().EnabledServers()
	if includeDisabled {
		servers = h.cfg.Get().Servers
	}

	_, sess := sessionUser(ctx)
	if sess != nil && sess.Role() != "admin" {
		// Allocate a new slice to avoid mutating the shared config.
		var kept []config.ServerConfig
		for _, s := range servers {
			if sess.CanSeeServer(s.Name) {
				kept = append(kept, s)
			}
		}
		servers = kept
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
		if h.brok != nil {
			st := h.brok.Status(s.Name)
			item.Running = st.Running
			item.Circuit = st.Circuit
			item.LastError = st.LastError
			if !st.LastToolCall.IsZero() {
				item.LastToolCall = st.LastToolCall.UTC().Format(time.RFC3339)
			}
		} else {
			// Test mode: no broker wired.
			item.Circuit = "closed"
		}
		out = append(out, item)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WhoAmI reports the current session identity and how many servers are
// visible to it. Useful when meta_list_servers returns an empty list:
// distinguishes "no servers configured" from "ACL hides them".
// args: {}
func (h *Handlers) WhoAmI(ctx context.Context, _ map[string]any) (string, error) {
	user, sess := sessionUser(ctx)

	out := map[string]any{
		"user": user,
	}
	if sess != nil {
		out["role"] = sess.Role()
		out["allowed_servers"] = sess.AllowedServers()
		out["session_id"] = sess.ID
	} else {
		out["role"] = "admin"
		out["allowed_servers"] = []string{}
	}

	all := h.cfg.Get().EnabledServers()
	visible := 0
	for _, s := range all {
		if sess == nil || sess.CanSeeServer(s.Name) {
			visible++
		}
	}
	out["total_servers"] = len(all)
	out["visible_servers"] = visible

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// splitRef parses "server/tool" into its two parts.
func splitRef(ref string) (server, tool string, err error) {
	i := strings.IndexByte(ref, '/')
	if i < 0 {
		return "", "", fmt.Errorf("invalid tool_ref %q: expected server/tool", ref)
	}
	server, tool = ref[:i], ref[i+1:]
	if err := validateRefPart("server", server); err != nil {
		return "", "", fmt.Errorf("invalid tool_ref %q: %w", ref, err)
	}
	if err := validateRefPart("tool", tool); err != nil {
		return "", "", fmt.Errorf("invalid tool_ref %q: %w", ref, err)
	}
	return server, tool, nil
}

const maxRefPartBytes = 256

func validateRefPart(kind, part string) error {
	if part == "" {
		return fmt.Errorf("%s part is empty", kind)
	}
	if len(part) > maxRefPartBytes {
		return fmt.Errorf("%s part exceeds %d bytes", kind, maxRefPartBytes)
	}
	if strings.TrimSpace(part) != part {
		return fmt.Errorf("%s part has leading or trailing whitespace", kind)
	}
	for _, r := range part {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s part contains control character %q", kind, r)
		}
	}
	return nil
}

// truncateBytes caps s to at most limit bytes without splitting a UTF-8 rune.
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

// wrapResult returns raw as json.RawMessage when it looks like valid
// JSON, otherwise as a plain string.
func wrapResult(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		if json.Valid([]byte(trimmed)) {
			return json.RawMessage(trimmed)
		}
	}
	return raw
}
