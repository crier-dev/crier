#!/usr/bin/env bash
#
# scripts/bunker-isolation-gate.sh — prove a bunker agent's network-isolation
# mode is ACTUALLY ENFORCED before anything trusts it (INT-CI-020).
#
# WHY THIS EXISTS
# ---------------
# `bunker spawn --network-mode systemd` is validated by the CLI only as a
# MODE NAME. The flag rides the SpawnAgent RPC as a proto field, so a daemon
# that PREDATES network-isolation reporting (any 0.1.x bunkerd, e.g.
# bunker-las-03 and bunker-las-02 at 0.1.4) ignores the unknown field, exits 0
# and creates an agent in the host network namespace. Measured on
# bunker-las-03 (2026-11-01, agent intci020-probe1):
#
#     bunker spawn intci020-probe1 --server bunker-las-03 --ttl 1h \
#         --network-mode systemd          -> "Agent created", rc 0, no warning
#     bunker info intci020-probe1         -> "Network Isolation: not reported by
#                                            this daemon — it predates
#                                            network-isolation reporting; the
#                                            network boundary is NOT verified"
#     bunker exec … -- curl https://proxy.golang.org -> HTTP 200, rc 0
#     bunker exec … -- ip route                      -> default via 192.168.123.1
#
# That is a FALSE CUT: a QA battery that proves its containment by the flag
# value runs with full egress. This gate is the consumer-side half of the fix
# (the producer-side half — a post-spawn read-back inside the bunker CLI — is
# recorded in docs/bunker-runbook.md §5 as the upstream row to file):
#
#   verify  read the agent's REPORTED isolation (bunker info) and, for modes
#           that are a network boundary, probe the agent from the inside
#           (external egress must FAIL, loopback must still work). Exit 0 only
#           when every check passes; anything unparseable/unreported is
#           UNVERIFIED and exits 1 — the gate fails CLOSED.
#
#   spawn   run `bunker spawn` and then `verify` it. A daemon that cannot
#           report the surface is refused BEFORE the spawn (no orphan agent);
#           an agent that comes back unverified is DESTROYED and the command
#           exits 1. `--allow-unverified` downgrades both refusals to a loud
#           warning plus an explicit UNVERIFIED marker (never a silent pass).
#
# THE REPORTING LAW (bunker specs/network-isolation.md §5.2)
# ---------------------------------------------------------
# "A bound that is not reported is not a bound." Absence of a reported mode is
# NEVER read as the requested mode, and never as "shared is fine": an
# unreported/unparseable answer is UNVERIFIED.
#
# WHAT EACH MODE MEANS HERE
# -------------------------
#   shared   no isolation is being requested — nothing to enforce. `verify`
#            reports SKIPPED (exit 0) and prints whatever the daemon reports.
#   systemd  a NETWORK boundary (PrivateNetwork=yes): external egress must be
#            DENIED and loopback must still work, on top of the reported-mode
#            read-back.
#   procvis  (alias: hidepid) NOT a network boundary by its own declaration —
#            only the reported-mode/boundary read-back applies. Pass
#            --no-egress-probe for it; the egress probe is skipped for procvis
#            automatically rather than failed, because a procvis agent is
#            expected to keep its network.
#
# USAGE
# -----
#   bash scripts/bunker-isolation-gate.sh verify --agent A [--server S] [--mode M]
#                                               [--no-egress-probe]
#   bash scripts/bunker-isolation-gate.sh spawn  --agent A --mode M [--server S]
#                                               [--ttl T] [--allow-unverified]
#                                               [--no-egress-probe] [-- spawn args…]
#
#   --agent   agent id/name (default: BUNKER_AGENT)
#   --server  bunker server alias (default: BUNKER_SERVER, then the CLI's active
#             server — resolved by the CLI, never guessed here)
#   --mode    the isolation mode the caller BELIEVES it has. For `verify` it is
#             optional: without it the gate only requires that SOME mode and
#             boundary are reported (and, if the reported mode is systemd,
#             still runs the egress probe).
#   --ttl     TTL passed through to `bunker spawn` (default: the CLI's own)
#   --allow-unverified  keep going on an unverified boundary, loudly. The spawn
#             still prints the UNVERIFIED marker and the agent is KEPT (not
#             destroyed) so the operator can inspect it.
#   --no-egress-probe   read-back only (no in-agent probes).
#
# ENVIRONMENT
#   BUNKER_BIN           bunker CLI (default $HOME/go/bin/bunker, then PATH)
#   BUNKER_AGENT         default --agent
#   BUNKER_SERVER        default --server
#   BUNKER_PROBE_URL     external probe target (default https://proxy.golang.org)
#   BUNKER_PROBE_TIMEOUT per-probe seconds (default 10)
#
# OUTPUT (greppable, one marker line per run — the shape batteries consume)
#   BUNKER ISOLATION: VERIFIED   mode=systemd agent=… server=… boundary="…"
#   BUNKER ISOLATION: SKIPPED    mode=shared  agent=… server=… detail="…"
#   BUNKER ISOLATION: UNVERIFIED reason=<class> mode=… agent=… server=… detail="…"
#   BUNKER ISOLATION: UNVERIFIED-ALLOWED reason=<class> … (with --allow-unverified)
#
# Exit codes: 0 verified (or requested-absent / explicitly allowed),
#             1 UNVERIFIED (fail closed), 2 usage error or missing dependency.
set -uo pipefail

