# 2026-10-20c — crier real-consumer workflow (run 3, same tick, third angle)

Runs 1 and 2 of this tick stayed on the repo's **board and its gate**. This run
drives the **product**: a scratch consumer built on the shipped first-party client
(`clients/python/crier_client.py`, stdlib only), against a freshly built HEAD server
with the documented durable configuration and signature enforcement ON.

- Build: `bin/crier v0.1.0-rc2-654-gedfec90-dirty-edfec909` (`make build`, 1.14 s wall warm).
- Server: `CR_REGISTRY_BACKEND=sqlite CR_REGISTRY_SQLITE_PATH=…/registry.sqlite`,
  `CR_SESSION_BACKEND=sqlite CR_SESSION_SQLITE_PATH=…/sessions.sqlite`, port 18941,
  `/status` → `registry_backend=sqlite`, `registry_durable=true`,
  `require_agent_signature=true`, `guard_enabled=true`.
- Identities: three `crier keygen` keypairs (the documented no-openssl, no-xxd path).
- Consumer: `/tmp/crier-consumer/queue_demo.py` + `after_restart.py` + `idem_probe.py`,
  outside the repo, importing the shipped client (`sys.path` → `clients/python`).

## 1. The workflow, as a user would run it

A scheduler dispatches solver jobs to a **capability pool**; two workers advertise
`solver`; one claims and acks its job and posts the result back into the
scheduler's inbox correlated by `reply_to`; the scheduler is woken by a long-poll.
Every step below is this run's own measurement, warm, on one host:

| step | observed | ms |
|---|---|---|
| `register` scheduler / worker-a (`caps=[solver]`) / worker-b | `201` ×3 | 6.7 / 15.5 / 6.7 |
| `POST /capabilities/solver/inbox` (1st) | `201`, `target=worker-a`, `guard_decision=allow` | 7.0 |
| `POST /capabilities/solver/inbox` (2nd) | `201`, `target=worker-b` | 15.0 |
| signed `GET /agents/worker-a/inbox` (lease) | `200`, 1 message, payload decoded | 7.1 |
| signed ack | `200` | 14.5 |
| signed `GET /agents/…/inbox/stats` | `queue_depth=0, leased_count=0` | 1.2 |
| deliver result → scheduler with `reply_to` | `201` | 5.7 |
| deliver while a `wait_seconds=20` read is parked | `201` | 71.6* |
| **the parked read woke** | 1 message, `6.8 ms` after the delivery | — |
| graceful `-stop` via pidfile | `pid stopped`, port released | 120 |

\* the 71.6 ms is the first delivery after a 2 s idle gap (page-cache/GC warm-up on
that path), not a steady-state cost: the same call is 5.7 ms immediately before it.

Pool rotation matched the documented rule exactly — round-robin, agent-id
ascending, one holder per delivery, and the accept names the holder
(`"capability":"solver","target":"worker-a"`).

## 2. The refusal ladder — every arm named, none silent

| probe | result |
|---|---|
| `GET` own inbox **unsigned** | `401 {"error":"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"}` |
| deliver to an unknown agent | `404 {"error":"agent not found: \"nobody-here\""}` |
| deliver to an unknown capability | `404 {"error":"NO_CAPABLE_AGENT","capability":"does-not-exist","detail":"no agent registered on this relay advertises capability …"}` |
| worker-b reads worker-a's inbox **with its own valid key** | `403 {"error":"agent \"worker-b\" may only access its own resources (target \"worker-a\")"}` |
| hand-built signature with a **300 s-stale** timestamp | `401 {"error":"request timestamp outside allowed window (±30s)"}` |

The panel named `NO_CAPABLE_AGENT` as the refusal a sender sees for an empty pool,
and the client surfaces the server's own text — no client-side reinterpretation.

## 3. Durability across a real restart (sqlite)

`-stop` → restart → the same store:

- boot → `/health` ready in **4 ms** (cold-ish, sqlite, 3 agents).
- `/status` after restart: `registry_backend=sqlite`, `agents_total=3` — identities survived with **no re-registration**.
- a message delivered before the restart but never retrieved was **still there** with the same id, and a second pool message (delivered to worker-b, unclaimed) survived too.
- after `ack`, a follow-up retrieve returned 0 — **ack durability holds**.
- pool rotation after restart started over at `worker-a, worker-b` — matches the
  documented "the cursor lives in the serving process and is not persisted".

## 4. Idempotency: a four-leg discrimination (and why it is NOT a new defect)

A post-restart retry in the workflow above stored a **second** copy of a message
instead of replaying the first accept. Rather than file that on sight, the probe
bisected it one variable at a time:

| leg | setup | result |
|---|---|---|
| (a) | two deliveries, same key, back to back, nothing retrieved | **same id**, `idempotent_replay=true` ✓ |
| (b) | retrieve + **ack**, then the same key again (same process) | **same id**, `idempotent_replay=true` ✓ |
| (c) | deliver, **restart**, deliver again with the original still unacked | **new id**, no replay flag — inbox then holds BOTH copies ✗ |
| (d) | control: a never-used key after the restart | one new message, no replay flag ✓ (the probe cannot manufacture replays) |

So the missing guarantee is bound to the **restart**, not to the ack and not to the
key scope. That is the documented limit: README's `CR_IDEMPOTENCY_WINDOW_S` row says
the window "is a per-relay, in-memory retry window, not a durable ledger — a restart
closes it early, and the message the retry would have duplicated is still in the
target's inbox". The failure class is already tracked as **DF-CRIER-357** (complete,
"the delivery idempotency window is process-local: a chat retry after a crier restart
re-runs the real agent"), whose premise this run re-proves end to end on the durable
backend. **No new defect; no re-file.** The residue is docs-only, and is filed as
DF-CRIER-435: §4's feature prose and `TESTERS.md`'s replay transcript both describe
the 24 h window without the restart bound that the env-var table states — the one
sentence an integrator needs before building a retry path.

## 5. Performance — measured, and no PERF row is owed

Nothing in this run produced a wait a user would notice: every interactive step is
single-digit-to-low-double-digit milliseconds, the long-poll resolves 6.8 ms after
the wake-up delivery, the durable restart is instant, and `-stop` is 120 ms. The
inline LLM guard is enabled on this run and did not add a felt cost (deliveries
5–15 ms). Local `make build` is 1.14 s warm. **No PERF-<n> row** — there is no
number here that would justify a profile.

## 6. Verdict

- **Does it work?** Yes — the documented multi-agent workflow ran end to end on the
  shipped client, first try: pool routing, lease/ack, correlated reply, long-poll wake.
- **Is it useful?** This is the thing crier is for, and the client makes it a
  ~10-line program with no dependency beyond the stdlib.
- **Is it usable?** Yes: `crier keygen` removes the openssl/xxd ceremony, and every
  refusal above is a named, machine-readable body.
- **Is it trustworthy?** Durable identities, durable messages, durable acks — with
  one documented boundary (the process-local idempotency window) that the feature
  prose does not state.
- **Is it fast enough?** Numbers in §5; nothing user-noticeable.

Artifacts: this report. Board row: DF-CRIER-435 (P4, docs). Re-proof recorded for
DF-CRIER-357. Scratch consumer, keys and store removed after the run.
