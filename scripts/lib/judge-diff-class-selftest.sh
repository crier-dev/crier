#!/usr/bin/env bash
#
# scripts/lib/judge-diff-class-selftest.sh — prove the board-only classifier's
# contract, and prove the proof is not vacuous (DF-CRIER-278).
#
# WHY THIS EXISTS
# ---------------
# scripts/lib/judge-diff-class.sh decides whether the Tier-2 judge may be SKIPPED
# on a `gitreins task complete` close. A classifier that is wrong in the
# permissive direction silently drops the judge on a diff the rule meant to
# protect, and nothing in the repo would notice: the failure mode of this script
# is an UNRUN evaluation, which leaves no red anywhere. So the contract is
# measured here, on synthetic path lists only — no git commands, no repository
# state, no network, no fixtures on disk beyond the scratch dir.
#
# WHAT IT PROVES (the mandated cases 1-7, plus the boundary decisions the
# classifier's own header documents)
#   1. a board-only list (docs/x.md, .coding-hermes/board/tasks.jsonl)
#         -> verdict=board-only, exit 0
#   2. a source list (internal/relay/handler.go)
#         -> verdict=source-bearing; without --allow-source, --strict-verify
#            exits 1 (and prints the verdict first)
#   3. the same source list WITH --allow-source
#         -> verdict=source-bearing, exit 0
#   4. EMPTY stdin -> exit 3 with the refusal line, in EVERY flag combination
#      (no flag, --strict-verify, --allow-source, both) and no verdict line
#   5. an unknown flag -> exit 2, with the usage line
#   6. a MIXED list (docs + one .go file) -> source-bearing
#   7. the NEUTER PROOF: a copy of the classifier whose verdict emitter is forced
#      to `board-only` unconditionally must FAIL the selftest's own
#      source-bearing assertion (exit 0 where the real script exits 1), which is
#      what proves those assertions are driven by the REAL verdict output and
#      that a verdict-forced-success classifier cannot pass this selftest.
#
#   Then the boundary rows, because each is a decision the classifier makes
#   about a path the brief's list leaves ambiguous — and each is asserted in the
#   conservative direction:
#   B1. specs/AGENT-ECOSYSTEM.md            -> SOURCE (specs/** is a source tree
#       and wins over the *.md rule)
#   B2. docs/openapi.yaml                   -> SOURCE ("openapi yaml" is named as
#       source; docs/ prose is not)
#   B3. scripts/lib/foo.sh                  -> SOURCE (scripts/**)
#   B4. examples/agent-ecosystem/battery/Dockerfile -> SOURCE (nested build file,
#       inside a source tree)
#   B5. a full non-source list (root Makefile, Dockerfile, Dockerfile.mcp,
#       .github/workflows/ci.yml, LICENSE, NOTICE, .gitignore, .gitattributes,
#       README.md, .gitreins/tasks.yaml) -> board-only
#   B6. an unrecognized path (weird.xyz)    -> SOURCE (never a silent board-only)
#   B7. duplicate paths are folded: the summary counts UNIQUE paths
#   B8. a CRLF (trailing CR) path list still classifies, and the summary count is
#       right — a path list through a CRLF-aware pipe is not mis-read
#
# THE NEUTER PROOF'S OWN GUARD
#   The sed that forces the verdict is matched against the real call site; the
#   selftest asserts the pattern is still present on self (exactly once) BEFORE
#   sedding and that the copy actually DIFFERS afterwards, so a future refactor
#   that moves the call site fails this selftest loudly instead of neutering
#   nothing and leaving the causality proof vacuous.
#
# EXIT CODES: 0 every check behaved, 1 at least one did not.
#
# DEPENDENCIES: bash, plus mktemp/grep/sed/cmp/cat/rm for the scratch dir. No
# git, no network, no docker, no port.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SELF_DIR="$(cd -- "$(dirname -- "$SELF")" && pwd)"
CLASSIFIER="$SELF_DIR/judge-diff-class.sh"
PROG="judge-diff-class-selftest"

# The classifier's verdict-emitting call site. The neuter proof seds exactly this
# line; it is also asserted present-and-unique on self before the sed runs.
VERDICT_CALL='  _emit_verdict "$verdict"'

_fails=0
_checks=0
_TMP=""

_ok() { printf 'PASS: %s\n' "$*"; }
_bad() {
  printf '%s: FAIL: %s\n' "$PROG" "$*" >&2
  _fails=$((_fails + 1))
}

_cleanup() {
  if [ -n "$_TMP" ]; then
    rm -rf "$_TMP"
    _TMP=""
  fi
  return 0
}

