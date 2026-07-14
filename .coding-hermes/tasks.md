# Crier tasks

## Open

- [x] **CI-003: Agent registry + persistent inboxes** (done 2026-07-14)
  - In-memory registry: register, list, get, unregister agents
  - Per-agent FIFO inbox with lease-based delivery
  - Lease prevents double-delivery (ACK confirms, un-ACKed return after TTL)
  - POST/GET/DELETE /agents, POST/GET /agents/{id}/inbox, POST .../ack, GET .../stats
  - 26 tests, 84.8% coverage, 8/8 GitReins PASS, commit 30c4aaa

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
  - `internal/relay/` — thread-safe in-memory pub/sub, 140+109 lines
  - 13 tests, 87.4% coverage, 7/7 GitReins PASS
- [x] **CI-002: Port mesh + peer connection** (done 2026-07-12)
  - `internal/mesh/` — dialer, message types, peer mesh — 744 lines
  - Ported from Hivemind pkg/federation (607 lines), de-Hivemind'd
  - 8/8 GitReins judge PASS
- [x] **CI-005: OpenAPI 3.1 spec** (done 2026-07-11)
  - 15 endpoints across 5 operation groups in docs/openapi.yaml
