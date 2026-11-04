#!/usr/bin/env bash
#
# scripts/lib/orphan-server-guard.sh — the "a leaked crier server set must be
# visible from inside this repository" guard (MON-CRIER-006).
#
# WHY THIS EXISTS (measured, 2026-11-04)
# --------------------------------------
# Between 07:25Z and 08:32Z the canonical checkout accumulated full crier server
# processes, one per port, marching upward on this shared host:
#
#     91  -> 121 -> 361  processes
#     361 LISTEN sockets on 19001..19273
#     ~24 MB RSS each  ->  7.84 GB resident  (2.9 GB at the 121-process sample)
#
# Every one of them was a real server binary built at 02:10 local. Their parent
# was already gone — they had reparented to the user systemd manager (ppid 7126,
# `systemd --user`) — so no live spawner could be named from the process table.
# A foreman reaped them by exact pid (`pgrep -x crier-mon`, never `pkill -f`);
# two samples 60 s apart read 0 and MemAvailable returned 43.41 GB -> 45.79 GB.
#
# CONSEQUENCE OF NOT HAVING THIS GUARD: ephemeral-port exhaustion (the whole
# 19001..19273 range was gone), memory pressure that made the port-guard E2E
# cells flake (INT-CI-024 is the standing row for that), and the leak was found
# by a foreman census 20+ minutes after it started — nothing in the repo could
# see it, and every gate that ran during the window reported green.
#
# ATTRIBUTION (the half this repository does NOT own)
# --------------------------------------------------
# The spawning component lives OUTSIDE this repository: it was an ad-hoc
# agent-side harness, not a committed script. Evidence, as measured:
#   * the leaked binary was /tmp/crier-mon, built 2026-11-04 02:10 local from
#     path github.com/crier-dev/crier/cmd/server
#     (mod v0.2.1-0.20261103220314-9f3bf2daa094+dirty);
#   * `grep -rn crier-mon` over this tree returns ZERO hits — no repo script
#     builds that binary;
#   * the user journal for 02:00-03:40 local shows hermes worker scopes
#     (hermes-worker-proc_*.scope units under systemd[7126]) running
#     server-spawning work in /home/kara/worktrees/crier-MON-CRIER-002
#     (`go test ./... -count=1` twice, plus a build);
#   * when such a scope is torn down its children reparent to `systemd --user`,
#     which is exactly the ppid observed on the 361 orphans.
# So: a server-spawning run under a transient worker scope leaked its children,
# and nothing in the repository noticed. The fix for that spawner is not in this
# tree and must not be invented here; what CAN live here is the regression guard
# that makes the class visible from inside the repo on the next run.
#
# WHAT IT CAN SEE
# ---------------
#   * every process whose /proc/<pid>/comm (or the basename of its cmdline
#     argv[0]) matches a crier-server name — `crier`, `crier-mon`,
#     `crier-server`, an optional `-N` suffix, plus anything named in
#     ORPHAN_SERVER_EXTRA_NAMES — and whose PARENT IS GONE: /proc/<ppid> is 1,
#     is a systemd manager running with `--user`, or no longer exists.
#   * the pid, ppid, name, age, RSS and full command line of each one.
#   * the count, against a threshold (default 10).
# A zombie is NOT counted: it has released memory and holds no port.
#
# WHAT IT CANNOT SEE
# ------------------
#   * WHO leaked it. A reparented process carries no record of its spawner, and
#     the guard deliberately does not guess: it reports the shape, not a cause.
#   * Intent. A server started DELIBERATELY detached — `setsid`, a detached
#     `make run`, a pidfile-managed relay — reparents to the same manager and is
#     therefore counted exactly like a leak. That is why the verdict is a
#     THRESHOLD, not a zero-tolerance assert: one detached relay is normal on a
#     dev box, a marching set of them is the incident. Set the threshold you
#     mean (ORPHAN_SERVER_THRESHOLD, or the argument to the assert).
#   * A crier server built from cmd/server under an UNRELATED binary name
#     (e.g. /tmp/xyz123): the name rule is what makes this cheap, so name it in
#     ORPHAN_SERVER_EXTRA_NAMES when a harness builds to a custom filename.
#     (/tmp/crier-mon itself matched by name.)
#   * Processes of other users whose /proc entries this user cannot read, and
#     anything in another PID namespace.
#   * Why a process is still running, its sockets, or its logs.
#
# PUBLIC SURFACE (a guard that fails must be able to ABORT its caller):
#
#   orphan_server_pids
#       Print the pids of orphaned crier servers, one per line; print NOTHING
#       when there are none. Returns 2 when <proc-root> cannot be read at all,
#       so a caller cannot mistake "I could not look" for "nothing there".
#
#   orphan_server_count
#       Print that count as a bare integer (0 when there are none). Returns 2
#       under the same condition as above.
#
#   assert_no_orphan_servers [<threshold>]
#       Exit 1 when the count is >= <threshold> (default 10, or
#       ORPHAN_SERVER_THRESHOLD), printing the count, the pid list, a bounded
#       per-process detail block and the audit commands; exit 0 QUITELY
#       otherwise; exit 2 (fail closed) when it cannot read <proc-root> or the
#       threshold is not a positive integer. It NEVER signals a process — it
#       reports; reaping stays an explicit, exact-pid operator action.
#
# CLI (only when executed — sourcing defines functions and nothing else):
#
#   bash scripts/lib/orphan-server-guard.sh --list          # what the guard sees
#   bash scripts/lib/orphan-server-guard.sh --assert [N]    # the gate verdict
#   bash scripts/lib/orphan-server-guard.sh --selftest      # its own proof
#
# DEPENDENCIES: bash 3.2+, coreutils (cat/tr/awk/cut), setsid + perl for the
#   selftest's seeded decoys only. No pgrep/pkill anywhere: this host runs live
#   servers owned by other sessions, and a `-f` pattern matches the shell that
#   runs it too. Every pid this script signals is one it started itself and
#   proved by pid + /proc/<pid>/stat start-time.
# EXIT CODES: 1 = the situation the guard exists for, 2 = misuse or a
#   dependency it cannot read (never a green).
#
# SELFTEST (used by `make orphan-guard-selftest`):
#
#   bash scripts/lib/orphan-server-guard.sh --selftest
#
# Four asserting arms plus one fail-closed arm, one PASS line each:
#   ARM 1 — a host whose orphan count is at its BASELINE passes quietly, with a
#           threshold one above that baseline (so the PASS itself is measured,
#           not assumed) and with the default threshold.
#   ARM 2 — a seeded orphan set is SEEN: decoys this selftest starts, detaches
#           and verifies are orphaned (`setsid --fork`, so they reparent to the
#           user systemd manager exactly like the incident's 361) appear in
#           orphan_server_pids. One decoy carries the crier name on its EXECVE
#           path (comm matches), the other only in argv[0] (`exec -a`), so both
#           detection dimensions are pinned.
#   ARM 3 — the same threshold that passed in ARM 1 now FAILS (exit 1), naming
#           the count, every seeded pid and the audit commands.
#   ARM 4 — cleanup is EXACT (only the pids this selftest started, matched by
#           pid + start-time) and the gate passes again afterwards.
#   ARM 5 — with an unreadable proc root the assert exits 2 and prints nothing
#           that could be read as a PASS (ORPHAN_GUARD_PROC_ROOT, below).
#
# Baseline note: a dev box may legitimately run one detached relay (started with
# a pidfile, reparented to `systemd --user`); the arms are therefore
# BASELINE-RELATIVE, so this selftest can never mistake a real server for its own
# leak — and on a host with no such relay the ARM 1 threshold IS 1.
#
# ORPHAN_GUARD_PROC_ROOT exists ONLY so ARM 5 can prove the fail-closed path
# against a directory that is not /proc. NEVER set it in a run you intend to
# trust: pointing it at a directory with no crier processes would turn a real
# leak into a green.
#
# TRACE: ch:trace row=MON-CRIER-006 spec=none:host-incident wave=.coding-hermes/waves/crier-foreman-2026-11-04-13-00-50.json test=scripts/lib/orphan-server-guard.sh::--selftest doc=scripts/lib/orphan-server-guard.sh#header prompt=none:worker-brief evidence=<selftest transcript path> witness=none:not-pushed verdict=none:pending-tier2 commit=none:worker memory=none:worker
#
# shellcheck shell=bash disable=SC2155,SC2317,SC2086

