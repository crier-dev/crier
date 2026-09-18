#!/usr/bin/env bash
#
# scripts/lib/load-survivors.sh — the ONE pid-liveness frontier for the load
# harness (DF-CRIER-254).
#
# WHY THIS EXISTS
# ---------------
# "No child outlived the run" is the whole point of a bounded load harness, and
# it is a claim that is easy to make in the wrong direction: a pid that has been
# killed but not yet reaped still has a /proc/<pid> directory, and a naive
# `kill -0 <pid>` (or an `ls /proc/<pid>`) reports it as a live survivor — which
# turns a clean teardown into a false alarm just often enough that the alarm is
# ignored. Worse, the same naive check in the NEUTER direction cannot be told
# apart from a check that always fails. So liveness lives in exactly one place,
# with one state rule, and both the wrapper and the harness selftest call it.
#
# PUBLIC SURFACE (sourcing runs nothing and prints nothing):
#
#     . "$REPO/scripts/lib/load-survivors.sh"
#
#   load_pid_state PID
#       Prints the process state letter from /proc/<pid>/stat (the field AFTER
#       the last ')' — the comm field is parenthesised and may itself contain
#       spaces and parentheses). Empty output when there is no such entry.
#
#   load_pid_alive PID
#       Returns 0 when the pid is a LIVE process, 1 when it is gone OR is only an
#       unreaped corpse (state Z/z/X/x). A ZOMBIE IS DEAD here: it burns no CPU,
#       runs no code, and every `wait` that could have reaped it has returned.
#       On a host without procfs it falls back to `kill -0`, which cannot tell a
#       zombie apart — documented, and unreachable on the Linux boxes this
#       harness targets.
#
#   load_survivors_verdict PID...
#       Prints one surviving pid per line and RETURNS 1 when at least one pid is
#       still alive, 0 when every pid is gone. This is the verdict every caller
#       keys its exit code on; it is deliberately the LAST statement of the
#       function so a caller cannot accidentally read a survivor list as success.
#
#   load_wait_pid_gone PID [tenths]
#       Poll (sleeping, never spinning) until the pid is gone or the budget
#       (default 100 tenths = 10s) is spent. Returns 0 when it went away.
#
# THE NEUTER MARK
# ---------------
# `load_survivors_verdict` carries `NEUTER-MARK[survivor-verdict]` on its
# verdict line. The harness selftest copies this file, seds exactly that line to
# a success, and requires the COPY to accept a fixture whose process IS still
# alive while the REAL check rejects it — the causality proof that the verdict
# is produced by this check and not by accident (the DF-CRIER-206/209/189
# pattern). If that line is ever rewritten, the selftest's `cmp` guard fails
# loudly instead of going vacuous.

# load_pid_state PID -> the state letter, or nothing when the pid has no stat.
load_pid_state() {
  local pid="$1" stat="" rest=""
  case "$pid" in
    '' | *[!0-9]*) return 1 ;;
  esac
  [ "$pid" -gt 1 ] || return 1
  stat="$(cat "/proc/$pid/stat" 2>/dev/null)" || return 1
  rest="${stat##*)}"
  # Word-split the remainder; the first field is the state letter.
  # shellcheck disable=SC2086
  set -- $rest
  [ "$#" -gt 0 ] || return 1
  printf '%s\n' "$1"
  return 0
}

# load_pid_alive PID -> 0 live, 1 gone-or-zombie.
load_pid_alive() {
  local pid="$1" state=""
  case "$pid" in
    '' | *[!0-9]*) return 1 ;;
  esac
  [ "$pid" -gt 1 ] || return 1
  if state="$(load_pid_state "$pid")"; then
    case "$state" in
      Z | z | X | x) return 1 ;; # unreaped corpse: dead for every purpose here
    esac
    return 0
  fi
  # No procfs entry. Without procfs at all, fall back to the signal probe.
  if [ ! -d /proc ]; then
    if kill -0 "$pid" 2>/dev/null; then
      return 0
    fi
  fi
  return 1
}

# load_survivors_verdict PID... -> prints survivors, 0 clean / 1 with survivors.
load_survivors_verdict() {
  local pid="" survivor_count=0
  local -a survivors=()
  for pid in "$@"; do
    [ -n "$pid" ] || continue
    # SURVIVOR = the pid is still ALIVE. Written as an if, never as
    # `load_pid_alive || survivors+=(...)`: the inverted predicate looks
    # plausible and reports every process that died cleanly as a survivor
    # (measured while building this, DF-CRIER-254).
    if load_pid_alive "$pid"; then
      survivors+=("$pid")
    fi
  done
  survivor_count="${#survivors[@]}"
  # NEUTER-MARK[survivor-verdict]: the verdict. The selftest seds exactly this
  # line in a copy of this file (and asserts the copy changed) to prove that a
  # rejection here is caused by this check.
  if [ "$survivor_count" -gt 0 ]; then
    printf '%s\n' "${survivors[@]}"
    return 1
  fi
  return 0
}

# load_wait_pid_gone PID [tenths] -> 0 when it went away, 1 when the budget spent.
load_wait_pid_gone() {
  local pid="$1" budget="${2:-100}" i=0
  while [ "$i" -lt "$budget" ]; do
    load_pid_alive "$pid" || return 0
    sleep 0.1
    i=$((i + 1))
  done
  load_pid_alive "$pid" && return 1
  return 0
}
