#!/usr/bin/env bash
#
# scripts/check-demo-cleanup-selftest.sh — prove the server-spawn cleanup gate is
# not vacuous (CR-GAP-069).
#
# WHY THIS EXISTS
# ---------------
# scripts/check-demo-cleanup.sh is the gate that keeps an ad-hoc dogfood script
# (or a future example runner) from re-creating the leak the dogfood federation
# demo left behind: two orphan crier servers on :18767/:18877 that had to be
# reaped by hand. A checker that can only say PASS is decoration — and a checker
# whose fixtures are not driven through its REAL rejection path can go green while
# enforcing nothing. This selftest therefore drives the real checker against
# fixture scripts it creates under ${TMPDIR:-/tmp} and requires a REJECTION for
# every violation shape, plus the DF-CRIER-206/209/189 NEUTER proof that the
# rejection comes from the checker's verdict and from nothing else.
#
# WHAT IS PROVED
# --------------
#   TRACKED TREE        no-argument run over the repo's own tracked scripts
#                       (the compliant population: scripts/e2e-battery.sh, the
#                       examples/**/run-demo.sh runners, scripts/check-mcp-stdout.sh,
#                       scripts/onboard-connection-selftest.sh, and any future
#                       server-spawning script added under the contract — the
#                       count is DERIVED at selftest time, never frozen)
#                       must exit 0 and must name each of them COMPLIANT — the
#                       regression guard that this gate never starts rejecting the
#                       demos it is supposed to protect
#   NOT-CLASSIFIED      scripts/bunker-matrix.sh (mentions `make run` / `./bin/crier`
#                       only inside usage text and a fatal message) and
#                       examples/demo.sh (spawns nothing) must be NOT-SPAWNING:
#                       a mention is never a spawn
#   REJECT-NO-TRAP      a fixture that backgrounds ./bin/crier with neither a trap
#                       nor an ownership assertion                 -> REJECT (1),
#                       naming the file, the spawn line and BOTH (a) and (b)
#   REJECT-TRAP-ONLY    the same fixture plus an EXIT trap that kills the pid
#                       (and nothing else)                          -> REJECT (1),
#                       naming (b) ONLY — a trap alone is not the contract
#   REJECT-BOUNDED-NO-OWNERSHIP
#                       a timeout-bounded spawn that binds a relay port and asserts
#                       no ownership                               -> REJECT (1),
#                       naming (b): the bounded-execution exception waives (a), it
#                       never waives (b)
#   ACCEPT-BOUNDED      a timeout-bounded spawn (no trap) WITH an ownership
#                       assertion                                  -> ACCEPT (0),
#                       printing the bounded-execution waiver as the reason
#   MENTION-ONLY        a fixture whose only crier references are a comment, an echo,
#                       a grep pattern and a fatal message         -> NOT-SPAWNING, and
#                       an explicit list of such scripts is refused (exit 1): the
#                       checker never prints a PASS over a list it enforced nothing on
#   SHAPES              one fixture carrying four executable spawn forms (a `bash -c`
#                       command string, a `nohup` wrapper, a bare `crier` on PATH, a
#                       backgrounded subshell) and four non-spawns (the binary's own
#                       `-version` and `-stop` flags, the non-listening `keygen`
#                       subcommand, a `go build -o …`): exactly the four must
#                       classify, the four must not
#   KEYGEN-ONLY         a fixture whose ONLY crier invocation is `crier keygen`
#                       (CR-FEAT-027)                       -> NOT-SPAWNING, 0 spawn
#                       sites, and an explicit list of it is still refused: the
#                       subcommand binds no port, so it owes neither a trap nor an
#                       ownership assertion. Before that rule it was REJECTED for
#                       missing (b) over a port it never binds (measured)
#   KEYGEN+NOSPAWN-CONTROL
#                       the same keygen call PLUS a backgrounded relay -> 1 spawn
#                       site, still REJECTED for (b): the new rule must not hide a
#                       real server on the same file
#   FAIL-CLOSED-MISSING a named path that does not exist            -> REJECT (1)
#   FAIL-CLOSED-NON-SHELL
#                       a named path that is not a shell script    -> REJECT (1)
#   FAIL-CLOSED-NO-REPO a DEFAULT run whose scope cannot be enumerated (no git repo
#                       at the root)                               -> exit 2, never a
#                       green over nothing
#   NEUTER PROOF        a copy of the checker whose verdict call is forced to success
#                       must ACCEPT the same no-trap fixture the real checker
#                       rejects — and the copy must DIFFER from the original, so the
#                       causality proof cannot go vacuous
#   MISSING COUNTERPART the checker itself missing on disk         -> exit 2
#
# The fixtures are never run: this is a STATIC text checker, so a fixture that
# would spawn a server is only ever read, never executed. No server is started, no
# port is touched, and no process of this host is signalled.
#
# EXIT CODES
#   0  every proof behaved as required
#   1  at least one proof did not behave
#   2  this selftest cannot run (the checker is missing, or git/mktemp unavailable)
#
# DEPENDENCIES: bash, git, mktemp, grep, cmp, sed, rm.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-demo-cleanup-selftest"
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
CHECKER="$SCRIPT_DIR/check-demo-cleanup.sh"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SELFTEST_TMP=""
FAILS=0
CHECKS=0
OUT=""
RC=0

