#!/usr/bin/env bash
#
# scripts/load-repro.sh — the entry point for reproducing a load-dependent
# failure on this box, safely (DF-CRIER-254).
#
# WHY THIS EXISTS
# ---------------
# "Reproduce it on a loaded box" used to mean an ad-hoc shell loop that
# backgrounded one burn unit per iteration with no owner and no teardown.  Those
# units reparented to the user systemd manager when their spawner exited, so they
# outlived every caller: 73 -> 278 concurrent burners, loadavg 220/346/237, a box
# shared with the Hermes gateway, the scheduler and DuckBrain unusable for hours.
#
# This script is the easy path that replaces it: bounded load (scripts/loadgen.py
# — hard caps of 8 workers and 300 s), a documented load-average refusal, one
# load run at a time per host, and a teardown that leaves NOTHING behind — not on
# success, not on a signal, not on a SIGKILL of the generator.
#
# USAGE
# -----
#   scripts/load-repro.sh [--workers N] [--seconds S] [--load-threshold L] \
#                         [--loadavg-file PATH] [--cpus SPEC] -- <command...>
#
# Examples:
#   scripts/load-repro.sh --workers 4 --seconds 60 -- go test ./... -count=1
#   scripts/load-repro.sh --workers 2 --seconds 5 -- bash -c 'exit 7'   # rc 7
#
# OPTIONS
#   --workers N          burners to start (passed through; hard cap 8 in loadgen)
#   --seconds S          how long the load should last (hard cap 300 in loadgen)
#   --load-threshold L   skip the run when the 1-minute loadavg is above L
#                        (default 8.0; the comparison is STRICTLY ABOVE)
#   --loadavg-file PATH  where the load figure is read from (default /proc/loadavg).
#                        This override exists so a test can pin a synthetic value —
#                        production runs should read /proc/loadavg.
#   --cpus SPEC          pin the load to a CPU set, e.g. 0-3
#   --                   everything after it is the target command
#
# CONTRACT
#   1. Above the threshold the run is SKIPPED with a recorded reason and exit 3,
#      and the target is NEVER started (the generator owns this gate — see "ONE
#      IMPLEMENTATION OF THE GATE" below — and this script relays its verdict).
#   2. A second concurrent run refuses ("another load-repro is already running",
#      naming the holder) with exit 3: one load run at a time per host.
#   3. The target runs alongside the bounded load and this script exits with the
#      TARGET's exit code — unless a child survived teardown, which exits 1 and
#      is reported rather than hidden behind the target's status.
#   4. On EXIT / INT / TERM / HUP the generator, its burners and the target are
#      killed (TERM -> bounded wait -> KILL) and verified gone.  The generator's
#      children additionally carry the kernel PR_SET_PDEATHSIG=SIGKILL, so even a
#      SIGKILL of the generator cannot leave a burner behind.
#   5. Exactly ONE machine-readable summary line is printed on stdout:
#         load-repro: {"event":"summary", ...}
#      Diagnostics go to stderr.
#
# ONE IMPLEMENTATION OF THE GATE
# ------------------------------
# This script does not re-implement the cap/threshold logic: every value is
# passed to scripts/loadgen.py, which validates the caps, reads the load average
# and refuses (exit 3, with the recorded `SKIPPED: ...` reason) BEFORE it starts a
# single child.  The wrapper relays that verdict verbatim and returns its exit
# code without ever starting the target.  The numbers below are only what this
# script REPORTS when a flag is absent; they mirror scripts/loadgen.py's
# DEFAULT_* constants and a drift can change the report, never what is allowed.
#
# EXIT CODES
#   0..125  the target's own exit code
#   1       a child survived teardown (outranks the target's status)
#   2       misuse, python3 missing, or the generator failed before starting
#   3       refused: host above the load threshold, or another run holds the lock
#   128+N   this wrapper was interrupted by signal N (teardown still ran)
#
# DEPENDENCIES: bash, python3 (scripts/loadgen.py is stdlib-only), flock, and the
# usual coreutils.  No docker, no network, no root.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPTS_DIR="$(cd "$(dirname "$SELF")" && pwd)"
LOADGEN="$SCRIPTS_DIR/loadgen.py"
SURVIVORS_LIB="$SCRIPTS_DIR/lib/load-survivors.sh"

