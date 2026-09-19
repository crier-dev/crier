# AGENTS.md — working in `specs/`

This directory holds crier's **normative specifications**: the contracts that implementations,
tests and docs are held to. `_index.md` lists each one with what it defines and its status.

- `AGENT-ECOSYSTEM.md` — the `examples/agent-ecosystem/` reference stack, its battery, CI matrix
  and bunker deployment.
- `LLM-MESSAGE-GUARD.md` — the inbound LLM message guard: verdict contract, per-channel policies,
  provider routing.
- `WEBHOOK-DELIVERY.md` — webhook push delivery: outbound envelope, delivery modes, schema
  templates, federation hold/retry.
- `ci-003b-postgresql-persistence.md` — PostgreSQL persistence for the registry and inboxes.

## Rules

1. **Specs and code move together.** A change to a documented contract — a wire field, header,
   status code, default, invariant or error code — lands together with its spec update, or the spec
   edit is filed as a board row (`DF-CRIER-*`) so it is tracked rather than forgotten.
2. **The shipped code wins.** When a spec and the shipped code disagree, do not silently rewrite
   either side: file the drift as a board row and fix whichever side is actually wrong.
3. **Contracts, not prose.** State exact field names, status codes, defaults, error codes and
   invariants, and keep the `Status:` / ticket line at the top current. A worker must be able to
   implement a spec with zero clarifying questions.
4. **Respect the scope lists.** Each spec's out-of-scope / non-goals section is binding: deliver
   what the spec names and nothing adjacent.
5. **Keep the index honest.** A new spec file needs an `_index.md` entry; do not rename an existing
   one, because code and docs reference these paths by name.

## Pointers

- `../AGENTS.md` — repo-wide build, test, commit and gate rules; read it first.
- `docs/` — the human-facing view: `architecture.md`, `specs.md`, `mesh-protocol.md`,
  `openapi.yaml`, `AGENT-ECOSYSTEM.md`, `integration-guide.md`.
- `docs/claims.yaml` + `cmd/server/docsclaims_test.go` — `make docs-check`; prose drift fails the
  build.

Last reviewed 2026-09-19.
