#!/usr/bin/env bash
#
# scripts/bunker-isolation-gate-selftest.sh — prove the network-isolation gate
# fails CLOSED in every direction (INT-CI-020).
#
# WHY THIS EXISTS
# ---------------
# scripts/bunker-isolation-gate.sh decides whether a QA battery may trust a
# bunker agent's network boundary. Its verdicts are only worth as much as its
# refusal arms: a gate that silently passes an unenforced mode, or that treats
# an unreported mode as the requested one, re-creates the exact FALSE CUT
# (bunker-las-03 accepted `--network-mode systemd`, created the agent, kept full
# egress) it was written to stop. That needs to be proven at every step, with no
# bunker host, no network and no docker — so this selftest drives the real gate
# with a STUB bunker CLI whose every answer (status Net mode line, info Network
# Isolation line, in-agent probe results, exit codes) is switchable per arm, and
# whose argv is logged so "was a spawn/destroy actually issued?" is measured
# rather than assumed.
#
# ARMS
# ----
#   1.  --help exits 0 and names both verbs
#   2.  usage errors exit 2: unknown verb, bad mode, verify without --agent,
#       spawn without --mode, an option without a value
#   3.  verify: daemon predates the surface -> UNVERIFIED/daemon-predates-surface
#       (rc 1) and the in-agent probe still runs, so the marker carries the
#       measured egress verdict
#   4.  verify: systemd + egress denied + loopback alive -> VERIFIED (rc 0)
#   5.  verify: systemd + EGRESS REACHABLE from the exec session ->
#       UNVERIFIED/exec-not-contained (rc 1): the mode scopes to the dockerd
#       unit, so an exec-scoped battery is NOT contained — the false cut
#   6.  verify: systemd with no boundary line -> UNVERIFIED/boundary-missing
#   7.  verify: daemon says shared while systemd was requested ->
#       UNVERIFIED/mode-mismatch (a silent fallback is never a satisfied request)
#   8.  verify: requested shared -> SKIPPED (rc 0, nothing to enforce)
#   9.  verify: procvis -> VERIFIED from the read-back alone and the egress probe
#       is NEVER run (procvis is not a network boundary by its own declaration;
#       a leaked egress value must not be able to fail it)
#   10. verify: systemd + --no-egress-probe -> UNVERIFIED/egress-probe-skipped
#       (read-back alone cannot prove the boundary)
#   11. verify: curl absent in the agent -> UNVERIFIED/probe-tool-missing
#   12. verify: missing "Network Isolation:" line (old CLI) -> UNVERIFIED and
#       never a pass
#   13. verify: `bunker info` fails (unknown agent) -> UNVERIFIED/info-failed
#   14. spawn on a pre-surface daemon -> rc 1, NO spawn issued, no agent created
#   15. spawn on a pre-surface daemon + --allow-unverified -> rc 0, spawn IS
#       issued, marker UNVERIFIED-ALLOWED, agent KEPT (no destroy)
#   16. spawn verified end-to-end -> rc 0, VERIFIED marker, spawn carried
#       --network-mode + --ttl, destroy NOT issued
#   17. spawn verified environment but egress reachable -> rc 1 and the agent is
#       DESTROYED (fail closed, no unisolated agent left behind)
#   18. spawn that cannot destroy its unverified agent -> rc 1 plus a loud
#       warning naming the surviving agent
#   19. `bunker exec` argv shape: --server BEFORE `--` (a flag after `--` is
#       remote argv — the bug this selftest pins)
#
# Exit codes: 0 all arms behaved, 1 at least one failed, 2 a dependency is
# missing (bash, mktemp).
set -uo pipefail

PASSED=0
FAILED=0
ok() { PASSED=$((PASSED + 1)); echo "PASS: $*"; }
bad() { FAILED=$((FAILED + 1)); echo "FAIL: $*"; }

usage() {
  cat <<'EOF'
scripts/bunker-isolation-gate-selftest.sh — hermetic proof that
scripts/bunker-isolation-gate.sh fails closed (INT-CI-020).

Drives the real gate against a STUB bunker CLI whose status/info/probe answers
and exit codes are switchable per arm, and whose argv is logged. No bunker
host, no network, no docker.

Usage:
  bash scripts/bunker-isolation-gate-selftest.sh          # every arm
  bash scripts/bunker-isolation-gate-selftest.sh --help   # this text

Exit codes: 0 all arms behaved, 1 at least one failed, 2 a dependency is missing.
EOF
}

