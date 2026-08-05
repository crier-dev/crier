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
| `CR_AUTH_TOKEN` | (required) | Bearer token for relay publish authentication |

## API

The full API is documented in [`docs/openapi.yaml`](docs/openapi.yaml) — an OpenAPI 3.1 spec covering 15 endpoints across 5 operation groups:

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

## Project Status

All core primitives are implemented and tested:

- **Relay** — Thread-safe in-memory pub/sub, 87.4% coverage, 7/7 GitReins PASS
- **Mesh** — P2P WebSocket connections ported from Hivemind, 8/8 GitReins PASS
- **Registry + Inboxes** — Net-new, 84.8% coverage, 8/8 GitReins PASS
- **Persistence** — PostgreSQL backend for registry + inboxes via `CR_DATABASE_URL`; verified live that agents and undelivered messages survive a server restart
- **API** — 17 HTTP endpoints wired with middleware, graceful shutdown
- **CI** — GitHub Actions, matrix build Go 1.25 + 1.26

### Roadmap

- **CI-003b** ✅ — PostgreSQL persistence for registry and inboxes (implemented, `CR_DATABASE_URL`)
- **CI-007** ✅ — MCP server exposing registry and inbox tools (implemented, `cmd/crier-mcp`)
- **Capability-based routing** — route messages by agent capability cards

## License

MIT — see [LICENSE](LICENSE).
