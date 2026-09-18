#!/usr/bin/env bash
#
# scripts/lib/port-guard.sh — shared "the server we measure is the server we
# started" guards for the example harnesses (QA-CRIER-9).
#
# Sourced (bash 4+), and safe under `set -euo pipefail`:
#
#     . "$REPO_ROOT/scripts/lib/port-guard.sh"
#
# WHY THIS EXISTS
# ---------------
# Three of the four example harnesses started a crier server on a fixed scratch
# port and then only polled GET /health. When a stale or foreign process already
# held that port, the freshly built binary died with
# "listen tcp :<port>: bind: address already in use" and the health poll was
# answered by the SQUATTER — the run then reported success for a binary it never
# started. That is the phantom-green class QA-CRIER-9 recorded for the E2E
# battery (14/14 "passed" against a 9-hour-old leftover server whose binary had
# already been deleted).
#
# PUBLIC SURFACE (each one EXITS nonzero on failure — they are called from the
# harnesses' main flow, where a guard that fails must abort the run):
#
#   port_holder_pid <port>
#       Print the pid listening on <port>; nothing when the port is free.
#       (Empty is also possible when the holder belongs to another user — its
#       pid is not visible to a non-root `ss`; use the exit-status functions to
#       tell "free" from "held by someone else".)
#
#   require_free_port <port> <label>
#       Refuse to start <label> while anything listens on <port>: print the port,
#       the holder pid, the holder's command line (/proc/<pid>/cmdline) and the
#       exact audit command, then exit 1. Never falls through to a server start.
#
#   assert_port_owned <port> <pid> <label>
#       After <label> answered /health, the LISTENING pid on <port> must be <pid>.
#       A mismatch (or a port nobody listens on, or a holder whose pid is not
#       visible to this user) prints both pids and the holder's command line and
#       exits 1 — fail closed, because "I cannot prove ownership" is not ownership.
#
#   wait_http_or_die <url> <pid> <logfile> <label>
#       Poll <url> until a 2xx (bounded, ~20s). If <pid> exits first — the server
#       died on the port we just checked — print the tail of <logfile> and exit 1
#       instead of papering the death over with a later connection error.
#
# DEPENDENCIES: bash 4+, coreutils, ss (iproute2), curl. No lsof/pgrep/fuser.
# EXIT CODES:   1 = the situation the guard exists for (abort the run),
#               2 = misuse (bad argument) or a missing dependency.
#
# SELFTEST (used by `make port-guard-selftest` and CI):
#
#     bash scripts/lib/port-guard.sh --selftest
#
# It exercises all three guards on a decoy listener whose port it PICKS ITSELF as
# free (never a fixed port, so a busy CI runner cannot make it flake), printing
# one PASS line per guard and exiting 0 only when all three behaved.
#
# It also proves the decoy BIND that precedes them, because the pick->bind window
# is unguarded: a concurrent job on a shared runner can take the candidate port
# between the `ss` probe and python3's bind — that is how CI run 35310987548 died
# (`FAIL: throwaway listener never bound` with EADDRINUSE) with no retry. Three
# arms, all driven by a PATH shim for python3 so they are deterministic:
#
#   ARM A — a python3 whose FIRST bind fails with EADDRINUSE must make the
#           selftest rotate to a fresh candidate and still pass end to end,
#           naming the abandoned port and the port it rotated to (a silent
#           rotation would hide the recovery).
#   ARM B — when every candidate in the rotation budget is lost, the selftest
#           fails closed (exit 1) naming the budget and every attempted port, and
#           prints no PASS for a decoy that never bound.
#   ARM C — a FOREIGN listener on the candidate port is never adopted as the
#           decoy: the port must be held by the pid this selftest started, not
#           merely by SOMETHING — the phantom-green QA-CRIER-9 exists for.
#
# The arms re-run this script as a CHILD process with the arms disabled
# (`PG_SELFTEST_SKIP_ARMS=1`), so the recursion is bounded at one level and the
# child is the same selftest CI runs. Budgets are overridable:
# PG_SELFTEST_DECOY_TRIES (rotation attempts, default 5) and
# PG_SELFTEST_BIND_TRIES (bounded bind wait per attempt, default 50 x 0.1s).
#
# shellcheck shell=bash disable=SC2155,SC2317

# ── bash version floor ────────────────────────────────────────────────────────
if [ "${BASH_VERSINFO[0]:-0}" -lt 4 ]; then
  echo "port-guard.sh requires bash 4+ (running ${BASH_VERSION:-unknown})" >&2
  return 2 2>/dev/null || exit 2
fi

# ── internal helpers ──────────────────────────────────────────────────────────

_pg_require_tool() { # <name> — exit 2 when a hard dependency is missing
  command -v "$1" >/dev/null 2>&1 && return 0
  echo "ERROR: port-guard.sh needs '$1' on PATH ($2)" >&2
  exit 2
}

_pg_require_ss() {
  _pg_require_tool ss "iproute2 — it is how the guard finds the port holder"
}

# _pg_listen_line <port> — the `ss -tlnp` LISTEN line for <port>, empty if free.
# Matched on the Local Address:Port field so 127.0.0.1:P, *:P and [::]:P all hit.
_pg_listen_line() {
  local port="${1:-}"
  [ -n "$port" ] || return 0
  ss -tlnp 2>/dev/null | awk -v suffix=":${port}" '$1 == "LISTEN" && $4 ~ (suffix "$") { print; exit }'
}

