#!/usr/bin/env bash
#
# scripts/lib/transport-retry.sh — retry and CLASSIFY one transport hop, so a
# single transient ssh/scp reset cannot kill a whole deploy leg (INT-CI-001).
#
# WHY THIS EXISTS
# ---------------
# CI run 35302932314 (workflow bunker-e2e, headSha 96684ed, 2026-09-18T03:22Z)
# failed inside the `blocking` cell BEFORE a single probe was sent:
#
#     Read from remote host <agent>: Connection reset by peer
#     scp: Connection closed
#     FATAL: cell deploy failed: blocking — no probes against stale container
#
# The commit under test was board-JSONL-only and the workflow is green on 18 of
# the last 20 runs, so the cause was one flaky transport hop, not a product
# regression. scripts/bunker-matrix.sh called the deploy ONCE — with no retry —
# and scripts/bunker-deploy.sh ran `scp` ONCE, also with no retry, so a single
# reset silently TRUNCATED the verification: the blocking cell, and every probe
# in it, never ran. This library is the retry and the classification those legs
# were missing, and the verdict they need so the next red run is attributable
# without re-running it.
#
# PUBLIC SURFACE (functions only; sourcing runs nothing and prints nothing, and
# is safe under `set -euo pipefail`):
#
#     . "$REPO/scripts/lib/transport-retry.sh"
#
#   retry_transport <label> -- <cmd...>
#       Run <cmd...>, capturing its combined output to a scratch log. Return 0 as
#       soon as it succeeds. On failure, CLASSIFY it: a transient transport reset
#       is retried (bounded, backed off), a non-transport failure is NOT retried.
#       Returns the LAST exit code the command produced — exhaustion neither
#       invents a success nor returns a synthetic code, so a caller's existing
#       fail-closed path (`retry_transport … || fatal "…"`) keeps working.
#
#       The wrapped command's own output is relayed verbatim on the final attempt
#       (so a wrapped leg's log lines survive) — this wrapper is for commands
#       with bounded output, such as an ssh/scp hop or a deploy leg. The
#       wrapper's OWN diagnostics are exactly one line per attempt.
#
#       On return the caller may read (set on every path, success included):
#         TRANSPORT_RESULT_CLASS      OK | TRANSPORT_RESET | NON_TRANSPORT
#         TRANSPORT_RESULT_ATTEMPTS   attempts that actually ran
#         TRANSPORT_RESULT_RETRIES    retries used (attempts - 1)
#         TRANSPORT_RESULT_BUDGET     the retry budget that was in force
#         TRANSPORT_RESULT_RC         the last exit code
#         TRANSPORT_RESULT_REASON     one-line reason, with the evidence
#         TRANSPORT_RESULT_VERDICT    '<CLASS> after <n> retry attempt(s) (<m>
#                                     total, budget <b>)' — the string a fatal
#                                     path embeds so the log is attributable
#         TRANSPORT_RESULT_LOG        the scratch log (kept on failure only)
#
#   transport_classify <rc> <text>
#       The predicate, on its own and free of any retry policy: prints
#       TRANSPORT_RESET when <rc>/<text> is a transport reset and NON_TRANSPORT
#       otherwise, and RETURNS 0 only for TRANSPORT_RESET. Sets
#       TRANSPORT_CLASS_REASON. TRANSIENT means either
#         - exit status 255 (the code ssh and scp use for their own failures —
#           a remote command's status is reported separately), or
#         - output matching TRANSPORT_TRANSIENT_PATTERN, i.e. one of
#           Connection reset by peer | Connection closed | Broken pipe |
#           kex_exchange_identification | Operation timed out |
#           Connection timed out | No route to host | client_loop: send disconnect
#       Everything else is NON_TRANSPORT: retrying a real deploy error only hides
#       it, so it is attempted exactly once and the verdict says so.
#
#   transport_backoff_for <retry-index> [base]
#       The delay before retry #<retry-index> (1-based): `base` doubled
#       (<retry-index> - 1) times and capped at TRANSPORT_RETRY_BACKOFF_MAX_S.
#       Pure — it prints the number and never sleeps — so the selftest pins the
#       whole schedule without spending the wall clock.
#
# BUDGET, AND THE OFF-BY-ONE IT IS EASY TO GET WRONG
# --------------------------------------------------
# TRANSPORT_RETRIES (default 3) is the RETRY budget, not the attempt budget: the
# initial attempt is NOT counted against it, so a budget of 3 runs at most
# 4 attempts (1 initial + 3 retries). Both numbers are printed on every attempt
# line and in the verdict, so neither reading can be mistaken for the other.
# TRANSPORT_RETRIES=0 disables retrying — one attempt, same verdicts.
#
# NESTING COST (why the budget is the knob to reach for)
# -----------------------------------------------------
# A deploy leg is itself a retry unit ABOVE the transfer retry inside
# scripts/bunker-deploy.sh, and both read TRANSPORT_RETRIES: with the default 3
# the worst case for one cell is 3 leg retries x 4 transfer attempts. One
# TRANSPORT_RETRIES=1 bounds it at 2 x 2 — that is the knob, not a new one.
#
# MISUSE AND DEPENDENCIES
# -----------------------
# A bad argument, a non-numeric TRANSPORT_RETRIES / backoff, or a missing
# mktemp/awk/grep/sleep exits 2 (or returns 2), naming what is wrong — never a
# silent default. awk is what computes the schedule; coreutils mktemp writes the
# scratch log under ${TMPDIR:-/tmp}.
#
# SELFTEST (used by `make transport-retry-selftest` and CI):
#
#     bash scripts/lib/transport-retry.sh --selftest
#
# Deterministic arms driven by a PATH shim (no ssh, no scp, no docker, no bunker
# host, no network), plus a pure backoff-schedule check, a misuse check, and a
# NEUTER proof — a copy of this file with the transient predicate forced false
# must make ARM A FAIL, and restoring it must make ARM A pass again. A selftest
# that cannot fail is not evidence.
#
# shellcheck shell=bash disable=SC2155,SC2317

