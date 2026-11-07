#!/usr/bin/env bash
#
# scripts/wait-for-command-selftest.sh — prove the wait primitive, and prove the
# trap it exists to remove (CRIER-HARNESS-016).
#
# WHY THIS EXISTS
# ---------------
# A selftest that only demonstrates the happy path says nothing about the failure
# the tool was written for. The load-bearing arm here is T4: it reproduces the
# SELF-MATCH trap on this host, in the same process tree, and shows the two
# verdicts side by side — the naive process-table probe reports a match for a job
# that has ALREADY EXITED (because the probe's own argv carries the pattern),
# while the marker-anchored waiter reports done. Both outcomes are printed with
# their exit codes. That is the difference the primitive buys.
#
# WHAT IS PROVED
# --------------
#   T1  pid mode      `sleep 2` is launched, the recorded pid is waited on:
#                     exit 0, STATUS=done mode=pid, elapsed in a sane window,
#                     and the job is reaped (no leftover process).
#   T2  marker mode   a background job appends `SUITE_DONE rc=0` to its own log
#                     after ~2s; the wait on --log/--marker: exit 0,
#                     STATUS=done mode=marker.
#   T3  timeout mode  a marker that never appears, --timeout 2: exit 4,
#                     STATUS=timeout, elapsed >= 2 and bounded (no runaway).
#   T4  RED PROOF     with a UNIQUE per-run token: (a) launch a job whose argv
#                     carries the token and which exits by itself; (b) after it
#                     has exited, the naive probe `sh -c 'pgrep -f "<token>"'`
#                     STILL reports a match — the trap; (c) the marker waiter
#                     reports done. Exit codes of both are printed.
#   T5  refusal       a name/pattern-shaped request (--name, --pattern, …) is
#                     REFUSED with exit 2 and the self-match reason, never
#                     silently reinterpreted; a pid outside 1..N, an unbounded
#                     timeout and a timeout above the 3600s cap are refused too.
#   T6  source rule   the tool itself contains no process-table pattern search
#                     (grep for the two pattern-matching tools comes back empty),
#                     so the rule cannot rot back in unnoticed.
#
# EXIT CODES
#   0  every arm behaved as required — prints SELFTEST=PASS
#   1  at least one arm was unmet   — prints SELFTEST=FAIL <case> and exits 1
#   2  this selftest cannot run (a required tool is missing on PATH; it is never
#      a silent skip)
#
# HERMETIC: bash, coreutils, procps' pgrep, /proc. No network, no docker, no
# server, no fixed port, and every process it starts is reaped — the trap sweeps
# any survivor on exit. Runs in well under 20 seconds.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="wait-for-command-selftest"
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
WAITER="$SCRIPT_DIR/wait-for-command.sh"

CHECKS=0
FAILS=0
FIRST_FAIL=""
PIDS=()

pass() {
  CHECKS=$((CHECKS + 1))
  printf 'PASS  %s\n' "$*"
}

fail() {
  CHECKS=$((CHECKS + 1))
  FAILS=$((FAILS + 1))
  [ -n "$FIRST_FAIL" ] || FIRST_FAIL="$1"
  printf 'FAIL  %s\n' "$*"
}

# check <arm> <description> <actual> <expected>
check_eq() {
  if [ "$3" = "$4" ]; then
    pass "$1: $2 (=$4)"
  else
    fail "$1: $2 — expected '$4', observed '$3'"
  fi
}

TMP_DIR=""
_cleanup() {
  local p
  for p in ${PIDS[@]+"${PIDS[@]}"}; do
    [ -n "$p" ] && kill -9 "$p" 2>/dev/null
  done
  [ -n "$TMP_DIR" ] && rm -rf "$TMP_DIR" 2>/dev/null
  return 0
}
trap _cleanup EXIT

for tool in bash awk grep sed kill sleep mktemp sh; do
  command -v "$tool" >/dev/null 2>&1 || {
    printf '%s: CANNOT RUN — required tool %s is not on PATH\n' "$PROG" "$tool" >&2
    exit 2
  }