# ── the runner: one classifier invocation, stdout / stderr / rc captured apart ─
_OUT=""
_ERR=""
_RC=0

# _run <listfile> [flags...] — run the CLASSIFIER over <listfile>'s stdin.
_run_classifier() {
  local bin="$1" list="$2"
  shift 2
  local efile="$_TMP/stderr.txt"
  _OUT="$(bash "$bin" "$@" <"$list" 2>"$efile")"
  _RC=$?
  _ERR="$(cat "$efile" 2>/dev/null || true)"
  return 0
}

_run() { # <listfile> [flags...] — the real classifier
  local list="$1"
  shift
  _run_classifier "$CLASSIFIER" "$list" "$@"
}

# _is_one_verdict_line <verdict> — stdout is EXACTLY `verdict=<verdict>` and
# nothing else (command substitution strips the one trailing newline, so any
# second line would break the equality).
_stdout_is() { [ "$_OUT" = "verdict=$1" ]; }

# _stderr_has_verdict — the verdict token must never leak onto stderr, or a
# caller reading the combined stream could see two of them.
_stderr_has_verdict() { printf '%s' "$_ERR" | grep -q 'verdict='; }

# _check <ok-bool-ish> <label> ... — assert helper used by the per-case blocks.
_assert_true() { # <label> <description-of-failure>
  _checks=$((_checks + 1))
  if [ "$1" = "1" ]; then
    _ok "$2"
  else
    _bad "$2"
  fi
}

# ── the selftest ──────────────────────────────────────────────────────────────

