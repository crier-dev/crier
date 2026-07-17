# Crier tasks

## Open

- [ ] **INFRA-001: Upgrade Go from 1.26.0 to 1.26.5** — 3 stdlib CVEs
  - GO-2026-5856: crypto/tls ECH privacy leak (fixed in go1.26.5)
  - GO-2026-5039: net/textproto error escaping (fixed in go1.26.4)
  - GO-2026-5037: crypto/x509 inefficient hostname parsing (fixed in go1.26.4)
  - _Load: none (infrastructure — Go version upgrade by admin)_

- [x] **CI-008: Add mesh test coverage** (done 2026-07-15)
  - mesh coverage 2.8% → 90.5% — dialer_test.go (299 lines), peer_test.go (557 lines), handler_test.go (156 lines)
  - 1012 lines of new tests across 3 files, all pass, guard clean
  - Commit 683cdbc
- [x] **CI-003b: PostgreSQL persistence** (done 2026-07-16)
  - [x] SPEC: Axiom-level PostgreSQL spec — 983 lines, 10 sections, exact DDL, Store interface, pgxpool backend, migrations, connection mgmt, test strategy (commit 0566274, 2026-07-16)
  - [x] IMPLEMENT: Replace in-memory registry and inbox storage with PostgreSQL per spec (commit fb4f896, 2026-07-16)
  - 16 files changed, +1377/-507: postgres_store.go (600 lines, 9 methods), migrate.go, 4 migration SQL files, interface extraction, handler refactor, config extension, Makefile test-integration target
  - All 26 existing registry tests pass on MemoryStore, build+vet green, guard PASS
  - _Load: ad-hoc-verification-bash-script_
- [x] **CI-007: MCP server** (2026-07-15)\n  - [x] SPEC: axiom-level MCP server spec — 818 lines, 8 tools, error catalog, Mermaid diagram, test strategy (commit f5b13a4, 2026-07-16)\n  - [x] IMPLEMENT: MCP server wrapping registry.Store — stdio transport, 5 files, +1424 lines, per spec docs/specs/ci-007-mcp-server.md (commit 862fe1c, 2026-07-17)\n  - 8 tools: register_agent, list_agents, get_agent, unregister_agent, deliver_message, retrieve_inbox, ack_messages, inbox_stats\n  - 27 tests (25 unit + 1 concurrent + 1 lease expiry), all pass. Build+vet green. Guard PASS.\n  - Foreman-direct-code from axiom-level spec — zero worker spawn. No new deps (stdlib only).\n  - _Load: ad-hoc-verification-bash-script_

## Done

- [x] **DOC-001: Create README.md** (done 2026-07-15)
  - README.md created: 133 lines, project overview, architecture, build/run/test, API table, docs links, badges, status/roadmap
  - Commit 57b3746
- [x] **DOC-002: Clarify PostgreSQL status in architecture.md** (done 2026-07-15)
  - "Storage: PostgreSQL" → "Storage: In-memory (PostgreSQL planned via CI-003b)"
  - Commit 57b3746
- [x] **CI-006: Add GitHub Actions CI workflow** (done 2026-07-15)
  - `.github/workflows/ci.yml` — build/vet/test on push/PR, matrix go 1.22 + 1.23
  - CI run 29429586342: conclusion=success, commit 7f5133f
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
  - 7/7 GitReins PASS, commit 23e541a. GitReins stale task resolved 2026-07-15 (7/7 PASS)
- [x] **CI-005: OpenAPI 3.1 spec** (done 2026-07-11)
  - 15 endpoints across 5 operation groups in docs/openapi.yaml
