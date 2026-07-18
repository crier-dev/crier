# Crier tasks

## Open

- [x] **INFRA-001: Upgrade Go to 1.26.5 for 3 stdlib CVEs** (done 2026-07-18, commit 0b99218)
  - CVE-2026-43489 (encoding/gob stack overflow), CVE-2026-43490 (mime/multipart), CVE-2026-43491 (crypto/rand on plan9)
  - Update go.mod to go 1.26.5, update CI matrix, rebuild, verify all tests pass

## Done

- [x] **CI-001: Wire relay server** (done 2026-07-11)
  - `internal/relay/` — thread-safe in-memory pub/sub, 140+109 lines
  - 13 tests, 87.4% coverage, 7/7 GitReins PASS
- [x] **CI-002: Port mesh + peer connection** (done 2026-07-12)
  - `internal/mesh/` — dialer, message types, peer mesh — 744 lines
  - Ported from Hivemind pkg/federation (607 lines)
  - 8/8 GitReins judge PASS
- [x] **CI-003: Agent registry + persistent inboxes** (done 2026-07-14)
  - `internal/registry/` — store, handler, types — 587 lines
  - FIFO inbox with lease-based delivery, TTL expiry
  - 26 tests, 84.8% coverage, 8/8 GitReins PASS
- [x] **CI-003b: PostgreSQL persistence** (done 2026-07-17)
  - `internal/registry/postgres_store.go` — 600 lines, pgxpool-backed Store
  - `internal/registry/migrate.go` — golang-migrate with embedded SQL
  - DDL: agents + inbox_entries with FK cascade, CHECK constraints, indexes
  - Atomic FIFO retrieve with SKIP LOCKED, lease-based ACK
  - Wired in main.go and cmd/crier-mcp via CR_DATABASE_URL
  - Commit: fb4f896
- [x] **CI-004: Wire full HTTP API + tests** (done 2026-07-14)
  - 17 endpoints across relay, mesh, registry, inbox
  - Middleware: logging + recovery. Graceful shutdown drains Mesh
  - 7/7 GitReins PASS
- [x] **CI-005: OpenAPI 3.1 spec** (done 2026-07-11)
  - 15 endpoints across 5 operation groups in docs/openapi.yaml
- [x] **CI-006: GitHub Actions CI** (done 2026-07-15)
  - `.github/workflows/ci.yml` — build/vet/test, go 1.22+1.23
- [x] **CI-007: MCP server** (done 2026-07-17)
  - `internal/mcp/` — stdio JSON-RPC server, 8 tools wrapping registry.Store
  - `cmd/crier-mcp/main.go` — standalone binary
  - 27 tests pass
  - Commit: a0cd686
- [x] **CI-008: Mesh test coverage** (done 2026-07-15)
  - 2.8% → 90.5% — 1012 lines of new tests across 3 files
- [x] **DOC-001: README.md** (done 2026-07-15)
- [x] **DOC-002: PostgreSQL status in architecture.md** (done 2026-07-15)
