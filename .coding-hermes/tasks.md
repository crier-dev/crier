# Crier tasks

## Open

- [x] **DOC-003: Create CONTRIBUTING.md** (done 2026-07-19, commit e695ee9)
  - Build/run/test commands, Docker setup, Go conventions, PR template
  - Architecture overview, package map, testing strategy (short vs integration)

- [x] **COV-005: Entrypoint smoke tests** (done 2026-07-19, commit c0b2667)
  - `cmd/server/main_test.go` — TestServerHealth: starts server, hits /health, verifies 200, graceful shutdown
  - `cmd/crier-mcp/main_test.go` — TestMCPServerInitialize: builds binary, runs JSON-RPC initialize handshake
  - 2 tests, 213 lines, all passing. Catches wiring regressions early

- [x] **SPEC-001: Sync README/CI docs to reality** (done 2026-07-19, commit c0190b9)
  - README Go badge says 1.22+ → updated to 1.26+
  - README CI matrix says 1.22+1.23 → updated to 1.25+1.26 (matches ci.yml)
  - README mentions CRIER_JWT_SECRET → updated to CR_AUTH_TOKEN (Bearer, not JWT)
  - Architecture doc mentions "persistent event log with replay" → removed (not implemented)
  - Architecture doc mentions "Mutual TLS + token auth" → changed to "Token auth" (no mTLS)
  - Architecture doc Go version 1.22+ → 1.26+

- [x] **DOC-004: Add missing LICENSE file** (done 2026-07-19, commit c0190b9)
  - Created LICENSE with MIT text
  - README referenced LICENSE file that didn't exist

- [x] **TEST-001: Config package tests (0%→~90% coverage)** (done 2026-07-20, commit 5d7c52d, fix d31bba9)
  - `config/config_test.go` — 523 lines, comprehensive test suite
  - Cover: defaults, log level (valid+invalid), port (bounds), DB URL precedence (3 vars), pool config (valid+invalid), durations, MinConns>MaxConns
  - Uses config_test package, t.Setenv(), testify
  - All 14 tests passing. 2 fixes: unused cfg var + val=100 exceeding default MaxConns

- [x] **TEST-002: PostgresStore unit tests with mock DB** (done 2026-07-20, commit d31bba9)
  - `internal/registry/postgres_store_unit_test.go` — 50 tests, 708 lines
  - Uses pgxmock/v5 for SQL mocking, no Docker required
  - Cover: Register, Retrieve, Ack, Stats, PurgeExpired, Inbox, input validation, error mapping
  - Added `connPool` interface to PostgresStore for testability

- [ ] **TEST-003: Migrate package tests**
  - `internal/registry/migrate_test.go` — verify embedded migrations are valid
  - Test: RunMigrations with test DB, verify schema_migrations table
  - Currently 0% coverage

- [ ] **PITFALL-001: Implement rate limiting (stubbed)**
  - `POST /relay/publish` always returns 202; spec says 429 after 100/min/agent
  - Implement token-bucket or sliding-window rate limiter per agent
  - Wire into relay handler

- [ ] **PITFALL-002: Restrict WebSocket origin check**
  - `CheckOrigin` returns true for all connections (open relay risk)
  - Add configurable allowed origins or at minimum require same-origin
  - Add `CR_WS_ALLOWED_ORIGINS` env var

- [ ] **PERF-001: Add benchmarks for hot paths** (audit confirmed: 0 benchmarks)
  - `relay/relay_test.go`: BenchmarkPublish, BenchmarkSubscribe
  - `registry/memory_store_test.go`: BenchmarkRetrieve, BenchmarkAck
  - `mesh/message_test.go`: BenchmarkMarshal
  - Establish baseline for regression detection

- [ ] **CI-010: Add Dockerfile + Makefile fixes**
  - Create `Dockerfile` for `cmd/server` (multi-stage, distroless or alpine)
  - Create `Dockerfile.mcp` for `cmd/crier-mcp`
  - Makefile: add `build-mcp` target for `cmd/crier-mcp`
  - Makefile: add `docker-build` target

- [ ] **CI-011: Add coverage reporting to CI**
  - Upload `go test -coverprofile` output to coverage dashboard
  - Set coverage thresholds in CI (fail below 70%)
  - Track trends across runs