# ── configuration ─────────────────────────────────────────────────────────────

# The proc filesystem the guard reads. Overridable ONLY for ARM 5.
_OSG_ROOT="${ORPHAN_GUARD_PROC_ROOT:-/proc}"

# Names that look like a crier SERVER, matched as a bash ERE against the
# basename of /proc/<pid>/comm and of the cmdline's argv[0].
ORPHAN_SERVER_NAME_RE="${ORPHAN_SERVER_NAME_RE:-^(crier|crier[-_]?mon|crier[-_]?server)(-[0-9]+)?$}"
# Extra EXACT names (space-separated), for a harness that builds cmd/server to a
# filename the pattern above does not cover.
ORPHAN_SERVER_EXTRA_NAMES="${ORPHAN_SERVER_EXTRA_NAMES:-}"
# The default threshold the assert applies when it is called with no argument.
ORPHAN_SERVER_THRESHOLD="${ORPHAN_SERVER_THRESHOLD:-10}"
# How many per-process detail lines a failure prints (the pid list is always
# complete; this only bounds the long table).
ORPHAN_SERVER_DETAIL_LIMIT="${ORPHAN_SERVER_DETAIL_LIMIT:-25}"

# This script, as the file that DEFINED these functions — so a failure report
# names a runnable audit command even when the caller is another script.
_OSG_SELF="${BASH_SOURCE[0]}"

