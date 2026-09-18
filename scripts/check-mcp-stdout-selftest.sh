#!/usr/bin/env bash
#
# scripts/check-mcp-stdout-selftest.sh — prove the MCP stdout checker is not
# vacuous (DF-CRIER-137).
#
# WHY THIS EXISTS
# ---------------
# scripts/check-mcp-stdout.sh is the gate that keeps make's recipe echo off the
# documented MCP launcher's stdout. A gate that can only say PASS is decoration:
# the same finding was filed three times (DF-CRIER-66, DF-CRIER-91, DF-CRIER-137),
# so this selftest drives the REAL checker against fixture launchers it builds
# under ${TMPDIR:-/tmp} and requires it to REJECT the contamination — including a
# NEUTER proof that the rejection comes from the checker's verdict and not from
# anything else (the DF-CRIER-206/209/189 selftest pattern).
#
# WHAT IS PROVED
# --------------
#   FIXTURE-CLEAN         prints only an initialize frame           -> ACCEPT (0)
#   FIXTURE-CONTAMINATED  prints a build-looking line, then the frame -> REJECT (1),
#                         naming the offending line verbatim
#   FIXTURE-SILENT        prints nothing on stdout                  -> never a green,
#                         exit 2 (the DF-CRIER-208 fail-closed class)
#   FIXTURE-HANG          never answers; a 2s budget is used        -> REJECT (1)
#                         naming the timeout, and the fixture's own pid is gone
#                         afterwards (the child is killed as a process group and
#                         reaped, never orphaned)
#   FIXTURE-UNRUNNABLE    a command that cannot run                 -> exit 2
#   FIXTURE-MAKELEVEL     prints make's own `make[1]: Entering directory …` line
#                         when MAKELEVEL is set (what a parent make exports)
#                                                                  -> ACCEPT, and the
#                         strip of the make-nesting env is reported: that line belongs
#                         to make's nesting, not to the launcher an MCP client runs
#   NEUTER proof          a copy of the checker whose verdict call is forced to
#                         success must ACCEPT the same contaminated fixture the
#                         real checker rejects (and the copy must differ from the
#                         original, so the proof cannot go vacuous)
#   MISSING COUNTERPART   the checker itself missing on disk        -> exit 2
#
# EXIT CODES
#   0  every fixture behaved as required
#   1  at least one expectation was unmet
#   2  this selftest cannot run (the checker is missing on disk)
#
# DEPENDENCIES: bash, mktemp, sed, cmp, grep, kill, timeout (via the checker).

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-mcp-stdout-selftest"
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
CHECKER="$SCRIPT_DIR/check-mcp-stdout.sh"

SELFTEST_TMP=""
HANG_PID_FILE=""
FAILS=0
CHECKS=0

_cleanup() {
  # Never leave a fixture process behind, even if an expectation failed.
  if [ -n "$HANG_PID_FILE" ] && [ -f "$HANG_PID_FILE" ]; then
    local p=""
    p="$(cat "$HANG_PID_FILE" 2>/dev/null || true)"
    if [ -n "$p" ] && kill -0 "$p" 2>/dev/null; then
      kill -KILL "$p" 2>/dev/null
    fi
  fi
  if [ -n "$SELFTEST_TMP" ]; then
    rm -rf "$SELFTEST_TMP"
    SELFTEST_TMP=""
  fi
  return 0
}

_ok() { printf 'PASS: %s\n' "$*"; }
_bad() {
  printf '%s selftest: FAIL: %s\n' "$PROG" "$*" >&2
  FAILS=$((FAILS + 1))
}
# _verdict <fixture> <rc> <verdict> — the per-fixture line, so a reader sees the
# exit code and what the checker said about it.
_verdict() {
  printf 'FIXTURE-%-14s rc=%-3s verdict=%s\n' "$1" "$2" "$3"
}