- [x] **QUALITY-001: Add doc comments to 11 undocumented exported functions** (done 2026-07-20)
  - `internal/registry/store.go`: NewHandler, NewMemoryStore
  - `internal/registry/migrate.go`: RunMigrations
  - `internal/registry/postgres_store.go`: DefaultPoolConfig, NewPostgresStore, NewPostgresStoreWithPoolConfig
  - `internal/mesh/peer.go`: DefaultMeshConfig, NewMesh
  - `internal/mesh/message.go`: Marshal
  - `internal/mesh/dialer.go`: DefaultDialerConfig, NewPeerConnection
  - All 11 functions now have Go-style doc comments describing purpose, defaults, and usage

- [x] **DUCKBRAIN-001: Populate project namespace** (done 2026-07-20)
  - `/projects/crier/` namespace now has 7 entries (was 2)
  - Stored: architecture, pitfalls (rate limit, WS origin), test patterns, Go config, CI-001/CI-002 history

## Done

- [x] **COV-002: Middleware tests — 0% → 80%+** (now 100% via FEAT-001 auth tests + existing logging/recovery tests)
  - `internal/middleware/` — logging, recovery, auth all tested
  - Coverage: 100% of statements
  - 10 auth tests covering no-token, health bypass, missing header, wrong scheme, wrong token, correct token, whitespace rejection, empty token
  - _Done 2026-07-19, commit ee64882_

- [x] **COV-003: MCP server tests — 67.5% → 80%+**
  - `internal/mcp/` — 27 tests exist but missing edge cases
  - Add: tool_handler error propagation, invalid JSON-RPC, concurrent requests, server shutdown
  - _Load: ad-hoc-verification-bash-script_

- [x] **INFRA-002: Add docker-compose.yml with PostgreSQL** (done 2026-07-19, commit 5e353e2)
  - postgres:16-alpine on :5437 (5432 taken), healthcheck, named volume
  - Enables PostgresStore integration tests locally
  - Integration tests verified passing against running container

- [x] **COV-004: PostgresStore tests — enable integration test suite** (done 2026-07-19, commit 02777e8)
  - `internal/registry/postgres_store_test.go` exists with `//go:build integration` tag
  - Wired into CI: added integration job running `go test -tags=integration ./internal/registry/`
  - Registry coverage: 36.4% → 78.1% with integration tests. All 48 tests pass.

- [x] **FEAT-001: Bearer auth middleware** (done 2026-07-19, commit ee64882)
  - OpenAPI spec § /relay/publish requires Bearer auth (401 on missing/invalid token)
  - `internal/middleware/auth.go` — Bearer token validation, /health exempt, empty token = pass-through
  - Wired on all routes via `r.Use(middleware.Auth(cfg.AuthToken))` in cmd/server/main.go
  - Config: `CR_AUTH_TOKEN` env var loaded into Config.AuthToken
  - 10 tests, 100% middleware coverage

- [x] **INFRA-001: Upgrade Go to 1.26.5** (done 2026-07-18)
- [x] **COV-001: PostgresStore integration tests** (done 2026-07-19)
- [x] **CI-009: Fix flaky mesh test** (done 2026-07-19)
- [x] **CI-001 through CI-008** (done 2026-07-11 to 2026-07-17)
- [x] **DOC-001, DOC-002** (done 2026-07-15)

- [x] **FEAT-002: Structured logging** (done 2026-07-19, commit b77c28a)
  - Replace `log.Printf` across all packages with `log/slog`
  - Structured fields: method, path, status, duration, agent_id, trace_id
  - Add `--log-level` flag and `CR_LOG_LEVEL` env var (debug/info/warn/error)
  - JSON format for production, text for development

## [ ] NEVER-DONE — Run coding-hermes-never-done 11-point audit

Load coding-hermes-never-done skill. Run ALL 11 checks: spec alignment, doc coverage, test gaps, package upgrades, pitfall hunt, performance audit, endpoint verification, CI/CD health, DuckBrain sync, code quality, middle-out wiring. Create a task for EVERY gap found. This task is never complete — the audit always finds something.