# ── internal helpers ──────────────────────────────────────────────────────────

# _osg_require_proc — 0 when the proc root is readable, 2 (and a reason on
# stderr) when it is not. "I could not look" is never "nothing there".
_osg_require_proc() {
  if [ ! -d "$_OSG_ROOT" ] || [ ! -r "$_OSG_ROOT" ] || [ ! -x "$_OSG_ROOT" ]; then
    echo "ERROR: orphan-server-guard: cannot read the process table at $_OSG_ROOT (not a readable directory) — refusing to report a count I could not measure." >&2
    return 2
  fi
  return 0
}

# _osg_proc_stat <pid> — parse /proc/<pid>/stat into _OSG_STATE, _OSG_PPID and
# _OSG_STARTTIME. Returns 1 when the process is gone or unreadable.
# The comm field can contain spaces and parentheses, so everything before the
# LAST ") " is discarded; the remainder starts at field 3 (state), which puts
# ppid (field 4) at position 2 and starttime (field 22) at position 20.
_osg_proc_stat() {
  local pid="${1:-}" raw rest
  [ -n "$pid" ] || return 1
  raw="$(cat "$_OSG_ROOT/$pid/stat" 2>/dev/null)" || return 1
  case "$raw" in
    *') '*) ;;
    *) return 1 ;;
  esac
  rest="${raw##*') '}"
  set -- $rest
  _OSG_STATE="${1:-?}"
  _OSG_PPID="${2:-}"
  _OSG_STARTTIME="${20:-}"
  case "$_OSG_PPID" in
    '' | *[!0-9]*) return 1 ;;
  esac
  return 0
}

# _osg_comm <pid> — the kernel's comm (may be shortened to 15 chars). A builtin
# `read`, not `cat`/`tr`: this runs once per process on the host, and a fork per
# process cost 32 s on a 4k-process box (measured while building this guard).
# NOTE the redirection order: `2>/dev/null` BEFORE `<file`, because bash prints a
# failed input redirect on its own stderr and a trailing redirect cannot silence
# it (measured: the first version sprayed "No such file or directory" for every
# process that exited mid-scan).
_osg_comm() {
  local c=""
  IFS= read -r c 2>/dev/null <"$_OSG_ROOT/${1:-}/comm" || :
  printf '%s' "$c"
}

# _osg_argv0_base <pid> — basename of the process's argv[0], so a renamed
# launcher (`exec -a crier-mon`), a symlink, or a /tmp-built binary are seen by
# the name they were started under. `read -d ''` stops at the first NUL, which is
# exactly argv[0], with no fork.
_osg_argv0_base() {
  local a0=""
  IFS= read -r -d '' a0 2>/dev/null <"$_OSG_ROOT/${1:-}/cmdline" || :
  printf '%s' "${a0##*/}"
}

# _osg_arg_matches <pid> <exact-argv-element> — 0 when any argv element is
# exactly <exact-argv-element> (used to tell `systemd --user` from the system
# manager without parsing prose).
_osg_arg_matches() {
  local a="" f="$_OSG_ROOT/${1:-}/cmdline"
  [ -r "$f" ] || return 1
  while IFS= read -r -d '' a; do
    [ "$a" = "${2:-}" ] && return 0
  done 2>/dev/null <"$f"
  return 1
}

# _osg_name_matches <name> — 0 when <name> looks like a crier server.
_osg_name_matches() {
  local name="${1:-}" extra
  [ -n "$name" ] || return 1
  # shellcheck disable=SC2053 # the pattern is an operator-supplied ERE on purpose
  if [[ "$name" =~ $ORPHAN_SERVER_NAME_RE ]]; then
    return 0
  fi
  for extra in $ORPHAN_SERVER_EXTRA_NAMES; do
    [ "$name" = "$extra" ] && return 0
  done
  return 1
}

# _osg_parent_is_gone <ppid> — 0 when the parent this process reparented AWAY
# from is gone. That is: pid 1, a systemd manager running as the USER manager
# (`systemd --user`, which is where a torn-down worker scope's children land),
# or a pid that no longer exists (exited, not yet reaped).
_osg_parent_is_gone() {
  local ppid="${1:-}" pcomm=""
  case "$ppid" in
    '' | *[!0-9]*) return 0 ;; # unknown ppid: cannot prove a live parent
  esac
  [ "$ppid" -eq 1 ] && return 0
  [ -d "$_OSG_ROOT/$ppid" ] || return 0
  IFS= read -r pcomm 2>/dev/null <"$_OSG_ROOT/$ppid/comm" || :
  [ "$pcomm" = "systemd" ] || return 1
  _osg_arg_matches "$ppid" "--user" && return 0
  return 1
}