if [ "${BASH_VERSINFO[0]:-0}" -lt 4 ]; then
  echo "transport-retry.sh requires bash 4+ (running ${BASH_VERSION:-unknown})" >&2
  return 2 2>/dev/null || exit 2
fi

# ── the transient signatures ──────────────────────────────────────────────────
#
# Exit 255 is the code ssh/scp use for their own failures; the messages are what
# a reset, a half-open connection or an unreachable host say on the wire. A
# failure matching NEITHER is a real error (a bad path, a full disk, a rejected
# command) and retrying it only hides it behind a delay.
TRANSPORT_SSH_RC=255
TRANSPORT_TRANSIENT_PATTERN='Connection reset by peer|Connection closed|Broken pipe|kex_exchange_identification|Operation timed out|Connection timed out|No route to host|client_loop: send disconnect'

# THE VERDICT LINE — the neuter lever. Every transient branch of
# transport_classify() below returns THIS value instead of a literal `return 0`,
# so the selftest can rewrite exactly this one line in a copy of this file and
# prove that ARM A then FAILS (see the NEUTER proof in the selftest).
TRANSPORT_TRANSIENT_VERDICT=0

# ── the result globals (defined at source time so a caller can read them under
#    `set -u` before the first call) ───────────────────────────────────────────
TRANSPORT_RESULT_CLASS=""
TRANSPORT_RESULT_ATTEMPTS=""
TRANSPORT_RESULT_RETRIES=""
TRANSPORT_RESULT_BUDGET=""
TRANSPORT_RESULT_RC=""
TRANSPORT_RESULT_REASON=""
TRANSPORT_RESULT_VERDICT=""
TRANSPORT_RESULT_LOG=""

# TRANSPORT_CLASS / TRANSPORT_CLASS_REASON are the classifier's own output.
TRANSPORT_CLASS=""
TRANSPORT_CLASS_REASON=""

# ── internal helpers ──────────────────────────────────────────────────────────

_tr_require_tool() { # <name> <why> — exit 2 when a hard dependency is missing
  command -v "$1" >/dev/null 2>&1 && return 0
  echo "ERROR: transport-retry.sh needs '$1' on PATH ($2)" >&2
  exit 2
}

_tr_uint() { # <name> <value> — echo the value, exit 2 when it is not a whole number
  case "${2:-}" in
    '' | *[!0-9]*)
      echo "ERROR: transport-retry.sh: $1 must be a whole number (got '${2:-}')" >&2
      exit 2
      ;;
  esac
  printf '%s' "$2"
}

_tr_seconds() { # <name> <value> — echo the value, exit 2 when it is not a non-negative number
  case "${2:-}" in
    '' | *[!0-9.]* | *.*.* | .* | *.)
      echo "ERROR: transport-retry.sh: $1 must be a non-negative number of seconds (got '${2:-}')" >&2
      exit 2
      ;;
  esac
  printf '%s' "$2"
}

_tr_results() { # <class> <attempts> <budget> <rc> <reason> <verdict>
  TRANSPORT_RESULT_CLASS="$1"
  TRANSPORT_RESULT_ATTEMPTS="$2"
  TRANSPORT_RESULT_BUDGET="$3"
  TRANSPORT_RESULT_RC="$4"
  TRANSPORT_RESULT_REASON="$5"
  TRANSPORT_RESULT_VERDICT="$6"
  TRANSPORT_RESULT_RETRIES="$(( $2 - 1 ))"
}

_tr_relay() { # <captured output> — the wrapped command's own lines, right where it put them
  local out="${1:-}"
  [ -n "$out" ] || return 0
  printf '%s\n' "$out" >&2
}

# ── public: the classifier (no retry policy in here at all) ───────────────────

