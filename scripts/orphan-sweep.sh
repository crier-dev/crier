#!/usr/bin/env bash
#
# scripts/orphan-sweep.sh — the tick-start orphan sweep (DF-CRIER-281; CR-GAP-069 ask #2).
#
# WHY THIS EXISTS
# ---------------
# CR-GAP-069 asked for two things. Ask #1 — the server-spawn cleanup GATE — landed
# as scripts/check-demo-cleanup.sh (wired into CI). Ask #2 — a TICK-START ORPHAN
# SWEEP — was never implemented, and the gap it leaves is structural: the cleanup
# gate reads TRACKED shell scripts (`git ls-files '*.sh'`), so it is blind to the
# leak shape that actually costs this shared host resources — an AD-HOC dogfood run
# whose compose file and scripts live under /tmp (never tracked, so never graded),
# plus a scratch-range port held by a process nobody owns. Measured on this host
# when this script was written: 20 containers named dogfood-<stack>-<service>-1 with
# their compose configs under /tmp/dogfood-*, and a leaked `./bin/crier` (pid
# 1149444) holding :8767 since Sep 24. No gate saw either of them.
#
# WHAT IT DOES, AND WHAT IT DELIBERATELY DOES NOT
# -----------------------------------------------
# It REPORTS. It never invokes a mutating verb: no container is stopped, removed,
# killed or pruned and no socket is killed, and that is the design rather than an
# unfinished edge — an automatic reaper cannot distinguish a leaked dogfood stack
# from a stack another session on this shared host is using RIGHT NOW, so the
# sweep's whole job is to make the leak VISIBLE at tick start with enough detail
# (container id, status incl. age, compose path, host ports, holder pid, full
# command line) for a human or a foreman to judge. The selftest asserts the
# invariant from a log of every docker/ss invocation its stubs record, and scans
# this file's own text for a mutating invocation.
#
# SCOPE 1 — CONTAINERS (`docker ps -a`: ALL containers, running and exited)
# -------------------------------------------------------------------------
# A container is an ORPHAN CANDIDATE through either rule, ORed and both reported:
#   * NAME RULE        its name matches dogfood-*
#   * TMP-COMPOSE RULE its com.docker.compose.project.config_files label names a
#                      file under /tmp/ (a scratch stack: the compose file lives in
#                      a temp dir no checkout owns)
# Both matches are reported, so a dogfood-* stack whose compose file sits in a
# checkout is STILL a candidate (the name rule alone), and a non-dogfood stack with
# a /tmp compose file is a candidate too (the tmp rule alone). Liveness is never
# assumed: an Exited container is reported as loudly as a running one and the
# status line carries its age. Reported per candidate: name, container id, status,
# created-at, the compose config file(s), the docker ps port column, and the
# PortMappings from `docker inspect` (so an unpublished-vs-published distinction is
# visible, not inferred).
#
# SCOPE 2 — PORTS (`ss -tlnp`, listeners in a configurable range)
# --------------------------------------------------------------
# Ports are reported for every listener whose port falls in
# [ORPHAN_SWEEP_PORT_MIN, ORPHAN_SWEEP_PORT_MAX] inclusive (default 14000-29000 —
# the scratch range the dogfood lane uses). Per listener: port, holder pid,
# process name, and the holder's FULL command line read from /proc/<pid>/cmdline,
# with the ss record kept alongside as the evidence. A holder whose command line
# names the range's own markers is STILL reported — the sweep never assumes a
# listener is benign, it reports it. A listener whose holder ss cannot attribute
# (another user's process, no privilege) is REPORTED as unattributable rather than
# skipped, so "no pid shown" can never be misread as "nothing there".
#
# OUTPUT AND EXIT CODES
# ---------------------
# A human-readable report on stdout, plus one machine-parsable SUMMARY line.
#   0  the sweep RAN — even when it found orphans. Findings never change the exit
#      code: a tick-start sweep that failed the tick whenever the host was dirty
#      could not be used at tick start.
#   1  not returned by a sweep run (a report has no verdict to fail).
#   2  fail-closed: a required tool is missing from PATH, a required tool FAILED
#      (`docker ps -a` or `ss` returning nonzero — a sweep that could not read the
#      container list or the socket table must never print a clean report), or the
#      configured port range is not two ordered integers. Each names what was
#      wrong.
#
# USAGE
#   bash scripts/orphan-sweep.sh                  # sweep, default range 14000-29000
#   bash scripts/orphan-sweep.sh --selftest       # prove the sweep still behaves
#   ORPHAN_SWEEP_PORT_MIN=1000 ORPHAN_SWEEP_PORT_MAX=30000 bash scripts/orphan-sweep.sh
#   make orphan-sweep-check                       # the selftest (what CI runs)
#
# THE SELFTEST (--selftest)
# -------------------------
# It creates STUB `docker` and `ss` executables in a temp dir and runs the real
# sweep with them FIRST on PATH, so the sweep's own logic is exercised against
# fixtures with no docker daemon, no socket table and no host process of any kind
# involved. The stubs log EVERY invocation, which is what makes the report-only
# invariant checkable. Proved: the classification rules (dogfood name, /tmp compose,
# and the two negatives), the fail-closed paths (no docker, no ss, invalid range),
# the report-only invariant (no mutating verb was even INVOKED), the range filter
# and its env override, the clean-host path (empty census + empty socket table is a
# legal exit 0 with zero findings), and a NEUTER proof (a copy of this script with
# the classification forced to always-no must FAIL the same fixture assertions —
# without it a green selftest could be proving nothing).
#
# DEPENDENCIES: bash, docker, ss, awk (named by the fail-closed check: a missing one
# is exit 2, never a silent skip) plus the coreutils this script uses. The selftest
# needs NO docker and NO ss.

