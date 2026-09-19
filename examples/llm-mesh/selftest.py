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
  port-rotate   a scratch-port collision on a runner's FIRST default candidate
                is rotated past (the colliding port and its holder are named)
                and the whole run still succeeds on the port it selected; the
                holder is left running — QA-CRIER-10
  port-refuse   the fail-closed half: every default candidate occupied is a
                named non-zero failure that starts nothing, an OCCUPIED explicit
                CRIER_PORT is refused instead of rotated, and a FREE explicit
                CRIER_PORT is used verbatim while every candidate is occupied
  port-wiring   source invariants over BOTH runners: the shared selector is
                used (no hard-coded default port, no ad-hoc `ss -tln` probe) and
                every component invocation is pinned to the selected port

Usage:
    python3 selftest.py                 # all cases
    python3 selftest.py --case solved   # one case
    python3 selftest.py --list          # case names
    python3 selftest.py --keep          # keep the per-case temp dirs

No paid API key and no network in any case — the port cases add no provider
call either: the collision is a squatter socket this selftest owns.

Exit status: 0 if every case passed, 1 otherwise.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import re
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

CASES = ["mcp-timeout", "solved", "wrong-answer", "stalled", "wall-clock", "mesh-solved",
         "port-rotate", "port-refuse", "port-wiring"]


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


def scenario_env(out_dir: Path, port: int | None, stub_url: str, **overrides) -> dict:
    env = dict(os.environ)
    env.pop("CRIER_MCP_TIMEOUT_S", None)
    env.pop("CRIER_PORT", None)
    env.pop("CRIER_PORT_CANDIDATES", None)
    env.update({
        "DEEPSEEK_API_KEY": STUB_KEY,
        "DOGFOOD_OUT": str(out_dir),
        "DOGFOOD_BASE_URL": stub_url,
        "DOGFOOD_MODEL": "stub-model",
    })
    if port is not None:
        env["CRIER_PORT"] = str(port)
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


# --------------------------------------------------- scratch-port fixtures
#
# The port-rotation cases need ports that are occupied DETERMINISTICALLY, by a
# listener this selftest owns, with no provider, no network and no external
# process — so the squatter is an in-process socket: bind+listen is synchronous,
# which removes the pick->bind race a spawned listener would introduce.

PORT_BASE_RE = re.compile(r"(?m)^PORT_BASE=(\d+)")
SELECTED_RE = re.compile(r"port-guard: selected :(\d+) for")


def runner_base_port(script: Path) -> int:
    """The first candidate of a runner's scratch-port rotation."""
    m = PORT_BASE_RE.search(script.read_text(encoding="utf-8"))
    if m is None:
        raise AssertionError(f"{script} has no `PORT_BASE=<port>` line")
    return int(m.group(1))


class Squatter:
    """A listener this selftest owns and keeps for the duration of a case."""

    def __init__(self, port: int):
        self.port = port
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            self.sock.bind(("127.0.0.1", port))
        except OSError as exc:
            self.sock.close()
            raise AssertionError(f"could not squat :{port} — something already holds it ({exc})") from exc
        self.sock.listen(16)

    def __enter__(self) -> "Squatter":
        return self

    def __exit__(self, *exc) -> None:
        self.sock.close()

    def still_listening(self) -> bool:
        """True when the port still accepts a connection: proof the runner
        rotated PAST this holder instead of killing it."""
        try:
            with socket.create_connection(("127.0.0.1", self.port), timeout=1):
                return True
        except OSError:
            return False


def squat_all(ports: list[int]) -> list[Squatter]:
    return [Squatter(p) for p in ports]


def selected_port(output: str) -> int | None:
    m = SELECTED_RE.search(output)
    return int(m.group(1)) if m else None


def free_run(count: int, low: int = 24000, high: int = 42000, tries: int = 60) -> list[int]:
    """`count` CONSECUTIVE free ports — a rotation fixture needs neighbours."""
    for _ in range(tries):
        base = random.randrange(low, high)
        if all(port_is_free(base + i) for i in range(count)):
            return [base + i for i in range(count)]
    return []


def port_is_free(port: int) -> bool:
    with socket.socket() as s:
        try:
            s.bind(("127.0.0.1", port))
            return True
        except OSError:
            return False


