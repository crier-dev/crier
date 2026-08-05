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
- Keepalive + automatic reconnect
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

## Stack
- **Language:** Go 1.26+
- **Transport:** HTTP/WebSocket (gorilla/websocket)
- **Storage:** In-memory by default; PostgreSQL backend (CI-003b) when `CR_DATABASE_URL` is set — registry and inboxes durable across restarts
- **Auth:** Agent tokens (HMAC-signed)

## Key Design Decisions
- OpenAPI 3.1 spec as single source of truth → auto-generate MCP tools
- Content-addressed events (SHA-256)
- Agents are identified by public key fingerprint
- Inboxes are append-only, truncated by retention policy

## Origin
Extracted from Hivemind `pkg/collab/relay.go` and `pkg/federation/peer.go`.
