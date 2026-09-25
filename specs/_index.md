# specs/ — index of the normative specifications

The six files below are crier's **normative design authority**: they define the contracts that
implementations, tests and docs must match. Where a spec and the shipped code disagree, the shipped
code wins — file the drift as a board row (`DF-CRIER-*`) rather than silently rewriting either side.

| Spec | What it normatively defines | Status |
|---|---|---|
| [`AGENT-ECOSYSTEM.md`](AGENT-ECOSYSTEM.md) | The `examples/agent-ecosystem/` reference stack: harness matrix (shipped + planned), self-registration payload, `schema_template`/`response_map` wiring, guard default policy, delivery lanes, battery probe contract + evidence JSONL shape + exit codes, CI jobs and bunker config-matrix cells, bunker deploy contract. | DRAFT v1 · 2026-08-24 · CR-SPEC-003, CR-FEAT-021, CR-FEAT-022 |
| [`LLM-MESSAGE-GUARD.md`](LLM-MESSAGE-GUARD.md) | The inbound message guard: attack classes and threat model, containment at the single delivery choke point, exact verdict schema (JSON + Go), the guard prompt, deterministic escalation, sanitize rewrite + deterministic fallbacks, provider-failure handling, per-channel policy model, provider presets / failover / circuit breaker, config surface. | DRAFT v1 · 2026-08-22 · CR-SPEC-002, CR-FEAT-010..014 |
| [`WEBHOOK-DELIVERY.md`](WEBHOOK-DELIVERY.md) | Push-based HTTP delivery: the `webhook` object on `POST /agents`, outbound envelope and delivery target identity, response contract, `blocking`/`async`/`batch` delivery modes, session and context mapping, schema templates, self-configuration directives, federation links and the hold/retry contract, env config surface, implementation plan, E2E battery additions. | DRAFT v1 · 2026-08-19 · CR-SPEC-001, CR-FEAT-001..008 |
| [`ci-003b-postgresql-persistence.md`](ci-003b-postgresql-persistence.md) | PostgreSQL persistence for the registry and inboxes: required repository layout, storage invariants, the `Store` contract and error taxonomy, migrations, per-operation behaviour (register / get / list / unregister / deliver / retrieve / ack / stats / purge), startup order and dependency injection, completion criteria. | Implemented — the file carries no `Status:` header; the README roadmap marks CI-003b as done, and `docs/specs.md` describes the same work as CI-003b. |
| [`A2A-OPTION.md`](A2A-OPTION.md) | A2A interoperability as an OPT-IN extra (INT-A2A-001/002/003): the binding decision (JSON-RPC 2.0 over HTTP + SSE implemented; gRPC and HTTP+JSON/REST declared MAY, not built), the A2A ⇄ crier object mapping, the two-half gate (`CR_A2A_ENABLED` default false + the optional per-agent `a2a` block), the route surface (two routes while the switch is on: the Agent Card discovery route, §5.3, and the JSON-RPC binding `POST /a2a`, §5.4), the JSON-RPC binding's request/part/error/streaming contracts and the one stated deviation from the spec's blocking default, the non-regression contract, and the binding non-goals. | DRAFT v2 · 2026-09-25 · INT-A2A-001 (the gate), INT-A2A-002 (Agent Card, SHIPPED), INT-A2A-003 (JSON-RPC binding, SHIPPED), INT-A2A-004..006 (the remaining surfaces, not yet built) |
| [`DETECTION.md`](DETECTION.md) | The OPT-IN detection & containment layer (CR-FEAT-030): the attribution-vs-detection gap it closes, the exact opt-in contract (`CR_DETECT_ENABLED` default false — five routes unregistered, no file written, delivery path byte-identical), the append-only ed25519 delivery-log record format (verdicts, hash chain, key management, startup refusal on a log that does not verify, fsync cost), the four behaviour signals with their windows / thresholds / evidence, the single-call kill-switch's four ordered actions and their status semantics, the canary contract and the payload-scan bound, and the non-claims. | Implemented· OPT-IN · 2026-09-25 · CR-FEAT-030 (source: external review DISPATCH · CRI-001 by Carter) |

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

Spec inventory reconciled 2026-09-25 (A2A-OPTION.md added — INT-A2A-001; DETECTION.md added — CR-FEAT-030; A2A-OPTION.md re-reconciled for INT-A2A-003 — the JSON-RPC binding's §5.4 and the two-route surface).
