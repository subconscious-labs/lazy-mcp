package broker

import (
	"encoding/json"
	"fmt"
	"strings"
)

// decodeToolsListPage decodes one page of tools/list and returns the
// tools plus the next cursor ("" if this is the last page).
func decodeToolsListPage(res json.RawMessage) ([]ToolInfo, string, error) {
	type rawTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
		Annotations ToolAnnotations `json:"annotations,omitempty"`
	}
	var lr struct {
		Tools      []rawTool `json:"tools"`
		NextCursor string    `json:"nextCursor,omitempty"`
	}
	if err := json.Unmarshal(res, &lr); err != nil {
		return nil, "", fmt.Errorf("parse tools/list: %w", err)
	}
	tools := make([]ToolInfo, 0, len(lr.Tools))
	for _, t := range lr.Tools {
		tools = append(tools, ToolInfo(t))
	}
	return tools, lr.NextCursor, nil
}

// decodeCallToolResult decodes a tools/call result. It preserves
// structuredContent, isError and non-text content (image/audio/
// resource) instead of silently dropping them.
func decodeCallToolResult(res json.RawMessage) (string, error) {
	type contentItem struct {
		Type     string          `json:"type"`
		Text     string          `json:"text,omitempty"`
		MimeType string          `json:"mimeType,omitempty"`
		Data     string          `json:"data,omitempty"`
		Resource json.RawMessage `json:"resource,omitempty"`
	}
	type callResult struct {
		Content           []contentItem   `json:"content"`
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	}
	var cr callResult
	if err := json.Unmarshal(res, &cr); err != nil {
		return "", fmt.Errorf("parse tools/call: %w", err)
	}

	if cr.IsError {
		var parts []string
		for _, c := range cr.Content {
			if c.Type == "text" {
				parts = append(parts, c.Text)
			}
		}
		return "", fmt.Errorf("%w: %s", ErrToolError, strings.Join(parts, "\n"))
	}

	if len(cr.StructuredContent) > 0 && string(cr.StructuredContent) != "null" {
		return string(cr.StructuredContent), nil
	}

	var parts []string
	var nonText []string
	for _, c := range cr.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "audio":
			nonText = append(nonText, fmt.Sprintf("[%s content omitted: mimeType=%s]", c.Type, c.MimeType))
		case "resource":
			nonText = append(nonText, "[resource content omitted]")
		default:
			nonText = append(nonText, fmt.Sprintf("[%s content omitted]", c.Type))
		}
	}
	if len(nonText) > 0 {
		parts = append(parts, nonText...)
	}
	return strings.Join(parts, "\n"), nil
}

// rpcErrorShape mirrors the JSON-RPC error object.
type rpcErrorShape struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// classifyRPCError maps a JSON-RPC error object to ErrRPCClientError
// (client-side codes -32700..-32602) or a generic error.
func classifyRPCError(e *rpcErrorShape) error {
	if e.Code >= -32700 && e.Code <= -32602 {
		return fmt.Errorf("%w: code %d: %s", ErrRPCClientError, e.Code, e.Message)
	}
	return fmt.Errorf("rpc error %d: %s", e.Code, e.Message)
}