# _osg_age_s <starttime> — the process's age in whole seconds, or "" when the
# uptime cannot be read. starttime is in clock ticks since boot.
_osg_age_s() {
  local start="${1:-}" hz up
  case "$start" in '' | *[!0-9]*) printf ''; return 0 ;; esac
  hz="$(getconf CLK_TCK 2>/dev/null)" || hz=100
  case "$hz" in '' | *[!0-9]*) hz=100 ;; esac
  up="$(cut -d. -f1 "$_OSG_ROOT/uptime" 2>/dev/null)" || up=""
  case "$up" in '' | *[!0-9]*) printf ''; return 0 ;; esac
  if [ "$up" -ge "$((start / hz))" ]; then
    printf '%s' "$((up - start / hz))"
  else
    printf '%s' "$((start / hz - up))"
  fi
}

# _osg_rss_kb <pid> — resident set in kB, or "" when unreadable.
_osg_rss_kb() {
  local rss=""
  rss="$(awk '/^VmRSS:/{print $2; exit}' "$_OSG_ROOT/${1:-}/status" 2>/dev/null)" || rss=""
  case "$rss" in '' | *[!0-9]*) printf '' ;; *) printf '%s' "$rss" ;; esac
}

# _osg_cmdline <pid> — the full command line, one line, bounded.
_osg_cmdline() {
  local cmd=""
  cmd="$(cat "$_OSG_ROOT/${1:-}/cmdline" 2>/dev/null | tr '\0' ' ' | cut -c1-400)" || cmd=""
  printf '%s' "${cmd% }"
}

# _osg_count_lines <newline-separated-list> — count without a pipeline (so the
# count cannot be lost in a subshell).
_osg_count_lines() {
  local n=0 w
  for w in ${1:-}; do
    n=$((n + 1))
  done
  printf '%s' "$n"
}

# ── public: which server processes have lost their parent ─────────────────────

orphan_server_pids() {
  local d pid ppid name
  _osg_require_proc || return 2
  for d in "$_OSG_ROOT"/[0-9]*; do
    pid="${d##*/}"
    [ -n "$pid" ] || continue
    [ "$pid" = "$$" ] && continue
    [ "$pid" = "${PPID:-}" ] && continue
    # Name first: it is the cheapest prune, and both reads are BUILTINS — a
    # command substitution here would fork twice per process on the host (32 s
    # for one scan, measured), so the hot path never uses one.
    name=""
    IFS= read -r name 2>/dev/null <"$_OSG_ROOT/$pid/comm" || :
    if ! _osg_name_matches "$name"; then
      name=""
      IFS= read -r -d '' name 2>/dev/null <"$_OSG_ROOT/$pid/cmdline" || :
      name="${name##*/}"
      _osg_name_matches "$name" || continue
    fi
    _osg_proc_stat "$pid" || continue
    # A zombie has released its memory and holds no socket: it cannot exhaust
    # ports or RSS, so it is not this guard's subject.
    [ "$_OSG_STATE" = "Z" ] && continue
    ppid="$_OSG_PPID"
    _osg_parent_is_gone "$ppid" || continue
    printf '%s\n' "$pid"
  done
  return 0
}

orphan_server_count() {
  local list=""
  list="$(orphan_server_pids)" || return 2
  _osg_count_lines "$list"
  printf '\n'
  return 0
}

# ── public: the gate verdict ──────────────────────────────────────────────────

assert_no_orphan_servers() { # [<threshold>]
  local threshold="${1:-${ORPHAN_SERVER_THRESHOLD:-10}}"
  case "$threshold" in
    '' | *[!0-9]*)
      echo "ERROR: assert_no_orphan_servers: threshold must be a positive integer (got '$threshold')" >&2
      exit 2
      ;;
  esac
  if [ "$threshold" -lt 1 ]; then
    echo "ERROR: assert_no_orphan_servers: threshold must be at least 1 (got '$threshold')" >&2
    exit 2
  fi
  _osg_require_proc || exit 2

  local list="" count=0
  list="$(orphan_server_pids)" || exit 2
  [ -n "$list" ] || return 0 # nothing to say, nothing to fail
  count="$(_osg_count_lines "$list")"

  # Below the threshold this is a PASS: one deliberately detached relay is not
  # an incident, and a guard that red-ed on it would be turned off within a day.
  [ "$count" -lt "$threshold" ] && return 0

  local pid detail=0 ppid age rss name
  {
    echo "ERROR: orphan-server-guard: $count crier server process(es) are running with their parent GONE (threshold $threshold)."
    echo "ERROR:   this is the 2026-11-04 leak class: 361 such servers held :19001..:19273 and 7.84 GB RSS."
    echo "ERROR:   pid         ppid  age      rss            name          command"
    for pid in $list; do
      detail=$((detail + 1))
      if [ "$detail" -gt "$ORPHAN_SERVER_DETAIL_LIMIT" ]; then
        echo "ERROR:   ... ($((count - ORPHAN_SERVER_DETAIL_LIMIT)) more, full list below)"
        break
      fi
      _osg_proc_stat "$pid" || {
        echo "ERROR:   $pid (exited while this report was being written)"
        continue
      }
      ppid="$_OSG_PPID"
      age="$(_osg_age_s "$_OSG_STARTTIME")"
      rss="$(_osg_rss_kb "$pid")"
      name="$(_osg_comm "$pid")"
      printf 'ERROR:   %-11s %-5s %-8s %-14s %-13s %s\n' \
        "$pid" "$ppid" "${age:+${age}s}" "${rss:+${rss}kB}" "$name" "$(_osg_cmdline "$pid")" >&2
    done
    echo "ERROR:   pids        :$(printf ' %s' $list)"
    echo "ERROR:   audit with  : bash ${_OSG_SELF:-scripts/lib/orphan-server-guard.sh} --list"
    echo "ERROR:   audit with  : ps -eo pid,ppid,etimes,rss,comm | awk '\$5 ~ /^crier/'"
    echo "ERROR:   audit with  : ss -tlnp | grep -E ':(19[0-9]{3})'    # the incident range :19001..:19273"
    echo "ERROR:   reap with   : kill <pid>    # exact pids, one line each — never 'pkill -f',"
    echo "ERROR:                                 which also matches the shell that runs it"
    echo "ERROR: this guard signals nothing: it reports, and the caller decides."
  } >&2
  exit 1
}

