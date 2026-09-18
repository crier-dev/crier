#!/usr/bin/env bash
#
# scripts/check-gofmt.sh — gofmt formatting gate (DF-CRIER-189).
#
# WHY THIS EXISTS
# ---------------
# The commit gate is the gitreins pre-commit hook and its Tier-1 battery is
# secrets / go_build / go_lint / go_tests. go_lint is `go vet`, which reports
# suspicious constructs — it says NOTHING about formatting — so no arm anywhere
# read formatting, and a drifting file could be committed on a green that
# verified nothing about it. Measured on this tree at HEAD 973f5e1, BEFORE this
# script existed:
#
#     $ gofmt -l $(git ls-files '*.go')
#     internal/guard/types.go
#     internal/webhook/schema.go
#
# while `gitreins guard` printed
#
#     Tier 1 Guards: PASS  (test mode: full)
#
# A whole-repo "gofmt clean" sweep was permanently red and nothing in the gate
# could see it. This is the THIRD arm of the gate, after DF-CRIER-206's
# shell/YAML arm and DF-CRIER-209's Makefile/Dockerfile arm: it takes an
# explicit file list (default: every tracked .go file) and fails closed.
#
# USAGE
# -----
#   bash scripts/check-gofmt.sh                  # default: all tracked .go files
#   bash scripts/check-gofmt.sh FILE [FILE...]   # exactly the files you name
#   bash scripts/check-gofmt.sh --selftest
#
# WHAT IS CHECKED
# ---------------
#   gofmt  every tracked .go file -> `gofmt -l <file>`. A file whose path comes
#          back on stdout has formatting drift and is REJECTED, with its
#          `gofmt -d` diff printed under the FAIL line (bounded — see below).
#          The resolved gofmt path AND the Go toolchain version are printed on
#          every run, so the evidence is attributable to a tool.
#
# THE VERDICT IS THE OUTPUT, NOT THE EXIT STATUS (measured)
#   `gofmt -l <drifting file>` exits 0 while printing the path, so an arm that
#   trusted the return code would PASS every drifting file — the exact defect
#   class this script exists to close. Only a source gofmt cannot PARSE makes it
#   exit nonzero (rc 2 + a `file:line: message` on stderr); that is reported as a
#   rejection too, carrying gofmt's own message.
#
# gofmt IS NOT A LINTER
#   It reports formatting only. It does not replace `go vet` (Tier-1 go_lint), it
#   says nothing about correctness or style, and it reads only tracked .go files.
#
# BOUNDED FAILURE OUTPUT
#   A failure prints the first ${CHECK_GOFMT_DIFF_LIMIT:-40} lines of the file's
#   `gofmt -d` diff plus a truncation note, so one drifting file cannot bury the
#   rest of the log; the fix line names the exact command (`gofmt -w <file>`).
#
# EXPLICIT FILE LISTS FAIL CLOSED (mirrors DF-CRIER-208 / DF-CRIER-209)
# --------------------------------------------------------------------
# The default (no FILE arguments) mode is scope-tolerant about what it FINDS:
# it lists the tracked .go files itself, so nothing it names is unexpected.
#
# An explicit file list is different: the caller ASSERTED that every named path
# is something to check. Two shapes make that assertion false, and both are
# rejected (exit 1, every path named) instead of ending in a green:
#   (1) a list in which NOTHING classifies as a .go file — the run verified
#       nothing, so `PASS — 0 file(s) checked` is never printed for it;
#   (2) a named .go path that does not exist — the caller claimed there was
#       something to check there.
# A MIXED list that carries at least one .go file keeps the historic
# note-and-continue behaviour for its unclassifiable entries. That is the
# property the tracked pre-commit wrapper (scripts/hooks/pre-commit) relies on:
# it passes exactly the files its own copy of the same predicate already
# classified, so neither rule above is reachable from the hook path.
#
# THE ZERO-FILE RULE (the DF-CRIER-208 defect class)
# --------------------------------------------------
# A DEFAULT run whose .go scope comes out EMPTY (no tracked .go file at all, or
# none of them present in the worktree) exits 2 with a message and prints no
# PASS line. A green over zero checked files is the blank green this arm exists
# to remove, so it is refused rather than reported.
#
# EXIT CODES
#   0  every file in scope passed
#   1  at least one file was rejected — which also covers a NAMED file that is
#      missing/unreadable, and an explicit file list whose assertion cannot hold
#      (nothing classified as .go)
#   2  misuse (bad option) or a missing dependency (gofmt is not on PATH), and a
#      DEFAULT run with an empty .go scope (see THE ZERO-FILE RULE)
#
# A tracked .go file that is ABSENT from the worktree is named in a note and left
# out of the counts (no content means no formatting to verify) — but a file NAMED
# on the command line that does not exist is a hard error (exit 1).
#
# OUTPUT CONTRACT (grep-able)
#   `gofmt: <path> (<go toolchain version>)`                    the tool identity
#   `PASS  gofmt     <file>` / `FAIL  gofmt     <file>` per checked file
#   `scope: N go file(s) verified`                              (all passed)
#   `scope: N go file(s) in scope — K rejected`
#   `PASS — N file(s) checked, 0 rejected`
#   The counts are real counts of files actually checked in this run.
#
# SELFTEST — used by `make gofmt-selftest`; it builds its fixtures in a scratch
# dir under ${TMPDIR:-/tmp} and asserts, by running THIS script on them:
#   1. a clean .go fixture is ACCEPTED with a real count line
#   2. a drifting but valid .go fixture is REJECTED, the path is NAMED, and its
#      diff is shown
#   3. the NEUTER proof: a copy of this script whose arm verdict call is forced
#      to success must ACCEPT that same drifting fixture
#   4. an explicit list in which NOTHING classifies as a .go file is REJECTED
#      with the paths named and no PASS line printed
#   5. an explicit list naming a NONEXISTENT .go file is REJECTED and names it
#   6. a MIXED list (one clean .go + one non-.go file) checks the .go file and
#      notes the other as not checked
#   7. a list of only clean .go files PASSES with the real count
#   8. gofmt hidden from PATH is exit 2 naming the tool and the mode — a missing
#      validator is never a silent skip
#   9. a DEFAULT run over a checkout with no tracked .go file exits nonzero with
#      a message and NO green (THE ZERO-FILE RULE)
#   `make gofmt-selftest` exits 0 only when all of them behaved.
#
# DEPENDENCIES: bash, git (only for the default file list), grep/sed/head/wc/awk/
# mktemp; plus gofmt itself, which is the validator this arm cannot run without.

