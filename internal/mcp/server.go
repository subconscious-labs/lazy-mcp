// Package mcp implements the stdio MCP frontend server.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/nikita-popov/lazy-mcp/internal/broker"
	"github.com/nikita-popov/lazy-mcp/internal/catalog"
	"github.com/nikita-popov/lazy-mcp/internal/config"
	"github.com/nikita-popov/lazy-mcp/internal/logx"
	"github.com/nikita-popov/lazy-mcp/internal/meta"
)

// Server is the MCP stdio frontend.
type Server struct {
	cfg      *config.Config
	handlers *meta.Handlers
	log      *logx.Logger
}

// NewServer creates a Server.
func NewServer(cfg *config.Config, brok *broker.Broker, cat *catalog.Catalog, log *logx.Logger) *Server {
	return &Server{
		cfg:      cfg,
		handlers: meta.New(cfg, brok, cat, log),
		log:      log,
	}
}

// Run reads JSON-RPC from r and writes responses to w.
// It returns when ctx is done, on EOF, or on a fatal encode error.
//
// stdin is read on a separate goroutine because bufio.Scanner.Scan blocks
// and cannot observe ctx cancellation. The main loop selects on the reader
// channel and ctx, so SIGTERM interrupts it promptly.
func (s *Server) Run(ctx context.Context, r io.Reader, w io.Writer) error {
	enc := json.NewEncoder(w)

	lines := make(chan string)
	scanErr := make(chan error, 1) // buffered: reader never blocks on send
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				scanErr <- nil
				return
			}
		}
		scanErr <- scanner.Err()
	}()

	for {
		select {
		case <-ctx.Done():
			s.log.Info("shutdown: context cancelled, stopping request loop")
			return nil
		case line, ok := <-lines:
			if !ok {
				return <-scanErr
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var req struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      any             `json:"id"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				s.log.Warn("parse error", "line", line)
				continue
			}
			s.log.Debug("req", "id", req.ID, "method", req.Method)
			resp := s.handle(ctx, req.ID, req.Method, req.Params)
			if resp != nil {
				if err := enc.Encode(resp); err != nil {
					return fmt.Errorf("encode response: %w", err)
				}
			}
		}
	}
}

func (s *Server) handle(ctx context.Context, id any, method string, params json.RawMessage) any {
	prefix := s.cfg.Proxy.ToolPrefix

	switch method {
	case "initialize":
		return s.respond(id, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":   map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "lazy-mcp",
				"version": "0.1.0",
			},
		})

	case "notifications/initialized", "notifications/cancelled":
		return nil // no response for notifications

	case "tools/list":
		return s.respond(id, map[string]any{"tools": s.toolManifest(prefix)})

	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return s.errResp(id, -32602, "invalid params: "+err.Error())
		}
		result, err := s.dispatch(ctx, prefix, p.Name, p.Arguments)
		if err != nil {
			s.log.Warn("tool error", "tool", p.Name, "err", err)
			return s.respond(id, map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			})
		}
		return s.respond(id, map[string]any{
			"content": []map[string]any{{"type": "text", "text": result}},
		})

	default:
		return s.errResp(id, -32601, "method not found: "+method)
	}
}

func (s *Server) dispatch(ctx context.Context, prefix, name string, args map[string]any) (string, error) {
	short := strings.TrimPrefix(name, prefix+"_")
	switch short {
	case "search":
		return s.handlers.Search(ctx, args)
	case "call":
		return s.handlers.Call(ctx, args)
	case "describe_tool":
		return s.handlers.DescribeTool(ctx, args)
	case "list_servers":
		return s.handlers.ListServers(ctx, args)
	default:
		return "", fmt.Errorf("unknown meta-tool: %s", name)
	}
}

func (s *Server) toolManifest(prefix string) []map[string]any {
	return []map[string]any{
		{
			"name":        prefix + "_search",
			"description": "Search for tools across all registered MCP backends. Returns compact tool refs with summaries. Use this first to discover available tools.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Search query (tool name, action, or keyword)"},
					"limit": map[string]any{"type": "integer", "description": "Max results (default: 8)"},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        prefix + "_call",
			"description": "Call a backend tool by tool_ref (e.g. \"mempalace/mempalacekgquery\") or by server+tool. Pass arguments as an object.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool_ref":  map[string]any{"type": "string", "description": "Tool reference: server/toolname"},
					"server":    map[string]any{"type": "string", "description": "Backend server name (alternative to tool_ref)"},
					"tool":      map[string]any{"type": "string", "description": "Tool name (alternative to tool_ref)"},
					"arguments": map[string]any{"type": "object", "description": "Tool arguments"},
				},
			},
		},
		{
			"name":        prefix + "_describe_tool",
			"description": "Get full JSON schema and description for a tool ref. Use when you need exact argument names before calling.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool_ref": map[string]any{"type": "string", "description": "Tool reference: server/toolname"},
				},
				"required": []string{"tool_ref"},
			},
		},
		{
			"name":        prefix + "_list_servers",
			"description": "List MCP backend servers with runtime status (running, circuit state, last tool call). By default only enabled servers are shown; pass include_disabled:true to see the full registry.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"include_disabled": map[string]any{
						"type":        "boolean",
						"description": "Include disabled servers (default: false)",
					},
				},
			},
		},
	}
}

func (s *Server) respond(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func (s *Server) errResp(id any, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
}