PROG="load-repro"

# Reported when the matching flag is absent (see "ONE IMPLEMENTATION OF THE
# GATE" — loadgen.py is the source of truth for what is actually allowed).
DEFAULT_WORKERS_REPORT=4
DEFAULT_SECONDS_REPORT=30
DEFAULT_LOAD_THRESHOLD_REPORT=8.0
DEFAULT_LOADAVG_FILE=/proc/loadavg

EXIT_SURVIVORS=1
EXIT_REFUSED=2
EXIT_SKIP=3

_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*" >&2; }

usage() {
  cat <<'EOF'
load-repro.sh — bounded, self-cleaning load reproduction (DF-CRIER-254)

Usage:
  scripts/load-repro.sh [options] -- <command...>

Options:
  --workers N          burner processes (hard cap 8; default 4)
  --seconds S          how long the load lasts (hard cap 300; default 30)
  --load-threshold L   skip when the 1-minute loadavg is strictly above L
                       (default 8.0)
  --loadavg-file PATH  load figure source (default /proc/loadavg); point this at
                       a synthetic file only to test the gate
  --cpus SPEC          pin the load to a CPU set, e.g. 0-3
  -h, --help           this text
  --                   separates the options from the target command

The target runs alongside the load and its exit code is this script's exit code.
Above the threshold nothing is started: the run is SKIPPED (exit 3) with the
recorded reason, and one load run at a time per host is enforced with a lockfile
(admins may move the lock with LOAD_REPRO_LOCKFILE).
EOF
}

# ── argument parsing ──────────────────────────────────────────────────────────
WORKERS=""
SECONDS_ARG=""
THRESHOLD=""
LOADAVG_FILE=""
CPUS=""
CMD=()

while [ "$#" -gt 0 ]; do
  case "$1" in
    --workers)
      [ "$#" -ge 2 ] || { _err "--workers needs a value"; exit "$EXIT_REFUSED"; }
      WORKERS="$2"
      shift 2
      ;;
    --seconds)
      [ "$#" -ge 2 ] || { _err "--seconds needs a value"; exit "$EXIT_REFUSED"; }
      SECONDS_ARG="$2"
      shift 2
      ;;
    --load-threshold)
      [ "$#" -ge 2 ] || { _err "--load-threshold needs a value"; exit "$EXIT_REFUSED"; }
      THRESHOLD="$2"
      shift 2
      ;;
    --loadavg-file)
      [ "$#" -ge 2 ] || { _err "--loadavg-file needs a value"; exit "$EXIT_REFUSED"; }
      LOADAVG_FILE="$2"
      shift 2
      ;;
    --cpus)
      [ "$#" -ge 2 ] || { _err "--cpus needs a value"; exit "$EXIT_REFUSED"; }
      CPUS="$2"
      shift 2
      ;;
    -h | --help | help)
      usage
      exit 0
      ;;
    --)
      shift
      CMD=("$@")
      break
      ;;
    -*)
      _err "unknown option '$1' (try --help)"
      exit "$EXIT_REFUSED"
      ;;
    *)
      _err "unexpected argument '$1' — the target command must follow '--'"
      exit "$EXIT_REFUSED"
      ;;
  esac
done

if [ "${#CMD[@]}" -eq 0 ]; then
  _err "no target command: pass one after '--' (try --help)"
  exit "$EXIT_REFUSED"
fi

if [ ! -f "$LOADGEN" ]; then
  _err "the bounded generator is missing: $LOADGEN"
  exit "$EXIT_REFUSED"
fi
if ! command -v python3 >/dev/null 2>&1; then
  _err "python3 is not on PATH — scripts/loadgen.py is stdlib-only python3 and cannot be skipped"
  exit "$EXIT_REFUSED"
fi
if [ ! -f "$SURVIVORS_LIB" ]; then
  _err "the shared pid-liveness library is missing: $SURVIVORS_LIB"
  exit "$EXIT_REFUSED"
fi
# shellcheck source=lib/load-survivors.sh
. "$SURVIVORS_LIB"

