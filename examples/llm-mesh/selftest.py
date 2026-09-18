#!/usr/bin/env python3
"""Deterministic self-test for the llm-mesh demo's termination/output contract.

No paid API key and no network: the MODEL endpoint is a local stub
(stub_llm.py) that solves the puzzle from the clues it is handed. Everything
else is the real thing — the `crier` server binary, the `crier-mcp` bridge
over stdio, the HTTP inbox lane, the WebSocket mesh lane, both harnesses and
the controller.

Cases:

  mcp-timeout   a bridge that never answers is a NAMED, BOUNDED error
                (MCPClient) that names the TOOL it was waiting on, not just
                the JSON-RPC method — the DF-CRIER-214 hang class
  solved        bridge lane, real transport: both agents exchange clues over
                crier, the controller verifies them -> PRODUCT SOLVED, with
                a live mesh PING round-trip while BOTH agents are still
                connected (a probe against an exited agent is not a pass)
  wrong-answer  wrong submissions are REJECTED: at least one received
                submission is checked, found wrong and rejected, and nothing
                claims success (the two agents' turn schedules are not
                identical, so the invariant is about verification, not about
                how many agents managed to submit)
  stalled       an agent that never submits ends in a bounded, named failure
                (turn budget) — never a hang, never a silent success
  wall-clock    a wedged run is killed by the wall-clock budget and the run
                reports NO_VERDICT with the cause (exit 124)
  mesh-solved   raw protocol lane end-to-end over the real mesh wire

Usage:
    python3 selftest.py                 # all cases
    python3 selftest.py --case solved   # one case
    python3 selftest.py --list          # case names
    python3 selftest.py --keep          # keep the per-case temp dirs

Exit status: 0 if every case passed, 1 otherwise.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
BRIDGE_RUNNER = HERE / "run-demo.sh"
MESH_RUNNER = HERE / "mesh" / "run-demo.sh"

sys.path.insert(0, str(HERE))
import stub_llm  # noqa: E402  (local test double)

# A placeholder credential for a stub endpoint — never a real key, never a secret.
STUB_KEY = "llm-mesh-selftest-stub"

CASES = ["mcp-timeout", "solved", "wrong-answer", "stalled", "wall-clock", "mesh-solved"]


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class CaseResult:
    def __init__(self, name: str):
        self.name = name
        self.checks: list[tuple[bool, str]] = []
        self.detail = ""

    def check(self, ok: bool, label: str, evidence: str = "") -> None:
        self.checks.append((bool(ok), label + (f" [{evidence}]" if evidence else "")))

    @property
    def passed(self) -> bool:
        return all(ok for ok, _ in self.checks)

    def render(self) -> str:
        lines = [f"{'PASS' if self.passed else 'FAIL'}  {self.name}"]
        for ok, label in self.checks:
            lines.append(f"        {'ok  ' if ok else 'FAIL'} {label}")
        if self.detail and not self.passed:
            lines.append("        ---- captured output (tail) ----")
            for line in self.detail.strip().splitlines()[-40:]:
                lines.append(f"        | {line}")
        return "\n".join(lines)


# ------------------------------------------------------------------ helpers


def scenario_env(out_dir: Path, port: int, stub_url: str, **overrides) -> dict:
    env = dict(os.environ)
    env.pop("CRIER_MCP_TIMEOUT_S", None)
    env.update({
        "DEEPSEEK_API_KEY": STUB_KEY,
        "DOGFOOD_OUT": str(out_dir),
        "DOGFOOD_BASE_URL": stub_url,
        "DOGFOOD_MODEL": "stub-model",
        "CRIER_PORT": str(port),
    })
    env.update(overrides)
    return env


def run_runner(script: Path, env: dict, hard_timeout: float) -> tuple[int, str, float]:
    """Run a demo runner; returns (exit code, combined output, seconds)."""
    started = time.time()
    try:
        proc = subprocess.run(["bash", str(script)], cwd=str(script.parent),
                              env=env, capture_output=True, text=True,
                              timeout=hard_timeout)
        return proc.returncode, proc.stdout + proc.stderr, time.time() - started
    except subprocess.TimeoutExpired as exc:
        out = (exc.stdout or "") + (exc.stderr or "")
        if isinstance(out, bytes):
            out = out.decode(errors="replace")
        return 999, out + f"\n[selftest] HARD TIMEOUT after {hard_timeout}s — the run hung", time.time() - started


def read_product(out_dir: Path) -> dict:
    path = out_dir / "verdict.json"
    if not path.exists():
        return {}
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return {"result": "UNPARSEABLE"}


def read_log(out_dir: Path, name: str) -> str:
    path = out_dir / name
    return path.read_text(encoding="utf-8", errors="replace") if path.exists() else ""


# ------------------------------------------------------------------ cases


def case_mcp_timeout(work: Path) -> CaseResult:
    """A bridge that never answers must fail loudly and quickly, not hang."""
    from mcp_client import MCPClient, MCPError

    res = CaseResult("mcp-timeout")
    silent = work / "silent-bridge"
    silent.write_text("#!/usr/bin/env python3\nimport time\ntime.sleep(120)\n", encoding="utf-8")
    silent.chmod(0o755)
    exits = work / "exiting-bridge"
    exits.write_text("#!/usr/bin/env python3\nimport sys\nsys.exit(7)\n", encoding="utf-8")
    exits.chmod(0o755)

    client = MCPClient(str(silent), {}, "selftest-silent", timeout_s=2)
    started = time.time()
    try:
        client.call("get_messages", {"max": 1})
        res.check(False, "wedged bridge must raise MCPError")
    except MCPError as exc:
        elapsed = time.time() - started
        res.check(2 <= elapsed < 10, f"wedged bridge fails within the 2s budget (took {elapsed:.1f}s)")
        # The diagnostic must name the CALL the caller was blocked on, not
        # just the JSON-RPC method it rides on: 'tools/call' alone leaves the
        # operator guessing which of the four harness tools wedged.
        res.check("tools/call get_messages" in str(exc),
                  "the error names the tool call it was waiting on", str(exc)[:140])
        res.check("no reply" in str(exc), "the error names the cause (no reply)")
    except Exception as exc:  # noqa: BLE001
        res.check(False, f"wedged bridge raised {type(exc).__name__}, not MCPError: {exc}")
    finally:
        t0 = time.time()
        client.close()
        res.check(time.time() - t0 < 6, "close() on a wedged bridge does not hang")

    client2 = MCPClient(str(exits), {}, "selftest-exited", timeout_s=5)
    try:
        client2.call("get_messages", {"max": 1})
        res.check(False, "a bridge that exits must raise MCPError")
    except MCPError as exc:
        res.check("exited" in str(exc), "the error names the exit", str(exc)[:140])
        res.check("get_messages" in str(exc),
                  "the exit error also names the tool that went unanswered", str(exc)[:140])
    except Exception as exc:  # noqa: BLE001
        res.check(False, f"exited bridge raised {type(exc).__name__}: {exc}")
    finally:
        client2.close()
    return res


def case_solved(work: Path) -> CaseResult:
    res = CaseResult("solved")
    out_dir = work / "out"
    server, state, stub_url = stub_llm.start_server("solved")
    try:
        env = scenario_env(out_dir, free_port(), stub_url,
                           DOGFOOD_CONTROLLER_DEADLINE_S="60",
                           DOGFOOD_TIMEOUT_S="120",
                           DOGFOOD_MESH_WAIT_S="20",
                           DOGFOOD_ASK_TIMEOUT_S="20")
        rc, output, secs = run_runner(BRIDGE_RUNNER, env, 180)
        res.detail = output
        product = read_product(out_dir)
        controller_log = read_log(out_dir, "controller.log")
        agent_a = read_log(out_dir, "agent-a.log")
        agent_b = read_log(out_dir, "agent-b.log")

        # 1. bounded, and it ends with the controller's verdict
        res.check(rc == 0, "exit 0", f"got {rc}")
        res.check(product.get("result") == "SOLVED", "PRODUCT result SOLVED",
                  f"got {product.get('result')!r}")
        res.check(product.get("finals_received") == 2, "both finals received")
        res.check(all(a.get("verdict") == "ACCEPTED" for a in product.get("agents", {}).values()),
                  "both agents ACCEPTED")
        res.check(all(a.get("submitted") == product.get("ground_truth")
                      for a in product.get("agents", {}).values()),
                  "submissions match the ground truth")

        # 2. the bridge/mesh path is REAL: the clues crossed crier, not a shortcut
        res.check("-> ask agent-b:" in agent_a and "<- agent-b answered:" in agent_a,
                  "agent-a asked and got an answer over the bridge")
        res.check("-> ask agent-a:" in agent_b and "<- agent-a answered:" in agent_b,
                  "agent-b asked and got an answer over the bridge")
        res.check("FINAL from agent-a" in controller_log and "FINAL from agent-b" in controller_log,
                  "the controller saw both finals arrive over the inbox lane")

        # 3. the live mesh lane was exercised WHILE both agents were connected.
        #    This is the case that regressed: the probe used to run after the
        #    verdicts, when both harnesses had already exited, and the server
        #    answered CONTROLLER_OFFLINE ("peer agent-a not connected"). The
        #    product now carries the peers seen at probe time, so a probe that
        #    ran against an empty mesh cannot read as a pass.
        mesh = product.get("mesh", {})
        peers = list(mesh.get("peers") or [])
        ping = mesh.get("ping_agent_a") or {}
        res.check("mesh_peers ->" in controller_log and "mesh_request PING" in controller_log,
                  "the live mesh lane was exercised", str(peers))
        res.check(mesh.get("peers_ok") is True,
                  "both agents were on the mesh when the controller probed",
                  f"peers={sorted(peers)}")
        res.check(all(a in peers for a in ("agent-a", "agent-b")),
                  "the probe's own peer list names both agents (proof it ran while they were up)",
                  str(sorted(peers)))
        res.check(ping.get("ok") is True,
                  "mesh PING round-trip succeeded (not CONTROLLER_OFFLINE)",
                  str(ping.get("reply"))[:160])
        res.check("bridge_alive" in str(ping.get("reply", "")),
                  "the peer's bridge answered the PING on the real wire path",
                  str(ping.get("reply"))[:160])
        probe_at = controller_log.find("mesh_peers ->")
        first_final = controller_log.find("FINAL from agent-a")
        res.check(0 <= probe_at < first_final,
                  "the mesh probe ran before the first verdict was even handled",
                  f"probe@{probe_at} first_final@{first_final}")

        # 4. the LLM was actually asked (the stub is the model double, the
        #    harnesses are not): the stub's solver was called by both agents
        solver_calls = [r for r in state.requests if r["kind"] == "solver"]
        agents_seen = {r["agent"] for r in solver_calls}
        res.check(agents_seen == {"agent-a", "agent-b"},
                  "the harnesses drove the model endpoint for both agents", str(sorted(agents_seen)))
        res.check(len(state.requests) > len(solver_calls),
                  "responder calls happened too (peer answers were generated)",
                  f"{len(solver_calls)} solver / {len(state.requests)} total")

        # 5. the harnesses saw their verdicts and exited cleanly
        res.check("SOLVED — verdict ACCEPTED" in agent_a, "agent-a observed its ACCEPTED verdict")
        res.check("SOLVED — verdict ACCEPTED" in agent_b, "agent-b observed its ACCEPTED verdict")
        outcomes = output.split("harness outcomes:")[-1]
        res.check("agent-a exited 0" in outcomes and "agent-b exited 0" in outcomes,
                  "both harnesses exited 0 after the verdict", outcomes.strip()[:160])
        return res
    finally:
        server.shutdown()
        server.server_close()


def case_wrong_answer(work: Path) -> CaseResult:
    """A wrong submission is REJECTED: the verification is not a success path.

    The stub deliberately does not carry a canned answer — it deduces from its
    own clues plus the peer's answers — so the two agents' turn schedules are
    NOT identical: whether an agent manages to submit within the turn budget
    depends on when the peer's reply lands. The invariant this case asserts is
    therefore about VERIFICATION, not about the count of submissions: whatever
    was received must have been checked, found wrong, and rejected, and nothing
    may call that success.
    """
    res = CaseResult("wrong-answer")
    out_dir = work / "out"
    server, _state, stub_url = stub_llm.start_server("wrong")
    try:
        env = scenario_env(out_dir, free_port(), stub_url,
                           DOGFOOD_CONTROLLER_DEADLINE_S="40",
                           DOGFOOD_TIMEOUT_S="120",
                           DOGFOOD_MESH_WAIT_S="15",
                           DOGFOOD_ASK_TIMEOUT_S="20",
                           DOGFOOD_MAX_TURNS="6")
        rc, output, secs = run_runner(BRIDGE_RUNNER, env, 180)
        res.detail = output
        product = read_product(out_dir)
        controller_log = read_log(out_dir, "controller.log")
        agents = product.get("agents", {})
        received = {a: v for a, v in agents.items() if v.get("submitted")}
        rejected = [a for a, v in agents.items() if v.get("verdict") == "REJECTED"]

        res.check(rc != 0, "a wrong answer is not a success", f"exit {rc}")
        res.check(secs < 150, "the wrong-answer run still ends (no hang)", f"{secs:.1f}s")
        res.check(product.get("result") == "NOT_ALL_CORRECT",
                  "PRODUCT result NOT_ALL_CORRECT", f"got {product.get('result')!r}")
        res.check(not (product.get("result") == "SOLVED" and
                       all(v.get("verdict") == "ACCEPTED" for v in agents.values())),
                  "the product never claims verified success",
                  str({k: v.get("verdict") for k, v in agents.items()}))
        res.check(bool(received),
                  "at least one wrong submission was really received and checked",
                  f"{len(received)} received of {len(agents)} expected")
        res.check(bool(rejected), "at least one received submission was REJECTED",
                  str(sorted(rejected)))
        res.check(all(v.get("submitted") != product.get("ground_truth")
                      for v in received.values()),
                  "every received submission differs from the ground truth",
                  str([v.get("submitted") for v in received.values()])[:160])
        res.check("SOLVED" not in output, "the run never prints SOLVED")
        res.check("REJECTED" in controller_log,
                  "the controller rejected it in its own log")
        return res
    finally:
        server.shutdown()
        server.server_close()


def case_stalled(work: Path) -> CaseResult:
    res = CaseResult("stalled")
    out_dir = work / "out"
    server, _state, stub_url = stub_llm.start_server("stall")
    try:
        env = scenario_env(out_dir, free_port(), stub_url,
                           DOGFOOD_CONTROLLER_DEADLINE_S="15",
                           DOGFOOD_TIMEOUT_S="60",
                           DOGFOOD_MESH_WAIT_S="8",
                           DOGFOOD_ASK_TIMEOUT_S="4",
                           DOGFOOD_MAX_TURNS="2")
        rc, output, secs = run_runner(BRIDGE_RUNNER, env, 120)
        res.detail = output
        product = read_product(out_dir)
        agent_a = read_log(out_dir, "agent-a.log")
        agent_b = read_log(out_dir, "agent-b.log")

        res.check(rc in (1, 2), "bounded, named failure exit (1 = verdict says not solved, 2 = turn budget)",
                  f"exit {rc} after {secs:.1f}s")
        res.check(secs < 110, "no hang: the stalled run still ends", f"{secs:.1f}s")
        res.check(product.get("result") == "NOT_ALL_CORRECT",
                  "PRODUCT result NOT_ALL_CORRECT", f"got {product.get('result')!r}")
        res.check("missing finals" in str(product.get("cause", "")),
                  "the cause names the missing submissions", str(product.get("cause"))[:100])
        res.check(product.get("finals_received") == 0, "no final was fabricated")
        res.check("turn budget exhausted" in agent_a and "tools called: [ask_peer" in agent_a,
                  "agent-a named its own turn-budget failure")
        res.check("SOLVED" not in output, "the run never prints SOLVED")
        return res
    finally:
        server.shutdown()
        server.server_close()


def case_wall_clock(work: Path) -> CaseResult:
    res = CaseResult("wall-clock")
    out_dir = work / "out"
    server, _state, stub_url = stub_llm.start_server("stall")
    try:
        env = scenario_env(out_dir, free_port(), stub_url,
                           DOGFOOD_CONTROLLER_DEADLINE_S="150",
                           DOGFOOD_TIMEOUT_S="6",
                           DOGFOOD_MESH_WAIT_S="5",
                           DOGFOOD_ASK_TIMEOUT_S="4",
                           DOGFOOD_MAX_TURNS="50")
        rc, output, secs = run_runner(BRIDGE_RUNNER, env, 90)
        res.detail = output
        product = read_product(out_dir)

        res.check(rc == 124, "the wall-clock bound is reported as exit 124", f"got {rc}")
        res.check(secs < 60, "the bound fires promptly, not eventually", f"{secs:.1f}s")
        res.check(product.get("result") == "NO_VERDICT",
                  "PRODUCT result NO_VERDICT (nothing claims success)", f"got {product.get('result')!r}")
        res.check("wall-clock budget" in str(product.get("cause", "")),
                  "the cause names the budget", str(product.get("cause"))[:120])
        res.check("SOLVED" not in json.dumps(product), "no SOLVED in the failure product")
        return res
    finally:
        server.shutdown()
        server.server_close()


def case_mesh_solved(work: Path) -> CaseResult:
    res = CaseResult("mesh-solved")
    out_dir = work / "out"
    server, state, stub_url = stub_llm.start_server("solved")
    try:
        env = scenario_env(out_dir, free_port(), stub_url,
                           DOGFOOD_CONTROLLER_DEADLINE_S="60",
                           DOGFOOD_TIMEOUT_S="120",
                           DOGFOOD_AGENT_WAIT_S="30",
                           DOGFOOD_MAX_TURNS="4")
        rc, output, secs = run_runner(MESH_RUNNER, env, 180)
        res.detail = output
        product = read_product(out_dir)
        controller_log = read_log(out_dir, "controller.log")
        agent_a = read_log(out_dir, "agent-a.log")

        res.check(rc == 0, "exit 0", f"got {rc}")
        res.check(product.get("result") == "SOLVED", "PRODUCT result SOLVED",
                  f"got {product.get('result')!r}")
        res.check("raw mesh lane" in str(product.get("verified_by", "")),
                  "the raw-protocol controller produced the verdict")
        res.check("both agents on mesh" in controller_log, "the mesh saw both agents")
        res.check("<- controller: ACCEPTED" in agent_a,
                  "agent-a got its verdict over the mesh wire")
        res.check("-> ask agent-a" in read_log(out_dir, "agent-b.log"),
                  "the agents asked each other over the mesh wire")
        res.check({r["agent"] for r in state.requests if r["kind"] == "solver"} == {"agent-a", "agent-b"},
                  "both mesh agents drove the model endpoint")
        return res
    finally:
        server.shutdown()
        server.server_close()


CASE_FUNCS = {
    "mcp-timeout": case_mcp_timeout,
    "solved": case_solved,
    "wrong-answer": case_wrong_answer,
    "stalled": case_stalled,
    "wall-clock": case_wall_clock,
    "mesh-solved": case_mesh_solved,
}


def main() -> int:
    ap = argparse.ArgumentParser(description="llm-mesh deterministic self-test")
    ap.add_argument("--case", action="append", choices=CASES,
                    help="run only this case (repeatable)")
    ap.add_argument("--list", action="store_true", help="list case names and exit")
    ap.add_argument("--keep", action="store_true", help="keep per-case temp dirs")
    ap.add_argument("--no-build", action="store_true", help="skip the build step")
    args = ap.parse_args()
    if args.list:
        print("\n".join(CASES))
        return 0

    selected = args.case or CASES
    print(f"llm-mesh self-test: {', '.join(selected)}")

    if not args.no_build:
        print("building crier + crier-mcp ...")
        build = subprocess.run(["make", "-C", str(REPO), "build", "build-mcp"],
                               capture_output=True, text=True)
        if build.returncode != 0:
            print("BUILD FAILED:\n" + build.stdout + build.stderr)
            return 1

    root = Path(tempfile.mkdtemp(prefix="llm-mesh-selftest-"))
    print(f"work dir: {root}\n")
    results: list[CaseResult] = []
    started = time.time()
    try:
        for name in selected:
            work = root / name
            work.mkdir(parents=True, exist_ok=True)
            print(f"--- {name} " + "-" * (50 - len(name)))
            try:
                res = CASE_FUNCS[name](work)
            except Exception as exc:  # noqa: BLE001
                import traceback
                res = CaseResult(name)
                res.check(False, f"case raised {type(exc).__name__}: {exc}")
                res.detail = traceback.format_exc()
            results.append(res)
            print(res.render())
            print()
    finally:
        if args.keep:
            print(f"kept: {root}")
        else:
            shutil.rmtree(root, ignore_errors=True)

    failed = [r.name for r in results if not r.passed]
    print("=" * 58)
    print(f"{len(results) - len(failed)}/{len(results)} cases passed "
          f"in {time.time() - started:.1f}s")
    if failed:
        print("FAILED: " + ", ".join(failed))
        return 1
    print("all cases passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
