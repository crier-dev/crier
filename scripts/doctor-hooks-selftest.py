#!/usr/bin/env python3
"""DF-CRIER-421: exercise the real doctor/installer in isolated Git repositories.

DF-CRIER-430: every hook is disarmed in turn and must be refused. A stale
GENERATION — the tracked source edited down to `exit 0 # DISARMED` and reinstalled
— is byte-identical to the source it was made from, so the byte comparison passes
and only the marker check can see it. Each declared marker is also dropped on its
own, so a marker that is not load-bearing cannot hide behind its siblings.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
HOOKS = ("pre-commit", "pre-merge-commit", "pre-applypatch")

# The explicit contract: the lines each tracked hook carries, as the doctor
# requires them. Hooks are re-worded only together with this table (the premise
# assertion below fails loudly otherwise) and with scripts/doctor-hooks.sh.
HOOK_ARM_MARKER = 'bash "$BOARD_CHECKER" -q --staged-index'
CHECKER_INVOCATION = 'exec bash "$checker" --staged-index'
MERGE_DISCLOSURE = (
    r"printf '%s\n' 'gate scope: pre-merge-commit: this entrypoint validates the board"
    r" only; content checks reach it through CI (Tier-1, gofmt, shell/YAML and"
    r" Makefile/Dockerfile checks are NOT RUN here).'"
)
APPLYPATCH_DISCLOSURE = (
    r"printf '%s\n' 'gate scope: pre-applypatch: this entrypoint validates the board"
    r" only; content checks reach it through CI (Tier-1, gofmt, shell/YAML and"
    r" Makefile/Dockerfile checks are NOT RUN here).'"
)
# hook -> [(marker line, label)] in the order the doctor checks them.
MARKERS = {
    "pre-commit": [(HOOK_ARM_MARKER, "board arm marker")],
    "pre-merge-commit": [
        (CHECKER_INVOCATION, "board arm marker"),
        (MERGE_DISCLOSURE, "gate-scope disclosure line"),
    ],
    "pre-applypatch": [
        (CHECKER_INVOCATION, "board arm marker"),
        (APPLYPATCH_DISCLOSURE, "gate-scope disclosure line"),
    ],
}
# The disarmed generation the row measured: the hook's whole function is gone.
DISARM = (
    "#!/usr/bin/env bash\n"
    "# DF-CRIER-430 staleness probe: this generation has no board arm at all\n"
    "exit 0 # DISARMED\n"
)


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

        # DF-CRIER-430: pin the marker contract against the tracked sources before
        # using it, so a re-worded hook fails HERE (loudly) instead of quietly
        # turning a refusal into a pass.
        for hook, markers in MARKERS.items():
            source = (ROOT / "scripts/hooks" / hook).read_text()
            for marker, label in markers:
                assert f"{marker}\n" in source, (
                    f"{hook}: the tracked hook no longer carries its {label}: {marker!r} — "
                    "update MARKERS here and scripts/doctor-hooks.sh together with it")

        for hook in HOOKS:
            invocation, label = MARKERS[hook][0]
            stale_diagnostic = f"installed {hook} lacks {label} ({invocation})"
            # (a) matching stale generation: tracked source AND installed copy are the
            # same disarmed text, so cmp passes and the marker check is the only thing
            # between this repository and a dead gate.
            matching = fixture(f"stale-generation-{hook}")
            (matching / "scripts/hooks" / hook).write_text(DISARM)
            (matching / ".git/hooks" / hook).write_text(DISARM)
            check(f"matching stale generation ({hook})", matching, False, stale_diagnostic)
            # (b) installed-only stale generation: the tracked source is intact, so
            # the refusal is attributed to the missing marker, never to byte drift.
            installed_stale = fixture(f"installed-stale-generation-{hook}")
            (installed_stale / ".git/hooks" / hook).write_text(DISARM)
            check(f"installed stale generation ({hook})", installed_stale, False, stale_diagnostic)
            # (c) every marker is load-bearing: drop ONE line at a time from the
            # installed copy (tracked source intact) and require the refusal to name
            # that exact line.
            for marker, marker_label in MARKERS[hook]:
                hole = fixture(f"marker-hole-{hook}-{marker_label.replace(' ', '-')}")
                dst = hole / ".git/hooks" / hook
                text = dst.read_text()
                assert f"{marker}\n" in text, (hook, marker_label, marker)
                dst.write_text(text.replace(f"{marker}\n", "", 1))
                check(f"dropped {marker_label} ({hook})", hole, False,
                      f"installed {hook} lacks {marker_label} ({marker})")

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
        # DF-CRIER-430: the same control over the NEW refusal. The disarmed fixture is
        # refused by the real doctor and accepted once the verdict is forced, so the
        # refusal above is the marker check rather than some unrelated exit path.
        disarmed = fixture("neuter-disarmed-pre-merge-commit")
        (disarmed / ".git/hooks/pre-merge-commit").write_text(DISARM)
        check("NEUTER fixture rejected by the real doctor", disarmed, False, "lacks board arm marker")
        check("NEUTER accepts a disarmed fixture the real doctor rejects", disarmed, True,
              "installed pre-merge-commit lacks board arm marker", doctor=neutered)
        print(f"doctor-hooks selftest: PASS ({count} cases, including NEUTER)")


if __name__ == "__main__":
    main()
