#!/usr/bin/env bash
#
# scripts/wait-for-command.sh — a bounded, PID/marker-anchored wait primitive for
# anything that must observe a long command finish (CRIER-HARNESS-016).
#
# WHY THIS EXISTS
# ---------------
# The GitReins tier-2 judge's `run_command` tool has a HARD 30-second timeout
# (gitreins `engine/evaluator.py`, `command_timeout`; neither 0.17.1 nor 0.18.1
# exposes a config key for it — measured, see docs/judge-wait.md), so a suite
# that takes minutes cannot be run to completion inside one call. It has to be
# WAITED ON. Twice the invented wait loop has been a pattern search of the
# process table inside a shell wrapper whose own command line CONTAINED the
# pattern it searched for: the wrapper matches ITSELF, so the loop reports
# RUNNING forever while the job it waits for has long since exited. Both dated
# occurrences (2026-11-03, 2026-11-07 on RELEASE-015 at 08a54216) are written up
# in docs/judge-wait.md.
#
# THE RULE THIS TOOL ENCODES
# --------------------------
# Completion is observed from ONE OF TWO ANCHORS, and both are RECORDED DATA
# rather than an inference about the process table:
#
#   --pid <PID> / --pid-file <path>   the PID the caller RECORDED when it started
#                                     the job. We poll THAT pid's liveness
#                                     (/proc/<pid> plus kill -0, zombie-aware)
#                                     and can optionally verify that
#                                     /proc/<pid>/cmdline still names the
#                                     expected command.
#   --log <path> --marker <regex>     a completion line the job itself wrote to its
#                                     own log; we poll `grep -E` of that file.
#
# This script NEVER searches the process table by name or pattern, and there is
# no name/pattern mode to reach for: a name-shaped request is REFUSED (exit 2)
# with the self-match reason, because that is the exact class this tool exists
# to remove. Identity, when it is checked at all, is read from
# /proc/<the-one-recorded-pid>/cmdline — a file belonging to the process the
# caller already named, never a scan.
#
# BOUNDED, ALWAYS. `--timeout` defaults to 600s and above 3600s the request is
# REFUSED rather than clamped: a wait longer than an hour belongs to a
# supervisor, not to a primitive a judge runs inside a tool call. The sleep
# cadence is `--interval` (default 2s) and is capped by the time remaining, so
# the bound is honoured to within a second. There is no busy-wait.
#
# OUTPUT CONTRACT: exactly one line on stdout, always the last thing printed:
#
#   STATUS=done|timeout pid=<pid|-> elapsed=<seconds> mode=<pid|marker>
#
# EXIT CODES: 0 = done (pid gone / marker appeared); 4 = timeout expired first;
# 2 = refused (bad or conflicting arguments, unbounded timeout, pid identity
# mismatch, unreadable pid file, a request that would need process-name
# matching). Diagnostics go to stderr; stdout carries the STATUS line only.
#
# DEPENDENCIES: bash, coreutils (sleep, awk, grep), /proc.

set -uo pipefail

PROG="wait-for-command"

# Hard limits. Both are REFUSALS, not clamps (fleet doctrine: a silent clamp
# hides the caller's mistake).
readonly MAX_TIMEOUT=3600
readonly MAX_INTERVAL=60

readonly SELF_MATCH_REASON="process-name matching is refused — a pattern that appears in the caller's own command line (or in this one) matches the waiter itself, so the wait can never observe completion. Anchor on a RECORDED --pid / --pid-file, or on --log + --marker."

usage() {
  cat <<'USAGE'
wait-for-command.sh — bounded wait on a RECORDED pid, or on a log marker.

  scripts/wait-for-command.sh --pid <PID>                    [options]
  scripts/wait-for-command.sh --pid-file <path>              [options]
  scripts/wait-for-command.sh --log <path> --marker <regex>  [options]

Modes (give exactly one):
  --pid <PID>           wait until the pid you recorded is GONE
  --pid-file <path>     read that pid from a file (first non-empty line)
  --log <path>          path of the job's OWN log
  --marker <regex>      extended regex; done when that log matches it (needs --log)

Options:
  --timeout <seconds>    overall bound; default 600, hard cap 3600 (a longer wait
                         is refused — this tool is never unbounded)
  --interval <seconds>   poll cadence; default 2, max 60 (never a busy-wait)
  --expect-cmdline <s>   pid mode only: refuse (exit 2) when /proc/<pid>/cmdline
                         no longer contains <s> — a reused or wrong pid
  -h, --help             print this text and exit 0 (the one mode with no
                         STATUS line)

Output — exactly one line on stdout when it finishes:

  STATUS=done|timeout pid=<pid|-> elapsed=<seconds> mode=<pid|marker>

Exit codes:
  0  the wait finished: the recorded pid is gone, or the marker appeared
  4  the timeout expired first
  2  refused / unusable input (bad arguments, unbounded timeout, pid identity
     mismatch, unreadable pid file, a request needing process-name matching)

There is deliberately no --name / --pattern mode: matching a process by name
from inside a wrapper matches the wrapper's own argv. See docs/judge-wait.md.
USAGE
}

