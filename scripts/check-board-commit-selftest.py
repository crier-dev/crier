#!/usr/bin/env python3
"""Exercise board gates through Git, with real boardctl and scratch-only writes.

--source can point at a historical source snapshot for reproducible RED controls.
Only gitreins is stubbed: its unrelated Go checks must not mask board failures.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, default=Path(__file__).resolve().parents[1])
    args = parser.parse_args()
    source = args.source.resolve()
    validator = shutil.which('boardctl')
    if not validator:
        print('commit selftest: boardctl missing; exit 2')
        return 2
    failures = 0
    with tempfile.TemporaryDirectory(prefix='check-board-commits.') as temporary:
        root = Path(temporary)
        global_hooks = root / 'global-hooks'
        global_config = root / 'global.gitconfig'
        subprocess.run(
            ('git', 'config', '--file', str(global_config),
             'core.hooksPath', str(global_hooks)),
            check=True,
        )
        tools = root / 'bin'
        tools.mkdir()
        stub = tools / 'gitreins'
        stub.write_text('#!/bin/sh\nprintf "stub gitreins guard (board-only test)\\n"\nexit 0\n')
        stub.chmod(0o755)
        env = os.environ.copy()
        # A caller's intentional-deletion declaration must not neuter RED fixtures.
        env.pop('BOARD_ALLOW_SHRINK', None)
        # Do not inherit a caller's alternate index/worktree or host Git hooks.
        for key in list(env):
            if key.startswith('GIT_'):
                del env[key]
        env.update(GIT_CONFIG_GLOBAL=str(global_config), GIT_CONFIG_NOSYSTEM='1',
                   GIT_AUTHOR_NAME='board selftest', GIT_COMMITTER_NAME='board selftest',
                   GIT_AUTHOR_EMAIL='selftest@example.invalid',
                   GIT_COMMITTER_EMAIL='selftest@example.invalid',
                   PATH=str(tools) + os.pathsep + env['PATH'])

        def run(repo, *command, check=False):
            result = subprocess.run(command, cwd=repo, env=env, text=True,
                                    stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            if check and result.returncode:
                raise RuntimeError(f'{command}: exit {result.returncode}\n{result.stdout}')
            return result

        def git(repo, *command, check=True):
            return run(repo, 'git', *command, check=check)

        def seed_commit(repo, message):
            git(repo, 'add', '.')
            git(repo, '-c', 'core.hooksPath=/dev/null', 'commit', '-qm', message)

        def hooks_snapshot(repo):
            hooks = repo / '.git/hooks'
            return [
                (str(path.relative_to(hooks)), path.stat().st_mode & 0o777,
                 path.read_bytes())
                for path in sorted(hooks.rglob('*')) if path.is_file()
            ]

        def fixture(name):
            repo = root / name
            repo.mkdir()
            git(repo, 'init', '-q', '-b', 'main')
            run(repo, validator, 'init', '-C', str(repo), '--project', 'selftest',
                '--namespace', 'selftest', check=True)
            run(repo, validator, 'create', '-C', str(repo), '--id', 'SELFTEST-1',
                '--title', 'canonical fixture', '--priority', 'P3', check=True)
            scripts = repo / 'scripts'
            scripts.mkdir()
            shutil.copy2(source / 'scripts/check-board.sh', scripts / 'check-board.sh')
            shutil.copytree(source / 'scripts/hooks', scripts / 'hooks')
            hooks = repo / '.hooks'
            hooks.mkdir()
            for name in ('pre-commit', 'pre-merge-commit', 'pre-applypatch'):
                path = source / 'scripts/hooks' / name
                if path.exists():
                    shutil.copy2(path, hooks / name)
                    (hooks / name).chmod(0o755)
            config = repo / '.gitreins/config.yaml'
            config.parent.mkdir()
            config.write_text('{}\n')
            (repo / 'probe.txt').write_text('base\n')
            seed_commit(repo, 'fixture base (hooks intentionally disabled for setup)')
            git(repo, 'config', 'core.hooksPath', str(hooks))
            return repo

        def poison(repo):
            path = repo / '.coding-hermes/board/tasks.jsonl'
            with path.open('a') as output:
                output.write('{"id":"INVALID-1","title":"invalid fixture","status":"__SELFTEST_INVALID_STATUS__"}\n')

        def probe(repo):
            (repo / 'probe.txt').write_text('non-board change\n')
            git(repo, 'add', 'probe.txt')

        def expect(name, result, expected, diagnostic=None):
            nonlocal failures
            ok = result.returncode == expected
            if diagnostic:
                # Merged-output helpers (run/git) put the checker's lines on
                # stdout; a capture_output=True caller keeps them on stderr, and
                # stderr is None whenever the helper merged the two streams.
                output = (result.stdout or '') + (result.stderr or '')
                ok = ok and diagnostic in output
            print(f'commit selftest: {name}: exit {result.returncode}, expected {expected}: '
                  + ('PASS' if ok else 'FAIL'))
            if not ok:
                print(result.stdout)
                failures += 1

        def board_rows(repo, count):
            path = repo / '.coding-hermes/board/tasks.jsonl'
            template = json.loads(path.read_text().splitlines()[0])
            rows = [dict(template, id=f'SELFTEST-{i}', title=f'fixture row {i}')
                    for i in range(1, count + 1)]
            path.write_text(''.join(json.dumps(row) + '\n' for row in rows))
            return path

        def event_rows(repo, count, no_id_at=0):
            path = repo / '.coding-hermes/board/events.jsonl'
            template = json.loads(path.read_text().splitlines()[0])
            rows = []
            for number in range(1, count + 1):
                row = dict(template, id=number, detail=f'fixture event {number}')
                if number == no_id_at:
                    row.pop('id')
                rows.append(json.dumps(row))
            path.write_text(''.join(row + '\n' for row in rows))
            return path

        def keep_rows(path, numbers):
            lines = path.read_text().splitlines(keepends=True)
            path.write_text(''.join(lines[number - 1] for number in numbers))
            return path

        def staged_check(repo):
            """The checker's staged-index verdict, stderr captured for assertions."""
            return subprocess.run(
                ('bash', 'scripts/check-board.sh', '--staged-index'), cwd=repo,
                env=env, text=True, capture_output=True,
            )

        def default_check(repo):
            """The checker's no-flag verdict — the command CI and a human run."""
            return subprocess.run(
                ('bash', 'scripts/check-board.sh'), cwd=repo, env=env, text=True,
                capture_output=True,
            )

        def show_stderr(label, result):
            print(f'commit selftest: {label} exact checker stderr BEGIN')
            print(result.stderr, end='')
            print(f'commit selftest: {label} exact checker stderr END')

        def expect_stderr(label, result, required, forbidden=()):
            nonlocal failures
            missing = [text for text in required if text not in result.stderr]
            present = [text for text in forbidden if text in result.stderr]
            ok = not missing and not present
            print(f'commit selftest: {label}: ' + ('PASS' if ok else 'FAIL'))
            if not ok:
                if missing:
                    print(f'  missing from stderr: {missing}')
                if present:
                    print(f'  unexpectedly present: {present}')
                print(result.stderr, end='')
                failures += 1

        repo = fixture('shrink-691-to-1')
        board = board_rows(repo, 691)
        seed_commit(repo, '691-row baseline (hooks disabled for setup)')
        good = board.read_bytes()
        board.write_bytes(good.splitlines(keepends=True)[0])
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        # The worktree is deliberately repaired: only the staged blob lost rows.
        board.write_bytes(good)
        rejected = subprocess.run(
            ('bash', 'scripts/check-board.sh', '--staged-index'), cwd=repo,
            env=env, text=True, capture_output=True,
        )
        print('commit selftest: 691 -> 1 exact checker stderr BEGIN')
        print(rejected.stderr, end='')
        print('commit selftest: 691 -> 1 exact checker stderr END')
        expect('691 -> 1 truncation', rejected, 1)
        expected_ids = ', '.join(f'SELFTEST-{i}' for i in range(2, 22))
        if not all(text in rejected.stderr for text in (
                'HEAD=691', 'index=1', '690 row(s) lost',
                f'lost ids (690 total; first 20): {expected_ids}',
                '670 more', 'BOARD_ALLOW_SHRINK=1')) or 'SELFTEST-22' in rejected.stderr:
            print('commit selftest: shrink counts, lost-id total and bounded list: FAIL')
            failures += 1
        else:
            print('commit selftest: shrink counts, lost-id total and bounded list: PASS')
        expect('real commit rejects staged shrink despite repaired worktree',
               git(repo, 'commit', '-qm', 'truncated board', check=False), 1, 'HEAD=691')
        expect('hatch requires exact 1, not a truthy string',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=true', 'bash',
                   'scripts/check-board.sh', '--staged-index'), 1, '690 row(s) lost')

        # DF-CRIER-429: events.jsonl is the unguarded half of the board. Before
        # this change a staged events.jsonl cut 5 -> 1 passed with exit 0 while
        # the same shrink of tasks.jsonl was refused, so the durable event log
        # (1750 live rows in the repo) had no retention gate at all.
        repo = fixture('events-shrink')
        board_rows(repo, 2)
        events = event_rows(repo, 3)
        seed_commit(repo, 'baseline: 2 tasks, 3 events (hooks disabled for setup)')
        keep_rows(events, [1])
        git(repo, 'add', '.coding-hermes/board/events.jsonl')
        rejected = staged_check(repo)
        show_stderr('events 3 -> 1', rejected)
        expect('events-only shrink 3 -> 1 rejected', rejected, 1, 'events.jsonl shrank')
        expect_stderr('events shrink names the file, both counts and the lost ids',
                      rejected,
                      ['.coding-hermes/board/events.jsonl shrank: HEAD=3, index=1; '
                       '2 row(s) lost', 'lost ids (2 total; first 20): 2, 3',
                       'shrink refused: .coding-hermes/board/events.jsonl'],
                      forbidden=['.coding-hermes/board/tasks.jsonl shrank'])
        expect('real commit rejects a staged events-only shrink',
               git(repo, 'commit', '-qm', 'truncated events', check=False), 1,
               'events.jsonl shrank')
        expect('events hatch requires exact 1, not a truthy string',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=true', 'bash',
                   'scripts/check-board.sh', '--staged-index'), 1, '2 row(s) lost')
        expect('declared events deletion accepted',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'git', 'commit', '-qm',
                   'intentional event deletion'), 0, 'shrink explicitly allowed')
        expect('CI rejects a committed events shrink against the first parent',
               run(repo, 'bash', 'scripts/check-board.sh'), 1, 'HEAD^1=3')
        expect('CI accepts an explicitly declared events deletion',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'bash',
                   'scripts/check-board.sh'), 0, 'shrink explicitly allowed')

        # DF-CRIER-434: the DEFAULT run prints `board: scope working tree` and
        # validates the working tree, but its retention arm compared two COMMITS
        # (HEAD^1 vs HEAD). Measured pre-fix on the first fixture below: 5 rows
        # in HEAD and the index, the worktree cut to 1 row, and the default
        # command printed `board: validation PASS — 1 row(s) accepted` with exit
        # 0 — a green over the tree it named, checked against something else.
        # Default mode must retain BOTH comparisons and label which one fired.
        repo = fixture('worktree-shrink')
        board = board_rows(repo, 5)
        event_rows(repo, 2)
        seed_commit(repo, 'five-row baseline (hooks disabled for setup)')
        keep_rows(board, [1])
        index_rows = git(repo, 'show', ':.coding-hermes/board/tasks.jsonl').stdout.splitlines()
        head_rows = git(repo, 'show', 'HEAD:.coding-hermes/board/tasks.jsonl').stdout.splitlines()
        worktree_rows = board.read_text().splitlines()
        if (len(head_rows), len(index_rows), len(worktree_rows)) != (5, 5, 1):
            print('commit selftest: worktree-shrink fixture premise (HEAD 5, index 5, '
                  'worktree 1 — index untouched): FAIL')
            failures += 1
        else:
            print('commit selftest: worktree-shrink fixture premise (HEAD 5, index 5, '
                  'worktree 1 — index untouched): PASS')
        refused = default_check(repo)
        show_stderr('unstaged worktree shrink, default (no-flag) command', refused)
        expect('default mode refuses a worktree shrink with an untouched index',
               refused, 1, 'worktree vs HEAD')
        expect_stderr('worktree shrink names worktree vs HEAD, the file, both counts '
                      'and the bounded lost-id list', refused,
                      ['retention: worktree vs HEAD',
                       '.coding-hermes/board/tasks.jsonl shrank: HEAD=5, worktree=1; '
                       '4 row(s) lost',
                       'lost ids (4 total; first 20): SELFTEST-2, SELFTEST-3, '
                       'SELFTEST-4, SELFTEST-5',
                       'shrink refused: .coding-hermes/board/tasks.jsonl '
                       '(retention: worktree vs HEAD)'],
                      forbidden=['validation PASS'])
        expect('the same tree under --staged-index still validates the 5-row index',
               staged_check(repo), 0, '5 row(s) accepted')

        # The CI case: the shrink is COMMITTED and the checkout is clean, so
        # worktree == HEAD and only the HEAD vs HEAD^1 comparison can fire.
        repo = fixture('committed-shrink-clean-tree')
        board = board_rows(repo, 5)
        event_rows(repo, 2)
        seed_commit(repo, 'five-row baseline (hooks disabled for setup)')
        keep_rows(board, [1])
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        seed_commit(repo, 'shrink committed without hooks (the CI case)')
        if git(repo, 'status', '--porcelain', '--',
               '.coding-hermes/board').stdout.strip():
            print('commit selftest: committed-shrink fixture premise (clean checkout, '
                  'worktree == HEAD): FAIL')
            failures += 1
        else:
            print('commit selftest: committed-shrink fixture premise (clean checkout, '
                  'worktree == HEAD): PASS')
        refused = default_check(repo)
        show_stderr('committed shrink on a clean checkout, default (CI) command', refused)
        expect('default mode (the CI command) refuses a committed shrink',
               refused, 1, 'HEAD vs HEAD^1')
        expect_stderr('committed shrink names HEAD vs HEAD^1, the file, both counts and '
                      'the bounded lost-id list', refused,
                      ['retention: HEAD vs HEAD^1',
                       '.coding-hermes/board/tasks.jsonl shrank: HEAD^1=5, HEAD=1; '
                       '4 row(s) lost',
                       'lost ids (4 total; first 20): SELFTEST-2, SELFTEST-3, '
                       'SELFTEST-4, SELFTEST-5',
                       'shrink refused: .coding-hermes/board/tasks.jsonl '
                       '(retention: HEAD vs HEAD^1)',
                       'retention: worktree vs HEAD — 2 file(s) compared, no shrink'],
                      forbidden=['shrank: HEAD=5, worktree=1'])

        repo = fixture('retention-healthy')
        board_rows(repo, 3)
        event_rows(repo, 2)
        seed_commit(repo, 'healthy baseline (hooks disabled for setup)')
        accepted = default_check(repo)
        show_stderr('healthy tree (worktree == HEAD == HEAD^1), default command', accepted)
        expect('healthy tree accepted by the default command',
               accepted, 0, 'validation PASS')
        expect_stderr('healthy default run states both comparisons it made', accepted,
                      ['retention: worktree vs HEAD — 2 file(s) compared, no shrink',
                       'retention: HEAD vs HEAD^1 — 2 file(s) compared, no shrink'])

        repo = fixture('events-shrink-no-id')
        board_rows(repo, 2)
        events = event_rows(repo, 3, no_id_at=2)
        seed_commit(repo, 'baseline: 3 events, row 2 has no id (hooks disabled)')
        keep_rows(events, [1, 3])
        git(repo, 'add', '.coding-hermes/board/events.jsonl')
        rejected = staged_check(repo)
        expect('lost event row without an id is refused, not skipped', rejected, 1,
               'events.jsonl shrank')
        expect_stderr('id-less lost row is named by its line number', rejected,
                      ['1 row(s) lost', 'lost ids (1 total; first 20): line 2 (no id)'])

        repo = fixture('tasks-only-shrink-scope')
        board_rows(repo, 3)
        event_rows(repo, 3)
        seed_commit(repo, 'baseline: 3 tasks, 3 events (hooks disabled for setup)')
        board_rows(repo, 1)
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        rejected = staged_check(repo)
        expect('tasks-only shrink still rejected', rejected, 1, 'tasks.jsonl shrank')
        expect_stderr('tasks-only shrink names tasks.jsonl and only tasks.jsonl',
                      rejected,
                      ['tasks.jsonl shrank: HEAD=3, index=1; 2 row(s) lost',
                       'shrink refused: .coding-hermes/board/tasks.jsonl'],
                      forbidden=['events.jsonl shrank'])

        repo = fixture('both-board-files-grow')
        board_rows(repo, 1)
        event_rows(repo, 1)
        seed_commit(repo, 'baseline: 1 task, 1 event (hooks disabled for setup)')
        board_rows(repo, 3)
        event_rows(repo, 4)
        git(repo, 'add', '.coding-hermes/board')
        expect('both board files growing accepted',
               git(repo, 'commit', '-qm', 'both grow', check=False), 0,
               'validation PASS')
        expect('CI accepts both board files growing',
               run(repo, 'bash', 'scripts/check-board.sh'), 0, 'validation PASS')

        repo = fixture('same-count-edit')
        board = board_rows(repo, 2)
        seed_commit(repo, 'two-row baseline (hooks disabled for setup)')
        rows = [json.loads(line) for line in board.read_text().splitlines()]
        rows[0]['title'] = 'ordinary row edit'
        board.write_text(''.join(json.dumps(row) + '\n' for row in rows))
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        expect('normal row edit (same count) accepted',
               git(repo, 'commit', '-qm', 'normal edit', check=False), 0, 'validation PASS')

        repo = fixture('declared-deletion')
        board = board_rows(repo, 2)
        seed_commit(repo, 'two-row baseline (hooks disabled for setup)')
        board.write_text(board.read_text().splitlines()[0] + '\n')
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        expect('undeclared deletion of one row rejected',
               git(repo, 'commit', '-qm', 'delete row', check=False), 1, '1 row(s) lost')
        expect('declared deletion of one row accepted',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'git', 'commit', '-qm',
                   'intentional deletion'), 0, 'shrink explicitly allowed')
        expect('CI rejects committed shrink against first parent',
               run(repo, 'bash', 'scripts/check-board.sh'), 1, 'HEAD^1=2')
        expect('CI accepts explicitly declared deletion',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'bash', 'scripts/check-board.sh'),
               0, 'shrink explicitly allowed')
        board_rows(repo, 2)
        expect('CI cannot borrow an unstaged repair to hide committed shrink',
               run(repo, 'bash', 'scripts/check-board.sh'), 1, 'HEAD=1')
        seed_commit(repo, 'restored two-row baseline (hooks disabled for setup)')
        poison(repo)
        invalid = board.read_text().splitlines()[-1]
        board.write_text(invalid + '\n')
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        expect('hatch does not bypass row validation on a shrink',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'bash', 'scripts/check-board.sh',
                   '--staged-index'), 1, 'INVALID-1')
        board.write_text('')
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        expect('empty candidate reports both counts before refusal',
               run(repo, 'bash', 'scripts/check-board.sh', '--staged-index'),
               1, 'HEAD=2, index=0')
        expect('hatch does not permit empty board',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'bash', 'scripts/check-board.sh',
                   '--staged-index'), 1, 'is empty')
        git(repo, 'rm', '-f', '.coding-hermes/board/tasks.jsonl')
        expect('hatch does not permit whole-file deletion',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'git', 'commit', '-qm',
                   'remove whole board'), 1, 'required index path')

        repo = fixture('new-board')
        board = repo / '.coding-hermes/board/tasks.jsonl'
        good = board.read_bytes()
        git(repo, 'rm', '.coding-hermes/board/tasks.jsonl')
        seed_commit(repo, 'HEAD without board (hooks disabled for setup)')
        board.write_bytes(good)
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        expect('brand-new board with no HEAD board accepted',
               git(repo, 'commit', '-qm', 'first board', check=False), 0, 'validation PASS')
        expect('CI accepts first board when parent has none',
               run(repo, 'bash', 'scripts/check-board.sh'), 0, 'validation PASS')

        repo = fixture('root-commit')
        git(repo, 'checkout', '--orphan', 'first-board')
        expect('unborn HEAD accepts staged first board',
               run(repo, 'bash', 'scripts/check-board.sh', '--staged-index'),
               0, 'validation PASS')
        seed_commit(repo, 'root board (hooks disabled for setup)')
        expect('CI accepts genuine root board commit without parent',
               run(repo, 'bash', 'scripts/check-board.sh'), 0, 'validation PASS')

        # A shallow checkout is NOT a root commit. CI must fetch the baseline,
        # not silently accept the very loss it was asked to measure.
        repo = fixture('shallow-source')
        probe(repo)
        seed_commit(repo, 'second commit (hooks disabled for setup)')
        shallow = root / 'shallow-clone'
        run(root, 'git', 'clone', '-q', '--depth=1', repo.as_uri(), str(shallow), check=True)
        expect('CI refuses unavailable shallow baseline',
               run(shallow, 'bash', 'scripts/check-board.sh'), 2, 'shallow history')
        git(shallow, 'fetch', '-q', '--deepen=1')
        expect('CI accepts depth-two checkout with available baseline',
               run(shallow, 'bash', 'scripts/check-board.sh'), 0, 'validation PASS')

        repo = fixture('dedupe')
        board = board_rows(repo, 1)
        good = board.read_text()
        board.write_text(good + good + '\n')
        seed_commit(repo, 'duplicate fixture (hooks disabled for setup)')
        board.write_text(good.rstrip() + '\n\n')
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        expect('dedupe counts records, not unique ids or blank lines',
               git(repo, 'commit', '-qm', 'dedupe', check=False), 1,
               'lost ids (1 total; first 20): SELFTEST-1')
        expect('explicitly declared dedupe accepted',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'git', 'commit', '-qm',
                   'intentional dedupe'), 0, 'shrink explicitly allowed')

        # A real merge's pre-commit HEAD is the eventual first parent. The
        # feature was seeded without hooks only to construct the hostile input.
        repo = fixture('merge-shrink')
        board = board_rows(repo, 3)
        seed_commit(repo, 'three-row baseline (hooks disabled for setup)')
        git(repo, 'checkout', '-qb', 'feature')
        board.write_text(board.read_text().splitlines()[0] + '\n')
        seed_commit(repo, 'shrunk feature (hooks disabled for setup)')
        git(repo, 'checkout', '-q', 'main')
        probe(repo)
        seed_commit(repo, 'divergence (hooks disabled for setup)')
        result = git(repo, 'merge', '--no-ff', '--no-edit', 'feature', check=False)
        expect('real merge rejects shrink against its first parent', result, 1, 'HEAD=3')
        expect('merge shrink retains board-only scope disclosure', result, 1,
               'pre-merge-commit: this entrypoint validates the board only')
        expect('explicitly declared merge deletion accepted',
               run(repo, 'env', 'BOARD_ALLOW_SHRINK=1', 'git', 'commit', '-qm',
                   'intentional merge deletion'), 0, 'shrink explicitly allowed')
        expect('CI merge check uses first parent, not shrunk second parent',
               run(repo, 'bash', 'scripts/check-board.sh'), 1, 'HEAD^1=3')

        for board_file in ('tasks.jsonl', 'events.jsonl', 'board.jsonl', 'fixtures.jsonl'):
            repo = fixture('delete-' + board_file)
            path = '.coding-hermes/board/' + board_file
            git(repo, 'rm', path)
            expect('staged deletion ' + path,
                   git(repo, 'commit', '-qm', 'delete board', check=False), 1, path)

        repo = fixture('staged-invalid-header')
        header = repo / '.coding-hermes/board/board.jsonl'
        good = header.read_bytes()
        header.write_text('{broken header\n')
        git(repo, 'add', '.coding-hermes/board/board.jsonl')
        header.write_bytes(good)
        expect('whole index includes staged topology-A header, not its worktree repair',
               git(repo, 'commit', '-qm', 'invalid header', check=False), 1, 'board.jsonl')

        unrelated = root / 'unrelated'
        unrelated.mkdir()
        git(unrelated, 'init', '-q', '-b', 'main')
        (unrelated / 'plain.txt').write_text('base\n')
        seed_commit(unrelated, 'unrelated base')
        unrelated_hooks_before = hooks_snapshot(unrelated)
        assert not (unrelated / 'scripts/check-board.sh').exists()

        repo = fixture('installer')
        # Expose the isolated inherited hooksPath that caused DF-CRIER-412. The
        # installer must replace it only for crier, never write into it.
        git(repo, 'config', '--unset', 'core.hooksPath')
        effective = git(repo, 'config', '--get', 'core.hooksPath').stdout.strip()
        assert effective == str(global_hooks)
        result = run(repo, 'make', '--no-print-directory', '-f', str(source / 'Makefile'),
                     'install-hooks')
        expect('installer ignores inherited global hooksPath', result, 0,
               'repository-scoped hooks active')
        installed_hooks = repo / '.git/hooks'
        for hook in ('pre-commit', 'pre-merge-commit', 'pre-applypatch'):
            installed = installed_hooks / hook
            tracked = source / 'scripts/hooks' / hook
            if (not installed.is_file() or not tracked.is_file()
                    or installed.read_bytes() != tracked.read_bytes()
                    or not os.access(installed, os.X_OK)):
                print(f'commit selftest: repository-local installer missing/incorrect {hook}: FAIL')
                failures += 1
        configured = git(repo, 'config', '--local', '--get', 'core.hooksPath',
                         check=False)
        if (configured.returncode != 0
                or configured.stdout.strip() != str(installed_hooks.resolve())):
            print('commit selftest: local core.hooksPath does not select repository hooks: FAIL')
            failures += 1
        else:
            print('commit selftest: local core.hooksPath selects repository hooks: PASS')
        if global_hooks.exists() and any(global_hooks.iterdir()):
            print('commit selftest: inherited global hooksPath was modified: FAIL')
            failures += 1
        else:
            print('commit selftest: inherited global hooksPath untouched: PASS')
        if hooks_snapshot(unrelated) != unrelated_hooks_before:
            print('commit selftest: unrelated repository .git/hooks changed: FAIL')
            failures += 1
        else:
            print('commit selftest: unrelated repository .git/hooks untouched: PASS')
        (unrelated / 'plain.txt').write_text('after crier install\n')
        git(unrelated, 'add', 'plain.txt')
        expect('unrelated repository without crier checker still commits',
               git(unrelated, 'commit', '-qm', 'unrelated change', check=False), 0)

        result = run(repo, 'make', '--no-print-directory', '-f', str(source / 'Makefile'),
                     'install-hooks')
        expect('repository-local installer repeat is idempotent', result, 0,
               'repository-scoped hooks active')
        if result.stdout.count('already installed') != 3:
            print('commit selftest: all three repository-local hooks must be idempotent: FAIL')
            failures += 1

        (repo / 'scripts/check-board.sh').unlink()
        probe(repo)
        expect('repository-local crier hook stays fail-closed without checker',
               git(repo, 'commit', '-qm', 'missing checker', check=False), 1,
               'UNAVAILABLE')

        repo = fixture('deleted-index-repaired-worktree')
        path = '.coding-hermes/board/tasks.jsonl'
        good = (repo / path).read_bytes()
        git(repo, 'rm', path)
        (repo / path).write_bytes(good)
        expect('staged deletion cannot borrow an unstaged restored file',
               git(repo, 'commit', '-qm', 'deleted index', check=False), 1, path)

        repo = fixture('deletion-classification')
        path = '.coding-hermes/board/notes.txt'
        (repo / path).write_text('optional note\n')
        seed_commit(repo, 'optional note fixture (hooks disabled for setup)')
        git(repo, 'rm', path)
        expect('D is classified as a board path before the absent-file skip',
               git(repo, 'commit', '-qm', 'remove optional note', check=False), 0,
               '1 board path(s) also in this diff')

        repo = fixture('unstaged-repair')
        board = repo / '.coding-hermes/board/tasks.jsonl'
        good = board.read_bytes()
        poison(repo)
        seed_commit(repo, 'invalid HEAD fixture (hooks disabled for setup)')
        board.write_bytes(good)
        probe(repo)
        assert not git(repo, 'diff', '--cached', '--name-only', '--',
                       '.coding-hermes/board').stdout.strip()
        expect('invalid HEAD/index plus unstaged repair',
               git(repo, 'commit', '-qm', 'non-board change', check=False), 1, 'INVALID-1')

        repo = fixture('missing-config')
        (repo / '.gitreins/config.yaml').unlink()
        poison(repo)
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        expect('missing .gitreins/config.yaml plus invalid staged board',
               git(repo, 'commit', '-qm', 'invalid board', check=False), 1, 'INVALID-1')

        repo = fixture('staged-invalid')
        board = repo / '.coding-hermes/board/tasks.jsonl'
        good = board.read_bytes()
        poison(repo)
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        board.write_bytes(good)
        expect('DF-CRIER-399 staged invalid plus repaired worktree',
               git(repo, 'commit', '-qm', 'invalid index', check=False), 1, 'INVALID-1')

        repo = fixture('every-commit')
        poison(repo)
        seed_commit(repo, 'invalid HEAD fixture (hooks disabled for setup)')
        probe(repo)
        expect('DF-CRIER-389 non-board commit on unchanged invalid board',
               git(repo, 'commit', '-qm', 'non-board change', check=False), 1, 'INVALID-1')

        for config_present in (True, False):
            repo = fixture('positive-' + str(config_present))
            if not config_present:
                (repo / '.gitreins/config.yaml').unlink()
            probe(repo)
            result = git(repo, 'commit', '-qm', 'valid board', check=False)
            expect('canonical index with config ' + str(config_present), result, 0,
                   'scope staged index')
            if not config_present:
                assert 'stub gitreins' not in result.stdout

        repo = fixture('missing-checker')
        (repo / 'scripts/check-board.sh').unlink()
        probe(repo)
        expect('missing checker on non-board commit',
               git(repo, 'commit', '-qm', 'missing checker', check=False), 1, 'UNAVAILABLE')

        def expect_board_only_scope(hook, result, expected):
            nonlocal failures
            scope = (f'gate scope: {hook}: this entrypoint validates the board only; '
                     'content checks reach it through CI (Tier-1, gofmt, shell/YAML '
                     'and Makefile/Dockerfile checks are NOT RUN here).')
            expect(hook + ' discloses board-only scope', result, expected, scope)
            if 'stub gitreins' in result.stdout:
                print(f'commit selftest: {hook} unexpectedly ran Tier-1: FAIL')
                failures += 1
            # Keep real Git hook output visible as reproducible scope evidence.
            for line in result.stdout.splitlines():
                if line == scope:
                    print(line)

        for invalid in (True, False):
            repo = fixture('merge-' + str(invalid))
            git(repo, 'checkout', '-qb', 'feature')
            if invalid:
                poison(repo)
            (repo / 'feature.txt').write_text('feature\n')
            seed_commit(repo, 'feature fixture (hooks disabled for setup)')
            git(repo, 'checkout', '-q', 'main')
            probe(repo)
            seed_commit(repo, 'divergence fixture (hooks disabled for setup)')
            result = git(repo, 'merge', '--no-ff', '--no-edit', 'feature', check=False)
            expected = 1 if invalid else 0
            expect('real merge invalid board ' + str(invalid), result, expected,
                   'INVALID-1' if invalid else 'scope staged index')
            expect_board_only_scope('pre-merge-commit', result, expected)

        for invalid in (True, False):
            repo = fixture('am-' + str(invalid))
            base = git(repo, 'rev-parse', 'HEAD').stdout.strip()
            git(repo, 'checkout', '-qb', 'feature')
            if invalid:
                poison(repo)
            (repo / 'feature.txt').write_text('feature\n')
            seed_commit(repo, 'patch fixture (hooks disabled for setup)')
            patch = root / ('patch-' + str(invalid) + '.mbox')
            patch.write_text(git(repo, 'format-patch', '-1', '--stdout').stdout)
            git(repo, 'checkout', '-qb', 'apply-target', base)
            result = git(repo, 'am', str(patch), check=False)
            expected = 1 if invalid else 0
            expect('real git am invalid board ' + str(invalid), result, expected,
                   'INVALID-1' if invalid else 'scope staged index')
            expect_board_only_scope('pre-applypatch', result, expected)

        repo = fixture('no-verify')
        poison(repo)
        git(repo, 'add', '.coding-hermes/board/tasks.jsonl')
        expect('--no-verify still bypasses local hooks (CI backstop required)',
               git(repo, 'commit', '--no-verify', '-qm', 'bypass fixture', check=False), 0)
        expect('CI command rejects the checked-out invalid bypass commit',
               run(repo, 'bash', 'scripts/check-board.sh'), 1, 'INVALID-1')
        board = repo / '.coding-hermes/board/tasks.jsonl'
        board.unlink()
        expect('CI command refuses an absent board',
               run(repo, 'bash', 'scripts/check-board.sh'), 1, 'tasks.jsonl')

    print(f'commit selftest: {failures} failure(s)')
    return 1 if failures else 0


if __name__ == '__main__':
    raise SystemExit(main())
