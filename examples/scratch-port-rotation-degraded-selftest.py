#!/usr/bin/env python3
"""Degraded-mode regression for `examples/scratch-port-rotation-selftest.py`.

The driver grades the demo harness, and on a clean agent it must never report a
missing HARNESS tool as a failed PRODUCT: a scratch runner may ship no iproute2
(the act container catthehacker/ubuntu has none) and, on a slimmer image, no
curl, no openssl or no coreutils either. Its contract (QA-CRIER-40, DF-CRIER-497)
is: drop EXACTLY the cases that declare the missing tool, name each one with the
tool and a remediation, keep the tool-free `wiring` case running, and exit 0 —
while a case that really fails still exits non-zero.

This module proves that contract against a REAL missing tool rather than a stub:
each arm builds a PATH directory that symlinks the whole ambient PATH minus one
tool, so `shutil.which` in the child cannot see it. That is the clean-agent
condition, reproduced locally.

Arms
  no-ss                 no `ss` (iproute2) — the act container class
  no-curl               a different ambient tool, so the guard is not ss-specific
  no-ambient-tools      ss + curl + openssl + sha256sum + go all absent
  neuter-guard-off      RED: the same no-ss PATH with the skip-guard neutered must
                        go non-zero, so the skip cannot hide a failure
  wiring-product-failure RED: a broken runner source must still fail `wiring`

Usage
    python3 examples/scratch-port-rotation-degraded-selftest.py [DRIVER]

`DRIVER` defaults to the sibling `scratch-port-rotation-selftest.py`; passing a
path runs the same arms against another copy, which is how the pre-fix behaviour
is shown red. Exit status is 0 only when every arm passed. The driver runs this
module as its `degraded` case, so `make scratch-port-rotation-selftest` gates it.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
DRIVER = HERE / "scratch-port-rotation-selftest.py"

CHILD_TIMEOUT_S = 240

# Every ambient tool the driver's cases can declare. The arms predict the skip set
# from THIS list against the ambient PATH, so they hold on a clean agent (where
# several are already missing) exactly as they do on a developer box.
KNOWN_TOOLS = ("bash", "curl", "go", "openssl", "python3", "sha256sum", "ss")

# The runner paths the driver's `wiring` case reads, relative to examples/ — the
# fixture tree for the product-failure arm mirrors exactly these.
RUNNERS = (
    "federation-demo/run-demo.sh",
    "hermes-gateway-demo/run-demo.sh",
    "ws-mesh-demo/run-demo.sh",
    "llm-mesh/run-demo.sh",
    "llm-mesh/mesh/run-demo.sh",
)

# A mutation of llm-mesh's runner that breaks a WIRING invariant (the port must
# come from the shared selector, never a literal): the product-failure arm proves
# the tool-free case is not vacuous.
WIRING_BREAK = ('PORT="$PORT_GUARD_SELECTED"', 'PORT="18770"')


def build_farm(root: Path, omit: frozenset[str]) -> Path:
    """A directory that symlinks every ambient tool EXCEPT `omit`, for PATH.

    Only executables that exist are linked, in PATH order, so the farm resolves
    exactly like the real PATH for everything it does carry.
    """
    farm = root / "path"
    farm.mkdir(parents=True, exist_ok=True)
    seen: set[str] = set()
    for entry in os.environ.get("PATH", "").split(os.pathsep):
        if not entry:
            continue
        try:
            names = sorted(os.listdir(entry))
        except OSError:
            continue
        for name in names:
            if name in seen or name in omit:
                continue
            src = os.path.join(entry, name)
            if not os.path.isfile(src) or not os.access(src, os.X_OK):
                continue
            try:
                os.symlink(src, farm / name)
            except OSError:
                continue
            seen.add(name)
    # The interpreter running this module must resolve BY NAME too: the driver and
    # federation-demo both ask PATH for `python3`.
    if "python3" not in omit and "python3" not in seen:
        os.symlink(sys.executable, farm / "python3")
    for tool in sorted(omit):
        if shutil.which(tool, path=str(farm)) is not None:
            raise AssertionError(f"the restricted PATH still resolves {tool!r}")
    return farm


def child_env(farm: Path | None = None) -> dict[str, str]:
    env = dict(os.environ)
    if farm is not None:
        env["PATH"] = str(farm)
    return env


def run_driver(driver: Path, cases: list[str], farm: Path | None = None) -> subprocess.CompletedProcess:
    argv = [sys.executable, str(driver)]
    for name in cases:
        argv += ["--case", name]
    return subprocess.run(argv, cwd=str(driver.parent.parent), capture_output=True,
                          text=True, env=child_env(farm), timeout=CHILD_TIMEOUT_S)


def driver_cases(driver: Path) -> list[str]:
    proc = subprocess.run([sys.executable, str(driver), "--list"], cwd=str(driver.parent.parent),
                          capture_output=True, text=True, timeout=60)
    if proc.returncode != 0:
        raise AssertionError(f"--list failed (exit {proc.returncode}): {proc.stderr.strip()}")
    return [line.strip() for line in proc.stdout.splitlines() if line.strip()]


def skip_lines(out: str) -> list[str]:
    return [line for line in out.splitlines() if line.startswith("SKIP: scratch-port-rotation-selftest")]


def expect(cond: bool, message: str) -> None:
    if not cond:
        raise AssertionError(message)


def degraded_arm(driver: Path, tmp: Path, omit: frozenset[str]) -> None:
    """Shared body of the skip arms: exit 0, one named SKIP per tool, wiring runs.

    The expected skip set is the ambient environment's own gaps UNION the tools
    this arm takes away, so the same arm proves the contract on a developer box
    (nothing ambient missing) and on a clean agent (several already absent).
    """
    cases = driver_cases(driver)
    live = [name for name in cases if name not in ("wiring", "degraded")]
    expect(bool(live), "the driver lists live cases to skip")
    expected = {tool for tool in KNOWN_TOOLS if shutil.which(tool) is None} | set(omit)
    farm = build_farm(tmp / "farm", omit)
    proc = run_driver(driver, [name for name in cases if name != "degraded"], farm)
    out = proc.stdout + proc.stderr

    expect(proc.returncode == 0,
           f"exit {proc.returncode} (a missing ambient tool is not a product failure): {out.strip()[-800:]}")
    lines = skip_lines(out)
    expect(len(lines) == len(expected),
           f"one SKIP line per missing tool ({sorted(expected)}), got {len(lines)}: {lines}")
    joined = "\n".join(lines)
    named_tools = {tool for tool in KNOWN_TOOLS if f"'{tool}'" in joined}
    expect(named_tools == expected,
           f"the SKIP lines name exactly the missing tools {sorted(expected)}, "
           f"got {sorted(named_tools)}: {joined}")
    for name in live:
        expect(name in joined, f"the SKIP line names the case it took down ({name}): {joined}")
    expect("PASS: wiring" in out, "the tool-free `wiring` case still ran to its own PASS")
    expect(f"{len(live)} case(s) skipped for missing ambient tool(s)" in out,
           "the summary states how many cases were skipped, and for which tool(s)")
    expect("1/1 case(s) behaved" in out,
           "the run reports the case that DID run, so the skip is not an empty green")


def arm_no_ss(driver: Path, tmp: Path) -> None:
    degraded_arm(driver, tmp, frozenset({"ss"}))


def arm_no_curl(driver: Path, tmp: Path) -> None:
    degraded_arm(driver, tmp, frozenset({"curl"}))


def arm_no_ambient_tools(driver: Path, tmp: Path) -> None:
    degraded_arm(driver, tmp, frozenset({"ss", "curl", "openssl", "sha256sum", "go"}))


def arm_neuter_guard_off(driver: Path, tmp: Path) -> None:
    """RED arm: with the skip-guard neutered, the same PATH must go non-zero.

    Proves the skip arms above are load-bearing and not an incidental early
    return: the ONLY difference here is that the guard no longer computes gaps,
    so the live cases run against a PATH without `ss` — and the runners refuse.
    """
    neutered = tmp / "neuter" / "examples" / driver.name
    neutered.parent.mkdir(parents=True, exist_ok=True)
    # Mirror the repo layout so the tool-free `wiring` case still reads the real
    # runner sources through these symlinks: the only thing this arm changes is
    # the skip-guard, so a red run here can only be the live cases it stopped
    # skipping (a missing-source crash would be a false red).
    for top in sorted({rel.split("/")[0] for rel in RUNNERS}):
        os.symlink(HERE / top, neutered.parent / top)
    source = driver.read_text(encoding="utf-8")
    target = "    gaps = ambient_gaps(selected)\n"
    expect(source.count(target) == 1,
           f"the skip-guard call site is present exactly once (found {source.count(target)})")
    neutered.write_text(source.replace(target, "    gaps = {}  # NEUTERED by the degraded selftest\n"),
                        encoding="utf-8")

    cases = driver_cases(neutered)
    farm = build_farm(tmp / "farm-neuter", frozenset({"ss"}))
    proc = run_driver(neutered, [name for name in cases if name != "degraded"], farm)
    out = proc.stdout + proc.stderr
    expect(proc.returncode != 0,
           f"the neutered guard went GREEN (exit 0) — the skip arms prove nothing: {out.strip()[-800:]}")
    expect("FAIL" in out, "the neutered run reports the failing cases it hid")


def arm_wiring_product_failure(driver: Path, tmp: Path) -> None:
    """RED arm: a broken runner source must still be a non-zero `wiring` failure."""
    tree = tmp / "product" / "examples"
    tree.mkdir(parents=True, exist_ok=True)
    shutil.copy(driver, tree / driver.name)
    for rel in RUNNERS:
        dest = tree / rel
        dest.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy(HERE / rel, dest)
    victim = tree / "llm-mesh" / "run-demo.sh"
    src = victim.read_text(encoding="utf-8")
    expect(src.count(WIRING_BREAK[0]) == 1,
           f"the fixture runner carries the wiring invariant exactly once ({victim})")
    victim.write_text(src.replace(*WIRING_BREAK), encoding="utf-8")

    proc = run_driver(tree / driver.name, ["wiring"])
    out = proc.stdout + proc.stderr
    expect(proc.returncode == 1,
           f"a broken runner source must be exit 1, got {proc.returncode}: {out.strip()[-800:]}")
    expect("FAIL" in out, "the broken invariant is reported as a FAIL, not swallowed")


ARMS = (
    ("no-ss", arm_no_ss),
    ("no-curl", arm_no_curl),
    ("no-ambient-tools", arm_no_ambient_tools),
    ("neuter-guard-off", arm_neuter_guard_off),
    ("wiring-product-failure", arm_wiring_product_failure),
)


def main() -> int:
    driver = Path(sys.argv[1]).resolve() if len(sys.argv) > 1 else DRIVER
    if not driver.is_file():
        print(f"FAIL: driver — not a file: {driver}", file=sys.stderr)
        return 2
    print(f"degraded-mode regression for {driver}")
    failures = 0
    for name, arm in ARMS:
        try:
            with tempfile.TemporaryDirectory(prefix=f"scratch-degraded-{name}-") as tmp:
                arm(driver, Path(tmp))
        except (AssertionError, OSError, subprocess.SubprocessError) as exc:
            failures += 1
            print(f"FAIL: {name} — {exc}")
            continue
        print(f"PASS: {name}")
    total = len(ARMS)
    if failures:
        print(f"degraded-mode regression: {total - failures}/{total} arm(s) behaved — FAIL")
        return 1
    print(f"degraded-mode regression: {total}/{total} arm(s) behaved")
    return 0


if __name__ == "__main__":
    sys.exit(main())