case "${1:-}" in
  -h | --help) usage; exit 0 ;;
  "") ;;
  *) echo "bunker-isolation-gate-selftest: unknown arg '$1' (try --help)" >&2; exit 2 ;;
esac

command -v bash >/dev/null 2>&1 || { echo "selftest: bash missing" >&2; exit 2; }
command -v mktemp >/dev/null 2>&1 || { echo "selftest: mktemp missing" >&2; exit 2; }

SELFTEST_DIR="$(cd "$(dirname "$0")" && pwd)"
GATE="$SELFTEST_DIR/bunker-isolation-gate.sh"
[[ -f "$GATE" ]] || { echo "selftest: gate not found at $GATE" >&2; exit 2; }

TMP="$(mktemp -d)"
cleanup() { [[ -n "$TMP" ]] && rm -rf "$TMP"; return 0; }
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

STUB_BIN="$TMP/bin"
mkdir -p "$STUB_BIN"
STUB_LOG="$TMP/bunker-argv.log"
: >"$STUB_LOG"

# ── the stub bunker CLI ──────────────────────────────────────────────────────
# Every knob is an environment variable read at call time, so one arm can ask
# for "systemd reported, egress reachable" and the next for "unreported daemon"
# without touching a file. The argv log is what makes "did the gate really
# spawn/destroy?" measurable.
cat >"$STUB_BIN/bunker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_LOG"
verb="${1:-}"
case "$verb" in
  status)
    if [[ -n "${STUB_STATUS_RC:-}" && "${STUB_STATUS_RC}" -ne 0 ]]; then
      echo "bunker: server info: unavailable: dial tcp 127.0.0.1:1: connection refused" >&2
      exit "${STUB_STATUS_RC}"
    fi
    echo "── stub-server ──"
    echo "  Version:  0.2.1"
    if [[ -n "${STUB_STATUS_NET:-}" ]]; then
      echo "  Net mode: ${STUB_STATUS_NET}"
    fi
    exit 0
    ;;
  info)
    if [[ -n "${STUB_INFO_RC:-}" && "${STUB_INFO_RC}" -ne 0 ]]; then
      echo "bunker: get agent: not_found: agent not found" >&2
      exit "${STUB_INFO_RC}"
    fi
    echo "══════════ Agent: stub-agent ══════════"
    if [[ "${STUB_OMIT_INFO_LINE:-0}" -ne 1 && -n "${STUB_INFO_NET:-}" ]]; then
      echo "  Network Isolation: ${STUB_INFO_NET}"
    fi
    exit 0
    ;;
  exec)
    if [[ "${STUB_CURL:-present}" == "absent" ]]; then
      echo "BUNKER_PROBE curl=absent"
      exit 0
    fi
    echo "BUNKER_PROBE egress=${STUB_EGRESS:-denied}"
    echo "BUNKER_PROBE loopback=${STUB_LOOPBACK:-refused-alive}"
    exit 0
    ;;
  spawn)
    if [[ -n "${STUB_SPAWN_RC:-}" && "${STUB_SPAWN_RC}" -ne 0 ]]; then
      echo "bunker: spawn: slice-limits: memory.swap.max=max" >&2
      exit "${STUB_SPAWN_RC}"
    fi
    echo "Creating agent..."
    echo "Agent created: stub-agent"
    exit 0
    ;;
  destroy)
    if [[ -n "${STUB_DESTROY_RC:-}" && "${STUB_DESTROY_RC}" -ne 0 ]]; then
      echo "bunker: destroy: archive failed: daemon unreachable" >&2
      exit "${STUB_DESTROY_RC}"
    fi
    echo "Agent stub-agent destroyed."
    exit 0
    ;;
  *)
    echo "stub bunker: unsupported verb '$verb'" >&2
    exit 99
    ;;
esac
STUB
chmod +x "$STUB_BIN/bunker"

UNREPORTED="not reported by this daemon — it predates network-isolation reporting; the network boundary is NOT verified"
SHARED="shared — host network namespace (no network isolation: shared loopback, global port space) — NOT tenant isolation"
SYSTEMD="systemd — private network namespace (loopback only): private 127.0.0.1 and port space; NO outbound networking (image pulls unavailable); covers only processes in the agent's dockerd unit"
PROCVIS="procvis — private /proc in the unit's mount namespace (hidepid=2 semantics): unit processes see only their own user's processes; does NOT cover the agent's SSH/exec sessions (host /proc view unchanged there), is NOT a network boundary, and does NOT stop another agent enumerating this unit via /proc/<unit-pid>/root/proc"