# _pg_pid_from_line <ss-line> — the pid inside users:(("cmd",pid=N,fd=M)), empty
# when the line carries no visible pid (holder owned by another user).
_pg_pid_from_line() {
  local line="${1:-}" after="${1#*pid=}" pid=""
  if [ "$after" != "$line" ]; then
    after="${after%%,*}"   # up to the next users: field
    after="${after%%)*}"   # and up to the closing paren
    case "$after" in
      '' | *[!0-9]*) pid="" ;;
      *) pid="$after" ;;
    esac
  fi
  printf '%s' "$pid"
}

# _pg_cmdline <pid> — the holder's command line, or a reason it is unknown.
_pg_cmdline() {
  local pid="${1:-}" raw=""
  if [ -z "$pid" ]; then
    printf 'unknown (the pid is not visible to uid %s — the holder runs as another user)' "$(id -u)"
    return 0
  fi
  if [ -r "/proc/$pid/cmdline" ]; then
    raw="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)" || raw=""
    raw="${raw% }"
  fi
  if [ -n "$raw" ]; then
    printf '%s' "$raw"
  else
    printf 'unknown (no readable /proc/%s/cmdline — the holder may have exited)' "$pid"
  fi
}

_pg_log_tail() { # <logfile> — last lines of a server log, for a death report
  local log="${1:-}"
  [ -n "$log" ] && [ -f "$log" ] && tail -n 12 "$log" 2>/dev/null
  return 0
}

_pg_check_port_arg() { # <port> <func>
  case "${1:-}" in
    '' | *[!0-9]*)
      echo "ERROR: $2: port must be a TCP port number (got '${1:-}')" >&2
      exit 2
      ;;
  esac
  return 0
}

# ── public: who holds the port ────────────────────────────────────────────────

port_holder_pid() { # <port>
  local port="${1:-}"
  [ -n "$port" ] || return 0
  _pg_require_ss
  _pg_pid_from_line "$(_pg_listen_line "$port")"
}

# ── public: refuse to start on an occupied port ───────────────────────────────

require_free_port() { # <port> <label>
  local port="${1:-}" label="${2:-server}"
  _pg_check_port_arg "$port" require_free_port
  _pg_require_ss

  local line pid cmd
  line="$(_pg_listen_line "$port")"
  [ -n "$line" ] || return 0 # free — start the server

  pid="$(_pg_pid_from_line "$line")"
  cmd="$(_pg_cmdline "$pid")"

  {
    echo "ERROR: refusing to start $label — TCP port :$port is already in use."
    echo "ERROR:   holder pid : ${pid:-unknown (not visible to uid $(id -u))}"
    echo "ERROR:   holder cmd : $cmd"
    echo "ERROR:   audit with : ss -tlnp | grep :$port"
    echo "ERROR: a server started now would die on \"bind: address already in use\","
    echo "ERROR: and $label's /health poll would then be answered by the process above —"
    echo "ERROR: the run would measure a server it did not start."
    echo "ERROR: free the port, or point this harness at another one (see its header"
    echo "ERROR: for the port override environment variable)."
  } >&2
  exit 1
}

# ── public: the listener we just polled must be OUR process ───────────────────

assert_port_owned() { # <port> <pid> <label>
  local port="${1:-}" want="${2:-}" label="${3:-server}"
  _pg_check_port_arg "$port" assert_port_owned
  _pg_require_ss

  local line pid cmd
  line="$(_pg_listen_line "$port")"
  pid="$(_pg_pid_from_line "$line")"
  cmd="$(_pg_cmdline "$pid")"

  if [ -z "$line" ]; then
    {
      echo "ERROR: nothing listens on :$port, but $label claims to be up — refusing to continue."
      echo "ERROR:   started pid : ${want:-?} — it does not hold the port"
      echo "ERROR:   audit with  : ss -tlnp | grep :$port"
    } >&2
    exit 1
  fi

  [ "$pid" = "$want" ] && return 0

  {
    echo "ERROR: $label is NOT the process holding :$port — refusing to measure it."
    echo "ERROR:   started pid : ${want:-?}"
    echo "ERROR:   holder pid  : ${pid:-unknown (not visible to uid $(id -u))}"
    echo "ERROR:   holder cmd  : $cmd"
    echo "ERROR:   audit with  : ss -tlnp | grep :$port"
    echo "ERROR: the /health answer came from a process this harness did not start;"
    echo "ERROR: its results say nothing about the build under test."
  } >&2
  exit 1
}

# ── public: wait for /health, but not through a dead process ──────────────────

wait_http_or_die() { # <url> <pid> <logfile> <label>
  local url="${1:-}" pid="${2:-}" log="${3:-}" label="${4:-server}"
  local tries="${PORT_GUARD_TRIES:-100}" i=0 code=""

  [ -n "$url" ] || {
    echo "ERROR: wait_http_or_die: <url> required" >&2
    exit 2
  }
  _pg_require_tool curl "it is how the guard polls /health"

  while [ "$i" -lt "$tries" ]; do
    if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then
      sleep 0.2 # let a just-died process flush its last line into the log
      {
        echo "ERROR: $label (pid $pid) exited before it answered $url."
        echo "ERROR:   last lines of ${log:-<no log>}:"
        _pg_log_tail "$log" | while IFS= read -r l; do echo "ERROR:     $l"; done
        echo "ERROR: a started process that dies must abort the run — never be papered over."
      } >&2
      exit 1
    fi
    if code="$(curl -sS -o /dev/null -w '%{http_code}' -m 2 "$url" 2>/dev/null)"; then
      case "$code" in
        2??) return 0 ;;
      esac
    fi
    sleep 0.2
    i=$((i + 1))
  done

  {
    echo "ERROR: $label never answered $url with 2xx (waited ~$((tries / 5))s, last code '${code:-none}')."
    echo "ERROR:   started pid : ${pid:-unknown} (still alive, so it is not serving /health)"
    echo "ERROR:   last lines of ${log:-<no log>}:"
    _pg_log_tail "$log" | while IFS= read -r l; do echo "ERROR:     $l"; done
  } >&2
  exit 1
}

