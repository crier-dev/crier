# Crier tasks

## Open

_All tasks complete._

## Done

- [x] **CI-001: Wire relay server** (done 2026-07-11)
  - `internal/relay/` — thread-safe in-memory pub/sub, 140+109 lines
  - 13 tests, 87.4% coverage, 7/7 GitReins PASS
- [x] **CI-002: Port mesh + peer connection** (done 2026-07-12)
  - `internal/mesh/` — dialer, message types, peer mesh — 744 lines
  - Ported from Hivemind pkg/federation (607 lines), de-Hivemind'd
  - 8/8 GitReins judge PASS
- [x] **CI-003: Agent registry + persistent inboxes** (done 2026-07-14)
  - `internal/registry/` — store, handler, types — 587 lines
  - FIFO inbox with lease-based delivery, TTL expiry
  - 26 tests, 84.8% coverage, 8/8 GitReins PASS
- [x] **CI-004: Wire full HTTP API + tests** (done 2026-07-14)
  - Mesh handler: WebSocket accept at /mesh/connect/{agentID}, GET /mesh/peers
  - Middleware: request logging + panic recovery
  - 17 endpoints wired across relay, mesh, registry, inbox
  - Graceful shutdown drains Mesh before HTTP
  - 7/7 GitReins PASS, commit 23e541a
- [x] **CI-005: OpenAPI 3.1 spec** (done 2026-07-11)
  - 15 endpoints across 5 operation groups in docs/openapi.yaml
