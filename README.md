# lazy-mcp

MCP proxy with lazy-load and meta-tools (`search`, `call`, `describe_tool`).

Model works with **one** MCP server (this proxy). Real stdio MCP backends are started on demand and indexed in a compact catalog — no tool-manifest flooding of the context window.

## How it works

```
Model
  ↕  stdio / MCP
lazy-mcp  (this proxy)
  ├── meta/search(query)       → returns compact tool refs
  ├── meta/call(ref, args)     → routes to backend, starts it if needed
  └── meta/describe_tool(ref) → returns full schema on demand
        ↕  stdio / JSON-RPC 2.0
  ├── mempalace  subprocess
  ├── dav-mcp    subprocess  (started lazily)
  └── ...        subprocess  (started lazily)
```

## Quick start

```sh
# build
go build -o bin/lazy-mcp ./cmd/lazy-mcp

# run (stdio transport, config from file)
./bin/lazy-mcp --config config.yaml
```

## Configuration

See [`config.example.yaml`](config.example.yaml).

## Meta-tools

| Tool | Description |
|---|---|
| `search` | Find tools by query/tags across all registered backends |
| `call` | Call a tool by ref (`server/tool`) or explicit `server`+`tool` |
| `describe_tool` | Return full JSON schema for a tool ref |
| `list_servers` | List configured backend servers and their status |

## Project layout

```
cmd/lazy-mcp/        main entrypoint
internal/config/     YAML config loader
internal/catalog/    tool index + scoring
internal/broker/     subprocess lifecycle manager
internal/meta/       meta-tool handlers
internal/mcp/        MCP server frontend (stdio)
internal/policy/     allow/deny, timeouts, result limits
internal/logx/       structured JSONL logger
```

## License

MIT