# run_gate <log-prefix> <env assignments…> -- <gate args…>
#   Hermetic: HOME points into the sandbox so the real CLI config can never
#   leak in, BUNKER_BIN is the stub, and BUNKER_AGENT/BUNKER_SERVER are cleared
#   unless an arm sets them.
run_gate() {
  local prefix="$1"
  shift
  local -a envs=()
  while [[ "${1:-}" != "--" ]]; do envs+=("$1"); shift; done
  shift
  env -u BUNKER_AGENT -u BUNKER_SERVER \
    HOME="$TMP/home" STUB_LOG="$STUB_LOG" BUNKER_BIN="$STUB_BIN/bunker" \
    BUNKER_PROBE_URL="https://probe.invalid" BUNKER_PROBE_TIMEOUT=1 \
    "${envs[@]}" \
    bash "$GATE" "$@" >"$prefix.out" 2>"$prefix.err"
  echo $?
}

combined() { cat "$1.out" "$1.err" 2>/dev/null; }
marker() { grep -m1 '^BUNKER ISOLATION: ' "$1.out" 2>/dev/null || true; }
count_argv() { grep -c -- "^$1 " "$STUB_LOG" 2>/dev/null || true; }
reset_log() { : >"$STUB_LOG"; }

# ── arm 1: help ──────────────────────────────────────────────────────────────
arm_help() {
  local rc out
  rc="$(run_gate "$TMP/help" -- --help)"
  out="$TMP/help.out"
  if [[ "$rc" -ne 0 ]]; then bad "HELP: exits $rc, want 0"; return; fi
  local missing=""
  for want in verify spawn UNVERIFIED fail-closed; do
    grep -qi -- "$want" "$out" || missing="$missing $want"
  done
  if [[ -n "$missing" ]]; then bad "HELP: output never mentions:$missing"; else ok "HELP: exits 0 and documents verify/spawn + the fail-closed contract"; fi
}

# ── arm 2: usage errors ──────────────────────────────────────────────────────
arm_usage() {
  run_case() { # <label> <want-rc> <gate args…>
    local lbl="$1" want="$2"
    shift 2
    local p="$TMP/usage-$lbl"
    local got
    got="$(run_gate "$p" -- "$@")"
    if [[ "$got" -ne "$want" ]]; then
      bad "USAGE/$lbl: exits $got, want $want"
    elif grep -q 'unbound variable' "$p.err" 2>/dev/null; then
      bad "USAGE/$lbl: unbound-variable traceback"
    else
      ok "USAGE/$lbl: exits $want with no crash"
    fi
  }
  run_case unknown-verb 2 frobnicate --agent a
  run_case bad-mode 2 verify --agent a --mode pasta
  run_case verify-no-agent 2 verify --server s
  run_case spawn-no-mode 2 spawn --agent a
  run_case option-no-value 2 verify --agent
  return 0
}

