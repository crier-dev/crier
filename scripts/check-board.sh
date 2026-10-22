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
#   BOARD_ALLOW_SHRINK=1 git commit ...      # explicitly intended row deletion
#   BOARD_ALLOW_SHRINK=1 bash scripts/check-board.sh # same declaration in CI
#
# The DEFAULT run validates the WORKING TREE, so it compares that tree with the
# committed baseline it names — `retention: worktree vs HEAD` — and keeps the
# committed-shrink comparison CI depends on, `retention: HEAD vs HEAD^1`. Every
# comparison that ran is printed, including the ones that found no shrink.
# --staged-index compares `retention: index vs HEAD` instead.
#
# SHRINK POLICY (DF-CRIER-426 tasks.jsonl; DF-CRIER-429 events.jsonl;
# DF-CRIER-434 the default run; DF-CRIER-442 the emptied file)
# ---------------------------------------------------------------------------
# BOTH canonical board files' nonblank record counts must not decrease. Which
# baseline is measured against depends on what the run validated, and each
# comparison says so in its own output line:
#   * default (no flag): `worktree vs HEAD` AND `HEAD vs HEAD^1`. CI runs this
#     command, and on a clean checkout worktree == HEAD, so the committed-shrink
#     comparison is the one that fires there (first parent for merges; fetch
#     depth >= 2). DF-CRIER-434: this command printed `scope working tree` while
#     comparing two COMMITS, so a worktree cut from 5 rows to 1 with the index
#     untouched validated 1 row and exited 0; the same tree under
#     --staged-index reported 5, and only a COMMITTED shrink was refused.
#   * --staged-index (the commit hooks): `index vs HEAD` — the blobs the commit
#     will carry, never an unstaged worktree repair.
# events.jsonl sat outside this gate until DF-CRIER-429: measured on
# a scratch repo, a staged events.jsonl cut from 5 rows to 1 passed with exit 0
# while the same shrink of tasks.jsonl was refused, so a 1750-row event log had no
# retention gate at all.
# Missing baseline board/root commit is allowed, decided per file; unavailable
# shallow history fails closed. Standalone non-Git roots have no baseline and get
# validation only.
# Loss diagnostics name the COMPARISON that fired and the FILE that shrank, both
# counts and the lost records, with their total and a bounded list. A lost row is
# named by its `id`; a row without an id — or an unparseable line — is named by
# its line number instead of being silently skipped.
# Only exact BOARD_ALLOW_SHRINK=1 permits a shrink, with a visible diagnostic
# naming the file AND which declared outcome it is — `declared, still populated
# (5 -> 1 row(s))` or `declared, but the file is now EMPTY (5 -> 0 row(s))`. The
# hatch permits a declared REDUCTION, never the disappearance of a board file: an
# empty `tasks.jsonl` and an empty `events.jsonl` are refused ALIKE, whatever the
# hatch says (DF-CRIER-442). Before that fix the empty-file refusal named
# `tasks.jsonl` only, so — measured with the real boardctl — `events.jsonl`
# truncated to 0 bytes under BOARD_ALLOW_SHRINK=1 printed `shrink explicitly
# allowed` and then `validation PASS — 5 row(s) accepted` with exit 0, while the
# same truncation of `tasks.jsonl` exited 1. The hatch also never permits missing
# files or invalid rows. A board FILE with no records is not a valid board:
# boardctl writes one event per mutation (`create`, `update`, `event` — `init`
# itself writes none), so a board's first row brings its first event, and a board
# that exists only as `boardctl init` left it (tasks template rows, zero events)
# is refused until its first row is created. Set the hatch on the intended
# command only, including CI's checker command for an intentional deletion, and
# row validation still runs on a declared shrink.
# Same-count replacements are not a shrink; this is not an identity/history audit.
#
# WHAT IS CHECKED
# ---------------
#   empty board file             BOTH canonical files must carry at least one
#                                 nonblank record: an empty `tasks.jsonl` or an
#                                 empty `events.jsonl` is refused (exit 1) in
#                                 every mode and whatever BOARD_ALLOW_SHRINK
#                                 says (DF-CRIER-442/453). Emptiness uses the
#                                 per-file census from boardctl validate, not
#                                 byte size; refusal quotes bytes AND nonblank
#                                 records. Comments are invalid JSON, not blank.
#                                 A board file with no records is not valid.
#   boardctl validate -C <root>   the board's own canon: status vocabulary, key
#                                 shapes, event ids. ERRORS are fatal (exit 1);
#                                 WARNINGS are printed and NOT fatal, because the
#                                 validator itself tolerates legacy shapes and
#                                 says so per line.
#   close-as-merged ancestry      every row that THIS diff newly closes (its
#                                 status becomes a closed word, or its recorded
#                                 commit changes) must name a commit that
#                                 actually exists on the branch it is closed on:
#                                 `git merge-base --is-ancestor <commit_hash>
#                                 <main|origin/main>`. A hash that is not an
#                                 ancestor, a hash that resolves to no commit,
#                                 and an 'already merged' close with NO hash are
#                                 all REFUSED, each naming the row and the value
#                                 it checked (DF-CRIER-452).
#
# WHY THE ANCESTRY ARM EXISTS (DF-CRIER-452, MEASURED 2026-10-21 at main 0974c73)
# -------------------------------------------------------------------------------
# Rows DF-CRIER-438/439/441 were closed in board commit 5cc8229 with
# commit_hash=cfe9f0a and the prose 'Implementation already merged in cfe9f0a'.
# `git merge-base --is-ancestor cfe9f0a main` exited 1; the commit existed only
# on the worker branch (`git rev-list --left-right --count main...wt/DF-CRIER-438`
# = '39 1'), and the fix's own artefact was absent from main
# (`grep -c kill-after scripts/doctor-hooks.sh` = 0 there, 8 on the branch). Three
# rows read complete for a day while the defect they address was still live. A
# close that RECORDS a hash is not a close that VERIFIED one — the same law as the
# landing script that printed success over a failed merge: assert the COMMIT is in
# the branch, never that a command printed something.
#
# SCOPE OF THE ANCESTRY ARM — stated, not implied:
#   * it reads only rows this diff CHANGES into a closed state (index vs HEAD, or
#     worktree vs HEAD). It is a gate on closes, NOT a historical audit: legacy
#     rows already closed before this gate existed are not re-judged, so the arm
#     cannot block an unrelated commit over an old row;
#   * the bases are the MAIN lineage — `main`, then `origin/main`, accepting an
#     ancestor of either (a local main that is ahead of the pushed remote is the
#     normal pre-push state). With neither ref present it falls back to HEAD and
#     SAYS SO in its scope line;
#   * a close with no commit_hash at all is allowed with an advisory count unless
#     its own close prose claims the work was 'already merged' — then it is
#     refused, because that claim is exactly what must be verified;
#   * it is not a claim about the commit's CONTENT: an ancestor that is empty, or
#     a merge that later reverted the change, is outside what ancestry can say.
#
# WHAT IS NOT CHECKED
# -------------------
#   * boardctl's OWN vocabulary. A status the validator rejects and the repo
#     considers legitimate is a boardctl bug, not something this arm can fix;
#     the fix belongs in the boardctl source (DF-CRIER-387 names that half).
#     What this arm DOES own is proving the deployed validator accepts the
#     vocabulary the fleet relies on: --selftest (DF-CRIER-388) feeds rows
#     carrying the plausible status words 'closed'/'completed'/'done' through
#     the same run_check path and fails loudly if the deployed boardctl
#     rejects any of them — a stale deploy (boardctl < v0.1.11) is caught by
#     the selftest, not rediscovered as a wall of failed commits.
#   * `.coding-hermes/` files that are not board data (lock files, notes).
#   * the semantic truth of a row: a well-formed row that lies about its own
#     verification is not detectable here.
#
# A MISSING VALIDATOR IS EXIT 2, NEVER A SILENT SKIP (same rule as the other
# arms of this gate): a board check that cannot run must say so loudly enough
# that the commit stops.

