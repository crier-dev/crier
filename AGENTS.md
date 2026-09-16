# AGENTS.md

Crier — agent-to-agent message bus (Go). Relay (pub/sub) + WebSocket mesh + agent registry + durable inboxes.

## Commands

```bash
make build            # build bin/crier (cmd/server)
make build-mcp        # build bin/crier-mcp (cmd/crier-mcp)
make run              # build + run server (default port :8767)
make test             # full suite: go test ./... -count=1 -timeout 60s
make test-short       # go test -short ./...
make test-integration # integration tests (build tag): internal/registry
make lint             # go vet ./...
make coverage-check   # 70% coverage gate
make docs-check       # execute the prose claims in docs/claims.yaml against a live in-process server — prose drift fails the build (CR-GAP-055)
make generate         # go generate ./... (regenerates cmd/server/openapi.yaml from docs/openapi.yaml)
```

Config is env-driven (`CRIER_PORT`, `CR_DATABASE_URL`, `CR_AUTH_TOKEN`, `CR_REQUIRE_AGENT_SIG`) — see README.md. The server binary also takes CLI flags: `./bin/crier --help` prints the full usage screen, `-port` overrides `CRIER_PORT`, `-db-url` overrides `CR_DATABASE_URL`, and `-version` prints the build version.

## Layout

- `cmd/server` — HTTP/WS relay server
- `cmd/crier-mcp` — MCP server exposing registry + inbox tools
- `internal/{relay,mesh,registry,middleware,config,mcp}` — packages
- `docs/` — architecture.md, specs.md, mesh-protocol.md, openapi.yaml
- `specs/` — AGENT-ECOSYSTEM.md, LLM-MESSAGE-GUARD.md, WEBHOOK-DELIVERY.md, ci-003b-postgresql-persistence.md
- `examples/demo.sh` — runnable register → deliver → signed retrieve → ack round-trip

`docs/openapi.yaml` is the OpenAPI source; `cmd/server/openapi.yaml` is a GENERATED copy — `//go:generate cp ../../docs/openapi.yaml openapi.yaml` in `cmd/server/openapi.go:16-19` (go:embed cannot reach outside the package dir), asserted byte-identical by `TestOpenAPIDocsSpec` and by CI. Edit the source, run `make generate`, and stage both.

## Board

Foreman board (JSONL-canonical, git-tracked): `.coding-hermes/board/tasks.jsonl` + `events.jsonl`. Recurring fixtures: E2E-001 (live battery), NEVER-DONE (audit sweep).

## Conventions

- Commit format: `<type>: <what> — <why>. Addresses <task-id>.` with a `Co-authored-by:` trailer.
- All commits must pass `gitreins guard` (secrets / build / lint / tests).
- No `git add -A`; stage specific files only.
