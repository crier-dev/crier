# Dogfood 2026-11-24 — the first-party client surface, for real

Tick: `crier-dogfood-2026-11-24-19-47-57` · tree `b48e1aad` (= origin/main at
fire time) · verdict: **SHIPPABLE for the documented client journey, with one
P1 trust finding (DF-CRIER-515) and one P2 onboarding finding (DF-CRIER-516)**.

## The promise under test

> "A new user can go from nothing to two real signed agents exchanging durable
> messages on a crier bus using only the first-party tooling: `make build` →
> `crier keygen` → `docker compose up -d postgres` → `make run` →
> `python3 clients/python/round_trip.py` / `two_agents.py` — no openssl, no
> xxd, no package manager."

This is the exact path `clients/README.md` documents as "The whole thing, from
nothing". It did not exist before v0.7.0; every earlier dogfood fire drove the
raw HTTP surface by hand.

## What a real run looked like (all numbers measured this fire)

| Milestone | Wall |
|---|---|
| `make build` (warm-ish tree) | 3.35 s, 42 MB binary, identity `v0.6.1-401-gb48e1aad` served by `-version` and `/status` |
| `crier keygen` ×3 (alice/bob/worker, fresh ids) | instant, mode-0600 PEM, prints `POST /agents` body |
| `docker compose up -d postgres` (port 5437 was held on this host → `CRIER_PG_HOST_PORT=15437`, remedy already in README §Run ~line 682) | ~6 s to healthy |
| server cold boot → first `/health` 200 (durable backend) | 1.24 s |
| `round_trip.py` (12 steps incl. two signature negative controls, self-cleanup) | 0.77 s, all green |
| TypeScript `round-trip.ts` (same 12 steps) | 0.57 s, all green |
| `two_agents.py` (signed deliver, cross-key ack refused 403, keyless delivery refused 401, proof nothing stored) | green |
| server stopped (`crier -stop`), restarted, `/health` 200 again | 0.26 s |
| unacked tasks surviving the restart (3/3, payloads intact) | verified |
| steady-state client-side op latency (n=20 deliver / n=15 ack) | deliver mean 5.69 ms, ack mean 6.75 ms |

**Perf verdict: no finding.** Cold boot 1.24 s, restart 0.26 s, per-op ~6 ms
client-side including ed25519 signing. Consistent with the 2026-11-15 fire's
11.6 ms p50 figures. Nothing a user would feel; no PERF row filed.

## The trust test that actually convinced me

A scratch durable work queue driven by my own consumer code (not the sample
transcripts): worker registers a queue identity and delivers 10 tasks; my
first consumer script had genuine user bugs (delivered-but-unregistered target
→ 404; double-register → 409; a retrieve that leased everything and returned
an empty batch on the next call). The lease model — retrieve leases for
`lease_seconds` (default 30 s), unacked messages return after expiry — is what
made every one of those bugs readable instead of "lost data". Clean arm:
deliver 10, ack 7, wait 32 s for the 3 leases to expire, stop the server,
restart, re-register idempotently, retrieve → **exactly the 3 unacked ids
survived with payloads intact**, ack → `unacked=0`. That is the restart story
README §Run promises, and it held.

## What fell over (rows filed)

1. **DF-CRIER-515 (P1, bug) — lost-key tombstone.** The shared compose volume
   (`crier_pgdata`) carried agents registered 2026-09-30 by an earlier run
   whose keys are gone (scratch pruned). Re-keying `alice` → 409
   `public_key_mismatch`; the error's own remedy (`DELETE /agents/{id}`)
   requires the *registered* key → 401. No operator/admin path exists (the
   management surface covers permissions writes only). On a durable bus the id
   is a tombstone forever, and this is not hypothetical: *this fire* reproduced
   it from a real prior run's residue two months later.
2. **DF-CRIER-516 (P2, task) — onboarding gaps in the client surface.** The
   three sample transcripts call bare `register()` and abort on re-run in
   exactly the case `crier_client.register_if_missing()` exists for ("re-runs
   are normal"); clients/README's own-code snippet omits the four facts this
   fire hit from the snippet alone: idempotent re-register, 404 for
   unregistered delivery targets, owner-only scoping on agent-scoped routes
   (403), and the retrieve-leases-for-30 s model.

## What a new user still needs that the docs don't say

See DF-CRIER-516; nothing else. Notably the friction I *expected* (port 5437
colliding with another container) was already documented with the exact
remedy — README §Run anticipated it.

## Evidence

Raw captures and probe scripts: `.coding-hermes/evidence/crier-dogfood-2026-11-24-19-47-57/`
(INDEX.md documents each file and its provenance). Diagnostic trail:
`docs/dogfood/diagnostics-2026-11-24-lost-key-lockout.md`.