done
if [ ! -f "$WAITER" ]; then
  printf '%s: CANNOT RUN — %s is missing on disk\n' "$PROG" "$WAITER" >&2
  exit 2
fi
# The red proof needs a process-table pattern probe to demonstrate the trap. It
# is the ARM's subject, not a dependency of the tool — a host without it cannot
# run this selftest, and that is a refusal (exit 2), never a silent skip.
if ! command -v pgrep >/dev/null 2>&1; then
  printf '%s: CANNOT RUN — pgrep is not on PATH, so the self-match red proof (T4) cannot be executed\n' "$PROG" >&2
  exit 2
fi

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/wait-for-command-selftest.XXXXXX")" || {
  printf '%s: CANNOT RUN — cannot create a scratch directory\n' "$PROG" >&2
  exit 2
}

elapsed_of() {
  printf '%s\n' "$1" | sed -n 's/.*elapsed=\([0-9][0-9]*\).*/\1/p'
}

# --------------------------------------------------------------------- T1
# pid mode: a recorded pid, waited on until it is gone.
printf '\n--- T1 pid mode ---\n'
sleep 2 &
T1_PID=$!
PIDS+=("$T1_PID")
T1_OUT="$(bash "$WAITER" --pid "$T1_PID" --timeout 20 --interval 1)"
T1_RC=$?
T1_ELAPSED="$(elapsed_of "$T1_OUT")"
check_eq T1 "exit code" "$T1_RC" "0"
case "$T1_OUT" in
  "STATUS=done pid=$T1_PID elapsed="*" mode=pid")
    pass "T1: STATUS line names done + the recorded pid + mode=pid ($T1_OUT)"
    ;;
  *)
    fail "T1: STATUS line wrong — expected STATUS=done pid=$T1_PID elapsed=<n> mode=pid, observed '$T1_OUT'"
    ;;
esac
if [ -n "$T1_ELAPSED" ] && [ "$T1_ELAPSED" -ge 1 ] && [ "$T1_ELAPSED" -le 15 ]; then
  pass "T1: elapsed=${T1_ELAPSED}s is sane for a 2s job with a 1s cadence"
else
  fail "T1: elapsed='${T1_ELAPSED}' is outside the sane window 1..15s"
fi
wait "$T1_PID" 2>/dev/null
if kill -0 "$T1_PID" 2>/dev/null; then
  fail "T1: the waited-on process is still alive after the waiter said done"
else
  pass "T1: the job was reaped — no leftover process"
fi

# --------------------------------------------------------------------- T2
# marker mode: completion is the job's OWN log line.
printf '\n--- T2 marker mode ---\n'
T2_LOG="$TMP_DIR/t2.log"
: >"$T2_LOG"
(
  sleep 2
  printf 'SUITE_DONE rc=0\n' >>"$T2_LOG"
) &
T2_JOB=$!
PIDS+=("$T2_JOB")
T2_OUT="$(bash "$WAITER" --log "$T2_LOG" --marker 'SUITE_DONE' --timeout 20 --interval 1)"
T2_RC=$?
check_eq T2 "exit code" "$T2_RC" "0"
case "$T2_OUT" in
  "STATUS=done pid=- elapsed="*" mode=marker")
    pass "T2: STATUS line names done + mode=marker ($T2_OUT)"
    ;;
  *)
    fail "T2: STATUS line wrong — expected STATUS=done pid=- elapsed=<n> mode=marker, observed '$T2_OUT'"
    ;;
esac
wait "$T2_JOB" 2>/dev/null
if grep -q 'SUITE_DONE' "$T2_LOG"; then
  pass "T2: the marker really was in the job's log"
else
  fail "T2: the marker is missing from the log the waiter accepted"
fi