transport_classify() { # <rc> <text> — prints the class; returns 0 only for TRANSPORT_RESET
  local rc="${1:-}" text="${2:-}" hit="" line=""
  case "$rc" in
    '' | *[!0-9]*) rc=1 ;;
  esac
  TRANSPORT_CLASS=""
  TRANSPORT_CLASS_REASON=""

  if [ "$rc" -eq "$TRANSPORT_SSH_RC" ]; then
    hit="$(printf '%s\n' "$text" | grep -m 1 -oE "$TRANSPORT_TRANSIENT_PATTERN" 2>/dev/null || true)"
    TRANSPORT_CLASS="TRANSPORT_RESET"
    TRANSPORT_CLASS_REASON="exit $rc (ssh/scp transport failure)"
    [ -n "$hit" ] && TRANSPORT_CLASS_REASON="$TRANSPORT_CLASS_REASON; output matched '$hit'"
    printf '%s' "$TRANSPORT_CLASS"
    return "$TRANSPORT_TRANSIENT_VERDICT"
  fi

  hit="$(printf '%s\n' "$text" | grep -m 1 -oE "$TRANSPORT_TRANSIENT_PATTERN" 2>/dev/null || true)"
  if [ -n "$hit" ]; then
    TRANSPORT_CLASS="TRANSPORT_RESET"
    TRANSPORT_CLASS_REASON="exit $rc; output matched '$hit'"
    printf '%s' "$TRANSPORT_CLASS"
    return "$TRANSPORT_TRANSIENT_VERDICT"
  fi

  TRANSPORT_CLASS="NON_TRANSPORT"
  line="$(printf '%s\n' "$text" | grep -m 1 -v '^[[:space:]]*$' 2>/dev/null | cut -c1-200 || true)"
  TRANSPORT_CLASS_REASON="exit $rc"
  [ -n "$line" ] && TRANSPORT_CLASS_REASON="$TRANSPORT_CLASS_REASON; not a transport signature, output: $line"
  printf '%s' "$TRANSPORT_CLASS"
  return 1
}

transport_is_transient() { # <rc> <text> — the retryable predicate, and nothing else
  transport_classify "$@" >/dev/null
  return $?
}

# ── public: the backoff schedule (pure — it never sleeps) ─────────────────────

transport_backoff_for() { # <retry-index> [base]
  local n base cap d i
  n="$(_tr_uint 'transport_backoff_for: retry index' "${1:-1}")"
  if [ "$n" -lt 1 ]; then
    echo "ERROR: transport-retry.sh: transport_backoff_for: the retry index is 1-based (got $n)" >&2
    exit 2
  fi
  base="$(_tr_seconds TRANSPORT_RETRY_BACKOFF_S "${2:-${TRANSPORT_RETRY_BACKOFF_S:-2}}")"
  cap="$(_tr_seconds TRANSPORT_RETRY_BACKOFF_MAX_S "${TRANSPORT_RETRY_BACKOFF_MAX_S:-8}")"
  _tr_require_tool awk "it computes the doubling backoff schedule"
  awk -v b="$base" -v n="$n" -v c="$cap" 'BEGIN {
    d = b
    for (i = 1; i < n; i++) d = d * 2
    if (d > c) d = c
    printf "%s\n", d
  }'
}

# ── public: run it, retry only what is transient, report what happened ────────

