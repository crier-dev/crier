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
# RELEASE-027: a coverage red used to be UNATTRIBUTABLE. The red-run
# branch kept `go test`'s output in a variable and printed a fixed
# `head -30` window of it, so when the Coverage CI job failed the log
# named no package, no test and no percentage (run 43303623612, step
# "Check coverage threshold"): the failing test sat below the window and
# the number was never printed at all. A gate nobody can triage is not a
# gate. The suite output is now ECHOED IN FULL and RETAINED in
# $COVERAGE_LOG (gitignored); the red-run branch extracts the failing
# package import paths and failing test names FROM that log and prints
# them with the total percentage measured from the failed run's profile,
# so any red names itself no matter where in the output it sits.
#
# CONFIG
#   COVERAGE_THRESHOLD  pass mark (default 70.0)
#   GO                  go binary to use (default: `go` on PATH)
#   COVERAGE_LOG        where the FULL `go test` output is retained
#                       (default .coverage-test-output.log, CWD-relative;
#                       gitignored, so it never dirties `git status`)
#   CHECK_COVERAGE_KEEP_GO_OUTPUT  accepted for compatibility with older
#                       invocations; the suite output is ALWAYS retained in
#                       COVERAGE_LOG and always echoed, so nothing is
#                       discarded or truncated any more.
#
# EXIT CODES
#   0  suite green and coverage >= threshold
#   1  suite green but coverage BELOW threshold
#   2  the suite FAILED (refusal; failing packages/tests + total printed)
#   3  missing/unreadable coverage data or an unparseable total (refusal)
#   4  misuse (unknown option / bad COVERAGE_THRESHOLD / missing `go` /
#      unwritable suite log)
#
# OUTPUT CONTRACT (grep-able)
#   coverage: using <go path> (<go version>)                    tool identity
#   coverage: cmd: <the exact suite command>                    evidence line
#   coverage: log: <abs path> (<n> line(s) retained)            retention path
#   coverage: FAILING: package <import path>                    red attribution
#   coverage: FAILING: test <TestName>                          red attribution
#   coverage: failed-run total <N.N> percent (threshold <T> percent)
#   coverage: total <N.NN%> (threshold <T>)                     parsed data
#   coverage: <N.N> percent (threshold <T> percent)             green summary
#   PASS  coverage <N.NN%> >= <threshold>%                      green
#   FAIL  coverage <N.NN%> < <threshold>%                       red
#   REFUSED  coverage-check: <reason>                           every refusal
#   A refusal NEVER prints a PASS line.
#   NOTHING is truncated to a fixed first/last-N window: the suite's own
#   output is echoed verbatim and the attribution above is read from the
#   retained log.
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
#      sets (COVERAGE_THRESHOLD is honoured, 100 refuses at 101);
#  13. a FAILING suite prints — on STDOUT — the failing package import
#      path, the failing TEST NAME, the total measured from the failed
#      run beside the threshold, and the retention path of the full log;
#  14. a green run prints the canonical summary line
#      `coverage: <N.N> percent (threshold <T> percent)` carrying BOTH
#      numbers (the line CI and operators parse);
#  15. NO WINDOW TRUNCATION: a fixture whose failing package and test sit
#      BELOW 40+ lines of suite output is still named in full, and the
#      retained log holds every one of those lines — a fixed head/tail
#      window would have dropped exactly the failure;
#  16. the retention path is honoured and written on a green run too, so
#      the operator can always re-read what the gate measured.
#   `make coverage-selftest` exits 0 only when all of them behaved.
#
# DEPENDENCIES: bash, go (real suites), awk; the selftest additionally
# uses sed/cmp/mktemp and a scratch `go` module.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="coverage"
THRESHOLD="${COVERAGE_THRESHOLD:-70.0}"
# RELEASE-027: the FULL `go test` output is retained here (gitignored).
LOG="${COVERAGE_LOG:-.coverage-test-output.log}"
# "check" applies the threshold verdict; "report" (make coverage) only
# measures and prints — two verbs, one engine, so they cannot disagree.
MODE="check"

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_refuse() { printf 'REFUSED  coverage-check: %s\n' "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

usage() {
  cat <<'EOF'
check-coverage.sh — fail-closed coverage gate engine (QA-CRIER-34)

Usage:
  bash scripts/check-coverage.sh [--report] [--selftest]

Runs the -short suite with -coverprofile into coverage.out, echoing its
output in full and retaining it in $COVERAGE_LOG (default
.coverage-test-output.log). Requires the suite to exit 0, parses the
`total:` line, refuses missing/unreadable/unparseable coverage data, and
compares the number against COVERAGE_THRESHOLD (default 70.0).

On a RED suite it prints the failing package import paths, the failing
test names and the total measured from the failed run beside the
threshold — extracted from the retained log, so no fixed output window
can hide the failure.

--report measures and prints without applying the threshold verdict
(`make coverage`); the suite must still be green.

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

# _log_path — the retention path, made absolute for the echo line so the
# operator can open it from anywhere (and CI can attach it).
_log_path() {
  local p="$LOG"
  case "$p" in /*) ;; *) p="$PWD/$p" ;; esac
  printf '%s' "$p"
}

# _log_lines <log> — how many lines of suite output were retained.
_log_lines() { wc -l <"$1" 2>/dev/null | tr -d ' ' || printf '0'; }

# _failing_packages <log> — every import path `go test` marked FAIL, plus
# the `# <pkg>` headers of build failures, deduplicated. Read from the
# RETAINED log, so the position of a failure in the output cannot hide it.
_failing_packages() {
  {
    grep -E '^FAIL[[:space:]]+[^[:space:]]' "$1" 2>/dev/null | awk '{print $2}'
    grep -E '^#[[:space:]]+[^[:space:]]' "$1" 2>/dev/null | awk '{print $2}'
  } | sort -u
}

# _failing_tests <log> — every `--- FAIL: <TestName>` from the retained log
# (subtests arrive indented, so the match is anchored loosely), deduped.
_failing_tests() {
  grep -E '^[[:space:]]*--- FAIL: ' "$1" 2>/dev/null | awk '{print $3}' | sort -u
}

# _report_failures <log> — RELEASE-027: the attribution a red run owed the
# operator all along. Printed on STDOUT (so a step log that only captures
# stdout still names the failure), from the retained log rather than a
# fixed window of it, together with the retention path itself.
_report_failures() {
  local log="$1" pkg test found=0
  for pkg in $(_failing_packages "$log"); do
    _info "FAILING: package $pkg"
    found=1
  done
  for test in $(_failing_tests "$log"); do
    _info "FAILING: test $test"
    found=1
  done
  if [ "$found" -eq 0 ]; then
    _info "FAILING: no package or test name parsed from the suite log — read the retained log: $(_log_path)"
  fi
  _info "log: $(_log_path) ($(_log_lines "$log") line(s) retained, full go test output)"
}

# _total_from_profile <profile> — the parsed total, or empty.
_total_from_profile() {
  "$GO_BIN" tool cover -func="$1" 2>/dev/null | grep '^total:' | awk '{print $3}' | sed 's/%$//'
}

# _report_red_total <profile> — RELEASE-027: a red run still reports the
# number it measured (from the profile the failed suite left behind) beside
# the threshold, explicitly as a measurement and never as a verdict.
_report_red_total() {
  local t=""
  t="$(_total_from_profile "$1")"
  if _data_ok "$t"; then
    _info "failed-run total ${t} percent (threshold ${THRESHOLD} percent) — measurement only; a red suite is refused, never graded"
  else
    _info "failed-run total unparseable (threshold ${THRESHOLD} percent) — the failed suite left no readable coverage profile"
  fi
}

run_check() {
  local prof="coverage.out"
  local test_rc=0 total_line="" coverage=""

  # The gate may only ever grade a profile from THIS run: a profile left
  # behind by an earlier tree must never be readable when the current
  # suite fails to produce one (the stale-data RED arm).
  rm -f "$prof" 2>/dev/null || true
  # Same rule for the retained log: never let a previous run's evidence be
  # read as this run's (RELEASE-027).
  rm -f "$LOG" 2>/dev/null || true

  resolve_go || return $?

  if ! ( : >"$LOG" ) 2>/dev/null; then
    _refuse "cannot write the suite log '$(_log_path)' — refusing to run a suite whose output could not be retained or attributed."
    return 4
  fi
  _info "log: $(_log_path) (full go test output is retained here, untruncated)"

  # NEUTER-MARK[suite-status]: the suite verdict call. The selftest seds
  # exactly this line (and asserts the copy changed) to prove refusals
  # 3/4 are caused by THIS status check and not by accident.
  local cmd=("$GO_BIN" test -short -count=1 -coverprofile="$prof" ./...)
  _info "cmd: ${cmd[*]}"
  # No window, no truncation (RELEASE-027): the suite's own output goes to
  # stdout in full AND to the retained log. `set -o pipefail` is in force,
  # so go test's status — not tee's — is the pipeline's status and a red
  # suite can never be laundered green by the retention pipe.
  "${cmd[@]}" 2>&1 | tee "$LOG"
  test_rc=$?
  if [ "$test_rc" -ne 0 ]; then
    _refuse "the test suite FAILED (exit $test_rc) — no coverage verdict can be trusted from a red run; refusing to grade stale or missing data."
    _report_failures "$LOG"
    _report_red_total "$prof"
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
  # RELEASE-027 / AC2: the one-line, parseable green summary — measured
  # total AND threshold, both present, always.
  _info "${coverage} percent (threshold ${THRESHOLD} percent)"
  _info "log: $(_log_path) ($(_log_lines "$LOG") line(s) retained, full go test output)"

  if [ "$MODE" = "report" ]; then
    _info "report mode: measuring only — the threshold verdict belongs to make coverage-check"
    return 0
  fi

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

# _mk_noisy_module <dir> — RELEASE-027 fixture: a scratch module whose
# PASSING package sorts first and whose FAILING package sorts LAST and
# emits 40+ lines of diagnostics BEFORE its failure line. Any fixed
# first/last-N window of the suite output drops exactly that failure — the
# defect this arm exists to keep dead.
_mk_noisy_module() {
  local dir="$1"
  mkdir -p "$dir/aaa" "$dir/zzz"
  cat >"$dir/go.mod" <<EOF
module selftest.example/noisy

go 1.24
EOF
  cat >"$dir/aaa/aaa.go" <<'GO'
package aaa

func A(x int) int { return x }
GO
  cat >"$dir/aaa/aaa_test.go" <<'GO'
package aaa

import "testing"

func TestA(t *testing.T) {
	if A(1) != 1 {
		t.Fatal("A(1) != 1")
	}
}
GO
  cat >"$dir/zzz/zzz.go" <<'GO'
package zzz

func Z(x int) int { return x }
GO
  cat >"$dir/zzz/zzz_test.go" <<'GO'
package zzz

import (
	"fmt"
	"testing"
)

// TestDeepFailure buries its failure below 40 lines of output.
func TestDeepFailure(t *testing.T) {
	for i := 0; i < 40; i++ {
		fmt.Printf("zzz diagnostic line %02d\n", i)
	}
	t.Fatal("deliberately failing test buried below 40 lines of output")
}
GO
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
  # `tee` is on this list because the suite output is now retained through
  # it (RELEASE-027); every arm must run with the same tool set.
  for b in sh bash env awk grep sed head printf mkdir rm cat wc cmp mktemp dirname tr uname cut sort tee; do
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
  _mk_noisy_module "$tmp/mod-noisy"

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

  # 13. RELEASE-027: a FAILING suite ATTRIBUTES ITSELF ON STDOUT — the
  #     failing package import path, the failing TEST NAME, the total
  #     measured from the failed run beside the threshold, and the path of
  #     the retained full log. This is exactly the information the old
  #     `head -30` window threw away (CI run 43303623612).
  checks=$((checks + 1))
  local red_log="$tmp/red-run.log" red_out="" red_err=""
  red_out="$(cd "$tmp/mod-fail" && COVERAGE_LOG="$red_log" bash "$SELF" 2>"$tmp/red-run.err")"
  rc=$?
  red_err="$(cat "$tmp/red-run.err" 2>/dev/null)"
  if [ "$rc" -ne 2 ]; then
    printf '%s selftest: FAIL: the attributing red-run arm exited %d, not 2\n  stdout: %s\n  stderr: %s\n' "$PROG" "$rc" "$red_out" "$red_err" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$red_out" | grep -q 'coverage: FAILING: package selftest.example/qa34/p'; then
    printf '%s selftest: FAIL: the red run did not name the failing package on STDOUT\n  stdout: %s\n  stderr: %s\n' "$PROG" "$red_out" "$red_err" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$red_out" | grep -q 'coverage: FAILING: test TestBroken'; then
    printf '%s selftest: FAIL: the red run did not name the failing TEST on STDOUT\n  stdout: %s\n  stderr: %s\n' "$PROG" "$red_out" "$red_err" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$red_out" | grep -Eq 'coverage: failed-run total [0-9]+([.][0-9]+)? percent \(threshold 70\.0 percent\)'; then
    printf '%s selftest: FAIL: the red run did not print the measured total beside the threshold\n  stdout: %s\n  stderr: %s\n' "$PROG" "$red_out" "$red_err" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$red_out" | grep -q "coverage: log: $red_log"; then
    printf '%s selftest: FAIL: the red run did not name the retained log path\n  stdout: %s\n  stderr: %s\n' "$PROG" "$red_out" "$red_err" >&2
    fails=$((fails + 1))
  elif [ ! -s "$red_log" ] || ! grep -q -e '--- FAIL: TestBroken' "$red_log"; then
    printf '%s selftest: FAIL: the full go test output was not retained at %s\n  log: %s\n' "$PROG" "$red_log" "$(cat "$red_log" 2>/dev/null)" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: RELEASE-027: a red suite names its failing package, its failing test, the measured total and the retained log on STDOUT (rc=2)\n'
  fi

  # 14. RELEASE-027 / AC2: a green run prints the parseable one-line
  #     summary carrying BOTH the measured total and the threshold.
  checks=$((checks + 1))
  out="$(cd "$tmp/mod-pass" && bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: the green arm for the summary line exited %d, not 0\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$out" | grep -Eq '^coverage: [0-9]+([.][0-9]+)? percent \(threshold 70\.0 percent\)$'; then
    printf '%s selftest: FAIL: the green run did not print the parseable summary line `coverage: NN.N percent (threshold 70.0 percent)`\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: RELEASE-027: a green run prints `coverage: NN.N percent (threshold 70.0 percent)` with both numbers\n'
  fi

  # 15. RELEASE-027: NO WINDOW TRUNCATION — the failing package and test
  #     sit BELOW 40+ lines of suite output and are still named, the 40th
  #     diagnostic line is still echoed to stdout, and the retained log
  #     holds every line. A fixed first/last-N window fails this arm.
  checks=$((checks + 1))
  local noisy_log="$tmp/noisy-run.log" noisy_out=""
  noisy_out="$(cd "$tmp/mod-noisy" && COVERAGE_LOG="$noisy_log" bash "$SELF" 2>/dev/null)"
  rc=$?
  if [ "$rc" -ne 2 ]; then
    printf '%s selftest: FAIL: the deep-failure arm exited %d, not 2\n  stdout: %s\n' "$PROG" "$rc" "$noisy_out" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$noisy_out" | grep -q 'coverage: FAILING: package selftest.example/noisy/zzz'; then
    printf '%s selftest: FAIL: the failure BELOW 40 lines of output was not attributed (fixed-window truncation is back)\n  stdout: %s\n' "$PROG" "$noisy_out" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$noisy_out" | grep -q 'coverage: FAILING: test TestDeepFailure'; then
    printf '%s selftest: FAIL: the failing test BELOW 40 lines of output was not named\n  stdout: %s\n' "$PROG" "$noisy_out" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$noisy_out" | grep -q 'zzz diagnostic line 39'; then
    printf '%s selftest: FAIL: the 40th line of suite output was not echoed to stdout (a window is cutting the log)\n  stdout: %s\n' "$PROG" "$noisy_out" >&2
    fails=$((fails + 1))
  elif [ ! -s "$noisy_log" ] || [ "$(_log_lines "$noisy_log")" -lt 40 ] || ! grep -q 'zzz diagnostic line 39' "$noisy_log"; then
    printf '%s selftest: FAIL: the retained log did not hold the full suite output (%s line(s))\n  log: %s\n' "$PROG" "$(_log_lines "$noisy_log" 2>/dev/null)" "$(cat "$noisy_log" 2>/dev/null)" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: RELEASE-027: a failure buried below 40+ lines is still named, echoed and retained — no fixed output window\n'
  fi

  # 16. RELEASE-027: the retention path is honoured on a GREEN run too and
  #     holds the suite's own output, so the operator can always re-read
  #     what the gate measured.
  checks=$((checks + 1))
  local green_log="$tmp/green-run.log"
  out="$(cd "$tmp/mod-pass" && COVERAGE_LOG="$green_log" bash "$SELF" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: the green retention arm exited %d, not 0\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s\n' "$out" | grep -q "coverage: log: $green_log"; then
    printf '%s selftest: FAIL: the green run did not print the retention path\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif [ ! -s "$green_log" ] || ! grep -q 'selftest.example/qa34/p' "$green_log"; then
    printf '%s selftest: FAIL: the green run did not retain the suite output at %s\n  log: %s\n' "$PROG" "$green_log" "$(cat "$green_log" 2>/dev/null)" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: RELEASE-027: a green run retains the full suite output at the printed path (COVERAGE_LOG honoured)\n'
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
      --report)
        MODE="report"
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