def ss_shows(port: int) -> bool:
    """Whether `ss` (what the guards use) agrees the port is occupied."""
    try:
        out = subprocess.run(["ss", "-tln"], capture_output=True, text=True).stdout
    except OSError:
        return False
    return any(re.search(rf":{port}\s", line) for line in out.splitlines()[1:])


def invocation_blocks(src: str, needle: str) -> list[str]:
    """Every shell invocation that mentions `needle`, including its
    backslash-continued continuation lines."""
    lines = src.splitlines()
    blocks = []
    for i, line in enumerate(lines):
        if needle not in line:
            continue
        block = [line]
        j = i
        while lines[j].rstrip().endswith("\\") and j + 1 < len(lines):
            j += 1
            block.append(lines[j])
        blocks.append("\n".join(block))
    return blocks


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
    port = free_port()
    server, state, stub_url = stub_llm.start_server("solved")
    try:
        env = scenario_env(out_dir, port, stub_url,
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

        # 0. an EXPLICIT CRIER_PORT is honored verbatim — never rotated away from
        #    (the runner is asked for a free port here, so the only correct
        #    behaviour is to use exactly that one; the rotation cases below cover
        #    the default path and the occupied-explicit path).
        res.check(f"port-guard: selected :{port} for" in output,
                  "the explicitly requested port was the one selected", f":{port}")
        res.check(f"starting crier server on :{port}" in output,
                  "the server was started on the requested port", f":{port}")

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
    port = free_port()
    server, state, stub_url = stub_llm.start_server("solved")
    try:
        env = scenario_env(out_dir, port, stub_url,
                           DOGFOOD_CONTROLLER_DEADLINE_S="60",
                           DOGFOOD_TIMEOUT_S="120",
                           DOGFOOD_AGENT_WAIT_S="30",
                           DOGFOOD_MAX_TURNS="4")
        rc, output, secs = run_runner(MESH_RUNNER, env, 180)
        res.detail = output
        product = read_product(out_dir)
        controller_log = read_log(out_dir, "controller.log")
        agent_a = read_log(out_dir, "agent-a.log")

        # An explicit CRIER_PORT is honored verbatim on this lane too, and the
        # raw mesh clients are pinned to it (the agents' only wire address).
        res.check(f"port-guard: selected :{port} for" in output,
                  "the explicitly requested port was the one selected", f":{port}")
        res.check(f":{port} is held by pid" in output,
                  "the relay on the requested port is the pid this run started", f":{port}")

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


# ------------------------------------------------- scratch-port rotation
# (QA-CRIER-10 — a fixed scratch port makes a probe skip or abort when anything
# already listens there: a long-lived unrelated listener, or a squatter that took
# the port between two runs of the same script. No provider call is made in any
# of these three cases.)


def case_port_rotate(work: Path) -> CaseResult:
    """A collision on the FIRST default candidate is rotated past, and the run
    still succeeds on the port it selected.

    The collision is deterministic: the squatter is a socket this selftest owns
    (bind+listen is synchronous, so there is no pick->bind race), and the
    rotated run is the real bridge-lane demo against the local stub — so a port
    that reached the server but not the bridges/harnesses/controller could not
    reach SOLVED here.
    """
    res = CaseResult("port-rotate")
    out_dir = work / "out"
    base = runner_base_port(BRIDGE_RUNNER)
    server, _state, stub_url = stub_llm.start_server("solved")
    squatter = None
    try:
        squatter = Squatter(base)
        res.check(ss_shows(base), "the first default candidate is occupied by a listener this case owns",
                  f":{base} · ss agrees: {ss_shows(base)}")
        env = scenario_env(out_dir, None, stub_url,
                           DOGFOOD_CONTROLLER_DEADLINE_S="60",
                           DOGFOOD_TIMEOUT_S="120",
                           DOGFOOD_MESH_WAIT_S="20",
                           DOGFOOD_ASK_TIMEOUT_S="20")
        rc, output, secs = run_runner(BRIDGE_RUNNER, env, 180)
        res.detail = output
        chosen = selected_port(output)
        product = read_product(out_dir)

        res.check(f"port-guard: candidate :{base} is in use" in output,
                  "the occupied candidate was named, with its holder, as the reason to rotate")
        res.check(chosen is not None, "a port was selected", f"selected {chosen}")
        res.check(chosen != base, "the selected port is NOT the occupied candidate",
                  f"squatted :{base}, selected {chosen}")
        res.check(chosen is not None and f"starting crier server on :{chosen}" in output,
                  "the server was started on the SELECTED port", f":{chosen}")
        res.check(rc == 0, "the rotated run still succeeds (no false skip)", f"exit {rc} after {secs:.1f}s")
        res.check(product.get("result") == "SOLVED", "PRODUCT result SOLVED",
                  f"got {product.get('result')!r}")
        res.check(product.get("finals_received") == 2,
                  "both finals crossed crier on the rotated port (no component kept the default)",
                  f"finals={product.get('finals_received')}")
        res.check(squatter.still_listening(),
                  f"the holder of :{base} was left running — a foreign listener is not ours to kill")
        return res
    finally:
        if squatter is not None:
            squatter.sock.close()
        server.shutdown()
        server.server_close()


def case_port_refuse(work: Path) -> CaseResult:
    """The fail-closed half of the rotation contract, on both lanes.

    Every default candidate occupied must be a NAMED non-zero failure that
    starts nothing; an OCCUPIED explicit CRIER_PORT must be refused rather than
    rotated; a FREE explicit CRIER_PORT must be used verbatim even while every
    default candidate is occupied.
    """
    res = CaseResult("port-refuse")
    budget = 3
    bridge_candidates = [runner_base_port(BRIDGE_RUNNER) + i for i in range(budget)]
    mesh_candidates = [runner_base_port(MESH_RUNNER) + i for i in range(budget)]
    ports = sorted(set(bridge_candidates) | set(mesh_candidates))
    squatters: list[Squatter] = []
    try:
        squatters = squat_all(ports)
        res.check(all(s.still_listening() for s in squatters),
                  "every candidate port is occupied by a listener this selftest owns",
                  f":{ports}")
        res.check(all(ss_shows(p) for p in ports), "ss agrees the candidates are occupied", f":{ports}")

        # (a) the default path, every candidate occupied -> named failure
        out_a = work / "out-exhausted"
        env = scenario_env(out_a, None, "http://127.0.0.1:1", CRIER_PORT_CANDIDATES=str(budget))
        rc, output, secs = run_runner(MESH_RUNNER, env, 60)
        res.detail = output
        missing = [p for p in mesh_candidates if f":{p} — holder pid" not in output]
        res.check(rc != 0, "an exhausted candidate budget is a non-zero failure", f"exit {rc}")
        res.check(f"all {budget} scratch-port candidate(s) from :{mesh_candidates[0]} are in use" in output,
                  "the failure names the budget and its base", f":{mesh_candidates[0]} x{budget}")
        res.check(not missing, "every attempted candidate is listed with its holder", f"missing={missing}")
        res.check("port-guard: selected" not in output, "no port was selected")
        res.check(not out_a.exists(), "nothing was started (the run failed before its out dir existed)")

        # (b) an OCCUPIED explicit port -> refused, never rotated
        out_b = work / "out-explicit-busy"
        env = scenario_env(out_b, bridge_candidates[0], "http://127.0.0.1:1",
                           CRIER_PORT_CANDIDATES=str(budget))
        rc, output, secs = run_runner(BRIDGE_RUNNER, env, 60)
        res.detail = output
        res.check(rc != 0, "an occupied explicit CRIER_PORT is a non-zero failure", f"exit {rc}")
        res.check(f"the explicit port :{bridge_candidates[0]} is already in use" in output,
                  "the refusal names the explicitly requested port")
        res.check("holder pid :" in output, "the refusal names the holder of that port")
        res.check("port-guard: candidate :" not in output,
                  "the candidate list was NOT consulted (an explicit port is never rotated)")
        res.check("port-guard: selected" not in output, "no port was selected")
        res.check(not out_b.exists(), "nothing was started")

        # (c) a FREE explicit port wins even while every candidate is occupied
        out_c = work / "out-explicit-free"
        wanted = free_port()
        res.check(port_is_free(wanted), "premise: the requested port is free to bind", f":{wanted}")
        res.check(not ss_shows(wanted), "premise: ss agrees the requested port is free", f":{wanted}")
        env = scenario_env(out_c, wanted, "http://127.0.0.1:1",
                           CRIER_PORT_CANDIDATES=str(budget),
                           DOGFOOD_TIMEOUT_S="1",
                           DOGFOOD_CONTROLLER_DEADLINE_S="1",
                           DOGFOOD_MAX_TURNS="1",
                           DOGFOOD_ASK_TIMEOUT_S="1")
        rc, output, secs = run_runner(BRIDGE_RUNNER, env, 60)
        res.detail = output
        res.check(f"port-guard: selected :{wanted} for" in output,
                  "the explicit free port was selected verbatim", f":{wanted}")
        res.check("port-guard: candidate :" not in output,
                  "the occupied candidates were not consulted")
        res.check(f":{wanted} is held by pid" in output and "== the server pid" in output,
                  "the server that answered /health on the explicit port is the pid this run started",
                  f":{wanted}")
        res.check("==> launching controller" in output,
                  "the run proceeded past server startup", f"exit {rc} after {secs:.1f}s")
        res.check(rc in (0, 124), "the probe run is bounded (its own wall clock ends it)", f"exit {rc}")
        res.check(all(s.still_listening() for s in squatters),
                  "no squatter was disturbed by any of the three runs", f":{ports}")
        return res
    finally:
        for s in squatters:
            s.sock.close()


def case_port_wiring(work: Path) -> CaseResult:
    """Source invariants over BOTH runners — the half a live run cannot show: the
    port is CHOSEN by the shared helper and threaded to every component."""
    res = CaseResult("port-wiring")
    del work
    bases = {}
    for label, script in (("bridge", BRIDGE_RUNNER), ("mesh", MESH_RUNNER)):
        src = script.read_text(encoding="utf-8")
        bases[label] = runner_base_port(script)

        res.check('. "$REPO/scripts/lib/port-guard.sh"' in src,
                  f"{label}: sources the shared port-guard library")
        res.check('select_scratch_port "${CRIER_PORT:-}" "$PORT_BASE"' in src,
                  f"{label}: chooses its port with the shared selector (explicit override passed through)")
        res.check('"$PORT_CANDIDATES"' in src, f"{label}: threads the candidate budget to the selector")
        res.check('PORT="$PORT_GUARD_SELECTED"' in src,
                  f"{label}: uses the SELECTED port (PORT is not assigned anywhere else)")
        res.check(re.search(r'PORT="\$\{CRIER_PORT:-[0-9]', src) is None,
                  f"{label}: no hard-coded default port is left in the runner")
        res.check('ss -tln 2>/dev/null | grep -q ":$PORT "' not in src,
                  f"{label}: the ad-hoc `ss -tln | grep` busy probe is gone")
        res.check(re.search(r"ss -tln\w*\s*\|", src) is None,
                  f"{label}: the runner pipes no ss output itself (the shared helper owns the probe)")
        res.check(f'PORT_BASE={bases[label]}' in src, f"{label}: names its first candidate")
        res.check(f'wait_http_or_die "http://127.0.0.1:$PORT/health" "$SERVER_PID"' in src,
                  f"{label}: waits for /health through the shared guard")
        res.check(f'assert_port_owned "$PORT" "$SERVER_PID"' in src,
                  f"{label}: asserts the holder of the selected port is the process it started")
        res.check('CRIER_PORT="$PORT"' in src, f"{label}: starts the server on the selected port")

        blocks = invocation_blocks(src, "python3 -u ")
        unpinned = [b for b in blocks if "$PORT" not in b]
        res.check(len(blocks) >= 3, f"{label}: expects the component invocations to be present",
                  f"found {len(blocks)}")
        res.check(not unpinned,
                  f"{label}: every component invocation ({len(blocks)}) is pinned to the selected port",
                  f"unpinned={len(unpinned)}")

    res.check(bases["bridge"] != bases["mesh"],
              "the two lanes keep distinct candidate bases", str(bases))
    res.check(all(1024 <= p <= 65000 for p in bases.values()),
              "both candidate bases are in the usable port range", str(bases))

    mesh_src = MESH_RUNNER.read_text(encoding="utf-8")
    res.check(mesh_src.count('ws://127.0.0.1:$PORT/mesh/connect/') >= 2,
              "raw mesh clients are addressed on the selected port",
              str(mesh_src.count('ws://127.0.0.1:$PORT/mesh/connect/')))
    return res


CASE_FUNCS = {
    "mcp-timeout": case_mcp_timeout,
    "solved": case_solved,
    "wrong-answer": case_wrong_answer,
    "stalled": case_stalled,
    "wall-clock": case_wall_clock,
    "mesh-solved": case_mesh_solved,
    "port-rotate": case_port_rotate,
    "port-refuse": case_port_refuse,
    "port-wiring": case_port_wiring,
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
