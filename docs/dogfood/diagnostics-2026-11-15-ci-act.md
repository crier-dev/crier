# Diagnostics — the clean-agent act failure for `go test ./internal/ci`

Tick: `crier-dogfood-2026-11-15-15-54-49` (second fire, 2026-11-15T17:2xZ / 12:2x EST)
Subject row: **DF-CRIER-497-R2** (pending) — act CI simulation on a fresh clean agent
fails at `Agent-ecosystem registration contract` (`go test -count=1 ./internal/ci`,
30.27 s, exit 1) with zero step stdout.
Repo state during this fire: `HEAD = a065ad2a434e779fee1d5e16fe153a9ea0dce1fd`,
identical to `origin/main`, working tree clean.

This fire did not re-walk the product leg (fire #1 at 10:54 EST already did) and did not
re-run the bunker install leg. It attacked the one thing fire #1 left open: the row's
attribution was a stated, explicitly untested hypothesis. It is now tested.

## What was recorded (agent-side, unrecoverable)

Harness rows that survived (`.coding-hermes/evidence/crier-dogfood-2026-11-15-15-54-49/`):

    launch   OK    16:15:19Z  agent=765c30b7 server=bunker-las-03 ttl=4h cpu=4.0
    collect  FAIL  16:42:33Z  "cannot reach agent=765c30b7 on bunker-las-03
                               (ssh probe returned no state) — agent left intact"  273 ms
    harness-failure INFO      abort after one failed collect

From DF-CRIER-497-R2, the act trace fire #1 read off the agent's `ci.log`:

    'Build & Test' -> Set up Go 1.26.10 ok
                   -> 'Toolchain floor' SUCCESS (798 ms)
                   -> 'Agent-ecosystem registration contract' FAILURE 30.27 s
                   -> exitcode 1
    ci.log lines 44-47 jump from "docker exec bash -e /var/run/act/workflow/3"
    straight to "Failure": NO step stdout at all.

Fire #1's stated hypothesis: the act image lacks a tool (`iproute2`/`ip` or a PATH stub)
that `internal/ci`'s bash fixtures need (`internal/ci/chaos_disconnect_test.go:31,50,90,126`
run `exec.Command("bash", script, ...)` with PATH-injected stubs), so a test fails instead
of skipping.

## What this fire measured

Reproduction harness: `git archive` of the recorded tree, one-job workflow replaying the
exact step chain — `actions/checkout@v5` (with token input) → `actions/setup-go@v6`
(1.26.10) → `run: go test -count=1 ./internal/ci`, `GOTOOLCHAIN=local`, act 0.2.89,
act's default image `node:22-bookworm`, network `host`.
Artifacts: `act-repro.workflow.yml`, `act-ci-online.out`, `act-ci-offline.out`,
`act-ci-warmcache-mem2g.out` in this tick's evidence directory.

| condition | result | duration | output |
|---|---|---|---|
| cold module cache, normal network | **PASS** (`ok .../internal/ci 0.057s`) | 19.51 s step, of which ~19.4 s is `go: downloading` of ~20 modules | full stream captured |
| warm actions cache, `--memory 2g` | **PASS** (`0.054s`) | 0.85 s step | full stream captured |
| `--container-options "--network none"` | **FAIL** `dial tcp: lookup proxy.golang.org on 8.8.4.4:53: network is unreachable` | **0.127 s** | error captured in full |

## Findings (each one is evidence, none is a code edit)

1. **The tree is not at fault, and this is now reproduced, not merely asserted.**
   The exact package the clean agent failed passes in the real act runtime at the recorded
   commit (0.057 s), and passes again with a 2 GiB memory cap. This matches hosted CI green
   at the dogfood pin and the 0.143 s native pass fire #1 already had.

2. **30.27 s is a cold-cache signature, not a hang.** A cold act container spends ~19.5 s
   downloading and compiling ~20 modules before the test's own 0.057 s; with a warm actions
   cache the same step is 0.85 s. The recorded 30.27 s therefore says nothing about the test
   — it is the cost of a first build in a fresh container on that host. No user feels this
   number; it is CI-side only, so no PERF row is warranted for it.

3. **The "missing bash tool" hypothesis is refuted for this step.** A tool or network gap
   does not produce silence: the no-egress run failed in 0.127 s and act printed the exact
   `dial tcp ... network is unreachable` error into the stream. The whole package — bash
   fixtures included — passes in the same image. What fire #1 recorded is therefore not the
   shape of a missing-tool failure.

4. **The remaining anomaly is the silence itself, and it is a capture defect, not an act
   behaviour.** act surfaces step output on the success path and on the failure path in
   every run above. A `ci.log` that jumps from the `docker exec` line to `Failure` lost
   those bytes between act and the file: the harness's own log writer. This is exactly
   DF-CRIER-497-R2 criterion 1 and it is fixable without a re-run of the whole leg
   (`act` output must be teed whole, not filtered).

5. **The evidence that would have named the agent-side cause no longer exists.**
   `collect` fail-closed in 273 ms and the harness aborted after a single attempt; the
   26-cell ledger and `qa-logs-286493/ci.log` were harvested by hand and never committed;
   `bunker list` on bunker-las-03 now shows one unrelated agent (3c9c4402, created 11:09 EST)
   and 765c30b7 is gone. Filed as **QA-CRIER-066-R2** (recurrence of QA-CRIER-066: evidence
   referenced by a board row but surviving nowhere).

## Honest limits

- The exact act image and platform the clean agent used cannot be recovered with the log;
  this host's act default (`node:22-bookworm`) is a close but not certified substitute.
- I did not obtain the failing assertion, and I am not claiming the cell should be graded
  green: I am claiming the cell carries no information about crier, which is the same
  conclusion fire #1 reached, now with the hypothesis eliminated rather than open.
- Next fire, if the question is still worth an hour: rerun the same replay with
  `--container-options "--network none --memory 2g"` on a host whose act image matches the
  agent's, and tee act's raw output — that isolates capture from runtime in one run.

## Perf leg (Step 2b)

No new user-facing slowness. The headline numbers for this tick are fire #1's (live
dashboard warm p50 6.5 ms; degraded renders 2.5 s — already carried by QA-CRIER-7), and
they were not re-walked. The only numbers added here are CI-side (19.5 s cold / 0.85 s warm
/ 0.057 s test), which no user feels. Filing a PERF row for them would devalue the real ones.
