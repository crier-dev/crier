#!/usr/bin/env bash
#
# scripts/check-board.sh — the foreman board validation gate (DF-CRIER-387).
#
# WHY THIS EXISTS
# ---------------
# `.coding-hermes/board/tasks.jsonl` + `events.jsonl` are the durable queue every
# foreman lane reads. The board has its own validator (`boardctl validate`), and
# the tracked commit hook ran it — and IGNORED ITS EXIT STATUS. Measured on
# 2026-10-19: `boardctl validate` printed
#
#     [error] tasks.jsonl line 551 (task INT-CI-010): status "closed" not in vocabulary …
#     RESULT: FAIL (5 error(s), 20 warning(s))
#
# and the commit that followed SUCCEEDED and was pushed to both mirrors. Five
# rows had carried a non-canonical status word for days, so every commit in that
# window was made over an invalid board while the gate read green. A validator
# whose failure does not stop the commit is a print, not a gate.
#
# USAGE
# -----
#   bash scripts/check-board.sh              # validate this repo's board
#   bash scripts/check-board.sh --staged-index # whole index, not worktree
#   bash scripts/check-board.sh -C DIR       # validate another board root
#   bash scripts/check-board.sh --selftest   # prove the checker still rejects
#
# WHAT IS CHECKED
# ---------------
#   boardctl validate -C <root>   the board's own canon: status vocabulary, key
#                                 shapes, event ids. ERRORS are fatal (exit 1);
#                                 WARNINGS are printed and NOT fatal, because the
#                                 validator itself tolerates legacy shapes and
#                                 says so per line.
#
# WHAT IS NOT CHECKED
# -------------------
#   * boardctl's OWN vocabulary. A status the validator rejects and the repo
#     considers legitimate is a boardctl bug, not something this arm can fix;
#     the fix belongs in the boardctl source (DF-CRIER-387 names that half).
#   * `.coding-hermes/` files that are not board data (lock files, notes).
#   * the semantic truth of a row: a well-formed row that lies about its own
#     verification is not detectable here.
#
# A MISSING VALIDATOR IS EXIT 2, NEVER A SILENT SKIP (same rule as the other
# arms of this gate): a board check that cannot run must say so loudly enough
# that the commit stops.

set -uo pipefail

PROG="check-board.sh"
tmp=""
INDEX_ROOT=""
QUIET=0
STAGED_INDEX=0
cleanup_index_root() { [ -n "${INDEX_ROOT:-}" ] && rm -rf "$INDEX_ROOT"; INDEX_ROOT=""; }
trap cleanup_index_root EXIT

_err() { printf '%s: %s\n' "$PROG" "$*" >&2; }
_say() { printf '%s\n' "$*"; }

ROOT_DEFAULT="$(git rev-parse --show-toplevel 2>/dev/null || true)"
[ -n "$ROOT_DEFAULT" ] || ROOT_DEFAULT="$PWD"

# materialise_index <root> <dest>
#
# DF-CRIER-399/403/408: EVERY commit carries the INDEX, including unchanged
# HEAD blobs when only a non-board path is staged. Materialise both canonical
# board files; absence/deletion/unmerged entries are failures, never empty scope.
materialise_index() {
  local root="$1" dest="$2"
  local path
  local paths=(.coding-hermes/board/tasks.jsonl .coding-hermes/board/events.jsonl)
  # Preserve topology-A metadata/fixtures when present, including unchanged
  # HEAD blobs. Otherwise validating only tasks/events silently narrows scope.
  for path in .coding-hermes/board/board.jsonl .coding-hermes/board/fixtures.jsonl; do
    if git -C "$root" cat-file -e ":$path" 2>/dev/null ||
      git -C "$root" cat-file -e "HEAD:$path" 2>/dev/null; then
      paths+=("$path")
    fi
  done
  mkdir -p "$dest/.coding-hermes/board" || return 2
  for path in "${paths[@]}"; do
    if ! git -C "$root" cat-file -e ":$path" 2>/dev/null; then
      _err "board: required index path $path is missing, deleted or unmerged — commit refused."
      return 1
    fi
    git -C "$root" show ":$path" > "$dest/$path" || return 2
  done
  return 0
}

