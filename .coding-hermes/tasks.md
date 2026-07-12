# Crier tasks

## Open

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

## Done

- [x] **CI-001: Wire relay server** — HTTP endpoints for pub/sub with topic routing (done 2026-07-11)
  - POST /relay/publish, GET /relay/subscribe/{topic} (WebSocket), GET /relay/topics
  - 87.4% coverage, 13 tests, 7/7 GitReins criteria PASS
- [x] **CI-005: OpenAPI 3.1 spec** — single source of truth for all endpoints (done 2026-07-11)
