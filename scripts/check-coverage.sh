#!/usr/bin/env bash
#
# scripts/check-coverage.sh — the engine behind `make coverage-check`
# (QA-CRIER-34). The old Makefile recipe was NOT fail-closed, and both
# failure modes were proven live on this tree before the fix:
#
#   RED (failing suite): with a PATH shim making `go test` exit 1, the
#   recipe ignored the status, `go tool cover` found no coverage.out, bc
#   choked on the empty COVERAGE ("(standard_in) 1: syntax error") and the
#   target printed  PASS: Coverage % meets 70% threshold  and exited 0 —
#   a red build smuggled through the coverage gate.
#
#   RED (stale data): with a coverage.out whose total is 99.9% left on
#   disk, the same failing suite still printed  PASS: Coverage 99.9% meets
#   70% threshold  — the gate graded the PREVIOUS tree.
#
# This script grades only what IT measured, in THIS run:
#   1. it runs the -short suite through the coverage build and requires
#      exit 0 (stale coverage.out removed first; a failure names the
#      package and prints go test's own output);
#   2. it refuses to grade a missing/unreadable coverage.out (exit 3);
#   3. it refuses an empty/unparseable total line (exit 3) — the COVERAGE
#      value must be a positive percentage number, so "PASS over missing
#      data" is unreachable;
#   4. it compares the number against the 70.0 threshold.
#
# Every run prints the resolved tool identities, the exact command run,
# and the parsed total, so the verdict is always attributable to evidence.
#
# CONFIG
#   COVERAGE_THRESHOLD  pass mark (default 70.0)
#   GO                  go binary to use (default: `go` on PATH)
#   CHECK_COVERAGE_KEEP_GO_OUTPUT=1  do not discard `go test`'s output
#
# EXIT CODES
#   0  suite green and coverage >= threshold
#   1  suite green but coverage BELOW threshold
#   2  the suite FAILED (refusal; go test's output is printed)
#   3  missing/unreadable coverage data or an unparseable total (refusal)
#   4  misuse (unknown option / bad COVERAGE_THRESHOLD / missing `go`)
#
# OUTPUT CONTRACT (grep-able)
#   coverage: using <go path> (<go version>)                    tool identity
#   coverage: cmd: <the exact suite command>                    evidence line
#   coverage: total <N.NN%> (threshold 70.0)                    parsed data
#   PASS  coverage <N.NN%> >= <threshold>%                      green
#   FAIL  coverage <N.NN%> < <threshold>%                       red
#   REFUSED  coverage-check: <reason>                           every refusal
#   A refusal NEVER prints a PASS line.
#
# SELFTEST — `bash scripts/check-coverage.sh --selftest` (wired as
# `make coverage-selftest`). It never needs the repo's own suite to be
# red or its real coverage to move: every arm runs THIS script against a
# scratch Go module and a PATH shim directory, so the arms are
# deterministic and the repo tree is untouched:
#   1. a GREEN suite over a module whose total is above the threshold is
#      accepted (rc 0), with the parsed total and the tool identity
#      printed;
#   2. a green suite BELOW the threshold is FAIL + exit 1;
#   3. a FAILING suite is REFUSED (rc 2) with the failing package named
#      and NO PASS line — the vacuous-PASS defect is dead;
#   4. a failing suite over a STALE high-coverage profile is still
#      refused (rc 2) — stale data can no longer buy a PASS;
#   5. a green suite that leaves NO coverage.out is refused (rc 3);
#   6. a green suite leaving an UNPARSEABLE coverage.out is refused
#      (rc 3);
#   7. a green suite whose total is EMPTY is refused (rc 3);
#   8. go hidden from PATH is exit 4 naming the tool (fail closed, never
#      a silent skip);
#   9. an env-poisoned empty COVERAGE value cannot reach the verdict:
#      with a profile planted in the environment the checker still
#      measures its own suite and refuses on the missing data (rc 3,
#      same as arm 5);
#  10. a NEUTER proof: a copy of this script whose suite-status check is
#      forced to accept must FAIL arm 3's fixture (print PASS over the
#      failing suite) — proving refusals 3/4 are caused by the status
#      check and not by accident;
#  11. a second NEUTER proof: a copy whose data-validation call is
#      forced to succeed must print a PASS over an EMPTY total — proving
#      the empty-data refusal is caused by that validation;
#  12. a clean scratch module suite passes at ANY threshold the caller
#      sets (COVERAGE_THRESHOLD is honoured, 100 refuses at 101).
#   `make coverage-selftest` exits 0 only when all of them behaved.
#
# DEPENDENCIES: bash, go (real suites), awk; the selftest additionally
# uses sed/cmp/mktemp and a scratch `go` module.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="coverage"
THRESHOLD="${COVERAGE_THRESHOLD:-70.0}"

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_refuse() { printf 'REFUSED  coverage-check: %s\n' "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

usage() {
  cat <<'EOF'
check-coverage.sh — fail-closed coverage gate engine (QA-CRIER-34)

Usage:
  bash scripts/check-coverage.sh [--selftest]

Runs the -short suite with -coverprofile into coverage.out, requires the
suite to exit 0, parses the `total:` line, refuses missing/unreadable/
unparseable coverage data, and compares the number against
COVERAGE_THRESHOLD (default 70.0).

Exit codes: 0 pass, 1 below threshold, 2 suite failed (refused),
3 missing/unparseable coverage data (refused), 4 misuse/missing go.
A refusal never prints a PASS line.
EOF
}

# ── validator resolution ──────────────────────────────────────────────────────
GO_BIN=""

resolve_go() {
  local cand="${GO:-go}"
  command -v "$cand" >/dev/null 2>&1 || {
    _refuse "go is not on PATH ('$cand') — refusing to skip the coverage gate."
    return 4
  }
  GO_BIN="$(command -v "$cand")"
  _info "using $GO_BIN ($("$GO_BIN" version 2>/dev/null || echo 'version unknown'))"
  return 0
}

# ── the check ─────────────────────────────────────────────────────────────────

# _data_ok <value> — the coverage-total data-validation: nonempty and a
# bare number (integer or decimal). One verdict helper, so the selftest's
# neuter proof can force it in one line.
_data_ok() {
  local v="$1"
  [ -n "$v" ] || return 1
  printf '%s' "$v" | grep -Eq '^[0-9]+([.][0-9]+)?$'
}

run_check() {
  local prof="coverage.out"
  local test_rc=0 total_line="" coverage="" diag=""

  # The gate may only ever grade a profile from THIS run: a profile left
  # behind by an earlier tree must never be readable when the current
  # suite fails to produce one (the stale-data RED arm).
  rm -f "$prof" 2>/dev/null || true

  resolve_go || return $?

  # NEUTER-MARK[suite-status]: the suite verdict call. The selftest seds
  # exactly this line (and asserts the copy changed) to prove refusals
  # 3/4 are caused by THIS status check and not by accident.
  local cmd=("$GO_BIN" test -short -count=1 -coverprofile="$prof" ./...)
  _info "cmd: ${cmd[*]}"
  if [ "${CHECK_COVERAGE_KEEP_GO_OUTPUT:-0}" = "1" ]; then
    "${cmd[@]}"
  else
    diag="$("${cmd[@]}" 2>&1)"
  fi
  test_rc=$?
  if [ "$test_rc" -ne 0 ]; then
    _refuse "the test suite FAILED (exit $test_rc) — no coverage verdict can be trusted from a red run; refusing to grade stale or missing data."
    if [ -n "$diag" ]; then
      # Name the failing package(s) — go test marks them "FAIL\tpkg".
      local failing
      failing="$(printf '%s\n' "$diag" | grep '^FAIL' | head -5)"
      if [ -n "$failing" ]; then
        printf '%s\n' "$failing" | sed 's/^/coverage: FAILING: /' >&2
      fi
      printf '%s\n' "$diag" | head -30 | sed 's/^/coverage: | /' >&2
      local n
      n="$(printf '%s\n' "$diag" | wc -l | tr -d ' ')"
      [ "${n:-0}" -gt 30 ] && _info "(go test output truncated to the first 30 of $n line(s); re-run with CHECK_COVERAGE_KEEP_GO_OUTPUT=1 for the full log)"
    else
      _info "(go test's output above — CHECK_COVERAGE_KEEP_GO_OUTPUT=1 keeps it verbatim)"
    fi
    return 2
  fi
  _info "suite: PASS (exit 0) — grading the profile from this run"

  if [ ! -s "$prof" ]; then
    _refuse "coverage profile '$prof' is missing or empty after a GREEN suite — refusing to invent a verdict over missing data (QA-CRIER-34)."
    return 3
  fi
  if [ ! -r "$prof" ]; then
    _refuse "coverage profile '$prof' is not readable — refusing to invent a verdict over unreadable data."
    return 3
  fi

  # NEUTER-MARK[data-parse]: the data-validation call. The selftest seds
  # exactly this line to prove the empty/unparseable-data refusal is
  # caused by THIS validation and not by accident.
  total_line="$("$GO_BIN" tool cover -func="$prof" 2>/dev/null | grep '^total:' | awk '{print $3}' | sed 's/%$//')"
  if ! _data_ok "$total_line"; then
    _refuse "could not parse the total coverage from '$prof' (got '${total_line:-<empty>}') — refusing to print a PASS over missing data (QA-CRIER-34)."
    return 3
  fi
  coverage="$total_line"
  _info "total ${coverage}% (threshold ${THRESHOLD})"

  if awk -v c="$coverage" -v t="$THRESHOLD" 'BEGIN { exit !(c + 0 >= t + 0) }'; then
    printf 'PASS  coverage %s%% >= %s%%\n' "$coverage" "$THRESHOLD"
    return 0
  fi
  printf 'FAIL  coverage %s%% < %s%%\n' "$coverage" "$THRESHOLD"
  _err "coverage ${coverage}% is below the ${THRESHOLD}% threshold"
  return 1
}

# ── selftest ──────────────────────────────────────────────────────────────────

_selftest_cleanup() { # EXIT trap for the selftest scratch dir (global var only)
  if [ -n "${_SELFTEST_TMP:-}" ]; then
    rm -rf "$_SELFTEST_TMP"
    _SELFTEST_TMP=""
  fi
  return 0
}

# _mk_module <dir> <total_hint> <fail> — a tiny scratch Go module whose
# covered package's total coverage is steered by <total_hint>
# (above|below) and whose suite FAILS when <fail>=1.
_mk_module() {
  local dir="$1" hint="$2" fail="$3"
  mkdir -p "$dir/p"
  cat >"$dir/go.mod" <<EOF
module selftest.example/qa34

go 1.24
EOF
  if [ "$hint" = "above" ]; then
    cat >"$dir/p/p.go" <<'GO'
package p

func F(x int) int { return x * 2 }

func G(x int) int { return x + 1 }
GO
    cat >"$dir/p/p_test.go" <<'GO'
package p

import "testing"

func TestF(t *testing.T) {
	if F(2) != 4 {
		t.Fatal("F(2) != 4")
	}
	if G(2) != 3 {
		t.Fatal("G(2) != 3")
	}
}
GO
  else
    cat >"$dir/p/p.go" <<'GO'
package p

func F(x int) int { return x * 2 }

func G(x int) int { return x + 1 }

func H(x int) int { return x - 1 }

func I(x int) int { return x * 3 }

func J(x int) int { return x * 5 }

func K(x int) int { return x * 7 }
GO
    cat >"$dir/p/p_test.go" <<'GO'
package p

import "testing"

func TestF(t *testing.T) {
	if F(2) != 4 {
		t.Fatal("F(2) != 4")
	}
}
GO
  fi
  if [ "$fail" = "1" ]; then
    cat >"$dir/p/broken_test.go" <<'GO'
package p

import "testing"

func TestBroken(t *testing.T) {
	t.Fatal("deliberate selftest failure")
}
GO
  fi
}

# _mk_shim <dir> <mode> — a PATH shim directory. mode=hidego: symlinks to
# every tool the script needs EXCEPT go (so the check proves the
# script's own resolution path, not a broken environment). mode=stale:
# go shim that runs the real go but, on `test`, first plants a stale
# high-coverage profile; mode=emptyout / badout / noout: go shims that
# run the real suite and then overwrite / corrupt / delete the profile.
_mk_shim() {
  local dir="$1" mode="$2" b p realgo
  mkdir -p "$dir"
  realgo="$(command -v go)"
  for b in sh bash env awk grep sed head printf mkdir rm cat wc cmp mktemp dirname tr uname cut sort; do
    p="$(command -v "$b" 2>/dev/null)" && ln -sf "$p" "$dir/$b"
  done
  case "$mode" in
    hidego) ;;
    stale)
      cat >"$dir/go" <<EOF