# ── selftest ──────────────────────────────────────────────────────────────────

_osg_selftest_cleanup() {
  local rec pid st
  for rec in ${_OSG_SELFTEST_SEEDS:-}; do
    pid="${rec%%:*}"
    st="${rec#*:}"
    [ -n "$pid" ] || continue
    if _osg_proc_stat "$pid" && [ "${_OSG_STARTTIME:-}" = "$st" ]; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  if [ -n "${_OSG_SELFTEST_TMP:-}" ]; then
    rm -rf "$_OSG_SELFTEST_TMP"
  fi
  return 0
}

# _osg_wait_orphan <pid> — 0 when the guard would call <pid> orphaned.
_osg_wait_orphan() {
  local pid="${1:-}" i=0
  while [ "$i" -lt 40 ]; do
    if _osg_proc_stat "$pid" && _osg_parent_is_gone "$_OSG_PPID"; then
      return 0
    fi
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# _osg_seed_orphan <pidfile> <exec-name|""> <program> [args...] — start <program>
# in a NEW SESSION (`setsid --fork`, so the launcher exits and the child
# reparents to the user systemd manager exactly as the incident's leaks did),
# with <exec-name> as argv[0] when it is non-empty. Prints the pid.
_osg_seed_orphan() {
  local pidfile="${1:-}" name="${2:-}"
  shift 2
  local pid="" i=0
  [ -n "$pidfile" ] || return 1
  : >"$pidfile"
  if [ -n "$name" ]; then
    setsid --fork bash -c 'p=$1; n=$2; shift 2; echo $$ >"$p"; exec -a "$n" "$@"' \
      _ "$pidfile" "$name" "$@" </dev/null >/dev/null 2>&1
  else
    setsid --fork bash -c 'p=$1; shift; echo $$ >"$p"; exec "$@"' \
      _ "$pidfile" "$@" </dev/null >/dev/null 2>&1
  fi
  while [ "$i" -lt 60 ]; do
    pid="$(cat "$pidfile" 2>/dev/null)" || pid=""
    [ -n "$pid" ] && break
    sleep 0.1
    i=$((i + 1))
  done
  [ -n "$pid" ] || return 1
  printf '%s' "$pid"
}

# _osg_selftest_run_assert <threshold> — run the assert in a SUBSHELL (its
# contract is to EXIT) and print "<rc>|<output>".
_osg_selftest_run_assert() {
  local rc=0 out=""
  out="$( ( assert_no_orphan_servers "$1" ) 2>&1 )" || rc=$?
  printf '%s|%s' "$rc" "$out"
}

_osg_selftest() {
  local fails=0 checks=0
  local self="${1:-${BASH_SOURCE[0]}}"
  if [ ! -f "$self" ]; then
    echo "orphan-server-guard selftest: FAIL: cannot locate this script (${self:-unknown})" >&2
    return 2
  fi
  _osg_require_proc || return 2

  _OSG_SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/orphan-server-guard.XXXXXX")" || {
    echo "orphan-server-guard selftest: FAIL: mktemp -d failed" >&2
    return 1
  }
  _OSG_SELFTEST_SEEDS=""
  trap '_osg_selftest_cleanup' EXIT
  local tmp="$_OSG_SELFTEST_TMP"

  # ── baseline ────────────────────────────────────────────────────────────────
  local baseline="" baseline_count=0 threshold="" rc=0 out="" res=""
  baseline="$(orphan_server_pids)" || {
    echo "orphan-server-guard selftest: FAIL: cannot read $_OSG_ROOT" >&2
    return 2
  }
  baseline_count="$(_osg_count_lines "$baseline")"
  threshold=$((baseline_count + 1))
  echo "orphan-server-guard selftest: baseline $baseline_count pre-existing orphaned server(s)${baseline:+ (pids:$(printf ' %s' $baseline))}"
  if [ "$baseline_count" -gt 0 ]; then
    echo "orphan-server-guard selftest: note — a deliberately detached server (setsid / detached 'make run' / a pidfile-managed relay) reparents exactly like a leak and IS counted; the arms below are baseline-relative so a real server is never mistaken for this selftest's seed"
  fi

  # ── ARM 1: at the baseline the gate passes, quietly ─────────────────────────
  checks=$((checks + 1))
  res="$(_osg_selftest_run_assert "$threshold")"
  rc="${res%%|*}"
  out="${res#*|}"
  local res_default="" rc_default=0 out_default=""
  res_default="$(_osg_selftest_run_assert "${ORPHAN_SERVER_THRESHOLD:-10}")"
  rc_default="${res_default%%|*}"
  out_default="${res_default#*|}"
  if [ "$rc" -ne 0 ]; then
    echo "orphan-server-guard selftest: FAIL: ARM 1: assert_no_orphan_servers $threshold exited $rc with $baseline_count orphan(s) — it must pass below its threshold" >&2
    echo "  output: $out" >&2
    fails=$((fails + 1))
  elif [ -n "$out" ]; then
    echo "orphan-server-guard selftest: FAIL: ARM 1: the passing verdict printed something (a PASS must be quiet): $out" >&2
    fails=$((fails + 1))
  elif [ "$rc_default" -ne 0 ]; then
    echo "orphan-server-guard selftest: FAIL: ARM 1: the DEFAULT threshold (${ORPHAN_SERVER_THRESHOLD:-10}) exited $rc_default on a host with $baseline_count orphan(s) — this host is already over the gate; reap first, then re-run" >&2
    echo "  output: $out_default" >&2
    fails=$((fails + 1))
  else
    echo "PASS: ARM 1: below the threshold the gate passes quietly (baseline $baseline_count orphan(s), threshold $threshold, and the default threshold too)"
  fi

  # ── ARM 2/3/4 need seeded decoys ────────────────────────────────────────────
  if ! command -v setsid >/dev/null 2>&1; then
    echo "SKIP: ARM 2/3/4 — 'setsid' (util-linux) is not on PATH; this selftest cannot detach a decoy to a manager-owned parent without it. Install util-linux to run the seeded arms."
    checks=$((checks + 3))
  else
    local seed_a="" seed_b="" comm_arm=1 st_a="" st_b=""
    # The comm arm needs a program that keeps running when its EXECVE path is
    # renamed to a crier server name. Every coreutils applet on this class of
    # host is a MULTICALL binary that dispatches on argv[0] and dies on an
    # unknown name (measured: a symlink named crier-server exited immediately),
    # so perl is preferred; when it is absent that arm SKIPs loudly.
    if command -v perl >/dev/null 2>&1; then
      ln -sf "$(command -v perl)" "$tmp/crier-mon"
      seed_a="$(_osg_seed_orphan "$tmp/seed-a.pid" "" "$tmp/crier-mon" -e 'sleep 300')" || seed_a=""
    else
      comm_arm=0
      echo "SKIP: ARM 2 comm-name decoy — no perl on PATH, so this selftest cannot start a process whose EXECVE path is named crier-mon (the host's coreutils are multicall and die under a foreign name). The argv[0] decoy and the rest of ARM 2 still run."
    fi
    # The argv[0] arm: comm stays 'sleep', the name is only in argv[0].
    seed_b="$(_osg_seed_orphan "$tmp/seed-b.pid" "crier-server" sleep 300)" || seed_b=""

    if [ -z "$seed_b" ] || { [ "$comm_arm" -eq 1 ] && [ -z "$seed_a" ]; }; then
      echo "orphan-server-guard selftest: FAIL: ARM 2: could not start the decoy(s) (comm-arm='${seed_a:-none}', argv0-arm='${seed_b:-none}')" >&2
      checks=$((checks + 3))
      fails=$((fails + 3))
    else
      # register for cleanup BEFORE any assertion can abort the run
      if [ -n "$seed_a" ] && _osg_proc_stat "$seed_a"; then
        st_a="$_OSG_STARTTIME"
        _OSG_SELFTEST_SEEDS="$_OSG_SELFTEST_SEEDS ${seed_a}:${st_a}"
      fi
      if _osg_proc_stat "$seed_b"; then
        st_b="$_OSG_STARTTIME"
        _OSG_SELFTEST_SEEDS="$_OSG_SELFTEST_SEEDS ${seed_b}:${st_b}"
      fi

      # premise: the decoys must REALLY be orphaned, or the arm proves nothing
      local premise="" seed_pids="" seed_count=0
      if [ -n "$seed_a" ]; then
        _osg_wait_orphan "$seed_a" || premise="$premise comm-arm(pid $seed_a did not reparent: ppid $(_osg_proc_stat "$seed_a" && printf '%s' "$_OSG_PPID"))"
        seed_pids="$seed_pids $seed_a"
        seed_count=$((seed_count + 1))
      fi
      _osg_wait_orphan "$seed_b" || premise="$premise argv0-arm(pid $seed_b did not reparent: ppid $(_osg_proc_stat "$seed_b" && printf '%s' "$_OSG_PPID"))"
      seed_pids="$seed_pids $seed_b"
      seed_count=$((seed_count + 1))

      # ── ARM 2: the seeded orphans are SEEN ──────────────────────────────────
      checks=$((checks + 1))
      local seen="" list2="" p="" name_a="" name_b=""
      list2="$(orphan_server_pids)" || list2=""
      for p in $seed_pids; do
        printf '%s\n' "$list2" | grep -qx "$p" || seen="$seen $p"
      done
      [ -n "$seed_a" ] && name_a="$(_osg_comm "$seed_a")"
      name_b="$(_osg_comm "$seed_b")"
      if [ -n "$premise" ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 2 PREMISE BROKEN —$premise (a decoy that never reparented is not evidence about this guard)" >&2
        fails=$((fails + 1))
      elif [ -n "$seen" ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 2: orphan_server_pids did not report seeded pid(s):$seen (it reported:$(printf ' %s' $list2))" >&2
        fails=$((fails + 1))
      elif [ -n "$seed_a" ] && [ "${name_a}" != "crier-mon" ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 2: the comm-name decoy's comm is '${name_a}', not crier-mon — the EXECVE-path arm is not exercising what it claims" >&2
        fails=$((fails + 1))
      elif [ "${name_b}" = "crier-server" ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 2: the argv[0] decoy's comm is 'crier-server' — it must be matched by argv[0] ALONE, or the arm proves nothing about that dimension" >&2
        fails=$((fails + 1))
      else
        echo "PASS: ARM 2: a seeded orphan set is seen (comm-name decoy pid ${seed_a:-none} comm=crier-mon; argv[0]-only decoy pid $seed_b comm=$name_b argv[0]=crier-server; both reparented to a manager-owned parent)"
      fi

      # ── ARM 3: over the threshold it FAILS, naming everything ──────────────
      checks=$((checks + 1))
      local want_count=$((baseline_count + seed_count))
      res="$(_osg_selftest_run_assert "$threshold")"
      rc="${res%%|*}"
      out="${res#*|}"
      local missing=""
      for p in $seed_pids; do
        printf '%s\n' "$out" | grep -q "pids *:.* $p" || missing="$missing $p"
      done
      if [ "$rc" -ne 1 ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 3: with $seed_count seeded orphan(s) the assert exited $rc at threshold $threshold — it must fail closed with 1" >&2
        echo "  output: $out" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$out" | grep -q "orphan-server-guard: $want_count crier server process(es)"; then
        echo "orphan-server-guard selftest: FAIL: ARM 3: the failure did not report the count $want_count (baseline $baseline_count + $seed_count seeded)" >&2
        echo "  output: $out" >&2
        fails=$((fails + 1))
      elif [ -n "$missing" ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 3: the failure's pid list did not name seeded pid(s):$missing" >&2
        echo "  output: $out" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$out" | grep -q -- '--list'; then
        echo "orphan-server-guard selftest: FAIL: ARM 3: the failure printed no audit command (--list)" >&2
        echo "  output: $out" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$out" | grep -q "ss -tlnp"; then
        echo "orphan-server-guard selftest: FAIL: ARM 3: the failure printed no port audit command (ss -tlnp)" >&2
        echo "  output: $out" >&2
        fails=$((fails + 1))
      elif ! printf '%s\n' "$out" | grep -q "reap with"; then
        echo "orphan-server-guard selftest: FAIL: ARM 3: the failure gave no exact-pid reap instruction" >&2
        echo "  output: $out" >&2
        fails=$((fails + 1))
      else
        echo "PASS: ARM 3: over the threshold it fails (exit 1) naming the count $want_count, every seeded pid (${seed_pids# }) and the audit + exact-pid reap commands"
      fi

      # ── ARM 4: cleanup is EXACT, and the gate passes again ─────────────────
      checks=$((checks + 1))
      local leftover="" still=""
      for p in $seed_pids; do
        if _osg_proc_stat "$p"; then
          kill "$p" 2>/dev/null || true
        fi
      done
      local i=0
      while [ "$i" -lt 40 ]; do
        still=""
        for p in $seed_pids; do
          # a zombie (state Z) has released everything; systemd reaps it
          if _osg_proc_stat "$p" && [ "$_OSG_STATE" != "Z" ]; then
            still="$still $p"
          fi
        done
        [ -z "$still" ] && break
        sleep 0.1
        i=$((i + 1))
      done
      leftover="$still"
      _OSG_SELFTEST_SEEDS=""
      res="$(_osg_selftest_run_assert "$threshold")"
      rc="${res%%|*}"
      out="${res#*|}"
      local after_count=""
      after_count="$(_osg_count_lines "$(orphan_server_pids 2>/dev/null)")"
      if [ -n "$leftover" ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 4: seeded pid(s) $leftover survived the exact-pid kill — this selftest must leave nothing behind" >&2
        fails=$((fails + 1))
      elif [ "$rc" -ne 0 ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 4: after reaping the seeded set the assert exited $rc at threshold $threshold (count $after_count, baseline $baseline_count)" >&2
        echo "  output: $out" >&2
        fails=$((fails + 1))
      elif [ "$after_count" != "$baseline_count" ]; then
        echo "orphan-server-guard selftest: FAIL: ARM 4: the orphan count went $baseline_count -> $after_count across the seeded run — a process this selftest did not start was killed, or one of its own escaped" >&2
        fails=$((fails + 1))
      else
        echo "PASS: ARM 4: cleanup is exact — only the $seed_count seeded pid(s) were killed (pid + start-time matched), the count returned to the baseline $baseline_count and the gate passes again"
      fi
    fi
  fi

  # ── ARM 5: an unreadable proc root fails closed ─────────────────────────────
  checks=$((checks + 1))
  local fc_rc=0 fc_out="" fc_root="$tmp/not-a-proc-root"
  fc_out="$( ( ORPHAN_GUARD_PROC_ROOT="$fc_root" bash "$self" --assert 1 ) 2>&1 )" || fc_rc=$?
  if [ "$fc_rc" -ne 2 ]; then
    echo "orphan-server-guard selftest: FAIL: ARM 5: an unreadable proc root exited $fc_rc, not 2 — the guard must fail closed when it cannot look" >&2
    echo "  output: $fc_out" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$fc_out" | grep -q 'cannot read the process table'; then
    echo "orphan-server-guard selftest: FAIL: ARM 5: the refusal did not name the unreadable process table (output: $fc_out)" >&2
    fails=$((fails + 1))
  else
    echo "PASS: ARM 5: an unreadable proc root fails closed (exit 2, naming the process table it could not read — never a silent 0)"
  fi

  if [ "$fails" -ne 0 ]; then
    echo "orphan-server-guard selftest: $((checks - fails))/$checks checks behaved — FAIL" >&2
    return 1
  fi
  echo "orphan-server-guard selftest: $checks/$checks checks behaved"
  return 0
}

# ── CLI (only when executed — sourcing must define functions and nothing else) ─

if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  case "${1:-}" in
    --selftest)
      _osg_selftest "${BASH_SOURCE[0]}"
      exit $?
      ;;
    --list)
      orphan_server_pids || exit $?
      exit 0
      ;;
    --assert)
      assert_no_orphan_servers "${2:-${ORPHAN_SERVER_THRESHOLD:-10}}"
      echo "orphan-server-guard: OK — $(orphan_server_count) orphaned crier server process(es), below the threshold"
      exit 0
      ;;
    -h | --help | help)
      cat <<'EOF'
orphan-server-guard.sh — orphaned crier server processes (MON-CRIER-006)

Usage:
  bash scripts/lib/orphan-server-guard.sh --selftest        # its own proof
  bash scripts/lib/orphan-server-guard.sh --list            # what it sees
  bash scripts/lib/orphan-server-guard.sh --assert [N]      # gate verdict, default N=10

It is a library, not a tool: harnesses and the E2E battery source it for
  orphan_server_pids / orphan_server_count / assert_no_orphan_servers

A process is an ORPHANED CRIER SERVER when its /proc/<pid>/comm (or the basename
of its cmdline argv[0]) matches a crier server name (crier, crier-mon,
crier-server, ± a -N suffix, plus ORPHAN_SERVER_EXTRA_NAMES) AND its parent is
gone — pid 1, a `systemd --user` manager, or a pid that no longer exists. That
is the shape a leaked spawner leaves behind: its worker scope is torn down and
its children reparent to the user manager, so no live spawner can be named.

   2026-11-04: 361 such servers, ports :19001..:19273, 7.84 GB RSS.
   The spawner was an out-of-repo ad-hoc harness under a transient worker scope.
   This guard is the in-repo half: it makes the leak visible on the next run.

It NEVER signals a process. Reap with exact pids (`kill <pid>`), never
`pkill -f`, which also matches the shell that runs it.

Exit codes: 1 = over the threshold (the situation the guard exists for),
            2 = misuse, or a process table it could not read (never a green).
EOF
      exit 0
      ;;
    *)
      echo "orphan-server-guard.sh: unknown argument '${1:-}' (try --selftest | --list | --assert [N])" >&2
      exit 2
      ;;
  esac
fi