set -uo pipefail

PROG="check-board.sh"
# DF-CRIER-429: both canonical board files are retention-gated, not just tasks.
BOARD_RETENTION_FILES=(tasks.jsonl events.jsonl)
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

# DF-CRIER-426 (tasks.jsonl) / DF-CRIER-429 (events.jsonl) / DF-CRIER-434 (the
# default run): validate row retention separately from row validity over BOTH
# canonical board files, with ONE implementation, so neither file can sit
# outside the gate again, and so the default run cannot compare a different pair
# of things than the one it says it validated. The mode decides the comparisons
# and every one of them is labelled in the output:
#   index    — index vs HEAD (the blobs a commit will carry);
#   worktree — worktree vs HEAD (the tree this run validated) AND HEAD vs HEAD^1
#              (the committed-shrink check CI depends on; on a clean checkout
#              worktree == HEAD, so only the second one can fire there).
# The first parent is used for a pending merge's HEAD^1.
check_row_retention() {
  if ! command -v python3 >/dev/null 2>&1; then
    _err "board: python3 is required for the shrink check — refusing a silent skip."
    return 2
  fi
  python3 - "$1" "$2" "$3" "${BOARD_RETENTION_FILES[@]}" <<'PY'
from collections import Counter
from pathlib import Path
import json
import os
import subprocess
import sys

root, board_dir, mode = sys.argv[1:4]
names = sys.argv[4:]
PREFIX = '.coding-hermes/board/'


def git(*args):
    return subprocess.run(['git', '-C', root, *args], capture_output=True, text=True)


def fail(message, code=2):
    print('check-board.sh: board: ' + message, file=sys.stderr)
    raise SystemExit(code)


def records(text):
    # Nonblank records with their 1-based line numbers, duplicates included;
    # trailing newlines and formatting are not rows.
    return [(number, line) for number, line in enumerate(text.splitlines(), 1)
            if line.strip()]


def identity(number, line):
    """(label, key) for one row: its id when it has one, else its line number.

    A row without an id is NAMED rather than skipped, so a loss report can always
    account for every removed record. The key is content-based for id-less rows,
    so an identical row is not reported as lost merely for moving lines.
    """
    try:
        row = json.loads(line)
    except ValueError:
        return 'line %d (unparseable record)' % number, ('raw', line.strip())
    if isinstance(row, dict) and row.get('id') is not None:
        return json.dumps(str(row['id']), ensure_ascii=True)[1:-1], ('id', str(row['id']))
    return 'line %d (no id)' % number, ('no-id', json.dumps(row, sort_keys=True,
                                                           ensure_ascii=True))


# Standalone board roots and unborn repositories have no historical baseline.
if git('rev-parse', '--verify', 'HEAD^{commit}').returncode:
    raise SystemExit(0)


def comparisons(mode):
    """The (label, baseline, target-label, target-source) tuples to compare.

    DF-CRIER-434: the no-flag run prints `scope working tree` and validates the
    WORKING TREE, but it used to compare two COMMITS (HEAD^1 vs HEAD), so a
    worktree truncation with an untouched index passed with exit 0 while the
    scope line claimed the tree it named had been checked. Default mode now
    retains BOTH comparisons: the tree it validated against the committed
    baseline it was compared with, AND the committed-shrink check CI depends on
    (on a clean checkout worktree == HEAD, so only the second one can fire).
    --staged-index keeps its single index-vs-HEAD comparison.
    """
    if mode == 'index':
        return (('index vs HEAD', 'HEAD', 'index', 'workdir'),)
    if mode == 'worktree':
        return (('worktree vs HEAD', 'HEAD', 'worktree', 'workdir'),
                ('HEAD vs HEAD^1', 'HEAD^1', 'HEAD', 'commit'))
    fail('unknown retention mode ' + repr(mode) + ' — refusing to guess which '
         'baseline to compare against.')


def target_text(source, name, path):
    """The rows under test: the tree on disk, or the committed HEAD blob."""
    if source == 'workdir':
        try:
            return (Path(board_dir) / name).read_text()
        except OSError as error:
            fail('cannot read the tree under test for ' + path + ': ' + str(error))
    committed = git('show', 'HEAD:' + path)
    if committed.returncode:
        fail('required committed path ' + path + ' is missing — commit refused.', 1)
    return committed.stdout


shallow = git('rev-parse', '--is-shallow-repository').stdout.strip() == 'true'
shrunken = []
unavailable = []
for label, base, target, source in comparisons(mode):
    if git('rev-parse', '--verify', base + '^{commit}').returncode:
        if shallow:
            # A shallow checkout is NOT a root commit: the baseline must be
            # fetched, never silently accepted as the very loss being measured.
            unavailable.append(base)
            continue
        # A genuine root commit, not a missing parent concealed by a shallow
        # checkout. The other comparison still runs.
        print('check-board.sh: board: retention: ' + label + ' — baseline ' + base
              + ' unavailable (root commit); comparison skipped', file=sys.stderr)
        continue
    compared = 0
    shrank = False
    for name in names:
        path = PREFIX + name
        # Missing board at the baseline is the first-board case, not a shrink,
        # and that is decided per file. Use ls-tree so a read error cannot
        # masquerade as an absent board.
        entry = git('ls-tree', base, '--', path)
        if entry.returncode:
            fail('cannot inspect shrink baseline ' + base + ' for ' + path + ': '
                 + entry.stderr.strip())
        if not entry.stdout.strip():
            continue
        old = git('show', base + ':' + path)
        if old.returncode:
            fail('cannot read shrink baseline ' + base + ' for ' + path + ': '
                 + old.stderr.strip())
        before = records(old.stdout)
        after = records(target_text(source, name, path))
        compared += 1
        if len(after) >= len(before):
            # Growth, or a same-count replacement: not a shrink, not in scope.
            continue
        # Counter subtraction names duplicate occurrences lost too; a row
        # without an id is named by its line number instead of being skipped.
        old_rows = [identity(number, line) for number, line in before]
        new_keys = Counter(key for _, key in (identity(number, line)
                                              for number, line in after))
        lost = list((Counter(key for _, key in old_rows) - new_keys).elements())
        labels = {}
        for row_label, key in old_rows:
            labels.setdefault(key, row_label)
        # The comparison is part of every line: the same file can shrink in
        # both directions at once, and a refusal must say which baseline it
        # measured against.
        summary = (f'{label} — {path} shrank: {base}={len(before)}, '
                   f'{target}={len(after)}; {len(before) - len(after)} row(s) lost')
        print(f'check-board.sh: board: retention: {summary}', file=sys.stderr)
        limit = 20
        suffix = f' ... ({len(lost) - limit} more)' if len(lost) > limit else ''
        print(f'check-board.sh: board: retention: {label} — lost ids ({len(lost)} '
              f'total; first {limit}): '
              + ', '.join(labels[key] for key in lost[:limit]) + suffix,
              file=sys.stderr)
        shrunken.append((label, path, len(before), len(after)))
        shrank = True
    if not shrank:
        # State what WAS compared even when nothing shrank: a default run that
        # silently compared nothing is the defect this ticket measures.
        if compared:
            print(f'check-board.sh: board: retention: {label} — {compared} file(s) '
                  'compared, no shrink', file=sys.stderr)
        else:
            print(f'check-board.sh: board: retention: {label} — baseline {base} '
                  'carries no board file (first board); nothing to compare',
                  file=sys.stderr)

if not shrunken:
    if unavailable:
        fail('shrink baseline ' + unavailable[0] + ' unavailable in shallow history; '
             'fetch at least two commits (git fetch --deepen=1) and rerun.')
    raise SystemExit(0)
if os.environ.get('BOARD_ALLOW_SHRINK') == '1':
    for label, path, before_count, after_count in shrunken:
        # DF-CRIER-442: the diagnostic must say WHICH declared outcome this is.
        # "still populated" and "now EMPTY" are different verdicts — the hatch
        # permits a declared reduction, never the disappearance of a board file —
        # and the emptied case is refused by the empty-board-file check that runs
        # when this helper returns 0, whatever this notice just said.
        state = (('declared, still populated (%d -> %d row(s))'
                  % (before_count, after_count)) if after_count else
                 ('declared, but the file is now EMPTY (%d -> %d row(s)) — an '
                  'empty board file is not a valid board'
                  % (before_count, after_count)))
        print('check-board.sh: board: ' + path + ' (retention: ' + label
              + '): shrink explicitly allowed by BOARD_ALLOW_SHRINK=1: '
              + state + '; required paths and row validation still apply.',
              file=sys.stderr)
    raise SystemExit(0)
fail('shrink refused: '
     + ', '.join(path + ' (retention: ' + label + ')'
                 for label, path, _, _ in shrunken)
     + '; if this deletion is intended, rerun with BOARD_ALLOW_SHRINK=1 git commit ... '
     '(hooks) or BOARD_ALLOW_SHRINK=1 bash scripts/check-board.sh (CI).', 1)
PY
}

