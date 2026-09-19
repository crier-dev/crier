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
#           Connection timed out | No route to host | client_loop: send disconnect |
#           stream error | deadline_exceeded | context deadline exceeded |
#           DeadlineExceeded | Unavailable | i/o timeout
#       The signatures are matched case-SENSITIVELY, exactly as they always have
#       been: the pattern carries the lowercase wire spellings AND the capitalized
#       gRPC status names, so both spellings are covered without a `grep -i` that
#       would silently widen every pre-existing alternative.
#       Everything else is NON_TRANSPORT: retrying a real deploy error only hides
#       it, so it is attempted exactly once and the verdict says so.
#
# THE EVIDENCE RULE (why a NON_TRANSPORT reason is never the first output line)
# ----------------------------------------------------------------------------
# A NON_TRANSPORT reason quotes the line it judged, and that line must be RELATED
# TO THE FAILURE: prefer the last output line matching a transport signature,
# else the LAST non-blank output line (the freshest thing the command said before
# it died), truncated to 200 chars. It used to be the FIRST non-blank line of the
# whole attempt output, which is only correct when the wrapped command is a leaf.
# The cell-deploy leg is a RELAY (bunker-matrix.sh -> bunker-deploy.sh -> docker
# load), and CI run 35408229089 measured the consequence: the wrapper reported a
# sha256 digest from an earlier SUCCESSFUL `docker load` as the evidence for a
# stream-deadline failure (`bunker: stream error: deadline_exceeded: context
# deadline exceeded`, rc 1), with the retry budget left unspent. Harmless THAT
# time only because a digest matches no signature — a different interleaving could
# classify a real error as transient. The reason therefore names which rule
# picked the line it quotes, so a reader can tell the two apart.
#
# ONE LINE, ALWAYS
# ----------------
# TRANSPORT_CLASS_REASON is ONE line whatever it names: the transient branches
# quote the FIRST signature match — a single output line can carry several (the
# deadline line above carries three) and `grep -oE` prints one match per line, so
# an un-headed capture would leak newlines the moment two signatures land on one
# line — and the NON_TRANSPORT branch quotes one output line, cut to 200 chars.
# Callers embed the reason in a `FATAL: …` line, so a newline in it would split
# the single line an operator reads.
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
# mktemp/awk/grep/head/sleep exits 2 (or returns 2), naming what is wrong — never
# a silent default. awk is what computes the schedule; coreutils mktemp writes the
# scratch log under ${TMPDIR:-/tmp}; head is what keeps the reason one line.
#
# SELFTEST (used by `make transport-retry-selftest` and CI):
#
#     bash scripts/lib/transport-retry.sh --selftest
#
# Deterministic arms driven by a PATH shim (no ssh, no scp, no docker, no bunker
# host, no network), plus a pure backoff-schedule check, a misuse check, and a
# NEUTER proof — a copy of this file with the transient predicate forced false
# must make ARM A FAIL, and restoring it must make ARM A pass again. A selftest
# that cannot fail is not evidence. The arms:
#   ARM A — one transient reset, then success: exactly one retry;
#   ARM B — every attempt transient: fails closed after the budget;
#   ARM C — a real deploy error: one attempt, NON_TRANSPORT named;
#   ARM D — the run-35408229089 stream deadline, at rc 1 with the earlier success
#           digest as its first line: TRANSPORT_RESET and retried;
#   ARM E — the nested-relay shape: the evidence must be the failure (the LAST
#           line), never an earlier hop's success line.
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
#
# The stream/transport DEADLINE family is here because of CI run 35408229089: the
# cell-deploy leg died on `bunker: stream error: deadline_exceeded: context
# deadline exceeded` with rc 1 — NOT 255, so only the SIGNATURE can classify it,
# and with no matching alternative it was called NON_TRANSPORT and the retry
# budget went unspent. A stream that times out mid-transfer is exactly the
# transient class retrying exists for, so the wire spellings (`stream error`,
# `deadline_exceeded`, `context deadline exceeded`, `i/o timeout`) and the
# capitalized gRPC status names (`DeadlineExceeded`, `Unavailable`) are all in.
TRANSPORT_SSH_RC=255
TRANSPORT_TRANSIENT_PATTERN='Connection reset by peer|Connection closed|Broken pipe|kex_exchange_identification|Operation timed out|Connection timed out|No route to host|client_loop: send disconnect|stream error|deadline_exceeded|context deadline exceeded|DeadlineExceeded|Unavailable|i/o timeout'

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
  local rc="${1:-}" text="${2:-}" hit="" line="" ev_shape=""
  case "$rc" in
    '' | *[!0-9]*) rc=1 ;;
  esac
  TRANSPORT_CLASS=""
  TRANSPORT_CLASS_REASON=""

  if [ "$rc" -eq "$TRANSPORT_SSH_RC" ]; then
    # `<the first match>` and nothing else: `grep -m 1` bounds matching LINES, not
    # matches, so one output line carrying several signatures prints several
    # lines. Every reason here must stay ONE line (callers embed it in `FATAL: …`
    # — see the SINGLE-LINE REASON note in the header) — and run 35408229089's
    # stream-error line carries three of the signatures at once.
    hit="$(printf '%s\n' "$text" | grep -m 1 -oE "$TRANSPORT_TRANSIENT_PATTERN" 2>/dev/null | head -n 1 || true)"
    TRANSPORT_CLASS="TRANSPORT_RESET"
    TRANSPORT_CLASS_REASON="exit $rc (ssh/scp transport failure)"
    [ -n "$hit" ] && TRANSPORT_CLASS_REASON="$TRANSPORT_CLASS_REASON; output matched '$hit'"
    printf '%s' "$TRANSPORT_CLASS"
    return "$TRANSPORT_TRANSIENT_VERDICT"
  fi

  hit="$(printf '%s\n' "$text" | grep -m 1 -oE "$TRANSPORT_TRANSIENT_PATTERN" 2>/dev/null | head -n 1 || true)"
  if [ -n "$hit" ]; then
    TRANSPORT_CLASS="TRANSPORT_RESET"
    TRANSPORT_CLASS_REASON="exit $rc; output matched '$hit'"
    printf '%s' "$TRANSPORT_CLASS"
    return "$TRANSPORT_TRANSIENT_VERDICT"
  fi

  # ── NON_TRANSPORT: the evidence must be RELATED to the failure ─────────────
  # NOT the first non-blank line of the whole attempt output: the wrapped command
  # is often a RELAY, so its first line can be an earlier hop's already-relayed
  # SUCCESS line. CI run 35408229089 measured that — the reason quoted a sha256
  # digest from a successful `docker load` for a stream-deadline failure. So:
  # prefer the last line matching a transport signature, else the LAST non-blank
  # line (the freshest thing the command said before it died), cut to 200 chars.
  # The signature preference is defensive — the whole-output scan above already
  # decides TRANSPORT_RESET — but the evidence rule must not silently depend on
  # that scan staying whole-output.
  TRANSPORT_CLASS="NON_TRANSPORT"
  ev_shape="the last non-blank output line"
  line="$(printf '%s\n' "$text" | grep -E "$TRANSPORT_TRANSIENT_PATTERN" 2>/dev/null | grep -v '^[[:space:]]*$' | tail -n 1 || true)"
  if [ -n "$line" ]; then
    ev_shape="the last output line matching a transport signature"
  else
    line="$(printf '%s\n' "$text" | grep -v '^[[:space:]]*$' 2>/dev/null | tail -n 1 || true)"
  fi
  line="$(printf '%s' "$line" | cut -c1-200)"
  TRANSPORT_CLASS_REASON="exit $rc"
  [ -n "$line" ] && TRANSPORT_CLASS_REASON="$TRANSPORT_CLASS_REASON; not a transport signature, output: $line [$ev_shape]"
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
  _tr_require_tool head "it keeps the classifier's reason to a single line"
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

