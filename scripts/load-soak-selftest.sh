#!/usr/bin/env bash
#
# scripts/load-soak-selftest.sh — prove the capacity soak is BOUNDED, gated and
# self-owned (CR-FEAT-033).
#
# WHY THIS EXISTS
# ---------------
# docs/capacity-ceiling.md publishes numbers taken from scripts/load-soak.py.  A
# harness that publishes a "ceiling" is a harness people will reach for, and the
# shape it would otherwise have — start a server, register N agents, open N
# sockets, publish in a loop — is exactly the shape that took this host to
# loadavg 346 on 2026-09-18 (DF-CRIER-254: 278 orphaned burners, no owner, no
# teardown).  So every bound the harness claims is measured here, not asserted in
# prose:
#
#   1-10. every dimension is REFUSED above its cap, and the refusal NAMES the cap:
#         agents > 1000, messages > 5000, events > 100, subscribers > 1000, mesh
#         connections > 1000, concurrency > 8, budget-seconds > 300 (loadgen's own
#         hard lifetime cap), timeout > 30, a non-integer profile, an agent
#         profile of 0
#  11.    the load gate is scripts/loadgen.py's: a synthetic 1-minute loadavg of
#         99 is SKIPPED with exit 3 and a recorded reason naming the measured
#         value AND the threshold — and the run produced no measurement at all
#  12.    an IDLE synthetic loadavg does NOT short-circuit: the run proceeds to the
#         real work (here: refusing the missing binary), so assertion 11 is a
#         gate verdict and not an unconditional early exit
#  13.    an unreadable load-average file FAILS CLOSED (exit 2): an unknown load
#         figure is never read as "the host is idle"
#  14.    BOUNDED STOP: a 1000-agent / 5000-delivery / 100-event profile given a
#         0.5 s budget exits 4 with truncated=true and a named stage instead of
#         running to completion, and the whole invocation is wall-clock bounded
#  15.    a REAL 2-agent run completes and VERIFIES ITSELF: every registration
#         answered 201, every delivery answered 201, the relay fan delivered
#         frames_received == frames_expected to sockets_complete == subscribers,
#         GET /mesh/peers reported exactly the sockets opened, the CR-FEAT-023
#         deliver-to-ping path answered, and the shipped 100/min publish cap was
#         OBSERVED (100 accepted, 10 rejected) rather than assumed
#  16.    NO RESIDUE: the server pid recorded in that run is gone from /proc (read
#         by THIS script, not by the harness that started it) and no process is
#         left LISTENing on the run's port
#  17.    NEUTER PROOF: a copy of the harness whose cap verdict is forced to a
#         no-op ACCEPTS the very request the real harness refuses, so the exit 2
#         is produced by that check and not by accident (the DF-CRIER-206/209/189
#         pattern)
#  18.    ONE IMPLEMENTATION OF THE GATE: the harness takes its threshold, its
#         defaults and its load-average read from loadgen — it carries no second
#         copy of the bound (source assertion)
#  19.    this selftest itself leaves no crier process behind
#
# WHAT IT COSTS: one real 2-agent soak (well under a second of measurement), one
# 0.5 s-budget bounded-stop run, and one neutered 2-agent run.  Every fixture is a
# 2-agent profile except the two that exist to prove the BOUND, and no fixture
# burns CPU: the soak's own gate (assertion 11) plus the budget (assertion 14) are
# the load-safety mechanism, not this script.
#
# USAGE: make load-soak-selftest   (also a CI step; needs a built bin/crier —
# the Makefile target depends on `make build`)

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPTS_DIR="$(cd "$(dirname "$SELF")" && pwd)"
REPO_ROOT="$(cd "$SCRIPTS_DIR/.." && pwd)"
SOAK="$SCRIPTS_DIR/load-soak.py"
BINARY="${SOAK_BINARY:-$REPO_ROOT/bin/crier}"

PASS=0
FAIL=0
declare -a FAILURES=()

