#!/usr/bin/env bash
#
# scripts/benchmark-sessions.sh — record the SQLite session API performance
# baseline (DF-CRIER-308).
#
# WHY THIS EXISTS
# ---------------
# The session API (CR-CHAT-019) is a shipped, always-on surface, and the SQLite
# adapter is the "a user does not have to run PostgreSQL" path — so its latency
# is a product property, not an internal detail. Nothing in the repo measured
# it: the only benchmarks that existed covered the relay and the memory
# registry. A performance claim without a recorded baseline is a guess, and a
# guess cannot be regressed against.
#
# This script runs the benchmark in internal/session/bench_test.go (the REAL
# route set over the REAL handler over the REAL sqlite store), records the raw
# output plus the host context it was taken under, and prints a per-endpoint
# p50/p95/p99 table with a PASS/OVER verdict against the DF-CRIER-308 target
# (100ms p99). The recorded artefact is docs/session-bench-baseline.txt and the
# analysis is docs/performance.md.
#
# WHAT IT DOES NOT DO
# -------------------
# It starts no server, binds no port, writes no config and installs nothing —
# the whole measurement is in-process (httptest) inside `go test`. It therefore
# has no server lifecycle to clean up and no EXIT trap to owe.
#
# LOAD POSTURE
# ------------
# The benchmark's concurrent cells keep at most 100 requests in flight against
# a store pinned to ONE sqlite connection, so the server-side work is a few
# percent of a core; the whole run is bounded by the -benchtime arguments and
# creates no background process. It is therefore NOT a burn loop and does not
# take the repo's load-repro gate — but a p99 taken on a busy host measures the
# host, so the observed 1-minute loadavg and the IO pressure stall counters are
# written into the header of every recording, and --require-idle turns the
# observation into a refusal (exit 3) for an operator who needs a quiet
# baseline.
#
# USAGE
#   scripts/benchmark-sessions.sh [--quick] [--check] [--require-idle]
#
#   --quick         shorter rounds (a smoke baseline, not a publishable one)
#   --check         exit 1 when any endpoint's p99 is OVER the budget
#   --require-idle  exit 3 before measuring when the 1-minute loadavg is at or
#                   above CR_SESSION_BENCH_LOAD_THRESHOLD (default 8.0)
#
# ENVIRONMENT
#   CR_SESSION_BENCH_OUT    recording path (default docs/session-bench-baseline.txt)
#   CR_SESSION_P99_BUDGET_MS  p99 ceiling the verdict uses (default 100)
#   CR_SESSION_BENCH_SEQ_TIME  -benchtime for the sequential cells (default 2s)
#   CR_SESSION_BENCH_READ_REQUESTS   -benchtime for the concurrent read cell (default 2000x)
#   CR_SESSION_BENCH_CREATE_REQUESTS -benchtime for the concurrent write cell (default 300x)
#   CR_SESSION_BENCH_COMMIT_REQUESTS -benchtime for the store-commit-cost cell (default 100x)
#   CR_SESSION_BENCH_LOAD_THRESHOLD  loadavg at/above which --require-idle refuses
#
# EXIT CODES
#   0  baseline recorded (the verdict is printed, and is not an exit-code gate)
#   1  --check was given and at least one endpoint is OVER budget
#   2  bad usage
#   3  --require-idle was given and the host is busy
#   *  anything else: `go test` itself failed (build error, panic, assertion)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${CR_SESSION_BENCH_OUT:-$ROOT/docs/session-bench-baseline.txt}"
LOAD_THRESHOLD="${CR_SESSION_BENCH_LOAD_THRESHOLD:-8.0}"

QUICK=0
CHECK=0
REQUIRE_IDLE=0

usage() {
  sed -n '2,60p' "${BASH_SOURCE[0]}" | sed -n '/^# USAGE/,$p' | sed 's/^# \{0,1\}//'
}

for arg in "$@"; do
  case "$arg" in
    --quick) QUICK=1 ;;
    --check) CHECK=1 ;;
    --require-idle) REQUIRE_IDLE=1 ;;
    -h|--help) usage; exit 0 ;;
    *)
      printf 'benchmark-sessions: unknown argument %q\n\n' "$arg" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ "$QUICK" == "1" ]]; then
  SEQ_BENCHTIME="${CR_SESSION_BENCH_SEQ_TIME:-200x}"
  READ_REQUESTS="${CR_SESSION_BENCH_READ_REQUESTS:-300x}"
  CREATE_REQUESTS="${CR_SESSION_BENCH_CREATE_REQUESTS:-100x}"
  COMMIT_REQUESTS="${CR_SESSION_BENCH_COMMIT_REQUESTS:-20x}"
else
  SEQ_BENCHTIME="${CR_SESSION_BENCH_SEQ_TIME:-2s}"
  READ_REQUESTS="${CR_SESSION_BENCH_READ_REQUESTS:-2000x}"
  CREATE_REQUESTS="${CR_SESSION_BENCH_CREATE_REQUESTS:-300x}"
  COMMIT_REQUESTS="${CR_SESSION_BENCH_COMMIT_REQUESTS:-100x}"
fi

loadavg_1m() {
  if [[ -r /proc/loadavg ]]; then
    awk '{print $1}' /proc/loadavg
  else
    printf '0'
  fi
}

PRESSURE_IO="unavailable"
if [[ -r /proc/pressure/io ]]; then
  PRESSURE_IO="$(tr '\n' ' ' < /proc/pressure/io)"
fi