# ── arms 3-13: verify verdicts ───────────────────────────────────────────────
arm_verify_matrix() {
  local rc m

  # 3. daemon predates the surface: fail closed, but report the measured egress.
  reset_log
  rc="$(run_gate "$TMP/v-unreported" \
    "STUB_INFO_NET=$UNREPORTED" "STUB_EGRESS=reachable" -- \
    verify --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/v-unreported")"
  if [[ "$rc" -ne 1 ]]; then
    bad "VERIFY/unreported: exits $rc, want 1"
  elif [[ "$m" != *"UNVERIFIED reason=daemon-predates-surface"* ]]; then
    bad "VERIFY/unreported: marker = '$m'"
  elif [[ "$m" != *"egress=reachable"* ]]; then
    bad "VERIFY/unreported: marker never carries the measured in-agent egress ('$m')"
  elif [[ "$(count_argv exec)" -lt 1 ]]; then
    bad "VERIFY/unreported: no in-agent probe was run"
  else
    ok "VERIFY/unreported: UNVERIFIED/daemon-predates-surface, rc 1, probe measured egress=reachable"
  fi

  # 4. the happy path: systemd reported, egress denied, loopback alive.
  rc="$(run_gate "$TMP/v-verified" \
    "STUB_INFO_NET=$SYSTEMD" "STUB_EGRESS=denied" "STUB_LOOPBACK=refused-alive" -- \
    verify --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/v-verified")"
  if [[ "$rc" -ne 0 ]]; then
    bad "VERIFY/verified: exits $rc, want 0 (marker '$m')"
  elif [[ "$m" != *"BUNKER ISOLATION: VERIFIED mode=systemd"* ]]; then
    bad "VERIFY/verified: marker = '$m'"
  elif [[ "$m" != *"private network namespace"* ]]; then
    bad "VERIFY/verified: marker does not quote the daemon boundary ('$m')"
  else
    ok "VERIFY/verified: VERIFIED, rc 0, boundary quoted in the marker"
  fi

  # 5. THE FALSE CUT: the daemon claims systemd, but the exec session (where the
  #    battery runs) reaches the internet — outside the declared unit scope.
  rc="$(run_gate "$TMP/v-leak" \
    "STUB_INFO_NET=$SYSTEMD" "STUB_EGRESS=reachable" -- \
    verify --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/v-leak")"
  if [[ "$rc" -ne 1 ]]; then
    bad "VERIFY/exec-not-contained: exits $rc, want 1"
  elif [[ "$m" != *"UNVERIFIED reason=exec-not-contained"* ]]; then
    bad "VERIFY/exec-not-contained: marker = '$m'"
  else
    ok "VERIFY/exec-not-contained: UNVERIFIED/exec-not-contained, rc 1 (a reported mode whose exec session still reaches the internet is refused)"
  fi

  # 6. a mode with no boundary line fails the reporting law.
  rc="$(run_gate "$TMP/v-noboundary" \
    "STUB_INFO_NET=systemd — boundary not reported" -- \
    verify --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/v-noboundary")"
  if [[ "$rc" -ne 1 || "$m" != *"reason=boundary-missing"* ]]; then
    bad "VERIFY/boundary-missing: rc=$rc marker='$m'"
  else
    ok "VERIFY/boundary-missing: UNVERIFIED/boundary-missing, rc 1"
  fi

  # 7. silent fallback to shared is never a satisfied systemd request.
  rc="$(run_gate "$TMP/v-mismatch" "STUB_INFO_NET=$SHARED" -- \
    verify --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/v-mismatch")"
  if [[ "$rc" -ne 1 || "$m" != *"reason=mode-mismatch"* ]]; then
    bad "VERIFY/mode-mismatch: rc=$rc marker='$m'"
  else
    ok "VERIFY/mode-mismatch: UNVERIFIED/mode-mismatch, rc 1"
  fi

  # 8. shared requested: nothing to enforce, and it says so.
  rc="$(run_gate "$TMP/v-shared" "STUB_INFO_NET=$SHARED" -- \
    verify --agent a1 --server s1 --mode shared)"
  m="$(marker "$TMP/v-shared")"
  if [[ "$rc" -ne 0 || "$m" != *"BUNKER ISOLATION: SKIPPED mode=shared"* ]]; then
    bad "VERIFY/shared: rc=$rc marker='$m'"
  else
    ok "VERIFY/shared: SKIPPED, rc 0"
  fi

  # 9. procvis is NOT a network boundary: read-back only, the leaked egress knob
  #    must not even be consulted.
  reset_log
  rc="$(run_gate "$TMP/v-procvis" \
    "STUB_INFO_NET=$PROCVIS" "STUB_EGRESS=reachable" -- \
    verify --agent a1 --server s1 --mode procvis)"
  m="$(marker "$TMP/v-procvis")"
  if [[ "$rc" -ne 0 || "$m" != *"BUNKER ISOLATION: VERIFIED mode=procvis"* ]]; then
    bad "VERIFY/procvis: rc=$rc marker='$m'"
  elif [[ "$(count_argv exec)" -ne 0 ]]; then
    bad "VERIFY/procvis: an egress probe was run on a non-network boundary"
  else
    ok "VERIFY/procvis: VERIFIED from read-back alone, no egress probe (category rule holds)"
  fi

  # 9b. the hidepid alias canonicalises to procvis.
  rc="$(run_gate "$TMP/v-hidepid" "STUB_INFO_NET=$PROCVIS" -- \
    verify --agent a1 --server s1 --mode hidepid)"
  m="$(marker "$TMP/v-hidepid")"
  if [[ "$rc" -ne 0 || "$m" != *"mode=procvis"* ]]; then
    bad "VERIFY/hidepid-alias: rc=$rc marker='$m'"
  else
    ok "VERIFY/hidepid-alias: --mode hidepid canonicalises to procvis and verifies"
  fi

  # 10. read-back without the probe cannot prove a NETWORK boundary.
  rc="$(run_gate "$TMP/v-noprobe" "STUB_INFO_NET=$SYSTEMD" -- \
    verify --agent a1 --server s1 --mode systemd --no-egress-probe)"
  m="$(marker "$TMP/v-noprobe")"
  if [[ "$rc" -ne 1 || "$m" != *"reason=egress-probe-skipped"* ]]; then
    bad "VERIFY/no-egress-probe: rc=$rc marker='$m'"
  else
    ok "VERIFY/no-egress-probe: UNVERIFIED/egress-probe-skipped, rc 1"
  fi

  # 11. no curl in the agent: unmeasurable is not verified.
  rc="$(run_gate "$TMP/v-nocurl" "STUB_INFO_NET=$SYSTEMD" "STUB_CURL=absent" -- \
    verify --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/v-nocurl")"
  if [[ "$rc" -ne 1 || "$m" != *"reason=probe-tool-missing"* ]]; then
    bad "VERIFY/curl-absent: rc=$rc marker='$m'"
  else
    ok "VERIFY/curl-absent: UNVERIFIED/probe-tool-missing, rc 1"
  fi

  # 12. an absent "Network Isolation:" line (old CLI) is never a pass.
  rc="$(run_gate "$TMP/v-noline" "STUB_OMIT_INFO_LINE=1" -- \
    verify --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/v-noline")"
  if [[ "$rc" -ne 1 || "$m" != *"reason=info-unparsed"* ]]; then
    bad "VERIFY/no-line: rc=$rc marker='$m'"
  else
    ok "VERIFY/no-line: UNVERIFIED/info-unparsed, rc 1"
  fi

  # 13. info failure (unknown agent) is a refusal, not a pass.
  rc="$(run_gate "$TMP/v-infoerr" "STUB_INFO_RC=1" -- \
    verify --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/v-infoerr")"
  if [[ "$rc" -ne 1 || "$m" != *"reason=info-failed"* ]]; then
    bad "VERIFY/info-failed: rc=$rc marker='$m'"
  else
    ok "VERIFY/info-failed: UNVERIFIED/info-failed, rc 1"
  fi
}