set -euo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="orphan-sweep"

# The docker ps census separator. It must NOT be IFS whitespace: bash's `read`
# collapses a run of IFS-whitespace delimiters, so with a tab a container that has
# an EMPTY compose label would silently shift every field after it — a wrong report
# instead of an error. 0x1f (US) cannot occur in an id, a name, a status, a
# timestamp or a path.
US=$'\x1f'
DOCKER_PS_FORMAT="{{.ID}}${US}{{.Names}}${US}{{.Status}}${US}{{.CreatedAt}}${US}{{.Label \"com.docker.compose.project.config_files\"}}${US}{{.Ports}}"
DOCKER_INSPECT_FORMAT='{{json .NetworkSettings.Ports}}'

PORT_MIN="${ORPHAN_SWEEP_PORT_MIN:-14000}"
PORT_MAX="${ORPHAN_SWEEP_PORT_MAX:-29000}"

# The mutating verbs this sweep must never invoke. Kept as a LIST so no line of this
# file ever contains a docker-mutating invocation a reader (or the selftest's static
# scan) could mistake for one.
FORBIDDEN_DOCKER_VERBS="stop rm kill rmi prune"
FORBIDDEN_SS_FLAGS="-K --kill"

SELFTEST_TMP=""
SELFTEST_CHECKS=0
SELFTEST_FAILS=0

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

usage() {
  cat <<'EOF'
orphan-sweep.sh — the tick-start orphan sweep: REPORT-ONLY (DF-CRIER-281)

Usage:
  bash scripts/orphan-sweep.sh                  # sweep (default port range 14000-29000)
  bash scripts/orphan-sweep.sh --selftest       # prove the sweep still behaves
  bash scripts/orphan-sweep.sh --help

It reports, it never fixes: no container is stopped/removed/killed/pruned and no
socket is killed. Findings do not change the exit code.

  containers  docker ps -a (running and exited); ORPHAN CANDIDATE when the name
              matches dogfood-* OR the compose config_files label names a file
              under /tmp/ — either rule, both reported.
  ports       ss -tlnp listeners in [ORPHAN_SWEEP_PORT_MIN, ORPHAN_SWEEP_PORT_MAX]
              (default 14000-29000), with holder pid, process name and the holder's
              full command line.

Exit codes: 0 the sweep ran (findings do not change it); 2 fail-closed — a
required tool (docker, ss, awk) is missing or failed, or the range is invalid.
EOF
}

# ── configuration guards ──────────────────────────────────────────────────────
_validate_range() {
  local bad=0
  case "$PORT_MIN" in '' | *[!0-9]*) bad=1 ;; esac
  case "$PORT_MAX" in '' | *[!0-9]*) bad=1 ;; esac
  if [ "$bad" -eq 1 ]; then
    _err "the port range must be two integers — got ORPHAN_SWEEP_PORT_MIN='$PORT_MIN'"
    _err "  ORPHAN_SWEEP_PORT_MAX='$PORT_MAX'. Refusing to sweep a range nothing can"
    _err "  match (exit 2, fail-closed)."
    return 2
  fi
  if [ "$PORT_MIN" -gt "$PORT_MAX" ]; then
    _err "the port range is inverted: min=$PORT_MIN > max=$PORT_MAX (exit 2)."
    return 2
  fi
  return 0
}

_require_tools() {
  local -a missing=()
  local t=""
  for t in docker ss awk; do
    if ! command -v "$t" >/dev/null 2>&1; then
      missing+=("$t")
    fi
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    for t in "${missing[@]}"; do
      _err "'$t' is not on PATH — the sweep cannot run, so nothing would be verified."
      _err "  Fail-closed (exit 2): a sweep that cannot read what it reports must"
      _err "  never print a clean host."
    done
    return 2
  fi
  return 0
}

