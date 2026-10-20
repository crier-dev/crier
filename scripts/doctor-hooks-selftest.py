#!/usr/bin/env python3
"""DF-CRIER-421: exercise the real doctor/installer in isolated Git repositories.

DF-CRIER-430: every hook is disarmed in turn and must be refused. A stale
GENERATION — the tracked source edited down to `exit 0 # DISARMED` and reinstalled
— is byte-identical to the source it was made from, so the byte comparison passes
and only the marker check can see it. Each declared marker is also dropped on its
own, so a marker that is not load-bearing cannot hide behind its siblings.

DF-CRIER-433: presence is not reachability, and this file now pins BOTH halves.
  * STATIC PRECONDITION — `exit 0`/`return 0` PREPENDED to the tracked source,
    reinstalled with the whole body and every marker line kept, must be refused by
    naming the line and the arm it makes unreachable.
  * EXECUTION PROBE — the acceptance cases assert the doctor's own probe evidence
    line for every hook, so a green cannot come from exit 0 alone: the installed
    hook really ran, accepted a healthy fixture, and refused a staged board shrink.
    A disarm the static arm CANNOT see (`[ -n "$X" ] || exit 0`) must be caught by
    the probe, and a NEUTER control that forces the probe's verdict to success must
    ACCEPT that same fixture the real doctor rejects — so the probe, not an
    unrelated exit path, is what refused it.

DF-CRIER-437: the probe presents the Git-provided hook environment instead of
deleting it. A hook keyed on `GIT_INDEX_FILE` — which real Git exports to these
entrypoints (measured on git 2.53.0: `.git/index` plus `GIT_PREFIX` for
pre-commit/pre-merge-commit) — was FALSELY REFUSED as a board arm that cannot run.
Now the probe SYNTHESIZES GIT_DIR/GIT_INDEX_FILE/GIT_PREFIX from its fixture, and a
case below pins the acceptance; the BOARD_ALLOW_SHRINK acceptance case additionally
carries THIS repository's GIT_DIR/GIT_INDEX_FILE, pinning that the caller's Git
environment never reaches the fixture (with it inherited, the probe would stage its
shrink into the caller's index and measure the wrong repository).
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[1]
HOOKS = ("pre-commit", "pre-merge-commit", "pre-applypatch")
SHEBANG = "#!/usr/bin/env bash\n"

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
# DF-CRIER-433 (a): the row's exact fixture — a success exit ABOVE the marker, the
# rest of the hook (body and markers) kept, so only execution can tell the truth.
PREPEND_DISARM = "exit 0 # DISARMED\n"
# DF-CRIER-433 (b): a success exit the static arm CANNOT see — the statement does
# not begin with the keyword, so only the execution probe can catch it.
PROBE_ONLY_DISARM = '[ -n "${CRIER_SKIP_BOARD_GATE:-}" ] || exit 0\n'
# DF-CRIER-437: a hook that is CORRECT under the environment Git gives a hook —
# it arms only when GIT_INDEX_FILE is set, and real Git exports it (measured on git
# 2.53.0: `.git/index`). The probe must therefore ACCEPT this hook; it used to run
# the hook under `env -u GIT_INDEX_FILE …` and refuse a healthy install.
GIT_ENV_KEYED_PREFIX = 'if [ -z "${GIT_INDEX_FILE:-}" ]; then exit 0; fi\n'


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

        def run(cwd, *argv, extra=None):
            call_env = dict(env)
            if extra:
                call_env.update(extra)
            return subprocess.run(argv, cwd=cwd, env=call_env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)

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
            # DF-CRIER-433: every entrypoint's board arm execs this, so the doctor's
            # execution probe needs it in the repository it doctors — exactly as a
            # real checkout has it.
            shutil.copy2(ROOT / "scripts/check-board.sh", repo / "scripts/check-board.sh")
            shutil.copy2(ROOT / "Makefile", repo / "Makefile")
            require(repo, "make", "install-hooks")
            return repo

        def disarm(repo, hook, line):
            """Tracked source := disarmed line(s) above the original body, reinstalled."""
            src = repo / "scripts/hooks" / hook
            src.write_text(SHEBANG + line + src.read_text().split("\n", 1)[1])
            require(repo, "make", "install-hooks")
            return src

        def check(label, repo, expected, diagnostic="", doctor=None, diagnostics=(), forbidden=(), env_extra=None):
            nonlocal count
            result = run(repo, "bash", str(doctor or ROOT / "scripts/doctor-hooks.sh"), extra=env_extra)
            assert (result.returncode == 0) == expected, (label, result.returncode, result.stdout)
            assert diagnostic in result.stdout, (label, diagnostic, result.stdout)
            for text in tuple(diagnostics):
                assert text in result.stdout, (label, text, result.stdout)
            for text in tuple(forbidden):
                assert text not in result.stdout, (label, text, result.stdout)
            if expected and doctor is None:
                # DF-CRIER-433: the green must SHOW the arm executing — one OK line
                # and one probe evidence line per hook, naming the refusal the probe
                # produced. An exit code alone is not evidence that the probe ran.
                ok_lines = [line for line in result.stdout.splitlines() if line.startswith("doctor-hooks: OK ")]
                assert len(ok_lines) == len(HOOKS), (label, result.stdout)
                assert result.stdout.count("doctor-hooks: probe scope:") == 1, (label, result.stdout)
                for hook in HOOKS:
                    assert f"doctor-hooks: OK {hook} " in result.stdout, (label, result.stdout)
                    assert f"doctor-hooks: probe {hook}: EXECUTED" in result.stdout, (label, result.stdout)
                assert result.stdout.count("4 row(s) lost") == len(HOOKS), (label, result.stdout)
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
        # DF-CRIER-433: the probe must be immune to BOARD_ALLOW_SHRINK, the one
        # variable check-board.sh honours as "this deletion is intended". Inheriting
        # it would let the probe's staged shrink through and turn a healthy install
        # into a FALSE refusal — so the probe strips it from the hook's environment.
        # DF-CRIER-437: this same case carries THIS repository's Git environment, which
        # must not reach the fixture either. With an inherited GIT_DIR/GIT_INDEX_FILE
        # the probe's own reset/stage would act on the CALLER's index (the shrink would
        # never be staged in the fixture) and the hook would read the wrong repository —
        # so those are stripped and re-set from the fixture, and the install stays green.
        check("correct install with BOARD_ALLOW_SHRINK=1 and this repository's "
              "GIT_DIR/GIT_INDEX_FILE in the environment", good, True, "arm REACHES",
              env_extra={"BOARD_ALLOW_SHRINK": "1",
                         "GIT_DIR": str(good / ".git"),
                         "GIT_INDEX_FILE": str(good / ".git" / "index")})
        # DF-CRIER-437: and a hook keyed on the Git-provided environment is ACCEPTED,
        # because real Git exports GIT_INDEX_FILE to these entrypoints (measured, git
        # 2.53.0). Before the fix the probe ran the hook under `env -u GIT_INDEX_FILE …`,
        # so this correct hook exited 0 before its board arm and a healthy install was
        # refused: "a staged 5->1 shrink … was ACCEPTED (exit 0)". The prepended guard
        # is DATA for the probe, not a static-arm shape: it opens a block the static arm
        # cannot see into, so only the probe can decide this case.
        git_env_keyed = fixture("git-env-keyed-pre-commit")
        disarm(git_env_keyed, "pre-commit", GIT_ENV_KEYED_PREFIX)
        check("hook keyed on the Git-provided GIT_INDEX_FILE (probe sets it)",
              git_env_keyed, True, "arm REACHES")

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

        # ── DF-CRIER-433: PRESENCE IS NOT REACHABILITY ───────────────────────────
        # (1) STATIC PRECONDITION. The row's measured disarm: `exit 0 # DISARMED`
        # PREPENDED to the tracked source, the entire body and every marker line
        # kept, reinstalled — so bytes match their own source (cmp passes) and every
        # marker IS present. Before this change the doctor printed three OK lines
        # and exited 0 over a gate that could not run.
        for hook in ("pre-commit", "pre-merge-commit"):
            dead = fixture(f"prepend-disarm-{hook}")
            src = disarm(dead, hook, PREPEND_DISARM)
            # premises: installed == source (byte comparison is satisfied) and every
            # marker is present, so ONLY reachability can refuse this fixture.
            assert (dead / ".git/hooks" / hook).read_bytes() == src.read_bytes(), hook
            for marker, _label in MARKERS[hook]:
                assert f"{marker}\n" in src.read_text(), (hook, marker)
            check(f"unconditional `exit 0` above the board arm ({hook})", dead, False,
                  "exits before its board arm",
                  diagnostics=("the arm cannot run", MARKERS[hook][0][0]))

        # (2) EXECUTION PROBE on a disarm the static arm CANNOT see: the statement
        # does not begin with the keyword, so a text scan is silent and only running
        # the hook exposes it. The refusal must name the arm that never ran.
        probe_only = fixture("probe-only-disarm-pre-merge-commit")
        disarm(probe_only, "pre-merge-commit", PROBE_ONLY_DISARM)
        check("probe-only disarm (guard the static arm cannot see)", probe_only, False,
              "does not reach its board arm",
              diagnostics=("ACCEPTED (exit 0)", "the arm never ran",
                           str(probe_only / ".git/hooks" / "pre-merge-commit")),
              forbidden=("exits before its board arm",))

        # (3) A checker the arm execs but that is not there: the probe must REFUSE
        # rather than fall back to a presence-only green.
        no_checker = fixture("missing-board-checker")
        (no_checker / "scripts/check-board.sh").unlink()
        check("missing scripts/check-board.sh", no_checker, False, "check-board.sh is missing",
              diagnostics=("will not fall back to presence-only",))

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
        shutil.copy2(ROOT / "scripts/check-board.sh", linked / "scripts/check-board.sh")
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
        # DF-CRIER-433: the EXECUTION arm's own NEUTER control. Forcing only the
        # probe's verdict to success must ACCEPT the fixture the real doctor refuses
        # above — proving the refusal came from the probe running the hook, not from
        # the static text scan (which is silent on that shape) nor from any other
        # exit path.
        probe_neutered = base / "doctor-probe-neutered.sh"
        verdict = 'require_probe() { probe_hook "$1" "$2" "$3" "$4" || return 1; }'
        assert text.count(verdict) == 1, "NEUTER transform must replace exactly the probe verdict"
        probe_neutered.write_text(text.replace(verdict, 'require_probe() { probe_hook "$1" "$2" "$3" "$4" || true; }'))
        assert probe_neutered.read_bytes() != (ROOT / "scripts/doctor-hooks.sh").read_bytes()
        check("NEUTER underlying premise: the real doctor refuses the probe-only disarm",
              probe_only, False, "does not reach its board arm")
        check("NEUTER probe verdict accepts what the real doctor rejects", probe_only, True,
              "does not reach its board arm", doctor=probe_neutered)
        print(f"doctor-hooks selftest: PASS ({count} cases, including NEUTER)")


if __name__ == "__main__":
    main()