# ── state + teardown ─────────────────────────────────────────────────────────
WORK=""
GEN_PID=""
TARGET_PID=""
GEN_PIDS=()
ALL_PIDS=()
SURVIVORS=()
GENERATOR_RC=""
TARGET_RC=""
SIGNAL_NAME=""
TEARDOWN_DONE=0
GEN_REAPED=0
SUMMARY_PRINTED=0
LOCK_FD_OPEN=0

_cleanup_work() {
  [ -n "$WORK" ] && rm -rf "$WORK"
  WORK=""
}

# Reap the generator exactly once, and never block on a process that is still
# alive (it is a survivor, and it is reported as one instead of hanging the exit).
_reap_generator() {
  [ -n "$GEN_PID" ] || return 0
  [ "$GEN_REAPED" -eq 1 ] && return 0
  GEN_REAPED=1
  if load_pid_alive "$GEN_PID"; then
    return 0
  fi
  wait "$GEN_PID" 2>/dev/null
  GENERATOR_RC=$?
  return 0
}

# Poll (sleeping) until every recorded pid is gone, bounded by <tenths>.
_poll_all_gone() {
  local budget="$1" i=0 p alive=0
  while [ "$i" -lt "$budget" ]; do
    alive=0
    for p in ${ALL_PIDS[@]+"${ALL_PIDS[@]}"}; do
      if load_pid_alive "$p"; then
        alive=1
        break
      fi
    done
    [ "$alive" -eq 0 ] && return 0
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# teardown: TERM everything we started, bounded wait, KILL what is left, VERIFY.
# Idempotent — the EXIT trap and a signal trap can both reach it.
teardown() {
  [ "$TEARDOWN_DONE" -eq 1 ] && return 0
  TEARDOWN_DONE=1
  local p alive=0

  # 1. Ask everything to stop. The generator first: it owns its burners and tears
  #    them down on SIGTERM (and the kernel's PR_SET_PDEATHSIG covers a SIGKILL).
  for p in ${ALL_PIDS[@]+"${ALL_PIDS[@]}"}; do
    if load_pid_alive "$p"; then
      kill -TERM "$p" 2>/dev/null
    fi
  done

  # 2. Bounded wait, then KILL whatever is still alive.
  _poll_all_gone 50 || true
  alive=0
  for p in ${ALL_PIDS[@]+"${ALL_PIDS[@]}"}; do
    if load_pid_alive "$p"; then
      alive=1
      break
    fi
  done
  if [ "$alive" -eq 1 ]; then
    for p in ${ALL_PIDS[@]+"${ALL_PIDS[@]}"}; do
      if load_pid_alive "$p"; then
        kill -KILL "$p" 2>/dev/null
      fi
    done
    _poll_all_gone 20 || true
  fi

  # 3. VERIFY with the shared frontier (a zombie is dead, never a survivor).
  SURVIVORS=()
  while IFS= read -r p; do
    [ -n "$p" ] && SURVIVORS+=("$p")
  done < <(load_survivors_verdict ${ALL_PIDS[@]+"${ALL_PIDS[@]}"} 2>/dev/null)
  # Reap the generator here (the only place) so its exit status is recorded once,
  # on the signal path as well as the normal one.
  _reap_generator
  return 0
}

_json_num_or_null() {
  case "${1:-}" in
    '' | *[!0-9.-]*) printf 'null' ;;
    *) printf '%s' "$1" ;;
  esac
}

_json_int_list() {
  local first=1 p
  for p in ${ALL_PIDS[@]+"${ALL_PIDS[@]}"}; do
    [ -n "$p" ] || continue
    if [ "$first" -eq 1 ]; then
      printf '%s' "$p"
      first=0
    else
      printf ',%s' "$p"
    fi
  done
}

_json_survivor_list() {
  local first=1 p
  for p in ${SURVIVORS[@]+"${SURVIVORS[@]}"}; do
    [ -n "$p" ] || continue
    if [ "$first" -eq 1 ]; then
      printf '%s' "$p"
      first=0
    else
      printf ',%s' "$p"
    fi
  done
}