_cleanup() { # EXIT trap for the scratch dir (global var only)
  if [ -n "$SELFTEST_TMP" ]; then
    rm -rf "$SELFTEST_TMP"
    SELFTEST_TMP=""
  fi
  return 0
}

_ok() {
  CHECKS=$((CHECKS + 1))
  printf 'PASS: %s\n' "$*"
}

_bad() {
  CHECKS=$((CHECKS + 1))
  FAILS=$((FAILS + 1))
  printf '%s selftest: FAIL: %s\n' "$PROG" "$*" >&2
}

# _verdict <fixture> <rc> <expected> — one line per proof, so a reader sees the
# exit code the checker produced and what was expected of it.
_verdict() { # <name> <rc> <want> <what>
  printf 'fixture %-28s rc=%-3s want=%-3s %s\n' "$1" "$2" "$3" "$4"
}

# _run <args...> — run the REAL checker, capture output + rc. The checker is a
# static analyzer: a fixture is read, never executed.
_run() {
  OUT="$(bash "$CHECKER" "$@" 2>&1)"
  RC=$?
}

# _has <needle> — did the last checker run print this?
_has() { printf '%s' "$OUT" | grep -qF -- "$1"; }

# _has_re <ere> — did the last checker run print a line matching this?
_has_re() { printf '%s' "$OUT" | grep -qE -- "$1"; }

