#!/usr/bin/env bash
#
# scripts/load-repro-selftest.sh — prove the bounded load harness is bounded and
# leaves nothing behind (DF-CRIER-254).
#
# WHY THIS EXISTS
# ---------------
# The 2026-09-18 host lock-up happened because a "load the box" step had no bound
# and no teardown: 73 -> 278 burners reparented to the user systemd manager and
# outlived every caller (loadavg 220/346/237 on a box shared with the Hermes
# gateway, the scheduler and DuckBrain).  A harness that CLAIMS to be bounded is
# worth nothing unless the claim is measured, so this selftest drives the real
# files against real processes and requires each property to hold:
#
#   1. workers 9 is REFUSED, and the message names the cap (8)
#   2. seconds 301 is REFUSED, and the message names the cap (300)
#   3. an out-of-range --cpus spec is REFUSED (never silently intersected)
#   4. a loadavg above the threshold SKIPS the run: exit 3, one SKIPPED line
#      naming the measured value AND the threshold
#   5. a loadavg below the threshold lets the run proceed, and the summary
#      records the value it measured
#   6. a loadavg EQUAL to the threshold proceeds — the gate is "strictly above"
#   7. after a normal exit, every started child pid is GONE (checked from /proc
#      by this script, not by the process that started them), the summary says
#      clean=true, and the children reported arming PR_SET_PDEATHSIG
#   8. SIGKILLing the RUNNER mid-run leaves no child behind (the PDEATHSIG proof),
#      with the premise asserted: the children WERE alive when the kill landed
#   9. the wrapper exits with the TARGET's exit code and prints one parseable
#      summary line
#  10. the wrapper relays the generator's SKIPPED verdict (exit 3) and does NOT
#      run the target at all
#  11. SIGTERMing the WRAPPER mid-run leaves no generator, no burner and no
#      target behind, and its summary records the signal
#  12. a second concurrent run is refused by the lockfile, naming the holder
#  13. NEUTER PROOF: a copy of the survivor check with its verdict call forced to
#      success ACCEPTS a fixture process that IS still alive, while the real
#      check rejects it — so the rejection is produced by that check, not by
#      accident (the DF-CRIER-206/209/189 pattern)
#  14. a ZOMBIE is treated as dead: an unreaped corpse is visible in /proc and
#      must never be reported as a survivor
#  15. this selftest itself left nothing behind
#
# PREMISES ARE ASSERTED, NOT ASSUMED
# ----------------------------------
# A "no survivors" reading is worthless if the processes were already gone when
# the signal landed.  Assertions 8 and 11 therefore check the PREMISE (the runner
# and its children were alive at the moment of the kill), retry once on a slow
# box, and FAIL loudly rather than passing for the wrong reason.
#
# SHARED-HOST DISCIPLINE (this box runs the Hermes gateway, the scheduler and
# DuckBrain)
# ---------------------------------------------------------------------------
# Every fixture run is tiny and bounded: <= 2 workers and <= 2 s of generator
# lifetime, and the threshold cases always read a SYNTHETIC --loadavg-file so the
# gate itself is exercised without depending on (or adding to) the real load.
# No fixture contains or executes the busy-wait idiom the host reaper kills — the
# "burner" fixtures are a bounded python CPU loop and `sleep`s, all invoked as
# FILE paths, so no process command line ever carries the idiom.  Every fixture
# pid is tracked and the traps TEAR THEM DOWN on EXIT/INT/TERM/HUP.
#
# EXIT CODES
#   0  every assertion behaved
#   1  at least one assertion failed (the line names which)
#   2  a dependency is missing (python3, flock, /proc) — never a silent skip
#
# DEPENDENCIES: bash, python3, flock, /proc (Linux).  No root, no network, no
# docker, and no process outside this selftest's own fixtures is touched.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPTS_DIR="$(cd "$(dirname "$SELF")" && pwd)"
LOADGEN="$SCRIPTS_DIR/loadgen.py"
WRAPPER="$SCRIPTS_DIR/load-repro.sh"
SURVIVORS_LIB="$SCRIPTS_DIR/lib/load-survivors.sh"
PYTHON="${PYTHON:-python3}"