retry_transport() { # <label> -- <cmd...>
  local label="${1:-}" dash="${2:-}"
  if [ -z "$label" ] || [ "$dash" != "--" ] || [ "$#" -lt 3 ]; then
    echo "retry_transport: misuse — usage: retry_transport <label> -- <cmd...>" >&2
    return 2
  fi
  shift 2

  _tr_require_tool mktemp "it creates the per-run scratch log"
  _tr_require_tool awk "it computes the backoff schedule"
  _tr_require_tool grep "it reads the transient signatures out of the failed command's output"
  _tr_require_tool sleep "it backs off between attempts"

  local budget base cap attempt total rc out cls reason delay transient log start
  budget="$(_tr_uint TRANSPORT_RETRIES "${TRANSPORT_RETRIES:-3}")" || return 2
  base="$(_tr_seconds TRANSPORT_RETRY_BACKOFF_S "${TRANSPORT_RETRY_BACKOFF_S:-2}")" || return 2
  cap="$(_tr_seconds TRANSPORT_RETRY_BACKOFF_MAX_S "${TRANSPORT_RETRY_BACKOFF_MAX_S:-8}")" || return 2
  total=$((budget + 1))
  attempt=1
  rc=0
  out=""
  cls=""
  reason=""
  delay=""
  transient=0

  log="$(mktemp "${TMPDIR:-/tmp}/transport-retry-$$-XXXXXX.log" 2>/dev/null)" || {
    echo "retry_transport[$label]: cannot create a scratch log under '${TMPDIR:-/tmp}'" >&2
    return 2
  }
  TRANSPORT_RESULT_LOG="$log"

  while : ; do
    rc=0
    out="$("$@" 2>&1)" || rc=$?
    {
      printf -- '--- attempt %s/%s (rc=%s) ---\n' "$attempt" "$total" "$rc"
      printf '%s\n' "$out"
    } >>"$log"

    # ── success ───────────────────────────────────────────────────────────────
    if [ "$rc" -eq 0 ]; then
      _tr_relay "$out"
      echo "transport-retry[$label]: attempt $attempt/$total (retry budget $budget) — OK (exit 0)" >&2
      _tr_results OK "$attempt" "$budget" 0 "" \
        "OK after $((attempt - 1)) retry attempt(s) ($attempt total, budget $budget)"
      rm -f "$log"
      TRANSPORT_RESULT_LOG=""
      return 0
    fi

    # ── classify BEFORE deciding to retry (the predicate is separate) ─────────
    # Called through a redirection, NOT a command substitution: the classifier
    # reports the class on stdout AND in TRANSPORT_CLASS / …_REASON, and a
    # substitution would run it in a subshell where those globals are thrown
    # away — the reason would silently print as "()".
    transient=0
    cls=""
    if transport_classify "$rc" "$out" >/dev/null; then
      transient=1
    fi
    cls="$TRANSPORT_CLASS"
    reason="$TRANSPORT_CLASS_REASON"

    # ── not retryable: exactly one attempt, and say so ────────────────────────
    if [ "$transient" -eq 0 ]; then
      echo "transport-retry[$label]: attempt $attempt/$total (retry budget $budget not spent — not retryable) — $cls ($reason) — not retried" >&2
      _tr_relay "$out"
      _tr_results "$cls" "$attempt" "$budget" "$rc" "$reason" \
        "$cls after $attempt attempt(s), retry budget $budget not spent"
      echo "transport-retry[$label]: VERDICT — ${TRANSPORT_RESULT_VERDICT}: $reason" >&2
      echo "transport-retry[$label]: raw attempt log: $log" >&2
      return "$rc"
    fi

    # ── transient, budget exhausted: fail closed with the LAST error ──────────
    if [ "$attempt" -ge "$total" ]; then
      echo "transport-retry[$label]: attempt $attempt/$total (retry budget $budget) — $cls ($reason) — retry budget exhausted" >&2
      _tr_relay "$out"
      _tr_results "$cls" "$attempt" "$budget" "$rc" "$reason" \
        "$cls after $((attempt - 1)) retry attempt(s) ($attempt total, budget $budget)"
      echo "transport-retry[$label]: VERDICT — ${TRANSPORT_RESULT_VERDICT}: $reason" >&2
      echo "transport-retry[$label]: raw attempt log: $log" >&2
      return "$rc"
    fi

    # ── transient, budget left: back off (doubling, capped) and retry ─────────
    delay="$(transport_backoff_for "$attempt" "$base")" || return 2
    echo "transport-retry[$label]: attempt $attempt/$total (retry budget $budget) — $cls ($reason) — retrying in ${delay}s" >&2
    sleep "$delay"
    attempt=$((attempt + 1))
  done
}

# ── selftest ──────────────────────────────────────────────────────────────────

_tr_SELFTEST_TMP=""

_tr_selftest_cleanup() {
  if [ -n "$_tr_SELFTEST_TMP" ]; then
    rm -rf "$_tr_SELFTEST_TMP"
  fi
  return 0
}

_tr_selftest_fail() { # <tag> <message> <output>
  echo "transport-retry selftest: FAIL: $1: $2" >&2
  printf '%s\n' "${3:-}" | sed 's/^/  output: /' >&2
  return 1
}

# _tr_arm_run <lib> <driver> <shim-dir> <count-file> <mode> <retries> <backoff>
#             <label>
# Run retry_transport for ONE arm in a child bash, against the PATH shim, with
# the arm's own call counter. Prints the child's combined output — including the
# ARM_RESULT line, which echoes the child's exit code and the result globals, so
# every assertion below reads a MEASURED number instead of eyeballing a
# sentence — and returns the child's exit status (the wrapped hop's last code).
_tr_arm_run() {
  local lib="$1" driver="$2" shim="$3" count="$4" mode="$5" retries="$6" backoff="$7" label="$8"
  printf '0\n' >"$count"
  env PATH="$shim:$PATH" TR_SHIM_COUNT="$count" TR_SHIM_MODE="$mode" \
    TRANSPORT_RETRIES="$retries" TRANSPORT_RETRY_BACKOFF_S="$backoff" \
    TMPDIR="$_tr_SELFTEST_TMP" \
    bash "$driver" "$lib" "$label" -- transport-shim 2>&1
}

