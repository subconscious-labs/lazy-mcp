![release](https://github.com/subconscious-labs/lazy-mcp/actions/workflows/release.yml/badge.svg)

# lazy-mcp

MCP proxy with lazy-load, per-user policy and an optional HTTP daemon.
The model sees **six** meta-tools; real MCP backends are started on
demand, indexed into a compact catalog, and never reach the context
window directly.

## How it works

```
             Model
               ↕  stdio or HTTP
       ┌───────┴────────┐
       │    lazy-mcp    │
       │                │
       │  meta_search   │  ← compact tool refs
       │  meta_call     │  ← routes to backend, starts it lazily
       │  meta_describe │  ← full schema on demand
       │  meta_list_    │  ← registry + runtime status
       │  meta_whoami   │  ← session identity + visible servers
       │  meta_reload   │  ← re-read config (admin)
       └───────┬────────┘
               ↕  stdio, per backend
       ┌───────┴──────────────────────────┐
       │  dav  git-mcp  ssh  tasks  ...   │
       └──────────────────────────────────┘
```

Key properties:

- **Lazy start.** A backend subprocess is spawned on first use.
- **Compact catalog.** `meta_search` returns short refs, not full schemas.
- **Circuit breaker.** Misbehaving backends are short-circuited, then probed.
- **Idle reaping.** Backends with no `tools/call` for `idle_ttl` are stopped.
- **Bounded output.** Large results are paginated; pages are byte-level slices.
- **Notifications.** Downstream `notifications/message` and
  `tools/list_changed` are relayed to HTTP clients via SSE.
- **Per-session policy.** `role: agent` sees only `allowed_servers`;
  rate limit is per `(user, server, tool)`.

## Quick start

**stdio (MCP client subprocess):**

```sh
make build
./bin/lazy-mcp --config config.yaml
```

Any MCP client that spawns subprocesses can use it. The client sees six
meta-tools.

**HTTP daemon:**

```yaml
# config.yaml
proxy:
  transport: "http"
  http:
    addr: ":3333"
    session_ttl: 30m

auth:
  enabled: true
  users:
    - name: admin
      token_sha256: "<64 hex chars>"
      role: admin
    - name: elsa
      token_sha256: "<64 hex chars>"
      role: agent
      allowed_servers: [git, tasks]

include:
  - "conf.d/*.yaml"
```

```sh
./bin/lazy-mcp --config /etc/lazy-mcp/config.yaml

# health
curl -s localhost:3333/health

# MCP over HTTP
curl -s -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -X POST localhost:3333/mcp \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize"}' -D -

# SSE events (server → client notifications)
curl -N -H "Authorization: Bearer <token>" \
  -H "Mcp-Session-Id: <id>" \
  localhost:3333/mcp
```

To generate a token hash:

```sh
printf %s "$TOKEN" | sha256sum | awk '{print $1}'
```

## Configuration

See [`config.example.yaml`](config.example.yaml). Loaded once at startup;
send SIGHUP (or call `meta_reload`) to apply changes in place.

### `proxy`

| Key | Default | Meaning |
|---|---|---|
| `transport` | auto | `stdio`, `http`, `both`. Empty picks `http` if `http.addr` is set. |
| `tool_prefix` | `meta` | Prefix for meta-tool names. |
| `max_search_results` | `8` | Cap on `meta_search` hits. |
| `warmup` | `false` | Start all enabled backends at boot. |
| `idle_ttl` | `10m` | Stop backends idle this long. `0` = never. |
| `tool_cache_ttl` | `30m` | How long `tools/list` stays cached. |
| `exec_timeout` | `60s` | Per `tools/call` timeout. |
| `initialize_timeout` | `10s` | MCP handshake timeout when starting a backend. |
| `max_result_bytes` | `262144` | Hard cap; larger results paginate. `0` = unlimited. |
| `max_arguments_bytes` | `262144` | Cap on serialized `arguments`. `-1` = unlimited. |
| `default_page_size` | `0` | Pagination size when `page_size` isn't passed. `0` = auto only. |
| `cursor_ttl` | `5m` | How long paged cursors survive. |
| `shutdown_timeout` | `5s` | Grace period for in-flight work on SIGTERM. |
| `log_file` | `""` | JSONL also written to this path (rotated by lumberjack). |
| `log_file_max_mb` | `50` | Rotation threshold for `log_file`. |
| `log_file_max_backups` | `7` | Retained rotated files. |
| `log_file_max_age_days` | `30` | Max age of rotated files. |
| `log_file_compress` | `false` | Gzip rotated files. |
| `metrics_file` | `""` | Prometheus textfile path for node_exporter. |
| `metrics_interval` | `15s` | `15s` / `30s` / `60s`. |
| `deny_dangerous` | `false` | Reject tools from `server.dangerous_tools`. |
| `deny_destructive` | `false` | Reject tools with `annotations.destructiveHint: true`. |

#### `proxy.http`

| Key | Default | Meaning |
|---|---|---|
| `addr` | `""` | Listen address for HTTP MCP. Empty = stdio only. |
| `session_ttl` | `30m` | Idle session lifetime. |
| `allowed_origins` | localhost | CORS/Origin allow-list; supports `*` suffix. |

#### `proxy.circuit_breaker`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | |
| `failure_threshold` | `5` | Consecutive transport failures before opening. |
| `cooldown` | `30s` | Probe delay. |

#### `proxy.rate_limit`

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Opt-in token bucket per `(user, server, tool)`. |
| `per_minute` | `60` | Sustained rate. |
| `burst` | `10` | Instantaneous burst. |

### `auth`

```
auth.enabled:  bool
auth.users[]:
  name:             string   # label, appears in logs
  token_sha256:     string   # 64 hex chars
  role:             "admin" | "agent"
  allowed_servers:  [string] # required for role=agent; empty = no access
```

`role: agent` **without** `allowed_servers` is a load error. Visibility
filters: `meta_list_servers`, `meta_search`, `meta_describe_tool`,
`meta_call`. `denied_tools` and `deny_dangerous`/`deny_destructive`
apply to admin too; only the allow-list is admin-bypassable.

### `servers[]`

| Key | Meaning |
|---|---|
| `name` | Unique; used in `server/tool` refs. |
| `transport` | `stdio` (only supported today). |
| `command`, `args` | How to spawn the backend. |
| `env` | Extra environment variables. |
| `description`, `tags` | Indexed for `meta_search`. |
| `autostart` | Start at boot independently of `warmup`. |
| `enabled` | If `false`, not started, not indexed, rejected on call. |
| `allowed_tools` | If non-empty, only these tools are visible. |
| `denied_tools` | Always hidden and rejected. |
| `dangerous_tools` | Flagged in `describe_tool`; rejected when `deny_dangerous`. |

### `include`

```yaml
include:
  - "conf.d/*.yaml"
```

- Paths relative to the config file's directory; globs supported.
- `proxy` and `auth` may appear **only** in the top-level file.
- `servers` merge by name; the last definition wins (warning).
- `auth.users` duplicate names are an error.
- Nested `include` is rejected.
- `${...}` expansion works inside included files.

### Substitutions

Any string value may reference the environment or a file:

```
${NAME}            required env var (error if unset or empty)
${NAME:-default}   default if unset or empty
${NAME:?message}   required with custom error
${file:/path}      file contents, trailing newlines stripped
$$                 literal '$'
```

Values that look like bools/ints after expansion are re-tagged so
`enabled: ${SVC_ENABLED}` works. Cyclic references (an env value that
is itself a `${...}`) are rejected at load time.

## Meta-tools

| Tool | Description |
|---|---|
| `meta_search` | Find tools. Returns `{hits, total, has_more, warnings?}`. |
| `meta_call` | Call a backend tool. Supports pagination. |
| `meta_describe_tool` | Full schema, tags, `dangerous`, `annotations`. |
| `meta_list_servers` | Registry with runtime status. `include_disabled:true` shows the full registry (admin) or the allowed subset (agent). |
| `meta_whoami` | `{user, role, allowed_servers, session_id, total_servers, visible_servers}`. |
| `meta_reload` | Re-read the config file. **Admin only.** 5 s cooldown. |

### `meta_search`

```jsonc
// args
{ "query": "execute command", "limit": 8 }

// result
{
  "hits": [
    { "tool_ref": "ssh/ssh_exec", "server": "ssh", "tool": "ssh_exec",
      "summary": "...", "tags": ["ssh"],
      "input_hint": "host:string, command:string",
      "destructive": true }
  ],
  "total": 3,
  "has_more": false,
  "warnings": [{"server": "monitor", "terms": ["metrics"], "reason": "..."}]
}
```

Search semantics:

- Words are scored against tool name, ref, server, tags and summary.
- A word that matches **nothing** disqualifies the whole query.
- A word that matches only server metadata yields a **warning**
  (distinguishes "no such tool" from "backend down / catalog empty").
- Warnings are filtered by session visibility: an agent with
  `allowed_servers: [git]` never sees a warning about `monitor`,
  even when `monitor`'s catalog is empty.
- `warnings` is omitted when empty.

### `meta_call`

```jsonc
// first call
{ "tool_ref": "tasks/get__tasks", "arguments": {"per_page": 50}, "page_size": 4096 }

// subsequent pages
{ "cursor": "<next_cursor>" }
```

Response:

```jsonc
{
  "result": "…",         // first page (or full result if pagination off)
  "page": 1,             // emitted only when has_more
  "has_more": true,
  "next_cursor": "…",
  "hint": "…"            // only when auto-pagination kicked in
}
```

Pages are **byte-level slices** of the full result: concatenate
verbatim; individual pages are not necessarily valid JSON. `result`
is embedded as an object/array when the backend returned JSON.

### `meta_whoami`

Diagnostic for "empty list_servers": distinguishes "nothing
configured" from "ACL hides everything".

### `meta_reload`

```jsonc
{ "ok": true, "servers": 8, "enabled": 7, "names": ["dav","git"], "warnings": [] }
```

Does not mutate live sessions: `role` and `allowed_servers` take
effect on reconnect. Does not touch `log_file`, `metrics_file`,
`metrics_interval` or `cursor_ttl` — those require a full restart
(reload returns a warning).

## HTTP sessions and events

- `POST /mcp` — JSON-RPC request. First call returns `Mcp-Session-Id`.
- `GET /mcp` — SSE stream of downstream notifications
  (`notifications/message` with `logger: "downstream:<server>"`).
- `DELETE /mcp` — close session.
- `GET /health` — `{"status":"ok"}`, no auth.

A `POST /mcp` without `Mcp-Session-Id` creates a new session and returns
its id in the `Mcp-Session-Id` response header. In practice the first
call is always `initialize` (per MCP spec), but the proxy does not
enforce that ordering. Subsequent requests must echo the header.
Sessions idle longer than `session_ttl` are reaped.

## Observability

- **Logs**: JSONL to stderr and, optionally, `proxy.log_file`
  (rotated by lumberjack).
- **Metrics**: Prometheus textfile for node_exporter's textfile
  collector at `proxy.metrics_file`, rewritten every
  `metrics_interval` (15s / 30s / 60s).
  Series: `lazy_mcp_tool_calls_total`,
  `lazy_mcp_tool_call_duration_seconds` (histogram),
  `lazy_mcp_circuit_state`, `lazy_mcp_backend_starts_total`,
  `lazy_mcp_catalog_tools`, `lazy_mcp_sessions_active`,
  `lazy_mcp_sessions_created_total`,
  `lazy_mcp_notifications_sent_total`,
  `lazy_mcp_auth_failures_total`, `lazy_mcp_info{version}`.
- **Startup log** includes `version` (git hash) so the running build
  is identifiable from `journalctl`.

## Operations

**SIGHUP** — reload config from disk. Same path as `meta_reload`.

**SIGTERM / SIGINT** — graceful shutdown: drain in-flight with
`shutdown_timeout`, then stop all backends. Second signal forces exit.

**systemd unit:**

```ini
[Service]
Type=simple
ExecStart=/usr/local/bin/lazy-mcp --config /etc/lazy-mcp/config.yaml
ExecReload=/bin/kill -HUP $MAINPID
EnvironmentFile=-/etc/lazy-mcp/env
Restart=on-failure
RestartSec=5s
KillSignal=SIGTERM
TimeoutStopSec=20
```

## Development

```sh
make build        # production binary, -trimpath -ldflags="-s -w -X ...Version=<hash>"
make test         # go test ./...
make test-race    # race detector (CI)
make lint         # golangci-lint v2
make cover        # coverage profile + total %
make cover-html   # HTML report
make cover-check  # fails if below COVER_MIN (default 50)
make bench        # benchmarks
make check        # build + test
make ci           # lint + test-race
```

Broker integration tests compile a small mock MCP server from
`internal/broker/testdata/mockmcp`; no network required.
Env vars drive behaviour: `MOCK_TOOLS`, `MOCK_CALL_DELAY`,
`MOCK_CALL_ERROR`, `MOCK_INIT_DELAY`, `MOCK_DIE_ON_INIT`,
`MOCK_DIE_AFTER`, `MOCK_NOTIFY_MESSAGE`, `MOCK_RPC_ERROR_CODE`.

## Project layout

```
cmd/lazy-mcp/        entrypoint, transport selection, signal handling
internal/config/     YAML loader, include, ${...} expansion, Holder
internal/catalog/    tool index, scoring, per-user Search filter
internal/broker/     subprocess lifecycle, circuit breaker, idle reaper
  testdata/mockmcp/  mock MCP server for tests
internal/policy/     allow/deny, deny_dangerous, rate limit
internal/session/    per-connection identity in context
internal/hub/        HTTP MCP transport (POST/GET/DELETE, SSE, auth)
internal/meta/       meta-tool handlers, pagination, reload
internal/mcp/        MCP frontend core (used by stdio and hub)
internal/observability/ Prometheus textfile metrics
internal/logx/       structured JSONL logger
```

## Status

Implemented:

- Lazy backend startup, warmup, idle reaping.
- Circuit breaker with half-open probe; JSON-RPC client errors do not
  trip the breaker.
- Word-based search with server-metadata warnings; `total`/`has_more`.
- Byte-level pagination with idempotent cursors.
- Config: `include`, `${...}` substitutions, SIGHUP and `meta_reload`
  hot-reload.
- HTTP MCP transport (POST + SSE) with bearer auth, roles and
  per-session visibility and rate limit.
- Downstream notifications → `notifications/message` over SSE.
- Tool annotations (`destructiveHint`, `readOnlyHint`, …) propagated;
  `deny_destructive` enforcement.
- Prometheus metrics (tools, sessions, notifications, auth failures).
- Graceful shutdown.

Not implemented:

- `resources/*`, `prompts/*`, `sampling`, `elicitation` (proxy is
  tools-only).
- OAuth 2.1 / dynamic client registration (bearer-only today).
- Runtime add/remove of downstream servers (config-file only, by
  design).
- Semantic search (keyword today).
- OpenTelemetry tracing / correlation IDs.

## License

MIT