VERB=""
AGENT="${BUNKER_AGENT:-}"
SERVER="${BUNKER_SERVER:-}"
REQUESTED_MODE=""
TTL=""
ALLOW_UNVERIFIED=0
EGRESS_PROBE=1
PROBE_URL="${BUNKER_PROBE_URL:-https://proxy.golang.org}"
PROBE_TIMEOUT="${BUNKER_PROBE_TIMEOUT:-10}"
PASSTHROUGH=()

usage() {
  cat <<'EOF'
scripts/bunker-isolation-gate.sh — fail-closed verification that a bunker
agent's requested network-isolation mode is actually enforced (INT-CI-020).

Usage:
  bash scripts/bunker-isolation-gate.sh verify --agent A [--server S] [--mode M]
                                              [--no-egress-probe]
  bash scripts/bunker-isolation-gate.sh spawn  --agent A --mode M [--server S]
                                              [--ttl T] [--allow-unverified]
                                              [--no-egress-probe] [-- spawn args...]

Verbs
  verify   Check an EXISTING agent: `bunker info` must report a mode whose
           boundary the daemon states, and for a network-boundary mode
           (systemd) an in-agent external probe must FAIL while loopback still
           works. Anything unreported/unparseable is UNVERIFIED — exit 1.
  spawn    `bunker spawn`, then the same verification. A daemon that cannot
           report the isolation surface is refused BEFORE the spawn; an
           unverified agent is destroyed and the command exits 1.

Options
  --agent NAME        agent id/name (default: $BUNKER_AGENT)
  --server NAME       bunker server alias (default: $BUNKER_SERVER, else the
                      CLI's active server)
  --mode MODE         shared | systemd | procvis | hidepid (canonicalised to
                      procvis). For `verify` the mode is optional.
  --ttl T             TTL for `spawn` (passed through to the CLI)
  --allow-unverified  continue on an unverified boundary with a LOUD warning
                      and an UNVERIFIED-ALLOWED marker; the spawn keeps the
                      agent instead of destroying it
  --no-egress-probe   read-back only (no in-agent probes)

Environment
  BUNKER_BIN, BUNKER_AGENT, BUNKER_SERVER, BUNKER_PROBE_URL,
  BUNKER_PROBE_TIMEOUT

Exit codes: 0 verified / nothing requested / explicitly allowed,
            1 UNVERIFIED (fail closed), 2 usage error or missing dependency.
EOF
}

die_usage() {
  echo "bunker-isolation-gate: $*" >&2
  echo "bunker-isolation-gate: try --help" >&2
  exit 2
}

# ── mode vocabulary ──────────────────────────────────────────────────────────
# Kept in lockstep with internal/config/config.go ValidNetworkMode/
# canonicalNetworkMode (bunker repo): the CLI accepts exactly these names and
# `hidepid` is the config-facing spelling of the canonical `procvis`.
valid_mode() {
  case "$1" in
    shared | systemd | procvis | hidepid) return 0 ;;
    *) return 1 ;;
  esac
}

