# QA-CRIER-56 — evidence

Row: `QA-CRIER-56` — cmd/server `TestMetricsState_SeriesRenderOnIdleBoot` asserts an
absolute 0 on a process-wide alert counter.
Worktree: `/home/kara/worktrees/crier-fix/qa-crier-56` (branch `wt/fix/qa-crier-56`,
base `5dc81b05`). Host: 16 CPUs, go1.26.10 linux/amd64.
Run id: `qa-crier-56-2026-11-06T1255Z`. Files changed: `cmd/server/metrics_state_test.go`
(test-only; no non-test file touched).

## 1. Reproduction (pre-fix)

The failing arm needs the detector's default UTC quiet window (01:00–05:00) to be
active, so the run clock decides whether the sibling trips. The failure was
reproduced at 12:47Z by forcing the same code path the default window takes:
`CR_DETECT_QUIET_HOURS=0-23` makes every delivery fall inside the quiet window
(start 0, end 23 → quiet for all hours except 23:00; `s == e` is the documented
"off" spelling, so 0-24 is not available). No product code was changed for this.

    A  CR_DETECT_QUIET_HOURS=0-23 go test -short -count=1 -cover \
         -run 'TestDetectionCatchesAndContainsACompromisedAgent|TestMetricsState_SeriesRenderOnIdleBoot' \
         ./cmd/server
       → FAIL: metrics_state_test.go:131:
         crier_detect_alerts_total{signal="odd_hour_volume",severity="warning"} = 1, want 0
         — every bounded (signal,severity) pair exists before the first alert
       (byte-identical to the CI signature in run 42139854898, job 127640392221)

    B  same pair, no env override, run at 12:47Z (outside 01:00–05:00 UTC) → PASS
       → the assertion's outcome is a function of the WALL CLOCK, not of the boot

    C  CR_DETECT_QUIET_HOURS=0-23, -run 'TestMetricsState_SeriesRenderOnIdleBoot'
       alone → PASS
       → the increment is not produced by the server the test boots (that boot has
         detection disabled); it is produced by an earlier test in the same binary

## 2. The sibling that increments the counter (AC 2)

`TestDetectionCatchesAndContainsACompromisedAgent` — `cmd/server/crfeat030_test.go:152`,
boots a detection-enabled server and delivers **five messages from the single sender
`compromised`** (fan-out to `peer-1..peer-5`). `internal/detect` accumulates per-sender
quiet-window events and, at ≥ `DefaultQuietMinMessages` (3) inside the quiet window,
raises `odd_hour_volume`/`warning`; `raise()` (`internal/detect/detect.go:576`) increments
the package-level `alertsTotal` counter vec, which lives in `metrics.Default` and is
therefore process-wide — shared by every test in the `cmd/server` binary.

Ordering evidence, from the same command (`-v`, both tests selected alone):

    === RUN   TestDetectionCatchesAndContainsACompromisedAgent
    --- PASS: TestDetectionCatchesAndContainsACompromisedAgent (0.18s)
    === RUN   TestMetricsState_SeriesRenderOnIdleBoot
        metrics_state_test.go:131: ... odd_hour_volume ... = 1, want 0
    --- FAIL: TestMetricsState_SeriesRenderOnIdleBoot (0.03s)

and a temporary untracked probe (`TestZZZAlertCounterProbe`, file sorts last, reads the
exposition after the sibling has run; probe deleted after the run):

    PROBE crier_detect_alerts_total{signal="odd_hour_volume",severity="warning"} = 1
    PROBE crier_detect_alerts_total{signal="fanout_spike",severity="critical"} = 1
    PROBE crier_detect_alerts_total{signal="new_peer_burst",severity="critical"} = 1

That also explains the CI asymmetry (Coverage red, Build & Test green on the same push):
both jobs run the same package but only at the moment one of them crosses into the
01:00–05:00Z window does the sibling's three deliveries trip the signal before the
scrape. The counter's product behaviour is correct; only the test's expectation was not.