main() {
  [ -r "$CHECKER" ] || {
    printf '%s: ERROR: %s is not readable — nothing to prove (exit 2)\n' "$PROG" "$CHECKER" >&2
    return 2
  }

  SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-mcp-stdout-selftest.XXXXXX")" || {
    printf '%s: ERROR: mktemp -d failed\n' "$PROG" >&2
    return 2
  }
  trap '_cleanup' EXIT
  local tmp="$SELFTEST_TMP"

  # ── fixtures ────────────────────────────────────────────────────────────────
  # A clean launcher: consumes the initialize frame on stdin and answers with one
  # JSON-RPC line on stdout.
  cat >"$tmp/fx-clean.sh" <<'CLEAN'
#!/usr/bin/env bash
read -r _frame || true
printf '%s\n' '{"jsonrpc":"2.0","result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"fixture","version":"1"},"capabilities":{"tools":{}}},"id":1}'
CLEAN

  # The contaminant the whole task is about: a build-looking line BEFORE the frame.
  # The first line is echoed by make on the documented launcher (measured), so the
  # fixture reproduces the real defect shape, not an invented one.
  cat >"$tmp/fx-contaminated.sh" <<'CONTAM'
#!/usr/bin/env bash
printf '%s\n' 'go build -ldflags "-X github.com/crier-dev/crier/internal/buildinfo.Version=deadbeef" -o bin/crier-mcp ./cmd/crier-mcp'
read -r _frame || true
printf '%s\n' '{"jsonrpc":"2.0","result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"fixture","version":"1"},"capabilities":{"tools":{}}},"id":1}'
CONTAM

  # Prints nothing on stdout and exits 0 — a green over this would be the blank
  # green the checker exists to refuse.
  cat >"$tmp/fx-silent.sh" <<'SILENT'
#!/usr/bin/env bash
exit 0
SILENT

  # Bounds the checker's own timeout path: records its pid, then never answers.
  cat >"$tmp/fx-hang.sh" <<'HANG'
#!/usr/bin/env bash
printf '%s\n' "$$" > "$1"
sleep 30
HANG

  HANG_PID_FILE="$tmp/fx-hang.pid"

  local out="" rc=0 first=""

  # ── 1. FIXTURE-CLEAN -> ACCEPT (exit 0) ─────────────────────────────────────
  CHECKS=$((CHECKS + 1))
  out="$(bash "$CHECKER" "bash $tmp/fx-clean.sh" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    _verdict "CLEAN" "$rc" "REJECT (expected ACCEPT)"
    _bad "a launcher that prints only an initialize frame was REJECTED (rc=$rc)
  output: $out"
  elif ! printf '%s' "$out" | grep -q 'PASS — launcher=bash '"$tmp"'/fx-clean.sh first_stdout_line={"jsonrpc":"2.0"'; then
    _verdict "CLEAN" "$rc" "ACCEPT but the PASS line does not carry the frame"
    _bad "the clean fixture passed without reporting the launcher and its first stdout line
  output: $out"
  else
    _verdict "CLEAN" "$rc" "ACCEPT (expected ACCEPT) — OK"
    _ok "a launcher that prints only an initialize frame is ACCEPTED (rc=$rc) with the launcher and the frame named"
  fi

  # ── 2. FIXTURE-CONTAMINATED -> REJECT (exit 1), offending line named ────────
  CHECKS=$((CHECKS + 1))
  out="$(bash "$CHECKER" "bash $tmp/fx-contaminated.sh" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    _verdict "CONTAMINATED" "$rc" "ACCEPT (expected REJECT)"
    _bad "a launcher that prints a build-looking line before the frame was ACCEPTED (rc=0)
  output: $out"
  elif [ "$rc" -ne 1 ]; then
    _verdict "CONTAMINATED" "$rc" "REJECT with the wrong code (expected 1)"
    _bad "the contaminated fixture was rejected with rc=$rc, not 1 (the documented rejection code)
  output: $out"
  elif ! printf '%s' "$out" | grep -q 'go build -ldflags'; then
    _verdict "CONTAMINATED" "$rc" "REJECT but the offending line was not named"
    _bad "the contaminated fixture was rejected without naming the offending first stdout line
  output: $out"
  elif printf '%s' "$out" | grep -q '^PASS — launcher='; then
    _verdict "CONTAMINATED" "$rc" "REJECT but a green was printed anyway"
    _bad "the contaminated fixture was rejected but a PASS line was printed anyway
  output: $out"
  else
    _verdict "CONTAMINATED" "$rc" "REJECT (expected REJECT) — offending line named"
    _ok "a launcher that prints a build-looking line before the frame is REJECTED (rc=$rc) and the line is named verbatim"
  fi

  # ── 3. FIXTURE-SILENT -> never a green (exit 2, the fail-closed class) ──────
  CHECKS=$((CHECKS + 1))
  out="$(bash "$CHECKER" "bash $tmp/fx-silent.sh" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    _verdict "SILENT" "$rc" "ACCEPT (expected non-pass)"
    _bad "a launcher that printed nothing on stdout was ACCEPTED (rc=0) — a checker that verified nothing reported a green
  output: $out"
  elif [ "$rc" -ne 2 ]; then
    _verdict "SILENT" "$rc" "non-pass with the wrong code (expected 2)"
    _bad "the silent fixture exited $rc, not 2 (the documented fail-closed code)
  output: $out"
  elif printf '%s' "$out" | grep -q '^PASS — launcher='; then
    _verdict "SILENT" "$rc" "non-pass but a green was printed"
    _bad "the silent fixture did not pass but a PASS line was printed anyway
  output: $out"
  elif ! printf '%s' "$out" | grep -q 'produced NO stdout line at all'; then
    _verdict "SILENT" "$rc" "non-pass without naming the cause"
    _bad "the silent fixture was refused without saying that nothing was on stdout
  output: $out"
  else
    _verdict "SILENT" "$rc" "REFUSED, no green (expected non-pass) — OK"
    _ok "a launcher that prints nothing on stdout is refused (rc=$rc) with no green and the cause named"
  fi

  # ── 4. FIXTURE-HANG -> bounded timeout is a FAILURE, and the child is gone ──
  CHECKS=$((CHECKS + 1))
  rm -f "$HANG_PID_FILE"
  out="$(CHECK_MCP_STDOUT_TIMEOUT=2 bash "$CHECKER" "bash $tmp/fx-hang.sh $HANG_PID_FILE" 2>&1)"
  rc=$?
  local hang_pid=""
  [ -f "$HANG_PID_FILE" ] && hang_pid="$(cat "$HANG_PID_FILE" 2>/dev/null || true)"
  if [ "$rc" -ne 1 ]; then
    _verdict "HANG" "$rc" "wrong code (expected 1 = timeout failure)"
    _bad "a launcher that never answers with a 2s budget exited $rc, not 1 (a timeout must not be a skip)
  output: $out"
  elif ! printf '%s' "$out" | grep -q 'TIMEOUT'; then
    _verdict "HANG" "$rc" "rejected without naming the timeout"
    _bad "the hang fixture was rejected without naming the timeout
  output: $out"
  elif [ -z "$hang_pid" ]; then
    _verdict "HANG" "$rc" "rejected but the fixture never recorded its pid"
    _bad "the hang fixture did not record its pid — the orphan check cannot be proved
  output: $out"
  elif kill -0 "$hang_pid" 2>/dev/null; then
    _verdict "HANG" "$rc" "rejected but the child is STILL RUNNING (pid $hang_pid)"
    _bad "the hang fixture's child (pid $hang_pid) outlived the run — the launcher is not reaped
  output: $out"
  else
    _verdict "HANG" "$rc" "REJECT on the 2s budget, child reaped (expected REJECT) — OK"
    _ok "a hanging launcher is REJECTED (rc=$rc) naming the timeout, and its child (pid $hang_pid) is gone afterwards"
  fi

  # ── 5. FIXTURE-UNRUNNABLE -> exit 2, never a green ─────────────────────────
  CHECKS=$((CHECKS + 1))
  out="$(bash "$CHECKER" 'definitely-not-a-real-tool-137 --serve' 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    _verdict "UNRUNNABLE" "$rc" "ACCEPT (expected exit 2)"
    _bad "a launcher whose command cannot run was ACCEPTED (rc=0) — a green over nothing
  output: $out"
  elif [ "$rc" -ne 2 ]; then
    _verdict "UNRUNNABLE" "$rc" "nonzero with the wrong code (expected 2)"
    _bad "the unrunnable launcher exited $rc, not 2 (the documented fail-closed code)
  output: $out"
  elif printf '%s' "$out" | grep -q '^PASS — launcher='; then
    _verdict "UNRUNNABLE" "$rc" "exit 2 but a green was printed anyway"
    _bad "the unrunnable launcher exited 2 but a PASS line was printed anyway
  output: $out"
  elif ! printf '%s' "$out" | grep -q 'produced NO stdout line at all'; then
    _verdict "UNRUNNABLE" "$rc" "exit 2 without naming the launcher"
    _bad "the unrunnable launcher was refused without naming it
  output: $out"
  else
    _verdict "UNRUNNABLE" "$rc" "REFUSED, no green (expected exit 2) — OK"
    _ok "a launcher that cannot run exits 2 (rc=$rc) with the launcher named and no green"
  fi

  # ── 6. NEUTER PROOF: force the checker's verdict to success and re-run the ──
  #      SAME contaminated fixture. Accepting it is what proves the rejection
  #      above is caused by that verdict.
  CHECKS=$((CHECKS + 1))
  local neutered="$tmp/neutered-mcp-stdout-verdict.sh"
  cp "$CHECKER" "$neutered"
  sed -i.tmp -e 's|^\([[:space:]]*\)if ! _is_initialize_response "\$first"; then$|\1if false; then|' "$neutered"
  rm -f "$neutered.tmp"
  if cmp -s "$CHECKER" "$neutered"; then
    _verdict "NEUTER" "-" "the sed no longer matches the verdict line"
    _bad "the neuter sed no longer matches the checker's verdict line (NEUTER-MARK[mcp-stdout-verdict]) — the causality proof would be vacuous"
  else
    out="$(bash "$neutered" "bash $tmp/fx-contaminated.sh" 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      _verdict "NEUTER" "$rc" "neutered copy still REJECTS (causality not proved)"
      _bad "NEUTER PROOF: with the verdict forced to success the same contaminated fixture is STILL rejected (rc=$rc) — the rejection does not come from that verdict
  output: $out"
    elif ! printf '%s' "$out" | grep -q '^PASS — launcher='; then
      _verdict "NEUTER" "$rc" "neutered copy accepted without a PASS line"
      _bad "NEUTER PROOF: the neutered copy exited 0 without reporting a PASS line
  output: $out"
    else
      _verdict "NEUTER" "$rc" "neutered copy ACCEPTS the contaminated fixture (proof holds) — OK"
      _ok "NEUTER PROOF: the neutered copy differs from the original and ACCEPTS the same contaminated fixture (rc=0) — the rejection is caused by that verdict"
    fi
  fi

  # ── 7. FIXTURE-MAKELEVEL: make's own nesting chatter is NOT the launcher's.
  #      Run with MAKELEVEL=1 in the environment (exactly what a parent make
  #      exports) against a fixture that prints `make[1]: Entering directory …`
  #      when — and only when — MAKELEVEL is set. The checker must still ACCEPT:
  #      it strips the make-nesting env and says so.
  CHECKS=$((CHECKS + 1))
  cat >"$tmp/fx-makelevel.sh" <<'MKLVL'
#!/usr/bin/env bash
if [ -n "${MAKELEVEL:-}" ]; then
  printf '%s\n' "make[1]: Entering directory '$(pwd)'"
fi
read -r _frame || true
printf '%s\n' '{"jsonrpc":"2.0","result":{"protocolVersion":"2024-11-05","serverInfo":{"name":"fixture","version":"1"},"capabilities":{"tools":{}}},"id":1}'
MKLVL
  out="$(MAKELEVEL=1 bash "$CHECKER" "bash $tmp/fx-makelevel.sh" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    _verdict "MAKELEVEL" "$rc" "REJECT (expected ACCEPT — make-nesting env must be stripped)"
    _bad "with MAKELEVEL=1 exported (what a parent make sets) the checker REJECTED a launcher over make's own 'Entering directory' line instead of stripping it (rc=$rc)
  output: $out"
  elif ! printf '%s' "$out" | grep -q 'stripped make-nesting env'; then
    _verdict "MAKELEVEL" "$rc" "ACCEPT but the strip was not reported"
    _bad "the make-nesting env was stripped without reporting it (a silent strip hides why the run looked clean)
  output: $out"
  else
    _verdict "MAKELEVEL" "$rc" "ACCEPT, strip reported (expected ACCEPT) — OK"
    _ok "with MAKELEVEL=1 exported the checker strips the make-nesting env, reports it, and ACCEPTS the launcher (rc=$rc)"
  fi

  # ── 8. MISSING COUNTERPART: run a copy of this selftest with NO checker next to
  #      it (that is how the checker is resolved) -> exit 2, never a green.
  CHECKS=$((CHECKS + 1))
  local lonely_dir="$tmp/lonely"
  mkdir -p "$lonely_dir"
  cp "$SELF" "$lonely_dir/$(basename "$SELF")"
  out="$(bash "$lonely_dir/$(basename "$SELF")" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    _verdict "NO-CHECKER" "$rc" "ACCEPT (expected exit 2)"
    _bad "with the checker missing on disk the selftest reported success (rc=0)
  output: $out"
  elif [ "$rc" -ne 2 ]; then
    _verdict "NO-CHECKER" "$rc" "nonzero with the wrong code (expected 2)"
    _bad "with the checker missing on disk the selftest exited $rc, not 2
  output: $out"
  elif ! printf '%s' "$out" | grep -q 'is not readable'; then
    _verdict "NO-CHECKER" "$rc" "exit 2 without naming the missing checker"
    _bad "with the checker missing on disk the selftest refused without naming it
  output: $out"
  elif printf '%s' "$out" | grep -q 'checks behaved'; then
    _verdict "NO-CHECKER" "$rc" "exit 2 but it reported a check tally"
    _bad "with the checker missing on disk the selftest still reported a check tally
  output: $out"
  else
    _verdict "NO-CHECKER" "$rc" "REFUSED (expected exit 2) — OK"
    _ok "with the checker missing on disk the selftest exits 2 (rc=$rc), names it, and reports no tally"
  fi

  if [ "$FAILS" -ne 0 ]; then
    printf '%s selftest: %d/%d checks behaved — FAIL\n' "$PROG" "$((CHECKS - FAILS))" "$CHECKS" >&2
    return 1
  fi
  printf '%s selftest: %d/%d checks behaved\n' "$PROG" "$CHECKS" "$CHECKS"
  return 0
}

# ── entry point ───────────────────────────────────────────────────────────────

case "${1:-}" in
  '') ;;
  -h | --help | help)
    cat <<'EOF'
check-mcp-stdout-selftest.sh — prove the MCP stdout checker rejects contamination
(DF-CRIER-137)

Usage:
  bash scripts/check-mcp-stdout-selftest.sh

Takes no arguments: it drives scripts/check-mcp-stdout.sh against fixture
launchers under ${TMPDIR:-/tmp} and exits 0 only when every fixture behaved
(accept clean, reject contaminated, refuse silent/unrunnable, prove the neuter
copy flips the verdict). Exit codes: 0 all behaved, 1 an expectation was unmet,
2 the checker is missing on disk.
EOF
    exit 0
    ;;
  *)
    printf 'check-mcp-stdout-selftest: ERROR: takes no arguments (got %s) — try --help\n' "$1" >&2
    exit 2
    ;;
esac

main
exit $?
