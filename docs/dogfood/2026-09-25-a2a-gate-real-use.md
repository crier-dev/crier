# Crier real-use run — the A2A opt-in gate (INT-A2A-001), 2026-09-25

**Run:** crier-dogfood lane, 2026-09-25 ~20:55–21:30 UTC-5. Target build:
`crier v0.1.0-rc2-61-g4a87ec5-4a87ec58` (HEAD `4a87ec5`, INT-A2A-001 merged
earlier the same day — this is the feature's FIRST real-use evidence; the
non-regression proof shipped with it is in-process tests, not a user drive).

**Angle** (skill rule: change the angle, not the depth — 9 prior runs had done
relay/mesh/inbox/federation/webhook/guard/rate-limiter): the A2A opt-in gate
that shipped today. Promise under test, from `specs/A2A-OPTION.md` §1/§4 and
README env table:

> A2A is an EXTRA, OPT-IN, DEFAULT-OFF. With `CR_A2A_ENABLED` unset, every
> route, response body, auth requirement and storage path is byte-identical to
> pre-A2A crier; an agent participates only if it ALSO carries an optional
> per-agent `a2a` block (`{"enabled":true}`, strict-decoded). This row ships
> the switch and the opt-in only — no A2A route exists under either value yet.

## What I did as a user (multi-endpoint workflow)

Scratch environment, nothing touched in the repo: scratch Postgres from the
README compose recipe (`docker compose up -d postgres`, host port overridden
with the documented `CRIER_PG_HOST_PORT=15449`, `COMPOSE_PROJECT_NAME=dogfood-a2a-crier`),
two scratch DBs, scratch ports 18777/18778, scratch keys, README signing helper
verbatim (`openssl pkeyutl -sign -rawin` over `METHOD\nPATH\nTS`).

1. **Boot with the switch unset** (the posture every existing deployment is in)
   on a fresh Postgres — `GET /version` 200 unauthenticated.
2. **Future surfaces are dark**: `GET /.well-known/agent-card.json` → 404; a
   plausible future JSON-RPC POST → 404. With `CR_A2A_ENABLED=true` on a second
   server: the same paths STILL 404, and `/openapi.json` is md5-identical
   across unset / on / restart postures. Nothing is registered under either
   value — exactly what §5.2 promises.
3. **Opt-in workflow**: `POST /agents` with `"a2a":{"enabled":true}` → 201 with
   `a2a.enabled:true` echoed; a second agent without the block → 201 with NO
   `a2a` key (pre-A2A row shape preserved); `PATCH /agents/{id}` (signed) to
   opt in / clear with `a2a:null` (the three-state rule) / re-opt-in.
4. **Strict decode**: register with `{"a2a":{"enabld":true}}` →
   `400 {"error":"a2a: unknown field \"enabld\" (accepted: enabled)"}` and
   `GET /agents/bad-a2a` → 404: the misnamed opt-in registered NOTHING. A
   wrong-typed member (`"enabled":"yes"`) is refused 400 `invalid json` —
   measured parity with the ill-typed `webhook` member
   (`internal/registry/a2a_optin_test.go:125` documents this intentionally).
5. **Ordinary messaging still works on an opted-in agent** (A2A must not be a
   special lane): deliver → signed retrieve → ack on Postgres, twice
   (switch-off and switch-on servers).
6. **Durability**: SIGTERM stop (`./bin/crier -stop`), restart on the same PG —
   the opt-in survives (migration 005 nullable column), the empty `{}` block
   survives as `{}`, the no-block agent still has no `a2a` key, all 3 agents
   survive, an undelivered message survives, and a PRE-RESTART lease is still
   held after restart (`leased_count=2`, `queue_depth=2` — durable leases per
   DF-CRIER-177).
7. **Negatives on the way** (documented order held): unsigned agent-scoped call
   → 401 trio message; unregistered id → 404 before timestamp/signature checks;
   duplicate register → 409 (see finding DF-CRIER-288 — README never mentions
   this status).

**Result: every promise held.** 39/39 corrected assertions green
(`/tmp/dogfood-a2a/run2.log`, results `results2.jsonl`; raw evidence
`/tmp/dogfood-a2a/`, wiped after the run with the scratch stack).

## What it cost in time (measured, per the perf law)

| Operation (user-visible) | Number | Notes |
|---|---|---|
| Cold start → healthy (fresh PG, migrations apply) | **119 ms** | EPOCHREALTIME-bracketed, `/health` probe |
| Restart → healthy (existing PG, same day) | 118–119 ms (2 samples) | |
| Warm deliver + signed retrieve + ack (via curl) | 55–64 ms (5 iters, mean 60) | 3 curl execs + 2 openssl signing forks per round trip |
| Server-side per-request (its own logs) | mean 45 ms, **p50 5.6 ms**, max 127 ms (n=8) | the mean is skewed by ONE 85 ms request; the 60 ms client wall is curl/openssl process forks, not the server — consistent with the 09-23 finding (server 20–270 µs/handler) |

Nothing here is slow enough that a user would notice. **No PERF row filed** —
a win nobody can feel is not a finding, and filing it would devalue the real
ones. The only number a fresh user feels is time-to-first-success, which for a
registered-and-opted-in agent is minutes, dominated by key generation and
reading, not by the server.

## Friction (what a new user hits)

- **README silence on 409 duplicate register** — the only real friction found;
  filed as DF-CRIER-288.
- The compose `postgres` service creates only the `crier` database; a
  multi-instance user (like this run: two switch postures side by side) needs
  `CREATE DATABASE` inside the container first. Not a defect — the README's
  recipe is one server — but worth a line when the durable-by-default work
  (CR-FEAT-034) touches the compose section.
- Everything else followed the docs verbatim: env-overridable compose port,
  JSON pidfile + `-stop`, the ±30 s signing window, the strict-decode error
  message naming both the offending and the accepted key.

## Verdict

**SHIPPABLE (behaviour).** The A2A option does exactly what its spec promises:
inert by default, additive-only when on, strict at the opt-in boundary, durable
through restart, and invisible to every pre-A2A surface. The install leg could
not run this tick (every bunker host down or broken — DF-CRIER-289), so
installability rests on this same day's earlier fresh-box pass (b0c9afa,
38 s build + signed round-trip on las-bunker-03, agent destroyed).
