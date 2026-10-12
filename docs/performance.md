# Session API performance — SQLite baseline

**Status:** measured (DF-CRIER-308) · **Scope:** the `CR_SESSION_BACKEND=sqlite`
session API surface · **Recorded artefact:** `docs/session-bench-baseline.txt`

The session API (CR-CHAT-019) is an always-on surface, and the SQLite adapter is
the "a user does not have to run PostgreSQL" path — so its latency is a product
property, not an internal detail. Nothing in the repo measured it before this
tick. This file is the baseline: what was measured, what it came out as, why,
and what the numbers say to do about it.

The DF-CRIER-308 target is **100 ms p99 with the SQLite backend under moderate
load**, where "moderate load" is 100 concurrent requests. The short verdict is
that the target holds for the **reads taken one at a time** (retrieve p99
~3 ms, list p99 ~20 ms) and does not hold for **any endpoint at 100-way
concurrency**, for three measured reasons named under *Analysis* — none of which
is the HTTP layer.

---

## Reproducing

```bash
scripts/benchmark-sessions.sh                # record the baseline
scripts/benchmark-sessions.sh --check        # ... and exit 1 if a cell is OVER
scripts/benchmark-sessions.sh --quick        # short rounds (smoke, not publishable)
scripts/benchmark-sessions.sh --require-idle # refuse (exit 3) on a busy host

go test -bench=BenchmarkSession -benchtime=2s ./internal/session/   # or drive it directly
```

The recorder writes `docs/session-bench-baseline.txt`: the host context it was
taken under (uname, CPU count, 1-minute loadavg, `/proc/pressure/io`, `go
version`, the p99 budget), the raw `go test -bench` output of every phase, and a
summary table of the last round per `(endpoint, mode)` cell.

Knobs: `CR_SESSION_P99_BUDGET_MS` (the verdict ceiling, default 100),
`CR_SESSION_ASSERT_P99=1` (turn the verdict into a failed run),
`CR_SESSION_BENCH_{SEQ_TIME,READ_REQUESTS,CREATE_REQUESTS,COMMIT_REQUESTS}` (round
sizes).

### What is actually measured

`internal/session/bench_test.go` drives the **real route set**
(`registerSessionRoutes`), the **real handler** (`NewHTTPHandler`) and the
**real adapter** (`NewSQLiteStore` — pure-Go `modernc.org/sqlite`, one pooled
connection, the `sqliteDialect` pragmas) over an `httptest` server, so HTTP
framing and routing are inside the measurement. The fixture is sealed before the
timer starts: `benchSeedSessions` (20) sessions, each with one agent member and
`benchSeedMessages` (20) messages, written through the `Repository` the handler
serves from. An empty store would measure the empty-store path and prove
nothing.

Two shapes are measured, and the summary never merges them:

| mode | what it is |
| --- | --- |
| `seq` | one request at a time — the endpoint's own service time, no queue in front of it |
| `conc100` | 100 requests in flight (`benchClients`) — the DF-CRIER-308 "moderate load" |

The write path is measured sequentially on purpose. `NewSQLiteStore` pins
`SetMaxOpenConns(1)` (SQLite admits a single writer, and one pooled connection is
what keeps the per-connection pragmas in force), so concurrent writers queue on
one connection by design and a concurrent-create p99 measures the queue, not the
write. `BenchmarkSessionCreateP99` reports that queue honestly; it is not the
number to regress against.

---

## The recorded baseline

Taken on the tick host at loadavg `10.15 15.21 17.00`, 16 CPUs, `/proc/pressure/io`
`some avg10=10.46 avg60=11.96`, `go1.26.6`. Round sizes: sequential
`-benchtime=2s`, reads `2000x`, creates `300x`, commit cost `100x`. This host is
shared with the rest of the fleet, so absolute numbers move with the neighbours
(see *Variance* below); the *ratios* are stable and are what the analysis is
built on.

```
  ENDPOINT          MODE     n      p50_ms     p95_ms     p99_ms     max_ms     VERDICT
  ----------------- -------- ------ ---------- ---------- ---------- ---------- -------
  commit-rollback   seq      100    18.508     41.883     47.836     111.446    PASS
  commit-wal        seq      100    0.040      0.068      0.085      0.085      PASS
  create            conc100  300    11742.192  18613.378  22890.001  26652.125  OVER
  create            seq      33     77.263     130.737    130.913    130.913    OVER
  list              conc100  1000   2159.450   2514.133   2667.695   2858.185   OVER
  list              seq      160    13.909     17.649     20.312     23.446     PASS
  retrieve          conc100  1000   112.406    168.188    190.217    222.772    OVER
  retrieve          seq      1378   1.285      2.179      3.267      5.579      PASS

  p99 budget 100ms: 4 endpoint cell(s) OVER
```

`commit-*` is not an endpoint: it is the floor under every write, measured in the
same package (`BenchmarkSessionStoreCommitCost`) as the two statements
`SQLStore.Append` issues for one record — the projection `INSERT` plus the
`chat_seq` upsert, i.e. two implicit transactions. `commit-rollback` is the store
**as it ships today**; `commit-wal` is the same write with
`PRAGMA journal_mode=WAL`, measured so the recommendation below is a measurement
rather than a claim.

### Against the target

| endpoint | `seq` p99 | `conc100` p99 | 100 ms p99 target |
| --- | --- | --- | --- |
| `GET /sessions/{id}/messages` (retrieve) | 3.3 ms | 190.2 ms | met sequentially, missed under load |
| `GET /sessions` (list) | 20.3 ms | 2667.7 ms | met sequentially, missed under load |
| `POST /sessions` (create) | 130.9 ms | 22.9 s | missed |