PROG="load-repro-selftest"

checks=0
fails=0
TMP=""
TRACKED=()

_pass() {
  checks=$((checks + 1))
  printf 'PASS  %s\n' "$1"
  return 0
}

_fail() {
  checks=$((checks + 1))
  fails=$((fails + 1))
  printf 'FAIL  %s\n' "$1"
  if [ -n "${2:-}" ]; then
    printf '        %s\n' "$2" >&2
  fi
  return 0
}

_note() { printf '%s: %s\n' "$PROG" "$*"; }

# ── dependency + layout checks (fail closed, never a silent skip) ────────────
_missing=0
for dep in "$PYTHON" flock mktemp; do
  if ! command -v "$dep" >/dev/null 2>&1; then
    _note "ERROR: '$dep' is not on PATH — refusing to run a partial selftest"
    _missing=1
  fi
done
for f in "$LOADGEN" "$WRAPPER" "$SURVIVORS_LIB"; do
  if [ ! -f "$f" ]; then
    _note "ERROR: missing file: $f"
    _missing=1
  fi
done
if [ ! -d /proc ]; then
  _note "ERROR: /proc is not mounted — the liveness checks cannot run here"
  _missing=1
fi
[ "$_missing" -eq 0 ] || exit 2

# shellcheck source=lib/load-survivors.sh
. "$SURVIVORS_LIB"

# ── fixture bookkeeping ──────────────────────────────────────────────────────

_track() { [ -n "${1:-}" ] && TRACKED+=("$1"); return 0; }

_cleanup() {
  local p i=0 alive=0
  for p in ${TRACKED[@]+"${TRACKED[@]}"}; do
    if load_pid_alive "$p"; then
      kill -TERM "$p" 2>/dev/null
    fi
  done
  while [ "$i" -lt 20 ]; do
    alive=0
    for p in ${TRACKED[@]+"${TRACKED[@]}"}; do
      if load_pid_alive "$p"; then
        alive=1
      fi
    done
    [ "$alive" -eq 0 ] && break
    sleep 0.1
    i=$((i + 1))
  done
  for p in ${TRACKED[@]+"${TRACKED[@]}"}; do
    if load_pid_alive "$p"; then
      kill -KILL "$p" 2>/dev/null
    fi
  done
  [ -n "$TMP" ] && rm -rf "$TMP"
  TMP=""
  return 0
}

_on_signal() {
  printf '%s: interrupted by SIG%s — tearing the fixtures down\n' "$PROG" "$1" >&2
  exit "$2"
}

# ── small helpers ────────────────────────────────────────────────────────────

ppid_of() { sed -n 's/^PPid:[[:space:]]*//p' "/proc/$1/status" 2>/dev/null | head -n 1; }

cmdline_of() { tr '\0' ' ' <"/proc/$1/cmdline" 2>/dev/null; }

children_of() { # pid -> child pids, space separated (empty when none)
  local want="$1" d pid out=""
  for d in /proc/[0-9]*; do
    pid="${d#/proc/}"
    if [ "$(ppid_of "$pid")" = "$want" ]; then
      out="$out $pid"
    fi
  done
  printf '%s\n' "${out# }"
}

# Find (by polling) a direct child of <pid> whose command line contains <marker>.
wait_child_matching() { # <parent-pid> <marker> <tenths>
  local parent="$1" marker="$2" budget="${3:-100}" i=0 p
  while [ "$i" -lt "$budget" ]; do
    for p in $(children_of "$parent"); do
      case "$(cmdline_of "$p")" in
        *"$marker"*)
          printf '%s\n' "$p"
          return 0
          ;;
      esac
    done
    sleep 0.1
    i=$((i + 1))
  done
  return 1
}

# Find (by polling) a direct child of <parent> that is NOT the generator: the
# wrapper's target.  Returns nothing (rc 1) if it never appeared.
wait_target_matching() { # <parent-pid> <tenths> <exclude-pid>
  local parent="$1" budget="${2:-40}" exclude="${3:-}" i=0 p
  while [ "$i" -lt "$budget" ]; do
    for p in $(children_of "$parent"); do
      [ "$p" = "$exclude" ] && continue
      printf '%s\n' "$p"
      return 0
    done
    sleep 0.05
    i=$((i + 1))
  done
  return 1
}

