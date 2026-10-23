// Command mockmcp is a minimal MCP stdio server used only by broker tests.
//
// Behaviour is driven by environment variables:
//
//	MOCK_TOOLS       comma-separated tool names (default: "echo")
//	MOCK_CALL_DELAY  duration to sleep on every tools/call (e.g. "50ms")
//	MOCK_CALL_ERROR  if non-empty, tools/call returns isError:true with this text
//	MOCK_INIT_DELAY  duration to sleep before responding to initialize
//	MOCK_DIE_ON_INIT if "1", exit(1) before responding to initialize
//	MOCK_DIE_AFTER   exit(0) after this many tools/call requests
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func main() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	out := json.NewEncoder(os.Stdout)

	tools := strings.Split(envOr("MOCK_TOOLS", "echo"), ",")
	callDelay, _ := time.ParseDuration(os.Getenv("MOCK_CALL_DELAY"))
	initDelay, _ := time.ParseDuration(os.Getenv("MOCK_INIT_DELAY"))
	callError := os.Getenv("MOCK_CALL_ERROR")
	dieOnInit := os.Getenv("MOCK_DIE_ON_INIT") == "1"
	dieAfter, _ := strconv.Atoi(os.Getenv("MOCK_DIE_AFTER"))

	calls := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if req.ID == nil {
			continue // notification
		}

		switch req.Method {
		case "initialize":
			if dieOnInit {
				os.Exit(1)
			}
			if initDelay > 0 {
				time.Sleep(initDelay)
			}
			write(out, *req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "mockmcp", "version": "0.0.1"},
			})

		case "tools/list":
			list := make([]map[string]any, 0, len(tools))
			for _, name := range tools {
				list = append(list, map[string]any{
					"name":        name,
					"description": "mock tool " + name,
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				})
			}
			write(out, *req.ID, map[string]any{"tools": list})

		case "tools/call":
			calls++
			if dieAfter > 0 && calls > dieAfter {
				os.Exit(0)
			}
			if callDelay > 0 {
				time.Sleep(callDelay)
			}
			if rpcCode, _ := strconv.Atoi(os.Getenv("MOCK_RPC_ERROR_CODE")); rpcCode != 0 {
				_ = out.Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      *req.ID,
					"error": map[string]any{
						"code":    rpcCode,
						"message": os.Getenv("MOCK_RPC_ERROR_MSG"),
					},
				})
				continue
			}
			if callError != "" {
				write(out, *req.ID, map[string]any{
					"content": []map[string]any{{"type": "text", "text": callError}},
					"isError": true,
				})
				continue
			}
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			write(out, *req.ID, map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": fmt.Sprintf("called %s args=%v", p.Name, p.Arguments),
				}},
			})
			if notifyMsg := os.Getenv("MOCK_NOTIFY_MESSAGE"); notifyMsg != "" {
				_ = out.Encode(map[string]any{
					"jsonrpc": "2.0",
					"method":  "notifications/message",
					"params": map[string]any{
						"level": "info",
						"data":  notifyMsg,
					},
				})
			}
		}
	}
}

func write(out *json.Encoder, id int64, result any) {
	_ = out.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
