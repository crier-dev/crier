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
# It exercises all three guards on a port it PICKS ITSELF as free (never a fixed
# port, so a busy CI runner cannot make it flake), printing one PASS line per
# guard and exiting 0 only when all three behaved.
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

# _pg_selftest_listener <port> <log> — start a throwaway listener in the CURRENT
# shell and record its pid in _pg_SELFTEST_LISTENER_PID, so the EXIT trap can
# always kill it. (Called through a command substitution it would run in a
# subshell and the listener would outlive the selftest — the orphaned decoy that
# made this a real bug the first time round.) Returns nonzero when it never bound.
_pg_selftest_listener() {
  local port="$1" log="$2" pid=""
  if command -v python3 >/dev/null 2>&1; then
    (cd "$_pg_SELFTEST_TMP" && exec python3 -m http.server "$port" --bind 127.0.0.1) >"$log" 2>&1 &
    pid=$!
  elif command -v nc >/dev/null 2>&1; then
    nc -l 127.0.0.1 "$port" >"$log" 2>&1 &
    pid=$!
  else
    echo "port-guard selftest: FAIL: need python3 (or nc) to create a throwaway listener" >&2
    return 1
  fi
  _pg_SELFTEST_LISTENER_PID="$pid"

  # It must really be listening before we test the guards against it.
  local i=0
  while [ "$i" -lt 50 ]; do
    [ -n "$(_pg_listen_line "$port")" ] && break
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
    i=$((i + 1))
  done
  if [ -z "$(_pg_listen_line "$port")" ]; then
    echo "port-guard selftest: FAIL: throwaway listener never bound :$port" >&2
    echo "  log: $(tail -n 5 "$log" 2>/dev/null)" >&2
    return 1
  fi
  return 0
}

_pg_selftest() {
  local fails=0

  _pg_require_ss
  _pg_require_tool curl "the selftest drives wait_http_or_die"

  _pg_SELFTEST_TMP="$(mktemp -d)" || {
    echo "port-guard selftest: FAIL: mktemp -d failed" >&2
    return 1
  }
  _pg_SELFTEST_LISTENER_PID=""
  trap '_pg_selftest_cleanup' EXIT

  local tmp="$_pg_SELFTEST_TMP"
  local port decoy_log decoy_pid
  port="$(_pg_selftest_free_port)" || {
    echo "port-guard selftest: FAIL: could not find a free port for the decoy listener" >&2
    return 1
  }
  decoy_log="$tmp/decoy.log"

  echo "port-guard selftest: decoy port :$port (picked as free by the selftest itself)"
  _pg_selftest_listener "$port" "$decoy_log" || return 1
  decoy_pid="$_pg_SELFTEST_LISTENER_PID"
  echo "port-guard selftest: decoy listener pid $decoy_pid ($(_pg_cmdline "$decoy_pid"))"

  # ── guard 1: require_free_port refuses an occupied port (in a subshell, since
  #            the guard's contract is to EXIT) ────────────────────────────────
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

  if [ "$fails" -ne 0 ]; then
    echo "port-guard selftest: $((3 - fails))/3 guards behaved — FAIL" >&2
    return 1
  fi
  echo "port-guard selftest: 3/3 guards behaved"
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
The selftest exercises the three guards on a port it picks as free itself.
EOF
      exit 0
      ;;
    *)
      echo "port-guard.sh: unknown argument '${1:-}' (try --selftest)" >&2
      exit 2
      ;;
  esac
fi