canonical_mode() { # hidepid -> procvis, everything else unchanged
  case "$1" in
    hidepid) echo "procvis" ;;
    *) echo "$1" ;;
  esac
}

# is_network_boundary <canonical-mode> — only `systemd` claims to remove egress
# (netmode.BoundaryFor: "private network namespace (loopback only) … NO outbound
# networking"). `procvis` explicitly declares it is NOT a network boundary, so
# an egress probe on it would be a category error, not evidence.
is_network_boundary() {
  [[ "${1:-}" == "systemd" ]]
}

# ── arg parsing ──────────────────────────────────────────────────────────────
if [[ $# -eq 0 ]]; then usage >&2; exit 2; fi
VERB="$1"; shift
case "$VERB" in
  -h | --help | help) usage; exit 0 ;;
  verify | spawn) ;;
  *) die_usage "unknown verb '$VERB' (want verify or spawn)" ;;
esac

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h | --help) usage; exit 0 ;;
    --) shift; PASSTHROUGH=("$@"); break ;;
    --agent | --server | --mode | --ttl)
      if [[ $# -lt 2 ]]; then
        die_usage "option $1 requires a value"
      fi
      case "$2" in
        --*) die_usage "option $1 requires a value, got another option ('$2')" ;;
      esac
      case "$1" in
        --agent) AGENT=$2 ;;
        --server) SERVER=$2 ;;
        --mode) REQUESTED_MODE=$2 ;;
        --ttl) TTL=$2 ;;
      esac
      shift
      ;;
    --allow-unverified) ALLOW_UNVERIFIED=1 ;;
    --no-egress-probe) EGRESS_PROBE=0 ;;
    *) die_usage "unknown arg '$1'" ;;
  esac
  shift
done

if [[ -n "$REQUESTED_MODE" ]] && ! valid_mode "$REQUESTED_MODE"; then
  die_usage "invalid --mode '$REQUESTED_MODE' (valid: shared systemd procvis hidepid)"
fi
if [[ "$VERB" == "verify" || "$VERB" == "spawn" ]]; then
  [[ -n "$AGENT" ]] || die_usage "--agent is required (or set BUNKER_AGENT)"
fi
if [[ "$VERB" == "spawn" ]]; then
  [[ -n "$REQUESTED_MODE" ]] || die_usage "spawn requires --mode (shared|systemd|procvis|hidepid)"
fi
[[ "$PROBE_TIMEOUT" =~ ^[0-9]+$ ]] || die_usage "BUNKER_PROBE_TIMEOUT must be an integer (got '$PROBE_TIMEOUT')"

# ── the bunker CLI ───────────────────────────────────────────────────────────
BUNKER="${BUNKER_BIN:-}"
if [[ -z "$BUNKER" ]]; then
  for cand in "$HOME/go/bin/bunker" "$(command -v bunker 2>/dev/null || true)"; do
    if [[ -n "$cand" && -x "$cand" ]]; then BUNKER="$cand"; break; fi
  done
fi
if [[ -z "$BUNKER" || ! -x "$BUNKER" ]]; then
  echo "bunker-isolation-gate: no bunker CLI found (set BUNKER_BIN or build one at \$HOME/go/bin/bunker)" >&2
  exit 2
fi

# SERVER_FLAGS — the --server pair, empty when the caller did not name one (the
# CLI then uses its own active server; this gate never invents one).
# Built once: flag position matters for `exec`, where everything after `--` is
# the REMOTE argv. (`bunker exec AGENT --server S -- cmd` worked by accident in
# an early draft — it sent `--server S` to the remote shell.)
SERVER_FLAGS=()
if [[ -n "$SERVER" ]]; then SERVER_FLAGS=(--server "$SERVER"); fi

