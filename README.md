![release](https://github.com/subconscious-labs/lazy-mcp/actions/workflows/release.yml/badge.svg)

# lazy-mcp

MCP proxy with lazy-load and meta-tools (`search`, `call`, `describe_tool`, `list_servers`).

The model talks to **one** MCP server — this proxy. Real stdio MCP backends are
started on demand, indexed into a compact catalog, and their full tool manifests
never reach the context window. Backends that misbehave are isolated by a
per-server circuit breaker; idle subprocesses are reaped automatically.

## How it works

```
Model
  ↕  stdio / MCP
lazy-mcp (this proxy)
  ├── meta_search(query)              → compact tool refs + warnings
  ├── meta_call(tool_ref, args)       → routes to backend, starts it if needed
  ├── meta_describe_tool(tool_ref)    → full JSON schema on demand
  └── meta_list_servers               → registry + runtime status
        ↕  stdio / JSON-RPC 2.0
  ├── mempalace  subprocess           (started lazily)
  ├── dav-mcp    subprocess           (started lazily)
  ├── monitor    subprocess           (started lazily)
  └── ...
```

Key properties:

- **Lazy start.** A backend process is spawned on first use, not at boot
  (unless `warmup: true` or `autostart: true` on the server).
- **Compact catalog.** `meta_search` returns short refs, summaries, tags and a
  one-line input hint — not full schemas. Full schemas come from
  `meta_describe_tool` when actually needed.
- **Resilience.** Each backend has its own circuit breaker. A backend that
  fails repeatedly is short-circuited for a cooldown, then probed once
  (half-open) before being trusted again.
- **Idle reaping.** Backends that haven't received a `tools/call` for
  `idle_ttl` are killed; the next call transparently restarts them.
- **Bounded output.** Tool results are paginated or truncated (UTF-8 safe)
  before they reach the model.

## Quick start

```sh
# build
make build

# run (stdio transport, config from file)
./bin/lazy-mcp --config config.yaml
```

`lazy-mcp` speaks MCP over stdio on its own stdin/stdout. Point any MCP client
at the binary; it will see exactly four meta-tools.

## Configuration

See [`config.example.yaml`](config.example.yaml) for a full example.

### `proxy`

| Key | Default | Meaning |
|---|---|---|
| `tool_prefix` | `meta` | Prefix for exposed meta-tool names. |
| `max_search_results` | `8` | Cap on `meta_search` hits. |
| `warmup` | `false` | Start all enabled backends at boot instead of lazily. |
| `idle_ttl` | `10m` | Kill backends idle for this long. `0` = never. Only `tools/call` counts as activity. |
| `tool_cache_ttl` | `30m` | How long a backend's `tools/list` stays cached. |
| `exec_timeout` | `60s` | Per `tools/call` timeout. |
| `initialize_timeout` | `10s` | MCP handshake timeout when starting a backend. |
| `max_result_bytes` | `262144` (256 KiB) | Hard safety net on result size. Applies when pagination is disabled or set coarser than this. `0` = unlimited. |
| `default_page_size` | `0` | Pagination size for `meta_call` when `page_size` isn't passed. `0` = pagination off. |
| `cursor_ttl` | `5m` | How long paged cursors survive in memory. |
| `shutdown_timeout` | `5s` | Grace period for in-flight requests on SIGINT/SIGTERM. |

#### `proxy.circuit_breaker`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | Set to `false` to disable isolation entirely. |
| `failure_threshold` | `5` | Consecutive transport-level failures before the circuit opens. |
| `cooldown` | `30s` | How long the circuit stays open before a single probe is allowed. |

Only **transport-level** failures count: failed spawn, RPC error, deadline,
backend stream closed. Tool-level errors (`isError: true` in the MCP result)
and client-side cancellation do **not** trip the breaker.

### `servers[]`

| Key | Meaning |
|---|---|
| `name` | Unique name; used in `server/tool` refs. |
| `transport` | `stdio` (only stdio is supported today). |
| `command`, `args` | How to spawn the backend. |
| `env` | Extra environment variables for the child process. |
| `description` | Shown in `meta_list_servers`; also indexed for `meta_search`. |
| `tags` | Indexed for `meta_search`. |
| `autostart` | Start at boot, independently of `warmup`. |
| `enabled` | If `false`, the server is not started, not indexed, and rejected on call. |
| `dangerous_tools` | Tool names flagged as dangerous in `meta_describe_tool` output. |

## Meta-tools

| Tool | Description |
|---|---|
| `search` | Find tools by query. Returns `{"hits": [...], "warnings": [...]}`. |
| `call` | Call a backend tool by `tool_ref` (`server/tool`) or explicit `server`+`tool`. Supports pagination. |
| `describe_tool` | Full JSON schema and description for a tool ref. |
| `list_servers` | Registry with runtime status. `include_disabled: true` shows disabled servers too. |

### `search`

```jsonc
// args
{ "query": "execute command", "limit": 8 }

// result
{
  "hits": [
    { "tool_ref": "ssh/ssh_exec", "server": "ssh", "tool": "ssh_exec",
      "summary": "Execute a command on a remote host via SSH",
      "tags": ["ssh"], "input_hint": "host:string, command:string" }
  ],
  "warnings": [
    { "server": "monitor", "terms": ["metrics"],
      "reason": "matched server metadata but no tools are indexed (catalog unavailable or empty)" }
  ]
}
```

Search semantics:

- The query is split into words. Each word is scored against tool name, ref,
  server name, tags and summary.
- A word that matches **nothing anywhere** disqualifies the whole query
  (AND semantics across words).
- A word that matches only server metadata (name/description/tags) contributes
  no score but yields a `warning`. This is how a broken-but-`enabled` server
  stays visible: `{"hits": [], "warnings": [{"server": "monitor", ...}]}` is
  distinguishable from `{"hits": []}` (no match anywhere).
- `warnings` is omitted when empty.

### `call`

```jsonc
// first call
{ "tool_ref": "tasks/get__tasks", "arguments": { "per_page": 50 }, "page_size": 4096 }

// subsequent pages
{ "cursor": "<next_cursor from previous response>" }
```

Response envelope:

```jsonc
{
  "result": "…",         // first page (or full result if pagination off)
  "page": 1,             // our page counter (not the backend's)
  "has_more": true,      // present when more pages exist
  "next_cursor": "…",    // opaque, use in subsequent meta_call
  "truncated": true,     // present when max_result_bytes kicked in
  "hint": "…"            // how to paginate next time
}
```

### `list_servers`

```jsonc
// args
{ "include_disabled": false }

// result (array)
[
  {
    "name": "git",
    "description": "Git with Forgejo",
    "tags": ["git"],
    "enabled": true,
    "autostart": false,
    "running": true,
    "circuit": "closed",             // closed | open | half-open | disabled
    "last_tool_call": "2026-10-15T10:49:42Z",
    "last_error": "broker: start ..." // present only after a failure
  }
]
```

`last_tool_call` advances on `tools/call` only — never on `tools/list` or
process restarts. Idle reaper uses `max(startedAt, last_tool_call)`.

## Development

```sh
make build        # production binary (-trimpath, -ldflags="-s -w")
make test         # go test ./...
make test-race    # go test -race ./...   (CI runs this)
make vet          # go vet ./...
make lint         # go vet + golangci-lint
make bench        # benchmarks for internal/catalog
make check        # build + test  (pre-push convenience)
make ci           # lint + test-race
```

The `broker` integration tests compile a small mock MCP server from
`internal/broker/testdata/mockmcp` and drive it in-process; no network access
is required. Behaviour is controlled with environment variables
(`MOCK_TOOLS`, `MOCK_CALL_DELAY`, `MOCK_CALL_ERROR`, `MOCK_INIT_DELAY`,
`MOCK_DIE_ON_INIT`, `MOCK_DIE_AFTER`).

## Project layout

```
cmd/lazy-mcp/        main entrypoint, shutdown orchestration
internal/config/     YAML config loader + defaults
internal/catalog/    tool index, word scoring, search warnings
internal/broker/     subprocess lifecycle, circuit breaker, idle reaper
  testdata/mockmcp/  mock MCP server for integration tests
internal/meta/       meta-tool handlers, pagination
internal/mcp/        MCP server frontend (stdio)
internal/logx/       structured JSONL logger (stderr)
```

## Status

Implemented:

- Lazy backend startup, warmup, idle reaping.
- Circuit breaker with half-open probe.
- Pagination and UTF-8-safe truncation for `meta_call` results.
- Word-based search with server-metadata warnings.
- Graceful shutdown (SIGINT/SIGTERM with grace period, second signal forces exit).
- Unit + integration tests, race-clean.

Not yet:

- HTTP transport (only stdio today).
- Hot-reload of config (SIGHUP).
- Runtime add/remove of servers.
- Structured logging to file (logs go to stderr).
- OpenTelemetry / Prometheus metrics.
- Semantic search (currently word-based scoring only).

## License

MIT
