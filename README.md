![release](https://github.com/subconscious-labs/lazy-mcp/actions/workflows/release.yml/badge.svg)

# lazy-mcp

MCP proxy with lazy-load, per-user policy, and optional HTTP daemon.
The model sees **six** meta-tools; real MCP backends (stdio or HTTP) are started on demand, indexed into a compact catalog, and never reach the context window directly.

## How it works

```
             Model / Client
               ↕  stdio or HTTP/SSE
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
               ↕  stdio or HTTP
       ┌───────┴──────────────────────────┐
       │  dav  git-mcp  mempalace  ...    │
       └──────────────────────────────────┘
```

Key properties:
- **Lazy start.** A backend is started on first use (unless `autostart: true`).
- **Compact catalog.** `meta_search` returns short refs, not full schemas.
- **Circuit breaker.** Misbehaving backends are short-circuited, then probed.
- **Idle reaping.** Backends with no `tools/call` for `idle_ttl` are stopped.
- **Bounded output.** Large results are paginated safely (byte-level, UTF-8 safe).
- **Notifications.** Downstream `notifications/message` are relayed to HTTP clients via SSE.
- **Per-session policy.** `role: agent` sees only `allowed_servers`; rate limit is per `(user, server, tool)`.

## Quick start

### 1. stdio (MCP client subprocess)
```sh
make build
./bin/lazy-mcp --config config.yaml
```
Any MCP client that spawns subprocesses can use it. The client sees six meta-tools.

### 2. HTTP Daemon
```yaml
# config.yaml
proxy:
  transport: "http"
  http:
    addr: ":3333"
auth:
  enabled: true
  users:
    - name: admin
      token_sha256: "<64 hex chars>"
      role: admin
```

```sh
./bin/lazy-mcp --config /etc/lazy-mcp/config.yaml
curl -s localhost:3333/health
curl -s -H "Authorization: Bearer <token>" -X POST localhost:3333/mcp \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize"}' -D -
```

To generate a token hash:
`printf %s "$TOKEN" | sha256sum | awk '{print $1}'`

## Configuration

See [`config.example.yaml`](config.example.yaml).

### `proxy`
| Key | Default | Meaning |
|---|---|---|
| `transport` | auto | `stdio`, `http`, or `both`. Empty picks `http` if `http.addr` is set. |
| `tool_prefix` | `meta` | Prefix for meta-tool names. |
| `max_search_results` | `8` | Cap on `meta_search` hits. |
| `warmup` | `false` | Start all enabled backends at boot. |
| `idle_ttl` | `10m` | Stop backends idle this long. `-1` = never. `0` = default to `10m` |
| `tool_cache_ttl` | `30m` | How long `tools/list` stays cached. |
| `exec_timeout` | `60s` | Per `tools/call` timeout. |
| `initialize_timeout` | `10s` | MCP handshake timeout when starting a backend. |
| `max_result_bytes` | `262144` | Hard cap; larger results paginate. `-1` = unlimited. |
| `max_arguments_bytes` | `262144` | Cap on serialized `arguments`. `-1` = unlimited. |
| `max_summary_len` | `200` | Cap on summary returned by `meta_search`. Full description is in `meta_describe_tool`. `-1` disables. |
| `max_raw_result_bytes` | `10485760` | Reject raw results larger than this before buffering. `-1` = unlimited. |
| `default_page_size` | `0` | Pagination size when `page_size` isn't passed. `0` = auto only. |
| `cursor_ttl` | `5m` | How long paged cursors survive. |
| `shutdown_timeout` | `5s` | Grace period for in-flight work on SIGTERM. |
| `deny_dangerous` | `false` | Reject tools from `server.dangerous_tools`. |
| `deny_destructive` | `false` | Reject tools with `annotations.destructiveHint: true`. |

### `auth`

HTTP transport only (stdio ignores this).

```yaml
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
```

| Field | Meaning |
|---|---|
| `enabled` | Enforce Bearer auth on every HTTP request. |
| `users[].name` | Label; appears in logs and sessions. |
| `users[].token_sha256` | SHA-256 of the client's bearer token, hex. |
| `users[].role` | `admin` (sees every enabled server) or `agent` (sees only `allowed_servers`). |
| `users[].allowed_servers` | Required for `role: agent`; empty list is a load error. Ignored for `admin`. |

Generate a hash with:
```sh
printf %s "$TOKEN" | sha256sum | awk '{print $1}'
```

### `servers[]`
| Key | Meaning |
|---|---|
| `name` | Unique; used in `server/tool` refs. |
| `transport` | `stdio` (default) or `http`. |
| `command`, `args`, `env` | For `stdio`: how to spawn the backend. |
| `url`, `headers` | For `http`: downstream MCP endpoint and auth headers. |
| `autostart` | Start at boot (independent of `warmup`). |
| `enabled` | If `false`, not started, not indexed, rejected on call. |
| `allowed_tools` | If non-empty, only these tools are visible. |
| `denied_tools` | Always hidden and rejected. |
| `dangerous_tools` | Flagged in `describe_tool`; rejected when `deny_dangerous`. |

### `include`
`include: ["conf.d/*.yaml"]` — merges servers by name (last wins). `proxy` and `auth` must be in the top-level file.

### Substitutions
`${VAR}`, `${VAR:-default}`, `${VAR:?msg}`, `${file:/path}`, `$$` for literal `$`. Evaluated after YAML parsing. Cyclic references are rejected at load time.

## Meta-tools
- `meta_search` — `{hits, total, has_more, warnings?}`.
- `meta_call` — supports pagination (`has_more`, `next_cursor`).
- `meta_describe_tool` — full schema, `dangerous`, `annotations`.
- `meta_list_servers` — registry + runtime status.
- `meta_whoami` — identity, role, visible servers.
- `meta_reload` — re-read config (admin only, 5s cooldown).

### Pagination
Pages are **byte-level slices** of the full result. Concatenate them verbatim; individual pages are not necessarily valid JSON. `result` is embedded as an object/array when the backend returned JSON.

### Notifications (HTTP)
- `GET /mcp` (Accept: text/event-stream) — server pushes `notifications/message`.
- `data` envelope contains `{source: <server>, payload: <raw>}`.
- Requires `capabilities.logging` (advertised by proxy).
- Session must be authenticated.

## Operations
- **SIGHUP** — reload config from disk. Transactional: new config is published only after the broker reconciles.
- **SIGTERM/SIGINT** — graceful shutdown. Kills process groups (`Setpgid`).
- **Systemd:** runs as `Type=simple`, uses `ExecReload=/bin/kill -HUP $MAINPID`.

## Development
```sh
make build        # -trimpath -ldflags="-s -w -X ...Version=<hash>"
make test-race    # go test -race
make lint         # golangci-lint v2
make cover        # coverage profile
make bench        # benchmarks
```
Broker tests use a mock MCP server (`internal/broker/testdata/mockmcp`) supporting pagination, annotations, notifications, and error injection.

## Status

Implemented:
- HTTP + stdio transports; HTTP is at parity with stdio on pagination, `structuredContent`, non-text content markers and JSON-RPC client-error classification.
- Bearer auth, per-user policy (`allowed_servers`, `allowed_tools`, `denied_tools`), `deny_dangerous` / `deny_destructive` gates.
- Config include (`conf.d/*.yaml`) and `${...}` substitutions.
- Hot-reload via SIGHUP or `meta_reload`; transactional (config is published only after the broker pool reconciles) and replays session ACLs on open SSE streams.
- SSE notifications (`notifications/message`) with ACL filtering; multi-line SSE events parsed correctly.
- Byte-level pagination with UTF-8-safe cuts, idempotent cursors, per-owner cursors.
- Envelope validation: `jsonrpc` must be `"2.0"`; `id` must be string or number; id-less messages are honoured only for `notifications/*`.
- `MCP-Protocol-Version` validated on incoming HTTP requests and forwarded to downstream backends after `initialize`.
- Per-generation process isolation: a timed-out request cannot leak its write into a restarted backend; `Stop` kills the whole process group.
- Metrics (tools, sessions, notifications, auth failures), structured JSONL logs, graceful shutdown.
- JSON-RPC compliance: `ping`, `logging/setLevel`, exact integer IDs, parse error responses.

Not implemented:
- OAuth 2.1 / dynamic client registration (Bearer only).
- Semantic search (keyword today).
- `resources/*`, `prompts/*`.

## License

MIT