# ── selftest ──────────────────────────────────────────────────────────────────

_pg_selftest_cleanup() {
  if [ -n "${_pg_SELFTEST_LISTENER_PID:-}" ]; then
    kill "$_pg_SELFTEST_LISTENER_PID" 2>/dev/null || true
  fi
  # The ARM C fixture's foreign listener is never adopted as a decoy, but it is
  # this selftest's own fixture, so it must not outlive the run either.
  if [ -n "${_pg_SELFTEST_FOREIGN_PID:-}" ]; then
    kill "$_pg_SELFTEST_FOREIGN_PID" 2>/dev/null || true
  fi
  if [ -n "${_pg_SELFTEST_TMP:-}" ]; then
    rm -rf "$_pg_SELFTEST_TMP"
  fi
  return 0
}

_pg_selftest_free_port() { # prints a port nothing is listening on
  local tries=0 port=""
  while [ "$tries" -lt 200 ]; do
    port=$((20000 + RANDOM % 40000))
    if [ -z "$(_pg_listen_line "$port")" ]; then
      printf '%s' "$port"
      return 0
    fi
    tries=$((tries + 1))
  done
  return 1
}

# _pg_selftest_listener_start <port> <log> — start a throwaway listener in the
# CURRENT shell and record its pid in _pg_SELFTEST_LISTENER_PID, so the EXIT trap
# can always kill it. (Called through a command substitution it would run in a
# subshell and the listener would outlive the selftest — the orphaned decoy that
# made this a real bug the first time round.) Returns 0 when a listener was
# started, 2 when neither python3 nor nc exists (a missing dependency). A
# listener that starts and then DIES on the port is NOT a start failure: it is
# the pick->bind window being lost, which _pg_selftest_decoy_ready detects and
# _pg_selftest_start_decoy recovers from.
_pg_selftest_listener_start() {
  local port="$1" log="$2"
  _pg_SELFTEST_LISTENER_PID=""
  if command -v python3 >/dev/null 2>&1; then
    (cd "$_pg_SELFTEST_TMP" && exec python3 -m http.server "$port" --bind 127.0.0.1) >"$log" 2>&1 &
    _pg_SELFTEST_LISTENER_PID=$!
  elif command -v nc >/dev/null 2>&1; then
    nc -l 127.0.0.1 "$port" >"$log" 2>&1 &
    _pg_SELFTEST_LISTENER_PID=$!
  else
    echo "port-guard selftest: FAIL: need python3 (or nc) to create a throwaway listener" >&2
    return 2
  fi
  return 0
}

# _pg_selftest_decoy_ready <port> <pid> <log> — bounded wait (~<tries> x 0.1s) for
# the pid WE started to be the one holding <port>, and returns 0 only on PROVEN
# OWNERSHIP. Presence is not ownership: a candidate port held by any other pid (a
# foreign listener that took it inside the pick->bind window) is refused, never
# adopted — adopting it would make the guards measure a process this selftest did
# not start, which is the phantom-green class QA-CRIER-9 recorded. On failure sets
# _pg_SELFTEST_TRY_REASON to the attributable reason and returns 1.
_pg_selftest_decoy_ready() {
  local port="$1" pid="$2" log="$3" i=0 tries="${PG_SELFTEST_BIND_TRIES:-50}"
  local line="" holder=""
  _pg_SELFTEST_TRY_REASON=""
  while [ "$i" -lt "$tries" ]; do
    line="$(_pg_listen_line "$port")"
    if [ -n "$line" ]; then
      holder="$(_pg_pid_from_line "$line")"
      if [ "$holder" = "$pid" ]; then
        return 0
      fi
      _pg_SELFTEST_TRY_REASON="port :$port is held by pid ${holder:-unknown (not visible to uid $(id -u))}, not the decoy pid $pid this selftest started"
      return 1
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      _pg_SELFTEST_TRY_REASON="pid $pid exited before it bound :$port ($(tail -n 1 "$log" 2>/dev/null))"
      return 1
    fi
    sleep 0.1
    i=$((i + 1))
  done
  _pg_SELFTEST_TRY_REASON="pid $pid never bound :$port within $((tries / 10))s"
  return 1
}

