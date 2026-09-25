# Crier — Component Specs

## CI-001: Port Relay Server + Client from Hivemind

**Source:** `hivemind-work/pkg/collab/relay.go` (422 lines) + `event.go` (180 lines)

### What to port
- `RelayClient` — pub/sub with token-bucket rate limiting, channel-based subscribers, HTTP relay POST with Bearer auth, reconnect with exponential backoff
- `RelayServer` — ported as the crier relay: `POST /relay/publish` + JSON event decode, Bearer auth middleware, `GET /health` (the Hivemind source served `/events` (POST); shipped crier registers no `/events` route)
- `RelayManager` — thin lifecycle wrapper (Start/Stop/Publish/Subscribe)
- `Event` types — simplified: keep `Type`, `Topic` (was WorkspaceID), `AgentID` (was UserID), `Timestamp`, `Payload`. Drop Hivemind-specific event types (file:*, git:*, presence:*). Add agent-native types: `agent:announce`, `agent:heartbeat`, `agent:message`, `agent:error`

### Changes from Hivemind
- `workspaceID` → `topic` throughout
- `userID` → `agentID` throughout
- Drop `RelayClient.Connect()` stub ("simulate" comment) — wire real WebSocket via gorilla/websocket
- Replace `net/http` ServeMux with gorilla/mux router
- Add graceful shutdown (already in our scaffold)
- Import path: `github.com/crier-dev/crier/internal/relay`

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
- `Mesh` — peer connect, one-way REGISTER on connect (fire-and-forget; the server never emits REGISTER_ACK), keepalive loop (30s), `SendRequest` with pending tracking + timeout, `handleMessage` type dispatch (RESPONSE/ERROR)
- Federation message types: Envelope, Register, RegisterAck, Keepalive, Request, Response, ErrorMessage, ErrorDetail, PeerRef, Capabilities (KeepaliveAck is NOT part of the wire protocol — see `docs/mesh-protocol.md`)
- `Marshal()` — newline-delimited JSON framing
- `newMessageID()` — crypto/rand hex

### Changes from Hivemind
- Remove `collab.PeerStore` dependency — use `internal/registry` (built in CI-003)
- `controllerID` → `agentID` throughout
- `workspaces` in Capabilities → `topics`
- Import path: `github.com/crier-dev/crier/internal/mesh`
- Drop Deregister message type (use agent unregister from CI-003)

### Wire format
`docs/mesh-protocol.md` is the authoritative wire reference for this spec: REQUEST and
RESPONSE frames carry `PeerRef` `source`/`target`, a RESPONSE carries its `status_code`,
and a RESPONSE's `request_id` MUST echo the originating REQUEST's `message_id` (the
correlation contract — a non-matching `request_id` is dropped with no error and the
requester stalls). KEEPALIVE frames ride the same socket as the conversation (the
client's 30s ticker fires between its REQUEST and its RESPONSE), so a reader MUST
loop-recv and dispatch each frame on its `type`, buffering frames it is not ready for;
a naive blocking read stalls or misparses mid-exchange.

### Acceptance criteria
1. `PeerConnection.Connect()` dials WebSocket, starts read loop
2. `PeerConnection.Send()` writes with 10s deadline
3. `PeerConnection.Close()` sends close frame, cleans up
4. `Mesh.ConnectPeer()` dials → registers → starts keepalive
5. `Mesh.SendRequest()` marshals → sends → waits for response with timeout
6. Keepalive ticker fires every 30s, sends KEEPALIVE message (client-driven; nothing on the server requires it, and the receiving server records it as liveness evidence for the sender's registry row — CR-FEAT-024)
7. `handleMessage()` dispatches RESPONSE to the pending channel; ERROR frames are converted to synthetic RESPONSEs (status 500) into the same channel
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
8. Concurrent delivery is race-free but NOT fairly distributed: no message is leased to two retrievers, yet a retrieve claims up to `max` (default 10) of the currently claimable messages, so a concurrent second retrieve can get an empty batch while the first drains the queue — observed 4/0 starvation, not distribution (DF-CRIER-169). `GET /agents/{id}/inbox/stats` (`queue_depth` vs `leased_count`) is how a starved retriever tells a drained queue from an empty one
