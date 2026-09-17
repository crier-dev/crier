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
make shell-yaml-check # bash -n every tracked shell script + actionlint (or PyYAML fallback) every .github/workflows/*.yml (DF-CRIER-206); an explicit file list fails closed (DF-CRIER-206, DF-CRIER-208)
make shell-yaml-selftest # prove that checker still rejects a broken script/workflow and accepts a clean pair (DF-CRIER-206)
make make-docker-check # make -n dry-parse every tracked Makefile + hadolint (or the built-in python3 parse) every tracked Dockerfile (DF-CRIER-209); an explicit file list fails closed
make make-docker-selftest # prove that checker still rejects a broken Makefile / malformed Dockerfile and accepts a clean set, incl. a neuter proof (DF-CRIER-209)
make install-hooks    # install scripts/hooks/pre-commit into .git/hooks (idempotent; DF-CRIER-206)
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

## The commit gate — what it covers, and what it does not

The gate is `gitreins guard` run from `.git/hooks/pre-commit` (Tier 1: secrets /
go_build / go_lint / go_tests). Those checks read Go source and the staged diff —
they do NOT read a shell script, a Makefile recipe or a workflow YAML. On a diff
made only of such files the gate used to print `Tier 1 Guards: PASS` having
verified nothing about it (DF-CRIER-206: measured with an unterminated `if` in a
`.sh` and an unclosed flow sequence in a workflow — both PASSed). A
Makefile-only or Dockerfile-only diff had the same problem until DF-CRIER-209:
the shell/YAML arm reads no Makefile and no Dockerfile, so that green was only
*labelled*, not earned.

The tracked wrapper `scripts/hooks/pre-commit` closes that gap and states the
scope of every green:

- it runs `gitreins guard` first, unchanged (a nonzero guard still stops the
  commit before anything else runs), then
- runs `scripts/check-shell-yaml.sh` on the staged shell/workflow files and exits
  nonzero if it rejects one,
- runs `scripts/check-make-docker.sh` on the staged Makefile/Dockerfile files and
  exits nonzero if it rejects one (a checker that is missing on disk is itself
  exit 1, so the gate cannot degrade into silence), and
- prints which of those Tier 1 actually covers, e.g.
  `gate scope: no Go source staged — shell/YAML checks ran on 2 file(s): …`,
  `gate scope: no Go source staged — no shell/YAML file staged + 1 makefile/Dockerfile file(s) staged — those are checked here too: Makefile`
  or
  `gate scope: nothing guardable staged — the Tier-1 PASS verified nothing about this diff`.

`.git/hooks/pre-commit` is generated and untracked, so the tracked wrapper is the
source of truth: `make install-hooks` copies it into place (idempotent; it backs
up any existing hook first) and is the repair path after any of these, all of
which are MEASURED to clobber the installed hook:

| command | effect on the installed hook |
| --- | --- |
| `gitreins install` | overwrites `.git/hooks/pre-commit` unconditionally |
| `gitreins init --reset` | overwrites it (`if not isfile(hook) or args.reset`) |
| `gitreins init` | leaves an existing hook alone |

Never install the hook as a SYMLINK: `gitreins install` opens the hook for
writing, which writes THROUGH a symlink and destroys the tracked file it points
at (measured). `make install-hooks` copies, then verifies with `cmp`.

`make shell-yaml-check` is also a CI step (`.github/workflows/ci.yml`), so a
`.sh`/`.yml`-only push gets real evidence instead of a vacuous green. Workflows
are checked with `actionlint` when it is on PATH, otherwise with a python3 +
PyYAML parse; the mode and tool version are printed every run, and a run with
neither validator exits 2 rather than skipping. actionlint validates `runs-on:`
labels, so this repo declares its `bunker` self-hosted runner label in
`.github/actionlint.yaml` — that is a declaration, not a silencer: an undeclared
label is still reported.

`make make-docker-check` is the second arm (DF-CRIER-209) and is a CI step too.
It checks every tracked Makefile and Dockerfile, and prints the engines it used:

- **makefile** — a `make -n --no-print-directory -f <file> <target>` dry-parse
  over a target list derived from the file itself: the `.PHONY` list, else the
  first non-special target, else no target at all (GNU make's own default goal —
  nothing like `all` is ever invented, because a repo without an `all` target
  would fail with `No rule to make target 'all'`). It catches Makefile
  *structure*: a recipe indented with spaces (`missing separator`), an
  unterminated `define`, a bad `include`, an unresolvable prerequisite —
  reported with make's own `file:line` message. It does **not** check the shell
  inside a recipe: `make -n` prints recipes, it does not run them, and no recipe
  text extraction is implemented (a recipe that calls a tracked script IS
  covered, by the shell arm on that script). Note that parsing a Makefile runs
  its `$(shell …)` calls, exactly as GNU make does even under `-n`.
- **dockerfile** — `hadolint <file>` when hadolint is on PATH (version printed),
  otherwise a built-in python3 (stdlib-only) structural parse. The built-in parse
  catches an unknown/misspelled instruction, a `FROM` with no image reference, a
  missing/invalid `AS` alias, a duplicate stage alias, a line-continuation
  dangling at EOF, and a `COPY/ADD --from=<ref>` that is empty or
  forward-references a stage alias defined LATER (an external image ref such as
  `nginx:alpine` is not rejected). It is not a linter: no base-image existence
  check, no hadolint rules, no `# escape=` directive handling.

`make make-docker-selftest` proves that checker still behaves — including two
NEUTER proofs (a copy of the checker with one arm's verdict call forced to
success must ACCEPT the same fixture the real arm rejects) so a green selftest
cannot be vacuous. The `make` binary is required whenever a makefile is in
scope, and one of hadolint/python3 whenever a dockerfile is: a missing validator
is exit 2 naming the tool, never a silent skip.

Given an EXPLICIT file list both checkers fail closed (DF-CRIER-208,
DF-CRIER-209): for the shell/YAML checker a named `*.yml`/`*.yaml` that is not
under `.github/workflows/`, or a list in which nothing classifies as shell or
workflow; for the make/docker checker a list in which nothing classifies as a
makefile or dockerfile. Either is rejected (exit 1) with every such path named —
neither ever prints `PASS — 0 file(s) checked` over a list it read nothing from.
The hook cannot trip these rules: it passes only files its own copy of the same
predicates already classified, and the default (no-argument) whole-repo mode is
unchanged.

Not covered by any of this: Markdown/prose drift (`make docs-check` covers the
claims in `docs/claims.yaml`); the shell inside a Makefile recipe (a recipe that
invokes a tracked script is covered on that script, but inline shell in a recipe
is not); Dockerfile semantics beyond structure (no base-image existence, no lint
rules when hadolint is absent); `$(shell …)` side effects of parsing a Makefile;
and anything outside the tracked file set.

## Board

Foreman board (JSONL-canonical, git-tracked): `.coding-hermes/board/tasks.jsonl` + `events.jsonl`. Recurring fixtures: E2E-001 (live battery), NEVER-DONE (audit sweep).

## Conventions

- Commit format: `<type>: <what> — <why>. Addresses <task-id>.` with a `Co-authored-by:` trailer.
- All commits must pass `gitreins guard` (secrets / build / lint / tests).
- No `git add -A`; stage specific files only.