run_check() {
  local root="$1" staged="${2:-0}"
  cleanup_index_root
  local validator
  validator="$(command -v boardctl 2>/dev/null || true)"
  if [ -z "$validator" ]; then
    _err "boardctl is not on PATH — refusing to report a green over a board nobody validated."
    _err "install boardctl (coding-hermes-boardctl) or unstage the board change."
    return 2
  fi
  local version
  version="$("$validator" version 2>/dev/null | head -1)"
  [ -n "$version" ] || version="boardctl (no version output)"
  _say "board: validator $validator — $version"

  local scope="working tree"
  if [ "$staged" -eq 1 ]; then
    # Validate the INDEX, not the tree: the commit carries these blobs.
    local idxroot
    idxroot="$(mktemp -d "${TMPDIR:-/tmp}/check-board-index.XXXXXX")" || return 2
    materialise_index "$root" "$idxroot"
    local materialise_rc=$?
    if [ "$materialise_rc" -ne 0 ]; then
      _err "board: could not read the required board blobs from the index — refusing to validate the working tree instead."
      rm -rf "$idxroot"
      return "$materialise_rc"
    fi
    root="$idxroot"
    scope="staged index (the blobs this commit will carry)"
    INDEX_ROOT="$idxroot"
  fi
  _say "board: root $root"
  _say "board: scope $scope"

  local board="$root/.coding-hermes/board"
  local required
  for required in tasks.jsonl events.jsonl; do
    if [ ! -f "$board/$required" ]; then
      _err "board: required path .coding-hermes/board/$required is absent under $root — refusing a vacuous PASS."
      return 1
    fi
  done
  if [ ! -s "$board/tasks.jsonl" ]; then
    _err "board: .coding-hermes/board/tasks.jsonl is empty — no board records to validate."
    return 1
  fi

  local out rc
  out="$("$validator" validate -C "$root" 2>&1)"
  rc=$?
  if [ "$QUIET" -eq 1 ] && [ "$rc" -eq 0 ]; then
    # quiet mode: a PASS does not reprint 20 legacy warnings on every commit
    QUIET=1
  else
    printf '%s\n' "$out"
  fi
  if [ "$rc" -ne 0 ]; then
    _err "board: validation FAILED (boardctl exit $rc) — fix the rows named above; do not commit an invalid board with --no-verify."
    return 1
  fi
  local rows warns
  rows="$(grep -c '"id"' "$board/tasks.jsonl" 2>/dev/null || echo 0)"
  warns="$(printf '%s\n' "$out" | grep -c '^\[warn\]' || true)"
  _say "board: validation PASS — $rows row(s) accepted ($warns warning(s), not fatal)"
  return 0
}

_selftest() {
  local rc fails=0
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/check-board-selftest.XXXXXX")" || return 2
  trap 'cleanup_index_root; rm -rf "${tmp:-}"' EXIT

  if ! command -v boardctl >/dev/null 2>&1; then
    _err "selftest cannot run: boardctl is not on PATH (that is itself the exit-2 path this script claims)"
    return 2
  fi

  # fixture 1: a canonical board
  boardctl init -C "$tmp/ok" --project selftest --namespace selftest >/dev/null 2>&1
  boardctl create -C "$tmp/ok" --id SELFTEST-1 --title "a well formed row" --priority P3 >/dev/null 2>&1

  run_check "$tmp/ok" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -ne 0 ]; then
    _err "selftest: a canonical board was REJECTED (exit $rc)"
    fails=$((fails + 1))
  else
    _say "selftest: canonical board accepted (exit 0)"
  fi

  # fixture 2: an unambiguously unknown status. Some boardctl releases treat
  # 'closed' as a warning-only read alias; the fixture must remain an ERROR.
  cp -r "$tmp/ok" "$tmp/bad"
  python3 - "$tmp/bad/.coding-hermes/board/tasks.jsonl" <<'PY'
