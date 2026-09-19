# specs/ — index of the normative specifications

The four files below are crier's **normative design authority**: they define the contracts that
implementations, tests and docs must match. Where a spec and the shipped code disagree, the shipped
code wins — file the drift as a board row (`DF-CRIER-*`) rather than silently rewriting either side.

| Spec | What it normatively defines | Status |
|---|---|---|
| [`AGENT-ECOSYSTEM.md`](AGENT-ECOSYSTEM.md) | The `examples/agent-ecosystem/` reference stack: harness matrix (shipped + planned), self-registration payload, `schema_template`/`response_map` wiring, guard default policy, delivery lanes, battery probe contract + evidence JSONL shape + exit codes, CI jobs and bunker config-matrix cells, bunker deploy contract. | DRAFT v1 · 2026-08-24 · CR-SPEC-003, CR-FEAT-021, CR-FEAT-022 |
| [`LLM-MESSAGE-GUARD.md`](LLM-MESSAGE-GUARD.md) | The inbound message guard: attack classes and threat model, containment at the single delivery choke point, exact verdict schema (JSON + Go), the guard prompt, deterministic escalation, sanitize rewrite + deterministic fallbacks, provider-failure handling, per-channel policy model, provider presets / failover / circuit breaker, config surface. | DRAFT v1 · 2026-08-22 · CR-SPEC-002, CR-FEAT-010..014 |
| [`WEBHOOK-DELIVERY.md`](WEBHOOK-DELIVERY.md) | Push-based HTTP delivery: the `webhook` object on `POST /agents`, outbound envelope and delivery target identity, response contract, `blocking`/`async`/`batch` delivery modes, session and context mapping, schema templates, self-configuration directives, federation links and the hold/retry contract, env config surface, implementation plan, E2E battery additions. | DRAFT v1 · 2026-08-19 · CR-SPEC-001, CR-FEAT-001..008 |
| [`ci-003b-postgresql-persistence.md`](ci-003b-postgresql-persistence.md) | PostgreSQL persistence for the registry and inboxes: required repository layout, storage invariants, the `Store` contract and error taxonomy, migrations, per-operation behaviour (register / get / list / unregister / deliver / retrieve / ack / stats / purge), startup order and dependency injection, completion criteria. | Implemented — the file carries no `Status:` header; the README roadmap marks CI-003b as done, and `docs/specs.md` describes the same work as CI-003b. |

## How to read these

- For the narrative view, start with `docs/architecture.md` (architecture overview and design
  decisions) and `docs/specs.md` (component specs CI-001..CI-003 with their acceptance criteria);
  the files here are the contract-level detail behind them.
- `docs/mesh-protocol.md` is the authoritative wire reference for the mesh, and
  `docs/AGENT-ECOSYSTEM.md` is the human walkthrough derived from `specs/AGENT-ECOSYSTEM.md`.
- `docs/README.md`… the repo-wide documentation table lives in the root `README.md` (§Documentation).
- `docs/claims.yaml` + `cmd/server/docsclaims_test.go` — run by `make docs-check` — execute the prose
  claims those docs make, so a doc edit that invents a route or a default fails the build. Prose docs
  are the human view; these specs are what the code is held to.

Spec inventory reconciled 2026-09-19.