# _tr_arm_a <lib> <driver> <shim> <count-file> <tag>
# ARM A: the first hop resets, the next succeeds. Exactly ONE retry must have
# happened — proven by counting the shim's calls, not by reading the log.
_tr_arm_a() {
  local lib="$1" driver="$2" shim="$3" count="$4" tag="${5:-ARM A}"
  local out="" rc=0 calls=0 lines=0 result=""
  out="$(_tr_arm_run "$lib" "$driver" "$shim" "$count" reset-first 3 0.05 "$tag")" || rc=$?
  calls="$(cat "$count" 2>/dev/null || printf '0')"
  lines="$(printf '%s\n' "$out" | grep -c '^transport-retry\[.*\]: attempt ' || true)"
  result="$(printf '%s\n' "$out" | grep '^ARM_RESULT ' | tail -n 1 || true)"

  if [ "$rc" -ne 0 ]; then
    _tr_selftest_fail "$tag" "one transient reset still killed the hop (rc=$rc) — that is the CI red class (run 35302932314)" "$out"
    return 1
  fi
  if [ "$calls" -ne 2 ]; then
    _tr_selftest_fail "$tag" "the reset was retried $((calls - 1)) time(s), not exactly one (shim calls=$calls: 1 initial + 1 retry expected)" "$out"
    return 1
  fi
  if [ "$lines" -ne 2 ]; then
    _tr_selftest_fail "$tag" "$lines attempt line(s) for 2 attempts — the wrapper must print exactly one line per attempt" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q 'TRANSPORT_RESET'; then
    _tr_selftest_fail "$tag" "the retry line did not name the class TRANSPORT_RESET" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q 'attempt 1/4 (retry budget 3)'; then
    _tr_selftest_fail "$tag" "the retry line did not name the attempt number and the budget 'attempt 1/4 (retry budget 3)'" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q 'retrying in 0.05s'; then
    _tr_selftest_fail "$tag" "the retry did not print the backoff it waited" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q "output matched 'Connection reset by peer'"; then
    _tr_selftest_fail "$tag" "the retry line named the class but not the EVIDENCE (no \"output matched 'Connection reset by peer'\")" "$out"
    return 1
  fi
  case "$result" in
    *'rc=0 class=OK attempts=2 retries=1 budget=3'*) ;;
    *)
      _tr_selftest_fail "$tag" "the result globals do not describe one retry (got '${result:-none}', want rc=0 class=OK attempts=2 retries=1 budget=3)" "$out"
      return 1
      ;;
  esac
  echo "PASS: $tag: a reset on the first hop is retried exactly once and recovers (2 shim calls = 1 initial + 1 retry, rc=0, 'TRANSPORT_RESET' + 'attempt 1/4 (retry budget 3)' named)"
  return 0
}

# _tr_arm_b <lib> <driver> <shim> <count-file> — ARM B: every attempt transient.
_tr_arm_b() {
  local lib="$1" driver="$2" shim="$3" count="$4"
  local budget=4 out="" rc=0 calls=0 lines=0 result="" d=""
  out="$(_tr_arm_run "$lib" "$driver" "$shim" "$count" reset-always "$budget" 0.05 "arm-b")" || rc=$?
  calls="$(cat "$count" 2>/dev/null || printf '0')"
  lines="$(printf '%s\n' "$out" | grep -c '^transport-retry\[.*\]: attempt ' || true)"
  result="$(printf '%s\n' "$out" | grep '^ARM_RESULT ' | tail -n 1 || true)"

  if [ "$rc" -ne 255 ]; then
    _tr_selftest_fail "ARM B" "an exhausted budget returned $rc, not the LAST error (255) — exhaustion must return the last code, never a synthetic one" "$out"
    return 1
  fi
  if [ "$calls" -ne $((budget + 1)) ]; then
    _tr_selftest_fail "ARM B" "the hop ran $calls time(s) for a $budget-retry budget; expected $((budget + 1)) attempts (the initial attempt is not counted against the budget)" "$out"
    return 1
  fi
  if [ "$lines" -ne $((budget + 1)) ]; then
    _tr_selftest_fail "ARM B" "$lines attempt line(s) for $((budget + 1)) attempts — one line per attempt, no more" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q 'TRANSPORT_RESET'; then
    _tr_selftest_fail "ARM B" "the exhausted run did not name the class TRANSPORT_RESET" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q "retry budget $budget"; then
    _tr_selftest_fail "ARM B" "the exhausted run did not name the $budget-retry budget" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q "TRANSPORT_RESET after $budget retry attempt(s) ($((budget + 1)) total, budget $budget)"; then
    _tr_selftest_fail "ARM B" "the verdict did not carry the class AND the budget as one named sentence" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q "output matched 'Connection reset by peer'"; then
    _tr_selftest_fail "ARM B" "the exhausted run named the class but not the EVIDENCE (no \"output matched 'Connection reset by peer'\")" "$out"
    return 1
  fi
  if printf '%s\n' "$out" | grep -q 'OK (exit 0)'; then
    _tr_selftest_fail "ARM B" "a success line was printed for a run that never succeeded" "$out"
    return 1
  fi
  for d in 0.05 0.1 0.2 0.4; do
    if ! printf '%s\n' "$out" | grep -q "retrying in ${d}s"; then
      _tr_selftest_fail "ARM B" "the backoff schedule is not the documented doubling (missing 'retrying in ${d}s')" "$out"
      return 1
    fi
  done
  case "$result" in
    *"rc=255 class=TRANSPORT_RESET attempts=$((budget + 1)) retries=$budget budget=$budget"*) ;;
    *)
      _tr_selftest_fail "ARM B" "the result globals do not describe an exhausted $budget-retry budget (got '${result:-none}')" "$out"
      return 1
      ;;
  esac
  echo "PASS: ARM B: every attempt transient fails closed after the budget (rc=255 = the last error, $((budget + 1)) hop calls = 1 initial + $budget retries, class + budget named, doubling backoff 0.05/0.1/0.2/0.4 printed, no success line)"
  return 0
}

