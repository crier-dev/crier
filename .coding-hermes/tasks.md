# Crier tasks

## Open

- [x] **COV-002: Middleware tests — 0% → 80%+**
  - `internal/middleware/middleware.go` — Logging + Recovery, 40 lines, zero tests
  - Test: request logging captures method/path/status/duration, recovery returns 500 on panic
  - 5+ test cases. GIVEN/WHEN/THEN for each
  - _Load: ad-hoc-verification-bash-script_

- [ ] **COV-003: MCP server tests — 67.5% → 80%+**
  - `internal/mcp/` — 27 tests exist but missing edge cases
  - Add: tool_handler error propagation, invalid JSON-RPC, concurrent requests, server shutdown
  - _Load: ad-hoc-verification-bash-script_

- [ ] **INFRA-002: Add docker-compose.yml with PostgreSQL**
  - postgres:16-alpine on :5432, healthcheck, init scripts
  - Enables PostgresStore integration tests locally
  - Required for COV-004

- [ ] **COV-004: PostgresStore tests — enable integration test suite**
  - `internal/registry/postgres_store_test.go` exists with `//go:build integration` tag
  - Wire into CI: add PostgreSQL service container + `go test -tags=integration ./internal/registry`
  - Target: registry package 36.4% → 80%+ (when PostgreSQL available)

- [ ] **FEAT-001: Bearer auth middleware**
  - OpenAPI spec § /relay/publish requires Bearer auth (401 on missing/invalid token)
  - Add `internal/middleware/auth.go` — Bearer token validation
  - Wire on /relay/*, /agents/*, /mesh/* endpoints
  - Config: CR_AUTH_TOKEN env var or static shared secret for v0.1
  - _Load: exhaustive-specification_

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