# --------------------------------------------------------------------- T3
# timeout mode: a marker that never appears, with a 2s bound.
printf '\n--- T3 timeout mode ---\n'
T3_LOG="$TMP_DIR/t3.log" # deliberately never created: nothing can match
T3_START=$SECONDS
T3_OUT="$(bash "$WAITER" --log "$T3_LOG" --marker 'NEVER_APPEARS' --timeout 2 --interval 1)"
T3_RC=$?
T3_WALL=$((SECONDS - T3_START))
check_eq T3 "exit code" "$T3_RC" "4"
case "$T3_OUT" in
  "STATUS=timeout pid=- elapsed="*" mode=marker")
    pass "T3: STATUS line names timeout ($T3_OUT)"
    ;;
  *)
    fail "T3: STATUS line wrong — expected STATUS=timeout … mode=marker, observed '$T3_OUT'"
    ;;
esac
T3_ELAPSED="$(elapsed_of "$T3_OUT")"
if [ -n "$T3_ELAPSED" ] && [ "$T3_ELAPSED" -ge 2 ] && [ "$T3_ELAPSED" -le 10 ]; then
  pass "T3: elapsed=${T3_ELAPSED}s respects the 2s bound instead of running away"
else
  fail "T3: elapsed='${T3_ELAPSED}' is outside the bounded window 2..10s"
fi
if [ "$T3_WALL" -le 10 ]; then
  pass "T3: the whole bounded wait cost ${T3_WALL}s wall"
else
  fail "T3: the bounded wait took ${T3_WALL}s wall — the bound is not holding"
fi

# --------------------------------------------------------------------- T4
# RED PROOF of the self-match trap. This is the arm that keeps the tool honest.
printf '\n--- T4 red proof: the self-match trap, on this host ---\n'
T4_TOKEN="waitfc-selftest-$$-${RANDOM}"
T4_LOG="$TMP_DIR/t4.log"
: >"$T4_LOG"
printf 'RED-PROOF token=%s\n' "$T4_TOKEN"

# (a) a short job whose OWN argv carries the token, which exits by itself.
bash -c 'sleep 1; printf "JOB_DONE token=%s\n" "$2" >>"$1"' waitfc-job "$T4_LOG" "$T4_TOKEN" &
T4_JOB=$!
PIDS+=("$T4_JOB")

# (c) the blessed shape: wait on the completion line the job writes itself.
T4_WAIT_OUT="$(bash "$WAITER" --log "$T4_LOG" --marker '^JOB_DONE token=' --timeout 20 --interval 1)"
T4_WAIT_RC=$?
wait "$T4_JOB" 2>/dev/null

# (b) the trap: the job is now GONE, and the naive probe still "finds" it.
if kill -0 "$T4_JOB" 2>/dev/null; then
  fail "T4: the job did not exit, so the red proof cannot be run"
  T4_PROBE_OUT=""
  T4_PROBE_RC=-1
else
  pass "T4: the job exited and its pid $T4_JOB is gone (kill -0 rc=1)"
  T4_PROBE_OUT="$(sh -c 'pgrep -f "$1"' waitfc-probe "$T4_TOKEN")"
  T4_PROBE_RC=$?
fi

printf 'RED-PROOF naive probe: sh -c %s%s%s -> rc=%s matches=[%s]\n' \
  "'pgrep -f \"" "$T4_TOKEN" "\"'" "$T4_PROBE_RC" "$(printf '%s' "$T4_PROBE_OUT" | tr '\n' ' ')"
printf 'RED-PROOF marker waiter: rc=%s %s\n' "$T4_WAIT_RC" "$T4_WAIT_OUT"

if [ "$T4_PROBE_RC" -eq 0 ] && [ -n "$T4_PROBE_OUT" ]; then
  pass "T4: the naive probe STILL reports a match after the job exited — the trap is real on this host"
else
  fail "T4: the naive probe did not reproduce the trap (rc=$T4_PROBE_RC) — the red proof is not falsifiable here"
fi
# The match must be the PROBE's own command line, never the dead job's pid.
if printf '%s\n' "$T4_PROBE_OUT" | grep -qx -- "$T4_JOB"; then
  fail "T4: the matched pid IS the exited job (pid reuse?) — this host cannot demonstrate the trap"
else
  pass "T4: none of the matched pids is the exited job — the match is the probe's own argv"