read_started_pids() { sed -n 's/.* pids=//p' "$1" 2>/dev/null | head -n 1; }

all_gone() { # pids... -> 0 when none is alive, 1 when any is alive
  local p
  for p in "$@"; do
    [ -n "$p" ] || continue
    if load_pid_alive "$p"; then
      return 1
    fi
  done
  return 0
}

wait_all_gone() { # <tenths> pids...
  local budget="$1" i=0
  shift
  while [ "$i" -lt "$budget" ]; do
    all_gone "$@" && return 0
    sleep 0.1
    i=$((i + 1))
  done
  all_gone "$@"
}

# _json_field <summary-line> <key> -> the value, or <unparsed:...>
_json_field() {
  local line="$1" key="$2" payload="${line#load-repro: }"
  printf '%s' "$payload" | "$PYTHON" -c '
import json, sys
key = sys.argv[1]
try:
    print(json.load(sys.stdin)[key])
except Exception as exc:
    print("<unparsed:%s>" % exc)
' "$key"
}

# The shapes /proc/loadavg has: five fields, the first is the 1-minute average.
make_loadavg_fixture() { # <path> <1-min> <5-min> <15-min>
  printf '%s %s %s 1/100 12345\n' "$2" "$3" "$4" >"$1"
}

# ── the battery ──────────────────────────────────────────────────────────────

