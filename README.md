# Crier — Agent-to-Agent Message Bus

[![Go Version](https://img.shields.io/badge/Go-1.26.6%2B-00ADD8?logo=go)](https://go.dev)

Communication backbone for the autonomous agent economy. Extracted and generalized from Hivemind.

Crier provides four primitives for agent communication:

## Architecture

### 1. Relay (Pub/Sub)

Central message relay. Agents publish events to topics; subscribers receive them over WebSocket.

- HTTP + WebSocket transport
- Topic-based routing with subscriber counts
- Publishing is fire-and-forget: `POST /relay/publish` returns 202 as soon as the event is accepted, and if the topic has **zero subscribers** the event is dropped by design — publishes to empty topics are not queued or retained. A topic only appears in `GET /relay/topics` after at least one subscriber connects (subscribe first, then publish; a publish to an empty topic does not create it)
- Rate limiting per agent (100 events/minute) — requires the `X-Agent-ID` header when enabled; `0` disables both
- Bearer token authentication

### 2. WebSocket Mesh (Peer-to-Peer)

Direct agent-to-agent communication layer with discovery, keepalive, and request/response correlation.

- Peer discovery via registry
- One-way REGISTER on connect (fire-and-forget; the server never sends REGISTER_ACK — see `docs/mesh-protocol.md`)
- 30-second keepalive loop
- Concurrent request/response with timeout tracking
- Clean shutdown with WebSocket close frames

### 3. Agent Registry

Every agent has a discoverable identity with capability cards.

- Register/unregister with ed25519 public key
- List and detail endpoints with registration status (see note below)
- Capability-based routing (future)

> **What `status` means — registration-liveness only.** An agent's `status` is
> set to `"online"` at registration and never changes until unregistration; it
> is NOT live-connection health. The registry has no heartbeat source today
> (mesh KEEPALIVE frames are sent but not processed server-side), so an agent
> whose process crashed still reports `"online"` with a frozen `last_seen`.
> For live-connection truth, use the mesh: `GET /mesh/peers` lists agents with
> an active WebSocket connection (see [Try the Mesh](#try-the-mesh)).

### 4. Inboxes

Durable per-agent FIFO queues with lease-based delivery. Durability is backend-dependent: with `CR_DATABASE_URL` set (PostgreSQL backend) agents and undelivered messages survive server restarts; without it the in-memory backend is used (process-lifetime only).

- Lease prevents double-delivery: messages are leased for N seconds on retrieval
- ACK confirms delivery; un-ACKed messages return to queue after lease expiry
- TTL expiry auto-purges stale messages (default 24h)
- Concurrent retrievers get disjoint message sets

## Quick Start

### Prerequisites

- Go 1.26.6 or later

### Build

```bash
make build
```

Or build directly:

```bash
go build -o bin/crier ./cmd/server
```

### Run

```bash
# Default port :8767
make run
```

### Try it

A minimal register → deliver → retrieve round-trip with the default signed configuration. If you started the server with `CR_AUTH_TOKEN` set (auth enabled), every request except `/health` needs the Bearer header shown below; if `CR_AUTH_TOKEN` is unset, auth is disabled and the header can be dropped:

```bash
AUTH=(-H "Authorization: Bearer ${CR_AUTH_TOKEN:-}")

# 0. One-time setup: generate an ed25519 keypair for agent-1 (needs openssl + xxd)
openssl genpkey -algorithm ED25519 -out /tmp/crier-agent.key >/dev/null 2>&1
PUBKEY_HEX=$(openssl pkey -in /tmp/crier-agent.key -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)
# sig helper: hex(ed25519_sign("METHOD\nPATH\nTS", key)) — same wire format as examples/demo.sh
sig() { printf '%s\n%s\n%s' "$1" "$2" "$3" > /tmp/crier-payload.txt; openssl pkeyutl -sign -rawin -inkey /tmp/crier-agent.key -in /tmp/crier-payload.txt 2>/dev/null | xxd -p -c 128; }

# 1. Register an agent (public_key = hex-encoded ed25519 public key)
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"id\":\"agent-1\",\"public_key\":\"${PUBKEY_HEX}\",\"capabilities\":[\"demo\"]}"
# 201

# 2. Deliver a message to its inbox
curl -s -X POST localhost:8767/agents/agent-1/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world"}}'
# 201 {"id":"..."}

# 3. Retrieve — agent-scoped endpoints require per-agent request signatures by
#    default (CR_REQUIRE_AGENT_SIG=true). This covers inbox retrieve/ack/stats
#    AND DELETE /agents/{id}. Headers:
#      X-Agent-ID  agent id
#      X-Agent-Ts  unix seconds (must be within ±30s of the server clock)
#      X-Agent-Sig hex ed25519 signature over "METHOD\nPATH\nTS"
#    Timestamps are generated fresh below — never hardcode them (stale timestamps
#    are rejected with 401).
TS=$(date +%s)
curl -s localhost:8767/agents/agent-1/inbox "${AUTH[@]}" \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig GET /agents/agent-1/inbox "$TS")"
# 200 {"messages":[{"id":"...","payload":"eyJoZWxsbyI6IndvcmxkIn0=","lease_id":"..."}],"lease_id":"..."}
# Note: message payloads are base64-encoded on the wire ([],byte form)

# 4. Ack the message — message_ids is REQUIRED (an ack without it is rejected
#    with 400: it would otherwise be a silent no-op and the message would be
#    redelivered after lease expiry). Sign "POST\n/agents/agent-1/inbox/ack\n<ts>".
TS=$(date +%s)
curl -s -X POST localhost:8767/agents/agent-1/inbox/ack "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig POST /agents/agent-1/inbox/ack "$TS")" \
  -d '{"lease_id":"<lease_id from retrieve>","message_ids":["<id from retrieve>"]}'
# 204 — message permanently removed (never redelivered after lease expiry)

# Dev shortcut: disable signing for trusted single-user setups
CR_REQUIRE_AGENT_SIG=false make run
curl -s localhost:8767/agents/agent-1/inbox
# 200 — no signature headers required

# 5. Delete the agent — DELETE /agents/{id} requires the same per-agent
#    signature (not just inbox endpoints). Sign "DELETE\n/agents/agent-1\n<ts>".
TS=$(date +%s)
curl -s -X DELETE localhost:8767/agents/agent-1 "${AUTH[@]}" \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig DELETE /agents/agent-1 "$TS")"
# 204 — agent removed (401 without the signature headers)
```

> Prefer the runnable script: [`examples/demo.sh`](examples/demo.sh) performs the
> full register → deliver → signed retrieve → ack round-trip with an ephemeral
> ed25519 keypair (openssl). Start the server, then run `./examples/demo.sh`.

### Try the Mesh

The mesh is the second primitive: direct agent-to-agent WebSocket connections.
Unlike the registry/inboxes it needs no signing setup — just a WebSocket client
([websocat](https://github.com/vi/websocat), or `npx wscat -c <url>`):

```bash
# Terminal A — connect as agent-1, then send one REGISTER frame (fire-and-forget:
# any RFC3339 timestamp works, the server never replies)
websocat ws://localhost:8767/mesh/connect/agent-1
{"type":"REGISTER","version":1,"message_id":"0123456789abcdef01234567","timestamp":"2026-08-10T18:00:00.123456789-05:00","agent_id":"agent-1","lease_id":"","lease_ttl_ms":3600000,"capabilities":{"version":"0.1.0","topics":[],"max_concurrent_sessions":10}}

# Terminal B — the peer is now visible:
curl -s localhost:8767/mesh/peers
# {"peers":[{"agent_id":"agent-1"}],"count":1}
```

A peer shows up as soon as the socket connects and stays listed while the
connection is open (keepalive frames are exchanged every 30s); close Terminal A
and it disappears. If you started the server with `CR_AUTH_TOKEN` set, add the
Bearer header to the `curl` as in the section above. For the full wire protocol —
REQUEST/RESPONSE correlation, error frames, a verified two-agent round-trip —
see [`docs/mesh-protocol.md`](docs/mesh-protocol.md).

### Test

```bash
# Full test suite
make test

# Short (no integration tests)
make test-short

# Vet
make lint
```

## Configuration

All configuration is via environment variables (defaults shown):

| Variable | Default | Description |
|----------|---------|-------------|
| `CRIER_PORT` | `8767` | Server listen port |
| `CR_DATABASE_URL` | _(unset — in-memory backend)_ | PostgreSQL connection (optional). When set, the registry and inboxes use the durable PostgreSQL backend (migrations applied automatically on start). Precedence: `CR_DATABASE_URL` → `DATABASE_URL` → `CRIER_DATABASE_URL`. Example: `postgres://crier:crier@localhost:5432/crier?sslmode=disable` |
| `CR_AUTH_TOKEN` | _(unset — auth disabled)_ | Bearer token for API authentication. When set, all requests except `/health` require `Authorization: Bearer <token>`; unset = no auth (local dev). |
| `CR_REQUIRE_AGENT_SIG` | `true` | Enforce per-agent ed25519 request signing on agent-scoped endpoints (inbox retrieve/ack/stats, DELETE /agents/{id}, and PATCH /agents/{id}). Set `false` only for trusted single-user dev setups. |
| `CR_LOG_LEVEL` | `info` | Log level. One of `debug`, `info`, `warn`, `error`. |
| `CR_LOG_FORMAT` | `text` | Log format. One of `text`, `json`. |
| `CR_RATE_LIMIT_PER_MINUTE` | `100` | Per-agent publish rate limit (events/minute), keyed on the `X-Agent-ID` header. `0` disables rate limiting and the identity requirement. |
| `CR_WS_ALLOWED_ORIGINS` | _(unset — all origins allowed)_ | Comma-separated list of allowed WebSocket `Origin` headers (`scheme://host:port`). `*` allows all origins. |
| `CR_FED_LINKS` | _(unset — federation disabled)_ | Comma-separated base URLs of linked relays (relay-to-relay federation, CR-FEAT-006). When set, deliveries to agents unknown on this relay are forwarded to each linked relay in order (first non-404 answer wins and is relayed back verbatim — a blocking webhook reply returns to the original sender), and `GET /fed/peers` lists the linked relays with their agents. Example: `http://localhost:18772`. |
| `CR_FED_NAME` | _(unset — `localhost:<port>`)_ | Optional display name for this relay in the `GET /fed/peers` listing. |
| `CR_DATABASE_MAX_CONNS` | `4` | Maximum PostgreSQL pool connections. |
| `CR_DATABASE_MIN_CONNS` | `0` | Minimum PostgreSQL pool connections kept open (must be ≤ `CR_DATABASE_MAX_CONNS`). |
| `CR_DATABASE_MAX_CONN_LIFETIME` | `30m` | Maximum lifetime of a pooled connection (Go duration, e.g. `30m`, `1h`). |
| `CR_DATABASE_MAX_CONN_IDLE_TIME` | `5m` | Maximum idle time of a pooled connection (Go duration). |
| `CR_DATABASE_CONNECT_TIMEOUT` | `10s` | PostgreSQL connect timeout (Go duration). |

## API

The full API is documented in [`docs/openapi.yaml`](docs/openapi.yaml) — an OpenAPI 3.1 spec covering 15 endpoints across 5 operation groups:

| Group | Endpoints | Description |
|-------|-----------|-------------|
| **Health** | `GET /health` | Service health check |
| **Relay** | `POST /relay/publish`, `GET /relay/subscribe/{topic}`, `GET /relay/topics` | Pub/sub |
| **Mesh** | `GET /mesh/connect/{agentID}`, `GET /mesh/peers` | P2P connections |
| **Registry** | `POST /agents`, `GET /agents` (capability filter), `GET /agents/{id}`, `PATCH /agents/{id}`, `DELETE /agents/{id}` | Agent identity + self-configuration |
| **Inbox** | `POST /agents/{id}/inbox`, `GET /agents/{id}/inbox`, `POST /agents/{id}/inbox/ack`, `GET /agents/{id}/inbox/stats` | Message delivery |

## Documentation

| File | Description |
|------|-------------|
| [`docs/architecture.md`](docs/architecture.md) | Architecture overview and design decisions |
| [`docs/specs.md`](docs/specs.md) | Component specifications and acceptance criteria |
| [`docs/mesh-protocol.md`](docs/mesh-protocol.md) | Mesh wire protocol — framing, message types, correlation contract, worked example |
| [`docs/openapi.yaml`](docs/openapi.yaml) | OpenAPI 3.1 API specification |
| [`docs/integration-guide.md`](docs/integration-guide.md) | End-to-end integration guide — auth modes, signing, inbox lifecycle, mesh, Postgres |
| [`examples/demo.sh`](examples/demo.sh) | Runnable end-to-end demo (register → deliver → signed retrieve → ack) |

## Project Status

All core primitives are implemented and tested:

- **Relay** — Thread-safe in-memory pub/sub, 87.5% coverage, 7/7 GitReins PASS
- **Mesh** — P2P WebSocket connections ported from Hivemind, 8/8 GitReins PASS
- **Registry + Inboxes** — Net-new, 78.3% coverage, 8/8 GitReins PASS
- **Persistence** — PostgreSQL backend for registry + inboxes via `CR_DATABASE_URL`; verified live that agents and undelivered messages survive a server restart
- **API** — 15 HTTP endpoints wired with middleware, graceful shutdown
- **CI** — GitHub Actions, matrix build Go 1.26.6

Coverage numbers above are measured fresh per change (`go test -short -count=1 -cover ./internal/<pkg>`); the ≥70% gate lives in `make coverage-check`.

### Roadmap

- **CI-003b** ✅ — PostgreSQL persistence for registry and inboxes (implemented, `CR_DATABASE_URL`)
- **CI-007** ✅ — MCP server exposing registry and inbox tools (implemented, `cmd/crier-mcp`)
- **Capability-based routing** — route messages by agent capability cards

## License

MIT — see [LICENSE](LICENSE).