if [[ "$REQUIRE_IDLE" == "1" ]]; then
  observed="$(loadavg_1m)"
  if awk -v a="$observed" -v b="$LOAD_THRESHOLD" 'BEGIN { exit (a >= b) ? 0 : 1 }'; then
    printf 'benchmark-sessions: REFUSED — 1-minute loadavg %s >= threshold %s.\n' \
      "$observed" "$LOAD_THRESHOLD" >&2
    printf 'A p99 measured under another tenant'"'"'s load describes the host, not the endpoint.\n' >&2
    printf 'Wait for a quiet window, or raise CR_SESSION_BENCH_LOAD_THRESHOLD deliberately.\n' >&2
    exit 3
  fi
fi

cd "$ROOT"

# One recording per run: truncate, then append each phase under its own header.
: > "$OUT"

{
  printf '# SQLite session API baseline — raw benchmark output (DF-CRIER-308)\n'
  printf '# recorded: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  printf '# host: %s\n' "$(uname -srm)"
  printf '# cpus: %s\n' "$(getconf _NPROCESSORS_ONLN 2>/dev/null || printf 'unknown')"
  printf '# loadavg: %s\n' "$(cut -d' ' -f1-3 /proc/loadavg 2>/dev/null || printf 'unknown')"
  printf '# pressure/io: %s\n' "$PRESSURE_IO"
  printf '# go: %s\n' "$(go version 2>/dev/null || printf 'unknown')"
  printf '# budget: %sms p99 per endpoint (CR_SESSION_P99_BUDGET_MS)\n' \
    "${CR_SESSION_P99_BUDGET_MS:-100}"
} >> "$OUT"

run_phase() {
  local header="$1"
  shift
  printf '\n## %s\n\n$ go test %s ./internal/session/\n\n' "$header" "$*" | tee -a "$OUT"
  go test "$@" ./internal/session/ 2>&1 | tee -a "$OUT"
}

# The sequential cells: one endpoint at a time, the per-request service time
# with no queue in front of it.
run_phase "sequential endpoints (${SEQ_BENCHTIME})" \
  -run '^$' -bench='BenchmarkSession(Create|Retrieve|List)$' -benchtime="$SEQ_BENCHTIME"

# The concurrent cells: benchClients (100) requests in flight.
run_phase "100-concurrent read endpoints (${READ_REQUESTS})" \
  -run '^$' -bench='BenchmarkSessionReadP99$' -benchtime="$READ_REQUESTS"

run_phase "100-concurrent write endpoint (${CREATE_REQUESTS})" \
  -run '^$' -bench='BenchmarkSessionCreateP99$' -benchtime="$CREATE_REQUESTS"

# The floor under every write: one durable record write with the store's own
# pragmas (rollback, the shipped shape) versus the same write with WAL (the
# recommended change). This is what the create numbers are made of.
run_phase "one durable store write (${COMMIT_REQUESTS})" \
  -run '^$' -bench='BenchmarkSessionStoreCommitCost$' -benchtime="$COMMIT_REQUESTS"

# The final round of each (endpoint, mode) pair is the one with the most
# samples; earlier rounds are warm-up calibration and are dropped here.
# `|| true` is load-bearing: with `set -o pipefail` a grep that matches nothing
# (the zero-cell run the refusal below exists to catch) would otherwise abort
# the script on this assignment and it would exit 1 with NO diagnosis.
rows="$(grep -oE 'BASELINE [a-z0-9-]+ mode=[^ ]+ n=[0-9]+ .*verdict=[A-Z]+ budget_ms=[0-9]+' "$OUT" \
  | awk '{ last[$2 "/" $3] = $0 } END { for (key in last) print last[key] }' \
  | sort -k2,2 -k3,3 || true)"

{
  printf '\n## summary — last round per (endpoint, mode)\n\n'
  printf '  %-9s %-7s %-6s %-10s %-10s %-10s %-10s %s\n' \
    ENDPOINT MODE n p50_ms p95_ms p99_ms max_ms VERDICT
  printf '  %-9s %-7s %-6s %-10s %-10s %-10s %-10s %s\n' \
    --------- ------- ------ ---------- ---------- ---------- ---------- -------
  printf '%s\n' "$rows" | awk '
    {
      delete kv
      for (i = 1; i <= NF; i++) { split($i, p, "="); kv[p[1]] = p[2] }
      printf "  %-9s %-7s %-6s %-10s %-10s %-10s %-10s %s\n", \
        $2, kv["mode"], kv["n"], kv["p50_ms"], kv["p95_ms"], kv["p99_ms"], kv["max_ms"], kv["verdict"]
    }'
} | tee -a "$OUT"

over="$(printf '%s\n' "$rows" | grep -c 'verdict=OVER' || true)"
measured="$(printf '%s\n' "$rows" | grep -c 'verdict=' || true)"

# A run that produced no cell has recorded nothing, and `go test -bench` exits 0
# when a pattern matches no benchmark (the shape a rename or a moved file leaves
# behind). Recording zero cells and exiting 0 would be a vacuous green, the same
# class the repo's other checkers refuse, so it is a failure here too.
if [[ "$measured" -eq 0 ]]; then
  printf 'benchmark-sessions: FAILED — no BASELINE cell was produced, so nothing was recorded.\n' >&2
  printf 'A build failure, a panic, a skipped benchmark and a -bench pattern that matched nothing all look like this.\n' >&2
  printf 'Raw output kept at %s — read it before re-running.\n' "$OUT" >&2
  exit 1
fi

printf '\nrecorded %s endpoint cell(s) to %s\n' "$measured" "$OUT" >&2
printf 'p99 budget %sms: %s endpoint cell(s) OVER — see docs/performance.md for the analysis.\n' \
  "${CR_SESSION_P99_BUDGET_MS:-100}" "$over" >&2

if [[ "$CHECK" == "1" && "$over" -gt 0 ]]; then
  printf 'benchmark-sessions: --check FAILED (%s endpoint cell(s) over budget)\n' "$over" >&2
  exit 1
fi

exit 0