# check_close_ancestry <git-repo-root> <board-dir>
#
# DF-CRIER-452: a close as "already merged" must be VERIFIED, not recorded.
# Reads only the rows THIS diff changes into a closed state (index vs HEAD, or
# worktree vs HEAD) and refuses any close whose commit_hash is not an ancestor of
# the main lineage — `main`, else `origin/main`, else HEAD, named in the scope
# line — and any hash that resolves to no commit at all. An 'already merged'
# close carrying no hash is refused too, because the claim is the thing that must
# be proven. Rows already closed at HEAD are never re-judged: this gates closes,
# it is not a historical audit.
check_close_ancestry() {
  if ! command -v python3 >/dev/null 2>&1; then
    _err "board: python3 is required for the close-ancestry gate — refusing a silent skip."
    return 2
  fi
  python3 - "$1" "$2" <<'PY'
import json
import os
import re
import subprocess
import sys

repo, board_dir = sys.argv[1], sys.argv[2]
PREFIX = '.coding-hermes/board/'
TASKS = 'tasks.jsonl'
CLOSED = {'complete', 'closed', 'done', 'completed'}
CLAIM = re.compile(r'already[\s_-]*merged', re.I)
# Fields a closer writes. Deliberately NOT the filing prose (title/reasoning/
# source): a row that merely DESCRIBES an 'already merged' defect is not a close
# claiming one.
CLOSE_PROSE = ('worker_summary', 'foreman_note', 'review_notes', 'note',
               'resolution', 'close_note')


def git(*args):
    return subprocess.run(['git', '-C', repo, *args], capture_output=True,
                          text=True)


def say(message):
    print('board: close-ancestry: ' + message)


def err(message):
    print('check-board.sh: board: close-ancestry: ' + message, file=sys.stderr)


# A board root outside git has no branch to verify a close against. Say so —
# the selftest fixtures are such roots — and leave the other arms to decide.
if git('rev-parse', '--git-dir').returncode:
    say('board root is not a git repository — no branch to verify a close '
        'against; arm skipped')
    raise SystemExit(0)

baseline = git('show', 'HEAD:' + PREFIX + TASKS)
if baseline.returncode:
    say('no committed ' + PREFIX + TASKS + ' at HEAD — nothing to compare; '
        'arm skipped')
    raise SystemExit(0)

path = os.path.join(board_dir, TASKS)
try:
    with open(path) as handle:
        target_text = handle.read()
except OSError as error:
    err('cannot read ' + path + ': ' + str(error))
    raise SystemExit(2)


def parse(text):
    rows = {}
    for line in text.splitlines():
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except ValueError:
            continue
        if isinstance(row, dict) and row.get('id') is not None:
            rows[str(row['id'])] = row
    return rows


before, after = parse(baseline.stdout), parse(target_text)

bases = [name for name in ('main', 'origin/main')
         if git('rev-parse', '--verify', '--quiet',
                name + '^{commit}').returncode == 0]
if not bases:
    bases = ['HEAD']
    say('neither main nor origin/main resolves here — verifying against HEAD '
        '(the branch tip this commit extends)')

new_closes = []
for rid, row in after.items():
    if row.get('status') not in CLOSED:
        continue
    prev = before.get(rid)
    if (prev is not None and prev.get('status') in CLOSED
            and str(prev.get('commit_hash') or '')
            == str(row.get('commit_hash') or '')):
        # Already closed at the baseline with the same hash: not a new close,
        # not this arm's business (that is the legacy-row scope above).
        continue
    new_closes.append(row)

refused = []
unhashed = []
checked = 0
for row in new_closes:
    rid = str(row['id'])
    value = str(row.get('commit_hash') or '').strip()
    if not value:
        prose = ' '.join(str(row.get(field) or '') for field in CLOSE_PROSE)
        if CLAIM.search(prose):
            refused.append((rid, '(none)',
                            "was closed as 'already merged' with no commit_hash "
                            'to verify'))
        else:
            unhashed.append(rid)
        continue
    if git('rev-parse', '--verify', '--quiet', value + '^{commit}').returncode:
        refused.append((rid, value,
                        'is not a commit in this repository — there is nothing '
                        'on the branch to verify'))
        continue
    checked += 1
    if not any(git('merge-base', '--is-ancestor', value, base).returncode == 0
               for base in bases):
        refused.append((rid, value,
                        'is not an ancestor of ' + ' / '.join(bases)))

say('checked ' + str(checked) + ' new close(s) against ' + ' / '.join(bases)
    + '; ' + str(len(unhashed)) + ' close(s) carried no commit_hash (allowed, '
    'advisory only); ' + str(len(refused)) + ' refused')

if refused:
    for rid, value, reason in refused:
        err('row ' + rid + ': commit_hash ' + value + ' ' + reason
            + ' — cannot close as already-merged')
    err('close refused: ' + str(len(refused)) + ' row(s) name a commit that is '
        'not on the branch this close lands on; merge the work first, then close '
        'with the MERGED commit (`git merge-base --is-ancestor <hash> '
        + bases[0] + '` must exit 0)')
    raise SystemExit(1)
raise SystemExit(0)
PY
}