## 3. Fix

`cmd/server/metrics_state_test.go` only. The two `crier_detect_alerts_total` pairs are
removed from the absolute-zero table (they are process-wide counters, like the
already-relaxed `crier_webhook_attempts_total` buckets) and asserted on their own
contract instead:

- present on the boot's own scrape (absence is still a failure — the seeded bounded
  pair must render before the first alert);
- not NaN, not negative;
- monotonic: a second scrape of the same boot must not read lower.

The five boot-scoped gauges keep their absolute idle value (`0`): webhook queue depth,
oldest-age, federation held-age, `crier_store_queue_depth_read_errors_total{backend="memory"}`
(sibling failures are labelled `sqlite`/`proxy`, so the memory child cannot be
contaminated) and `crier_detect_quarantined_current` (a gauge over this boot's detector).
A comment at the new block names the sibling, the quiet window and the CI signature.

## 4. Presence contract survives (AC 3 — negative control)

Seeding removed on purpose in `internal/detect` (`init()` no longer calls
`alertsTotal.With(...)`), the fixed test run alone:

    metrics_state_test.go:174: crier_detect_alerts_total{signal="canary_trip",severity="critical"}
      is absent from /metrics — detection is disabled here, but the bounded pair must
      still be seeded by internal/detect
    metrics_state_test.go:174: crier_detect_alerts_total{signal="odd_hour_volume",severity="warning"}
      is absent from /metrics — every bounded (signal,severity) pair must render before
      the first alert
    FAIL

`internal/detect/detect.go` was restored immediately (`git checkout --`) and the tree
carries no change to it. So the relaxation removes only the pristine-value claim.

## 5. Verification

Target test, 10× alone: `go test -short -count=10 -run TestMetricsState_SeriesRenderOnIdleBoot ./cmd/server`
→ 10/10 `--- PASS`.

10× with the contaminator in the same binary and the quiet window forced:

    CR_DETECT_QUIET_HOURS=0-23 go test -short -count=10 \
      -run 'TestDetectionCatchesAndContainsACompromisedAgent|TestMetricsState_SeriesRenderOnIdleBoot' ./cmd/server
    → ok  github.com/crier-dev/crier/cmd/server  1.764s   (10/10)

AC 1 — the CI job's exact command, three consecutive runs
(`go test -short -count=1 -cover -coverprofile=/tmp/criercov.out ./cmd/server`):

| run | loadavg at start | result | wall |
|-----|------------------|--------|------|
| 1 | 18.59 16.05 20.57 | rc=0, ok 45.958s, coverage 80.2% | 53s |
| 2 | 35.81 20.83 21.94 | rc=0, ok 52.740s, coverage 80.2% | 55s |
| 3 | 34.26 23.29 22.74 | rc=0, ok 39.768s, coverage 80.2% | 41s |

Same command three more times with the quiet window forced (the deterministic
emulation of the 01:00–05:00Z window that reded CI; the pre-fix tree fails this
every time):

| run | loadavg at start | result |
|-----|------------------|--------|
| 1 | 22.71 21.76 22.24 | rc=0, ok 35.996s |
| 2 | 17.65 20.50 21.79 | rc=0, ok 34.985s |
| 3 | 17.99 20.25 21.66 | rc=0, ok 33.950s |

Full suite: `go test ./... -count=1` at loadavg 14.83 → rc=0, **40 packages ok, 0 FAIL**.
`go vet ./...` (= `make lint`) → rc=0. `gofmt -l cmd/server/metrics_state_test.go` → empty.

## 6. Not satisfied by this worker (AC 4)

AC 4 asks for three consecutive pushes to `main` whose `CI / Coverage (Go 1.26.10)` job is
green, with run ids and head SHAs. A worker worktree cannot push to `main`; that arm
belongs to the foreman after the merge (three pushes, quoting run ids and heads). Local
evidence for it is the six green coverage runs above, three of them under the exact
contamination condition that reded run 42139854898.