# bunker_call <args…> — run the CLI, capture combined output, return its exit
# code. Callers read the captured text from $BUNKER_OUT. Keeping the raw text
# (not a re-rendered summary) is deliberate: every verdict below quotes the
# daemon's OWN words, so a human can re-run the same command and see the same
# string.
BUNKER_OUT=""
bunker_call() {
  if [[ ${#SERVER_FLAGS[@]} -gt 0 ]]; then
    BUNKER_OUT="$("$BUNKER" "$@" "${SERVER_FLAGS[@]}" 2>&1)"
  else
    BUNKER_OUT="$("$BUNKER" "$@" 2>&1)"
  fi
  return $?
}

# bunker_exec_call <agent> <remote argv…> — `bunker exec` with the server flag
# BEFORE the `--` separator, so the remote argv is exactly what the caller
# meant.
bunker_exec_call() {
  local agent="$1"
  shift
  if [[ ${#SERVER_FLAGS[@]} -gt 0 ]]; then
    BUNKER_OUT="$("$BUNKER" exec "$agent" "${SERVER_FLAGS[@]}" -- "$@" 2>&1)"
  else
    BUNKER_OUT="$("$BUNKER" exec "$agent" -- "$@" 2>&1)"
  fi
  return $?
}

server_label() { [[ -n "$SERVER" ]] && echo "$SERVER" || echo "(active)"; }

# ── parsing the daemon's own report ──────────────────────────────────────────
# classify_isolation_value <text after "Network Isolation:"> — echoes one of:
#   unreported:<value>          daemon predates the surface (never "safe")
#   unknown:<value>             daemon could not verify the boundary
#   boundary-missing:<mode>     a mode with no boundary line
#   mode:<mode>:<boundary>      the only affirmative shape
#   unparsed:<value>            anything else — fail closed
#
# The em dash is the renderer's separator (internal/cli/netmode_render.go:
# `fmt.Sprintf("%s — %s", mode, boundary)`); the mode vocabulary is the closed
# set above, so an unrecognised prefix can never be mistaken for a mode.
classify_isolation_value() {
  local value="$1" mode boundary
  case "$value" in
    *"not reported by this daemon"*) echo "unreported:$value"; return 0 ;;
    unknown*) echo "unknown:$value"; return 0 ;;
    *"boundary not reported"*)
      mode="${value%% — *}"
      echo "boundary-missing:$mode"
      return 0
      ;;
  esac
  mode="${value%% — *}"
  if [[ "$mode" != "$value" ]] && valid_mode "$mode"; then
    boundary="${value#* — }"
    if [[ -n "$boundary" ]]; then
      echo "mode:$(canonical_mode "$mode"):$boundary"
      return 0
    fi
  fi
  echo "unparsed:$value"
}

# info_isolation_value <agent> — echoes the `Network Isolation:` payload from
# `bunker info`, or the empty string when the line is absent.
info_isolation_value() {
  local agent="$1" line
  bunker_call info "$agent" || return 1
  line="$(printf '%s\n' "$BUNKER_OUT" | grep -m1 '^ *Network Isolation: ' || true)"
  [[ -n "$line" ]] || return 0
  printf '%s' "${line#*Network Isolation: }"
  return 0
}

# status_net_mode_value — the daemon-wide line from `bunker status`, used as the
# PRE-SPAWN capability probe (cheap: no agent is created to find out).
status_net_mode_value() {
  local line
  bunker_call status || return 1
  line="$(printf '%s\n' "$BUNKER_OUT" | grep -m1 '^ *Net mode: ' || true)"
  [[ -n "$line" ]] || return 0
  printf '%s' "${line#*Net mode: }"
  return 0
}

# ── in-agent probes ──────────────────────────────────────────────────────────
# The remote script is a single bash -c argument (the agent always has bash —
# bunker's own QA legs run bash there). It prints BUNKER_PROBE key=value lines
# and never exits nonzero for a "denied" verdict: denial is the EXPECTED answer
# under a real systemd boundary, so exit status alone cannot distinguish it
# from a broken probe. curl absent is reported separately instead of being read
# as "egress denied".
probe_remote_script() {
  cat <<PROBE
if command -v curl >/dev/null 2>&1; then
  if curl -sS -o /dev/null --max-time $PROBE_TIMEOUT "$PROBE_URL" 2>/dev/null; then
    echo "BUNKER_PROBE egress=reachable"
  else
    echo "BUNKER_PROBE egress=denied"
  fi
else
  echo "BUNKER_PROBE curl=absent"
fi
lb="\$( (exec 3<>/dev/tcp/127.0.0.1/1) 2>&1 )"
case "\$lb" in
  *"Connection refused"*) echo "BUNKER_PROBE loopback=refused-alive" ;;
  *)
    if ip -4 addr show lo 2>/dev/null | grep -q "127.0.0.1"; then
      echo "BUNKER_PROBE loopback=addr-only"
    else
      echo "BUNKER_PROBE loopback=absent"
    fi
    ;;
esac
PROBE
}

PROBE_EGRESS=""
PROBE_LOOPBACK=""
PROBE_CURL="present"
probe_agent() { # <agent> — fills PROBE_* ; nonzero when the exec itself failed
  local script out
  script="$(probe_remote_script)"
  bunker_exec_call "$1" bash -c "$script" || true
  out="$BUNKER_OUT"
  PROBE_EGRESS="$(printf '%s\n' "$out" | sed -n 's/^BUNKER_PROBE egress=//p' | head -1)"
  PROBE_LOOPBACK="$(printf '%s\n' "$out" | sed -n 's/^BUNKER_PROBE loopback=//p' | head -1)"
  if printf '%s\n' "$out" | grep -q '^BUNKER_PROBE curl=absent$'; then
    PROBE_CURL="absent"
  fi
  # Success means "the probe ran and answered": a missing curl is an answer
  # (probe-tool-missing), not a failed probe.
  [[ -n "$PROBE_EGRESS" || -n "$PROBE_LOOPBACK" || "$PROBE_CURL" == "absent" ]]
}

# ── verdict ──────────────────────────────────────────────────────────────────
# RESULTS_* are set by evaluate_boundary; the marker is built from them.
VERDICT=""     # VERIFIED | SKIPPED | UNVERIFIED
REASON=""
DETAIL=""
REPORTED_MODE=""
REPORTED_BOUNDARY=""

emit_marker() {
  local mode_label="${REPORTED_MODE:-${REQUESTED_MODE:-unknown}}"
  case "$VERDICT" in
    VERIFIED)
      printf 'BUNKER ISOLATION: VERIFIED mode=%s agent=%s server=%s boundary="%s"\n' \
        "$mode_label" "$AGENT" "$(server_label)" "$REPORTED_BOUNDARY"
      ;;
    SKIPPED)
      printf 'BUNKER ISOLATION: SKIPPED mode=%s agent=%s server=%s detail="%s"\n' \
        "$mode_label" "$AGENT" "$(server_label)" "$DETAIL"
      ;;
    UNVERIFIED)
      local kind="UNVERIFIED"
      [[ "$ALLOW_UNVERIFIED" -eq 1 ]] && kind="UNVERIFIED-ALLOWED"
      printf 'BUNKER ISOLATION: %s reason=%s mode=%s agent=%s server=%s detail="%s"\n' \
        "$kind" "$REASON" "$mode_label" "$AGENT" "$(server_label)" "$DETAIL"
      ;;
  esac
}