import json, sys
path = sys.argv[1]
lines = [l for l in open(path) if l.strip()]
row = json.loads(lines[-1])
row['status'] = '__SELFTEST_INVALID_STATUS__'
lines[-1] = json.dumps(row) + '\n'
open(path, 'w').writelines(lines)
PY
  run_check "$tmp/bad" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -eq 0 ]; then
    _err "selftest: a board carrying an unknown selftest status was ACCEPTED — the arm is vacuous"
    fails=$((fails + 1))
  else
    _say "selftest: off-vocabulary row rejected (exit $rc)"
  fi

  # fixture 3: no validator on PATH must be exit 2, never a silent skip
  env PATH=/nonexistent /bin/bash "$0" -C "$tmp/ok" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -ne 2 ]; then
    _err "selftest: missing boardctl produced exit $rc, expected 2"
    fails=$((fails + 1))
  else
    _say "selftest: missing boardctl exits 2"
  fi

  # fixture 4: NEUTER proof — a copy whose verdict is forced to success must
  # ACCEPT the bad fixture, proving fixture 2 fails because of the check.
  sed 's/^  if \[ "\$rc" -ne 0 \]; then$/  if [ 0 -ne 0 ]; then/' "$0" > "$tmp/neuter.sh"
  if ! cmp -s "$0" "$tmp/neuter.sh"; then
    bash "$tmp/neuter.sh" -C "$tmp/bad" >/dev/null 2>&1
    rc=$?
    if [ "$rc" -eq 0 ]; then
      _say "selftest: neutered copy accepts the bad fixture (fixture 2 is not vacuous)"
    else
      _err "selftest: neutered copy still rejected the bad fixture (exit $rc) — the checker fails for another reason"
      fails=$((fails + 1))
    fi
  else
    _err "selftest: neuter transform did not apply — the proof is unavailable"
    fails=$((fails + 1))
  fi

  # fixture 5 (DF-CRIER-399): a commit carries the INDEX, so a staged-only
  # invalid row must be rejected even when the working tree was restored clean.
  idxrepo="$tmp/idxrepo"
  mkdir -p "$idxrepo/.coding-hermes/board"
  git -C "$idxrepo" init -q >/dev/null 2>&1
  cp "$tmp/ok/.coding-hermes/board/tasks.jsonl" "$idxrepo/.coding-hermes/board/tasks.jsonl"
  if [ -f "$tmp/ok/.coding-hermes/board/events.jsonl" ]; then
    cp "$tmp/ok/.coding-hermes/board/events.jsonl" "$idxrepo/.coding-hermes/board/events.jsonl"
  else
    : > "$idxrepo/.coding-hermes/board/events.jsonl"
  fi
  git -C "$idxrepo" add .coding-hermes/board >/dev/null 2>&1
  git -C "$idxrepo" -c core.hooksPath=/dev/null -c user.email=selftest@example.invalid -c user.name=selftest commit -qm base >/dev/null 2>&1
  cp "$tmp/bad/.coding-hermes/board/tasks.jsonl" "$idxrepo/.coding-hermes/board/tasks.jsonl"
  git -C "$idxrepo" add .coding-hermes/board/tasks.jsonl >/dev/null 2>&1
  cp "$tmp/ok/.coding-hermes/board/tasks.jsonl" "$idxrepo/.coding-hermes/board/tasks.jsonl"
  run_check "$idxrepo" 1 >/dev/null 2>&1
  rc=$?
  if [ "$rc" -eq 0 ]; then
    _err "selftest: a staged-only invalid row was ACCEPTED — the index scope is not being read"
    fails=$((fails + 1))
  else
    _say "selftest: staged-only invalid row rejected while the worktree is clean (index scope)"
  fi

  # Real commit/merge/applypatch entrypoints, not just direct checker calls.
  local commit_tests
  commit_tests="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/check-board-commit-selftest.py"
  if ! python3 "$commit_tests"; then
    fails=$((fails + 1))
  fi

  if [ "$fails" -ne 0 ]; then
    _err "selftest FAILED ($fails problem(s))"
    return 1
  fi
  _say "selftest PASS"
  return 0
}

main() {
  local root="$ROOT_DEFAULT" selftest=0
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -C)
        root="$2"
        shift 2
        ;;
      --selftest)
        selftest=1
        shift
        ;;
      -q | --quiet)
        QUIET=1
        shift
        ;;
      --staged-index)
        STAGED_INDEX=1
        shift
        ;;
      -h | --help)
        sed -n '2,40p' "$0"
        return 0
        ;;
      *)
        _err "unknown argument '$1'"
        return 2
        ;;
    esac
  done

  if [ "$selftest" -eq 1 ]; then
    _selftest
    return $?
  fi

  run_check "$root" "${STAGED_INDEX:-0}"
}

main "$@"
exit $?