#!/bin/sh
if [ "\$1" = "test" ]; then
  shift 0
  # plant a STALE profile before running the (failing) suite
  printf 'mode: set\n' > coverage.out
  printf 'github.com/crier-dev/crier/internal/old/file.go:1.0,2.0 1 1\n' >> coverage.out
fi
exec $realgo "\$@"
EOF
      ;;
    emptyout)
      cat >"$dir/go" <<EOF
#!/bin/sh
$realgo "\$@"
rc=\$?
[ \$rc -ne 0 ] && exit \$rc
: > coverage.out
exit 0
EOF
      ;;
    badout)
      cat >"$dir/go" <<EOF
#!/bin/sh
$realgo "\$@"
rc=\$?
[ \$rc -ne 0 ] && exit \$rc
printf 'this is not a coverage profile\n' > coverage.out
exit 0
EOF
      ;;
    noout)
      cat >"$dir/go" <<EOF
#!/bin/sh
$realgo "\$@"
rc=\$?
[ \$rc -ne 0 ] && exit \$rc
rm -f coverage.out
exit 0
EOF
      ;;
  esac
  chmod +x "$dir"/go 2>/dev/null || true
}

_selftest() {
  local fails=0 checks=0
  _SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-coverage-selftest.XXXXXX")" || {
    _err "selftest: mktemp -d failed"
    return 1
  }
  local tmp="$_SELFTEST_TMP"
  trap '_selftest_cleanup' EXIT

  # scratch modules
  _mk_module "$tmp/mod-pass" above 0
  _mk_module "$tmp/mod-low" below 0
  _mk_module "$tmp/mod-fail" above 1

  # shim dirs
  _mk_shim "$tmp/shim-stale" stale
  _mk_shim "$tmp/shim-emptyout" emptyout
  _mk_shim "$tmp/shim-badout" badout
  _mk_shim "$tmp/shim-noout" noout
  _mk_shim "$tmp/shim-hidego" hidego

  local out="" rc=0

  # 1. a GREEN suite above the threshold is ACCEPTED with real evidence
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-pass" && bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: the above-threshold module was REJECTED (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q '^PASS  coverage [0-9]'; then
    printf '%s selftest: FAIL: the green run printed no PASS line with a numeric total\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'coverage: total '; then
    printf '%s selftest: FAIL: the green run did not print the parsed total\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'coverage: using '; then
    printf '%s selftest: FAIL: the green run did not name the go tool it used\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a green suite above the threshold is accepted (rc=0) with the total and tool identity printed\n'
  fi

  # 2. a green suite BELOW the threshold is FAIL + exit 1
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-low" && COVERAGE_THRESHOLD=70.0 bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 1 ]; then
    printf '%s selftest: FAIL: the below-threshold module exited %d, not 1\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q '^FAIL  coverage '; then
    printf '%s selftest: FAIL: the below-threshold run printed no FAIL line\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a green suite below the threshold fails with exit 1 and a FAIL line\n'
  fi

  # 3. a FAILING suite is REFUSED with the package named and NO PASS
  #    (the vacuous-PASS defect, dead)
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-fail" && bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: a failing suite was ACCEPTED (rc=0) — the vacuous PASS is back\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif [ "$rc" -ne 2 ]; then
    printf '%s selftest: FAIL: a failing suite exited %d, not the documented refusal 2\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q '^REFUSED  coverage-check:'; then
    printf '%s selftest: FAIL: the failing suite produced no REFUSED line\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'FAILING: .*selftest.example/qa34/p'; then
    printf '%s selftest: FAIL: the refusal did not name the failing package\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q 'PASS'; then
    printf '%s selftest: FAIL: the failing suite produced a PASS line anyway\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a failing suite is refused (rc=2) with the package named and no PASS printed\n'
  fi

  # 4. STALE DATA: a failing suite over a planted high-coverage profile
  #    is still refused — stale data can no longer buy a PASS
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-fail" && env PATH="$tmp/shim-stale:$PATH" bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: a failing suite over a STALE 100%% profile was ACCEPTED (rc=0)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif [ "$rc" -ne 2 ]; then
    printf '%s selftest: FAIL: the stale-data arm exited %d, not 2\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q '^PASS'; then
    printf '%s selftest: FAIL: the stale-data arm printed a PASS over stale data\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a failing suite over a stale high-coverage profile is still refused (rc=2) — no PASS over stale data\n'
  fi

  # 5. a green suite leaving NO profile is refused (rc 3)
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-pass" && env PATH="$tmp/shim-noout:$PATH" bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 3 ]; then
    printf '%s selftest: FAIL: the missing-profile arm exited %d, not 3\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'missing or empty'; then
    printf '%s selftest: FAIL: the missing-profile refusal did not say why\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q '^PASS'; then
    printf '%s selftest: FAIL: the missing-profile arm printed a PASS over missing data\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a green suite that leaves no coverage profile is refused (rc=3), never a PASS over missing data\n'
  fi

  # 6. an UNPARSEABLE profile is refused (rc 3)
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-pass" && env PATH="$tmp/shim-badout:$PATH" bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 3 ]; then
    printf '%s selftest: FAIL: the unparseable-profile arm exited %d, not 3\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'could not parse the total coverage'; then
    printf '%s selftest: FAIL: the unparseable-profile refusal did not say why\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: an unparseable coverage profile is refused (rc=3) — no PASS over garbage data\n'
  fi

  # 7. an EMPTY profile after a green suite is refused (rc 3)
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-pass" && env PATH="$tmp/shim-emptyout:$PATH" bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 3 ]; then
    printf '%s selftest: FAIL: the empty-profile arm exited %d, not 3\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'missing or empty'; then
    printf '%s selftest: FAIL: the empty-profile refusal did not say why\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: an empty coverage profile after a green suite is refused (rc=3)\n'
  fi

  # 8. go hidden from PATH -> exit 4 naming the tool (fail closed)
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-pass" && env PATH="$tmp/shim-hidego" bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 4 ]; then
    printf '%s selftest: FAIL: with go hidden the run exited %d, not 4 (fail-closed)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "go is not on PATH"; then
    printf '%s selftest: FAIL: with go hidden the error did not name the missing tool\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q '^PASS'; then
    printf '%s selftest: FAIL: with go hidden the run printed a PASS anyway\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: with go hidden the run exits 4 and names the missing tool — never a silent skip (rc=%d)\n' "$rc"
  fi

  # 9. env-poisoned empty COVERAGE value: the checker measures its own
  #    data and refuses — the verdict never reads an inherited variable
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-pass" && env PATH="$tmp/shim-noout:$PATH" COVERAGE= CHECK_COVERAGE_KEEP_GO_OUTPUT=1 bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 3 ]; then
    printf '%s selftest: FAIL: the env-poisoned arm exited %d, not 3\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q '^PASS'; then
    printf '%s selftest: FAIL: the env-poisoned arm printed a PASS over missing data\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: an env-poisoned empty COVERAGE value cannot reach the verdict — own measurement refused (rc=3)\n'
  fi

  # 10. NEUTER PROOF (suite status): a copy whose status check is forced
  #     to accept must print a PASS over the SAME failing-suite fixture —
  #     proving refusals 3/4 are caused by the status check.
  checks=$((checks + 1))
  local neutered="$tmp/neutered-suite-status.sh"
  cp "$SELF" "$neutered"
  sed -i.tmp -e 's|if \[ "\$test_rc" -ne 0 \]; then|if false \&\& [ "$test_rc" -ne 0 ]; then|' "$neutered"
  rm -f "$neutered.tmp"
  if cmp -s "$SELF" "$neutered"; then
    printf '%s selftest: FAIL: the neuter sed no longer matches the suite-status check (NEUTER-MARK[suite-status]) — the causality proof would be vacuous\n' "$PROG" >&2
    fails=$((fails + 1))
  else
    out="$(cd "$tmp/mod-fail" && env CHECK_COVERAGE_KEEP_GO_OUTPUT=1 bash "$neutered" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q '^PASS  coverage'; then
      printf 'PASS: NEUTER PROOF: the neutered copy differs from the original and prints a PASS over the same failing suite — the refusal is caused by the suite-status check\n'
    else
      printf '%s selftest: FAIL: NEUTER PROOF: the neutered status check did not print a PASS over the failing suite (rc=%d) — the causality proof does not implicate the status arm\n  output: %s\n' "$PROG" "$rc" "$out" >&2
      fails=$((fails + 1))
    fi
  fi

  # 11. NEUTER PROOF (data validation): a copy whose data-validation call
  #     is forced to succeed must print a PASS over the garbage profile —
  #     proving the bad-data refusal is caused by that validation. The
  #     garbage profile parses to an EMPTY total, so the neutered copy's
  #     awk comparison would also refuse (empty < 0 is false); the copy
  #     is given THRESHOLD=0 via COVERAGE_THRESHOLD so the only remaining
  #     refusal can be the data validation itself.
  checks=$((checks + 1))
  local neutered2="$tmp/neutered-data-parse.sh"
  cp "$SELF" "$neutered2"
  sed -i.tmp -e 's|if ! _data_ok "\$total_line"; then|if false; then|' "$neutered2"
  rm -f "$neutered2.tmp"
  if cmp -s "$SELF" "$neutered2"; then
    printf '%s selftest: FAIL: the neuter sed no longer matches the data-validation call (NEUTER-MARK[data-parse]) — the causality proof would be vacuous\n' "$PROG" >&2
    fails=$((fails + 1))
  else
    out="$(cd "$tmp/mod-pass" && env PATH="$tmp/shim-badout:$PATH" COVERAGE_THRESHOLD=0 bash "$neutered2" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ] && printf '%s' "$out" | grep -q '^PASS  coverage'; then
      printf 'PASS: NEUTER PROOF: the data-parse-neutered copy prints a PASS over the garbage profile — the bad-data refusal is caused by that validation\n'
    else
      printf '%s selftest: FAIL: NEUTER PROOF: the data-parse-neutered copy did not print a PASS over the garbage profile (rc=%d) — the causality proof does not implicate the validation\n  output: %s\n' "$PROG" "$rc" "$out" >&2
      fails=$((fails + 1))
    fi
  fi

  # 12. COVERAGE_THRESHOLD is honoured: a 100%-coverage module refuses at
  #     a 101 threshold (exit 1) and passes at 99 (exit 0)
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-pass" && COVERAGE_THRESHOLD=101 bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 1 ]; then
    printf '%s selftest: FAIL: COVERAGE_THRESHOLD=101 did not fail the 100%% module (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  else
    out="$(cd "$tmp/mod-pass" && COVERAGE_THRESHOLD=99 bash "$SELF" 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      printf '%s selftest: FAIL: COVERAGE_THRESHOLD=99 did not pass the 100%% module (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
      fails=$((fails + 1))
    else
      printf 'PASS: COVERAGE_THRESHOLD is honoured (101 refuses a 100%% module, 99 accepts it)\n'
    fi
  fi

  if [ "$fails" -ne 0 ]; then
    printf '%s selftest: %d/%d checks behaved — FAIL\n' "$PROG" "$((checks - fails))" "$checks" >&2
    return 1
  fi
  printf '%s selftest: %d/%d checks behaved\n' "$PROG" "$checks" "$checks"
  return 0
}

# ── entry point ───────────────────────────────────────────────────────────────

main() {
  local selftest=0
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --selftest)
        selftest=1
        shift
        ;;
      -h | --help | help)
        usage
        return 0
        ;;
      *)
        _err "unknown option '$1' (try --help)"
        return 4
        ;;
    esac
  done

  case "$THRESHOLD" in
    '' | *[!0-9.]* | *[!0-9]*.*[!0-9]*)
      # allowed shapes: bare integer or a decimal number; anything else is misuse
      case "$THRESHOLD" in
        '' | *[!0-9.]*) _err "COVERAGE_THRESHOLD must be a number (got '$THRESHOLD')"; return 4 ;;
      esac
      ;;
  esac
  case "$THRESHOLD" in
    *..* | .* | *.) _err "COVERAGE_THRESHOLD must be a number (got '$THRESHOLD')"; return 4 ;;
  esac

  if [ "$selftest" -eq 1 ]; then
    _selftest
    return $?
  fi

  run_check
}

main "$@"
exit $?
