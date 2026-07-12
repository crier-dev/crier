# Crier tasks

## Open

- [ ] **CI-001: Wire relay server** — HTTP endpoints for pub/sub with topic routing
  - POST /publish — publish event to topic
  - GET /subscribe/{topic} — WebSocket upgrade for topic subscription
  - GET /topics — list active topics
  - _Load: exhaustive-specification_

- [ ] **CI-002: Wire agent registry** — agent registration, discovery, health
  - POST /agents/register — register agent with capabilities + public key
  - GET /agents — list registered agents
  - GET /agents/{id} — agent detail with health status
  - DELETE /agents/{id} — unregister
  - _Load: exhaustive-specification_

- [ ] **CI-003: Wire persistent inboxes** — per-agent message queue
  - POST /agents/{id}/inbox — deliver message to agent inbox
  - GET /agents/{id}/inbox — retrieve undelivered messages
  - Lease-based delivery with ACK/NACK
  - Retention policy (age or count)
  - _Load: exhaustive-specification_

- [ ] **CI-004: Wire WebSocket mesh** — P2P agent connections
  - WebSocket dial + accept with mutual auth
  - Keepalive + automatic reconnect
  - Request/response correlation
  - _Load: exhaustive-specification_

- [ ] **CI-005: OpenAPI 3.1 spec** — single source of truth for all endpoints
  - Covers relay, registry, inbox, mesh
  - Auto-generates MCP tools
  - _Load: exhaustive-specification_

## Done