# _pg_selftest_start_decoy <log> — fold picking and binding into ONE bounded
# rotate-and-retry loop, because the pick->bind window is unguarded: a concurrent
# job on a shared runner can take the port between the `ss` probe and python3's
# bind, and the single-attempt version aborted the whole selftest there (CI run
# 35310987548: EADDRINUSE, listener dead, no retry). Each attempt picks a FRESH
# candidate, starts the listener in this shell, and requires the port to be
# provably held by the pid it started. Sets:
#   _pg_SELFTEST_DECOY_PORT          the port held by our live listener
#   _pg_SELFTEST_LISTENER_PID        that listener's pid
#   _pg_SELFTEST_DECOY_ATTEMPT       the attempt that succeeded
#   _pg_SELFTEST_DECOY_BUDGET        the budget it was measured against
#   _pg_SELFTEST_DECOY_ATTEMPTED     " :p1 :p2 …" every candidate tried
#   _pg_SELFTEST_DECOY_ABANDONED_PORTS  "p1 p2 …" every candidate abandoned
#   _pg_SELFTEST_DECOY_ABANDONED     one ":port — reason" line per abandoned
#                                    attempt, newline-separated, so the recovery
#                                    is attributable in the output (a silent
#                                    rotation is not acceptable)
# Returns 0 on success, 1 when the budget is exhausted (fail closed: no PASS may
# be printed for a decoy that is not proven ours), 2 when python3/nc is missing.
_pg_selftest_start_decoy() {
  local log="$1"
  local budget="${PG_SELFTEST_DECOY_TRIES:-5}"
  local attempt=1 port="" pid=""
  _pg_SELFTEST_DECOY_PORT=""
  _pg_SELFTEST_LISTENER_PID=""
  _pg_SELFTEST_DECOY_ATTEMPT=0
  _pg_SELFTEST_DECOY_BUDGET="$budget"
  _pg_SELFTEST_DECOY_ATTEMPTED=""
  _pg_SELFTEST_DECOY_ABANDONED_PORTS=""
  _pg_SELFTEST_DECOY_ABANDONED=""

  while [ "$attempt" -le "$budget" ]; do
    port="$(_pg_selftest_free_port)" || {
      echo "port-guard selftest: FAIL: could not find a free candidate port for the decoy listener (attempt $attempt/$budget, tried:${_pg_SELFTEST_DECOY_ATTEMPTED:- none})" >&2
      return 1
    }
    _pg_SELFTEST_DECOY_ATTEMPTED="${_pg_SELFTEST_DECOY_ATTEMPTED} :$port"

    if ! _pg_selftest_listener_start "$port" "$log"; then
      echo "port-guard selftest: FAIL: no throwaway listener available (see the message above)" >&2
      return 2
    fi
    pid="$_pg_SELFTEST_LISTENER_PID"

    if _pg_selftest_decoy_ready "$port" "$pid" "$log"; then
      _pg_SELFTEST_DECOY_PORT="$port"
      _pg_SELFTEST_DECOY_ATTEMPT="$attempt"
      echo "port-guard selftest: candidate :$port is held by the decoy this selftest started (pid $pid, attempt $attempt/$budget)" >&2
      return 0
    fi

    # Abandon this attempt: kill the pid WE started (a no-op when it already died
    # on the port) and record it. A FOREIGN holder is never killed — it is not
    # ours to kill; we rotate away from it instead.
    kill "$pid" 2>/dev/null || true
    _pg_SELFTEST_DECOY_ABANDONED_PORTS="${_pg_SELFTEST_DECOY_ABANDONED_PORTS} $port"
    _pg_SELFTEST_DECOY_ABANDONED="${_pg_SELFTEST_DECOY_ABANDONED}:$port — ${_pg_SELFTEST_TRY_REASON}
"
    echo "port-guard selftest: abandoned candidate :$port (attempt $attempt/$budget) — ${_pg_SELFTEST_TRY_REASON}; rotating to a fresh candidate" >&2
    attempt=$((attempt + 1))
  done

  {
    echo "port-guard selftest: FAIL: could not start a decoy listener in $budget attempt(s) — every candidate port was lost to another process, or never held by the pid this selftest started (tried:${_pg_SELFTEST_DECOY_ATTEMPTED})"
    printf '%s\n' "$_pg_SELFTEST_DECOY_ABANDONED" | while IFS= read -r line; do
      if [ -n "$line" ]; then
        echo "  $line"
      fi
    done
    echo "  a shared runner races this pick->bind window; re-run, or raise PG_SELFTEST_DECOY_TRIES (currently $budget)"
  } >&2
  return 1
}

# _pg_selftest_child <self> <shim-dir> <count-file> <fail-count> [foreign-pids]
#                    [foreign-log] [foreign]
# Run this selftest as a CHILD process with <shim-dir> first on PATH and the
# arms disabled, so a bind failure is replayable deterministically. Prints the
# child's combined output and returns the child's exit status (0 = pass, 1 = the
# situation the guard exists for, 2 = misuse/missing dependency).
_pg_selftest_child() {
  local self="$1" shim="$2" count="$3" fails="$4" fpid="${5:-}" flog="${6:-}" foreign="${7:-0}"
  local out="" rc=0
  printf '0\n' >"$count"
  if [ "$foreign" = "1" ]; then
    out="$(env PATH="$shim:$PATH" PG_SHIM_COUNT="$count" PG_SHIM_FAILS="$fails" \
      PG_SHIM_FOREIGN=1 PG_SHIM_FOREIGN_PID="$fpid" PG_SHIM_FOREIGN_LOG="$flog" \
      PG_SELFTEST_SKIP_ARMS=1 bash "$self" --selftest 2>&1)" || rc=$?
  else
    out="$(env PATH="$shim:$PATH" PG_SHIM_COUNT="$count" PG_SHIM_FAILS="$fails" \
      PG_SELFTEST_SKIP_ARMS=1 bash "$self" --selftest 2>&1)" || rc=$?
  fi
  printf '%s' "$out"
  return "$rc"
}