run_battery() {
  local low="$TMP/loadavg-low" high="$TMP/loadavg-high" eq="$TMP/loadavg-eq"
  make_loadavg_fixture "$low" 0.05 0.10 0.15
  make_loadavg_fixture "$high" 12.34 11.00 10.00
  make_loadavg_fixture "$eq" 2.00 1.00 1.00

  local out="" rc=0 line="" w_out="" w_err="" marker="" lockfile="" holder=""
  local pids_csv="" pids=() p="" attempt=0 premise_ok=0 survivors=""
  local runner="" gen_pid="" target_pid="" wrap_pid="" wrap1="" burners=()
  local waited=0 spin="" spin_pid="" neutered="" real_out="" neut_out=""
  local real_rc=0 neut_rc=0 zomb="" zomb_out="" zomb_holder="" zomb_pid="" zstate=""
  local gen_out="$TMP/gen.out" gen_err="$TMP/gen.err"
  local all=()

  printf '\n%s: bounded load harness selftest (fixtures are <= 2 workers / <= 2 s)\n\n' "$PROG"

  # ── 1. worker cap ──────────────────────────────────────────────────────────
  out="$("$PYTHON" "$LOADGEN" --workers 9 --seconds 1 --loadavg-file "$low" --json 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ]; then
    _fail "(1) workers 9 is refused with exit 2" "rc=$rc output: $out"
  elif ! printf '%s' "$out" | grep -q 'hard cap of 8'; then
    _fail "(1) workers 9 is refused, naming the cap (8)" "output: $out"
  else
    _pass "(1) workers 9 refused (rc=2) and the message names the hard cap of 8"
  fi

  # ── 2. lifetime cap ────────────────────────────────────────────────────────
  out="$("$PYTHON" "$LOADGEN" --seconds 301 --workers 1 --loadavg-file "$low" --json 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ]; then
    _fail "(2) seconds 301 is refused with exit 2" "rc=$rc output: $out"
  elif ! printf '%s' "$out" | grep -q 'hard cap of 300s'; then
    _fail "(2) seconds 301 is refused, naming the cap (300s)" "output: $out"
  else
    _pass "(2) seconds 301 refused (rc=2) and the message names the hard cap of 300s"
  fi

  # ── 3. a bad --cpus spec is refused, never silently intersected ────────────
  out="$("$PYTHON" "$LOADGEN" --workers 1 --seconds 1 --loadavg-file "$low" --cpus 9999 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ]; then
    _fail "(3) an out-of-range --cpus is refused with exit 2" "rc=$rc output: $out"
  elif ! printf '%s' "$out" | grep -q 'does not allow'; then
    _fail "(3) the --cpus refusal names the CPUs the host does not allow" "output: $out"
  else
    _pass "(3) --cpus 9999 refused (rc=2), naming the CPUs outside the host's set"
  fi

  # ── 4. the load gate skips, with the recorded reason ───────────────────────
  out="$("$PYTHON" "$LOADGEN" --workers 1 --seconds 1 --loadavg-file "$high" --load-threshold 8 2>&1)"
  rc=$?
  if [ "$rc" -ne 3 ]; then
    _fail "(4) a loadavg above the threshold skips with exit 3" "rc=$rc output: $out"
  elif ! printf '%s' "$out" | grep -q 'SKIPPED:'; then
    _fail "(4) the skip prints a SKIPPED reason" "output: $out"
  elif ! printf '%s' "$out" | grep -q '12.34'; then
    _fail "(4) the SKIPPED reason names the measured 1-minute loadavg" "output: $out"
  elif ! printf '%s' "$out" | grep -q '8.00'; then
    _fail "(4) the SKIPPED reason names the threshold" "output: $out"
  else
    _pass "(4) loadavg 12.34 above threshold 8.00 -> exit 3 with 'SKIPPED: ... 12.34 ... 8.00'"
  fi

  # ── 5. below the threshold the run proceeds and records what it measured ───
  out="$("$PYTHON" "$LOADGEN" --workers 1 --seconds 1 --loadavg-file "$low" --json 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    _fail "(5) a loadavg below the threshold proceeds (rc=0)" "rc=$rc output: $out"
  elif ! printf '%s' "$out" | grep -q '"loadavg": 0.05'; then
    _fail "(5) the summary records the loadavg it measured" "output: $out"
  else
    _pass "(5) loadavg 0.05 below threshold 8.00 -> exit 0, summary records the measured 0.05"
  fi

  # ── 6. the gate is STRICTLY above (load == threshold runs) ─────────────────
  out="$("$PYTHON" "$LOADGEN" --workers 1 --seconds 1 --loadavg-file "$eq" --load-threshold 2 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    _fail "(6) a loadavg EQUAL to the threshold is not 'above' it" "rc=$rc output: $out"
  else
    _pass "(6) loadavg 2.00 == threshold 2 -> proceeds (the gate is strictly above)"
  fi

  # ── 7. a normal exit leaves no child behind (checked independently) ────────
  "$PYTHON" "$LOADGEN" --workers 2 --seconds 2 --loadavg-file "$low" --json \
    >"$gen_out" 2>"$gen_err"
  rc=$?
  pids_csv="$(read_started_pids "$gen_err")"
  pids=(${pids_csv//,/ })
  if [ "$rc" -ne 0 ]; then
    _fail "(7) a 2x2s run exits 0" "rc=$rc output: $(cat "$gen_out")"
  elif [ "${#pids[@]}" -ne 2 ]; then
    _fail "(7) the start line names both started pids" "start line: $(grep -m1 started "$gen_err")"
  elif ! grep -q '"clean": true' "$gen_out"; then
    _fail "(7) the summary reports clean=true" "output: $(cat "$gen_out")"
  elif ! grep -q '"pdeathsig": true' "$gen_out"; then
    _fail "(7) the children reported arming PR_SET_PDEATHSIG" "output: $(cat "$gen_out")"
  elif ! wait_all_gone 20 "${pids[@]}"; then
    _fail "(7) every started child is gone after a normal exit" "still alive: ${pids[*]}"
  else
    _pass "(7) 2x2s run: exit 0, pdeathsig=true, and both child pids are gone from /proc"
  fi

  # ── 8. SIGKILL the RUNNER mid-run: no child survives (PDEATHSIG proof) ─────
  # The premise is asserted too: if the children were already gone when the kill
  # landed, the "no survivors" reading would be vacuous, so this retries once and
  # fails loudly rather than passing for the wrong reason.
  premise_ok=0
  survivors=""
  for attempt in 1 2; do
    gen_out="$TMP/run8.out"
    gen_err="$TMP/run8.err"
    "$PYTHON" "$LOADGEN" --workers 2 --seconds 2 --loadavg-file "$low" --json \
      >"$gen_out" 2>"$gen_err" &
    runner=$!
    _track "$runner"
    waited=0
    pids_csv=""
    while [ "$waited" -lt 100 ]; do
      pids_csv="$(read_started_pids "$gen_err")"
      [ -n "$pids_csv" ] && break
      load_pid_alive "$runner" || break
      sleep 0.05
      waited=$((waited + 1))
    done
    pids=(${pids_csv//,/ })
    if [ -n "$pids_csv" ] && load_pid_alive "$runner" && ! all_gone "${pids[@]}"; then
      premise_ok=1
      kill -9 "$runner"
      survivors=""
      if ! wait_all_gone 100 "${pids[@]}"; then
        survivors="$(load_survivors_verdict "${pids[@]}")"
      fi
      wait "$runner" 2>/dev/null
      break
    fi
    # The run finished before the kill could land (slow box): clean up and retry.
    kill -9 "$runner" 2>/dev/null
    wait "$runner" 2>/dev/null
  done
  if [ "$premise_ok" -ne 1 ]; then
    _fail "(8) SIGKILL of the runner: the children were still alive when the kill landed" \
      "could not catch the run mid-flight in 2 attempts — a no-survivor reading would be vacuous"
  elif [ -n "$survivors" ]; then
    _fail "(8) SIGKILL of the runner leaves no child behind (PDEATHSIG)" \
      "survivors: $(printf '%s' "$survivors" | tr '\n' ' ')"
  else
    _pass "(8) SIGKILL of the runner mid-run (premise: burners alive) -> every child gone via PDEATHSIG"
  fi

  # ── 9. the wrapper exits with the TARGET's exit code ──────────────────────
  w_out="$TMP/run9.out"
  w_err="$TMP/run9.err"
  bash "$WRAPPER" --workers 2 --seconds 2 --loadavg-file "$low" -- bash -c 'exit 7' \
    >"$w_out" 2>"$w_err"
  rc=$?
  line="$(grep -m1 '^load-repro: {' "$w_out")"
  if [ "$rc" -ne 7 ]; then
    _fail "(9) the wrapper exits with the target's exit code (7)" \
      "rc=$rc stdout: $(cat "$w_out") stderr: $(cat "$w_err")"
  elif [ -z "$line" ]; then
    _fail "(9) the wrapper prints one machine-readable summary line" "stdout: $(cat "$w_out")"
  elif [ "$(_json_field "$line" target_rc)" != "7" ]; then
    _fail "(9) the summary line carries target_rc=7" "line: $line"
  elif [ "$(_json_field "$line" clean)" != "True" ]; then
    _fail "(9) the summary line reports clean=true" "line: $line"
  else
    _pass "(9) wrapper exits 7 (the target's code) and prints one parseable summary line with target_rc=7, clean=true"
  fi

  # ── 10. the wrapper refuses above the threshold, never starting the target ──
  marker="$TMP/target-ran-marker"
  rm -f "$marker"
  w_out="$TMP/run10.out"
  w_err="$TMP/run10.err"
  bash "$WRAPPER" --workers 2 --seconds 2 --loadavg-file "$high" --load-threshold 8 \
    -- bash -c "touch '$marker'" >"$w_out" 2>"$w_err"
  rc=$?
  if [ "$rc" -ne 3 ]; then
    _fail "(10) the wrapper refuses above the threshold with exit 3" \
      "rc=$rc stdout: $(cat "$w_out") stderr: $(cat "$w_err")"
  elif ! grep -q 'SKIPPED:' "$w_out"; then
    _fail "(10) the wrapper relays the recorded SKIPPED reason" "stdout: $(cat "$w_out")"
  elif ! grep -q '12.34' "$w_out" || ! grep -q '8.00' "$w_out"; then
    _fail "(10) the relayed reason names the measured loadavg and the threshold" "stdout: $(cat "$w_out")"
  elif [ -e "$marker" ]; then
    _fail "(10) the target is NOT run when the run is skipped" "the target created $marker anyway"
  else
    _pass "(10) wrapper above threshold -> exit 3, SKIPPED reason relayed (12.34 / 8.00), target NOT run"
  fi

  # ── 11. SIGTERM the WRAPPER mid-run: generator, burners and target all gone ─
  premise_ok=0
  survivors=""
  burners=()
  for attempt in 1 2; do
    w_out="$TMP/run11.out"
    w_err="$TMP/run11.err"
    bash "$WRAPPER" --workers 2 --seconds 2 --loadavg-file "$low" -- sleep 10 \
      >"$w_out" 2>"$w_err" &
    wrap_pid=$!
    _track "$wrap_pid"
    gen_pid="$(wait_child_matching "$wrap_pid" "loadgen.py" 60)" || gen_pid=""
    if [ -z "$gen_pid" ]; then
      kill -TERM "$wrap_pid" 2>/dev/null
      wait "$wrap_pid" 2>/dev/null
      continue
    fi
    # Poll until the generator has at least 2 burner children (its start line is
    # printed once they exist AND have armed PDEATHSIG), and until the wrapper's
    # target child exists too.
    burners=()
    waited=0
    while [ "$waited" -lt 60 ]; do
      burners=($(children_of "$gen_pid"))
      target_pid="$(wait_target_matching "$wrap_pid" 1 "$gen_pid")" || target_pid=""
      if [ "${#burners[@]}" -ge 2 ] && [ -n "$target_pid" ]; then
        break
      fi
      sleep 0.05
      waited=$((waited + 1))
    done
    if [ "${#burners[@]}" -ge 2 ] && [ -n "$target_pid" ] && load_pid_alive "$gen_pid" &&
      load_pid_alive "$wrap_pid" && load_pid_alive "$target_pid"; then
      premise_ok=1
      kill -TERM "$wrap_pid"
      all=("$gen_pid" "$wrap_pid" "$target_pid")
      all+=(${burners[@]+"${burners[@]}"})
      survivors=""
      if ! wait_all_gone 100 "${all[@]}"; then
        survivors="$(load_survivors_verdict "${all[@]}")"
      fi
      wait "$wrap_pid" 2>/dev/null
      break
    fi
    kill -TERM "$wrap_pid" 2>/dev/null
    wait "$wrap_pid" 2>/dev/null
  done
  line="$(grep -m1 '^load-repro: {' "$w_out" 2>/dev/null)"
  if [ "$premise_ok" -ne 1 ]; then
    _fail "(11) SIGTERM of the wrapper: the generator, its burners and the target were running" \
      "could not catch the run mid-flight in 2 attempts — a no-survivor reading would be vacuous"
  elif [ -n "$survivors" ]; then
    _fail "(11) SIGTERM of the wrapper leaves no generator, burner or target behind" \
      "survivors: $(printf '%s' "$survivors" | tr '\n' ' ')"
  elif [ "$(_json_field "$line" signal)" != "TERM" ]; then
    _fail "(11) the wrapper's summary records the interrupting signal" "line: $line"
  elif [ "$(_json_field "$line" clean)" != "True" ]; then
    _fail "(11) the wrapper's summary reports clean=true" "line: $line"
  else
    _pass "(11) SIGTERM of the wrapper mid-run -> generator, ${#burners[@]} burners and target all gone; summary signal=TERM clean=true"
  fi

  # ── 12. the lockfile refuses a second concurrent run, naming the holder ────
  lockfile="$TMP/load-repro.lock"
  rm -f "$lockfile"
  holder=""
  waited=0
  LOAD_REPRO_LOCKFILE="$lockfile" bash "$WRAPPER" --workers 1 --seconds 2 --loadavg-file "$low" \
    -- sleep 10 >"$TMP/run12a.out" 2>"$TMP/run12a.err" &
  wrap1=$!
  _track "$wrap1"
  while [ "$waited" -lt 100 ]; do
    holder="$(cat "$lockfile" 2>/dev/null)"
    [ "$holder" = "$wrap1" ] && break
    sleep 0.05
    waited=$((waited + 1))
  done
  LOAD_REPRO_LOCKFILE="$lockfile" bash "$WRAPPER" --workers 1 --seconds 2 --loadavg-file "$low" \
    -- true >"$TMP/run12b.out" 2>"$TMP/run12b.err"
  rc=$?
  if [ "$holder" != "$wrap1" ]; then
    _fail "(12) the first run recorded itself as the lock holder" \
      "lockfile content: '${holder:-<empty>}' expected '$wrap1'"
  elif [ "$rc" -ne 3 ]; then
    _fail "(12) a second concurrent run is refused with exit 3" \
      "rc=$rc stdout: $(cat "$TMP/run12b.out") stderr: $(cat "$TMP/run12b.err")"
  elif ! grep -q 'another load-repro is already running' "$TMP/run12b.out"; then
    _fail "(12) the refusal says another run is already running" "stdout: $(cat "$TMP/run12b.out")"
  elif ! grep -q "holder pid $wrap1" "$TMP/run12b.out"; then
    _fail "(12) the refusal names the holder" "stdout: $(cat "$TMP/run12b.out")"
  else
    _pass "(12) a second concurrent run refuses (rc=3) naming the holder pid $wrap1"
  fi
  kill -TERM "$wrap1" 2>/dev/null
  wait "$wrap1" 2>/dev/null

  # ── 13. NEUTER PROOF for the survivor check ────────────────────────────────
  # A bounded CPU-burning fixture (a FILE invocation — never the busy-wait idiom
  # the host reaper kills) is started, the premise that it IS alive is asserted,
  # and then: the real check must REJECT it, while a copy of the check with its
  # verdict line forced to success must ACCEPT it.
  spin="$TMP/spin_fixture.py"
  cat >"$spin" <<'PY'
import sys, time
deadline = time.monotonic() + float(sys.argv[1])
counter = 0
while time.monotonic() < deadline:
    counter += 1
print(counter)
PY
  "$PYTHON" "$spin" 2 >/dev/null 2>&1 &
  spin_pid=$!
  _track "$spin_pid"
  waited=0
  premise_ok=0
  while [ "$waited" -lt 40 ]; do
    if load_pid_alive "$spin_pid"; then
      premise_ok=1
      break
    fi
    sleep 0.05
    waited=$((waited + 1))
  done
  neutered="$TMP/neutered-survivors.sh"
  cp "$SURVIVORS_LIB" "$neutered"
  sed -i.tmp -e 's|^\([[:space:]]*\)if \[ "\$survivor_count" -gt 0 \]; then$|\1if false; then|' "$neutered"
  rm -f "$neutered.tmp"
  if [ "$premise_ok" -ne 1 ]; then
    _fail "(13) NEUTER PROOF: the fixture burner is alive when the verdict runs" \
      "the spin fixture (pid $spin_pid) was already gone — the proof would be vacuous"
  elif cmp -s "$SURVIVORS_LIB" "$neutered"; then
    _fail "(13) NEUTER PROOF: the neuter sed still matches the verdict line" \
      "the copy is byte-identical to $SURVIVORS_LIB — the causality proof would be vacuous"
  else
    real_out="$(load_survivors_verdict "$spin_pid")"
    real_rc=$?
    neut_out="$(bash -c '. "$1"; load_survivors_verdict "$2"' _ "$neutered" "$spin_pid")"
    neut_rc=$?
    if [ "$real_rc" -ne 1 ] || [ "$real_out" != "$spin_pid" ]; then
      _fail "(13) the real survivor check rejects the live fixture burner" \
        "rc=$real_rc output='$real_out' (expected rc=1 and the pid $spin_pid)"
    elif [ "$neut_rc" -ne 0 ]; then
      _fail "(13) NEUTER PROOF: the neutered copy ACCEPTS the same live burner" \
        "rc=$neut_rc output='$neut_out' — forcing the verdict to success must accept it"
    else
      _pass "(13) NEUTER PROOF: the real check rejects the live burner $spin_pid (rc=1) while the neutered copy accepts it (rc=0)"
    fi
  fi
  kill -TERM "$spin_pid" 2>/dev/null
  wait "$spin_pid" 2>/dev/null

  # ── 14. a zombie is dead ───────────────────────────────────────────────────
  zomb="$TMP/zombie_fixture.py"
  cat >"$zomb" <<'PY'
import os, sys, time
pid = os.fork()
if pid == 0:
    os._exit(0)  # exits at once; stays unreaped until the parent waits
print(pid, flush=True)
time.sleep(float(sys.argv[1]))
os.waitpid(pid, 0)
PY
  zomb_out="$TMP/zombie.out"
  "$PYTHON" "$zomb" 2 >"$zomb_out" 2>&1 &
  zomb_holder=$!
  _track "$zomb_holder"
  zomb_pid=""
  waited=0
  while [ "$waited" -lt 40 ]; do
    zomb_pid="$(head -n 1 "$zomb_out" 2>/dev/null)"
    [ -n "$zomb_pid" ] && break
    sleep 0.05
    waited=$((waited + 1))
  done
  zstate=""
  [ -n "$zomb_pid" ] && zstate="$(load_pid_state "$zomb_pid")"
  if [ "$zstate" != "Z" ]; then
    _fail "(14) a zombie is treated as dead: the fixture really is a zombie" \
      "state='${zstate:-<none>}' pid='${zomb_pid:-<none>}' (premise: an unreaped corpse in state Z)"
  elif load_pid_alive "$zomb_pid"; then
    _fail "(14) a zombie pid is not reported as alive" \
      "pid $zomb_pid is state Z but load_pid_alive said alive"
  elif ! load_survivors_verdict "$zomb_pid" >/dev/null; then
    _fail "(14) a zombie is not reported as a survivor" \
      "pid $zomb_pid is state Z but the verdict rejected the run"
  else
    _pass "(14) pid $zomb_pid is state Z (a real unreaped corpse) and is treated as DEAD, never a survivor"
  fi
  kill -TERM "$zomb_holder" 2>/dev/null
  wait "$zomb_holder" 2>/dev/null

  # ── 15. this selftest left nothing behind ──────────────────────────────────
  local leftover=""
  for p in ${TRACKED[@]+"${TRACKED[@]}"}; do
    if load_pid_alive "$p"; then
      leftover="$leftover $p"
    fi
  done
  if [ -n "$leftover" ]; then
    _fail "(15) the selftest left nothing behind" "still alive:$leftover"
  else
    _pass "(15) nothing left behind: every fixture pid this selftest started is gone"
  fi

  printf '\n'
  if [ "$fails" -ne 0 ]; then
    printf '%s: %d/%d assertions behaved — FAIL\n' "$PROG" "$((checks - fails))" "$checks" >&2
    return 1
  fi
  printf '%s: %d/%d assertions behaved\n' "$PROG" "$checks" "$checks"
  return 0
}

main() {
  case "${1:-}" in
    -h | --help | help)
      cat <<'EOF'
load-repro-selftest.sh — prove the bounded load harness is bounded (DF-CRIER-254)

Usage:
  bash scripts/load-repro-selftest.sh

Runs the real scripts/loadgen.py and scripts/load-repro.sh against tiny fixtures
(<= 2 workers, <= 2 s) with a synthetic --loadavg-file, prints one PASS/FAIL line
per assertion, and exits nonzero if any of them failed. Fixture pids are tracked
and torn down on EXIT/INT/TERM/HUP, so an interrupted run leaves no burner.

Exit codes: 0 all behaved, 1 at least one failed, 2 a dependency is missing.
EOF
      return 0
      ;;
  esac
  TMP="$(mktemp -d "${TMPDIR:-/tmp}/load-repro-selftest.XXXXXX")" || {
    _note "ERROR: mktemp -d failed"
    return 2
  }
  trap '_cleanup' EXIT
  trap '_on_signal INT 130' INT
  trap '_on_signal TERM 143' TERM
  trap '_on_signal HUP 129' HUP
  run_battery
}

main "$@"
exit $?