# Exactly one machine-readable summary line, on stdout.
print_summary() {
  [ "$SUMMARY_PRINTED" -eq 1 ] && return 0
  SUMMARY_PRINTED=1
  local workers="${#GEN_PIDS[@]}" seconds threshold loadavg loadavg_pretty clean signal

  seconds="$(awk -v v="${SECONDS_ARG:-$DEFAULT_SECONDS_REPORT}" 'BEGIN{printf "%g", v}')"
  threshold="$(awk -v v="${THRESHOLD:-$DEFAULT_LOAD_THRESHOLD_REPORT}" 'BEGIN{printf "%g", v}')"
  loadavg="${MEASURED_LOADAVG:-}"
  loadavg_pretty="$(awk -v v="${loadavg:-0}" 'BEGIN{printf "%.2f", v}')"
  if [ "${#SURVIVORS[@]}" -eq 0 ]; then clean=true; else clean=false; fi
  if [ -n "$SIGNAL_NAME" ]; then signal="\"$SIGNAL_NAME\""; else signal=null; fi

  printf 'load-repro: {"event":"summary","workers":%s,"seconds":%s,"loadavg":%s,"threshold":%s,"target_rc":%s,"generator_rc":%s,"pids":[%s],"survivors":[%s],"clean":%s,"signal":%s}\n' \
    "$workers" "$seconds" "$loadavg_pretty" "$threshold" \
    "$(_json_num_or_null "$TARGET_RC")" "$(_json_num_or_null "$GENERATOR_RC")" \
    "$(_json_int_list)" "$(_json_survivor_list)" "$clean" "$signal"

  if [ "${#SURVIVORS[@]}" -gt 0 ]; then
    _err "SURVIVORS after teardown: ${SURVIVORS[*]} — these processes are still alive"
  fi
  return 0
}

_on_exit() {
  local rc=$?
  teardown
  if [ "$SUMMARY_PRINTED" -eq 0 ] && [ -n "$GEN_PID" ]; then
    print_summary
  fi
  _cleanup_work
  return "$rc"
}

_on_signal() {
  local name="$1" code="$2"
  SIGNAL_NAME="$name"
  _info "received SIG$name — tearing down the generator, its burners and the target"
  teardown
  print_summary
  _cleanup_work
  exit "$code"
}

trap '_on_exit' EXIT
trap '_on_signal TERM 143' TERM
trap '_on_signal INT 130' INT
trap '_on_signal HUP 129' HUP

# ── the load gate: relayed from loadgen.py, which owns it ────────────────────
# (Kept in one place on purpose — see "ONE IMPLEMENTATION OF THE GATE" above.)
read_loadavg_first_field() { # <file> -> prints the 1-minute value
  local file="$1" line=""
  if [ ! -r "$file" ]; then
    return 1
  fi
  line="$(head -n 1 "$file" 2>/dev/null)" || return 1
  set -- $line
  [ "$#" -gt 0 ] || return 1
  printf '%s\n' "$1"
  return 0
}

# ── the concurrency lock: one load run at a time per host ────────────────────
LOCKFILE="${LOAD_REPRO_LOCKFILE:-${TMPDIR:-/tmp}/load-repro.$(id -u 2>/dev/null || echo 0).lock}"
exec 9>>"$LOCKFILE" || {
  _err "cannot open the lockfile $LOCKFILE"
  exit "$EXIT_REFUSED"
}
LOCK_FD_OPEN=1
if ! flock -n 9; then
  holder="$(cat "$LOCKFILE" 2>/dev/null | head -n 1)"
  printf 'SKIPPED: another load-repro is already running (holder pid %s) — one load run at a time per host (lockfile %s)\n' \
    "${holder:-unknown}" "$LOCKFILE"
  exit "$EXIT_SKIP"
fi
# The lock is the flock; the content is a best-effort diagnostic so the NEXT run
# can name the holder. Written only while holding the lock.
: >"$LOCKFILE" 2>/dev/null
printf '%s\n' "$$" >"$LOCKFILE" 2>/dev/null