_pg_selftest() {
  local fails=0 checks=0
  # BASH_SOURCE[0] inside this function is the file that DEFINED it — the script
  # the decoy-bind arms re-run as a child. A caller may override it explicitly.
  local self="${1:-${BASH_SOURCE[0]}}"
  local arms_skipped=0
  if [ "${PG_SELFTEST_SKIP_ARMS:-0}" = "1" ]; then
    arms_skipped=1
  fi
  if [ ! -f "$self" ]; then
    echo "port-guard selftest: FAIL: cannot locate this script (${self:-unknown}) to re-run the decoy-bind arms" >&2
    return 2
  fi

  _pg_require_ss
  _pg_require_tool curl "the selftest drives wait_http_or_die"

  _pg_SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/port-guard-selftest.XXXXXX")" || {
    echo "port-guard selftest: FAIL: mktemp -d failed" >&2
    return 1
  }
  _pg_SELFTEST_LISTENER_PID=""
  _pg_SELFTEST_FOREIGN_PID=""
  trap '_pg_selftest_cleanup' EXIT

  local tmp="$_pg_SELFTEST_TMP"
  local decoy_log="$tmp/decoy.log" decoy_rc=0
  _pg_selftest_start_decoy "$decoy_log" || decoy_rc=$?
  if [ "$decoy_rc" -ne 0 ]; then
    if [ "$decoy_rc" -eq 2 ]; then
      return 2
    fi
    return 1
  fi
  local port="$_pg_SELFTEST_DECOY_PORT" decoy_pid="$_pg_SELFTEST_LISTENER_PID"

  # A rotation is NEVER silent: the abandoned candidate(s) and the port that
  # replaced them are named, so a recovered pick->bind race is attributable in
  # the log instead of looking like an ordinary run.
  if [ -n "$_pg_SELFTEST_DECOY_ABANDONED_PORTS" ]; then
    local rotated_phrase="" past_port=""
    for past_port in $_pg_SELFTEST_DECOY_ABANDONED_PORTS; do
      rotated_phrase="$rotated_phrase :$past_port"
    done
    echo "port-guard selftest: decoy port :$port (picked as free by the selftest itself; attempt $_pg_SELFTEST_DECOY_ATTEMPT/$_pg_SELFTEST_DECOY_BUDGET, after rotating past$rotated_phrase)"
    printf '%s\n' "$_pg_SELFTEST_DECOY_ABANDONED" | while IFS= read -r abandoned; do
      if [ -n "$abandoned" ]; then
        echo "port-guard selftest: rotated past $abandoned"
      fi
    done
  else
    echo "port-guard selftest: decoy port :$port (picked as free by the selftest itself; attempt $_pg_SELFTEST_DECOY_ATTEMPT/$_pg_SELFTEST_DECOY_BUDGET)"
  fi
  echo "port-guard selftest: decoy listener pid $decoy_pid ($(_pg_cmdline "$decoy_pid"))"

  # ── guard 1: require_free_port refuses an occupied port (in a subshell, since
  #            the guard's contract is to EXIT) ────────────────────────────────
  checks=$((checks + 1))
  local rc=0 out=""
  out="$( ( require_free_port "$port" "selftest-decoy" ) 2>&1 )" || rc=$?
  if [ "$rc" -eq 0 ]; then
    echo "port-guard selftest: FAIL: guard 1 require_free_port exited 0 on an occupied port :$port" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "ERROR:.*:$port is already in use"; then
    echo "port-guard selftest: FAIL: guard 1 refused :$port but did not name the port" >&2
    echo "  output: $out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "holder pid : $decoy_pid"; then
    echo "port-guard selftest: FAIL: guard 1 refused :$port but did not name holder pid $decoy_pid" >&2
    echo "  output: $out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "ss -tlnp | grep :$port"; then
    echo "port-guard selftest: FAIL: guard 1 did not print the audit command for :$port" >&2
    echo "  output: $out" >&2
    fails=$((fails + 1))
  else
    echo "PASS: require_free_port refuses an occupied port (pid $decoy_pid + cmdline + audit command named)"
  fi

  # ── guard 2: assert_port_owned accepts the true holder and rejects any other ─
  checks=$((checks + 1))
  local rc_ok=0 rc_bad=0 out_ok="" out_bad=""
  out_ok="$( ( assert_port_owned "$port" "$decoy_pid" "selftest-decoy" ) 2>&1 )" || rc_ok=$?
  out_bad="$( ( assert_port_owned "$port" "$$" "selftest-decoy" ) 2>&1 )" || rc_bad=$?
  if [ "$rc_ok" -ne 0 ]; then
    echo "port-guard selftest: FAIL: guard 2 rejected the true holder pid $decoy_pid (rc=$rc_ok)" >&2
    echo "  output: $out_ok" >&2
    fails=$((fails + 1))
  elif [ "$rc_bad" -eq 0 ]; then
    echo "port-guard selftest: FAIL: guard 2 accepted pid $$ which does not hold :$port" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out_bad" | grep -q "holder pid  : $decoy_pid"; then
    echo "port-guard selftest: FAIL: guard 2 mismatch report did not name holder pid $decoy_pid" >&2
    echo "  output: $out_bad" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out_bad" | grep -q "started pid : $$"; then
    echo "port-guard selftest: FAIL: guard 2 mismatch report did not name the started pid" >&2
    echo "  output: $out_bad" >&2
    fails=$((fails + 1))
  else
    echo "PASS: assert_port_owned accepts the true holder (pid $decoy_pid) and aborts on a mismatched pid (both pids named)"
  fi

  # ── guard 3: wait_http_or_die returns when the started pid serves, and aborts
  #            when the started pid exits first (named death + log tail) ───────
  checks=$((checks + 1))
  local rc_up=0 rc_dead=0 out_up="" out_dead=""
  out_up="$( ( wait_http_or_die "http://127.0.0.1:$port/" "$decoy_pid" "$decoy_log" "selftest-decoy" ) 2>&1 )" || rc_up=$?

  local dead_port dead_log dead_pid
  dead_port="$(_pg_selftest_free_port)" || dead_port=""
  dead_log="$tmp/dead.log"
  (echo "selftest-decoy: bind: address already in use" >&2; exit 3) >"$dead_log" 2>&1 &
  dead_pid=$!
  wait "$dead_pid" 2>/dev/null || true
  if [ -z "$dead_port" ]; then
    echo "port-guard selftest: FAIL: guard 3 could not find a second free port" >&2
    fails=$((fails + 1))
  else
    out_dead="$( ( wait_http_or_die "http://127.0.0.1:$dead_port/health" "$dead_pid" "$dead_log" "selftest-dead" ) 2>&1 )" || rc_dead=$?
  fi
  if [ "$rc_up" -ne 0 ]; then
    echo "port-guard selftest: FAIL: guard 3 did not return 0 for a live pid serving 200 (rc=$rc_up)" >&2
    echo "  output: $out_up" >&2
    fails=$((fails + 1))
  elif [ "$rc_dead" -eq 0 ]; then
    echo "port-guard selftest: FAIL: guard 3 exited 0 although the started pid (pid $dead_pid) had exited" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out_dead" | grep -q "pid $dead_pid) exited before it answered"; then
    echo "port-guard selftest: FAIL: guard 3 death report did not name pid $dead_pid" >&2
    echo "  output: $out_dead" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out_dead" | grep -q "selftest-decoy: bind: address already in use"; then
    echo "port-guard selftest: FAIL: guard 3 death report did not carry the log tail" >&2
    echo "  output: $out_dead" >&2
    fails=$((fails + 1))
  else
    echo "PASS: wait_http_or_die returns on a live 2xx and aborts when the started pid exits first (pid + log tail named)"
  fi

  # ── ARM A/B/C: the decoy BIND itself ─────────────────────────────────────────
  # The pick->bind race cannot be scheduled from inside this process, so it is
  # REPLAYED deterministically through a PATH shim for python3 — the technique
  # `scripts/check-make-docker.sh --selftest` uses to drive hadolint where
  # hadolint is not installed. Each arm re-runs THIS selftest as a child with the
  # arms disabled (PG_SELFTEST_SKIP_ARMS=1), so the recursion is bounded at one
  # level and the child is the same code `make port-guard-selftest` runs.
  if [ "$arms_skipped" -eq 0 ]; then
    local real_py="" decoy_budget="${PG_SELFTEST_DECOY_TRIES:-5}"
    real_py="$(command -v python3 2>/dev/null)"
    checks=$((checks + 3)) # ARM A, ARM B, ARM C — counted even when the fixture cannot be built
    if [ -z "$real_py" ]; then
      echo "port-guard selftest: FAIL: ARM A/B/C need a real python3 to shim (the decoy listener itself needs one)" >&2
      fails=$((fails + 3))
    else
      local shim="$tmp/shim-bin"
      mkdir -p "$shim"
      cat >"$shim/python3" <<'SHIM'
