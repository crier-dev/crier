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
```

Config is env-driven (`CRIER_PORT`, `CR_DATABASE_URL`, `CR_AUTH_TOKEN`, `CR_REQUIRE_AGENT_SIG`) — see README.md. The server binary takes no CLI flags.

## Layout

- `cmd/server` — HTTP/WS relay server
- `cmd/crier-mcp` — MCP server exposing registry + inbox tools
- `internal/{relay,mesh,registry,middleware,config,mcp}` — packages
- `docs/` — architecture.md, specs.md, mesh-protocol.md, openapi.yaml
- `examples/demo.sh` — runnable register → deliver → signed retrieve → ack round-trip

## Board

Foreman board (JSONL-canonical, git-tracked): `.coding-hermes/board/tasks.jsonl` + `events.jsonl`. Recurring fixtures: E2E-001 (live battery), NEVER-DONE (audit sweep).

## Conventions

- Commit format: `<type>: <what> — <why>. Addresses <task-id>.` with a `Co-authored-by:` trailer.
- All commits must pass `gitreins guard` (secrets / build / lint / tests).
- No `git add -A`; stage specific files only.
