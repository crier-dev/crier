# Crier — Agent-to-Agent Message Bus

[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](https://go.dev)

Communication backbone for the autonomous agent economy. Extracted and generalized from Hivemind.

Crier provides four primitives for agent communication:

## Architecture

### 1. Relay (Pub/Sub)

Central message relay. Agents publish events to topics; subscribers receive them over WebSocket.

- HTTP + WebSocket transport
- Topic-based routing with subscriber counts
- Rate limiting per agent (100 events/minute)
- Bearer token authentication

### 2. WebSocket Mesh (Peer-to-Peer)

Direct agent-to-agent communication layer with discovery, keepalive, and request/response correlation.

- Peer discovery via registry
- Registration handshake (REGISTER/REGISTER_ACK)
- 30-second keepalive loop
- Concurrent request/response with timeout tracking
- Clean shutdown with WebSocket close frames

### 3. Agent Registry

Every agent has a discoverable identity with capability cards.

- Register/unregister with ed25519 public key
- List and detail endpoints with health status
- Capability-based routing (future)

### 4. Inboxes

Durable per-agent FIFO queues with lease-based delivery. Durability is backend-dependent: with `CR_DATABASE_URL` set (PostgreSQL backend) agents and undelivered messages survive server restarts; without it the in-memory backend is used (process-lifetime only).

- Lease prevents double-delivery: messages are leased for N seconds on retrieval
- ACK confirms delivery; un-ACKed messages return to queue after lease expiry
- TTL expiry auto-purges stale messages (default 24h)
- Concurrent retrievers get disjoint message sets

## Quick Start

### Prerequisites

- Go 1.26 or later

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

# 1. Register an agent (public_key = hex-encoded ed25519 public key)
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"id":"agent-1","public_key":"<hex ed25519 pubkey>","capabilities":["demo"]}'
# 201

# 2. Deliver a message to its inbox
curl -s -X POST localhost:8767/agents/agent-1/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world"}}'
# 201 {"id":"..."}

# 3. Retrieve — inbox endpoints require per-agent request signatures by default
#    (CR_REQUIRE_AGENT_SIG=true). Headers:
#      X-Agent-ID  agent id
#      X-Agent-Ts  unix seconds
#      X-Agent-Sig hex ed25519 signature over "METHOD\nPATH\nTS"
#    e.g. sign "GET\n/agents/agent-1/inbox\n1712345678" with the agent's private key
curl -s localhost:8767/agents/agent-1/inbox "${AUTH[@]}" \
  -H 'X-Agent-ID: agent-1' -H 'X-Agent-Ts: 1712345678' -H 'X-Agent-Sig: <hex sig>'
# 200 {"messages":[{"id":"...","payload":"eyJoZWxsbyI6IndvcmxkIn0=","lease_id":"..."}],"lease_id":"..."}
# Note: message payloads are base64-encoded on the wire ([],byte form)

# Dev shortcut: disable signing for trusted single-user setups
CR_REQUIRE_AGENT_SIG=false make run
curl -s localhost:8767/agents/agent-1/inbox
# 200 — no signature headers required
```

> Prefer the runnable script: [`examples/demo.sh`](examples/demo.sh) performs the
> full register → deliver → signed retrieve → ack round-trip with an ephemeral
> ed25519 keypair (openssl). Start the server, then run `./examples/demo.sh`.

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
| `CR_REQUIRE_AGENT_SIG` | `true` | Enforce per-agent ed25519 request signing on inbox endpoints (retrieve/ack/delete). Set `false` only for trusted single-user dev setups. |

## API

The full API is documented in [`docs/openapi.yaml`](docs/openapi.yaml) — an OpenAPI 3.1 spec covering 14 endpoints across 5 operation groups:

| Group | Endpoints | Description |
|-------|-----------|-------------|
| **Health** | `GET /health` | Service health check |
| **Relay** | `POST /relay/publish`, `GET /relay/subscribe/{topic}`, `GET /relay/topics` | Pub/sub |
| **Mesh** | `GET /mesh/connect/{agentID}`, `GET /mesh/peers` | P2P connections |
| **Registry** | `POST /agents`, `GET /agents`, `GET /agents/{id}`, `DELETE /agents/{id}` | Agent identity |
| **Inbox** | `POST /agents/{id}/inbox`, `GET /agents/{id}/inbox`, `POST /agents/{id}/inbox/ack`, `GET /agents/{id}/inbox/stats` | Message delivery |

## Documentation

| File | Description |
|------|-------------|
| [`docs/architecture.md`](docs/architecture.md) | Architecture overview and design decisions |
| [`docs/specs.md`](docs/specs.md) | Component specifications and acceptance criteria |
| [`docs/openapi.yaml`](docs/openapi.yaml) | OpenAPI 3.1 API specification |
| [`examples/demo.sh`](examples/demo.sh) | Runnable end-to-end demo (register → deliver → signed retrieve → ack) |

## Project Status

All core primitives are implemented and tested:

- **Relay** — Thread-safe in-memory pub/sub, 87.4% coverage, 7/7 GitReins PASS
- **Mesh** — P2P WebSocket connections ported from Hivemind, 8/8 GitReins PASS
- **Registry + Inboxes** — Net-new, 84.8% coverage, 8/8 GitReins PASS
- **Persistence** — PostgreSQL backend for registry + inboxes via `CR_DATABASE_URL`; verified live that agents and undelivered messages survive a server restart
- **API** — 14 HTTP endpoints wired with middleware, graceful shutdown
- **CI** — GitHub Actions, matrix build Go 1.26

### Roadmap

- **CI-003b** ✅ — PostgreSQL persistence for registry and inboxes (implemented, `CR_DATABASE_URL`)
- **CI-007** ✅ — MCP server exposing registry and inbox tools (implemented, `cmd/crier-mcp`)
- **Capability-based routing** — route messages by agent capability cards

## License

MIT — see [LICENSE](LICENSE).