_pass() { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
_fail() {
  FAIL=$((FAIL + 1))
  FAILURES+=("$1")
  printf '  FAIL %s\n' "$1"
  [ -n "${2:-}" ] && printf '       %s\n' "$2"
  return 0
}

_assert_eq() { # <want> <got> <name>
  if [ "$1" = "$2" ]; then _pass "$3"; else _fail "$3" "want [$1], got [$2]"; fi
}

_assert_contains() { # <file> <needle> <name>
  if grep -qF -- "$2" "$1" 2>/dev/null; then _pass "$3"; else _fail "$3" "missing [$2] in $1"; fi
}

_assert_not_contains() { # <file> <needle> <name>
  if grep -qF -- "$2" "$1" 2>/dev/null; then _fail "$3" "unexpected [$2] in $1"; else _pass "$3"; fi
}

WORK="$(mktemp -d "${TMPDIR:-/tmp}/load-soak-selftest.XXXXXX")" || {
  echo "load-soak-selftest: cannot create a scratch directory" >&2
  exit 2
}
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# The gate fixture: a synthetic 1-minute loadavg far above any sane threshold.
printf '99.00 99.00 99.00 1/100 1\n' >"$WORK/loaded.loadavg"
printf '0.10 0.10 0.10 1/100 1\n' >"$WORK/idle.loadavg"

SOAK_OUT="$WORK/out"
SOAK_ERR="$WORK/err"
SOAK_RC=0

run_soak() { # <args...> — captures stdout/stderr/exit in SOAK_OUT/SOAK_ERR/SOAK_RC
  : >"$SOAK_OUT"
  : >"$SOAK_ERR"
  python3 "$SOAK" "$@" >"$SOAK_OUT" 2>"$SOAK_ERR"
  SOAK_RC=$?
  return 0
}

jget() { # <json file> <expr over `d`> — one interpreter call per read
  python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
print(eval(sys.argv[2], {"d": d}))' "$1" "$2" 2>/dev/null
}

echo "load-soak-selftest: harness=$SOAK binary=$BINARY"

# ── 1-10: every dimension is capped, and the refusal names the cap ────────────
echo "-- caps (a refusal must name the cap it refused)"

refuse() { # <name> <needle> <args...>
  local name="$1" needle="$2"
  shift 2
  # Every fixture pins a SYNTHETIC IDLE loadavg so the verdict under test is the
  # cap and never this box's current load (the loadgen selftest pattern): the only
  # thing wrong with each request below is the dimension it exceeds.
  run_soak --loadavg-file "$WORK/idle.loadavg" "$@"
  local rc="$SOAK_RC"
  _assert_eq "2" "$rc" "$name exits 2"
  _assert_contains "$SOAK_ERR" "$needle" "$name names the cap"
}

refuse "agents above cap" "hard cap of 1000 agents" --agents 1001
refuse "agents profile 0" "must be >= 1 agent" --agents 0
refuse "agents non-integer" "is not an integer" --agents banana
refuse "messages above cap" "hard cap of 5000" --messages 5001
refuse "events above cap" "hard cap of 100" --events 101
refuse "subscribers above cap" "hard cap of 1000" --subscribers 1001
refuse "mesh connections above cap" "hard cap of 1000" --mesh-connections 1001
refuse "concurrency above cap" "hard cap of 8" --concurrency 9
refuse "timeout above cap" "outside (0, 30]" --timeout 31
# loadgen's own hard lifetime cap: the soak may not outlive what the repo already
# allows a load run to last, and the message says so.
refuse "budget above cap" "outside (0, 300]" --budget-seconds 301

# ── 11-13: the gate is loadgen's, and it fails closed ────────────────────────
echo "-- load gate (one implementation, from scripts/loadgen.py)"

run_soak --agents 2 --loadavg-file "$WORK/loaded.loadavg" --binary "$BINARY"
_assert_eq "3" "$SOAK_RC" "loaded host is SKIPPED (exit 3)"
_assert_contains "$SOAK_OUT" "SKIPPED:" "skip is recorded on stdout"
_assert_contains "$SOAK_OUT" "99.00 is above the threshold 8.00" "skip names the measured loadavg and the threshold"
_assert_not_contains "$SOAK_OUT" "COUNT-SOAK-" "a skipped run publishes NO measurement"

run_soak --agents 2 --loadavg-file "$WORK/idle.loadavg" --binary /nonexistent
_assert_eq "2" "$SOAK_RC" "an idle host does not short-circuit: the run proceeds to the work"
_assert_contains "$SOAK_ERR" "server binary is missing" "the proceeding run fails on the real precondition, not the gate"

run_soak --agents 2 --loadavg-file "$WORK/absent.loadavg"
_assert_eq "2" "$SOAK_RC" "an unreadable load-average file fails CLOSED (exit 2)"

# ── 14: the wall-clock budget is a real bound ────────────────────────────────
echo "-- bounded stop (a huge profile may not run to completion)"

if [ ! -x "$BINARY" ]; then
  _fail "a runnable server binary exists" "missing or not executable: $BINARY — run 'make build'"
else
  started="$(date +%s)"
  run_soak --agents 1000 --messages 5000 --events 100 --budget-seconds 0.5 \
    --loadavg-file "$WORK/idle.loadavg" --binary "$BINARY" --json
  wall=$(( $(date +%s) - started ))
  _assert_eq "4" "$SOAK_RC" "an exhausted budget exits 4 (truncated), not 0"
  _assert_contains "$SOAK_OUT" '"truncated": true' "the summary records truncated=true"
  _assert_contains "$SOAK_OUT" '"truncated_at"' "the summary names the stage it stopped at"
  if [ "$wall" -le 60 ]; then
    _pass "the bounded run stopped inside 60s wall (measured ${wall}s)"
  else
    _fail "the bounded run stopped inside 60s wall" "took ${wall}s"
  fi
fi

# ── 15: the real run, and everything it must prove about itself ──────────────
echo "-- a real 2-agent soak measures and verifies itself"

RUN_JSON="$WORK/run.json"
run_soak --agents 2 --messages 20 --events 5 --loadavg-file "$WORK/idle.loadavg" \
  --binary "$BINARY" --output "$RUN_JSON" --json
_assert_eq "0" "$SOAK_RC" "the 2-agent profile exits 0"

if [ ! -s "$RUN_JSON" ]; then
  _fail "the run wrote its machine-readable summary" "no output at $RUN_JSON"
else
  _assert_eq "2" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['register']['ok']")" "register measured 2/2 agents"
  _assert_eq "2" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['register']['requested']")" "register asked for 2 agents"
  _assert_eq "20" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['deliver']['ok']")" "deliver measured 20/20 deliveries"
  _assert_eq "20" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['deliver_sequential']['ok']")" "the sequential replay measured 20/20"
  fan_rcv="$(jget "$RUN_JSON" "d['runs'][0]['stages']['relay_fanout']['frames_received']")"
  fan_exp="$(jget "$RUN_JSON" "d['runs'][0]['stages']['relay_fanout']['frames_expected']")"
  _assert_eq "$fan_exp" "$fan_rcv" "relay fan-out delivered every expected frame"
  _assert_eq "True" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['relay_fanout']['all_sockets_complete']")" "every subscriber received all events"
  _assert_eq "$(jget "$RUN_JSON" "d['runs'][0]['stages']['mesh_fan']['connections_ok']")" \
    "$(jget "$RUN_JSON" "d['runs'][0]['stages']['mesh_fan']['peers_reported']")" \
    "GET /mesh/peers reports exactly the sockets opened"
  _assert_eq "0" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['mesh_fan']['ping_errors']")" "every inbox ping was received"
  _assert_eq "100" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['rate_limit']['accepted']")" "the 100/min cap accepted exactly 100"
  _assert_eq "10" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['rate_limit']['rejected_429']")" "the 100/min cap rejected the 10 over it"
  _assert_eq "True" "$(jget "$RUN_JSON" "d['runs'][0]['server_stopped_clean']")" "the server stopped cleanly"
  _assert_eq "True" "$(jget "$RUN_JSON" "d['runs'][0]['port_listener_gone']")" "no listener was left on the port"
  _assert_eq "0" "$(jget "$RUN_JSON" "d['runs'][0]['stages']['register']['unattempted']")" "no registration was skipped"

  # ── 16: no residue — checked from OUTSIDE the harness ─────────────────────
  echo "-- no residue (read from /proc by this script)"
  residue="$(python3 -c 'import json,sys,os
d=json.load(open(sys.argv[1]))
bad=[]
for r in d["runs"]:
    pid=r.get("server_pid")
    if isinstance(pid,int) and os.path.isdir("/proc/%d"%pid):
        bad.append("pid %d still alive"%pid)
print(";".join(bad))' "$RUN_JSON")"
  _assert_eq "" "$residue" "no server pid from the run survives"
fi

# ── 17: NEUTER PROOF — the cap verdict is what refuses ───────────────────────
echo "-- neuter proof (the cap check, not an accident, produces the refusal)"

NEUTERED="$WORK/load-soak-neutered.py"
cp "$SOAK" "$NEUTERED"
# The harness imports its gate implementation (scripts/loadgen.py) from the
# directory it lives in, so a neutered COPY must be able to import the same REAL
# module: the copy is byte-identical and is asserted to be, so the neuter edit is
# the only difference between the two runs.
cp "$SCRIPTS_DIR/loadgen.py" "$WORK/loadgen.py"
if cmp -s "$SCRIPTS_DIR/loadgen.py" "$WORK/loadgen.py"; then
  _pass "the neutered fixture imports the REAL loadgen module (no second copy)"
else
  _fail "the neutered fixture imports the REAL loadgen module" "the copied loadgen.py differs from the tracked one"
fi
if ! grep -q '^    raise loadgen.Refused(message)$' "$NEUTERED"; then
  _fail "the cap verdict is one identifiable call to neuter" "pattern not found in $SOAK"
else
  python3 -c 'import sys
p=sys.argv[1]
src=open(p).read()
open(p,"w").write(src.replace("    raise loadgen.Refused(message)","    return None  # NEUTERED"))
print("neutered")' "$NEUTERED" >/dev/null

  run_soak --agents 2 --messages 4 --events 101 --loadavg-file "$WORK/idle.loadavg" --binary "$BINARY"
  _assert_eq "2" "$SOAK_RC" "the REAL harness refuses --events 101"

  : >"$SOAK_OUT"
  : >"$SOAK_ERR"
  python3 "$NEUTERED" --agents 2 --messages 4 --events 101 --loadavg-file "$WORK/idle.loadavg" \
    --binary "$BINARY" >"$SOAK_OUT" 2>"$SOAK_ERR"
  neutered_rc=$?
  _assert_eq "0" "$neutered_rc" "the NEUTERED copy ACCEPTS the same over-cap request (so the refusal is that call's)"
  _assert_not_contains "$SOAK_ERR" "hard cap of 100" "the neutered copy prints no cap refusal"
fi

# ── 18: one implementation of the gate ──────────────────────────────────────
echo "-- one implementation of the bound (loadgen)"

if grep -q '^import loadgen' "$SOAK"; then
  _pass "the harness imports scripts/loadgen.py"
else
  _fail "the harness imports scripts/loadgen.py" "no 'import loadgen' in $SOAK"
fi
_assert_contains "$SOAK" "loadgen.DEFAULT_LOAD_THRESHOLD" "the threshold default comes from loadgen"
_assert_contains "$SOAK" "loadgen.read_loadavg" "the load-average read comes from loadgen"
_assert_contains "$SOAK" "loadgen.MAX_SECONDS" "the wall-clock ceiling is loadgen's own hard cap"
_assert_contains "$SOAK" "loadgen.Refused" "the refusal type is loadgen's"

# ── 19: this selftest leaves nothing behind ─────────────────────────────────
echo "-- this selftest's own residue"
leftover="$(python3 -c 'import os,sys
root=sys.argv[1]
mine={os.getpid(), os.getppid()}
bad=[]
for pid in os.listdir("/proc"):
    if not pid.isdigit() or int(pid) in mine:
        continue
    try:
        cmd=open("/proc/%s/cmdline"%pid,"rb").read().decode("utf-8","replace")
    except OSError:
        continue
    if root in cmd and "load-soak" in cmd:
        bad.append(pid)
print(";".join(bad))' "$WORK")"
_assert_eq "" "$leftover" "no harness process outlived this selftest"

# ── verdict ─────────────────────────────────────────────────────────────────
total=$((PASS + FAIL))
echo "load-soak-selftest: ${PASS}/${total} assertions passed"
if [ "$FAIL" -gt 0 ]; then
  echo "load-soak-selftest: FAILED — ${FAIL} assertion(s):" >&2
  for f in "${FAILURES[@]}"; do
    printf '  - %s\n' "$f" >&2
  done
  exit 1
fi
echo "load-soak-selftest: PASS — the soak's caps refuse, the gate skips, the budget bounds, the run verifies itself and leaves nothing behind"
exit 0