# _tr_arm_c <lib> <driver> <shim> <count-file> — ARM C: a real deploy error.
_tr_arm_c() {
  local lib="$1" driver="$2" shim="$3" count="$4"
  local out="" rc=0 calls=0 lines=0 result=""
  out="$(_tr_arm_run "$lib" "$driver" "$shim" "$count" deploy-error 3 0 "arm-c")" || rc=$?
  calls="$(cat "$count" 2>/dev/null || printf '0')"
  lines="$(printf '%s\n' "$out" | grep -c '^transport-retry\[.*\]: attempt ' || true)"
  result="$(printf '%s\n' "$out" | grep '^ARM_RESULT ' | tail -n 1 || true)"

  if [ "$rc" -eq 0 ]; then
    _tr_selftest_fail "ARM C" "a real deploy error was reported as success" "$out"
    return 1
  fi
  if [ "$rc" -ne 1 ]; then
    _tr_selftest_fail "ARM C" "the caller's own exit code was lost (got $rc, want 1)" "$out"
    return 1
  fi
  if [ "$calls" -ne 1 ]; then
    _tr_selftest_fail "ARM C" "a NON-transient failure was attempted $calls time(s) — it must be attempted exactly once (retrying a real error only hides it)" "$out"
    return 1
  fi
  if [ "$lines" -ne 1 ]; then
    _tr_selftest_fail "ARM C" "$lines attempt line(s) for 1 attempt" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q 'NON_TRANSPORT'; then
    _tr_selftest_fail "ARM C" "the verdict did not say NON_TRANSPORT" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q 'retry budget 3 not spent'; then
    _tr_selftest_fail "ARM C" "the verdict did not say the retry budget was left unspent" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q 'not a transport signature, output: docker: Error response from daemon: no space left on device'; then
    _tr_selftest_fail "ARM C" "the NON_TRANSPORT verdict did not carry the real error as its evidence" "$out"
    return 1
  fi
  if printf '%s\n' "$out" | grep -q 'retrying in'; then
    _tr_selftest_fail "ARM C" "the run announced a retry for a failure it must not retry" "$out"
    return 1
  fi
  case "$result" in
    *'rc=1 class=NON_TRANSPORT attempts=1 retries=0 budget=3'*) ;;
    *)
      _tr_selftest_fail "ARM C" "the result globals do not describe one unretried attempt (got '${result:-none}')" "$out"
      return 1
      ;;
  esac
  echo "PASS: ARM C: a non-transient failure is NOT retried (1 hop call, rc=1, 'NON_TRANSPORT' + 'retry budget 3 not spent' named, no retry announced)"
  return 0
}

_transport_selftest() {
  local fails=0 checks=0
  local self="${1:-${BASH_SOURCE[0]}}"
  local tool=""
  if [ ! -f "$self" ]; then
    echo "transport-retry selftest: FAIL: cannot locate this script (${self:-unknown}) — the arms source it" >&2
    return 2
  fi
  for tool in mktemp awk grep sed tr sleep cat cut; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      echo "transport-retry selftest: FAIL: '$tool' is not on PATH — the arms and the scratch logs need it" >&2
      return 2
    fi
  done

  _tr_SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/transport-retry-selftest.XXXXXX")" || {
    echo "transport-retry selftest: FAIL: cannot create a scratch dir under '${TMPDIR:-/tmp}'" >&2
    return 2
  }
  trap '_tr_selftest_cleanup' EXIT

  local tmp="$_tr_SELFTEST_TMP"
  local shim="$tmp/shim-bin"
  local driver="$tmp/arm-driver.sh"

  mkdir -p "$shim" || {
    echo "transport-retry selftest: FAIL: cannot create the shim dir $shim" >&2
    return 2
  }
  if ! cat >"$shim/transport-shim" <<'SHIM'
#!/usr/bin/env bash
# transport-retry selftest shim — stands in for ONE ssh/scp hop. No network, no
# scp, no docker, no bunker host: the behaviour is driven entirely by env, so
# every arm is deterministic. TR_SHIM_MODE:
#   reset-first   the first call resets (255 + the real scp messages), then OK
#   reset-always  every call resets
#   deploy-error  a real deploy failure (exit 1, no transport signature)
n=0
if [ -n "${TR_SHIM_COUNT:-}" ] && [ -f "$TR_SHIM_COUNT" ]; then
  n="$(cat "$TR_SHIM_COUNT" 2>/dev/null)"
  [ -n "$n" ] || n=0
fi
n=$((n + 1))
if [ -n "${TR_SHIM_COUNT:-}" ]; then printf '%s\n' "$n" >"$TR_SHIM_COUNT"; fi
case "${TR_SHIM_MODE:-reset-first}" in
  reset-first)
    if [ "$n" -le 1 ]; then
      echo "Read from remote host bunker-fake: Connection reset by peer" >&2
      echo "scp: Connection closed" >&2
      exit 255
    fi
    ;;
  reset-always)
    echo "Read from remote host bunker-fake: Connection reset by peer" >&2
    echo "scp: Connection closed" >&2
    exit 255
    ;;
  deploy-error)
    echo "docker: Error response from daemon: no space left on device" >&2
    exit 1
    ;;
  *)
    echo "shim: unknown TR_SHIM_MODE '${TR_SHIM_MODE:-}'" >&2
    exit 99
    ;;
