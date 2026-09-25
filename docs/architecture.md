# Crier — Agent-to-Agent Message Bus

Communication backbone for the autonomous agent economy. Extracted from Hivemind.

## Architecture

Crier provides four primitives for agent communication:

### 1. Relay Server (pub/sub)
Central message relay. Agents publish events to topics; subscribers receive them.
- HTTP + WebSocket transport
- Topic-based routing with subscriber-side wildcards: `*` matches exactly one
  dot-separated segment (`demo.*` matches `demo.one`, not `demo.one.two`), and
  `>` matches one or more trailing segments in final position (`demo.>` matches
  `demo.one` and `demo.one.two`, not `demo`). Published topic names stay
  literal — a publish to `demo.*` is rejected with 400. Every matching
  subscription receives each event exactly once.
- Rate limiting per agent

### 2. WebSocket Mesh (peer-to-peer)
Direct agent-to-agent communication layer.
- Peer discovery from the live WebSocket connection table (`GET /mesh/peers`); the
  registry is not consulted for mesh peers
- Token auth
- Keepalive (client-driven, 30s interval; a heartbeat advances the sender's registry liveness — §3)
- Request/response correlation
- Wire format: `docs/mesh-protocol.md` is the authoritative reference. REQUEST and
  RESPONSE frames carry `PeerRef` `source`/`target`, a RESPONSE carries its
  `status_code`, and a RESPONSE's `request_id` MUST echo the originating REQUEST's
  `message_id` — a non-matching `request_id` is dropped and the requester stalls
  until its own timeout. KEEPALIVE frames ride the same socket as the conversation
  (the client's 30s ticker fires between its REQUEST and its RESPONSE), so a reader
  MUST loop-recv and dispatch each frame on its `type`, buffering frames it is not
  ready for; a naive blocking read stalls or misparses mid-exchange.

### 3. Agent Registry
Every agent has a discoverable identity.
- Agent registration with capabilities
- Public-key identity (ed25519) + per-agent request signing
- Presence derived from liveness evidence (CR-FEAT-024): a mesh socket accepted
  for the agent, a KEEPALIVE heartbeat on it, or a signed PATCH advances
  `last_seen`, and the reported `status` — `online` inside the documented
  staleness window (`CR_PRESENCE_STALE_AFTER_S`, default 90s), `stale` outside it
  — is derived per read, never stored
- Capability-based routing

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
- **Auth:** Bearer shared token (`CR_AUTH_TOKEN`) on the bus, with `/health`,
  `/version`, `/openapi.json`, `/openapi.yaml` and `/docs` exempt; when
  `CR_REQUIRE_AGENT_SIG` is set, per-agent requests are additionally ed25519-signed
  (`X-Agent-ID` + `X-Agent-Ts` + `X-Agent-Sig` over method/path/timestamp). The
  HMAC-SHA256 signature (`CR_WEBHOOK_SECRET` → `X-Crier-Signature`) covers OUTBOUND
  webhook deliveries — it is not agent identity

## ZeroMQ Pattern Mapping
Crier speaks the patterns ZeroMQ made standard — prebuilt, so agents never assemble sockets:

| ZeroMQ pattern | Crier realization |
|---|---|
| PUB/SUB | Relay: HTTP publish (202) → topic frames over WebSocket |
| REQ/REP | Mesh REQUEST/RESPONSE between registered peers |
| PUSH/PULL (pipeline) | Durable lease-based inboxes — **superset**: queues survive peer death and bus restarts when a durable store is configured (`CR_DATABASE_URL`); the in-memory default dies with the process |
| PAIR (exclusive 1:1) | Direct mesh connection between two peers |
| ROUTER/DEALER | Prebuilt: crier **is** the broker ZMQ makes you assemble from those sockets |
| XPUB/XSUB | Prebuilt: the relay is the subscription-forwarding proxy |
| STREAM | **Deliberately excluded** — raw passthrough would bypass the inbox message guard and the signed registry (choke-point doctrine: messages addressed to an agent are guarded on delivery — the guard is wired to the inbox path in `cmd/server/main.go`, not to the relay; relay publishes stay opaque caller JSON) |

Beyond ZMQ: durable queues, an ed25519 identity registry, the LLM guard on inbox
delivery, the MCP tool surface (agents send via tools, never raw sockets), and
federation across buses (`/fed/peers`). ZMQ retains the edge on latency
(microseconds, in-process IPC); crier is deliberately HTTP/WS-network-class for
the agent economy.

## Key Design Decisions
- The OpenAPI 3.1 spec (`docs/openapi.yaml`) is the contract of record for the HTTP
  surface and is served live (`/openapi.json`, `/openapi.yaml`, `/docs`); the MCP tool
  surface mirrors it and is hand-registered in `internal/mcp/server.go`
- Events are opaque caller JSON, passed through verbatim — the bus assigns no id or
  content hash (`internal/relay/handler.go`); mesh frames carry a crypto/rand
  `message_id` for correlation
- Agent identity is the client-supplied agent ID plus the ed25519 public key stored
  alongside it (`POST /agents`); nothing derives or verifies a key fingerprint
- Inboxes are append-only, truncated by retention policy

## Origin
Extracted from Hivemind `pkg/collab/relay.go` and `pkg/federation/peer.go`.