#!/usr/bin/env bash
# port-guard selftest python3 shim — replays the pick->bind race CI hit (run
# 35310987548) deterministically: it fails its first ${PG_SHIM_FAILS} binds like
# EADDRINUSE, or (PG_SHIM_FOREIGN=1) leaves a FOREIGN listener on the candidate
# port before failing. Every call after the budget is the real binary.
n=0
if [ -n "${PG_SHIM_COUNT:-}" ] && [ -f "$PG_SHIM_COUNT" ]; then
  n="$(cat "$PG_SHIM_COUNT" 2>/dev/null)"
  [ -n "$n" ] || n=0
fi
n=$((n + 1))
if [ -n "${PG_SHIM_COUNT:-}" ]; then printf '%s\n' "$n" >"$PG_SHIM_COUNT"; fi
if [ "$n" -le "${PG_SHIM_FAILS:-0}" ]; then
  if [ "${PG_SHIM_FOREIGN:-0}" = "1" ]; then
    port=""
    prev=""
    for a in "$@"; do
      if [ "$prev" = "http.server" ]; then port="$a"; fi
      prev="$a"
    done
    if [ -n "$port" ]; then
      nohup @REAL_PY@ -m http.server "$port" --bind 127.0.0.1 \
        >"${PG_SHIM_FOREIGN_LOG:-/dev/null}" 2>&1 &
      printf '%s\n' "$!" >>"${PG_SHIM_FOREIGN_PID:-/dev/null}"
      i=0
      while [ "$i" -lt 50 ]; do
        if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then break; fi
        sleep 0.1
        i=$((i + 1))
      done
    fi
  fi
  echo "OSError: [Errno 98] Address already in use" >&2
  exit 1
