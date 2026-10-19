#!/usr/bin/env python3
"""Exercise board gates through Git, with real boardctl and scratch-only writes.

--source can point at a historical source snapshot for reproducible RED controls.
Only gitreins is stubbed: its unrelated Go checks must not mask board failures.
"""
import argparse
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
        tools = root / 'bin'
        tools.mkdir()
        stub = tools / 'gitreins'
        stub.write_text('#!/bin/sh\nprintf "stub gitreins guard (board-only test)\\n"\nexit 0\n')
        stub.chmod(0o755)
        env = os.environ.copy()
        # Do not inherit a caller's alternate index/worktree or host Git hooks.
        for key in list(env):
            if key.startswith('GIT_'):
                del env[key]
        env.update(GIT_CONFIG_GLOBAL='/dev/null', GIT_CONFIG_NOSYSTEM='1',
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
                ok = ok and diagnostic in result.stdout
            print(f'commit selftest: {name}: exit {result.returncode}, expected {expected}: '
                  + ('PASS' if ok else 'FAIL'))
            if not ok:
                print(result.stdout)
                failures += 1

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

        repo = fixture('installer')
        # Even with pre-commit already identical, install the other entrypoints
        # into the effective hooksPath, not the inactive .git/hooks directory.
        for hook in ('pre-merge-commit', 'pre-applypatch'):
            (repo / '.hooks' / hook).unlink(missing_ok=True)
        result = run(repo, 'make', '--no-print-directory', '-f', str(source / 'Makefile'),
                     'install-hooks')
        expect('installer fills merge/applypatch beside identical pre-commit', result, 0)
        for hook in ('pre-commit', 'pre-merge-commit', 'pre-applypatch'):
            installed = repo / '.hooks' / hook
            tracked = source / 'scripts/hooks' / hook
            if (not installed.is_file() or not tracked.is_file()
                    or installed.read_bytes() != tracked.read_bytes()
                    or not os.access(installed, os.X_OK)):
                print(f'commit selftest: installer missing/incorrect {hook}: FAIL')
                failures += 1
        result = run(repo, 'make', '--no-print-directory', '-f', str(source / 'Makefile'),
                     'install-hooks')
        expect('installer repeat is idempotent', result, 0)
        if result.stdout.count('already installed') != 3:
            print('commit selftest: all three hooks must be idempotent: FAIL')
            failures += 1

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
            expect('real merge invalid board ' + str(invalid),
                   git(repo, 'merge', '--no-ff', '--no-edit', 'feature', check=False),
                   1 if invalid else 0, 'INVALID-1' if invalid else 'scope staged index')

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
            expect('real git am invalid board ' + str(invalid),
                   git(repo, 'am', str(patch), check=False),
                   1 if invalid else 0, 'INVALID-1' if invalid else 'scope staged index')

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