_selftest() {
  _TMP="$(mktemp -d "${TMPDIR:-/tmp}/judge-diff-class-selftest.XXXXXX")" || {
    printf '%s: FAIL: mktemp -d failed\n' "$PROG" >&2
    return 1
  }
  trap '_cleanup' EXIT

  if [ ! -f "$CLASSIFIER" ]; then
    _bad "classifier not found at $CLASSIFIER"
    return 1
  fi
  printf '%s: classifier under test: %s\n' "$PROG" "$CLASSIFIER"

  # ── fixtures: synthetic path lists only ─────────────────────────────────────
  local board_list="$_TMP/board-only.list"
  printf 'docs/x.md\n.coding-hermes/board/tasks.jsonl\n' >"$board_list"

  local source_list="$_TMP/source.list"
  printf 'internal/relay/handler.go\n' >"$source_list"

  local mixed_list="$_TMP/mixed.list"
  printf 'docs/x.md\ninternal/relay/handler.go\n' >"$mixed_list"

  local empty_list="$_TMP/empty.list"
  : >"$empty_list"

  local blank_list="$_TMP/blank.list"
  printf '\n   \n' >"$blank_list"

  local nonsource_list="$_TMP/nonsource-rich.list"
  printf 'Makefile\nDockerfile\nDockerfile.mcp\n.github/workflows/ci.yml\nLICENSE\nNOTICE\n.gitignore\n.gitattributes\nREADME.md\n.gitreins/tasks.yaml\n.coding-hermes/evidence/DF-CRIER-278.md\n' >"$nonsource_list"

  local dup_list="$_TMP/dup.list"
  printf 'docs/x.md\ndocs/x.md\n.coding-hermes/board/tasks.jsonl\n' >"$dup_list"

  local crlf_list="$_TMP/crlf.list"
  printf 'docs/x.md\r\n.coding-hermes/board/tasks.jsonl\r\n' >"$crlf_list"

  local specs_list="$_TMP/specs.list"
  printf 'specs/AGENT-ECOSYSTEM.md\n' >"$specs_list"

  local openapi_list="$_TMP/openapi.list"
  printf 'docs/openapi.yaml\n' >"$openapi_list"

  local script_list="$_TMP/script.list"
  printf 'scripts/lib/judge-diff-class.sh\n' >"$script_list"

  local nested_df_list="$_TMP/nested-dockerfile.list"
  printf 'examples/agent-ecosystem/battery/Dockerfile\n' >"$nested_df_list"

  local unknown_list="$_TMP/unknown.list"
  printf 'weird.xyz\n' >"$unknown_list"

  # ── CASE 1: board-only ──────────────────────────────────────────────────────
  _run "$board_list"
  _assert_true "$([ "$_RC" -eq 0 ] && printf 1 || printf 0)" \
    "case 1: a board-only list exits 0 (rc=$_RC)"
  _assert_true "$(_stdout_is board-only && printf 1 || printf 0)" \
    "case 1: a board-only list prints exactly 'verdict=board-only' on stdout (stdout='$_OUT')"
  _assert_true "$(! _stderr_has_verdict && printf 1 || printf 0)" \
    "case 1: the verdict token does not leak onto stderr"
  printf '  stderr: %s\n' "$(printf '%s' "$_ERR" | tr '\n' '|')"

  # ── CASE 2: source-bearing (plain and --strict-verify) ──────────────────────
  _run "$source_list"
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "case 2: a source list prints exactly 'verdict=source-bearing' on stdout (stdout='$_OUT')"
  _assert_true "$([ "$_RC" -eq 0 ] && printf 1 || printf 0)" \
    "case 2: a source list WITHOUT --strict-verify still exits 0 (rc=$_RC) — the verdict is informational"
  printf '%s' "$_ERR" | grep -q 'source-bearing: internal/relay/handler.go' && _assert_true 1 \
    "case 2: stderr names the source-bearing path that decided the verdict" || \
    _assert_true 0 "case 2: stderr names the source-bearing path that decided the verdict (stderr='$_ERR')"

  _run "$source_list" --strict-verify
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "case 2 (--strict-verify): the verdict is still printed before the refusal (stdout='$_OUT')"
  _assert_true "$([ "$_RC" -eq 1 ] && printf 1 || printf 0)" \
    "case 2 (--strict-verify): a source-bearing diff is REFUSED with exit 1 (rc=$_RC)"

  # ── CASE 3: source-bearing WITH --allow-source ──────────────────────────────
  _run "$source_list" --allow-source
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "case 3 (--allow-source): the verdict is still source-bearing (stdout='$_OUT')"
  _assert_true "$([ "$_RC" -eq 0 ] && printf 1 || printf 0)" \
    "case 3 (--allow-source): the explicit opt-in exits 0 (rc=$_RC)"

  _run "$source_list" --allow-source --strict-verify
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "case 3 (both flags): --allow-source wins and the verdict is still source-bearing (stdout='$_OUT')"
  _assert_true "$([ "$_RC" -eq 0 ] && printf 1 || printf 0)" \
    "case 3 (both flags): the explicitly-stated opt-in exits 0 (rc=$_RC)"

  # ── CASE 4: empty stdin is refused in EVERY flag combination ────────────────
  local -a combos=("" "--strict-verify" "--allow-source" "--strict-verify --allow-source")
  local combo=""
  for combo in "${combos[@]}"; do
    # shellcheck disable=SC2086 # deliberate word splitting of the flag combo
    _run "$empty_list" $combo
    _assert_true "$([ "$_RC" -eq 3 ] && printf 1 || printf 0)" \
      "case 4 (flags='${combo:-<none>}'): empty stdin exits 3 (rc=$_RC)"
    _assert_true "$([ -z "$_OUT" ] && printf 1 || printf 0)" \
      "case 4 (flags='${combo:-<none>}'): no verdict line is printed for a refusal (stdout='$_OUT')"
    printf '%s' "$_ERR" | grep -q 'error: empty diff — refusing to classify' && _assert_true 1 \
      "case 4 (flags='${combo:-<none>}'): the refusal line names the reason" || \
      _assert_true 0 "case 4 (flags='${combo:-<none>}'): the refusal line names the reason (stderr='$_ERR')"
  done

  # a whitespace-only/blank-only list is the same refusal (no path at all)
  _run "$blank_list"
  _assert_true "$([ "$_RC" -eq 3 ] && printf 1 || printf 0)" \
    "case 4 (blank-only list): a list of blank lines is the empty-diff refusal (rc=$_RC)"

  # ── CASE 5: unknown flag -> exit 2 with usage ───────────────────────────────
  _run "$board_list" --definitely-not-a-flag
  _assert_true "$([ "$_RC" -eq 2 ] && printf 1 || printf 0)" \
    "case 5: an unknown flag exits 2 (rc=$_RC)"
  printf '%s' "$_ERR" | grep -q "unknown option '--definitely-not-a-flag'" && _assert_true 1 \
    "case 5: the misuse message names the offending flag" || \
    _assert_true 0 "case 5: the misuse message names the offending flag (stderr='$_ERR')"
  printf '%s' "$_ERR" | grep -q '^Usage:' && _assert_true 1 \
    "case 5: a usage line is printed on misuse" || \
    _assert_true 0 "case 5: a usage line is printed on misuse (stderr='$_ERR')"
  _assert_true "$([ -z "$_OUT" ] && printf 1 || printf 0)" \
    "case 5: no verdict line is printed on misuse (stdout='$_OUT')"

  # a positional argument is misuse too (the list is stdin, never argv)
  _run "$board_list" 'some/path.go'
  _assert_true "$([ "$_RC" -eq 2 ] && printf 1 || printf 0)" \
    "case 5 (positional): a positional argument exits 2 (rc=$_RC)"

  # ── CASE 6: mixed list -> source-bearing ────────────────────────────────────
  _run "$mixed_list"
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "case 6: a mixed list (docs + one .go file) is source-bearing (stdout='$_OUT')"
  _run "$mixed_list" --strict-verify
  _assert_true "$([ "$_RC" -eq 1 ] && printf 1 || printf 0)" \
    "case 6: one .go file is enough to refuse the gate (rc=$_RC)"

  # ── boundary rows: each decision asserted in the conservative direction ─────
  _run "$specs_list"
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "boundary: specs/AGENT-ECOSYSTEM.md is SOURCE despite the *.md rule (stdout='$_OUT')"
  _run "$openapi_list"
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "boundary: docs/openapi.yaml is SOURCE (openapi yaml) (stdout='$_OUT')"
  _run "$script_list"
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "boundary: scripts/lib/judge-diff-class.sh is SOURCE despite the .sh/self path (stdout='$_OUT')"
  _run "$nested_df_list"
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "boundary: a NESTED Dockerfile is SOURCE (only the root build files are non-source) (stdout='$_OUT')"
  _run "$unknown_list"
  _assert_true "$(_stdout_is source-bearing && printf 1 || printf 0)" \
    "boundary: an unrecognized path is SOURCE, never a silent board-only (stdout='$_OUT')"
  _run "$nonsource_list"
  _assert_true "$(_stdout_is board-only && printf 1 || printf 0)" \
    "boundary: a rich non-source list (root Makefile/Dockerfiles, workflow YAML, LICENSE, NOTICE, .gitignore, .gitattributes, README.md, .gitreins history, .coding-hermes evidence) is board-only (stdout='$_OUT')"
  printf '  stderr: %s\n' "$(printf '%s' "$_ERR" | tr '\n' '|')"

  # duplicate folding: the summary must count UNIQUE paths (2, not 3)
  _run "$dup_list"
  _assert_true "$(_stdout_is board-only && printf 1 || printf 0)" \
    "boundary: duplicate paths still classify board-only (stdout='$_OUT')"
  printf '%s' "$_ERR" | grep -q '(3 unique path(s)' && _assert_true 0 \
    "boundary: duplicate paths are folded — the summary must NOT count 3 unique paths (stderr='$_ERR')" || \
    _assert_true 1 "boundary: duplicate paths are folded to the real unique count"
  printf '%s' "$_ERR" | grep -q '(2 unique path(s)' && _assert_true 1 \
    "boundary: the summary names the real unique count (2)" || \
    _assert_true 0 "boundary: the summary names the real unique count (2) (stderr='$_ERR')"

  # CRLF tolerance: a trailing CR must not turn a path into a source-bearing one
  _run "$crlf_list"
  _assert_true "$(_stdout_is board-only && printf 1 || printf 0)" \
    "boundary: a CRLF path list still classifies (a trailing CR is stripped) (stdout='$_OUT')"
  printf '%s' "$_ERR" | grep -q '(2 unique path(s)' && _assert_true 1 \
    "boundary: the CRLF list is counted as 2 unique paths" || \
    _assert_true 0 "boundary: the CRLF list is counted as 2 unique paths (stderr='$_ERR')"

  # ── CASE 7: THE NEUTER PROOF ────────────────────────────────────────────────
  # A copy of the classifier whose verdict call site is forced to the literal
  # `board-only` must FAIL the selftest's own source-bearing assertion — the same
  # assertion case 2 used. Guarded: the pattern must be present exactly once on
  # self before sedding, and the copy must actually differ afterwards.
  local neutered="$_TMP/neutered-board-only.sh"
  local call_sites=0
  call_sites="$(grep -c -F -x "$VERDICT_CALL" "$CLASSIFIER" || true)"
  _assert_true "$([ "$call_sites" -eq 1 ] && printf 1 || printf 0)" \
    "case 7 guard: the verdict call site is present exactly once in the classifier (found $call_sites) — the neuter sed cannot be vacuous"

  cp "$CLASSIFIER" "$neutered"
  sed -i.tmp -e 's|^  _emit_verdict "\$verdict"$|  _emit_verdict "board-only"|' "$neutered"
  rm -f "$neutered.tmp"
  if cmp -s "$CLASSIFIER" "$neutered"; then
    _bad "case 7 guard: the neuter sed no longer matches the verdict call site ('$VERDICT_CALL') — the causality proof would be vacuous"
  else
    _ok "case 7 guard: the neutered copy differs from the real classifier"

    # (a) the forced copy reports board-only for the SOURCE fixture ...
    _run_classifier "$neutered" "$source_list" --strict-verify
    _assert_true "$(_stdout_is board-only && printf 1 || printf 0)" \
      "case 7: the verdict-forced copy prints 'verdict=board-only' for the source list (the sed is load-bearing) (stdout='$_OUT')"
    # (b) ... which makes the selftest's own case-2 assertion FAIL on it, and the
    #     harness reports that per-case boolean, exactly as it does for case 2.
    local case2_ok=0
    if _stdout_is source-bearing && [ "$_RC" -eq 1 ]; then
      case2_ok=1
    fi
    _assert_true "$([ "$case2_ok" -eq 0 ] && printf 1 || printf 0)" \
      "case 7: the verdict-forced copy FAILS the selftest's source-bearing assertion (verdict='$_OUT', rc=$_RC where the real script gives 'verdict=source-bearing', rc=1) — the assertions are driven by the real verdict output"

    # (c) control: the neuter does not change a board-only verdict.
    _run_classifier "$neutered" "$board_list"
    _assert_true "$(_stdout_is board-only && printf 1 || printf 0)" \
      "case 7 control: the neutered copy still reports board-only for a board-only list (stdout='$_OUT')"
    _assert_true "$([ "$_RC" -eq 0 ] && printf 1 || printf 0)" \
      "case 7 control: the neutered copy exits 0 on a board-only list (rc=$_RC)"

    # (d) and the empty-diff refusal is independent of the emitter, so it must
    #     survive the neuter — otherwise the two properties would be entangled.
    _run_classifier "$neutered" "$empty_list"
    _assert_true "$([ "$_RC" -eq 3 ] && printf 1 || printf 0)" \
      "case 7 control: the neutered copy still refuses an empty diff with exit 3 (rc=$_RC)"

    # (e) THE STRONGEST FORM: run THIS selftest against the verdict-forced
    #     classifier. The selftest resolves the classifier as
    #     <its own dir>/judge-diff-class.sh, so a scratch copy of the PAIR makes a
    #     copy of the selftest test the neutered classifier. It MUST fail — a
    #     verdict-forced classifier can never pass this selftest.
    local pair="$_TMP/pair"
    mkdir -p "$pair"
    cp "$CLASSIFIER" "$pair/judge-diff-class.sh"
    cp "$SELF" "$pair/judge-diff-class-selftest.sh"
    sed -i.tmp -e 's|^  _emit_verdict "\$verdict"$|  _emit_verdict "board-only"|' "$pair/judge-diff-class.sh"
    rm -f "$pair/judge-diff-class.sh.tmp"
    if cmp -s "$CLASSIFIER" "$pair/judge-diff-class.sh"; then
      _bad "case 7(e) guard: the pair copy's classifier was not neutered — the selftest-against-neuter proof would be vacuous"
    else
      local pair_out="" pair_rc=0
      pair_out="$(bash "$pair/judge-diff-class-selftest.sh" 2>&1)"
      pair_rc=$?
      _assert_true "$([ "$pair_rc" -ne 0 ] && printf 1 || printf 0)" \
        "case 7(e): the selftest run against the verdict-forced classifier FAILS (rc=$pair_rc) — a verdict-forced-success classifier cannot pass this selftest"
      printf '%s' "$pair_out" | grep -q 'checks behaved — FAIL' && _assert_true 1 \
        "case 7(e): that failing run reports its own FAIL summary" || \
        _assert_true 0 "case 7(e): that failing run reports its own FAIL summary"
      printf '%s' "$pair_out" | grep -qF "FAIL: case 2: a source list prints exactly 'verdict=source-bearing' on stdout" && _assert_true 1 \
        "case 7(e): the failure is attributed to the source-bearing verdict assertion itself, not to an unrelated check" || \
        _assert_true 0 "case 7(e): the failure is attributed to the source-bearing verdict assertion (tail: $(printf '%s' "$pair_out" | tail -4 | tr '\n' '|'))"
    fi
  fi

  # ── verdict ─────────────────────────────────────────────────────────────────
  if [ "$_fails" -ne 0 ]; then
    printf '%s: %d/%d checks behaved — FAIL\n' "$PROG" "$((_checks - _fails))" "$_checks" >&2
    return 1
  fi
  printf '%s: %d/%d checks behaved\n' "$PROG" "$_checks" "$_checks"
  return 0
}

_selftest
exit $?