set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac

PROG="check-gofmt"
GOFMT_BIN=""
GOFMT_DESC=""
DIFF_LIMIT="${CHECK_GOFMT_DIFF_LIMIT:-40}"

# ── output helpers ────────────────────────────────────────────────────────────
_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_info() { printf '%s: %s\n' "$PROG" "$*"; }

# ── file classification ───────────────────────────────────────────────────────

# Go: the .go suffix is the whole rule (a Go file that does not compile is still
# a Go file — gofmt, go_build and go vet each get to say their own thing about it).
_is_go() {
  case "$1" in *.go) return 0 ;; esac
  return 1
}

# ── the check ─────────────────────────────────────────────────────────────────

# CHECK_DIAG carries the rejected file's tool output up to the reporter, so the
# report reads `FAIL <file>` first and gofmt's own evidence underneath it.
CHECK_DIAG=""

# _check_gofmt <file> — returns 0 when the file is gofmt-clean, nonzero when it
# drifts (CHECK_DIAG = bounded diff) or when gofmt cannot read/parse it
# (CHECK_DIAG = gofmt's message).
_check_gofmt() {
  local f="$1" listed="" rc=0 diff="" lines=0
  # `gofmt -l` PRINTS the path of every file it would rewrite and exits 0 doing
  # it — so the listed output is the verdict and the exit code is not (only an
  # unparseable source makes gofmt exit nonzero).
  listed="$("$GOFMT_BIN" -l "$f" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    CHECK_DIAG="$listed"
    return 1
  fi
  [ -n "$listed" ] || return 0
  diff="$("$GOFMT_BIN" -d "$f" 2>&1)"
  lines="$(printf '%s\n' "$diff" | wc -l | tr -d ' ')"
  if [ "$lines" -gt "$DIFF_LIMIT" ]; then
    CHECK_DIAG="$(printf '%s\n' "$diff" | head -n "$DIFF_LIMIT")
... (diff truncated: showing the first $DIFF_LIMIT of $lines line(s) — run: gofmt -d $f)"
  else
    CHECK_DIAG="$diff"
  fi
  return 1
}