refuse() {
  printf '%s: REFUSED: %s\n' "$PROG" "$*" >&2
  exit 2
}

warn() {
  printf '%s: %s\n' "$PROG" "$*" >&2
}

# ---------------------------------------------------------------- arg parsing
PID=""
PID_FILE=""
LOG=""
MARKER=""
EXPECT_CMDLINE=""
TIMEOUT=600
INTERVAL=2
HAVE_EXPECT_CMDLINE=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --pid)
      [ "$#" -ge 2 ] || refuse "--pid needs a value"
      PID="$2"
      shift 2
      ;;
    --pid-file)
      [ "$#" -ge 2 ] || refuse "--pid-file needs a value"
      PID_FILE="$2"
      shift 2
      ;;
    --log)
      [ "$#" -ge 2 ] || refuse "--log needs a value"
      LOG="$2"
      shift 2
      ;;
    --marker)
      [ "$#" -ge 2 ] || refuse "--marker needs a value"
      MARKER="$2"
      shift 2
      ;;
    --timeout)
      [ "$#" -ge 2 ] || refuse "--timeout needs a value"
      TIMEOUT="$2"
      shift 2
      ;;
    --interval)
      [ "$#" -ge 2 ] || refuse "--interval needs a value"
      INTERVAL="$2"
      shift 2
      ;;
    --expect-cmdline)
      [ "$#" -ge 2 ] || refuse "--expect-cmdline needs a value"
      EXPECT_CMDLINE="$2"
      HAVE_EXPECT_CMDLINE=1
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    # Name/pattern-shaped options: refused loudly, with the reason, instead of
    # being ignored or silently reinterpreted. These are the self-match class.
    --name | --process | --process-name | --pattern | --pgrep | --pkill | --match-argv)
      refuse "$1 is not supported. $SELF_MATCH_REASON"
      ;;
    -*)
      refuse "unknown option '$1'. $SELF_MATCH_REASON"
      ;;
    *)
      refuse "unexpected argument '$1' — this tool takes options only (see --help)"
      ;;
  esac
done

# --------------------------------------------------------------- validation
if [ -n "$PID" ] && [ -n "$PID_FILE" ]; then
  refuse "give exactly one of --pid and --pid-file (both were given)"
fi
if [ -n "$LOG" ] || [ -n "$MARKER" ]; then
  if [ -n "$PID" ] || [ -n "$PID_FILE" ]; then
    refuse "give exactly one wait mode: --pid/--pid-file OR --log + --marker (both were given)"
  fi
  [ -n "$LOG" ] || refuse "--marker needs --log <path> (the job's OWN log; nothing else is grepped)"
  [ -n "$MARKER" ] || refuse "--log needs --marker <regex> (the completion line the job writes)"
fi

case "$TIMEOUT" in
  '' | *[!0-9]*) refuse "--timeout must be a positive integer number of seconds (got '$TIMEOUT')" ;;
esac
[ "$TIMEOUT" -ge 1 ] || refuse "--timeout 0 would be unbounded; this tool is always bounded (1..$MAX_TIMEOUT)"
[ "$TIMEOUT" -le "$MAX_TIMEOUT" ] || refuse "--timeout ${TIMEOUT}s exceeds the hard cap of ${MAX_TIMEOUT}s — a wait longer than that belongs to a supervisor, not to this primitive"

case "$INTERVAL" in
  '' | *[!0-9]*) refuse "--interval must be a positive integer number of seconds (got '$INTERVAL')" ;;
esac
[ "$INTERVAL" -ge 1 ] || refuse "--interval 0 would be a busy-wait; the cadence floor is 1s"
[ "$INTERVAL" -le "$MAX_INTERVAL" ] || refuse "--interval ${INTERVAL}s exceeds the maximum cadence of ${MAX_INTERVAL}s"

MODE=""

if [ -n "$PID_FILE" ]; then
  [ -f "$PID_FILE" ] || refuse "--pid-file '$PID_FILE' does not exist"
  [ -r "$PID_FILE" ] || refuse "--pid-file '$PID_FILE' is not readable"
  PID="$(awk 'NF { print $1; exit }' "$PID_FILE" 2>/dev/null)"
  [ -n "$PID" ] || refuse "--pid-file '$PID_FILE' holds no pid (no non-empty line)"