# ── start the bounded generator and wait for its start line ──────────────────
WORK="$(mktemp -d "${TMPDIR:-/tmp}/load-repro.XXXXXX")" || {
  _err "cannot create a scratch directory under ${TMPDIR:-/tmp}"
  exit "$EXIT_REFUSED"
}
GEN_OUT="$WORK/loadgen.out"
GEN_ERR="$WORK/loadgen.err"

GEN_ARGS=(--json)
[ -n "$WORKERS" ] && GEN_ARGS+=(--workers "$WORKERS")
[ -n "$SECONDS_ARG" ] && GEN_ARGS+=(--seconds "$SECONDS_ARG")
[ -n "$THRESHOLD" ] && GEN_ARGS+=(--load-threshold "$THRESHOLD")
[ -n "$LOADAVG_FILE" ] && GEN_ARGS+=(--loadavg-file "$LOADAVG_FILE")
[ -n "$CPUS" ] && GEN_ARGS+=(--cpus "$CPUS")

MEASURED_LOADAVG="$(read_loadavg_first_field "${LOADAVG_FILE:-$DEFAULT_LOADAVG_FILE}" || true)"

# The child is spawned with fd 9 CLOSED (`9>&-`): it must not inherit the run
# lock, or the lock would stay held by a process the wrapper no longer owns (a
# long-lived target, a generator) and the next run would refuse for the wrong
# reason. Closing the child's duplicate does not release the wrapper's own lock.
python3 "$LOADGEN" ${GEN_ARGS[@]+"${GEN_ARGS[@]}"} >"$GEN_OUT" 2>"$GEN_ERR" 9>&- &
GEN_PID=$!

STARTED_CSV=""
i=0
while [ "$i" -lt 150 ]; do # 15s budget for a cold python start on a loaded box
  STARTED_CSV="$(sed -n 's/.* pids=//p' "$GEN_ERR" 2>/dev/null | head -n 1)"
  [ -n "$STARTED_CSV" ] && break
  STARTED_CSV=""
  load_pid_alive "$GEN_PID" || break
  sleep 0.1
  i=$((i + 1))
done

if [ -z "$STARTED_CSV" ]; then
  if load_pid_alive "$GEN_PID"; then
    _err "the generator did not announce its children within 15s — killing it and refusing to run the target"
    kill -TERM "$GEN_PID" 2>/dev/null
    exit "$EXIT_REFUSED"
  fi
  _reap_generator
  # Relay the generator's own verdict verbatim: this is the recorded SKIPPED
  # reason (exit 3) or the refusal/error text (exit 2). The target is NOT run.
  cat "$GEN_OUT" 2>/dev/null
  cat "$GEN_ERR" >&2 2>/dev/null
  _err "the load generator exited (rc=$GENERATOR_RC) before starting — the target was NOT run"
  exit "${GENERATOR_RC:-$EXIT_REFUSED}"
fi

GEN_PIDS=(${STARTED_CSV//,/ })
ALL_PIDS=("$GEN_PID")
p=""
for p in ${GEN_PIDS[@]+"${GEN_PIDS[@]}"}; do
  [ -n "$p" ] && ALL_PIDS+=("$p")
done

_info "generator pid=$GEN_PID workers=${#GEN_PIDS[@]} burners=${STARTED_CSV} — starting the target"

# ── run the target alongside the load ────────────────────────────────────────
# The target runs as an async child (its stdin is /dev/null, standard for a
# background job without job control) so a trapped signal can tear it down
# immediately instead of waiting for it to finish. It inherits neither the run
# lock (`9>&-`) nor any claim to outlive this script: it joins ALL_PIDS below, so
# teardown kills and verifies it like every other child.
"${CMD[@]}" 9>&- &
TARGET_PID=$!
ALL_PIDS+=("$TARGET_PID")
wait "$TARGET_PID"
TARGET_RC=$?

# The load never outlives the target by more than the teardown window, so tear it
# down as soon as the target is done (the generator's own --seconds bound stays
# the backstop if this script is SIGKILLed before it can).
teardown

print_summary
_cleanup_work

if [ "${#SURVIVORS[@]}" -gt 0 ]; then
  exit "$EXIT_SURVIVORS"
fi
exit "$TARGET_RC"
