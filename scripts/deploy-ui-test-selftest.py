#!/usr/bin/env python3
"""Offline UAT deploy evidence (stdlib only). Never forwards to real Docker.

Usage: python3 scripts/deploy-ui-test-selftest.py [--script /path/to/deploy-ui-test.sh]
The selectable script and its companion library are exercised unchanged; the
NEUTER cell copies both and changes only the pure dirty decision in the copy.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


SHIM = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

root = Path(os.environ["DEPLOY_FIXTURE"])
f = json.loads((root / "fixture.json").read_text())
a = sys.argv[1:]
with (root / "calls.jsonl").open("a") as out:
    out.write(json.dumps(a) + "\n")
image = "sha256:" + "a" * 64
old = "b" * 64
new = "c" * 64
if a[:2] == ["image", "inspect"]:
    print(image)
elif a[:3] == ["run", "--rm", "--network"]:
    assert a == ["run", "--rm", "--network", "none", image, "-version"], a
    print(f.get("line", "crier vui-test-8adbf50-8adbf50d"))
    sys.exit(f.get("provenance_rc", 0))
elif a[:2] == ["ps", "-aq"]:
    print(old)
elif a[0] == "inspect" and "--format" not in a:
    d = {
        "Id": old, "Name": "/crier-ui-test-current",
        "Config": {
            "Env": ["CRIER_PORT=8767", "CR_REQUIRE_AGENT_SIG=false",
                    "CR_SESSION_BACKEND=jsonl", "CR_SESSION_PATH=/old",
                    "CR_PERMISSIONS_ADMIN_TOKEN=old-fixture", "UNRELATED=keep me"],
            "Entrypoint": ["crier"], "Cmd": ["-log-level", "info"],
            "User": "", "WorkingDir": "/srv"
        },
        "Mounts": [{"Type": "volume", "Name": "uat-fixture-data",
                    "Destination": "/data", "RW": True}],
        "NetworkSettings": {"Networks": {"crier-ui-test-net": {
            "Aliases": ["crier-ui-live", "additional-alias"]}}},
        "HostConfig": {"PortBindings": {"8767/tcp": [
            {"HostIp": "127.0.0.1", "HostPort": "18769"}]},
            "RestartPolicy": {"Name": "unless-stopped", "MaximumRetryCount": 0}}
    }
    if f.get("bad_scope"):
        d["NetworkSettings"]["Networks"]["crier-ui-test-net"]["Aliases"] = ["other"]
    print(json.dumps([d]))
elif a[0] == "inspect" and "--format" in a:
    suffix = "false" if f.get("owner_mismatch") else "true"
    print(new + " " + image + " " + suffix)
elif a[:2] == ["rm", "-f"]:
    if a[2] == old and f.get("remove_rc"):
        sys.exit(1)
    print(a[2])
elif a[:2] == ["run", "-d"]:
    assert a[-3:] == [image, "-log-level", "info"], a
    env = Path(a[a.index("--env-file") + 1]).read_text()
    (root / "promoted-env").write_text(env)
    print(new)
    sys.exit(f.get("start_rc", 0))
elif a[0] == "exec":
    assert a[1] == new, a
    if a[-1].endswith("/health"):
        sys.exit(0)
    assert a[-1] == "http://127.0.0.1:8767/version", a
    count_file = root / "read-count"
    n = int(count_file.read_text()) if count_file.exists() else 0
    count_file.write_text(str(n + 1))
    readings = f.get("readings", [{"version": "ui-test-8adbf50", "commit": "8adbf50d", "modified": False}])
    value = readings[min(n, len(readings)-1)]
    print(value if isinstance(value, str) else json.dumps(value))
    sys.exit(f.get("read_rc", 0))
else:
    raise SystemExit("UNEXPECTED FAKE DOCKER CALL: " + repr(a))
'''


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--script", type=Path,
                        default=Path(__file__).with_name("deploy-ui-test.sh"))
    opts = parser.parse_args()
    script = opts.script.resolve()
    results = []
    with tempfile.TemporaryDirectory(prefix="deploy-ui-test-selftest-") as tmp:
        root = Path(tmp)
        shim_dir = root / "bin"
        shim_dir.mkdir()
        (shim_dir / "docker").write_text(SHIM)
        (shim_dir / "docker").chmod(0o755)
        # Polls have bounded counts in the actual script. No real sleep needed
        # in fixtures, and no real docker is reachable through this shim.
        (shim_dir / "sleep").write_text("#!/bin/sh\nexit 0\n")
        (shim_dir / "sleep").chmod(0o755)

        def run(fixture=None, target=script, extra=()):
            cell = root / f"cell-{len(list(root.glob('cell-*')))}"
            cell.mkdir()
            (cell / "fixture.json").write_text(json.dumps(fixture or {}))
            token = cell / "admin-token"
            token.write_text("fixture-admin\n")
            env = dict(os.environ, PATH=str(shim_dir) + os.pathsep + os.environ["PATH"],
                       DEPLOY_FIXTURE=str(cell), HOME=str(cell), XDG_RUNTIME_DIR=str(cell))
            proc = subprocess.run(
                ["bash", str(target), "--image", "crier-ui-test:misleading-label",
                 "--commit", "8adbf50", "--admin-token-file", str(token), *extra],
                env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                timeout=20)
            calls_path = cell / "calls.jsonl"
            calls = [json.loads(s) for s in calls_path.read_text().splitlines()] if calls_path.exists() else []
            return proc, calls, cell

        def rejected(fixture, commit, dirty, extra=()):
            proc, calls, _ = run(fixture, extra=extra)
            check(proc.returncode != 0, proc.stdout)
            check("REFUSED:" in proc.stdout, proc.stdout)
            for value in ("expected commit=8adbf50", f"reported commit={commit}", f"dirty={dirty}"):
                check(value in proc.stdout, proc.stdout)
            check(not any(c[0] == "rm" or c[:2] == ["run", "-d"] for c in calls), calls)

        def clean():
            proc, calls, cell = run()
            check(proc.returncode == 0 and "PROMOTED:" in proc.stdout, proc.stdout)
            remove = next(i for i, c in enumerate(calls) if c[:2] == ["rm", "-f"])
            start = next(i for i, c in enumerate(calls) if c[:2] == ["run", "-d"])
            reads = [i for i, c in enumerate(calls) if c[0] == "exec" and c[-1].endswith("/version")]
            check(remove < start and len(reads) == 3 and min(reads) > start, calls)
            args = calls[start]
            for value in ("crier-ui-test-net", "crier-ui-live", "additional-alias",
                          "type=volume,src=uat-fixture-data,dst=/data", "unless-stopped",
                          "127.0.0.1:18769:8767/tcp", "/srv"):
                check(value in args, args)
            env = (cell / "promoted-env").read_text().splitlines()
            for value in ("CR_SESSION_BACKEND=sqlite", "CR_SESSION_SQLITE_PATH=/data/sessions.db",
                          "CR_PERMISSIONS_ENABLED=true", "CR_GUARD_ENABLED=false",
                          "CR_PERMISSIONS_ADMIN_TOKEN=fixture-admin", "UNRELATED=keep me",
                          "CR_REQUIRE_AGENT_SIG=false"):
                check(value in env, env)
            check(not any(v.startswith("CR_SESSION_PATH=") for v in env), env)
            check("fixture-admin" not in proc.stdout and "fixture-admin" not in json.dumps(calls), proc.stdout)
            check(not list(cell.glob("crier-ui-test-deploy/run.*")), "secret scratch leaked")

        def failed_read(value, read_rc=0, readings=None):
            fixture = {"readings": readings or [value], "read_rc": read_rc}
            proc, calls, _ = run(fixture)
            check(proc.returncode != 0 and "FAILED:" in proc.stdout and "PROMOTED:" not in proc.stdout, proc.stdout)
            check(any(c[:2] == ["run", "-d"] for c in calls), calls)
            check(["rm", "-f", "c" * 64] in calls, "unverified replacement not removed")
            check("expected commit=8adbf50" in proc.stdout, proc.stdout)

        def neuter():
            copy_dir = root / "neutered"
            copy_dir.mkdir()
            shutil.copy2(script, copy_dir / script.name)
            lib = script.with_name("deploy-ui-test-lib.sh").read_text()
            needle = '[[ "$dirty" == false ]] || return 1 # DIRTY_GATE: selftest mutates this decision only'
            check(lib.count(needle) == 1, "dirty decision mutation anchor missing or nonunique")
            (copy_dir / "deploy-ui-test-lib.sh").write_text(lib.replace(needle, ": # DIRTY_GATE deliberately disabled by selftest"))
            fixture = {"line": "crier vui-test-8adbf50-8adbf50d-dirty"}
            rejected(fixture, "8adbf50d", "true")
            proc, calls, _ = run(fixture, target=copy_dir / script.name)
            check(proc.returncode == 0 and "PROMOTED:" in proc.stdout, proc.stdout)
            check(any(c[:2] == ["run", "-d"] for c in calls), calls)

        def operational_failure(fixture, phrase):
            proc, calls, _ = run(fixture)
            check(proc.returncode != 0 and phrase in proc.stdout and "PROMOTED:" not in proc.stdout, proc.stdout)
            if fixture.get("remove_rc"):
                check(not any(c[:2] == ["run", "-d"] for c in calls), calls)

        cases = [
            ("clean matching image promotes, preserves posture, then reads /version three times", clean),
            ("dirty image refuses before removal with all three values", lambda: rejected({"line": "crier vui-test-8adbf50-8adbf50d-dirty"}, "8adbf50d", "true")),
            ("mismatched image refuses with both commits", lambda: rejected({"line": "crier v1.0.0-deadbeef"}, "deadbeef", "false")),
            ("unreadable provenance is unknown, never clean", lambda: rejected({"provenance_rc": 1}, "unknown", "unknown")),
            ("empty provenance refuses", lambda: rejected({"line": ""}, "unknown", "unknown")),
            ("malformed provenance refuses", lambda: rejected({"line": "not a crier version"}, "unknown", "unknown")),
            ("dirty stamped version before commit refuses", lambda: rejected({"line": "crier v1.0.0-dirty-8adbf50d"}, "8adbf50d", "true")),
            ("successful docker start with mismatching read-back FAILS", lambda: failed_read({"version": "ui-test", "commit": "deadbeef", "modified": False})),
            ("modified live read-back FAILS", lambda: failed_read({"version": "ui-test", "commit": "8adbf50d", "modified": True})),
            ("missing modified field FAILS closed", lambda: failed_read({"version": "ui-test", "commit": "8adbf50d"})),
            ("string false modified field FAILS closed", lambda: failed_read({"version": "ui-test", "commit": "8adbf50d", "modified": "false"})),
            ("unreadable live provenance FAILS", lambda: failed_read("", read_rc=1)),
            ("later disagreement cannot be hidden by an earlier clean read", lambda: failed_read(None, readings=[{"version": "ui-test", "commit": "8adbf50d", "modified": False}, {"version": "ui-test", "commit": "deadbeef", "modified": False}])),
            ("JSON version dirty marker refuses even if modified is false", lambda: failed_read({"version": "ui-test-dirty", "commit": "8adbf50d", "modified": False})),
            ("non-UAT alias refuses without removal", lambda: rejected({"bad_scope": True}, "8adbf50d", "false")),
            ("removal failure never starts replacement", lambda: operational_failure({"remove_rc": 1}, "old container removal failed")),
            ("docker start failure never reports success", lambda: operational_failure({"start_rc": 1}, "replacement start failed")),
            ("name/image ownership disagreement FAILS", lambda: operational_failure({"owner_mismatch": True}, "no longer owns")),
            ("NEUTER: disabling only dirty decision accepts the refused dirty fixture", neuter),
        ]
        for name, fn in cases:
            try:
                fn()
                results.append(True)
                print("PASS " + name)
            except (AssertionError, OSError, subprocess.SubprocessError) as exc:
                results.append(False)
                print(f"FAIL {name}: {exc}")
    print(f"RESULT {sum(results)}/{len(results)} passed")
    return 0 if all(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
