#!/usr/bin/env bash
#
# scripts/check-bunker-matrix-verdict.sh — verdict-table gate for
# scripts/bunker-matrix.sh (QA-CRIER-36).
#
# WHY THIS EXISTS
# ---------------
# scripts/bunker-matrix.sh ended in the bare test `[[ $FAIL -eq 0 ]]`, so a run
# in which EVERY cell was skipped (PASS=0, FAIL=0, SKIP=4) exited 0 — a vacuous
# green that verified nothing while reading as a pass. QA-CRIER-36 replaced
# that line with a matrix_verdict() function: FAIL>0 -> exit 1, FAIL=0 with
# PASS>0 -> exit 0, FAIL=0 with PASS=0 (nothing actually probed) -> exit 2 with
# a loud line on both streams. This checker keeps that verdict table honest
# WITHOUT running any server, any deploy or any network call:
#
#   1. it extracts the matrix_verdict() function from the REAL tracked matrix
#      (sed range on the `matrix_verdict() {` ... `^}` lines),
#   2. wraps the extracted body in an rc-carrying harness and drives it with
#      overridden PASS/FAIL/SKIP under a fresh bash, asserting the exit code of
#      every row of the contract table (including all-skipped -> 2),
#   3. asserts the loud FATAL line appears on the vacuous arms and NOWHERE
#      else,
#   4. scans the matrix's CODE (comment lines stripped) for a regressed bare
#      `[[ $FAIL -eq 0 ]]` test — a re-introduced bare test is rejected even if
#      matrix_verdict() is still present,
#   5. asserts the single exit point is intact: the bare `matrix_verdict` call
#      line followed by `exit $?`, and the summary line still printed before
#      exiting.
#
# FAIL-CLOSED: a missing matrix_verdict() function, a missing file, or a
# missing bash is a loud nonzero exit — never a PASS over nothing. The
# checker does not run scripts/bunker-matrix.sh itself; it only reads it and
# executes the extracted function in a harness with no side effects.
#
# USAGE
# -----
#   bash scripts/check-bunker-matrix-verdict.sh                  # check the tracked matrix
#   bash scripts/check-bunker-matrix-verdict.sh --matrix <path>  # check another copy
#   bash scripts/check-bunker-matrix-verdict.sh --selftest       # prove this checker
#   bash scripts/check-bunker-matrix-verdict.sh -h | --help
#
# EXIT CODES
#   0  every table row behaved, loud lines verified, no regression found
#   1  at least one assertion failed (wrong verdict table, missing loud line,
#      re-introduced bare test, missing verdict function/call)
#   2  misuse or a missing matrix file
#
# SELFTEST — `make bunker-matrix-verdict-selftest`: builds fixtures under
# ${TMPDIR:-/tmp} from the REAL matrix and asserts this checker still rejects
#   a. a mutated verdict table (PASS>0 branch widened: all-skips exits 0),
#   b. a mutated verdict table (FAIL branch widened: a failed probe exits 0),
#   c. a matrix whose matrix_verdict() function is MISSING entirely,
#   d. the bare `[[ $FAIL -eq 0 ]]` test re-introduced as CODE,
# and accepts
#   e. the clean matrix — including its header COMMENT that mentions the bare
#      test (the scanner strips comments; a mention is not a regression),
# plus the NEUTER proof: a copy of this checker with the table arm's verdict
# forced to success must ACCEPT the same mutated-table fixture the real arm
# rejects — the rejection is caused by that arm, not by accident.
# Exit: 0 all assertions behaved, 1 at least one failed, 2 a dependency is
# missing.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-bunker-matrix-verdict"

_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