# ── classification ────────────────────────────────────────────────────────────
# Any listed compose config file under /tmp/ (the label may carry a comma-joined
# list of files; each one is trimmed and tested).
_compose_under_tmp() { # <config_files-label-value>
  local val="$1"
  local one="" trimmed=""
  local IFS=','
  for one in $val; do
    trimmed="${one#"${one%%[![:space:]]*}"}"
    trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
    case "$trimmed" in
      /tmp/*) return 0 ;;
    esac
  done
  return 1
}

_is_orphan_candidate() { # <name> <compose-config-files>
  local name="$1"
  local compose="$2"
  case "$name" in
    dogfood-*) return 0 ;;
  esac
  if _compose_under_tmp "$compose"; then
    return 0
  fi
  return 1
}

_candidate_reason() { # <name> <compose-config-files> -> the rule(s) that matched
  local name="$1"
  local compose="$2"
  local reason=""
  case "$name" in
    dogfood-*) reason="name matches dogfood-*" ;;
  esac
  if _compose_under_tmp "$compose"; then
    if [ -n "$reason" ]; then
      reason="$reason AND compose config file under /tmp/"
    else
      reason="compose config file under /tmp/"
    fi
  fi
  printf '%s' "$reason"
}

# ── reporting primitives ──────────────────────────────────────────────────────
_candidate_lines() { # <id> <name> <status> <created> <compose> <ports> -> stdout
  local id="$1" name="$2" status="$3" created="$4" compose="$5" ports="$6"
  local reason="" pins="" prc=0
  reason="$(_candidate_reason "$name" "$compose")"
  # PortMappings, as docker itself reports them (published vs unpublished).
  pins="$(docker inspect --format "$DOCKER_INSPECT_FORMAT" "$id" 2>&1)" || prc=$?
  printf '  ORPHAN CANDIDATE  %s\n' "$name"
  printf '      id:      %s\n' "$id"
  printf '      status:  %s\n' "$status"
  printf '      created: %s\n' "$created"
  if [ -n "$compose" ]; then
    printf '      compose: %s\n' "$compose"
  else
    printf '      compose: (no com.docker.compose.project.config_files label — not a compose stack)\n'
  fi
  printf '      matched: %s\n' "$reason"
  printf '      ports:   %s\n' "${ports:-(none published)}"
  if [ "$prc" -eq 0 ]; then
    printf '      port mappings (docker inspect): %s\n' "$pins"
  else
    printf '      port mappings (docker inspect): unavailable (rc=%s): %s\n' "$prc" "$pins"
  fi
}

# ── scope 1: containers ───────────────────────────────────────────────────────
sweep_containers() {
  local raw="" rc=0
  raw="$(docker ps -a --format "$DOCKER_PS_FORMAT" 2>&1)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    _err "'docker ps -a' failed (rc=$rc) — the container sweep read nothing, so a"
    _err "  report would have claimed a clean host. Fail-closed (exit 2):"
    printf '%s\n' "$raw" | sed 's/^/  /' >&2
    return 2
  fi

  local id="" name="" status="" created="" compose="" ports=""
  local total=0 cand=0
  local -a lines=() blk=()

  while IFS="$US" read -r id name status created compose ports; do
    [ -n "$name" ] || continue
    total=$((total + 1))
    # NEUTER-MARK[classify]: the classification call site. The selftest's neuter
    # proof forces THIS call to always-no and requires the same fixture's
    # classification rules to then FAIL — so the green selftest cannot be vacuous.
    if _is_orphan_candidate "$name" "$compose"; then
      cand=$((cand + 1))
      blk=()
      mapfile -t blk < <(_candidate_lines "$id" "$name" "$status" "$created" "$compose" "$ports")
      if [ "${#blk[@]}" -gt 0 ]; then
        lines+=("${blk[@]}")
      fi
    fi
  done <<<"$raw"

  printf '\n== containers (docker ps -a: all, running and exited) ==\n'
  printf 'containers total: %d   orphan candidates: %d\n' "$total" "$cand"
  if [ "${#lines[@]}" -gt 0 ]; then
    printf '%s\n' "${lines[@]}"
  else
    printf '  (no orphan candidate)\n'
  fi

  SWEEP_CONTAINERS="$total"
  SWEEP_CANDIDATES="$cand"
  return 0
}

# ── scope 2: port listeners ───────────────────────────────────────────────────
sweep_ports() {
  local raw="" rc=0
  raw="$(ss -tlnp 2>&1)" || rc=$?
  if [ "$rc" -ne 0 ]; then
    _err "'ss -tlnp' failed (rc=$rc) — the port sweep read nothing, so a report"
    _err "  would have claimed a clean host. Fail-closed (exit 2):"
    printf '%s\n' "$raw" | sed 's/^/  /' >&2
    return 2
  fi

  # Field 4 of an `ss -tlnp` row is Local Address:Port; the port is whatever
  # follows the LAST ':' (so [::]:18767 and 0.0.0.0:18767 both yield 18767). The
  # process list is `users:(("name",pid=N,fd=M),…)` and may hold several holders
  # for one port — each is reported.
  local recs=""
  recs="$(printf '%s\n' "$raw" | awk -v min="$PORT_MIN" -v max="$PORT_MAX" -v us="$US" '
    NR == 1 && /Local Address/ { next }
    {
      n = split($4, a, ":")
      port = a[n]
      if (port !~ /^[0-9]+$/) next
      if (port + 0 < min + 0 || port + 0 > max + 0) next
      line = $0
      sub(/^[ \t]+/, "", line)
      sub(/[ \t]+$/, "", line)
      proc = ""
      if (match($0, /users:\(\(.*\)\)/)) proc = substr($0, RSTART + 8, RLENGTH - 10)
      if (proc == "") {
        printf "%s%s-%s-%s%s\n", port, us, us, us, line
        next
      }
      m = split(proc, ents, /\),\(/)
      for (k = 1; k <= m; k++) {
        e = ents[k]
        pid = "-"
        nm = "-"
        if (match(e, /pid=[0-9]+/)) pid = substr(e, RSTART + 4, RLENGTH - 4)
        if (match(e, /"[^"]+"/)) nm = substr(e, RSTART + 1, RLENGTH - 2)
        printf "%s%s%s%s%s%s%s\n", port, us, pid, us, nm, us, line
      }
    }' | sort -n)"

  local port="" pid="" pname="" ssline="" cmd="" cmdtxt=""
  local n=0 i=0
  local -a lines=()

  while IFS="$US" read -r port pid pname ssline; do
    [ -n "$port" ] || continue
    n=$((n + 1))
    cmd=""
    if [ "$pid" != "-" ] && [ -r "/proc/$pid/cmdline" ]; then
      cmd="$(tr '\0' ' ' <"/proc/$pid/cmdline" 2>/dev/null)" || cmd=""
    fi
    for i in 1 2 3 4; do
      if [ -n "$cmd" ] && [ "${cmd% }" != "$cmd" ]; then
        cmd="${cmd% }"
      else
        break
      fi
    done
    if [ -n "$cmd" ]; then
      cmdtxt="cmd: $cmd"
    else
      cmdtxt="cmd: (unavailable — /proc/$pid/cmdline is not readable: the holder is either"
      cmdtxt="$cmdtxt unattributable without privilege, or already gone; the ss record is the evidence)"
    fi
    cmdtxt="$(printf '%s' "$cmdtxt" | tr '\n' ' ')"
    lines+=("$(printf '  port %s   pid %s   proc %s' "$port" "$pid" "$pname")")
    lines+=("$(printf '      %s' "$cmdtxt")")
    lines+=("$(printf '      ss:  %s' "$ssline")")
  done <<<"$recs"

  printf '\n== port listeners in range %s-%s (ss -tlnp) ==\n' "$PORT_MIN" "$PORT_MAX"
  printf 'in-range listeners: %d\n' "$n"
  if [ "${#lines[@]}" -gt 0 ]; then
    printf '%s\n' "${lines[@]}"
  else
    printf '  (no listener in range)\n'
  fi

  SWEEP_PORTS="$n"
  return 0
}

# ── the sweep ─────────────────────────────────────────────────────────────────
SWEEP_CONTAINERS=0
SWEEP_CANDIDATES=0
SWEEP_PORTS=0

sweep() {
  _validate_range || return 2
  _require_tools || return 2

  local hn="" now=""
  hn="$(cat /proc/sys/kernel/hostname 2>/dev/null)" || hn="unknown"
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)" || now="unknown"

  printf '%s: REPORT-ONLY — it never stops, kills, removes or prunes anything, and findings do not change the exit code.\n' "$PROG"
  printf '%s: host=%s date=%s range=%s-%s (override: ORPHAN_SWEEP_PORT_MIN / ORPHAN_SWEEP_PORT_MAX)\n' \
    "$PROG" "$hn" "$now" "$PORT_MIN" "$PORT_MAX"

  sweep_containers || return 2
  sweep_ports || return 2

  printf '\n%s: SUMMARY host=%s containers=%d orphan_candidates=%d in_range_listeners=%d range=%s-%s exit=0\n' \
    "$PROG" "$hn" "$SWEEP_CONTAINERS" "$SWEEP_CANDIDATES" "$SWEEP_PORTS" "$PORT_MIN" "$PORT_MAX"
  printf '%s: report-only — nothing was stopped, killed, removed or pruned; findings do not change the exit code.\n' "$PROG"
  return 0
}

# ══════════════════════════════════════════════════════════════════════════════
# THE SELFTEST
# ══════════════════════════════════════════════════════════════════════════════
_st_ok() {
  SELFTEST_CHECKS=$((SELFTEST_CHECKS + 1))
  printf 'PASS: %s\n' "$*"
}

_st_bad() {
  SELFTEST_CHECKS=$((SELFTEST_CHECKS + 1))
  SELFTEST_FAILS=$((SELFTEST_FAILS + 1))
  printf '%s selftest: FAIL: %s\n' "$PROG" "$*" >&2
}

_st_verdict() { # <label> <rc> <want> <what>
  printf 'fixture %-28s rc=%-3s want=%-3s %s\n' "$1" "$2" "$3" "$4"
}

_has() { printf '%s' "$ST_OUT" | grep -qF -- "$1"; }
_has_re() { printf '%s' "$ST_OUT" | grep -qE -- "$1"; }

_selftest_cleanup() {
  if [ -n "$SELFTEST_TMP" ]; then
    rm -rf "$SELFTEST_TMP"
    SELFTEST_TMP=""
  fi
  return 0
}

# _st_run <script> <path> <docker-ps-fixture> <ss-fixture> [VAR=VAL …]
# Runs the sweep with the stubs (or a restricted PATH) and captures output + rc.
_st_run() {
  local script="$1" path="$2" psf="$3" ssf="$4"
  shift 4
  ST_RC=0
  ST_OUT="$(env PATH="$path" \
    ORPHAN_SWEEP_STUB_LOG="$ST_LOG" \
    ORPHAN_SWEEP_STUB_DOCKER_PS="$psf" \
    ORPHAN_SWEEP_STUB_DOCKER_INSPECT="$ST_INSPECT" \
    ORPHAN_SWEEP_STUB_SS="$ssf" \
    ${1+"$@"} bash "$script" 2>&1)" || ST_RC=$?
}

# _st_path_without <destdir> <tool>… — a directory holding every entry of the ORIGINAL
# PATH except the named tools, so their absence is real and not simulated.
_st_path_without() {
  local dest="$1"
  shift
  local -a exclude=("$@")
  mkdir -p "$dest"
  local d="" f="" base="" x="" skip=0
  local -a dirs=()
  local IFS=':'
  read -r -a dirs <<<"$ST_ORIGINAL_PATH"
  for d in "${dirs[@]}"; do
    [ -d "$d" ] || continue
    for f in "$d"/*; do
      [ -e "$f" ] || continue
      base="$(basename "$f")"
      skip=0
      for x in "${exclude[@]}"; do
        if [ "$base" = "$x" ]; then
          skip=1
          break
        fi
      done
      [ "$skip" -eq 1 ] && continue
      if [ ! -e "$dest/$base" ]; then
        ln -s "$f" "$dest/$base" 2>/dev/null || true
      fi
    done
  done
  return 0
}

# The three classification rules the fixtures must satisfy. Used by BOTH the real
# fixture run and the neuter proof, which is the point: the neutered copy must make
# exactly these assertions fail.
_st_classification_ok() { # <report-text>
  local out="$1"
  printf '%s\n' "$out" | grep -qE '^  ORPHAN CANDIDATE  dogfood-asce-1$' || return 1
  printf '%s\n' "$out" | grep -qE '^  ORPHAN CANDIDATE  dogfood-federation$' || return 1
  printf '%s\n' "$out" | grep -qE '^  ORPHAN CANDIDATE  plain-tmp-stack$' || return 1
  if printf '%s\n' "$out" | grep -qE '^  ORPHAN CANDIDATE  some-other-stack$'; then
    return 1
  fi
  if printf '%s\n' "$out" | grep -qE '^  ORPHAN CANDIDATE  plain-nolabel$'; then
    return 1
  fi
  printf '%s\n' "$out" | grep -qF 'containers total: 5   orphan candidates: 3' || return 1
  return 0
}

_selftest() {
  command -v mktemp >/dev/null 2>&1 || {
    printf '%s: ERROR: %s is not on PATH — the selftest cannot run (exit 2)\n' "$PROG" "'mktemp'" >&2
    return 2
  }
  local tmp=""
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/orphan-sweep-selftest.XXXXXX")" || {
    printf '%s: ERROR: mktemp -d failed — the selftest cannot run (exit 2)\n' "$PROG" >&2
    return 2
  }
  SELFTEST_TMP="$tmp"
  trap '_selftest_cleanup' EXIT

  ST_ORIGINAL_PATH="$PATH"
  ST_LOG="$tmp/stub.log"
  ST_INSPECT="$tmp/docker-inspect.json"
  local stub="$tmp/stub"
  mkdir -p "$stub"

  # ── the stubs: they log EVERY invocation, and answer only known reads ───────
  cat >"$stub/docker" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
: "${ORPHAN_SWEEP_STUB_LOG:?the stub needs ORPHAN_SWEEP_STUB_LOG}"
printf 'docker %s\n' "$*" >>"$ORPHAN_SWEEP_STUB_LOG"
case "${1:-}" in
  ps)      cat "${ORPHAN_SWEEP_STUB_DOCKER_PS:?the stub needs ORPHAN_SWEEP_STUB_DOCKER_PS}" ;;
  inspect) cat "${ORPHAN_SWEEP_STUB_DOCKER_INSPECT:?the stub needs ORPHAN_SWEEP_STUB_DOCKER_INSPECT}" ;;
  *)
    printf 'STUB docker: refusing unknown verb: %s\n' "${1:-}" >&2
    exit 1
    ;;
esac
STUB
  chmod +x "$stub/docker"

  cat >"$stub/ss" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
: "${ORPHAN_SWEEP_STUB_LOG:?the stub needs ORPHAN_SWEEP_STUB_LOG}"
printf 'ss %s\n' "$*" >>"$ORPHAN_SWEEP_STUB_LOG"
cat "${ORPHAN_SWEEP_STUB_SS:?the stub needs ORPHAN_SWEEP_STUB_SS}"
STUB
  chmod +x "$stub/ss"

  # ── fixtures ────────────────────────────────────────────────────────────────
  # containers: one row per field-set, US-separated exactly as the census format
  # emits it. Covered: dogfood+/tmp (rule 1), dogfood+/home (rule 1 alone — the
  # name rule), non-dogfood+/home (neither), an empty compose label (neither), and
  # a non-dogfood stack with a /tmp compose file (rule 2 alone).
  _st_ps_line() {
    printf '%s%s%s%s%s%s%s%s%s%s%s\n' "$1" "$US" "$2" "$US" "$3" "$US" "$4" "$US" "$5" "$US" "$6"
  }
  {
    _st_ps_line aaaa11111111 dogfood-asce-1 "Up 5 hours" "2026-09-24 10:00:00 -0500 -05" "/tmp/dogfood-asce/docker-compose.yml" "0.0.0.0:18767->18767/tcp, [::]:18767->18767/tcp"
    _st_ps_line bbbb22222222 dogfood-federation "Exited (0) 3 days ago" "2026-09-21 08:00:00 -0500 -05" "/home/kara/dogfood-federation/docker-compose.yml" ""
    _st_ps_line cccc33333333 some-other-stack "Up 2 days" "2026-09-22 09:00:00 -0500 -05" "/home/kara/other/docker-compose.yml" "0.0.0.0:8080->8080/tcp"
    _st_ps_line dddd44444444 plain-nolabel "Up 1 hour" "2026-09-24 14:00:00 -0500 -05" "" "8080/tcp"
    _st_ps_line eeee55555555 plain-tmp-stack "Up 1 hour" "2026-09-24 14:05:00 -0500 -05" "/tmp/plain-stack/compose.yaml" "0.0.0.0:15000->15000/tcp"
  } >"$tmp/docker-ps.txt"
  : >"$tmp/docker-ps-empty.txt"
  printf '%s\n' '{"18767/tcp":[{"HostIp":"0.0.0.0","HostPort":"18767"}]}' >"$tmp/docker-inspect.json"

  # socket table: 8767 (out of range, the leaked-crier shape) and 39999 (above the
  # range), 14000 and 29000 exactly (the inclusive bounds), 18767 (in range), and a
  # row with no process attribution. The pids are far above pid_max so /proc/<pid>
  # never exists on a real host — the cmd column therefore always exercises the
  # ss-record fallback, deterministically.
  cat >"$tmp/ss.txt" <<'SS'
State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
LISTEN 0      4096   127.0.0.1:8767     0.0.0.0:*    users:(("crier",pid=999000001,fd=6))
LISTEN 0      128    0.0.0.0:14000      0.0.0.0:*    users:(("dogfood-marker",pid=999000002,fd=3))
LISTEN 0      128    [::]:18767          [::]:*       users:(("crier",pid=999000003,fd=9))
LISTEN 0      128    0.0.0.0:29000      0.0.0.0:*    users:(("scratch-stack",pid=999000004,fd=9))
LISTEN 0      128    0.0.0.0:39999      0.0.0.0:*    users:(("outside-range",pid=999000005,fd=9))
LISTEN 0      128    0.0.0.0:26379      0.0.0.0:*
SS
  cat >"$tmp/ss-empty.txt" <<'SS'
State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
SS

  local stubpath="$stub:$ST_ORIGINAL_PATH"

  # ── (a) classification, on the mixed fixture ────────────────────────────────
  : >"$ST_LOG"
  _st_run "$SELF" "$stubpath" "$tmp/docker-ps.txt" "$tmp/ss.txt"
  _st_verdict "FIXTURE-A (mixed census)" "$ST_RC" 0 "report, findings do not change rc"
  local a_out="$ST_OUT"
  if [ "$ST_RC" -eq 0 ] && _st_classification_ok "$a_out"; then
    _st_ok "FIXTURE-A: the dogfood+/tmp container, the dogfood+/home container (name rule) and the non-dogfood /tmp-compose stack are all reported, while the non-dogfood /home stack and the label-less stack are not — 3 of 5"
  else
    _st_bad "FIXTURE-A: rc=$ST_RC (want 0) and/or the classification rules did not hold
  report: $a_out"
  fi
  if _has "port mappings (docker inspect): {\"18767/tcp\""; then
    _st_ok "FIXTURE-A: PortMappings are reported from 'docker inspect' for each candidate"
  else
    _st_bad "FIXTURE-A: the 'docker inspect' PortMappings line is missing from the report
  report: $a_out"
  fi

  # ── (c) report-only invariant, from the stub log ────────────────────────────
  if [ ! -s "$ST_LOG" ]; then
    _st_bad "REPORT-ONLY: the stub log is empty — the sweep never invoked docker/ss, so this invariant would be vacuous"
  else
    local v="" hit=""
    for v in $FORBIDDEN_DOCKER_VERBS; do
      if grep -qE "^docker ${v}( |\$)" "$ST_LOG"; then
        hit="$hit docker-${v}"
      fi
    done
    local f=""
    for f in $FORBIDDEN_SS_FLAGS; do
      if grep -qE "^ss .*${f}( |\$)" "$ST_LOG"; then
        hit="$hit ss-${f}"
      fi
    done
    if [ -z "$hit" ]; then
      _st_ok "REPORT-ONLY: the sweep invoked only reads (docker ps/inspect, ss -tlnp) — no mutating verb appears in the stub log, which holds every invocation"
    else
      _st_bad "REPORT-ONLY: the stub log records mutating invocation(s):$hit
  log: $(printf '%s' "$(tr '\037' '|' <"$ST_LOG")")"
    fi
  fi
  if grep -qE '^docker ps ' "$ST_LOG" && grep -qE '^docker inspect ' "$ST_LOG" && grep -qE '^ss -tlnp' "$ST_LOG"; then
    _st_ok "REPORT-ONLY: the log proves the stubs were really used — docker ps, docker inspect and ss -tlnp were each invoked"
  else
    _st_bad "REPORT-ONLY: the expected reads are not all in the stub log
  log: $(printf '%s' "$(tr '\037' '|' <"$ST_LOG")")"
  fi
  # the static half of the same invariant: this file's own text must not contain a
  # docker-mutating invocation either.
  local line="" s_hit=""
  while IFS= read -r line; do
    for v in $FORBIDDEN_DOCKER_VERBS; do
      case "$line" in
        *"docker ${v}"*) s_hit="$s_hit docker-${v}" ;;
      esac
    done
  done <"$SELF"
  if [ -z "$s_hit" ]; then
    _st_ok "REPORT-ONLY: no line of $SELF itself contains a docker-mutating invocation"
  else
    _st_bad "REPORT-ONLY: the script text contains a docker-mutating invocation:$s_hit"
  fi

  # ── port sweep: the range filter, its bounds and its env override ───────────
  if _has_re '^  port 14000 ' && _has_re '^  port 18767 ' && _has_re '^  port 29000 '; then
    _st_ok "RANGE: the inclusive bounds (14000, 29000) and 18767 are reported"
  else
    _st_bad "RANGE: a bound/in-range listener is missing from the report
  report: $a_out"
  fi
  if _has "in-range listeners: 4" && ! _has_re '^  port 8767 ' && ! _has_re '^  port 39999 '; then
    _st_ok "RANGE: listeners outside 14000-29000 (8767 below, 39999 above) are NOT reported, and the in-range count is 4 (14000, 18767, 26379, 29000)"
  else
    _st_bad "RANGE: an out-of-range listener was reported, or the in-range count is not 4
  report: $a_out"
  fi
  if _has "proc dogfood-marker" && _has_re 'LISTEN .*0\.0\.0\.0:14000'; then
    _st_ok "RANGE: a holder whose name carries the range's own markers is still reported, with its pid and the ss record"
  else
    _st_bad "RANGE: the marker-named holder on 14000 was not reported with pid + ss record
  report: $a_out"
  fi
  if _has "pid -   proc -" && _has "unattributable without privilege"; then
    _st_ok "RANGE: a listener ss cannot attribute is reported as unattributable, never skipped"
  else
    _st_bad "RANGE: the unattributable listener (26379) was not reported as such
  report: $a_out"
  fi
  _st_run "$SELF" "$stubpath" "$tmp/docker-ps.txt" "$tmp/ss.txt" ORPHAN_SWEEP_PORT_MIN=1000 ORPHAN_SWEEP_PORT_MAX=30000
  _st_verdict "FIXTURE-A (range 1000-30000)" "$ST_RC" 0 "the env override widens the range"
  if [ "$ST_RC" -eq 0 ] && _has_re '^  port 8767 ' && _has_re '^  port 14000 ' && ! _has_re '^  port 39999 '; then
    _st_ok "RANGE-OVERRIDE: ORPHAN_SWEEP_PORT_MIN/MAX widen the sweep (8767 is reported, 39999 still is not)"
  else
    _st_bad "RANGE-OVERRIDE: rc=$ST_RC (want 0) with 8767 reported and 39999 not
  report: $ST_OUT"
  fi

  # ── (b) fail-closed: a required tool missing from PATH ──────────────────────
  _st_path_without "$tmp/path-no-docker" docker
  ln -sf "$stub/ss" "$tmp/path-no-docker/ss"
  : >"$ST_LOG"
  _st_run "$SELF" "$tmp/path-no-docker" "$tmp/docker-ps.txt" "$tmp/ss.txt"
  _st_verdict "PATH without docker" "$ST_RC" 2 "exit 2, the tool named"
  if [ "$ST_RC" -eq 2 ] && _has "'docker' is not on PATH"; then
    _st_ok "FAIL-CLOSED-TOOL: a PATH without docker exits 2 and names docker"
  else
    _st_bad "FAIL-CLOSED-TOOL: rc=$ST_RC (want 2) naming docker
  output: $ST_OUT"
  fi
  : >"$ST_LOG"
  _st_path_without "$tmp/path-no-ss" ss
  ln -sf "$stub/docker" "$tmp/path-no-ss/docker"
  _st_run "$SELF" "$tmp/path-no-ss" "$tmp/docker-ps.txt" "$tmp/ss.txt"
  _st_verdict "PATH without ss" "$ST_RC" 2 "exit 2, the tool named"
  if [ "$ST_RC" -eq 2 ] && _has "'ss' is not on PATH"; then
    _st_ok "FAIL-CLOSED-TOOL: a PATH without ss exits 2 and names ss"
  else
    _st_bad "FAIL-CLOSED-TOOL: rc=$ST_RC (want 2) naming ss
  output: $ST_OUT"
  fi
  if [ -s "$ST_LOG" ]; then
    _st_bad "FAIL-CLOSED-TOOL: the tool check runs AFTER a call — the sweep must not touch docker or ss before it knows its tools exist
  log: $(printf '%s' "$(tr '\037' '|' <"$ST_LOG")")"
  else
    _st_ok "FAIL-CLOSED-TOOL: with ss absent the sweep exited before invoking anything — the stub log is empty, so nothing was partly swept"
  fi

  # ── fail-closed: a failing tool is not a clean host ─────────────────────────
  printf '#!/usr/bin/env bash\nprintf "Cannot connect to the Docker daemon\\n" >&2\nexit 1\n' >"$stub/docker-broken"
  chmod +x "$stub/docker-broken"
  mkdir -p "$tmp/path-broken-docker"
  _st_path_without "$tmp/path-broken-docker" docker
  ln -sf "$stub/docker-broken" "$tmp/path-broken-docker/docker"
  ln -sf "$stub/ss" "$tmp/path-broken-docker/ss"
  _st_run "$SELF" "$tmp/path-broken-docker" "$tmp/docker-ps.txt" "$tmp/ss.txt"
  _st_verdict "docker ps fails (rc=1)" "$ST_RC" 2 "exit 2, never a clean report"
  if [ "$ST_RC" -eq 2 ] && _has "Cannot connect to the Docker daemon"; then
    _st_ok "FAIL-CLOSED-FAILURE: a failing 'docker ps' exits 2 with docker's own message — never a report claiming a clean host"
  else
    _st_bad "FAIL-CLOSED-FAILURE: rc=$ST_RC (want 2) carrying docker's error
  output: $ST_OUT"
  fi

  # ── fail-closed: an invalid range is refused ────────────────────────────────
  _st_run "$SELF" "$stubpath" "$tmp/docker-ps.txt" "$tmp/ss.txt" ORPHAN_SWEEP_PORT_MIN=abc
  _st_verdict "range min=abc" "$ST_RC" 2 "exit 2, fail-closed"
  if [ "$ST_RC" -eq 2 ] && _has "must be two integers"; then
    _st_ok "FAIL-CLOSED-RANGE: a non-numeric bound exits 2 instead of silently matching nothing"
  else
    _st_bad "FAIL-CLOSED-RANGE: rc=$ST_RC (want 2) naming the bad bound
  output: $ST_OUT"
  fi
  _st_run "$SELF" "$stubpath" "$tmp/docker-ps.txt" "$tmp/ss.txt" ORPHAN_SWEEP_PORT_MIN=29000 ORPHAN_SWEEP_PORT_MAX=14000
  _st_verdict "range 29000-14000" "$ST_RC" 2 "exit 2, fail-closed"
  if [ "$ST_RC" -eq 2 ] && _has "inverted"; then
    _st_ok "FAIL-CLOSED-RANGE: an inverted range exits 2"
  else
    _st_bad "FAIL-CLOSED-RANGE: rc=$ST_RC (want 2) for an inverted range
  output: $ST_OUT"
  fi

  # ── (e) an explicitly clean host is a legal, zero-finding report ────────────
  _st_run "$SELF" "$stubpath" "$tmp/docker-ps-empty.txt" "$tmp/ss-empty.txt"
  _st_verdict "FIXTURE-E (empty host)" "$ST_RC" 0 "exit 0, zero findings"
  if [ "$ST_RC" -eq 0 ] && _has "container" && _has "orphan candidates: 0" \
    && _has "in-range listeners: 0" && _has "orphan_candidates=0 in_range_listeners=0"; then
    _st_ok "FIXTURE-E: an empty census plus an empty socket table is a legal clean report (exit 0, zero findings, zero counts)"
  else
    _st_bad "FIXTURE-E: rc=$ST_RC (want 0) with explicit zero findings
  report: $ST_OUT"
  fi
  if _has "(no orphan candidate)" && _has "(no listener in range)"; then
    _st_ok "FIXTURE-E: the empty report says so in words rather than printing nothing"
  else
    _st_bad "FIXTURE-E: the empty report does not state the empty findings
  report: $ST_OUT"
  fi

  # ── (d) NEUTER proof: the classification is what classifies ─────────────────
  local neutered="$tmp/neutered-orphan-sweep.sh"
  cp "$SELF" "$neutered"
  sed -i.tmp -e 's|^\([[:space:]]*\)if _is_orphan_candidate "\$name" "\$compose"; then$|\1if false; then|' "$neutered"
  rm -f "$neutered.tmp"
  if cmp -s "$SELF" "$neutered"; then
    _st_bad "NEUTER PROOF: the neuter sed no longer matches the classification call site (NEUTER-MARK[classify]) — the causality proof would be vacuous"
  else
    _st_run "$neutered" "$stubpath" "$tmp/docker-ps.txt" "$tmp/ss.txt"
    _st_verdict "neutered sweep (fixture A)" "$ST_RC" 0 "runs, classifies nothing"
    if [ "$ST_RC" -eq 0 ] && _has "orphan candidates: 0"; then
      _st_ok "NEUTER PROOF: the neutered copy differs from the original, runs, and classifies 0 orphans"
    else
      _st_bad "NEUTER PROOF: the neutered copy did not run cleanly to 0 candidates (rc=$ST_RC)
  report: $ST_OUT"
    fi
    if _st_classification_ok "$ST_OUT"; then
      _st_bad "NEUTER PROOF: the neutered copy STILL satisfies the fixture's classification assertions — those assertions do not actually depend on the classification"
    else
      _st_ok "NEUTER PROOF: the same fixture assertions FAIL on the neutered copy — the classification is what classifies, so the green selftest is not vacuous"
    fi
  fi

  # ── the report-only summary line, and no bash syntax drift ──────────────────
  if _has_re "^${PROG}: SUMMARY host="; then
    _st_ok "SUMMARY: the machine-parsable SUMMARY line is printed on a sweep"
  else
    _st_bad "SUMMARY: the SUMMARY line is missing from the report
  report: $a_out"
  fi
  local syn=0
  bash -n "$SELF" || syn=$?
  if [ "$syn" -eq 0 ]; then
    _st_ok "SYNTAX: bash -n accepts $SELF"
  else
    _st_bad "SYNTAX: bash -n rejected $SELF (rc=$syn)"
  fi

  printf '\n'
  if [ "$SELFTEST_FAILS" -gt 0 ]; then
    printf '%s selftest: %d/%d checks behaved — FAIL\n' "$PROG" "$((SELFTEST_CHECKS - SELFTEST_FAILS))" "$SELFTEST_CHECKS" >&2
    return 1
  fi
  printf '%s selftest: %d/%d checks behaved — PASS\n' "$PROG" "$SELFTEST_CHECKS" "$SELFTEST_CHECKS"
  return 0
}

# ── entry point ───────────────────────────────────────────────────────────────
main() {
  if [ "$#" -gt 0 ]; then
    case "$1" in
      --help | -h)
        usage
        return 0
        ;;
      --selftest)
        local st=0
        _selftest || st=$?
        return "$st"
        ;;
      -*)
        _err "unknown option: $1"
        usage >&2
        return 2
        ;;
      *)
        _err "unexpected argument: $1"
        usage >&2
        return 2
        ;;
    esac
  fi
  local rc=0
  sweep || rc=$?
  return "$rc"
}

rc=0
main "$@" || rc=$?
exit "$rc"