fi

if [ -n "$PID" ]; then
  MODE=pid
  case "$PID" in
    '' | *[!0-9]*) refuse "--pid must be a positive integer (got '$PID')" ;;
  esac
  [ "$PID" -gt 0 ] || refuse "--pid must be greater than 0 (got '$PID')"
  [ "$PID" -ne "$$" ] || refuse "--pid $$ is this waiter's own pid — waiting on yourself can never finish; record the job's pid when you start it"
  if [ "$HAVE_EXPECT_CMDLINE" -eq 1 ]; then
    [ -n "$EXPECT_CMDLINE" ] || refuse "--expect-cmdline needs a non-empty substring"
  fi
elif [ -n "$LOG" ]; then
  MODE=marker
  if [ "$HAVE_EXPECT_CMDLINE" -eq 1 ]; then
    refuse "--expect-cmdline applies to --pid/--pid-file only; in marker mode the log is the anchor"
  fi
else
  refuse "no wait mode given — give --pid <PID>, --pid-file <path>, or --log <path> --marker <regex> (see --help)"
fi

# ------------------------------------------------------------------ helpers

# _pid_alive <pid>: true while that pid is a LIVE (non-zombie) process.
# /proc/<pid> is the authority; kill -0 confirms the caller may signal it. A
# zombie is reported as NOT alive: it has exited, and only its parent's reap is
# outstanding, which is not something this primitive should wait for.
_pid_alive() {
  local pid="$1" stat_line="" rest="" state=""
  [ -d "/proc/$pid" ] || return 1
  if [ -r "/proc/$pid/stat" ]; then
    IFS= read -r stat_line <"/proc/$pid/stat" 2>/dev/null || stat_line=""
    # Fields after the parenthesised comm: "<state> <ppid> ...". comm may contain
    # spaces and parentheses, so split at the LAST ')' and take the first token.
    rest="${stat_line##*) }"
    state="${rest%% *}"
    [ "$state" = "Z" ] && return 1
  fi
  kill -0 "$pid" 2>/dev/null || return 1
  return 0
}

# _cmdline_matches <pid> <substring>: fixed-string test against that pid's OWN
# /proc/<pid>/cmdline. Never a process-table scan.
_cmdline_matches() {
  local pid="$1" needle="$2" cmdline=""
  [ -r "/proc/$pid/cmdline" ] || return 1
  cmdline="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)" || return 1
  case "$cmdline" in
    *"$needle"*) return 0 ;;
    *) return 1 ;;
  esac
}

# _marker_present: the job's own log contains a line matching the marker regex.
_marker_present() {
  [ -f "$LOG" ] || return 1
  [ -r "$LOG" ] || return 1
  grep -Eq -- "$MARKER" "$LOG" 2>/dev/null
}

# _nap: sleep --interval, but never past the remaining budget, so the overall
# bound is honoured to within a second.
_nap() {
  local remaining=$((TIMEOUT - (SECONDS - START)))
  local secs="$INTERVAL"
  [ "$secs" -le "$remaining" ] || secs="$remaining"
  [ "$secs" -ge 1 ] || secs=1
  sleep "$secs"
}

# ---------------------------------------------------------------- the wait
START=$SECONDS
STATUS=""
PID_OUT="-"

case "$MODE" in
  pid)
    PID_OUT="$PID"
    while :; do
      if ! _pid_alive "$PID"; then
        STATUS=done
        break
      fi
      if [ "$HAVE_EXPECT_CMDLINE" -eq 1 ] && ! _cmdline_matches "$PID" "$EXPECT_CMDLINE"; then
        warn "--pid $PID is alive but /proc/$PID/cmdline no longer contains '$EXPECT_CMDLINE' — the recorded pid now belongs to a different command (pid reuse). Refusing to wait on it."
        exit 2
      fi
      if [ $((SECONDS - START)) -ge "$TIMEOUT" ]; then
        STATUS=timeout
        break
      fi
      _nap
    done
    ;;
  marker)
    while :; do
      if _marker_present; then
        STATUS=done
        break
      fi
      if [ $((SECONDS - START)) -ge "$TIMEOUT" ]; then
        STATUS=timeout
        break
      fi
      _nap
    done
    ;;
  *)
    refuse "internal: unhandled mode '$MODE'"
    ;;
esac

ELAPSED=$((SECONDS - START))
printf 'STATUS=%s pid=%s elapsed=%s mode=%s\n' "$STATUS" "$PID_OUT" "$ELAPSED" "$MODE"

[ "$STATUS" = "done" ] && exit 0
exit 4