usage() {
  cat <<'EOF'
check-bunker-matrix-verdict.sh — verdict-table gate for scripts/bunker-matrix.sh
(QA-CRIER-36: a run in which every cell was skipped must NOT exit 0)

Usage:
  bash scripts/check-bunker-matrix-verdict.sh                  # tracked matrix
  bash scripts/check-bunker-matrix-verdict.sh --matrix <path>  # another copy
  bash scripts/check-bunker-matrix-verdict.sh --selftest
  bash scripts/check-bunker-matrix-verdict.sh -h | --help

The verdict contract asserted (by DRIVING the matrix's own matrix_verdict()
function with overridden counters — no server, no deploy, no network):

  FAIL>0            -> exit 1
  FAIL=0, PASS>0    -> exit 0
  FAIL=0, PASS=0    -> exit 2 + "FATAL: vacuous green ..." on stdout AND stderr

and the matrix's code must not carry a re-introduced bare `[[ $FAIL -eq 0 ]]`
test (comments are stripped first — a mention in prose is not a regression).

Exit codes: 0 all assertions passed, 1 at least one failed, 2 misuse or a
missing matrix file.
EOF
}

# ── dependencies ──────────────────────────────────────────────────────────────
# bash only — the matrix's own interpreter. Nothing else is needed.

# ── extraction + execution harness ────────────────────────────────────────────

# extract_verdict <matrix-path> -> matrix_verdict() source on stdout (empty if absent)
extract_verdict() {
  sed -n '/^matrix_verdict() {/,/^}/p' "$1"
}

# verdict_exit <PASS> <FAIL> <SKIP> <fixture> -> child rc in $?, child output in
# _VX_OUT / _VX_ERR. Runs a FRESH bash with the matrix's own `set -uo pipefail`
# (the environment the function really executes under), seeds the counters and
# calls the extracted matrix_verdict(). The rc is carried by the assignment's
# command-substitution exit status — this checker does NOT run under `set -e`,
# so a nonzero child rc lands in $? instead of killing the harness.
verdict_exit() { # <p> <f> <s> <fixture>
  local p="$1" f="$2" s="$3" fx="$4" errf="" rc=0
  _VX_OUT=""
  _VX_ERR=""
  errf="$(mktemp "${TMPDIR:-/tmp}/vx-err.XXXXXX")" || return 2
  _VX_OUT="$(bash -c "set -uo pipefail
    PASS=$p FAIL=$f SKIP=$s
    source '$fx'
    matrix_verdict" 2>"$errf")"
  rc=$?
  _VX_ERR="$(cat "$errf")"
  rm -f "$errf"
  return "$rc"
}

# ── the verdict contract table ────────────────────────────────────────────────
# Parallel arrays (bash-3 compatible): the four rows of the QA-CRIER-36 table.
TABLE_PASS=(0 4 2 0)
TABLE_FAIL=(0 0 1 0)
TABLE_SKIP=(4 0 1 0)
TABLE_WANT=(2 0 1 2)
TABLE_WHY=(
  "every cell skipped — the vacuous green (the QA-CRIER-36 regression)"
  "all four cells probed and green"
  "one probe failed"
  "nothing ran at all — equally vacuous"
)
LOUD_WANT=(yes no no yes) # the FATAL line appears only on the vacuous arms

# ── main check ────────────────────────────────────────────────────────────────