The reads are comfortably fast one at a time — 5x to 30x inside the budget. At
100-way concurrency nothing is, and a create is three orders of magnitude out.

---

## Analysis

Three independent, measured causes. All three must be addressed for the target to
be reachable; each is separately actionable.

### 1. A durable write costs 15–30 ms with the shipped pragmas

One record write (two statements, two implicit transactions) is **p50 18.5 ms,
p95 41.9 ms, p99 47.8 ms** with `sqliteDialect.InitStatements()` — `busy_timeout`,
`foreign_keys`, `synchronous=NORMAL` — and the **default rollback journal**. The
same write with `journal_mode=WAL` is **p50 0.040 ms** (~460x at p50, ~560x at
p99).

This is not fsync latency on the measurement host: the same rollback-journal cost
reproduces on `tmpfs` (where `fsync` is a no-op), and `synchronous=OFF` in
rollback mode drops it to ~0.08 ms. The cost lives in the rollback journal's
per-commit path, and it dominates every write endpoint:

```
POST /sessions                    ≈ 5-6 record writes (create + 2 members + seq)
create, seq, p50                  = 77.3 ms       (measured)
cell service time under WAL       ≈ 2-5 ms        (5-6 × 0.04 ms + the reads below)
```

`sqliteDialect.InitStatements` deliberately does not set `journal_mode` — it
returns a row, so it cannot ride a list of row-less pragmas, and the comment
records "the default rollback journal is the most portable choice". The
measurement says what that choice costs.

### 2. One pooled connection makes p99 a queue, not a latency

`SetMaxOpenConns(1)` serialises every request. Under Little's law the 100th
concurrent request waits ≈ 100 × service time, which is exactly what the numbers
show: retrieve is ~1.3 ms alone and ~112 ms at `conc100`; list is ~14 ms alone
and ~2.2 s at `conc100`. Concurrency cannot be absorbed by the store, so a
100-concurrent p99 target is a statement about **service time**, not about
scaling: to stay under 100 ms at 100 in flight, every endpoint must serve in
~1 ms or less, or the pool must admit more than one reader.

### 3. `GET /sessions` is O(N) loads

`HandleListSessions` calls `store.Sessions()` and then `store.Load()` **per
session** (it needs each session's state, counts and preview for the
`sessionView`). At the fixture's 20 sessions that is 21 round trips and ~14 ms;
the cost grows linearly with the number of sessions held, so a deployment with
200 sessions pays ~140 ms for a list, alone, before any concurrency.

---

## Remediation (ranked by measured impact)

1. **`journal_mode=WAL` on the SQLite adapter** — the store's single biggest
   lever: one record write 18.5 ms → 0.040 ms (p50), and `POST /sessions` from
   ~77 ms to low single-digit milliseconds. It must be applied where it can
   return a row (a `QueryRow` at open, not `InitStatements`), and it needs a
   fallback for a database that cannot do WAL (`:memory:`, a network filesystem),
   since the portable-rollback-journal comment is a real constraint.
2. **Do one transaction per request, not one per record.** `POST /sessions`
   currently issues 5–6 separate commits (`SQLStore.Append` = projection +
   `chat_seq` upsert, called once per record). Batching the create's records into
   one transaction cuts the commit count — and with it the wall time — by that
   factor *on top of* any journal-mode change.
3. **One aggregate query for the list.** Replace the per-session `Load` in
   `HandleListSessions` with a single grouped read over the view tables (counts,
   last message, preview), so `GET /sessions` stops scaling with session count.
4. **Widen the read pool once WAL is on.** Multiple readers are safe under WAL;
   keeping one connection is only required today because the per-connection
   pragmas must not be reset by a fresh pooled connection.

Items 1–4 are production changes to the store and the handler and are **not**
made by this task: DF-CRIER-308 is a benchmarking/documentation row, and the
baseline's job is to name the cost and size the fix, not to ship it. Item 1 alone
is a one-line change with a measured ~400x effect on the write path and should be
the first thing a follow-up row takes.

---

## Caveats

- **Variance.** This host is shared with the rest of the fleet; the recorded
  header carries the loadavg and IO-pressure counters for exactly this reason.
  Across runs of the same code, `create seq` p50 moved between 77 ms and 163 ms
  and `commit-rollback` p50 between 15.5 ms and 29.3 ms. Read the ratios, and
  re-record (`scripts/benchmark-sessions.sh`) before quoting an absolute number.
- **Verdict is a recorder by default.** `scripts/benchmark-sessions.sh` prints
  PASS/OVER per cell and exits 0 having recorded them — except a run that
  records no cell at all, which exits 1: a `-bench` pattern that matches nothing,
  a build failure and a panic are indistinguishable at that point, and exiting 0
  there would be a vacuous green. `--check` makes an OVER cell an exit 1, and
  `CR_SESSION_ASSERT_P99=1` does the same inside `go test`. Neither is wired
  into CI: a wall-clock p99 gate on a shared runner would fail on the
  neighbours, not on a regression.
- **The fixture is fixed on purpose.** 20 sessions x 20 messages is a small
  deployment. Both slow paths (`list`, `create`) scale with data and load that a
  different fixture would move; the numbers above should be read as a baseline
  point, not a ceiling.
- **Covered:** the six session routes' read/write core — create, retrieve
  (transcript), list, and the store's durable-write floor. Untouched by design:
  the PostgreSQL and JSONL backends, fan-out delivery through the registry
  inboxes, the thread/compile/task/audit routes, and any multi-process or
  multi-host topology. A store-level baseline is not a federation baseline.