# ── validator resolution + version attribution ────────────────────────────────

# resolve_gofmt <mode-description> -> sets GOFMT_BIN / GOFMT_DESC, or returns 2.
# The mode is named in the error so a log reader can tell which run could not
# verify what (a missing validator is NEVER a silent skip).
resolve_gofmt() {
  local mode="$1" go_ver=""
  if ! command -v gofmt >/dev/null 2>&1; then
    _err "gofmt is not on PATH — refusing to skip the formatting check."
    _err "  mode: $mode"
    _err "  install the Go toolchain (gofmt ships with it) and re-run."
    return 2
  fi
  GOFMT_BIN="$(command -v gofmt)"
  if command -v go >/dev/null 2>&1; then
    go_ver="$(go version 2>/dev/null | awk '{print $3}')"
  fi
  [ -n "$go_ver" ] || go_ver="go toolchain version not resolvable (gofmt has no -version flag)"
  GOFMT_DESC="gofmt $GOFMT_BIN ($go_ver)"
  return 0
}

# ── main check ────────────────────────────────────────────────────────────────

usage() {
  cat <<'EOF'
check-gofmt.sh — gofmt formatting gate (DF-CRIER-189)

Usage:
  bash scripts/check-gofmt.sh [FILE...]
  bash scripts/check-gofmt.sh --selftest

With no FILE arguments every tracked .go file is considered (git ls-files '*.go')
and the real counts are printed:

  scope: 152 go file(s) verified

An explicit FILE list fails closed: a named .go path that does not exist, or a
list in which nothing classifies as a .go file, is rejected (exit 1) with the
paths named — a list that verified nothing never prints a PASS.

Exit codes: 0 all passed, 1 at least one rejected, 2 misuse/missing gofmt/empty
default scope. A drifting file is reported with its `gofmt -d` diff (bounded) and
the fix line `gofmt -w <file>`.
EOF
}