run_checks() { # <matrix-path>
  local src="$1"
  local tmp="" wrap="" out="" rc=0
  local fails=0 checks=0 i="" p="" f="" s="" want="" why=""

  if [ ! -f "$src" ] || [ ! -r "$src" ]; then
    _err "matrix script not found or not readable: $src (pass --matrix <path>?)"
    return 2
  fi

  tmp="$(mktemp -d "${TMPDIR:-/tmp}/check-bunker-matrix-verdict.XXXXXX")" || {
    _err "mktemp -d failed"
    return 2
  }
  # No trap-based cleanup dance here: every path below rm -rf's the scratch dir
  # explicitly before returning, and the checker has no background children.
  wrap="$tmp/verdict-wrapper.sh"

  # (0) the function must EXIST — a renamed/removed matrix_verdict() is a
  # regression this checker refuses to grade as a pass (fail-closed).
  checks=$((checks + 1))
  out="$(extract_verdict "$src")"
  if [ -z "$out" ]; then
    _err "no matrix_verdict() function found in $src — the QA-CRIER-36 verdict contract is not implemented (or was renamed); refusing a vacuous pass"
    rm -rf "$tmp"
    return 1
  fi
  # Wrap the EXTRACTED body: rc pre-seeded to 1 so a body that fails to run
  # (or a truncated extraction) exits nonzero, never 0.
  {
    printf '# extracted by %s from %s — do not edit\n' "$PROG" "$src"
    printf 'rc=1\n'
    printf '%s\n' "$out"
  } >"$wrap"
  _info "verdict source: extracted matrix_verdict() from $src ($(printf '%s\n' "$out" | wc -l | tr -d ' ') lines)"

  # (1) every row of the contract table, driven through the real function.
  for i in 1 2 3 4; do
    checks=$((checks + 1))
    p="${TABLE_PASS[$((i - 1))]}"
    f="${TABLE_FAIL[$((i - 1))]}"
    s="${TABLE_SKIP[$((i - 1))]}"
    want="${TABLE_WANT[$((i - 1))]}"
    why="${TABLE_WHY[$((i - 1))]}"
    verdict_exit "$p" "$f" "$s" "$wrap"
    rc=$?
    if [ "$rc" -ne "$want" ]; then
      _err "verdict table: row $i [$why]: PASS=$p FAIL=$f SKIP=$s -> exit $rc, want $want"
      fails=$((fails + 1)) # NEUTER-MARK[table-verdict]
      continue
    fi
    if [ "$want" -eq 2 ] \
      && { ! printf '%s' "$_VX_OUT" | grep -qF 'FATAL: vacuous green' || ! printf '%s' "$_VX_ERR" | grep -qF 'FATAL: vacuous green'; }; then
      _err "verdict table: row $i [$why]: exit is 2 but the loud 'FATAL: vacuous green' line was not printed on both streams (stdout hits: $(printf '%s' "$_VX_OUT" | grep -cF 'FATAL: vacuous green' || true), stderr hits: $(printf '%s' "$_VX_ERR" | grep -cF 'FATAL: vacuous green' || true))"
      fails=$((fails + 1))
      continue
    fi
    if [ "$want" -ne 2 ] \
      && { printf '%s' "$_VX_OUT" | grep -qF 'FATAL: vacuous green' || printf '%s' "$_VX_ERR" | grep -qF 'FATAL: vacuous green'; }; then
      _err "verdict table: row $i [$why]: exit $want but the vacuous-green FATAL line was printed anyway"
      fails=$((fails + 1))
      continue
    fi
    printf 'PASS  row %d: PASS=%s FAIL=%s SKIP=%s -> exit %d (%s) — %s\n' \
      "$i" "$p" "$f" "$s" "$rc" \
      "$([ "$want" -eq 2 ] && echo 'loud on both streams' || echo 'no loud line')" \
      "$why"
  done

  # (2) the loud line must reach BOTH streams on the vacuous arm — the matrix
  # prints it once on stdout and once on stderr; assert each, not the sum.
  checks=$((checks + 1))
  verdict_exit 0 0 4 "$wrap"
  rc=$?
  if [ "$rc" -eq 2 ] \
    && printf '%s' "$_VX_OUT" | grep -qF 'FATAL: vacuous green' \
    && printf '%s' "$_VX_ERR" | grep -qF 'FATAL: vacuous green'; then
    printf 'PASS  loud line reaches BOTH stdout and stderr on the all-skipped arm (exit %d)\n' "$rc"
  else
    _err "the all-skipped arm must exit 2 with the FATAL line on stdout AND stderr (rc=$rc; stdout hits: $(printf '%s' "$_VX_OUT" | grep -cF 'FATAL: vacuous green' || true), stderr hits: $(printf '%s' "$_VX_ERR" | grep -cF 'FATAL: vacuous green' || true))"
    fails=$((fails + 1)) # NEUTER-MARK[loud-stream-verdict]
  fi

  # (3) no re-introduced bare failure test in CODE: strip whole-line comments
  # first (the header NOTE quotes the old line on purpose), then scan.
  checks=$((checks + 1))
  if sed 's/^[[:space:]]*#.*//' "$src" | grep -nF '[[ $FAIL -eq 0 ]]' >/dev/null; then
    _err "regressed bare failure test found in $src (code lines): the verdict must go through matrix_verdict(), not an inline \`[[ \$FAIL -eq 0 ]]\`"
    sed 's/^[[:space:]]*#.*//' "$src" | grep -nF '[[ $FAIL -eq 0 ]]' | sed 's/^/  code line: /' >&2
    fails=$((fails + 1))
  else
    printf 'PASS  no bare [[ $FAIL -eq 0 ]] test in code (comment mentions excluded)\n'
  fi

  # (4) the single exit point: the bare matrix_verdict call + exit $? (the
  # empty line between the function and its call line belongs to the range, so
  # anchor on the call LINE and require the exit line right after).
  checks=$((checks + 1))
  local after=""
  after="$(grep -A1 '^matrix_verdict$' "$src" | tail -n 1)"
  if ! grep -nq '^matrix_verdict$' "$src" || [ "$after" != 'exit $?' ]; then
    _err "the single exit point is broken: expected a bare \`matrix_verdict\` call line followed by \`exit \$?\` in $src (found after the call: '$after')"
    fails=$((fails + 1))
  else
    printf 'PASS  single exit point intact: bare matrix_verdict call + exit $?\n'
  fi

  # (5) the summary line still prints before exiting (QA-CRIER-36 keeps it).
  checks=$((checks + 1))
  if ! grep -nq '^echo "== matrix done: \$PASS pass / \$FAIL fail / \$SKIP skipped (evidence \$EVIDENCE) =="$' "$src"; then
    _err "the summary line (\`== matrix done: ...\`) is missing or no longer printed before the verdict in $src"
    fails=$((fails + 1))
  else
    printf 'PASS  summary line still printed before exiting\n'
  fi

  rm -rf "$tmp"

  if [ "$fails" -ne 0 ]; then
    _info "FAIL — verdict contract: $((checks - fails))/$checks assertions held on $src"
    return 1
  fi
  _info "PASS — verdict contract: $checks/$checks assertions held on $src (vacuous-green rejection armed)"
  return 0
}

