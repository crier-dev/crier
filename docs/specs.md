# Crier — Component Specs

## CI-001: Port Relay Server + Client from Hivemind

**Source:** `hivemind-work/pkg/collab/relay.go` (422 lines) + `event.go` (180 lines)

### What to port
- `RelayClient` — pub/sub with token-bucket rate limiting, channel-based subscribers, HTTP relay POST with Bearer auth, reconnect with exponential backoff
- `RelayServer` — HTTP server on `/events` (POST) and `/health` (GET), JSON event decode, Bearer auth middleware
- `RelayManager` — thin lifecycle wrapper (Start/Stop/Publish/Subscribe)
- `Event` types — simplified: keep `Type`, `Topic` (was WorkspaceID), `AgentID` (was UserID), `Timestamp`, `Payload`. Drop Hivemind-specific event types (file:*, git:*, presence:*). Add agent-native types: `agent:announce`, `agent:heartbeat`, `agent:message`, `agent:error`

### Changes from Hivemind
- `workspaceID` → `topic` throughout
- `userID` → `agentID` throughout
- Drop `RelayClient.Connect()` stub ("simulate" comment) — wire real WebSocket via gorilla/websocket
- Replace `net/http` ServeMux with gorilla/mux router
- Add graceful shutdown (already in our scaffold)
- Import path: `github.com/totalwindupflightsystems/crier/internal/relay`

### Acceptance criteria
1. `POST /relay/publish` accepts `{topic, event}` → 202 Accepted
2. `GET /relay/subscribe/{topic}` upgrades to WebSocket, streams JSON events
3. `GET /relay/topics` lists active topics with subscriber counts
4. Rate limiting: >100 events/minute from same agent → 429
5. Bearer auth: missing/invalid token → 401
6. `GET /health` returns `{"status":"ok"}`
7. Unit tests for RelayClient.Publish, Subscribe, rate limiter, event validation

---

## CI-002: Port Mesh + PeerConnection from Hivemind

**Source:** `hivemind-work/pkg/federation/peer.go` (293 lines) + `dialer.go` (168 lines) + `message.go` (146 lines)

### What to port
- `PeerConnection` — gorilla/websocket dial, read loop, Send with write deadline, close with control frame, OnMessage/OnClose callbacks
- `Mesh` — peer connect, registration handshake (REGISTER/REGISTER_ACK), keepalive loop (30s), `SendRequest` with pending tracking + timeout, `handleMessage` type dispatch (RESPONSE/ERROR)
- Federation message types: Envelope, Register, RegisterAck, Keepalive, KeepaliveAck, Request, Response, ErrorMessage, ErrorDetail, PeerRef, Capabilities
- `Marshal()` — newline-delimited JSON framing
- `newMessageID()` — crypto/rand hex

### Changes from Hivemind
- Remove `collab.PeerStore` dependency — use `internal/registry` (built in CI-003)
- `controllerID` → `agentID` throughout
- `workspaces` in Capabilities → `topics`
- Import path: `github.com/totalwindupflightsystems/crier/internal/mesh`
- Drop Deregister message type (use agent unregister from CI-003)

### Acceptance criteria
1. `PeerConnection.Connect()` dials WebSocket, starts read loop
2. `PeerConnection.Send()` writes with 10s deadline
3. `PeerConnection.Close()` sends close frame, cleans up
4. `Mesh.ConnectPeer()` dials → registers → starts keepalive
5. `Mesh.SendRequest()` marshals → sends → waits for response with timeout
6. Keepalive ticker fires every 30s, sends KEEPALIVE message
7. `handleMessage()` dispatches RESPONSE to pending channel, ERROR to error channel
8. Unit tests for message marshal/unmarshal round-trip

---

## CI-003: Agent Registry + Inboxes (net-new)

**No Hivemind source — built from scratch**

### Design
- Registry is backed by a pluggable Store: in-memory by default, PostgreSQL when `CR_DATABASE_URL` is set (CI-003b — implemented)
- Agent: `{ID, PublicKey, Capabilities, Status, RegisteredAt, LastSeen}`
- Inbox is a per-agent FIFO queue with lease-based delivery
- Messages expire after TTL (default 24h)
- Lease prevents double-delivery: retrieve leases messages for N seconds, ACK confirms, un-ACKed messages return to queue after lease expires

### Endpoints
- `POST /agents` — register agent
- `GET /agents` — list all
- `GET /agents/{id}` — detail with health
- `DELETE /agents/{id}` — unregister
- `POST /agents/{id}/inbox` — deliver message
- `GET /agents/{id}/inbox` — retrieve leased messages
- `POST /agents/{id}/inbox/ack` — acknowledge delivery
- `GET /agents/{id}/inbox/stats` — queue depth, leased count, oldest message age

### Acceptance criteria
1. Register agent with ed25519 public key → 201
2. Duplicate agent ID → 409
3. Retrieve leased messages → only un-ACKed messages returned
4. ACK → messages permanently removed
5. Un-ACKed messages return to queue after lease TTL expires
6. TTL-expired messages auto-purged
7. Agent unregister cleans up inbox
8. Concurrent delivery: two retrievers get disjoint message sets