run_check() {
  local root="$1" staged="${2:-0}" repo_root="$1"
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
  # Only the shrink verdict is overridable; missing paths above and empty files /
  # boardctl validation below remain mandatory for declared deletions.
  # DF-CRIER-429: the whole board directory is passed, and the helper checks BOTH
  # canonical files with the one implementation above.
  # DF-CRIER-434: the MODE, not a staged flag, picks the comparisons. The
  # default run validates the working tree and must therefore compare the
  # working tree with HEAD as well as HEAD with HEAD^1.
  local retention_mode="worktree"
  if [ "$staged" -eq 1 ]; then
    retention_mode="index"
  fi
  check_row_retention "$repo_root" "$board" "$retention_mode"
  local retention_rc=$?
  # DF-CRIER-452: verify a close's commit_hash before the board can accept it.
  # Runs on the same scope (index or worktree) the retention arm just measured,
  # so the rows checked are the rows this commit will carry.
  check_close_ancestry "$repo_root" "$board"
  local ancestry_rc=$?
  # Retention diagnostics still run first (including the declared outcome), but
  # cannot hide the unconditional empty-file refusal or validator diagnostics.

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
  # DF-CRIER-453: ask the validator, not the byte-size test or a second JSONL
  # parser. Its census counts each file separately and skips whitespace-only
  # lines. Comments are not blank: boardctl already rejected them above as JSON.
  # Fail closed if a validator version stops exposing the census we rely on.
  local rows="" events="" line census_matches=0
  local census='^rows: ([0-9]+) tasks, ([0-9]+) events, [0-9]+ fixtures(, header parsed)?$'
  while IFS= read -r line; do
    if [[ "$line" =~ $census ]]; then
      rows="${BASH_REMATCH[1]}"
      events="${BASH_REMATCH[2]}"
      census_matches=$((census_matches + 1))
    fi
  done <<< "$out"
  if [ "$census_matches" -ne 1 ]; then
    _err "board: boardctl did not report exactly one recognized per-file record census — refusing to guess whether the board is empty."
    return 2
  fi
  local canonical count bytes
  for canonical in "${BOARD_RETENTION_FILES[@]}"; do
    count="$rows"
    [ "$canonical" != events.jsonl ] || count="$events"
    if [ "$count" -eq 0 ]; then
      bytes="$(wc -c < "$board/$canonical")" || return 2
      _err "board: .coding-hermes/board/$canonical is empty: $bytes byte(s), $count nonblank record(s) — no board records to validate."
      _err "board: an empty board file is not a valid board; BOARD_ALLOW_SHRINK=1 permits a declared reduction, never the disappearance of a board file."
      return 1
    fi
  done
  [ "$retention_rc" -eq 0 ] || return "$retention_rc"
  [ "$ancestry_rc" -eq 0 ] || return "$ancestry_rc"
  local warns
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

  # fixture 1b (DF-CRIER-388): the plausible status words the fleet actually
  # writes — 'closed' alongside the long-accepted aliases 'completed'/'done' —
  # must each be ACCEPTED (warn-only), exactly like fixture 1's canonical
  # vocabulary. The status vocabulary lives in the boardctl binary, not here
  # (DF-CRIER-387), so this fixture is the pin that holds across deploys: a
  # boardctl that predates v0.1.11 / DF-BOARDCTL-41 ('closed' -> 'complete'
  # read alias) or a later alias-table regression fails the selftest loudly
  # instead of silently re-breaking every commit that closes a row.
  for word in closed completed done; do
    rm -rf "$tmp/alias-$word"
    cp -r "$tmp/ok" "$tmp/alias-$word"
    python3 - "$tmp/alias-$word/.coding-hermes/board/tasks.jsonl" "$word" <<'PY'
import json, sys
path, word = sys.argv[1], sys.argv[2]
lines = [l for l in open(path) if l.strip()]
row = json.loads(lines[-1])
row['status'] = word
lines[-1] = json.dumps(row) + '\n'
open(path, 'w').writelines(lines)
PY
    run_check "$tmp/alias-$word" >/dev/null 2>&1
    rc=$?
    if [ "$rc" -ne 0 ]; then
      _err "selftest: a board carrying status '$word' was REJECTED (exit $rc) — the deployed boardctl predates v0.1.11/DF-BOARDCTL-41 or the alias table regressed"
      fails=$((fails + 1))
    else
      _say "selftest: status '$word' accepted"
    fi
  done

  # fixture 2: an unambiguously unknown status. Since boardctl v0.1.11
  # (DF-BOARDCTL-41) 'closed' IS a warning-only read alias — fixture 1b pins
  # that acceptance — so the rejection fixture must use a word NO boardctl
  # release accepts; the fixture must remain an ERROR on every release.
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

  # fixture 6 (DF-CRIER-442): an emptied half of the queue is not a valid board.
  # An empty `events.jsonl` now gets exactly the refusal an empty `tasks.jsonl`
  # always got, in every mode and whatever the hatch says. A scratch root has no
  # baseline, so here the shrink arm cannot be the thing refusing it — this pins
  # the empty-file rule itself, not the shrink rule.
  noevents="$tmp/noevents"
  cp -r "$tmp/ok" "$noevents"
  : > "$noevents/.coding-hermes/board/events.jsonl"
  run_check "$noevents" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -ne 1 ]; then
    _err "selftest: an empty events.jsonl produced exit $rc, expected 1"
    fails=$((fails + 1))
  else
    _say "selftest: empty events.jsonl refused (exit 1)"
  fi
  env BOARD_ALLOW_SHRINK=1 /bin/bash "$0" -C "$noevents" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -ne 1 ]; then
    _err "selftest: BOARD_ALLOW_SHRINK=1 let an empty events.jsonl through (exit $rc, expected 1)"
    fails=$((fails + 1))
  else
    _say "selftest: the hatch does not permit an empty events.jsonl (exit 1)"
  fi

  # fixture 7 (DF-CRIER-452): a close must name a commit that is ON the branch.
  # MEASURED defect: rows closed as 'Implementation already merged in cfe9f0a'
  # while `git merge-base --is-ancestor cfe9f0a main` exited 1 — the hash existed
  # only on a worker branch. The fixture reproduces exactly that shape with a real
  # sibling-branch commit, and proves BOTH arms:
  #   7a an ancestor of main is ACCEPTED (so the arm is not refusing everything),
  #   7b a commit that exists but is not on main is REFUSED, naming the hash,
  #   7c an 'already merged' close with NO hash at all is REFUSED as well.
  anc="$tmp/ancestry"
  cat >"$tmp/close-row.py" <<'PY'
