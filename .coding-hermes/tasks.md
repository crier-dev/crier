# Crier tasks

## Open

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

- [ ] **FEAT-002: Structured logging**
  - Replace `log.Printf` across all packages with `log/slog`
  - Structured fields: method, path, status, duration, agent_id, trace_id
  - Add `--log-level` flag and `CR_LOG_LEVEL` env var (debug/info/warn/error)
  - JSON format for production, text for development

- [ ] **DOC-003: Create CONTRIBUTING.md**
  - Build/run/test commands, Docker setup, Go conventions, PR template
  - Architecture overview, package map, testing strategy (short vs integration)

- [ ] **COV-005: Entrypoint smoke tests**
  - `cmd/server/main_test.go` — starts server, hits /health, verifies 200
  - `cmd/crier-mcp/main_test.go` — starts MCP server, runs initialize handshake
  - Catches wiring regressions early

## Done

- [x] **INFRA-001: Upgrade Go to 1.26.5** (done 2026-07-18)
- [x] **COV-001: PostgresStore integration tests** (done 2026-07-19)
- [x] **CI-009: Fix flaky mesh test** (done 2026-07-19)
- [x] **CI-001 through CI-008** (done 2026-07-11 to 2026-07-17)
- [x] **DOC-001, DOC-002** (done 2026-07-15)
