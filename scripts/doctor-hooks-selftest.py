#!/usr/bin/env python3
"""DF-CRIER-421: exercise the real doctor/installer in isolated Git repositories."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
HOOKS = ("pre-commit", "pre-merge-commit", "pre-applypatch")
MARKER = 'bash "$BOARD_CHECKER" -q --staged-index'


def main():
    # Hooks inherit Git's environment during commits. Never let those variables
    # redirect a fixture's config, index or common directory into the live repo.
    env = {key: val for key, val in os.environ.items() if not key.startswith("GIT_")}
    env["GIT_CONFIG_NOSYSTEM"] = "1"
    with tempfile.TemporaryDirectory(prefix="doctor-hooks-selftest-", dir=os.environ.get("TMPDIR", "/tmp")) as tmp:
        base = Path(tmp)
        env["GIT_CONFIG_GLOBAL"] = str(base / "global.config")
        (base / "global.config").write_text('[core]\n\thooksPath = /deliberately-wrong-global-hooks\n')
        count = 0

        def run(cwd, *argv):
            return subprocess.run(argv, cwd=cwd, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)

        def require(cwd, *argv):
            result = run(cwd, *argv)
            assert result.returncode == 0, (argv, result.returncode, result.stdout)
            return result.stdout

        def fixture(name):
            repo = base / name
            repo.mkdir()
            require(repo, "git", "init", "-q")
            (repo / "scripts/hooks").mkdir(parents=True)
            for hook in HOOKS:
                shutil.copy2(ROOT / "scripts/hooks" / hook, repo / "scripts/hooks" / hook)
            shutil.copy2(ROOT / "scripts/doctor-hooks.sh", repo / "scripts/doctor-hooks.sh")
            shutil.copy2(ROOT / "Makefile", repo / "Makefile")
            require(repo, "make", "install-hooks")
            return repo

        def check(label, repo, expected, diagnostic="", doctor=None):
            nonlocal count
            result = run(repo, "bash", str(doctor or ROOT / "scripts/doctor-hooks.sh"))
            assert (result.returncode == 0) == expected, (label, result.returncode, result.stdout)
            assert diagnostic in result.stdout, (label, diagnostic, result.stdout)
            if expected and doctor is None:
                assert len(result.stdout.splitlines()) == 3, (label, result.stdout)
                for hook in HOOKS:
                    assert f"doctor-hooks: OK {hook} " in result.stdout, (label, result.stdout)
            if not expected:
                assert "doctor-hooks: OK" not in result.stdout, (label, result.stdout)
            print(f"{label}: exit {result.returncode}\n{result.stdout.rstrip()}", flush=True)
            count += 1

        good = fixture("correct install with spaces")
        check("correct install (global override ignored)", good, True)
        require(good, "make", "doctor-hooks")
        nested = good / "nested"
        nested.mkdir()
        check("invoked from subdirectory", nested, True)
        require(good, "git", "config", "--local", "core.hooksPath", ".git/hooks")
        check("relative local hooksPath", good, True)

        unset = fixture("unset")
        require(unset, "git", "config", "--local", "--unset", "core.hooksPath")
        check("unset local hooksPath", unset, False, "local core.hooksPath is unset")
        wrong = fixture("wrong")
        require(wrong, "git", "config", "--local", "core.hooksPath", str(base))
        check("wrong local hooksPath", wrong, False, "local core.hooksPath resolves to")

        for hook in HOOKS:
            missing = fixture(f"missing-{hook}")
            (missing / ".git/hooks" / hook).unlink()
            check(f"missing {hook}", missing, False, f"installed {hook} is missing")
            different = fixture(f"different-{hook}")
            with (different / ".git/hooks" / hook).open("a") as out:
                out.write("\n# byte drift\n")
            check(f"byte-different {hook}", different, False, f"installed {hook} differs from")
            noexec = fixture(f"nonexec-{hook}")
            (noexec / ".git/hooks" / hook).chmod(0o644)
            check(f"non-executable {hook}", noexec, False, f"installed {hook} is not executable")
            # The documented repair must fix modes even when bytes already match.
            require(noexec, "make", "install-hooks")
            check(f"repair non-executable {hook}", noexec, True)

        stale = fixture("stale-generation")
        source = stale / "scripts/hooks/pre-commit"
        original = source.read_text()
        assert MARKER in original, "board-arm marker changed: update the explicit contract"
        source.write_text(original.replace(MARKER, ": # stale generation: board arm absent"))
        shutil.copy2(source, stale / ".git/hooks/pre-commit")
        # Matching stale source AND installed bytes isolates the marker check
        # from cmp; a byte-different stale hook is also tested independently.
        check("matching stale generation", stale, False, "pre-commit lacks board arm marker")
        installed_stale = fixture("installed-stale-generation")
        dst = installed_stale / ".git/hooks/pre-commit"
        dst.write_text(dst.read_text().replace(MARKER, ": # stale"))
        check("installed stale generation", installed_stale, False, "pre-commit lacks board arm marker")

        foreign = fixture("foreign-repo")
        (foreign / "scripts/hooks/pre-commit").unlink()
        check("foreign repo without source", foreign, False, "source scripts/hooks/pre-commit is missing")

        # A real linked worktree shares common hooks and local config, not .git/hooks
        # relative to the worktree's root. Make one before installing its hooks.
        parent = base / "parent"
        parent.mkdir()
        require(parent, "git", "init", "-q")
        # Empty initial commit has no installed hooks; isolate the global setting
        # by using an empty global config (never change the host's config).
        (base / "global.config").write_text("")
        require(parent, "git", "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "fixture")
        linked = base / "linked"
        require(parent, "git", "worktree", "add", "-qb", "doctor-fixture", str(linked))
        (linked / "scripts/hooks").mkdir(parents=True)
        for hook in HOOKS:
            shutil.copy2(ROOT / "scripts/hooks" / hook, linked / "scripts/hooks" / hook)
        shutil.copy2(ROOT / "scripts/doctor-hooks.sh", linked / "scripts/doctor-hooks.sh")
        shutil.copy2(ROOT / "Makefile", linked / "Makefile")
        require(linked, "make", "install-hooks")
        check("linked worktree common hooks", linked, True)
        require(linked, "make", "doctor-hooks")
        # A worktree-specific override must not silently defeat a valid local value.
        require(parent, "git", "config", "extensions.worktreeConfig", "true")
        require(linked, "git", "config", "--worktree", "core.hooksPath", str(base))
        check("effective worktree override", linked, False, "effective core.hooksPath")

        neutered = base / "doctor-neutered.sh"
        text = (ROOT / "scripts/doctor-hooks.sh").read_text()
        call = 'doctor_hooks "$@"\n'
        assert text.count(call) == 1, "NEUTER transform must replace exactly the verdict call"
        neutered.write_text(text.replace(call, 'doctor_hooks "$@"\nexit 0 # NEUTER: verdict forced to success\n'))
        assert neutered.read_bytes() != (ROOT / "scripts/doctor-hooks.sh").read_bytes()
        check("NEUTER accepts unset fixture rejected by real doctor", unset, True, doctor=neutered)
        print(f"doctor-hooks selftest: PASS ({count} cases, including NEUTER)")


if __name__ == "__main__":
    main()