set_unverified() { # <reason> <detail>
  VERDICT="UNVERIFIED"
  REASON="$1"
  DETAIL="$2"
}

# probe_note — informational in-agent probe for a boundary the daemon cannot
# confirm. It NEVER changes the verdict (the verdict is already UNVERIFIED by
# absence of a report); it puts the live fact — "this agent has full egress" —
# next to the refusal, so the operator reads the consequence, not just the
# rule. Empty when the probe cannot run or the mode is not a network boundary.
probe_note() {
  local mode="${REPORTED_MODE:-}"
  [[ -z "$mode" && -n "$REQUESTED_MODE" ]] && mode="$(canonical_mode "$REQUESTED_MODE")"
  [[ "$EGRESS_PROBE" -eq 1 ]] || return 0
  is_network_boundary "$mode" || return 0
  probe_agent "$AGENT" || return 0
  [[ -n "$PROBE_EGRESS" ]] || return 0
  printf '; in-agent probe: egress=%s loopback=%s' "$PROBE_EGRESS" "${PROBE_LOOPBACK:-?}"
}

# evaluate_boundary — read-back + (mode-appropriate) probes. Sets VERDICT/
# REASON/DETAIL and prints a human-readable ladder to stderr so a refused run
# explains itself without the operator re-running anything.
evaluate_boundary() {
  local value cls requested_canonical
  requested_canonical=""
  [[ -n "$REQUESTED_MODE" ]] && requested_canonical="$(canonical_mode "$REQUESTED_MODE")"

  if ! value="$(info_isolation_value "$AGENT")"; then
    set_unverified "info-failed" "bunker info $AGENT failed: $(printf '%s' "$BUNKER_OUT" | tail -1)"
    return 0
  fi
  if [[ -z "$value" ]]; then
    # The renderer always prints the line on a current CLI; an absent line means
    # an old CLI, i.e. nothing can be verified here. Fail closed.
    set_unverified "info-unparsed" "bunker info printed no 'Network Isolation:' line (old CLI?); nothing can be verified"
    return 0
  fi

  cls="$(classify_isolation_value "$value")"
  case "$cls" in
    unreported:*)
      set_unverified "daemon-predates-surface" \
        "the daemon reports no network-isolation surface at all: '$value' — the requested mode cannot have been applied$(probe_note)"
      return 0
      ;;
    unknown:*)
      set_unverified "daemon-cannot-verify" "the daemon could not verify the enforced boundary: '$value'$(probe_note)"
      return 0
      ;;
    boundary-missing:*)
      REPORTED_MODE="${cls#boundary-missing:}"
      set_unverified "boundary-missing" \
        "the daemon reports mode '$REPORTED_MODE' with NO boundary line — a mode without a boundary fails the reporting law (§5.2)$(probe_note)"
      return 0
      ;;
    unparsed:*)
      set_unverified "info-unparsed" "unrecognised 'Network Isolation:' payload ('$value') — refusing to guess"
      return 0
      ;;
    mode:*)
      REPORTED_MODE="$(printf '%s' "$cls" | awk -F: '{print $2}')"
      REPORTED_BOUNDARY="$(printf '%s' "$cls" | cut -d: -f3-)"
      ;;
    *)
      set_unverified "info-unparsed" "internal: could not classify '$value'"
      return 0
      ;;
  esac

  # A read-back whose mode is not what the caller asked for is exactly the
  # silent-fallback the reporting law forbids.
  if [[ -n "$requested_canonical" && "$REPORTED_MODE" != "$requested_canonical" ]]; then
    set_unverified "mode-mismatch" \
      "requested '$requested_canonical' but the daemon reports '$REPORTED_MODE' — a silent fallback, never a satisfied request"
    return 0
  fi

  # Nothing containment-shaped was requested: report honestly and pass.
  if [[ "$REPORTED_MODE" == "shared" ]]; then
    VERDICT="SKIPPED"
    DETAIL="the daemon reports shared (no isolation) — nothing was requested to enforce"
    return 0
  fi

  # Modes that are not a network boundary stop here: their read-back is the
  # whole claim, and probing egress on them would test the wrong property.
  if ! is_network_boundary "$REPORTED_MODE"; then
    VERDICT="VERIFIED"
    DETAIL="mode+boundary read back (not a network boundary — no egress probe applies)"
    return 0
  fi

  if [[ "$EGRESS_PROBE" -eq 0 ]]; then
    set_unverified "egress-probe-skipped" \
      "read-back says '$REPORTED_MODE' but --no-egress-probe leaves the network boundary UNPROVEN; re-run without it"
    return 0
  fi

  if ! probe_agent "$AGENT"; then
    set_unverified "probe-failed" "in-agent probe could not run (bunker exec produced no BUNKER_PROBE lines): $(printf '%s' "$BUNKER_OUT" | tail -1)"
    return 0
  fi
  if [[ "$PROBE_CURL" == "absent" ]]; then
    set_unverified "probe-tool-missing" "the agent has no curl, so external reachability cannot be measured — refusing to call this verified"
    return 0
  fi
  if [[ "$PROBE_EGRESS" == "reachable" ]]; then
    # The declared boundary for `systemd` is the agent's DOCKERD UNIT
    # ("covers only processes in the agent's dockerd unit" — netmode.BoundaryFor).
    # `bunker exec` is an SSH session, so it runs OUTSIDE that unit: egress
    # from here is expected and does NOT prove the mode is unimplemented. What
    # it does prove is the thing a QA battery cares about — the processes THIS
    # gate (and any exec-scoped battery) runs are NOT contained. Refusing is
    # the honest verdict; calling it an "egress leak" would misattribute a
    # declared scope to a broken daemon.
    set_unverified "exec-not-contained" \
      "the daemon reports '$REPORTED_MODE', but its declared boundary covers only the agent's dockerd unit while an exec session is outside it — this exec session reached $PROBE_URL (egress=reachable), so a battery running over \`bunker exec\` is NOT contained"
    return 0
  fi
  if [[ "$PROBE_EGRESS" != "denied" ]]; then
    set_unverified "probe-unparsed" "in-agent probe produced no egress verdict (got '${PROBE_EGRESS:-<empty>}')"
    return 0
  fi
  if [[ "$PROBE_LOOPBACK" == "absent" ]]; then
    set_unverified "loopback-missing" \
      "egress is denied but loopback is absent too (loopback=${PROBE_LOOPBACK:-<empty>}): the agent lost its loopback, which '$REPORTED_MODE' must preserve"
    return 0
  fi

  VERDICT="VERIFIED"
  DETAIL="read-back matches, external egress DENIED, loopback ${PROBE_LOOPBACK:-present}"
  return 0
}

