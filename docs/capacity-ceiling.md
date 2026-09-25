# The capacity ceiling, measured (CR-FEAT-033)

The external hands-on review *DISPATCH · CRI-001* (Carter, via Bane, 2026-09-25,
tested live at `ca28523d` on 2026-09-24) measured two real numbers — register 100
agents 0.19 s, deliver 500 inbox messages 0.95 s (≈ 529 msgs/s) on one box — and
then wrote the honest sentence: *"I did not run a 1000-agent soak; treat the
ceiling as unproven."* It also named the structural bottlenecks: one relay process
is the whole throughput (no sharding, no read replicas), the mesh is a full fan of
live WebSockets per agent, there are no namespaces, and the 100/min/agent publish
cap is the ONLY backpressure.

This page answers that with numbers instead of a claim. Everything below was
produced by `scripts/load-soak.py` (the bounded, repeatable soak) driving a real
`crier` server on a scratch port, on one host, in one run — and every figure
printed here is anchored in `docs/claims.yaml` to the artifact that run wrote, so
this page cannot drift from the measurement without failing `make docs-check`
(the CR-GAP-065 precedent). The artifact carries more than this page transcribes
(per-stage latency percentiles, resident memory, peer-table settle times); it is
`docs/soak-baseline.json`.

## The headline

One relay process, one box, **1000 registered agents with 1000 live mesh
WebSockets**, 20 000 relay fan-out frames delivered with zero errors, and the
100/min cap observed exactly where the review said the only backpressure lives.

| registered agents | registrations | inbox deliveries | relay WebSocket fan-out | live mesh sockets | inbox ping p95 |
| --- | --- | --- | --- | --- | --- |
| 2 | 2 agents registered in 0.002 s | 3197 inbox deliveries/s | 10932 relay fan-out frames/s | 2 live mesh sockets | inbox ping p95 0.65 ms |
| 10 | 10 agents registered in 0.007 s | 3254 inbox deliveries/s | 31630 relay fan-out frames/s | 10 live mesh sockets | inbox ping p95 1.54 ms |
| 100 | 100 agents registered in 0.026 s | 3317 inbox deliveries/s | 88318 relay fan-out frames/s | 100 live mesh sockets | inbox ping p95 3.31 ms |
| 1000 | 1000 agents registered in 0.308 s | 3638 inbox deliveries/s | 239133 relay fan-out frames/s | 1000 live mesh sockets | inbox ping p95 0.88 ms |

Each profile got a **fresh server process** (cold start, cold registry), so the
agent counts do not accumulate and the 1000-agent row is a real 1000-agent run,
not an extrapolation. The run happened **measured at commit 6628dec**, binary
**binary sha256 f9007e47f634**, **at a 1-minute loadavg of 19.48** **on a 16-CPU
host** shared with the rest of the fleet — housekeeping runs alongside it. That
condition is part of the result: it is not a tuned, idle-machine benchmark, and
the harness records the loadavg it measured so a reader can compare their own.

Every profile also ran the shipped rate-limit probe: 110 publishes from ONE
agent id against the default cap, which accepted exactly **100 relay publishes
accepted** and rejected the remaining ten with HTTP 429. That is the review's
"the 100/min cap is the ONLY backpressure" statement, measured: in this
configuration nothing else refused a request, at any agent count.

### The reviewer's method, replayed

The review's 529 msgs/s and this page's delivery figures are the same endpoint
measured by **different clients**, so they are not in conflict — they are both
true. Replaying the reviewer's shape (one client, one connection, one message at
a time) on the largest profile measures **2519 inbox deliveries/s from one
sequential client**, against 3638 inbox deliveries/s from the harness's four
keep-alive workers. The server path is the same; the difference is how the
client drives it:

| how the deliveries were driven (1000 agents) | measured |
| --- | --- |
| one client, one connection, sequential | 2519 inbox deliveries/s from one sequential client |
| four keep-alive workers, round-robin | 3638 inbox deliveries/s |

We did **not** reproduce 529 msgs/s exactly (see the gaps below), so the
remainder of that gap is unattributed rather than explained away.

## What the numbers support — and what they do not

Supported by this run:

- **The ceiling is at least 1000 agents on ONE relay process.** 1000
  registrations, 1000 inbox deliveries, 1000 live mesh sockets and 20 000
  fan-out frames all completed with no errors, no refused request and no dropped
  connection. No cliff appeared between 2 and 1000 agents: the delivery rate held
  between 3197 and 3638 inbox deliveries/s across four orders of magnitude of
  agent count, and the mesh peer table listed exactly the sockets opened.
- **The WebSocket fan is real and it is the scaling axis.** Throughput on the
  fan grew with the fan (31630 → 239133 relay fan-out frames/s from 10 to 1000
  subscribers), and the 1000-socket fan cost the server tens of megabytes of
  resident memory (exact figures in the artifact, alongside the peer-table settle
  time at 1000 sockets). That is the review's "full fan of live WebSockets per
  agent" made concrete: every additional agent is an additional long-lived socket
  on the one process.
- **The delivered-to-socket path works at fan scale.** Each mesh socket in the
  run opted into the CR-FEAT-023 inbox ping; every sampled delivery was answered
  on the live socket, in under two milliseconds p95 at every profile.

Not supported by this run — do **not** read these numbers as:

- **a saturated-host figure.** The repo's own load rule (DF-CRIER-254) caps a
  burner run at 8 workers, and this box has 16 CPUs: the sanctioned tooling
  cannot saturate the host the soak runs on. `make load-soak-under-load` does run
  the soak under burners, and it is published below precisely because it
  demonstrates how little a sub-second measurement window overlaps a sanctioned
  burn — not because it is a contended-host ceiling.
- **a multi-relay, sharded or replicated figure.** The soak drives ONE relay
  process, which is what crier ships today. Sharding, read replicas and
  namespaces do not exist here, so there is nothing to measure; the single-process
  result is a ceiling on the whole system as shipped, not on one component of a
  bigger one.
- **a PostgreSQL figure.** Every number here is the in-memory registry backend
  (the default when no database URL is set). The durable Postgres path is not
  measured.
- **a webhook or guard figure.** No soak agent carries a webhook, so the measured
  delivery path is the durable inbox accept (HTTP 201), not the push transport;
  and no agent carries an LLM message-guard policy, so no guard decision is in
  these numbers.
- **a long-running or leak figure.** Each profile is a seconds-long measurement,
  capped at a 300 s wall-clock budget. TTL expiry sweeps, the periodic purge
  cycle, and anything that only shows up after hours are outside this
  measurement.
- **a network or TLS figure.** Everything is loopback HTTP; a deployment adds a
  proxy and TLS hops that this harness does not simulate.

## The sub-second window, and why it is a gap worth naming

`scripts/load-repro.sh` composes with the soak directly (its burners run
alongside the target), so the contended condition is one command. Running it that
way — 4 CPU burners under the repo's own bounds, soak on a 10-agent profile —
produced these numbers **measured at commit 6628dec**, **binary sha256
f9007e47f634**, **at a 1-minute loadavg of 23.44** **on a 16-CPU host**:

| under 4 CPU burners (10 agents) | measured |
| --- | --- |
| registrations | 10 agents registered in 0.004 s |
| inbox deliveries | 4259 inbox deliveries/s |
| inbox deliveries, sequential replay | 3794 inbox deliveries/s from one sequential client |
| relay fan-out | 35408 relay fan-out frames/s |
| live mesh sockets | 10 live mesh sockets |
| inbox ping p95 | inbox ping p95 0.46 ms |
| rate cap | 100 relay publishes accepted |

Those figures are not lower than the unloaded row at the same agent count, and
that is the finding: the soak's whole measurement window is under a second, so
four burners ramping on 16 CPUs barely overlap it. The honest reading is that
**the contended-host ceiling is still unmeasured** — this artifact documents the
attempt and the reason it does not constitute one (8 sanctioned burners on 16
CPUs cannot saturate the box, and a sub-second workload does not feel a ramping
burner set). A saturated-host figure needs either a dedicated host or a longer
steady-state workload per profile; both are named here and neither is claimed.

## How to reproduce any of this

```bash
make build                 # the soak measures a binary you built; it never builds for you
make load-soak             # CI-sized smoke: 2 agents, prints the numbers
make load-soak-baseline    # the published profile set (2, 10, 100, 1000) -> docs/soak-baseline.json
make load-soak-under-load  # the same soak with scripts/load-repro.sh burners alongside it
make load-soak-selftest    # proves the bounds, the gate, the teardown and the verification
```

`make load-soak-baseline` honours the repo's load gate (a 1-minute loadavg above
8.0 skips the run with exit 3 and starts no server). The published baseline was
measured on this busy host with the gate raised — `make load-soak-baseline
LOAD_SOAK_THRESHOLD=20` — and the artifact records both the threshold and the
loadavg it measured, so a reader can see the condition rather than assume one.

The harness is bounded by construction, and that is measured rather than
promised (`make load-soak-selftest`, a CI step): agents, deliveries, events,
subscribers, mesh sockets and workers each have a hard cap that is **refused**,
never clamped; the wall-clock budget per profile is capped at the same 300 s
ceiling `scripts/loadgen.py` enforces and an exhausted budget stops the run and
reports `truncated: true` (exit 4) instead of continuing; the load-average gate
is `scripts/loadgen.py`'s own (one implementation, not a copy) and it skips
before any server is started; and the server the soak starts is owned — spawned
with the kernel parent-death signal, torn down TERM → wait → KILL → VERIFY, with
a survivor outranking every other result. The selftest also proves a NEUTER
proof: a copy of the harness whose cap verdict is forced to a no-op accepts the
request the real harness refuses, so the refusal is that check's.

## Regenerating the published numbers

`make load-soak-baseline` rewrites `docs/soak-baseline.json`. The numbers in this
page are pinned to that artifact:

1. re-run the harness (the artifact now reports new measurements),
2. copy the run's claim strings from its output (the harness prints exactly the
   strings this page prints, one per claim id),
3. update the matching `quote`/`expect` pairs in `docs/claims.yaml`.

Until step 3 happens, `make docs-check` FAILS — that is the point: a measured
number is only allowed to change when the measurement that produced it changed.

The committed baseline artifact was written by the harness in this change; its
recorded config block predates two later additions to the record's shape (the
`claim_prefix` key and the per-run peer-settle fields), neither of which changes
a measured path. Rules and caps quoted on this page are asserted by
`scripts/load-soak-selftest.sh` (a CI step), not by `docs/claims.yaml` — the
claims file anchors measurements.
