# Crier tasks

## Open

- [ ] **CI-002: Port mesh + peer connection from Hivemind**
  - Port `PeerConnection` — gorilla/websocket dial, read loop, Send, Close, callbacks (~168 lines)
  - Port `Mesh` — connect, register, keepalive, SendRequest, handleMessage (~293 lines)
  - Port federation message types — Envelope, Register, Keepalive, Request, Response, Error (~146 lines)
  - `controllerID` → `agentID`, remove `collab.PeerStore` dep, use our registry
  - Import as `internal/mesh`
  - _Spec: docs/specs.md § CI-002_
  - _Load: exhaustive-specification_

- [ ] **CI-003: Agent registry + persistent inboxes**
  - In-memory registry: register, list, get, unregister agents
  - Per-agent FIFO inbox with lease-based delivery
  - Lease prevents double-delivery (ACK confirms, un-ACKed return after TTL)
  - POST/GET/DELETE /agents, POST/GET /agents/{id}/inbox, POST .../ack, GET .../stats
  - _Spec: docs/specs.md § CI-003_
  - _Load: exhaustive-specification_

- [ ] **CI-004: Wire full HTTP API + tests**
  - Mount all handlers on gorilla/mux in cmd/server/main.go
  - Wire relay, mesh, registry, inbox routes
  - Middleware: request logging, recovery, agent auth
  - Wire graceful shutdown that drains mesh connections
  - GitReins tasks for each CI with per-criterion acceptance criteria
  - 85%+ coverage on internal/ packages
  - _Load: exhaustive-specification_

## Done

- [x] **CI-001: Wire relay server** (done 2026-07-11)
  - `internal/relay/relay.go` — thread-safe in-memory pub/sub, 140 lines
  - `internal/relay/handler.go` — WebSocket subscribe, JSON publish/topics, 109 lines
  - 13 tests, 87.4% coverage, 7/7 GitReins criteria PASS
- [x] **CI-005: OpenAPI 3.1 spec** (done 2026-07-11)
  - 15 endpoints across 5 operation groups in docs/openapi.yaml