esac
printf 'shim: hop %s delivered\n' "$n"
exit 0
SHIM
  then
    echo "transport-retry selftest: FAIL: cannot write the PATH shim $shim/transport-shim" >&2
    return 2
  fi
  chmod +x "$shim/transport-shim" || {
    echo "transport-retry selftest: FAIL: cannot make $shim/transport-shim executable" >&2
    return 2
  }

  # The child driver: source the library under test, run ONE retry_transport,
  # echo the measured exit code + result globals. The arms read THAT line.
  if ! cat >"$driver" <<'DRIVER'
#!/usr/bin/env bash
# transport-retry selftest arm driver — <lib> <label> -- <cmd...>
. "$1"
shift
retry_transport "$@"
rc=$?
printf 'ARM_RESULT rc=%s class=%s attempts=%s retries=%s budget=%s last_rc=%s verdict=%s\n' \
  "$rc" "${TRANSPORT_RESULT_CLASS:-}" "${TRANSPORT_RESULT_ATTEMPTS:-}" \
  "${TRANSPORT_RESULT_RETRIES:-}" "${TRANSPORT_RESULT_BUDGET:-}" \
  "${TRANSPORT_RESULT_RC:-}" "${TRANSPORT_RESULT_VERDICT:-}"
exit "$rc"
DRIVER
  then
    echo "transport-retry selftest: FAIL: cannot write the arm driver $driver" >&2
    return 2
  fi

  echo "transport-retry selftest: shim $shim/transport-shim (PATH shim only — no ssh, no scp, no docker, no bunker host, no network)"
  echo "transport-retry selftest: scratch $tmp (per-run attempt logs land here; removed on exit)"

  # ── ARM A: one transient reset, then success ────────────────────────────────
  checks=$((checks + 1))
  if _tr_arm_a "$self" "$driver" "$shim" "$tmp/arm-a.calls" "ARM A"; then :; else fails=$((fails + 1)); fi

  # ── ARM B: every attempt transient → fail closed on the budget ──────────────
  checks=$((checks + 1))
  if _tr_arm_b "$self" "$driver" "$shim" "$tmp/arm-b.calls"; then :; else fails=$((fails + 1)); fi

  # ── ARM C: a non-transient failure is not retried ───────────────────────────
  checks=$((checks + 1))
  if _tr_arm_c "$self" "$driver" "$shim" "$tmp/arm-c.calls"; then :; else fails=$((fails + 1)); fi

  # ── the backoff schedule, pinned without spending the wall clock ────────────
  checks=$((checks + 1))
  local sched="" sched_frac=""
  sched="$(for i in 1 2 3 4 5; do transport_backoff_for "$i" 2; done | tr '\n' ' ')"
  sched="${sched% }"
  sched_frac="$(for i in 1 2 3; do transport_backoff_for "$i" 0.05; done | tr '\n' ' ')"
  sched_frac="${sched_frac% }"
  if [ "$sched" != "2 4 8 8 8" ]; then
    _tr_selftest_fail "BACKOFF" "the schedule for a 2s base did not double to the 8s cap (got '${sched:-none}', want '2 4 8 8 8')" ""
    fails=$((fails + 1))
  elif [ "$sched_frac" != "0.05 0.1 0.2" ]; then
    _tr_selftest_fail "BACKOFF" "a sub-second base did not double exactly (got '${sched_frac:-none}', want '0.05 0.1 0.2')" ""
    fails=$((fails + 1))
  else
    echo "PASS: BACKOFF: the schedule doubles from the base and stops at the cap (base 2 -> '2 4 8 8 8', base 0.05 -> '0.05 0.1 0.2')"
  fi

  # ── misuse and misconfiguration are refused, never silently defaulted ───────
  checks=$((checks + 1))
  local misuse_out="" misuse_rc=0 cfg_out="" cfg_rc=0
  misuse_out="$( ( retry_transport "no separator" true ) 2>&1 )" || misuse_rc=$?
  cfg_out="$( ( TRANSPORT_RETRIES=three retry_transport "bad budget" -- true ) 2>&1 )" || cfg_rc=$?
  if [ "$misuse_rc" -ne 2 ]; then
    _tr_selftest_fail "MISUSE" "a call without the '--' separator returned $misuse_rc, not 2" "$misuse_out"
    fails=$((fails + 1))
  elif ! printf '%s\n' "$misuse_out" | grep -q 'usage:'; then
    _tr_selftest_fail "MISUSE" "the refusal did not print the usage line" "$misuse_out"
    fails=$((fails + 1))
  elif [ "$cfg_rc" -ne 2 ]; then
    _tr_selftest_fail "MISUSE" "a non-numeric TRANSPORT_RETRIES returned $cfg_rc, not 2 (a silent default would hide the typo)" "$cfg_out"
    fails=$((fails + 1))
  elif ! printf '%s\n' "$cfg_out" | grep -q 'TRANSPORT_RETRIES'; then
    _tr_selftest_fail "MISUSE" "the refusal did not name TRANSPORT_RETRIES" "$cfg_out"
    fails=$((fails + 1))
  else
    echo "PASS: MISUSE: a missing '--' separator (rc=2, usage named) and a non-numeric TRANSPORT_RETRIES (rc=2, the variable named) are both refused"
  fi

  # ── NEUTER proof: an arm that cannot fail is not evidence ───────────────────
  checks=$((checks + 1))
  local neut="$tmp/transport-retry.neutered.sh" lever=0 neut_lever=0
  local n_out="" n_rc=0 r_out="" r_rc=0 n_first=""
  lever="$(grep -c '^TRANSPORT_TRANSIENT_VERDICT=0$' "$self" || true)"
  sed -e 's/^TRANSPORT_TRANSIENT_VERDICT=0$/TRANSPORT_TRANSIENT_VERDICT=1/' "$self" >"$neut"
  neut_lever="$(grep -c '^TRANSPORT_TRANSIENT_VERDICT=1$' "$neut" || true)"
  if [ "$lever" -ne 1 ]; then
    _tr_selftest_fail "NEUTER" "the library does not carry exactly one 'TRANSPORT_TRANSIENT_VERDICT=0' lever line (found $lever) — the proof would be a no-op" ""
    fails=$((fails + 1))
  elif [ "$neut_lever" -ne 1 ]; then
    _tr_selftest_fail "NEUTER" "the copy did not take the substitution (found $neut_lever 'TRANSPORT_TRANSIENT_VERDICT=1' line(s) in $neut) — the proof would test the real predicate" ""
    fails=$((fails + 1))
  else
    n_out="$(_tr_arm_a "$neut" "$driver" "$shim" "$tmp/neuter-a.calls" "ARM A (neutered predicate)" 2>&1)"
    n_rc=$?
    n_first="$(printf '%s\n' "$n_out" | grep -m 1 'FAIL: ARM A (neutered predicate)' || true)"
    if [ "$n_rc" -eq 0 ]; then
      _tr_selftest_fail "NEUTER" "ARM A still PASSED with the transient predicate forced false — the arm cannot fail, so it is not evidence" "$n_out"
      fails=$((fails + 1))
    elif [ -z "$n_first" ]; then
      _tr_selftest_fail "NEUTER" "the neutered run failed (rc=$n_rc) without naming ARM A, so it failed for some other reason" "$n_out"
      fails=$((fails + 1))
    else
      r_out="$(_tr_arm_a "$self" "$driver" "$shim" "$tmp/restored-a.calls" "ARM A (restored predicate)" 2>&1)"
      r_rc=$?
      if [ "$r_rc" -ne 0 ]; then
        _tr_selftest_fail "NEUTER" "ARM A still fails after restoring the predicate (rc=$r_rc) — the RED was not caused by the neuter" "$r_out"
        fails=$((fails + 1))
      else
        echo "PASS: NEUTER: the transient predicate is load-bearing — forced false, ARM A fails ('${n_first#transport-retry selftest: }'); restored, ARM A passes again"
      fi
    fi
  fi

  if [ "$fails" -ne 0 ]; then
    echo "transport-retry selftest: $((checks - fails))/$checks checks behaved — FAIL" >&2
    return 1
  fi
  echo "transport-retry selftest: $checks/$checks checks behaved (3 arms + the backoff schedule + the misuse refusal + the neuter proof)"
  return 0
}