explain() {
  {
    echo "bunker-isolation-gate: agent '$AGENT' on server '$(server_label)'"
    [[ -n "$REQUESTED_MODE" ]] && echo "  requested mode:  $REQUESTED_MODE"
    if [[ -n "$REPORTED_MODE" ]]; then
      echo "  reported mode:   $REPORTED_MODE"
      echo "  reported boundary: $REPORTED_BOUNDARY"
    fi
    if [[ -n "$PROBE_EGRESS" || -n "$PROBE_LOOPBACK" ]]; then
      echo "  in-agent probe:  egress=${PROBE_EGRESS:-?} loopback=${PROBE_LOOPBACK:-?}"
    fi
    if [[ -n "$REASON" ]]; then
      echo "  verdict:         $VERDICT ($REASON) — $DETAIL"
    else
      echo "  verdict:         $VERDICT — $DETAIL"
    fi
  } >&2
}

destroy_agent() {
  bunker_call destroy "$AGENT" || {
    echo "bunker-isolation-gate: WARNING — could not destroy agent '$AGENT' (server '$(server_label)'): $(printf '%s' "$BUNKER_OUT" | tail -1)" >&2
    echo "bunker-isolation-gate: WARNING — '$AGENT' EXISTS with an UNVERIFIED boundary; destroy it by hand before using this host" >&2
    return 1
  }
  echo "bunker-isolation-gate: destroyed unverified agent '$AGENT'" >&2
  return 0
}