fi
exec @REAL_PY@ "$@"
SHIM
      sed -i.tmp -e "s|@REAL_PY@|$real_py|g" "$shim/python3"
      rm -f "$shim/python3.tmp"
      chmod +x "$shim/python3"

      # ── ARM A: a candidate lost inside the pick->bind window is rotated past ─
      local arm_a="$tmp/arm-a"
      mkdir -p "$arm_a"
      local a_count="$arm_a/bind-count" a_out="" a_rc=0 a_binds=0 a_guards=0
      local a_abandoned="" a_rotated=""
      a_out="$(_pg_selftest_child "$self" "$shim" "$a_count" 1)" || a_rc=$?
      a_binds="$(cat "$a_count" 2>/dev/null || printf '0')"
      a_guards="$(printf '%s\n' "$a_out" | grep -c '^PASS: ')"
      a_abandoned="$(printf '%s\n' "$a_out" | sed -n 's/^port-guard selftest: abandoned candidate :\([0-9][0-9]*\) .*/\1/p' | head -n 1)"
      a_rotated="$(printf '%s\n' "$a_out" | sed -n 's/^port-guard selftest: decoy port :\([0-9][0-9]*\) .*/\1/p' | head -n 1)"
      if [ "$a_rc" -ne 0 ]; then
        echo "port-guard selftest: FAIL: ARM A: a port lost inside the pick->bind window still aborted the selftest (rc=$a_rc) — that is the CI red class (run 35310987548)" >&2
        echo "  output: $a_out" >&2
        fails=$((fails + 1))
      elif [ "$a_guards" -ne 3 ]; then
        echo "port-guard selftest: FAIL: ARM A: the rotated run exited 0 but printed $a_guards of 3 guard PASS lines" >&2
        echo "  output: $a_out" >&2
        fails=$((fails + 1))
      elif [ -z "$a_abandoned" ] || [ -z "$a_rotated" ]; then
        echo "port-guard selftest: FAIL: ARM A: the rotation was not attributable — abandoned='${a_abandoned:-none}', rotated-to='${a_rotated:-none}'" >&2
        echo "  output: $a_out" >&2
        fails=$((fails + 1))
      elif [ "$a_abandoned" = "$a_rotated" ]; then
        echo "port-guard selftest: FAIL: ARM A: the same port :$a_abandoned was reported as abandoned and as rotated-to" >&2
        echo "  output: $a_out" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$a_out" | grep -q "after rotating past :$a_abandoned"; then
        echo "port-guard selftest: FAIL: ARM A: the decoy-port line did not name the abandoned port :$a_abandoned" >&2
        echo "  output: $a_out" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$a_out" | grep -q 'Address already in use'; then
        echo "port-guard selftest: FAIL: ARM A: the abandoned attempt did not carry the bind error the runner produced" >&2
        echo "  output: $a_out" >&2
        fails=$((fails + 1))
      elif [ "$a_binds" -lt 2 ]; then
        echo "port-guard selftest: FAIL: ARM A: the shimmed python3 was consulted $a_binds time(s) — the failed bind was never retried" >&2
        fails=$((fails + 1))
      else
        echo "PASS: ARM A: a candidate taken inside the pick->bind window is rotated past (abandoned :$a_abandoned on EADDRINUSE -> decoy re-bound on :$a_rotated, $a_binds bind(s), all three guards then passed)"
      fi

      # ── ARM B: an exhausted budget fails closed, naming every attempt ────────
      local arm_b="$tmp/arm-b"
      mkdir -p "$arm_b"
      local b_count="$arm_b/bind-count" b_out="" b_rc=0 b_binds=0 b_tried=0
      b_out="$(_pg_selftest_child "$self" "$shim" "$b_count" 99)" || b_rc=$?
      b_binds="$(cat "$b_count" 2>/dev/null || printf '0')"
      b_tried="$(printf '%s\n' "$b_out" | sed -n 's/.*(tried:\([^)]*\)).*/\1/p' | head -n 1 | grep -oE ':[0-9][0-9]*' | sort -u | wc -l)"
      if [ "$b_rc" -ne 1 ]; then
        echo "port-guard selftest: FAIL: ARM B: an exhausted rotation budget exited $b_rc, not 1 — it must fail closed with the guard's own exit code" >&2
        echo "  output: $b_out" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$b_out" | grep -q "FAIL: could not start a decoy listener in $decoy_budget attempt"; then
        echo "port-guard selftest: FAIL: ARM B: the exhausted budget did not name the $decoy_budget-attempt budget" >&2
        echo "  output: $b_out" >&2
        fails=$((fails + 1))
      elif [ "$b_tried" -ne "$decoy_budget" ]; then
        echo "port-guard selftest: FAIL: ARM B: the FAIL named $b_tried distinct attempted port(s), not the $decoy_budget the loop ran" >&2
        echo "  output: $b_out" >&2
        fails=$((fails + 1))
      elif [ "$b_binds" -ne "$decoy_budget" ]; then
        echo "port-guard selftest: FAIL: ARM B: the shimmed python3 was consulted $b_binds time(s) for a $decoy_budget-attempt budget — the loop is not bounded by it" >&2
        fails=$((fails + 1))
      elif printf '%s\n' "$b_out" | grep -q '^PASS'; then
        echo "port-guard selftest: FAIL: ARM B: a PASS was printed for a decoy that never bound" >&2
        echo "  output: $b_out" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$b_out" | grep -q 'Address already in use'; then
        echo "port-guard selftest: FAIL: ARM B: the per-attempt reasons did not carry the bind error the runner produced" >&2
        echo "  output: $b_out" >&2
        fails=$((fails + 1))
      else
        echo "PASS: ARM B: an exhausted rotation budget fails closed (exit 1, $decoy_budget attempts and their ports named, no PASS for a decoy that never bound)"
      fi

      # ── ARM C: a FOREIGN listener on the candidate is never adopted ──────────
      local arm_c="$tmp/arm-c"
      mkdir -p "$arm_c"
      local c_count="$arm_c/bind-count" c_pids="$arm_c/foreign-pids" c_flog="$arm_c/foreign.log"
      local c_out="" c_rc=0 c_foreign="" c_holder="" c_child_pid=""
      local c_abandoned="" c_rotated="" c_guards=0 c_extra=""
      printf '0\n' >"$c_count"
      : >"$c_pids"
      : >"$c_flog"
      c_out="$(_pg_selftest_child "$self" "$shim" "$c_count" 1 "$c_pids" "$c_flog" 1)" || c_rc=$?
      c_foreign="$(head -n 1 "$c_pids" 2>/dev/null)"
      c_guards="$(printf '%s\n' "$c_out" | grep -c '^PASS: ')"
      c_abandoned="$(printf '%s\n' "$c_out" | sed -n 's/^port-guard selftest: abandoned candidate :\([0-9][0-9]*\) .*/\1/p' | head -n 1)"
      c_rotated="$(printf '%s\n' "$c_out" | sed -n 's/^port-guard selftest: decoy port :\([0-9][0-9]*\) .*/\1/p' | head -n 1)"
      c_child_pid="$(printf '%s\n' "$c_out" | sed -n 's/^port-guard selftest: decoy listener pid \([0-9][0-9]*\) .*/\1/p' | head -n 1)"
      # The foreign holder must STILL hold the abandoned candidate after the run:
      # it is not ours to kill, so a run that killed it would be a second defect.
      if [ -n "$c_abandoned" ]; then
        c_holder="$(port_holder_pid "$c_abandoned")"
      fi
      # the fixture's own listener must not outlive this arm
      _pg_SELFTEST_FOREIGN_PID="$c_foreign"
      while IFS= read -r c_extra; do
        if [ -n "$c_extra" ] && [ "$c_extra" != "$c_foreign" ]; then
          kill "$c_extra" 2>/dev/null || true
        fi
      done <"$c_pids"
      if [ -n "$c_foreign" ]; then
        kill "$c_foreign" 2>/dev/null || true
      fi
      if [ "$c_rc" -ne 0 ]; then
        echo "port-guard selftest: FAIL: ARM C: the selftest aborted (rc=$c_rc) instead of rotating off a candidate held by a foreign listener" >&2
        echo "  output: $c_out" >&2
        fails=$((fails + 1))
      elif [ -z "$c_foreign" ]; then
        echo "port-guard selftest: FAIL: ARM C: PREMISE BROKEN — the shim left no foreign listener, so this arm proves nothing" >&2
        fails=$((fails + 1))
      elif [ "$c_holder" != "$c_foreign" ]; then
        echo "port-guard selftest: FAIL: ARM C: PREMISE BROKEN — port :${c_abandoned:-?} was held by '${c_holder:-nothing}', not the foreign pid $c_foreign" >&2
        fails=$((fails + 1))
      elif [ -z "$c_abandoned" ] || [ -z "$c_rotated" ] || [ "$c_abandoned" = "$c_rotated" ]; then
        echo "port-guard selftest: FAIL: ARM C: no rotation off the foreign-held candidate (abandoned='${c_abandoned:-none}', rotated-to='${c_rotated:-none}')" >&2
        echo "  output: $c_out" >&2
        fails=$((fails + 1))
      elif [ -z "$c_child_pid" ]; then
        echo "port-guard selftest: FAIL: ARM C: the rotated run did not name its own decoy pid" >&2
        echo "  output: $c_out" >&2
        fails=$((fails + 1))
      elif [ "$c_child_pid" = "$c_foreign" ]; then
        echo "port-guard selftest: FAIL: ARM C: the FOREIGN listener (pid $c_foreign) was adopted as the decoy — the phantom-green this arm exists for" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$c_out" | grep -q "held by pid $c_foreign"; then
        echo "port-guard selftest: FAIL: ARM C: the refusal did not name the foreign holder pid $c_foreign" >&2
        echo "  output: $c_out" >&2
        fails=$((fails + 1))
      elif [ "$c_guards" -ne 3 ]; then
        echo "port-guard selftest: FAIL: ARM C: the rotated run printed $c_guards of 3 guard PASS lines" >&2
        echo "  output: $c_out" >&2
        fails=$((fails + 1))
      else
        echo "PASS: ARM C: a foreign listener on :$c_abandoned (pid $c_foreign, still holding) is refused and rotated past — the decoy is pid $c_child_pid on :$c_rotated, proven ours, not merely something listening"
      fi
    fi
  fi

  if [ "$fails" -ne 0 ]; then
    echo "port-guard selftest: $((checks - fails))/$checks checks behaved — FAIL" >&2
    return 1
  fi
  if [ "$arms_skipped" -eq 0 ]; then
    echo "port-guard selftest: $checks/$checks checks behaved (3 guards + 3 decoy-bind arms)"
  else
    echo "port-guard selftest: $checks/$checks checks behaved (3 guards; the decoy-bind arms were skipped by PG_SELFTEST_SKIP_ARMS)"
  fi
  return 0
}