# ── selftest ──────────────────────────────────────────────────────────────────

SELFTEST_TMP=""
_selftest_cleanup() {
  if [ -n "$SELFTEST_TMP" ]; then
    rm -rf "$SELFTEST_TMP"
    SELFTEST_TMP=""
  fi
  return 0
}

_selftest() {
  local fails=0 checks=0
  SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-bunker-matrix-verdict-selftest.XXXXXX")" || {
    _err "selftest: mktemp -d failed"
    return 1
  }
  local tmp="$SELFTEST_TMP"
  trap '_selftest_cleanup' EXIT

  local repo="" real=""
  repo="$(cd "$(dirname "$SELF")/.." && pwd)"
  real="$repo/scripts/bunker-matrix.sh"
  if [ ! -f "$real" ]; then
    _err "selftest: cannot find the tracked matrix at $real"
    return 2
  fi

  local out="" rc=0

  # Fixture factory: a copy of the REAL matrix with one sed applied, so every
  # mutation is attributable to exactly that edit.
  fixture() { # <name> <sed-script>
    cp "$real" "$tmp/$1" || return 1
    sed -i.tmp -e "$2" "$tmp/$1"
    rm -f "$tmp/$1.tmp"
    return 0
  }

  # a. PASS>0 branch widened (( PASS > 0 ) -> ( PASS >= 0 )): the all-skipped
  #    arm then exits 0 — exactly the vacuous green QA-CRIER-36 removed.
  checks=$((checks + 1))
  fixture fx-pass-ge 's/if (( PASS > 0 )); then/if (( PASS >= 0 )); then/' || {
    _err "selftest: could not build the pass-ge fixture"; return 2; }
  if cmp -s "$real" "$tmp/fx-pass-ge"; then
    _err "selftest: the pass-ge sed no longer matches (( PASS > 0 )) — the fixture is a no-op and the assertion would be vacuous"
    return 2
  fi
  out="$(bash "$SELF" --matrix "$tmp/fx-pass-ge" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ] \
    || ! printf '%s' "$out" | grep -q 'verdict table: row 1' \
    || ! printf '%s' "$out" | grep -q 'PASS=0 FAIL=0 SKIP=4 -> exit 0, want 2'; then
    printf '%s selftest: FAIL: a widened PASS>0 branch (all-skips -> exit 0) was %s (rc=%d)\n  output: %s\n' \
      "$PROG" "$([ "$rc" -eq 0 ] && echo ACCEPTED || echo rejected-without-the-vacuous-arm-named)" "$rc" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a widened PASS>0 branch (all-skips -> 0) is rejected and the vacuous arm (row 1: 0/0/4 -> 0) is named\n'
  fi

  # b. FAIL branch widened (( FAIL > 0 ) -> ( FAIL >= 0 )): a failed probe then
  #    exits 0 too — the other side of the same regression.
  checks=$((checks + 1))
  fixture fx-fail-ge 's/if (( FAIL > 0 )); then/if (( FAIL >= 0 )); then/' || {
    _err "selftest: could not build the fail-ge fixture"; return 2; }
  if cmp -s "$real" "$tmp/fx-fail-ge"; then
    _err "selftest: the fail-ge sed no longer matches (( FAIL > 0 )) — the fixture is a no-op and the assertion would be vacuous"
    return 2
  fi
  out="$(bash "$SELF" --matrix "$tmp/fx-fail-ge" 2>&1)"
  rc=$?
  # ( FAIL >= 0 ) makes the first branch ALWAYS true: every arm exits 1, so the
  # discriminating named cell is row 2 — an all-green run rejected as a failure.
  if [ "$rc" -eq 0 ] \
    || ! printf '%s' "$out" | grep -q 'verdict table: row 2' \
    || ! printf '%s' "$out" | grep -q 'PASS=4 FAIL=0 SKIP=0 -> exit 1, want 0'; then
    printf '%s selftest: FAIL: a widened FAIL>0 branch (always-true, all-green -> exit 1) was %s (rc=%d)\n  output: %s\n' \
      "$PROG" "$([ "$rc" -eq 0 ] && echo ACCEPTED || echo rejected-without-the-green-arm-named)" "$rc" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a widened FAIL>0 branch (always-true, all-green -> 1) is rejected and the green arm (row 2: 4/0/0 -> 1) is named\n'
  fi

  # c. the verdict function missing entirely — fail-closed, never a pass over
  #    nothing.
  checks=$((checks + 1))
  fixture fx-nofn '/^matrix_verdict() {/,/^}/d' || {
    _err "selftest: could not build the no-function fixture"; return 2; }
  out="$(bash "$SELF" --matrix "$tmp/fx-nofn" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ] || ! printf '%s' "$out" | grep -q 'no matrix_verdict() function found'; then
    printf '%s selftest: FAIL: a matrix without matrix_verdict() was %s (rc=%d)\n  output: %s\n' \
      "$PROG" "$([ "$rc" -eq 0 ] && echo ACCEPTED || echo rejected-without-the-named-reason)" "$rc" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a matrix with the verdict function removed is rejected (fail-closed, no vacuous pass)\n'
  fi

  # d. the bare test re-introduced as CODE (function still present): the token
  #    scan must name it. The line is appended AFTER `exit $?`, so it runs at
  #    source time only inside the harness — the scan fires first regardless.
  checks=$((checks + 1))
  cp "$real" "$tmp/fx-bare" || return 2
  printf '[[ $FAIL -eq 0 ]]\n' >>"$tmp/fx-bare"
  out="$(bash "$SELF" --matrix "$tmp/fx-bare" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ] || ! printf '%s' "$out" | grep -q 'regressed bare failure test'; then
    printf '%s selftest: FAIL: a re-introduced bare [[ $FAIL -eq 0 ]] code line was %s (rc=%d)\n  output: %s\n' \
      "$PROG" "$([ "$rc" -eq 0 ] && echo ACCEPTED || echo rejected-without-the-token-named)" "$rc" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a re-introduced bare [[ $FAIL -eq 0 ]] code line is rejected and named\n'
  fi

  # e. precision: the clean matrix — whose header COMMENT quotes the bare test
  #    — must be ACCEPTED. A comment mention is prose, not a regression.
  checks=$((checks + 1))
  out="$(bash "$SELF" --matrix "$real" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: the clean tracked matrix was REJECTED (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'PASS — verdict contract'; then
    printf '%s selftest: FAIL: the clean matrix passed without the PASS verdict line\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: the clean tracked matrix is accepted — the header comment MENTION of the bare test does not fire the scanner\n'
  fi

  # NEUTER PROOF: a copy of this checker with BOTH verdict arms neutered (the
  # table arm AND the loud-stream arm — the pass-ge mutation trips both, so a
  # one-arm neuter would leave the other rejecting and prove nothing) must
  # ACCEPT fixture (a): the rejection is caused by those arms, not by accident.
  checks=$((checks + 1))
  local neutered="$tmp/neutered-checker.sh"
  cp "$SELF" "$neutered" || return 2
  sed -i.tmp \
    -e 's|^\([[:space:]]*\)fails=\$((fails + 1)).*NEUTER-MARK\[table-verdict\]$|\1:|' \
    -e 's|^\([[:space:]]*\)fails=\$((fails + 1)).*NEUTER-MARK\[loud-stream-verdict\]$|\1:|' \
    "$neutered"
  rm -f "$neutered.tmp"
  if cmp -s "$SELF" "$neutered"; then
    printf '%s selftest: FAIL: the neuter seds no longer match the arm verdict lines (NEUTER-MARK[table-verdict] / NEUTER-MARK[loud-stream-verdict]) — the causality proof would be vacuous\n' "$PROG" >&2
    return 2
  fi
  out="$(bash "$neutered" --matrix "$tmp/fx-pass-ge" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: NEUTER PROOF: with both verdict arms neutered the mutated-table fixture is STILL rejected (rc=%d) — the rejection does not come from those arms\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: NEUTER PROOF: the neutered copy (both verdict arms forced to success) ACCEPTS the same mutated-table fixture the real checker rejects — the rejection is caused by those arms\n'
  fi

  if [ "$fails" -ne 0 ]; then
    printf '%s selftest: %d/%d checks behaved — FAIL\n' "$PROG" "$((checks - fails))" "$checks" >&2
    return 1
  fi
  printf '%s selftest: %d/%d checks behaved (wrong-table a/b, missing-function, bare-test-in-code, comment-precision, neuter proof)\n' \
    "$PROG" "$checks" "$checks"
  return 0
}

# ── entry point ───────────────────────────────────────────────────────────────

main() {
  local matrix="" selftest=0
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --selftest)
        selftest=1
        shift
        ;;
      --matrix)
        [ "$#" -ge 2 ] || {
          _err "--matrix needs a path"
          return 2
        }
        matrix="$2"
        shift 2
        ;;
      --matrix=*)
        matrix="${1#*=}"
        shift
        ;;
      -h | --help | help)
        usage
        return 0
        ;;
      *)
        _err "unknown argument '$1' (try --help)"
        return 2
        ;;
    esac
  done

  if [ "$selftest" -eq 1 ]; then
    _selftest
    return $?
  fi

  if [ -z "$matrix" ]; then
    local repo=""
    repo="$(cd "$(dirname "$SELF")/.." && pwd)" || {
      _err "cannot resolve the repo root from $SELF (pass --matrix <path>)"
      return 2
    }
    matrix="$repo/scripts/bunker-matrix.sh"
  fi

  run_checks "$matrix"
}

main "$@"
exit $?