# ── verbs ────────────────────────────────────────────────────────────────────
cmd_verify() {
  evaluate_boundary
  emit_marker
  explain
  [[ "$VERDICT" == "UNVERIFIED" ]] && return 1
  return 0
}

cmd_spawn() {
  local requested_canonical status_value
  requested_canonical="$(canonical_mode "$REQUESTED_MODE")"

  # ── pre-spawn capability probe: refuse BEFORE creating anything ────────────
  # Cheap and side-effect free. A daemon that answers "not reported by this
  # daemon" can never apply the mode, so spawning it would only manufacture an
  # unverified agent (and, on hosts without destroy-archive, an orphan home).
  if is_network_boundary "$requested_canonical"; then
    if ! status_value="$(status_net_mode_value)"; then
      set_unverified "daemon-unreachable" "bunker status failed: $(printf '%s' "$BUNKER_OUT" | tail -1)"
      emit_marker
      explain
      if [[ "$ALLOW_UNVERIFIED" -eq 1 ]]; then
        echo "bunker-isolation-gate: --allow-unverified: continuing although the daemon could not be interrogated" >&2
      else
        echo "bunker-isolation-gate: refusing to spawn — the daemon's isolation surface could not be verified, so '$requested_canonical' cannot be assumed enforced (no agent was created)" >&2
        return 1
      fi
    elif [[ -z "$status_value" ]]; then
      set_unverified "info-unparsed" "bunker status printed no 'Net mode:' line (old CLI?)"
      emit_marker
      explain
      if [[ "$ALLOW_UNVERIFIED" -eq 1 ]]; then
        echo "bunker-isolation-gate: --allow-unverified: continuing although the CLI could not report the daemon's mode" >&2
      else
        echo "bunker-isolation-gate: refusing to spawn — no agent was created (see docs/bunker-runbook.md §4)" >&2
        return 1
      fi
    else
      case "$(classify_isolation_value "$status_value")" in
        unreported:*)
          set_unverified "daemon-predates-surface" \
            "the daemon reports no network-isolation surface ('$status_value'): a spawn with --network-mode $requested_canonical would be accepted and SILENTLY IGNORED"
          emit_marker
          explain
          if [[ "$ALLOW_UNVERIFIED" -ne 1 ]]; then
            echo "bunker-isolation-gate: refusing to spawn with --network-mode $requested_canonical — no agent was created. Upgrade bunkerd on '$(server_label)' (surface: NET-BUNKER-010) or drop the flag; see docs/bunker-runbook.md §3." >&2
            return 1
          fi
          echo "bunker-isolation-gate: WARNING — --allow-unverified was given and the daemon CANNOT enforce $requested_canonical; the agent will run with FULL EGRESS" >&2
          ;;
      esac
    fi
  fi

  # ── spawn ───────────────────────────────────────────────────────────────────
  local -a spawn_args=(spawn "$AGENT")
  [[ -n "$TTL" ]] && spawn_args+=(--ttl "$TTL")
  spawn_args+=(--network-mode "$REQUESTED_MODE")
  spawn_args+=("${PASSTHROUGH[@]+"${PASSTHROUGH[@]}"}")
  if ! bunker_call "${spawn_args[@]}"; then
    echo "bunker-isolation-gate: bunker spawn failed: $(printf '%s' "$BUNKER_OUT" | tail -3)" >&2
    exit 1
  fi
  printf '%s\n' "$BUNKER_OUT"

  # ── post-spawn verification (the read-back the flag alone never gave) ──────
  evaluate_boundary
  if [[ "$VERDICT" == "SKIPPED" && "$requested_canonical" != "shared" ]]; then
    # A containment mode that read back as shared is a mismatch, not a skip.
    set_unverified "mode-mismatch" "requested '$requested_canonical' but the daemon reports 'shared'"
  fi
  emit_marker
  explain
  if [[ "$VERDICT" == "UNVERIFIED" ]]; then
    if [[ "$ALLOW_UNVERIFIED" -eq 1 ]]; then
      echo "bunker-isolation-gate: WARNING — agent '$AGENT' is running with an UNVERIFIED boundary; anything run on it may have full egress" >&2
      return 0
    fi
    destroy_agent || true
    echo "bunker-isolation-gate: FAILED CLOSED — the requested isolation was not verified; see docs/bunker-runbook.md" >&2
    return 1
  fi
  return 0
}

case "$VERB" in
  verify) cmd_verify ;;
  spawn) cmd_spawn ;;
esac