# ── CLI (only when executed — sourcing must define functions and nothing else) ─

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  case "${1:-}" in
    --selftest)
      _pg_selftest
      exit $?
      ;;
    -h | --help | help)
      cat <<'EOF'
port-guard.sh — demo-harness port guards (QA-CRIER-9)

Usage:
  bash scripts/lib/port-guard.sh --selftest

It is a library, not a tool: the example harnesses source it for
  port_holder_pid / require_free_port / assert_port_owned / wait_http_or_die
The selftest exercises the three guards on a decoy listener whose port it picks
as free itself (rotation budget PG_SELFTEST_DECOY_TRIES, bind wait
PG_SELFTEST_BIND_TRIES), and proves the decoy BIND itself with three arms driven
by a PATH shim for python3:
  ARM A — a candidate taken inside the pick->bind window is rotated past (the CI
          failure of run 35310987548) and the selftest still passes, naming both
          the abandoned port and the port it rotated to;
  ARM B — an exhausted rotation budget fails closed (exit 1), naming the budget
          and every attempted port, with no PASS for a decoy that never bound;
  ARM C — a foreign listener on the candidate port is never adopted as the decoy:
          the port must be held by the pid the selftest started.
PG_SELFTEST_SKIP_ARMS=1 runs the guards only (that is how the arms re-run this
selftest as a child, bounding the recursion).
EOF
      exit 0
      ;;
    *)
      echo "port-guard.sh: unknown argument '${1:-}' (try --selftest)" >&2
      exit 2
      ;;
  esac
fi
