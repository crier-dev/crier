# Contributing to Crier

Thanks for your interest in contributing. Crier is the communication backbone for the autonomous agent economy — a compact Go service with a small surface area and a high bar for correctness. This guide covers everything you need to build, test, and submit changes.

## Table of Contents

1. [Code of Conduct](#code-of-conduct)
2. [Quick Start](#quick-start)
3. [Build, Run, and Test](#build-run-and-test)
4. [Docker Setup](#docker-setup)
5. [Project Layout](#project-layout)
6. [Go Conventions](#go-conventions)
7. [Testing Strategy](#testing-strategy)
8. [Environment Variables](#environment-variables)
9. [Pull Request Checklist](#pull-request-checklist)

## Code of Conduct

Be respectful, assume good faith, and keep discussions focused on the work. Report unacceptable behaviour to the maintainers.

## Quick Start

```bash
git clone https://github.com/crier-dev/crier.git
cd crier
make build
make test-short
```

You need Go 1.26+ and Docker (only for integration tests).

## Build, Run, and Test

All common workflows are exposed as `make` targets. Run `make help` to see what's available.

| Target | Command | Purpose |
|--------|---------|---------|
| `make build` | `go build -o bin/crier ./cmd/server` | Compile the main server binary into `bin/crier` |
| `make run` | `make build && ./bin/crier` | Build and run the server (default `:8767`) |
| `make test` | `go test ./... -count=1 -timeout 60s` | Full test suite, no caching |
| `make test-short` | `go test -short ./...` | Unit tests only — skips integration, no Docker required |
| `make test-integration` | `go test -tags=integration -count=1 -timeout 5m ./internal/registry` | PostgreSQL-backed registry tests (requires Docker) |
| `make lint` | `go vet ./...` | Static analysis |
| `make clean` | `rm -rf bin/` | Remove built binaries |
| `make generate` | `go generate ./...` | Run `go:generate` directives (mocks, etc.) |

### Building the MCP server

The MCP server is a separate binary:

```bash
go build -o bin/crier-mcp ./cmd/crier-mcp
```

It exposes registry and inbox tools over JSON-RPC — see [`docs/specs.md`](docs/specs.md) for the contract.

## Docker Setup

A single `docker-compose.yml` at the project root provides PostgreSQL 16 for integration tests.

| Service | Image | Port | Credentials |
|---------|-------|------|-------------|
| `postgres` | `postgres:16-alpine` | `5437:5432` | `crier` / `crier` / `crier` |

```bash
docker compose up -d     # start Postgres in the background
docker compose down      # stop and remove the container
```

The container must be running before `make test-integration`. The short test suite (`make test-short`) does not require Docker — it's safe to run anywhere.

## Project Layout

```
crier/
├── cmd/
│   ├── server/         # Main HTTP server entry point
│   └── crier-mcp/      # MCP server binary (JSON-RPC over stdio/HTTP)
├── internal/
│   ├── relay/          # HTTP pub/sub with WebSocket topic subscriptions
│   ├── mesh/           # P2P WebSocket connections (peer discovery, keepalive)
│   ├── registry/       # Agent registry + inboxes (in-memory + PostgreSQL stores)
│   ├── middleware/     # HTTP middleware: auth, logging, panic recovery
│   └── mcp/            # MCP tool handlers exposing registry operations
├── config/             # Environment-based configuration loader
├── docs/               # OpenAPI 3.1 spec, architecture, component specs
├── Makefile            # All common workflows
└── docker-compose.yml  # PostgreSQL for integration tests
```

### Package Map

| Package | Depends On | Used By |
|---------|-----------|---------|
| `config` | `os`, `strconv` | `cmd/server`, `internal/middleware` |
| `internal/middleware` | `config`, `net/http` | `cmd/server`, `internal/relay`, `internal/mesh` |
| `internal/relay` | `gorilla/websocket`, `internal/middleware` | `cmd/server` |
| `internal/mesh` | `internal/registry`, `internal/middleware` | `cmd/server` |
| `internal/registry` | `net/http`, PostgreSQL driver | `cmd/server`, `internal/mesh`, `internal/mcp` |
| `internal/mcp` | `internal/registry` | `cmd/crier-mcp` |
| `cmd/server` | all of the above | (entry point) |
| `cmd/crier-mcp` | `internal/mcp` | (entry point) |

Rules of thumb:

- `cmd/` packages never import each other. They are the only places where `os.Args` and `main()` live.
- `internal/` packages may import other `internal/` packages freely but must never be imported by `cmd/` packages that don't need them.
- New cross-package types belong in the smallest package that owns them.

## Go Conventions

These conventions are enforced by code review and (where possible) by the test suite.

### Logging — `log/slog` only

Use the standard library's structured logger. Never reach for the legacy `log` package in source files.

```go
// ✓ correct
slog.Info("publish", "topic", topic, "agent", agentID, "size", n)

// ✗ wrong
log.Printf("publish topic=%s agent=%s size=%d", topic, agentID, n)
log.Println("publish", topic)
log.Fatalf("boom: %v", err)
```

Test files (`*_test.go`) are exempt — the `log` package is fine there.

### Configuration via env vars

All configuration flows through `config/config.go`. Use `CR_`-prefixed names for new variables and document them in [Environment Variables](#environment-variables). Validation belongs in `config.Load()` — fail fast at startup rather than lazily on first use.

### Authentication — Bearer tokens

External requests authenticate via `Authorization: Bearer <token>`. The token is read from `CR_AUTH_TOKEN` at startup. When the env var is empty, all requests pass through unauthenticated (a startup warning is logged) — fine for local development, never fine for production.

### Agent identity — ed25519

Agents identify themselves with raw ed25519 public keys (32 bytes, often base64-encoded on the wire). Signatures are verified with `crypto/ed25519`; the key never leaves the agent.

### Routing and WebSockets

- HTTP routing: [`gorilla/mux`](https://github.com/gorilla/mux)
- WebSockets: [`gorilla/websocket`](https://github.com/gorilla/websocket)

Don't introduce alternative routers or WS libraries without a discussion in an issue first — the rest of the codebase assumes these.

### Style

- Standard `gofmt` formatting (`go fmt ./...`).
- Imports: standard library, then third-party, then internal — separated by blank lines.
- Errors: wrap with `%w`, never swallow. Return errors to callers; let `main` decide how to surface them.
- Tests live alongside the code they cover (`foo.go` ↔ `foo_test.go`).

## Testing Strategy

Crier has three test tiers. Pick the smallest one that exercises your change.

### Unit tests (`make test-short`)

Pure Go. No Docker, no network, no global state. Run them anywhere, run them often.

```bash
make test-short
# or
go test -short ./...
```

When porting or refactoring, every existing short test must still pass.

### Integration tests (`make test-integration`)

Hit a real PostgreSQL via the `integration` build tag. Required for any change to registry persistence, queries, or migrations.

```bash
docker compose up -d
make test-integration
```

Tests are gated by `//go:build integration` and skipped under `make test-short`. They use the container described in [Docker Setup](#docker-setup).

### Full suite

```bash
make test
```

Runs everything. Required for CI and before opening a PR.

### Coverage

```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

Current baselines (informal): relay ~87%, registry + inboxes ~85%. New code should not regress these significantly; if it must, justify it in the PR description.

## Environment Variables

Configuration is read from the environment in `config/config.go`. Defaults shown.

| Variable | Default | Description |
|----------|---------|-------------|
| `CRIER_PORT` | `8767` | Server listen port |
| `CR_AUTH_TOKEN` | _(none)_ | Bearer token required for all write endpoints. Empty disables enforcement — local dev only. |
| `CR_LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `CR_LOG_FORMAT` | `text` | Log format: `text` or `json` |
| `CR_DATABASE_URL` | `postgres://crier:crier@localhost:5437/crier` | PostgreSQL connection string (highest precedence, ahead of `DATABASE_URL` and the legacy fallback — see note below). The docker-compose setup maps host `:5437` to container `5432`. |
| `CR_WS_ALLOWED_ORIGINS` | _(none)_ | Comma-separated WebSocket origin allowlist. Empty allows all. |

> **Legacy:** `CRIER_DATABASE_URL` is still read as the lowest-precedence fallback, but it is deprecated — set `CR_DATABASE_URL` instead.

### Database pool tuning

Optional, only relevant if you operate the registry store:

| Variable | Default | Description |
|----------|---------|-------------|
| `DATABASE_URL` | _(none)_ | Standard 12-factor override |
| `CR_DATABASE_MAX_CONNS` | `4` | Maximum pool connections |
| `CR_DATABASE_MIN_CONNS` | `0` | Minimum idle connections |
| `CR_DATABASE_MAX_CONN_LIFETIME` | `30m` | Recycle connections older than this |
| `CR_DATABASE_MAX_CONN_IDLE_TIME` | `5m` | Close idle connections after this |
| `CR_DATABASE_CONNECT_TIMEOUT` | `10s` | Timeout for new connections |

## Pull Request Checklist

Before opening a PR, confirm each of these. CI will check most of them — running them locally first shortens the round trip.

- [ ] `make test-short` passes locally.
- [ ] `make lint` (`go vet ./...`) is clean.
- [ ] If your change touches `internal/registry/` persistence or queries, `make test-integration` passes against the docker-compose Postgres.
- [ ] No `log.Printf`, `log.Println`, or `log.Fatalf` in source files. Test files are exempt. Use `log/slog` instead.
- [ ] New env vars are documented in [Environment Variables](#environment-variables) and validated in `config/config.go`.
- [ ] Public API or wire-protocol changes are reflected in `docs/openapi.yaml` and the relevant spec under `docs/specs/`.
- [ ] New behaviour has at least one test. Bug fixes include a regression test that fails on `main` and passes on the branch.
- [ ] Commit messages are imperative and scoped: `fix: cap relay subscriber count` not `updates`.
- [ ] PR description links the relevant issue and summarises the user-visible change.

### What we won't merge

- Drive-by reformatting of unrelated files.
- New third-party dependencies without prior discussion.
- Rewrites of working code without a justified reason.
- Changes to public APIs without an accompanying spec update.

## License

By contributing, you agree that your contributions will be licensed under the MIT License — see [LICENSE](LICENSE).