import json
import sys

path, rid, value, prose = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
rows = [json.loads(line) for line in open(path) if line.strip()]
for row in rows:
    if str(row.get('id')) == rid:
        row['status'] = 'complete'
        if value == '-':
            row.pop('commit_hash', None)
        else:
            row['commit_hash'] = value
        if prose:
            row['worker_summary'] = prose
open(path, 'w').writelines(json.dumps(row) + '\n' for row in rows)
PY
  boardctl init -C "$anc" --project selftest --namespace selftest >/dev/null 2>&1
  boardctl create -C "$anc" --id DF-452-ANCESTOR --title "a close naming a merged commit" --priority P2 >/dev/null 2>&1
  boardctl create -C "$anc" --id DF-452-SIBLING --title "a close naming an unmerged commit" --priority P2 >/dev/null 2>&1
  boardctl create -C "$anc" --id DF-452-NOHASH --title "a close claiming already merged with no commit" --priority P2 >/dev/null 2>&1
  git -C "$anc" init -q >/dev/null 2>&1
  git -C "$anc" symbolic-ref HEAD refs/heads/main >/dev/null 2>&1
  git -C "$anc" add -A >/dev/null 2>&1
  git -C "$anc" -c core.hooksPath=/dev/null -c user.email=selftest@example.invalid -c user.name=selftest commit -qm base >/dev/null 2>&1
  main_sha="$(git -C "$anc" rev-parse --short HEAD)"
  # A real commit that exists in the repository but is NOT an ancestor of main.
  git -C "$anc" checkout -q -b wt/sibling >/dev/null 2>&1
  printf 'unmerged worker work\n' >"$anc/sibling-work.txt"
  git -C "$anc" add sibling-work.txt >/dev/null 2>&1
  git -C "$anc" -c core.hooksPath=/dev/null -c user.email=selftest@example.invalid -c user.name=selftest commit -qm "feat: unmerged worker commit" >/dev/null 2>&1
  sibling_sha="$(git -C "$anc" rev-parse --short HEAD)"
  git -C "$anc" checkout -q main >/dev/null 2>&1
  if [ "$main_sha" = "$sibling_sha" ]; then
    _err "selftest: the ancestry fixture built the same commit on both refs — the fixture is broken"
    fails=$((fails + 1))
  fi

  cp -r "$anc" "$tmp/anc-ok"
  python3 "$tmp/close-row.py" "$tmp/anc-ok/.coding-hermes/board/tasks.jsonl" \
    DF-452-ANCESTOR "$main_sha" ""
  run_check "$tmp/anc-ok" >/dev/null 2>&1
  rc=$?
  if [ "$rc" -ne 0 ]; then
    _err "selftest: a close naming an ancestor of main ($main_sha) was REJECTED (exit $rc) — the ancestry arm refuses legitimate closes"
    fails=$((fails + 1))
  else
    _say "selftest: close naming an ancestor of main accepted ($main_sha)"
  fi

  cp -r "$anc" "$tmp/anc-bad"
  python3 "$tmp/close-row.py" "$tmp/anc-bad/.coding-hermes/board/tasks.jsonl" \
    DF-452-SIBLING "$sibling_sha" ""
  bad_err="$(run_check "$tmp/anc-bad" 2>&1 >/dev/null)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    _err "selftest: a close naming $sibling_sha — a commit on no branch that lands on main — was ACCEPTED; the ancestry arm is vacuous"
    fails=$((fails + 1))
  elif ! printf '%s\n' "$bad_err" | grep -qF "commit_hash $sibling_sha"; then
    _err "selftest: the refusal did not name the unmerged commit $sibling_sha"
    fails=$((fails + 1))
  elif ! printf '%s\n' "$bad_err" | grep -qF "is not an ancestor of main"; then
    _err "selftest: the refusal did not say which base the commit failed against"
    fails=$((fails + 1))
  else
    _say "selftest: close naming unmerged commit $sibling_sha refused, naming the commit and the base"
  fi

  cp -r "$anc" "$tmp/anc-nohash"
  python3 "$tmp/close-row.py" "$tmp/anc-nohash/.coding-hermes/board/tasks.jsonl" \
    DF-452-NOHASH - "Implementation already merged in $sibling_sha"
  nohash_err="$(run_check "$tmp/anc-nohash" 2>&1 >/dev/null)"
  rc=$?
  if [ "$rc" -eq 0 ]; then
    _err "selftest: an 'already merged' close with no commit_hash was ACCEPTED — the claim was never verified"
    fails=$((fails + 1))
  elif ! printf '%s\n' "$nohash_err" | grep -qF "DF-452-NOHASH"; then
    _err "selftest: the no-hash refusal did not name the row it refused"
    fails=$((fails + 1))
  else
    _say "selftest: 'already merged' close with no commit_hash refused, naming the row"
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
        # Print the header summary block: from line 2 up to (not including) the
        # first detailed section. A hardcoded line range drifts silently every
        # time the header grows.
        sed -n '2,/^# WHAT IS CHECKED/p' "$0" | sed '$d'
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
