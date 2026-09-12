# Crier — Agent-to-Agent Message Bus

Communication backbone for the autonomous agent economy. Extracted from Hivemind.

## Architecture

Crier provides four primitives for agent communication:

### 1. Relay Server (pub/sub)
Central message relay. Agents publish events to topics; subscribers receive them.
- HTTP + WebSocket transport
- Topic-based routing with wildcards
- Rate limiting per agent

### 2. WebSocket Mesh (peer-to-peer)
Direct agent-to-agent communication layer.
- Peer discovery via registry
- Token auth
- Keepalive (client-driven, 30s interval; no server-side liveness processing)
- Request/response correlation

### 3. Agent Registry
Every agent has a discoverable identity.
- Agent registration with capabilities
- Public-key identity (ed25519) + per-agent request signing
- Health checking + capability-based routing

### 4. Agent Inboxes
Persistent per-agent inbox for offline delivery.
- Persistent inbox per agent (offline delivery)
- Lease-based delivery with acknowledgements
- Queue statistics + message expiry

## Delivery & Escalation Lanes
Three lanes carry agent work (lane split, CR-FEAT-009):
- **Dispatch** = the scheduler + per-repo JSONL boards. This is the only authority that dispatches fleet project work.
- **Communication** = Crier itself (relay, mesh, webhook delivery, inboxes) — agent-to-agent messaging.
- **Cross-profile queue** = the Hermes kanban. Crier may write cards there as an opt-in escalation sink (guard output option, spec `specs/LLM-MESSAGE-GUARD.md` §8): fire-and-forget cards via the `hermes kanban create` CLI writer (default) or an HTTP sink when `CR_GUARD_KANBAN_URL` is set. The kanban lane NEVER dispatches fleet work — cards are visibility/cross-profile exceptions only.

## Stack
- **Language:** Go 1.26.6+
- **Transport:** HTTP/WebSocket (gorilla/websocket)
- **Storage:** In-memory by default; PostgreSQL backend (CI-003b) when `CR_DATABASE_URL` is set — registry and inboxes durable across restarts
- **Auth:** Agent tokens (HMAC-signed)

## ZeroMQ Pattern Mapping
Crier speaks the patterns ZeroMQ made standard — prebuilt, so agents never assemble sockets:

| ZeroMQ pattern | Crier realization |
|---|---|
| PUB/SUB | Relay: HTTP publish (202) → topic frames over WebSocket |
| REQ/REP | Mesh REQUEST/RESPONSE between registered peers |
| PUSH/PULL (pipeline) | Durable lease-based inboxes — **superset**: queues survive peer death and bus restarts |
| PAIR (exclusive 1:1) | Direct mesh connection between two peers |
| ROUTER/DEALER | Prebuilt: crier **is** the broker ZMQ makes you assemble from those sockets |
| XPUB/XSUB | Prebuilt: the relay is the subscription-forwarding proxy |
| STREAM | **Deliberately excluded** — raw passthrough would bypass the message guard and the signed registry (choke-point doctrine: every message is guarded and attributed) |

Beyond ZMQ: durable queues, an ed25519 identity registry, the LLM guard on every
message, the MCP tool surface (agents send via tools, never raw sockets), and
federation across buses (`/fed/peers`). ZMQ retains the edge on latency
(microseconds, in-process IPC); crier is deliberately HTTP/WS-network-class for
the agent economy.

## Key Design Decisions
- OpenAPI 3.1 spec as single source of truth → auto-generate MCP tools
- Content-addressed events (SHA-256)
- Agents are identified by public key fingerprint
- Inboxes are append-only, truncated by retention policy

## Origin
Extracted from Hivemind `pkg/collab/relay.go` and `pkg/federation/peer.go`.