# ── the two arms that pin CI run 35408229089 ──────────────────────────────────
# The verbatim text harvested from that run's first cell (`bunker-e2e`, job
# "Bunker config-matrix E2E", first cell `guard-on`, 2026-09-19T00:08:56Z): one
# leftover SUCCESS line from the inner `docker load` hop, then the stream
# deadline. Both arms feed the SAME digest line so the two questions the incident
# raised stay separable — ARM D asks whether a deadline failure is RETRIED, ARM E
# asks what evidence an honest non-transport failure quotes.
_tr_INCIDENT_DIGEST='sha256:74a271eae952015703d5a08ed369d74d501e977fe38a4a2d70682e78278fcfc4'
_tr_INCIDENT_DEADLINE='bunker: stream error: deadline_exceeded: context deadline exceeded'
# The real, non-transport error ARM E's relayed output ends with.
_tr_INCIDENT_ERROR='docker: invalid reference format'

# _tr_arm_d <lib> <driver> <shim> <count-file>
# ARM D: the stream deadline. The shim's first call emits those two lines and
# exits 1 — NOT 255, so classifying it transient can only come from a SIGNATURE,
# which is exactly the defect the row was filed for; the next call succeeds. That
# must cost exactly one retry, and the failure's own reason must name the
# stream-error line rather than the digest that precedes it in the same text.
_tr_arm_d() {
  local lib="$1" driver="$2" shim="$3" count="$4"
  local out="" rc=0 calls=0 lines=0 result="" reset_line="" incident=""
  local cls="" reason="" first_line="" retry_line=""
  out="$(_tr_arm_run "$lib" "$driver" "$shim" "$count" stream-deadline-first 1 0 "arm-d")" || rc=$?
  calls="$(cat "$count" 2>/dev/null || printf '0')"
  lines="$(printf '%s\n' "$out" | grep -c '^transport-retry\[.*\]: attempt ' || true)"
  result="$(printf '%s\n' "$out" | grep '^ARM_RESULT ' | tail -n 1 || true)"
  reset_line="$(printf '%s\n' "$out" | grep -m 1 'TRANSPORT_RESET' || true)"
  retry_line="$(printf '%s\n' "$out" | grep -m 1 'retrying in 0s' || true)"

  # The same two lines, handed to the predicate DIRECTLY as well as through the
  # shim: the shim proves the RETRY, this proves the CLASSIFICATION of the exact
  # text the CI log carried — including the digest line, so a classifier that
  # quoted the first line instead of the failure cannot pass this arm.
  incident="$(printf '%s\n%s' "$_tr_INCIDENT_DIGEST" "$_tr_INCIDENT_DEADLINE")"
  first_line="$(printf '%s\n' "$incident" | grep -m 1 -v '^[[:space:]]*$' || true)"
  cls=""
  reason=""
  if transport_classify 1 "$incident" >/dev/null; then :; fi
  cls="$TRANSPORT_CLASS"
  reason="$TRANSPORT_CLASS_REASON"

  if [ "$rc" -ne 0 ]; then
    _tr_selftest_fail "ARM D" "a stream deadline still killed the hop (rc=$rc) — that is the CI red class (run 35408229089, 'bunker: stream error: deadline_exceeded')" "$out"
    return 1
  fi
  if [ "$calls" -ne 2 ]; then
    _tr_selftest_fail "ARM D" "the stream deadline was attempted $calls time(s), not exactly twice (1 initial + 1 retry expected for a 1-retry budget)" "$out"
    return 1
  fi
  if [ "$lines" -ne 2 ]; then
    _tr_selftest_fail "ARM D" "$lines attempt line(s) for 2 attempts — the wrapper must print exactly one line per attempt" "$out"
    return 1
  fi
  if ! printf '%s\n' "$out" | grep -q 'attempt 1/2 (retry budget 1)'; then
    _tr_selftest_fail "ARM D" "the retry line did not name the attempt number and the budget 'attempt 1/2 (retry budget 1)'" "$out"
    return 1
  fi
  if [ -z "$reset_line" ]; then
    _tr_selftest_fail "ARM D" "the run never named the class TRANSPORT_RESET" "$out"
    return 1
  fi
  if ! printf '%s\n' "$reset_line" | grep -Eq 'stream error|deadline_exceeded|context deadline exceeded'; then
    _tr_selftest_fail "ARM D" "the TRANSPORT_RESET line did not name the stream-deadline EVIDENCE (no 'stream error' / 'deadline_exceeded' in: $reset_line)" "$out"
    return 1
  fi
  if printf '%s\n' "$reset_line" | grep -q 'sha256:'; then
    _tr_selftest_fail "ARM D" "the TRANSPORT_RESET line carried the sha256 digest as its evidence: $reset_line" "$out"
    return 1
  fi
  # The reason must be ONE line: the incident text carries THREE signatures on one
  # output line, and `grep -oE` prints one match per line, so an un-headed capture
  # turns the wrapper's one diagnostic line into three (callers embed the reason in
  # a `FATAL: …` line). The whole diagnostic — label, budget, class, reason and
  # backoff — must therefore land on a single physical line.
  case "$retry_line" in
    'transport-retry[arm-d]: attempt 1/2 (retry budget 1) — TRANSPORT_RESET ('*') — retrying in 0s') ;;
    *)
      _tr_selftest_fail "ARM D" "the attempt line is not ONE line from the label to the backoff — the reason leaked a newline: '${retry_line:-none}'" "$out"
      return 1
      ;;
  esac
  case "$reason" in
    *$'\n'*)
      _tr_selftest_fail "ARM D" "the reason for the incident text embedded a newline (callers put it in a FATAL line): '$reason'" "$out"
      return 1
      ;;
  esac
  if [ "$first_line" != "$_tr_INCIDENT_DIGEST" ]; then
    _tr_selftest_fail "ARM D" "the incident arm is not testing the reported shape — its first line is '${first_line:-none}', not the earlier success digest" "$out"
    return 1
  fi
  if [ "$cls" != "TRANSPORT_RESET" ]; then
    _tr_selftest_fail "ARM D" "the verbatim incident text (digest line + '$(_tr_INCIDENT_DEADLINE)') classified as '$cls', not TRANSPORT_RESET" "$out"
    return 1
  fi
  if printf '%s\n' "$reason" | grep -q 'sha256:'; then
    _tr_selftest_fail "ARM D" "the incident text's reason quoted the sha256 digest, not the failure: $reason" "$out"
    return 1
  fi
  if ! printf '%s\n' "$reason" | grep -Eq 'stream error|deadline_exceeded|context deadline exceeded'; then
    _tr_selftest_fail "ARM D" "the incident text's reason did not name the stream-deadline line: ${reason:-none}" "$out"
    return 1
  fi
  case "$result" in
    *'rc=0 class=OK attempts=2 retries=1 budget=1'*) ;;
    *)
      _tr_selftest_fail "ARM D" "the result globals do not describe one retry of the deadline (got '${result:-none}', want rc=0 class=OK attempts=2 retries=1 budget=1)" "$out"
      return 1
      ;;
  esac
  echo "PASS: ARM D: the run-35408229089 stream deadline (rc=1, digest line first) is TRANSPORT_RESET and RETRIED (2 shim calls = 1 initial + 1 retry, budget 1, $lines attempt lines, evidence names the stream error — not the digest — and the whole diagnostic stays ONE line)"
  return 0
}