run_check() { # <files...>
  local -a files=("$@")
  local -a go_files=() other_files=()
  local from_default=0 repo_root="" tracked="" n_tracked=0
  local f rc=0 fails=0 checked=0 missing=0

  repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || repo_root=""

  if [ "${#files[@]}" -eq 0 ]; then
    from_default=1
    command -v git >/dev/null 2>&1 || {
      _err "git is required to derive the default (tracked) file list."
      return 2
    }
    [ -n "$repo_root" ] || {
      _err "not inside a git checkout — pass an explicit file list instead."
      return 2
    }
    tracked="$(git -C "$repo_root" ls-files '*.go')" || {
      _err "git ls-files '*.go' failed in $repo_root"
      return 2
    }
    [ -n "$tracked" ] || {
      _err "git ls-files '*.go' returned no tracked .go file — refusing to report a vacuous PASS over 0 files checked."
      return 2
    }
    while IFS= read -r f; do
      [ -n "$f" ] || continue
      files+=("$repo_root/$f")
      n_tracked=$((n_tracked + 1))
    done <<<"$tracked"
  fi

  # Classify. Nothing is skipped silently:
  #   - a NAMED file that is missing or unreadable is a FAILURE (the caller
  #     asserted it should be checked),
  #   - in the default (whole-repo) mode a TRACKED file that is missing from the
  #     worktree has no content to check — it is named in a note, excluded from
  #     the scope counts, and does not fail the run (a file that does not exist
  #     cannot carry formatting drift; the deletion itself is the git diff's
  #     business),
  #   - an UNREADABLE file is a failure in both modes: content exists and this
  #     checker cannot verify it.
  local -a missing_tracked_list=()
  for f in "${files[@]}"; do
    if [ ! -e "$f" ]; then
      if [ "$from_default" -eq 1 ]; then
        missing_tracked_list+=("$f")
      else
        _err "named file does not exist: $f (refusing to skip it silently)"
        missing=$((missing + 1))
      fi
      continue
    fi
    if [ ! -r "$f" ]; then
      _err "file is not readable: $f (refusing to skip it silently)"
      missing=$((missing + 1))
      continue
    fi
    if _is_go "$f"; then
      go_files+=("$f")
    else
      other_files+=("$f")
    fi
  done

  if [ "$from_default" -eq 1 ]; then
    _info "scope source: git ls-files '*.go' ($n_tracked tracked .go file(s)) from $repo_root"
    if [ "${#missing_tracked_list[@]}" -gt 0 ]; then
      local shown=0
      printf '%s: note: %d tracked .go file(s) are absent from the worktree and were not checked (no content = no formatting to verify): %s' \
        "$PROG" "${#missing_tracked_list[@]}" "$repo_root/" >&2
      for f in "${missing_tracked_list[@]}"; do
        shown=$((shown + 1))
        [ "$shown" -le 10 ] && printf '%s ' "${f#"$repo_root/"}" >&2
      done
      [ "${#missing_tracked_list[@]}" -gt 10 ] && printf '(+%d more)' "$((${#missing_tracked_list[@]} - 10))" >&2
      printf '\n' >&2
    fi
    # THE ZERO-FILE RULE: a default run that ends up with nothing in scope is a
    # refusal, not a green (see the header). A green over 0 checked files is the
    # blank green this arm exists to remove.
    if [ "${#go_files[@]}" -eq 0 ]; then
      _err "default mode ($n_tracked tracked .go file(s) listed) left 0 .go file(s) in scope — refusing to report a vacuous PASS over 0 files checked."
      return 2
    fi
  fi

  # ── fail-closed rule for an EXPLICIT file list (mirrors DF-CRIER-208) ────────
  # The default mode names nothing, so nothing it lists is unexpected. An
  # explicit list is a caller ASSERTION that every named path is something to
  # check; a list in which nothing classified as a .go file makes that assertion
  # false and is refused here instead of ending in a green, because
  # `PASS — 0 file(s) checked` is precisely the blank green this script exists to
  # remove (DF-CRIER-206 / DF-CRIER-208). A MIXED list carrying at least one .go
  # file keeps the historic note-and-continue treatment for its unclassifiable
  # entries: the tracked pre-commit wrapper passes exactly the files it already
  # classified with this same predicate, so the rule is unreachable from there.
  local policy_rejected=0
  if [ "$from_default" -eq 0 ]; then
    local -a implicated=()
    if [ "${#go_files[@]}" -eq 0 ] && [ "${#other_files[@]}" -gt 0 ]; then
      implicated+=("${other_files[@]}")
      _err "explicit file list: none of the ${#files[@]} named file(s) is a .go file — refusing to report an empty green over a list it verified nothing in:"
      for f in "${other_files[@]}"; do
        _err "  not a .go file: $f"
      done
    fi
    policy_rejected="${#implicated[@]}"
  fi

  # Resolve + announce the validator BEFORE checking, so the evidence is
  # attributable even if a later check aborts. Announced unconditionally when a
  # .go file is in scope; with nothing in scope (default mode) the zero-file rule
  # above has already refused, and an explicit list that asserted nothing has
  # already been rejected — neither needs a tool to say so.
  if [ "${#go_files[@]}" -gt 0 ]; then
    resolve_gofmt "$([ "$from_default" -eq 1 ] && printf 'default (tracked .go files)' || printf 'explicit file list')" || return 2
    _info "$GOFMT_DESC"
    _info "diff budget: the first $DIFF_LIMIT line(s) of each drifting file's diff (override with CHECK_GOFMT_DIFF_LIMIT)"
  fi

  for f in "${go_files[@]}"; do
    checked=$((checked + 1))
    # NEUTER-MARK[gofmt-arm-verdict]: the gofmt arm's verdict call. The selftest
    # seds exactly this line (and asserts the copy changed) to prove a rejected
    # fixture is rejected BY THIS ARM and not by accident.
    if ! _check_gofmt "$f"; then
      printf 'FAIL  gofmt     %s\n' "$f"
      [ -n "$CHECK_DIAG" ] && printf '%s\n' "$CHECK_DIAG" | sed 's/^/      /'
      _err "gofmt formatting drift: $f"
      _err "  fix: gofmt -w $f"
      fails=$((fails + 1))
    else
      printf 'PASS  gofmt     %s\n' "$f"
    fi
  done

  # Anything handed to the checker that is not a .go file: say so out loud (a
  # silent skip is the defect class this script exists to close). In explicit-list
  # mode this note-and-continue is kept ONLY for a list that still carries at
  # least one .go file — when the list asserted something uncheckable the failure
  # above already named those paths, so a second "not checked" note would
  # contradict it.
  if [ "${#other_files[@]}" -gt 0 ] && [ "$policy_rejected" -eq 0 ]; then
    local shown=0
    printf 'note: %d supplied file(s) are not .go files (not checked):' "${#other_files[@]}" >&2
    for f in "${other_files[@]}"; do
      shown=$((shown + 1))
      if [ "$shown" -le 10 ]; then
        printf ' %s' "$f" >&2
      fi
    done
    if [ "${#other_files[@]}" -gt 10 ]; then
      printf ' (+%d more)' "$((${#other_files[@]} - 10))" >&2
    fi
    printf '\n' >&2
  fi

  if [ "$fails" -eq 0 ] && [ "$missing" -eq 0 ] && [ "$policy_rejected" -eq 0 ]; then
    _info "scope: ${#go_files[@]} go file(s) verified"
    _info "PASS — $checked file(s) checked, 0 rejected"
    return 0
  fi

  _info "scope: ${#go_files[@]} go file(s) in scope — $((fails + missing + policy_rejected)) rejected"
  _err "FAIL — $checked file(s) checked, $((fails + missing + policy_rejected)) rejected"
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

_selftest() {
  local fails=0 checks=0
  # The scratch dir is held in a GLOBAL (not a local) so the EXIT trap can still
  # read it after this function returns — a trap that quotes a function-local
  # under `set -u` dies with "unbound variable" and leaks the directory.
  _SELFTEST_TMP="$(mktemp -d "${TMPDIR:-/tmp}/check-gofmt-selftest.XXXXXX")" || {
    _err "selftest: mktemp -d failed"
    return 1
  }
  local tmp="$_SELFTEST_TMP"
  trap '_selftest_cleanup' EXIT

  # clean .go fixtures (must be ACCEPTED) — gofmt-clean by construction, and the
  # selftest would fail check 1 if one of them were not.
  printf 'package selftest\n\ntype Clean struct {\n\tName string\n\tSize int\n}\n' >"$tmp/clean.go"
  printf 'package selftest\n\nfunc Answer() int {\n\treturn 42\n}\n' >"$tmp/clean2.go"

  # drifting but syntactically VALID .go fixture (must be REJECTED): misaligned
  # struct fields, a comment out of alignment, and space-indented statements.
  cat >"$tmp/drift.go" <<'GO'
package selftest

type Drift struct {
	Name string
	LongerField int
	Count      int
}

func DriftTotal() int {
    x := 1
   if x > 0 {
		return x
	}
	return 0
}
GO

  # a fixture that classifies as neither (mixed-list / fail-closed cases)
  printf 'a plain text file: not a .go source file\n' >"$tmp/notes.md"

  # an explicit-list path that does not exist
  local missing_go="$tmp/does-not-exist.go"

  local out="" out2="" rc=0 rc2=0

  # 1. a clean .go fixture is ACCEPTED with a real count line
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/clean.go" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: the clean .go fixture was REJECTED (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'PASS  gofmt     '"$tmp"'/clean.go'; then
    printf '%s selftest: FAIL: the clean run did not report the file it checked\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'PASS — 1 file(s) checked, 0 rejected'; then
    printf '%s selftest: FAIL: the clean run did not report a real count line\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'gofmt '; then
    printf '%s selftest: FAIL: the clean run did not name the gofmt tool it used\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a clean .go fixture is accepted (rc=%d) with the real count line and the tool identity\n' "$rc"
  fi

  # 2. a drifting (valid Go) fixture is REJECTED, NAMED, with its diff shown
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/drift.go" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: a drifting .go fixture was ACCEPTED (rc=0)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'FAIL  gofmt     '"$tmp"'/drift.go'; then
    printf '%s selftest: FAIL: the drifting fixture was rejected but the report did not name it\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -qE '^ +@@ '; then
    printf '%s selftest: FAIL: the drifting fixture was rejected without showing a diff line\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'gofmt -w '"$tmp"'/drift.go'; then
    printf '%s selftest: FAIL: the drifting fixture was rejected without naming the fix\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a drifting .go fixture is rejected (rc=%d), named, and its diff is shown\n' "$rc"
  fi

  # 3. NEUTER PROOF: force the arm's verdict call to success in a copy of this
  #    script and re-run the SAME drifting fixture — it must now be ACCEPTED,
  #    which is what proves the rejection above is caused by that arm.
  checks=$((checks + 1))
  local neutered="$tmp/neutered-gofmt-arm.sh"
  cp "$SELF" "$neutered"
  sed -i.tmp -e 's|^\([[:space:]]*\)if ! _check_gofmt "\$f"; then$|\1if false; then|' "$neutered"
  rm -f "$neutered.tmp"
  if cmp -s "$SELF" "$neutered"; then
    printf '%s selftest: FAIL: the neuter sed no longer matches the gofmt arm verdict line (NEUTER-MARK[gofmt-arm-verdict]) — the causality proof would be vacuous\n' "$PROG" >&2
    fails=$((fails + 1))
  else
    out="$(bash "$neutered" "$tmp/drift.go" 2>&1)"
    rc=$?
    if [ "$rc" -ne 0 ]; then
      printf '%s selftest: FAIL: NEUTER PROOF: with the arm neutered the same drifting .go fixture is STILL rejected (rc=%d) — the rejection does not come from that arm\n  output: %s\n' "$PROG" "$rc" "$out" >&2
      fails=$((fails + 1))
    else
      printf 'PASS: NEUTER PROOF: neutered copy differs from the original and ACCEPTS the same drifting .go fixture (rc=0) — the rejection is caused by that arm\n'
    fi
  fi

  # 4. an explicit list in which NOTHING classifies is REJECTED, names the path,
  #    and prints no green at all
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/notes.md" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: an explicit list in which nothing classifies was ACCEPTED (rc=0)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "not a .go file: $tmp/notes.md"; then
    printf '%s selftest: FAIL: the unclassifiable list was rejected but the path was not named\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q 'PASS'; then
    printf '%s selftest: FAIL: the unclassifiable list was rejected but a PASS line was printed anyway\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: an explicit list in which nothing classifies is rejected (rc=%d) with the path named and no green printed\n' "$rc"
  fi

  # 5. an explicit list naming a NONEXISTENT .go file is REJECTED and names it
  checks=$((checks + 1))
  out="$(bash "$SELF" "$missing_go" 2>&1)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    printf '%s selftest: FAIL: an explicit list naming a nonexistent .go file was ACCEPTED (rc=0)\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q "named file does not exist: $missing_go"; then
    printf '%s selftest: FAIL: the nonexistent named .go file was rejected but not named\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif printf '%s' "$out" | grep -q 'PASS'; then
    printf '%s selftest: FAIL: the nonexistent named .go file was rejected but a PASS line was printed anyway\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: an explicit list naming a nonexistent .go file is rejected (rc=%d) and names it\n' "$rc"
  fi

  # 6. a MIXED list (1 clean .go + 1 non-.go) checks the .go file and NOTES the
  #    other as not checked — the shape the pre-commit wrapper produces
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/clean.go" "$tmp/notes.md" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: a mixed explicit list (1 clean .go + 1 plain file) was REJECTED (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'PASS  gofmt     '"$tmp"'/clean.go'; then
    printf '%s selftest: FAIL: the mixed list passed without reporting the .go file it checked\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'not .go files (not checked): '"$tmp"'/notes.md'; then
    printf '%s selftest: FAIL: the mixed list passed without noting the non-.go entry it did not check\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'scope: 1 go file(s) verified'; then
    printf '%s selftest: FAIL: the mixed list passed without a real scope line\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a mixed list (1 clean .go + 1 non-.go file) is accepted (rc=%d), checks the .go file and notes the other\n' "$rc"
  fi

  # 7. a list of only clean .go files PASSES with the real count
  checks=$((checks + 1))
  out="$(bash "$SELF" "$tmp/clean.go" "$tmp/clean2.go" 2>&1)"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    printf '%s selftest: FAIL: a list of two clean .go files was REJECTED (rc=%d)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'scope: 2 go file(s) verified'; then
    printf '%s selftest: FAIL: the two-clean-file run did not report the real scope count\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'PASS — 2 file(s) checked, 0 rejected'; then
    printf '%s selftest: FAIL: the two-clean-file run did not report the real checked count\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: a list of only clean .go files passes (rc=%d) with the real count (2 checked)\n' "$rc"
  fi

  # ── missing-validator fail-closed (exit 2, the tool named, mode named) ───────
  # A PATH shim directory holding symlinks to everything this script needs EXCEPT
  # the tool being hidden — so the check proves the script's own resolution path,
  # not a broken environment.
  local shim="$tmp/shim-bin"
  mkdir -p "$shim"
  local b p
  for b in sh bash dash env git grep sed awk cat rm mkdir mktemp dirname basename cmp printf ls chmod head tr uname cut sort wc; do
    p="$(command -v "$b" 2>/dev/null)" && ln -sf "$p" "$shim/$b"
  done

  # 8. no gofmt at all -> exit 2 naming the tool and the mode
  checks=$((checks + 1))
  out="$(env PATH="$shim" bash "$SELF" "$tmp/clean.go" 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ]; then
    printf '%s selftest: FAIL: with gofmt hidden the run exited %d, not 2 (fail-closed)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'gofmt is not on PATH'; then
    printf '%s selftest: FAIL: with gofmt hidden the error did not name the missing tool\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  elif ! printf '%s' "$out" | grep -q 'mode: explicit file list'; then
    printf '%s selftest: FAIL: with gofmt hidden the error did not name the mode\n  output: %s\n' "$PROG" "$out" >&2
    fails=$((fails + 1))
  else
    printf 'PASS: with gofmt hidden the run exits 2 and names both the missing tool and the mode (rc=%d)\n' "$rc"
  fi

  # 9. THE ZERO-FILE RULE: a DEFAULT run over a checkout with no tracked .go file
  #    is nonzero with a message and NO green (the DF-CRIER-208 defect class)
  checks=$((checks + 1))
  local empty_repo="$tmp/empty-repo"
  mkdir -p "$empty_repo"
  if ! git -C "$empty_repo" init -q 2>/dev/null; then
    printf '%s selftest: FAIL: could not create the scratch git repo for the zero-file check\n' "$PROG" >&2
    fails=$((fails + 1))
  else
    out="$(cd "$empty_repo" && bash "$SELF" 2>&1)"
    rc=$?
    if [ "$rc" -eq 0 ]; then
      printf '%s selftest: FAIL: a default run with 0 tracked .go files was ACCEPTED (rc=0)\n  output: %s\n' "$PROG" "$out" >&2
      fails=$((fails + 1))
    elif [ "$rc" -ne 2 ]; then
      printf '%s selftest: FAIL: the zero-file run exited %d, not 2 (the documented refusal code)\n  output: %s\n' "$PROG" "$rc" "$out" >&2
      fails=$((fails + 1))
    elif ! printf '%s' "$out" | grep -q 'no tracked .go file'; then
      printf '%s selftest: FAIL: the zero-file run did not say why it refused\n  output: %s\n' "$PROG" "$out" >&2
      fails=$((fails + 1))
    elif printf '%s' "$out" | grep -qE 'PASS — |PASS  gofmt'; then
      printf '%s selftest: FAIL: the zero-file run printed a PASS line over 0 files checked\n  output: %s\n' "$PROG" "$out" >&2
      fails=$((fails + 1))
    else
      printf 'PASS: a default run with an empty .go scope is refused (rc=%d) with a message and no green\n' "$rc"
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
  local -a files=()
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
      --)
        shift
        while [ "$#" -gt 0 ]; do
          files+=("$1")
          shift
        done
        ;;
      -*)
        _err "unknown option '$1' (try --help)"
        return 2
        ;;
      *)
        files+=("$1")
        shift
        ;;
    esac
  done

  if [ "$selftest" -eq 1 ]; then
    _selftest
    return $?
  fi

  case "$DIFF_LIMIT" in
    '' | *[!0-9]*)
      _err "CHECK_GOFMT_DIFF_LIMIT must be a positive integer (got '$DIFF_LIMIT')"
      return 2
      ;;
  esac

  run_check "${files[@]}"
}

main "$@"
exit $?