fi
check_eq T4 "waiter exit code" "$T4_WAIT_RC" "0"
case "$T4_WAIT_OUT" in
  "STATUS=done pid=- elapsed="*" mode=marker")
    pass "T4: the marker-anchored waiter reported done on the same job"
    ;;
  *)
    fail "T4: the marker-anchored waiter did not report done — observed '$T4_WAIT_OUT'"
    ;;
esac

# --------------------------------------------------------------------- T5
# Refusals: the name/pattern class and every unbounded/unsafe bound.
printf '\n--- T5 refusals ---\n'
refuse_case() { # refuse_case <arm> <expect-substring> <args...>
  local arm="$1" want="$2"
  shift 2
  local out rc
  out="$(bash "$WAITER" "$@" 2>&1 >/dev/null)"
  rc=$?
  if [ "$rc" -ne 2 ]; then
    fail "$arm: expected exit 2 (refusal), observed $rc"
    return
  fi
  case "$out" in
    *"$want"*)
      pass "$arm: refused (2) naming '$want'"
      ;;
    *)
      fail "$arm: refused with the wrong reason — expected to mention '$want', observed '$out'"
      ;;
  esac
}

refuse_case "T5 --name" "process-name matching is refused" --name make
refuse_case "T5 --pattern" "process-name matching is refused" --pattern 'make test'
refuse_case "T5 unbounded timeout" "always bounded" --log /dev/null --marker x --timeout 0
refuse_case "T5 timeout above cap" "hard cap" --log /dev/null --marker x --timeout 3601
refuse_case "T5 bad pid" "positive integer" --pid abc
refuse_case "T5 no mode" "no wait mode" --timeout 5

# The self-pid refusal is driven through `sh -c 'exec …'` so the pid the waiter
# is handed IS its own: $$ expands in the sh, and exec keeps that pid.
T5_SELF_OUT="$(sh -c 'exec bash "$1" --pid $$' waitfc-self "$WAITER" 2>&1 >/dev/null)"
T5_SELF_RC=$?
if [ "$T5_SELF_RC" -eq 2 ]; then
  case "$T5_SELF_OUT" in
    *"own pid"*) pass "T5 self pid: refused (2) — the waiter will not wait on itself" ;;
    *) fail "T5 self pid: refused (2) with the wrong reason: $T5_SELF_OUT" ;;
  esac
else
  fail "T5 self pid: expected exit 2, observed $T5_SELF_RC ($T5_SELF_OUT)"
fi

# --------------------------------------------------------------------- T6
# The tool must contain no process-table pattern search at all — not disabled,
# not commented out, not reachable through an option. The ONLY permitted mention
# is the refusal arm that NAMES those options in order to reject them, so the arm
# reports the raw count too instead of hiding it behind a regex.
printf '\n--- T6 source rule ---\n'
T6_RAW="$(grep -cE 'pgrep|pkill' "$WAITER" 2>/dev/null)"
case "$T6_RAW" in '' | *[!0-9]*) T6_RAW=0 ;; esac
T6_HITS="$(grep -nE '(^|[^[:alnum:]_-])(pgrep|pkill)([^[:alnum:]_-]|$)' "$WAITER" 2>/dev/null)"
printf 'T6: raw mentions of the two pattern tools in the waiter: %s (expected: only the refusal arm naming them as REFUSED options)\n' "$T6_RAW"
if [ -z "$T6_HITS" ]; then
  pass "T6: zero command-position occurrences — the waiter never invokes a pattern-matching process tool"
else
  fail "T6: the waiter invokes a pattern-matching process tool: ${T6_HITS}"
fi

# ------------------------------------------------------------------- verdict
printf '\n'
if [ "$FAILS" -eq 0 ]; then
  printf 'SELFTEST=PASS checks=%s\n' "$CHECKS"
  exit 0
fi
printf 'SELFTEST=FAIL %s (%s of %s checks failed)\n' "$FIRST_FAIL" "$FAILS" "$CHECKS"
exit 1