# _tr_arm_e <lib> <driver> <shim> <count-file>
# ARM E: the nested-relay shape — the NON_TRANSPORT control. The hop's output
# starts with the leftover SUCCESS digest (an inner hop's own line, relayed) and
# ENDS with a real deploy error; no line carries a transport signature. A real
# error must still be attempted exactly ONCE, with the budget unspent and no
# retry announced, and the reason must quote the LAST line — never the digest,
# which is precisely what the pre-fix first-line rule reported in run 35408229089.
_tr_arm_e() {
  local lib="$1" driver="$2" shim="$3" count="$4"
  local out="" rc=0 calls=0 lines=0 result="" reason_line="" block=""
  local blk_first="" blk_last=""
  out="$(_tr_arm_run "$lib" "$driver" "$shim" "$count" relay-then-error 3 0 "arm-e")" || rc=$?
  calls="$(cat "$count" 2>/dev/null || printf '0')"
  lines="$(printf '%s\n' "$out" | grep -c '^transport-retry\[.*\]: attempt ' || true)"
  result="$(printf '%s\n' "$out" | grep '^ARM_RESULT ' | tail -n 1 || true)"
  reason_line="$(printf '%s\n' "$out" | grep -m 1 '^transport-retry\[.*\]: attempt ' || true)"
  # The wrapped command's own lines, as the wrapper relayed them: everything
  # between this attempt's single diagnostic line and the VERDICT that follows.
  block="$(printf '%s\n' "$out" | awk '/^transport-retry\[/ { if (started) exit; started = 1; next } started { print }')"
  blk_first="$(printf '%s\n' "$block" | grep -m 1 -v '^[[:space:]]*$' || true)"
  blk_last="$(printf '%s\n' "$block" | grep -v '^[[:space:]]*$' | tail -n 1 || true)"

  if [ "$rc" -ne 1 ]; then
    _tr_selftest_fail "ARM E" "the caller's own exit code was lost (got $rc, want 1) — a real deploy error must not be reported as success" "$out"
    return 1
  fi
  if [ "$calls" -ne 1 ]; then
    _tr_selftest_fail "ARM E" "a NON-transport failure was attempted $calls time(s) — it must be attempted exactly once (retrying a real error only hides it)" "$out"
    return 1
  fi
  if [ "$lines" -ne 1 ]; then
    _tr_selftest_fail "ARM E" "$lines attempt line(s) for 1 attempt" "$out"
    return 1
  fi
  if ! printf '%s\n' "$reason_line" | grep -q 'NON_TRANSPORT'; then
    _tr_selftest_fail "ARM E" "the verdict did not say NON_TRANSPORT" "$out"
    return 1
  fi
  if ! printf '%s\n' "$reason_line" | grep -q 'retry budget 3 not spent'; then
    _tr_selftest_fail "ARM E" "the verdict did not say the retry budget was left unspent" "$out"
    return 1
  fi
  if printf '%s\n' "$out" | grep -q 'retrying in'; then
    _tr_selftest_fail "ARM E" "the run announced a retry for a failure it must not retry" "$out"
    return 1
  fi
  # ONE line, like every other reason: label, budget, class, reason, decision.
  case "$reason_line" in
    'transport-retry[arm-e]: attempt 1/4 (retry budget 3 not spent — not retryable) — NON_TRANSPORT ('*') — not retried') ;;
    *)
      _tr_selftest_fail "ARM E" "the attempt line is not ONE line from the label to the decision — the reason leaked a newline: '${reason_line:-none}'" "$out"
      return 1
      ;;
  esac
  # Premises: the shape under test really is the nested relay (an earlier success
  # line FIRST, the real error LAST). Without these two the evidence assertions
  # below could pass over an output that never had a first-line decoy at all.
  if [ "$blk_first" != "$_tr_INCIDENT_DIGEST" ]; then
    _tr_selftest_fail "ARM E" "the arm is not testing the reported shape — the hop's first line is '${blk_first:-none}', not the earlier success digest" "$out"
    return 1
  fi
  if [ "$blk_last" != "$_tr_INCIDENT_ERROR" ]; then
    _tr_selftest_fail "ARM E" "the arm is not testing the reported shape — the hop's last line is '${blk_last:-none}', not '$_tr_INCIDENT_ERROR'" "$out"
    return 1
  fi
  if ! printf '%s\n' "$reason_line" | grep -qF "$blk_last"; then
    _tr_selftest_fail "ARM E" "the NON_TRANSPORT reason did not quote the real error line '$blk_last': $reason_line" "$out"
    return 1
  fi
  if printf '%s\n' "$reason_line" | grep -q 'sha256:'; then
    _tr_selftest_fail "ARM E" "the NON_TRANSPORT reason quoted the earlier success digest instead of the failure: $reason_line" "$out"
    return 1
  fi
  if printf '%s\n' "$reason_line" | grep -qF 'Unable to find image'; then
    _tr_selftest_fail "ARM E" "the NON_TRANSPORT reason quoted a line that is not the failure: $reason_line" "$out"
    return 1
  fi
  if ! printf '%s\n' "$reason_line" | grep -q 'last non-blank output line'; then
    _tr_selftest_fail "ARM E" "the NON_TRANSPORT reason did not name which rule picked its evidence: $reason_line" "$out"
    return 1
  fi
  case "$result" in
    *'rc=1 class=NON_TRANSPORT attempts=1 retries=0 budget=3'*) ;;
    *)
      _tr_selftest_fail "ARM E" "the result globals do not describe one unretried attempt (got '${result:-none}')" "$out"
      return 1
      ;;
  esac
  echo "PASS: ARM E: the nested-relay shape stays NON_TRANSPORT on ONE attempt (1 shim call, rc=1, budget 3 unspent, no retry announced) and its reason quotes the LAST line '$blk_last' — not the earlier success digest that opens the output — all on one line"
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
  for tool in mktemp awk grep sed tr sleep cat cut head tail; do
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
#   stream-deadline-first
#                 the first call dies the way CI run 35408229089 died: a digest
#                 line from an earlier SUCCESSFUL `docker load`, then the real
#                 `bunker: stream error: deadline_exceeded: context deadline
#                 exceeded` — with rc 1, NOT 255, so only a SIGNATURE can classify
#                 it — then OK
#   relay-then-error
#                 the nested-relay shape: the first line is that same leftover
#                 success digest and the LAST line is a real, non-transport deploy
#                 error (rc 1)
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
  stream-deadline-first)
    if [ "$n" -le 1 ]; then
      echo "sha256:74a271eae952015703d5a08ed369d74d501e977fe38a4a2d70682e78278fcfc4" >&2
      echo "bunker: stream error: deadline_exceeded: context deadline exceeded" >&2
      exit 1
    fi
    ;;
  relay-then-error)
    echo "sha256:74a271eae952015703d5a08ed369d74d501e977fe38a4a2d70682e78278fcfc4" >&2
    echo "Unable to find image 'crier:test' locally" >&2
    echo "docker: invalid reference format" >&2
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

  # ── ARM D: the run-35408229089 stream deadline (rc=1, digest line first) ────
  checks=$((checks + 1))
  if _tr_arm_d "$self" "$driver" "$shim" "$tmp/arm-d.calls"; then :; else fails=$((fails + 1)); fi

  # ── ARM E: the nested-relay shape — evidence is the failure, not a digest ───
  checks=$((checks + 1))
  if _tr_arm_e "$self" "$driver" "$shim" "$tmp/arm-e.calls"; then :; else fails=$((fails + 1)); fi

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
  echo "transport-retry selftest: $checks/$checks checks behaved (5 arms + the backoff schedule + the misuse refusal + the neuter proof)"
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

The selftest drives five arms through a PATH shim — no ssh, no scp, no docker,
no bunker host — and proves each one by COUNTING the shim's calls:
  ARM A — one transient reset, then success: exactly one retry, rc=0;
  ARM B — every attempt transient: fails closed after the budget, naming the
          class and the budget, and returns the LAST error code;
  ARM C — a real deploy error: exactly one attempt, NON_TRANSPORT named;
  ARM D — the run-35408229089 stream deadline (rc 1, an earlier success digest as
          the first output line): TRANSPORT_RESET, exactly one retry;
  ARM E — the nested-relay shape: a real error after a relayed success line is
          NON_TRANSPORT in exactly one attempt, and the reason quotes the FAILURE
          (the last output line), never the earlier digest.
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
