# Crier diagnostics — 2026-09-25 addendum 2: what "opt-in" must mean to be trustworthy

Context: the 2026-09-25 addendum covered the identity/rate-limiter split. This
one covers the A2A option gate (INT-A2A-001) from the same day, driven by an
actual user workflow instead of its own test suite. The lesson: **a feature
that promises "I change nothing" is judged by evidence classes its own unit
tests cannot produce.**

## What "opt-in and default-off" decomposes into (all four needed)

1. **The off posture is indistinguishable from the past.** Not "the new paths
   404" — the ENTIRE public contract must be unchanged. The cheapest user-level
   proof: `GET /openapi.json` is md5-identical with the switch unset, with it
   on, and across a restart. The server's own non-regression test asserts
   byte-for-byte equality of route behaviour; the user-level md5 check is the
   same idea observable from outside.
2. **A turned-on switch that ships nothing is STILL inert.** With
   `CR_A2A_ENABLED=true` the future paths (/.well-known/agent-card.json, a
   plausible JSON-RPC route) still 404 because INT-A2A-002..006 are not built.
   A user who flips the flag expecting surfaces gets silence — correct for
   today, and exactly why the README says "no A2A route, card or stream exists
   yet under either value".
3. **Half a gate must be inert alone.** The per-agent `a2a` block exists on the
   registry row while the server switch is off, and nothing consumes it. The
   interesting user-visible property is the inverse: registering WITH the block
   changes nothing else about the row — the no-block agent has NO `a2a` key at
   all (omitempty), so pre-A2A consumers diffing registry rows see nothing.
4. **Opt-in state is durable state.** The block rides PG migration 005 (a
   nullable column, additive). Verified by restart: enabled stays enabled, `{}`
   stays `{}`, absence stays absence. A gate that forgot its opt-in across a
   restart would be a silent-scope-change bug (the DF-CRIER-151 class).

## Why the strict decoder is user-visible (not just a test assertion)

`{"a2a":{"enabld":true}}` — one transposed letter — answers
`400 {"error":"a2a: unknown field \"enabld\" (accepted: enabled)"}` and
registers NOTHING (the 404 afterward proves the registry was not touched). The
alternative behavior — encoding/json dropping the unknown member — would hand
the user a 201 and an agent that silently never opted in; the user would
discover it months later when the A2A surfaces ship and their agent is not
reachable. Strict decode turns a time-bomb into an immediate, self-explaining
error. The error message carries BOTH the offending key and the accepted set,
which is what makes it fixable without opening docs.

Sibling parity note: a wrong-TYPED member (`"enabled":"yes"`) is refused by the
whole-body decoder as generic `400 invalid json` rather than the a2a-specific
message. Measured, and intentional — `internal/registry/a2a_optin_test.go:125`
documents that an ill-typed `webhook` member gets the same generic 400. Same
rule, same shape, two layers: unknown KEY inside the block → named error from
the strict scan; wrong TYPE → generic body-level decode error. A user sees two
different messages for two near-identical mistakes; that asymmetry is
documented, deliberate, and the named-key path is the one that matters.

## Errors hit this run, and the right way past each

- **Port collision on a shared host, twice.** First the compose default 5437
  (held by another project's container — checked `docker ps --filter
  publish=5437`, then used the documented `CRIER_PG_HOST_PORT` override), then
  the demo scratch-port range 18777-18781 (18787 was a python3 service; the
  README explicitly says to check with `ss -tlnp` and move). Crier's own
  failure mode is good: it exits non-zero naming the port, the holder-check
  command, and the `-port` alternative.
- **A fresh PG has no `crier_a`/`crier_b`.** The compose service creates only
  `crier`; the server's migration error names it precisely:
  `FATAL: database "crier_a" does not exist (SQLSTATE 3D000)`. One
  `docker exec <pg> psql -U crier -c "CREATE DATABASE ..."` per extra instance.
- **`%s%3N` epoch arithmetic produced 17-digit nonsense** in the timing script
  (date's %N resolution on this box is not ms — samples like
  178840736 ms). Correct tool: bash `EPOCHREALTIME` (µs float) subtracted in
  awk. The 119 ms / 60 ms numbers in the report are the corrected ones; the
  first pass's garbage timings were discarded, not averaged.
- **My own jq regex false-FAILed once** (`; accepted:` vs the actual
  `" (accepted:` separator in the strict-decode error). Rule: verify the
  verifier — re-run the failing assertion by hand against the captured body
  before believing a product defect.

## What "working" sounded like this run

Server logs stayed clean (`level=ERROR` count 0 on both servers across the
whole workflow); the guard failed OPEN with `errored=true` on a keyless
deployment exactly as the README's guard note promises; the signed PATCH round
trip re-registered `last_seen` only on signed calls; restarts re-applied
nothing and the `leased_count=2 / queue_depth=2` after restart showed durable
leases surviving the process boundary.