main() {
  [ -r "$CHECKER" ] || {
    printf '%s: ERROR: %s is not readable — nothing to prove (exit 2)\n' "$PROG" "$CHECKER" >&2
    return 2
  }
  command -v git >/dev/null 2>&1 || {
    printf "%s: ERROR: 'git' is not on PATH - the tracked-tree proof cannot run (exit 2)\n" "$PROG" >&2
    return 2
  }

  SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-demo-cleanup-selftest.XXXXXX")" || {
    printf '%s: ERROR: mktemp -d failed\n' "$PROG" >&2
    return 2
  }
  trap '_cleanup' EXIT
  local tmp="$SELFTEST_TMP"

  # ── fixtures ────────────────────────────────────────────────────────────────
  # (1) the leak shape: backgrounded server, no trap, no ownership assertion.
  cat >"$tmp/fx-no-trap.sh" <<'FX'
#!/usr/bin/env bash
set -uo pipefail
PORT=18767
OUT="$(mktemp -d)"
CRIER_PORT="$PORT" ./bin/crier > "$OUT/server.log" 2>&1 &
SERVER_PID=$!
echo "server up"
FX

  # (2) a trap that reaps the server, and nothing else.
  cat >"$tmp/fx-trap-only.sh" <<'FX'
#!/usr/bin/env bash
set -uo pipefail
PORT=18767
OUT="$(mktemp -d)"
SERVER_PID=""
cleanup() { [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null; rm -rf "$OUT"; }
trap cleanup EXIT
CRIER_PORT="$PORT" "$WORKDIR/crier" > "$OUT/server.log" 2>&1 &
SERVER_PID=$!
FX

  # (3) bounded execution (timeout wraps the run) + an ownership assertion: the
  # exception rule's accepted shape — no trap is required.
  cat >"$tmp/fx-bounded.sh" <<'FX'
#!/usr/bin/env bash
set -uo pipefail
PORT=18768
OUT="$(mktemp -d)"
timeout 30 "$CRIER_BIN" -port "$PORT" > "$OUT/server.log" 2>&1 &
SERVER_PID=$!
assert_port_owned "$PORT" "$SERVER_PID" "crier"
FX

  # (4) bounded execution that STILL owes (b): it binds a relay port.
  cat >"$tmp/fx-bounded-no-ownership.sh" <<'FX'
#!/usr/bin/env bash
set -uo pipefail
PORT=18769
timeout 30 ./bin/crier -port "$PORT" >/dev/null 2>&1 &
FX

  # (5) mentions only — every reference is text, never a spawn.
  cat >"$tmp/fx-mention-only.sh" <<'FX'
#!/usr/bin/env bash
# start the server first: make run, or ./bin/crier
fatal() { echo "no server answering (start one first with make run)" >&2; exit 1; }
grep -q "make run" /tmp/nothing.err
echo "./bin/crier" > /dev/null
FX

  # (6) a non-shell path, for the fail-closed proof.
  printf '# notes\n' >"$tmp/notes.md"

  # ── 1. the tracked tree is compliant, and the named population says so ──────
  local tree_out="" tree_rc=0
  tree_out="$(cd "$REPO_ROOT" && bash "$CHECKER" 2>&1)"
  tree_rc=$?
  if [ "$tree_rc" -eq 0 ]; then
    _ok "TRACKED TREE: the real tree passes (rc=0) — the demos the gate protects are not rejected"
  else
    _bad "TRACKED TREE: the checker rejected its own tracked tree (rc=$tree_rc)
  output: $(printf '%s' "$tree_out" | tail -20)"
  fi
  local p="" missing_pop=""
  for p in scripts/e2e-battery.sh examples/federation-demo/run-demo.sh \
           examples/hermes-gateway-demo/run-demo.sh examples/llm-mesh/run-demo.sh \
           examples/llm-mesh/mesh/run-demo.sh examples/ws-mesh-demo/run-demo.sh \
           scripts/check-mcp-stdout.sh; do
    if printf '%s' "$tree_out" | grep -qE "^COMPLIANT +$p( |—)"; then
      :
    else
      missing_pop="$missing_pop $p"
    fi
  done
  if [ -z "$missing_pop" ]; then
    _ok "TRACKED TREE: e2e-battery.sh, all 5 example runners and check-mcp-stdout.sh classify COMPLIANT"
  else
    _bad "TRACKED TREE: these scripts did not classify COMPLIANT:$missing_pop"
  fi
  missing_pop=""
  for p in scripts/bunker-matrix.sh examples/demo.sh; do
    if printf '%s' "$tree_out" | grep -qE "^NOT-SPAWNING +$p( |—)"; then
      :
    else
      missing_pop="$missing_pop $p"
    fi
  done
  if [ -z "$missing_pop" ]; then
    _ok "NOT-CLASSIFIED: bunker-matrix.sh (mentions only) and examples/demo.sh (spawns nothing) are NOT server-spawning"
  else
    _bad "NOT-CLASSIFIED: these mention-only/non-spawning scripts were classified as spawners:$missing_pop"
  fi

  # ── 2. REJECT: backgrounded spawn, no trap, no ownership ────────────────────
  _run "$tmp/fx-no-trap.sh"
  _verdict "fx-no-trap.sh" "$RC" 1 "REJECT (both requirements missing)"
  printf '%s\n' "$OUT" | grep -E '^(REJECTED|  spawn site| {14}missing)' || true
  if [ "$RC" -eq 1 ] \
    && _has "$tmp/fx-no-trap.sh" \
    && _has_re "spawn site +$tmp/fx-no-trap\.sh:[0-9]+" \
    && _has "missing (a) EXIT-trap cleanup" \
    && _has "missing (b) ownership assertion"; then
    _ok "REJECT-NO-TRAP: rejected with the file, the spawn line (file:line) and BOTH (a) and (b) named"
  else
    _bad "REJECT-NO-TRAP: rc=$RC (want 1); the file, the spawn line and both missing requirements are not all named
  output: $OUT"
  fi

  # ── 3. REJECT: a trap alone is not the contract (b) is still owed ───────────
  _run "$tmp/fx-trap-only.sh"
  _verdict "fx-trap-only.sh" "$RC" 1 "REJECT ((b) only)"
  printf '%s\n' "$OUT" | grep -E '^(REJECTED|  spawn site| {14}missing)' || true
  if [ "$RC" -eq 1 ] && _has "missing (b) ownership assertion" \
    && ! _has "missing (a) EXIT-trap cleanup"; then
    _ok "REJECT-TRAP-ONLY: the EXIT trap satisfies (a), so only (b) is named — the requirements are reported separately"
  else
    _bad "REJECT-TRAP-ONLY: rc=$RC (want 1) naming (b) and NOT (a)
  output: $OUT"
  fi

  # ── 4. REJECT: bounded execution waives (a), never (b) ──────────────────────
  _run "$tmp/fx-bounded-no-ownership.sh"
  _verdict "fx-bounded-no-ownership.sh" "$RC" 1 "REJECT ((b) only)"
  printf '%s\n' "$OUT" | grep -E '^(REJECTED|  spawn site| {14}missing)' || true
  if [ "$RC" -eq 1 ] && _has "missing (b) ownership assertion" \
    && ! _has "missing (a) EXIT-trap cleanup"; then
    _ok "REJECT-BOUNDED-NO-OWNERSHIP: a port-binding bounded run still owes (b) — the exception is not a blanket waiver"
  else
    _bad "REJECT-BOUNDED-NO-OWNERSHIP: rc=$RC (want 1) naming (b) and NOT (a)
  output: $OUT"
  fi

  # ── 5. ACCEPT: bounded execution + ownership assertion ─────────────────────
  _run "$tmp/fx-bounded.sh"
  _verdict "fx-bounded.sh" "$RC" 0 "ACCEPT (bounded + ownership)"
  printf '%s\n' "$OUT" | grep -E '^(COMPLIANT|  spawn site)' || true
  if [ "$RC" -eq 0 ] && _has "COMPLIANT" && _has "bounded execution"; then
    _ok "ACCEPT-BOUNDED: a timeout-wrapped spawn (no trap) with an ownership assertion is accepted, and the waiver is printed"
  else
    _bad "ACCEPT-BOUNDED: rc=$RC (want 0) printing the bounded-execution waiver
  output: $OUT"
  fi

  # ── 6. mentions never classify, and an empty scope is never a PASS ─────────
  _run "$tmp/fx-mention-only.sh"
  _verdict "fx-mention-only.sh" "$RC" 1 "NOT-SPAWNING + vacuous-PASS refusal"
  if [ "$RC" -eq 1 ] && _has "NOT-SPAWNING" && _has "refusing a vacuous PASS"; then
    _ok "MENTION-ONLY: comments/echo/grep/fatal references do not classify, and an explicit list that enforced nothing is refused"
  else
    _bad "MENTION-ONLY: rc=$RC (want 1) with NOT-SPAWNING + the vacuous-PASS refusal
  output: $OUT"
  fi

  # ── 7. shape coverage: what classifies and what must not ───────────────────
  # Four executable spawn forms (a shell string, a wrapper, a bare binary on PATH,
  # a subshell) against three exclusions (the binary's own -version/-stop flags and
  # a `go build`). The count is asserted exactly, so a future parser change that
  # starts classifying a mention (or stops seeing a real spawn) reddens here.
  cat >"$tmp/fx-shapes.sh" <<'FX'
#!/usr/bin/env bash
set -uo pipefail
bash -c './bin/crier --port 9001 > /tmp/a.log 2>&1 &'
nohup "$BIN/crier" > /tmp/b.log 2>&1 &
crier --port 9003 &
( "$WORKDIR/crier" --port 9004 > /tmp/d.log 2>&1 ) &
./bin/crier -version
./bin/crier -stop -pidfile /tmp/x.pid
"$BIN/crier" keygen -out /tmp/fx.key -id fx
"$CRIER_BIN" keygen -out /tmp/fx2.key -id fx2 -json
go build -o bin/crier ./cmd/server
FX
  _run "$tmp/fx-shapes.sh"
  local shapes_n=0
  shapes_n="$(printf '%s\n' "$OUT" | grep -c '  spawn site' || true)"
  _verdict "fx-shapes.sh" "$RC" 1 "$shapes_n spawn site(s) (want 4), all backgrounded"
  if [ "$RC" -eq 1 ] && [ "$shapes_n" -eq 4 ] \
    && ! printf '%s\n' "$OUT" | grep '  spawn site' | grep -q '\-version' \
    && ! printf '%s\n' "$OUT" | grep '  spawn site' | grep -q 'keygen' \
    && ! printf '%s\n' "$OUT" | grep '  spawn site' | grep -q 'go build'; then
    _ok "SHAPES: a shell string / wrapper / bare-PATH / subshell spawn each classify (4), while -version, -stop, the non-listening keygen subcommand and go build do not"
  else
    _bad "SHAPES: rc=$RC (want 1) with exactly 4 spawn sites and no -version/keygen/go-build site; got $shapes_n site(s)
  output: $OUT"
  fi

  # ── 7b. a keygen-only script is NOT a spawn at all (CR-FEAT-027) ──────────
  # Regression: before the keygen rule, this fixture was classified as a relay
  # spawn and REJECTED for missing the (b) ownership assertion — for a port the
  # subcommand never binds. It must be NOT-SPAWNING, and an explicit list of it
  # must still be refused (the vacuous-PASS rule is unchanged).
  cat >"$tmp/fx-keygen-only.sh" <<'FX'
#!/usr/bin/env bash
set -euo pipefail
"$CRIER_BIN" keygen -out /tmp/fx-keygen-only.key -id fixture
echo done
FX
  _run "$tmp/fx-keygen-only.sh"
  local keygen_n=0
  keygen_n="$(printf '%s\n' "$OUT" | grep -c '  spawn site' || true)"
  _verdict "fx-keygen-only.sh" "$RC" 1 "NOT-SPAWNING (no spawn site), vacuous-PASS refusal"
  if [ "$RC" -eq 1 ] && [ "$keygen_n" -eq 0 ] && _has "NOT-SPAWNING"; then
    _ok "KEYGEN-ONLY: the keygen subcommand is not a server spawn (0 spawn sites) and owes no trap/ownership assertion"
  else
    _bad "KEYGEN-ONLY: rc=$RC (want 1), want 0 spawn sites and NOT-SPAWNING; got $keygen_n site(s)
  output: $OUT"
  fi

  # And the rule must NOT suppress a real spawn on a line that also names keygen:
  # the same fixture plus a backgrounded relay must still classify and still owe
  # the ownership assertion.
  cat >"$tmp/fx-keygen-plus-spawn.sh" <<'FX'
#!/usr/bin/env bash
set -euo pipefail
"$CRIER_BIN" keygen -out /tmp/fx-k.key -id fixture
CRIER_PORT=9005 "$CRIER_BIN" > /tmp/fx-k.log 2>&1 &
SERVER_PID=$!
FX
  _run "$tmp/fx-keygen-plus-spawn.sh"
  local mixed_n=0
  mixed_n="$(printf '%s\n' "$OUT" | grep -c '  spawn site' || true)"
  _verdict "fx-keygen-plus-spawn.sh" "$RC" 1 "1 spawn site (the relay), REJECT for (a)+(b)"
  if [ "$RC" -eq 1 ] && [ "$mixed_n" -eq 1 ] && _has "missing (b) ownership assertion"; then
    _ok "KEYGEN+NOSPAWN-CONTROL: the keygen rule does not hide a backgrounded relay on the same file (1 site, still REJECTED)"
  else
    _bad "KEYGEN+NOSPAWN-CONTROL: rc=$RC (want 1) with 1 spawn site and a (b) rejection; got $mixed_n site(s)
  output: $OUT"
  fi

  # ── 8. fail closed on a named path that does not exist ─────────────────────
  _run "$tmp/does-not-exist.sh"
  _verdict "does-not-exist.sh" "$RC" 1 "REJECT (fail closed)"
  if [ "$RC" -eq 1 ] && _has "$tmp/does-not-exist.sh"; then
    _ok "FAIL-CLOSED-MISSING: a named path that does not exist is rejected with the path named"
  else
    _bad "FAIL-CLOSED-MISSING: rc=$RC (want 1) naming the missing path
  output: $OUT"
  fi

  # ── 9. fail closed on a named path that is not a shell script ──────────────
  _run "$tmp/notes.md"
  _verdict "notes.md" "$RC" 1 "REJECT (fail closed)"
  if [ "$RC" -eq 1 ] && _has "not a shell script"; then
    _ok "FAIL-CLOSED-NON-SHELL: a named non-shell path is rejected, not silently skipped"
  else
    _bad "FAIL-CLOSED-NON-SHELL: rc=$RC (want 1) naming the non-shell path
  output: $OUT"
  fi

  # ── 10. fail closed when the default scope cannot be enumerated ─────────────
  local nogit="$tmp/no-repo"
  mkdir -p "$nogit"
  OUT="$(cd "$nogit" && DEMO_CLEANUP_ROOT="$nogit" bash "$CHECKER" 2>&1)"
  RC=$?
  _verdict "no-repo (default scope)" "$RC" 2 "exit 2, nothing verified"
  if [ "$RC" -eq 2 ]; then
    _ok "FAIL-CLOSED-NO-REPO: a default run that cannot enumerate a scope exits 2 (never a green over nothing)"
  else
    _bad "FAIL-CLOSED-NO-REPO: rc=$RC (want 2)
  output: $OUT"
  fi

  # ── 11. NEUTER PROOF: the rejection is the checker's verdict ───────────────
  # Force the verdict call to success in a COPY; the same fixture the real checker
  # rejects must then be accepted, and the copy must differ from the original.
  local neutered="$tmp/neutered-checker.sh"
  cp "$CHECKER" "$neutered"
  sed -i.tmp -e 's|^\([[:space:]]*\)if ! _check_script "\$f"; then$|\1if false; then|' "$neutered"
  rm -f "$neutered.tmp"
  if cmp -s "$CHECKER" "$neutered"; then
    _bad "NEUTER PROOF: the neuter sed no longer matches the verdict call (NEUTER-MARK[verdict]) — the causality proof would be vacuous"
  else
    OUT="$(bash "$neutered" "$tmp/fx-no-trap.sh" 2>&1)"
    RC=$?
    _verdict "neutered checker (no-trap fx)" "$RC" 0 "ACCEPT (verdict disabled)"
    # the copy must still CLASSIFY the fixture (1 server-spawning): if disabling the
    # verdict also erased the classification, rc=0 would come from the
    # nothing-was-enforced rule instead of the neutered verdict — a vacuous proof.
    if [ "$RC" -eq 0 ] && _has "1 server-spawning (1 compliant, 0 rejected)"; then
      _ok "NEUTER PROOF: the neutered copy differs from the original, still classifies the fixture and ACCEPTS the same fixture the real checker rejects (rc=1) — the rejection is caused by the verdict"
    else
      _bad "NEUTER PROOF: with the verdict neutered the same no-trap fixture is STILL rejected (rc=$RC) or is no longer classified as server-spawning — the rejection does not come from that verdict
  output: $OUT"
    fi
  fi

  # ── 12. cwd independence: the default scope means the TRACKED tree ──────────
  # A run from another directory must check the same files and reach the same
  # verdict; resolving the tracked paths against the caller's cwd instead of the
  # checkout failed every file (measured from /tmp before the fix).
  local other_out="" other_rc=0
  other_out="$(cd "${TMPDIR:-/tmp}" && bash "$CHECKER" 2>&1)"
  other_rc=$?
  _verdict "default scope from ${TMPDIR:-/tmp}" "$other_rc" 0 "same verdict as the tree"
  # the expected population is DERIVED from the tree run, never frozen — a new
  # compliant server-spawning script (e.g. the tick-393 additions) must not
  # break this check, and a population DROP must still fail it.
  local expected_pop=""
  expected_pop="$(printf '%s' "$tree_out" | grep -oE '[0-9]+ server-spawning \([0-9]+ compliant, 0 rejected\)' | head -1)"
  if [ "$other_rc" -eq 0 ] \
    && [ -n "$expected_pop" ] \
    && printf '%s' "$other_out" | grep -qF "$expected_pop"; then
    _ok "CWD-INDEPENDENT: the default run reaches the same scope and verdict from another directory ($expected_pop)"
  else
    _bad "CWD-INDEPENDENT: rc=$other_rc (want 0, same population as the tree: ${expected_pop:-unparsed}) when run from ${TMPDIR:-/tmp}
  output: $(printf '%s' "$other_out" | tail -3)"
  fi

  # ── 13. the script itself must still parse ────────────────────────────────
  local syntax_rc=0
  bash -n "$CHECKER" || syntax_rc=$?
  if [ "$syntax_rc" -eq 0 ]; then
    _ok "CHECKER-SYNTAX: bash -n accepts the checker"
  else
    _bad "CHECKER-SYNTAX: bash -n rejected the checker (rc=$syntax_rc)"
  fi

  printf '\n'
  if [ "$FAILS" -gt 0 ]; then
    printf '%s: %d/%d checks behaved — FAIL\n' "$PROG" "$((CHECKS - FAILS))" "$CHECKS" >&2
    return 1
  fi
  printf '%s: %d/%d checks behaved — PASS\n' "$PROG" "$CHECKS" "$CHECKS"
  return 0
}

main "$@"