# ── CLI (only when executed — sourcing must define functions and nothing else) ─

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  case "${1:-}" in
    --selftest)
      _transport_selftest "${BASH_SOURCE[0]}"
      exit $?
      ;;
    -h | --help | help)
      cat <<'EOF'
transport-retry.sh — retry + classify one transport hop (INT-CI-001)

Usage:
  bash scripts/lib/transport-retry.sh --selftest

It is a library, not a tool: the deploy legs source it for
  retry_transport <label> -- <cmd...>   (retry ONLY a transient ssh/scp failure)
  transport_classify <rc> <text>        (the predicate, on its own)
  transport_backoff_for <retry-index>   (the doubling schedule, pure)

TRANSPORT_RETRIES (default 3) is the RETRY budget: the initial attempt is not
counted against it, so 3 means at most 4 attempts. TRANSPORT_RETRY_BACKOFF_S
(default 2, doubled per retry, capped at TRANSPORT_RETRY_BACKOFF_MAX_S = 8).

The selftest drives three arms through a PATH shim — no ssh, no scp, no docker,
no bunker host — and proves each one by COUNTING the shim's calls:
  ARM A — one transient reset, then success: exactly one retry, rc=0;
  ARM B — every attempt transient: fails closed after the budget, naming the
          class and the budget, and returns the LAST error code;
  ARM C — a real deploy error: exactly one attempt, NON_TRANSPORT named.
Plus the backoff schedule, the misuse refusals, and a NEUTER proof (forced
false, the transient predicate must make ARM A fail; restored, it passes).
EOF
      exit 0
      ;;
    *)
      echo "transport-retry.sh: unknown argument '${1:-}' (try --selftest)" >&2
      exit 2
      ;;
  esac
fi
