// Package meta implements the meta-tool handlers: search, call, describe_tool, list_servers.
package meta

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nikita-popov/lazy-mcp/internal/broker"
	"github.com/nikita-popov/lazy-mcp/internal/catalog"
	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
)

// Handlers holds dependencies for meta-tool execution.
type Handlers struct {
	cfg  *config.Config
	brok *broker.Broker
	cat  *catalog.Catalog
	log  *logx.Logger
}

// New creates Handlers.
func New(cfg *config.Config, brok *broker.Broker, cat *catalog.Catalog, log *logx.Logger) *Handlers {
	return &Handlers{cfg: cfg, brok: brok, cat: cat, log: log}
}

// Search finds tools matching query.
// args: {"query": "...", "tags": ["..."], "limit": N}
func (h *Handlers) Search(ctx context.Context, args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	limit := h.cfg.Proxy.MaxSearchResults
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = int(l)
	}

	// Lazy refresh: if catalog is empty, try to refresh all servers.
	if len(h.cat.Entries()) == 0 {
		for _, srv := range h.cfg.EnabledServers() {
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

// Call routes a tool call to the correct backend.
// args: {"tool_ref": "server/tool", "arguments": {...}}
//   OR  {"server": "name", "tool": "name", "arguments": {...}}
func (h *Handlers) Call(ctx context.Context, args map[string]any) (string, error) {
	server, tool := "", ""

	if ref, ok := args["tool_ref"].(string); ok && ref != "" {
		var err error
		server, tool, err = splitRef(ref)
		if err != nil {
			return "", err
		}
	} else {
		server, _ = args["server"].(string)
		tool, _ = args["tool"].(string)
	}

	if server == "" || tool == "" {
		return "", fmt.Errorf("call: provide tool_ref or server+tool")
	}

	var toolArgs map[string]any
	if a, ok := args["arguments"].(map[string]any); ok {
		toolArgs = a
	} else {
		toolArgs = map[string]any{}
	}

	h.log.Info("call", "server", server, "tool", tool)

	// Lazy refresh catalog if needed.
	ref := server + "/" + tool
	if _, ok := h.cat.Describe(ref); !ok {
		if err := h.cat.RefreshServer(ctx, server); err != nil {
			h.log.Warn("call: catalog refresh", "server", server, "err", err)
		}
	}

	result, err := h.brok.CallTool(ctx, server, tool, toolArgs)
	if err != nil {
		return "", fmt.Errorf("call %s/%s: %w", server, tool, err)
	}
	return result, nil
}

// DescribeTool returns full schema for a tool ref.
// args: {"tool_ref": "server/tool"}
func (h *Handlers) DescribeTool(ctx context.Context, args map[string]any) (string, error) {
	ref, _ := args["tool_ref"].(string)
	if ref == "" {
		return "", fmt.Errorf("describe_tool: tool_ref required")
	}

	// Ensure catalog has this server indexed.
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