# ── arms 14-18: spawn behaviour ──────────────────────────────────────────────
arm_spawn_matrix() {
  local rc m

  # 14. pre-surface daemon: refuse BEFORE the spawn, create nothing.
  reset_log
  rc="$(run_gate "$TMP/s-predates" "STUB_STATUS_NET=$UNREPORTED" \
    -- spawn --agent a1 --server s1 --mode systemd --ttl 1h)"
  m="$(marker "$TMP/s-predates")"
  if [[ "$rc" -ne 1 ]]; then
    bad "SPAWN/predates: exits $rc, want 1"
  elif [[ "$m" != *"UNVERIFIED reason=daemon-predates-surface"* ]]; then
    bad "SPAWN/predates: marker = '$m'"
  elif [[ "$(count_argv spawn)" -ne 0 ]]; then
    bad "SPAWN/predates: the gate spawned anyway (argv log has a spawn line)"
  elif ! grep -q "no agent was created" "$TMP/s-predates.err"; then
    bad "SPAWN/predates: refusal does not tell the operator nothing was created"
  else
    ok "SPAWN/predates: refused before any spawn (rc 1, no spawn argv, 'no agent was created')"
  fi

  # 15. --allow-unverified: spawn proceeds, is LOUD, and the agent is kept.
  reset_log
  rc="$(run_gate "$TMP/s-allowed" "STUB_STATUS_NET=$UNREPORTED" \
    -- spawn --agent a1 --server s1 --mode systemd --ttl 1h --allow-unverified)"
  m="$(marker "$TMP/s-allowed")"
  if [[ "$rc" -ne 0 ]]; then
    bad "SPAWN/allow: exits $rc, want 0"
  elif [[ "$m" != *"UNVERIFIED-ALLOWED"* ]]; then
    bad "SPAWN/allow: marker = '$m'"
  elif [[ "$(count_argv spawn)" -ne 1 ]]; then
    bad "SPAWN/allow: expected exactly one spawn invocation, got $(count_argv spawn)"
  elif [[ "$(count_argv destroy)" -ne 0 ]]; then
    bad "SPAWN/allow: destroyed an agent the operator explicitly allowed"
  elif ! grep -q "CANNOT enforce systemd" "$TMP/s-allowed.err"; then
    bad "SPAWN/allow: the pre-spawn warning was not printed"
  elif ! grep -q "UNVERIFIED boundary" "$TMP/s-allowed.err"; then
    bad "SPAWN/allow: the post-spawn warning was not printed"
  else
    ok "SPAWN/allow: loud warning, spawn issued once, agent kept, rc 0"
  fi

  # 16. verified spawn: mode read back, egress denied, no destroy.
  reset_log
  rc="$(run_gate "$TMP/s-verified" "STUB_STATUS_NET=$SYSTEMD" "STUB_INFO_NET=$SYSTEMD" \
    "STUB_EGRESS=denied" -- spawn --agent a1 --server s1 --mode systemd --ttl 1h)"
  m="$(marker "$TMP/s-verified")"
  if [[ "$rc" -ne 0 || "$m" != *"BUNKER ISOLATION: VERIFIED mode=systemd"* ]]; then
    bad "SPAWN/verified: rc=$rc marker='$m'"
  elif [[ "$(count_argv destroy)" -ne 0 ]]; then
    bad "SPAWN/verified: destroyed a verified agent"
  elif ! grep -q '^spawn a1 --ttl 1h --network-mode systemd' "$STUB_LOG"; then
    bad "SPAWN/verified: spawn argv = '$(grep -m1 '^spawn ' "$STUB_LOG")'"
  else
    ok "SPAWN/verified: rc 0, VERIFIED, spawn carried --ttl/--network-mode, no destroy"
  fi

  # 17. the daemon claims systemd but the agent leaks: destroy, fail closed.
  reset_log
  rc="$(run_gate "$TMP/s-leak" "STUB_STATUS_NET=$SYSTEMD" "STUB_INFO_NET=$SYSTEMD" \
    "STUB_EGRESS=reachable" -- spawn --agent a1 --server s1 --mode systemd)"
  m="$(marker "$TMP/s-leak")"
  if [[ "$rc" -ne 1 ]]; then
    bad "SPAWN/leak: exits $rc, want 1"
  elif [[ "$m" != *"reason=exec-not-contained"* ]]; then
    bad "SPAWN/leak: marker = '$m'"
  elif [[ "$(count_argv destroy)" -ne 1 ]]; then
    bad "SPAWN/leak: expected the unverified agent to be DESTROYED (destroy argv count = $(count_argv destroy))"
  else
    ok "SPAWN/leak: exec session not contained -> agent destroyed and rc 1 (no unisolated agent left behind)"
  fi

  # 18. destroy failure is loud and still non-zero.
  reset_log
  rc="$(run_gate "$TMP/s-leak-nodestroy" "STUB_STATUS_NET=$SYSTEMD" "STUB_INFO_NET=$SYSTEMD" \
    "STUB_EGRESS=reachable" "STUB_DESTROY_RC=1" -- spawn --agent a1 --server s1 --mode systemd)"
  if [[ "$rc" -ne 1 ]]; then
    bad "SPAWN/destroy-failed: exits $rc, want 1"
  elif ! grep -q "EXISTS with an UNVERIFIED boundary" "$TMP/s-leak-nodestroy.err"; then
    bad "SPAWN/destroy-failed: no warning that the agent survived"
  else
    ok "SPAWN/destroy-failed: rc 1 plus a named warning that the agent still exists"
  fi

  # 19. exec argv shape: --server must precede --.
  reset_log
  rc="$(run_gate "$TMP/s-argv" "STUB_STATUS_NET=$SYSTEMD" "STUB_INFO_NET=$SYSTEMD" \
    "STUB_EGRESS=denied" -- verify --agent a1 --server s1 --mode systemd)"
  local execline
  execline="$(grep -m1 '^exec ' "$STUB_LOG" || true)"
  if [[ -z "$execline" ]]; then
    bad "EXEC/argv: no exec invocation recorded (rc=$rc)"
  elif [[ "$execline" != exec\ a1\ --server\ s1\ --\ bash\ -c* ]]; then
    bad "EXEC/argv: '$execline' (want 'exec a1 --server s1 -- bash -c …')"
  else
    ok "EXEC/argv: --server lands before --, remote argv is exactly the probe"
  fi
}

arm_help
arm_usage
arm_verify_matrix
arm_spawn_matrix

echo
echo "bunker-isolation-gate-selftest: $PASSED passed, $FAILED failed"
if [[ "$FAILED" -ne 0 ]]; then
  echo "FAIL — the isolation gate can be talked into (or out of) a wrong verdict" >&2
  exit 1
fi
echo "PASS — every verdict arm behaves, including the fail-closed ones"
exit 0
