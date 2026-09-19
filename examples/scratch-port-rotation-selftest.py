#!/usr/bin/env python3
"""Scratch-port rotation selftest (QA-CRIER-10).

The crier repo ships five example runners that start a scratch service (two
llm-mesh lanes, federation-demo, hermes-gateway-demo, ws-mesh-demo). Each one
used to bind ONE fixed default port, so a long-lived unrelated listener on that
port — or a squatter that took it between two runs of the same script — made the
whole probe abort (or, worse, measure the squatter). They now all choose their
port with the shared selector `select_scratch_port` in
`scripts/lib/port-guard.sh`: a bounded candidate rotation with per-candidate
preflight, holder evidence for every candidate skipped, a named failure when
every candidate is occupied, and a fail-closed check for a caller-named port
(never rotated — a run on a port the operator did not name would misreport what
was measured).

This selftest proves that contract for the THREE runners fixed in this change,
deterministically and without any provider call:

  wiring     source invariants per runner: the shared selector is used (one call
             per service, each taking that service's override variable), the
             candidate base/budget are threaded, the selected port comes back
             from PORT_GUARD_SELECTED, every component/client invocation is
             pinned to it, and no port literal survives anywhere else in the
             runner — so no component can be left on its own default. It also
             re-states the five-runner claim (all of them source the library and
             select their port).
  rotate     LIVE: the FIRST default candidate is squatted by a listener this
             selftest owns; the runner must name that candidate and its holder,
             rotate to the next candidate, run to its own PASS on the port it
             selected, and leave the squatter running (a foreign listener is not
             ours to kill). The runner's own PASS lines are the proof that the
             selected port reached every component and client.
  exhaust    every default candidate squatted -> a NAMED non-zero failure that
             lists the budget, each attempted port and its holder, selects
             nothing, and starts nothing.
  explicit   a caller-named port is authoritative: an OCCUPIED one is refused
             naming that port and its holder (never rotated to a free candidate),
             and a FREE one is used verbatim even while every default candidate
             is occupied.

The two llm-mesh lanes already carry these arms in `examples/llm-mesh/selftest.py`
(port-rotate / port-refuse / port-wiring) and the ws-mesh Go suite
(`examples/ws-mesh-demo/run_demo_e2e_test.go`) adds the pid-ownership proof for
the relay it starts; this file is the same three arms for the two runners that
had no selftest, plus the wiring invariants and a live rotate arm for ws-mesh.

Nothing here needs a model, a key or the network: federation-demo's webhook is a
stdlib echo server, hermes-gateway-demo's adapter answers canned when no key file
is present (this selftest runs it with a throwaway HOME so it cannot pick one up),
and ws-mesh-demo drives only this repo's own Go client.

Usage:
    python3 examples/scratch-port-rotation-selftest.py [--case NAME]... [--list]

Exit status: 0 only when every check behaved; 1 when a check failed; 2 when a
prerequisite tool is missing (the selftest never reports a green it did not earn).
"""

from __future__ import annotations

import argparse
import os
import random
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent

FEDERATION = HERE / "federation-demo" / "run-demo.sh"
GATEWAY = HERE / "hermes-gateway-demo" / "run-demo.sh"
WS_MESH = HERE / "ws-mesh-demo" / "run-demo.sh"
LLM_BRIDGE = HERE / "llm-mesh" / "run-demo.sh"
LLM_MESH = HERE / "llm-mesh" / "mesh" / "run-demo.sh"

# Every repo-shipped runner that starts a scratch service, with the port override
# variable each one exposes. The three this row fixed are covered by the live arms
# below; all five must at least share the selector (the llm-mesh lanes prove their
# own arms in examples/llm-mesh/selftest.py).
ALL_RUNNERS = {
    "llm-mesh-bridge": (LLM_BRIDGE, ["CRIER_PORT"]),
    "llm-mesh-raw": (LLM_MESH, ["CRIER_PORT"]),
    "federation": (FEDERATION, ["RELAY1_PORT", "RELAY2_PORT", "WEBHOOK_PORT"]),
    "hermes-gateway": (GATEWAY, ["CRIER_PORT", "ADAPTER_PORT"]),
    "ws-mesh": (WS_MESH, ["DEMO_PORT"]),
}

CASES = [
    "wiring",
    "federation-rotate",
    "federation-exhaust",
    "federation-explicit",
    "gateway-rotate",
    "gateway-exhaust",
    "gateway-explicit",
    "ws-mesh-rotate",
    "ws-mesh-exhaust",
    "ws-mesh-explicit",
]


# --------------------------------------------------------------- result plumbing


@dataclass
class CaseResult:
    name: str
    checks: list[tuple[bool, str, str]] = field(default_factory=list)
    detail: str = ""

    def check(self, ok: bool, what: str, detail: str = "") -> bool:
        self.checks.append((bool(ok), what, detail))
        return bool(ok)

    @property
    def failed(self) -> list[tuple[bool, str, str]]:
        return [c for c in self.checks if not c[0]]

    def report(self) -> None:
        if not self.checks:
            print(f"FAIL: {self.name}: no check ran")
            return
        for ok, what, detail in self.checks:
            if ok:
                print(f"  ok   {what}" + (f" ({detail})" if detail else ""))
            else:
                print(f"  FAIL {what}" + (f" — {detail}" if detail else ""))
        if self.failed:
            print(f"FAIL: {self.name}: {len(self.failed)}/{len(self.checks)} check(s) failed")
            if self.detail:
                print(f"--- {self.name} runner output (tail) ---")
                for line in self.detail.splitlines()[-40:]:
                    print(f"  | {line}")
        else:
            print(f"PASS: {self.name} ({len(self.checks)} checks)")


def selections(output: str) -> dict[str, int]:
    """label -> selected port, for every `port-guard: selected` line."""
    found: dict[str, int] = {}
    for port, label in re.findall(r"port-guard: selected :(\d+) for (.+?) — ", output):
        found[label.strip()] = int(port)
    return found


def selected_for(output: str, label: str) -> int | None:
    return selections(output).get(label)


def skipped_ports(output: str) -> list[int]:
    return [int(p) for p in re.findall(r"port-guard: candidate :(\d+) is in use", output)]


def refused_ports(output: str) -> list[int]:
    """Ports an explicit refusal named (`the explicit port :N is already in use`)."""
    return [int(p) for p in re.findall(r"the explicit port :(\d+) is already in use", output)]


def attempted_ports(output: str) -> list[int]:
    m = re.search(r"ERROR:\s+candidates tried:([^\n]*)\n", output)
    if not m:
        return []
    return [int(p.lstrip(":")) for p in m.group(1).split()]


def exhausted_ports(output: str) -> list[int]:
    """Every port listed with a holder in an exhaustion refusal."""
    return [int(p) for p in re.findall(r"ERROR:\s+:(\d+) — holder pid", output)]


# ------------------------------------------------------------------- fixtures


class Squatter:
    """An occupied port this selftest owns, held for the duration of a case.

    bind+listen is synchronous, so the collision is deterministic — there is no
    pick->bind race as there would be with a spawned listener.
    """

    def __init__(self, port: int):
        self.port = port
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            self.sock.bind(("127.0.0.1", port))
        except OSError as exc:
            self.sock.close()
            raise AssertionError(f"could not squat :{port} — something already holds it ({exc})") from exc
        self.sock.listen(16)

    @property
    def pid(self) -> int:
        return os.getpid()

    def close(self) -> None:
        self.sock.close()

    def still_listening(self) -> bool:
        try:
            with socket.create_connection(("127.0.0.1", self.port), timeout=1):
                return True
        except OSError:
            return False


def port_is_free(port: int) -> bool:
    with socket.socket() as s:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
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


def free_run(count: int, low: int = 24000, high: int = 40000, tries: int = 80) -> list[int]:
    """`count` CONSECUTIVE free ports — a rotation fixture needs neighbours.

    The block is picked randomly and never fixed, so a busy box cannot make the
    arms flake; when no run can be found the caller reports that instead of
    pretending the arm ran.
    """
    for _ in range(tries):
        base = random.randrange(low, high - count)
        if all(port_is_free(base + i) for i in range(count)):
            return [base + i for i in range(count)]
    return []


def clean_env(extra: dict[str, str]) -> dict[str, str]:
    """The ambient environment minus every example-runner override, plus `extra`.

    An inherited CRIER_PORT / DEMO_PORT / RELAY*_PORT / WEBHOOK_PORT would decide
    the arm by accident (and an inherited CR_AUTH_TOKEN makes two of these demos
    refuse to run at all), so none of them is passed through.
    """
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("CR_", "CRIER_", "DEMO_", "RELAY", "WEBHOOK"))}
    env.update(extra)
    return env


def run_script(script: Path, env: dict[str, str], timeout: int) -> tuple[int, str, float]:
    """Run a shipped runner, bounded, capturing its combined output.

    The runner starts servers of its own, so it gets its own process group and the
    WHOLE group is signalled when the bound expires: a hang must not leave a relay
    or an adapter behind (the orphaning class DF-CRIER-254 is about), and the
    timeout is reported as exit 124 with a named line instead of a traceback.
    """
    start = time.monotonic()
    proc = subprocess.Popen(
        ["bash", str(script)],
        cwd=str(script.parent),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        start_new_session=True,
    )
    try:
        out, _ = proc.communicate(timeout=timeout)
        rc = proc.returncode
    except subprocess.TimeoutExpired:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
            out, _ = proc.communicate(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(proc.pid, signal.SIGKILL)
            out, _ = proc.communicate()
        out = (out or "") + f"\n[selftest] runner exceeded its {timeout}s bound and was signalled (exit 124)\n"
        rc = 124
    except BaseException:
        os.killpg(proc.pid, signal.SIGKILL)
        raise
    secs = time.monotonic() - start
    return rc, out or "", secs


def squat_all(res: CaseResult, ports: list[int]) -> list[Squatter]:
    """Squat every port, or report the premise failure and close what was taken.

    The ports come from `free_run`, so a bind failure means something raced this
    selftest for one — the arm cannot be run and says so instead of crashing.
    """
    squatters: list[Squatter] = []
    for port in ports:
        try:
            squatters.append(Squatter(port))
        except AssertionError as exc:
            res.check(False, f"the fixture port :{port} is squattable by this selftest", str(exc))
            for taken in squatters:
                taken.close()
            return []
    return squatters


def go_cache_env() -> dict[str, str]:
    """The real Go caches, so a runner started with a throwaway HOME still builds
    from the warm module/build cache instead of re-downloading the module tree."""
    extra = {}
    for var in ("GOPATH", "GOMODCACHE", "GOCACHE"):
        try:
            value = subprocess.run(["go", "env", var], capture_output=True, text=True).stdout.strip()
        except OSError:
            value = ""
        if value:
            extra[var] = value
    return extra


# ------------------------------------------------------------------- wiring

@dataclass
class Service:
    port_var: str
    base_var: str
    budget_var: str
    label: str


# The three runners fixed in this change: how their services are wired, and the
# lines that prove each component/client is addressed through a SELECTED port.
RUNNER_WIRING = {
    "federation": dict(
        script=FEDERATION,
        services=[
            Service("RELAY1_PORT", "RELAY1_PORT_BASE", "RELAY1_PORT_CANDIDATES", "relay-1 (federation-demo)"),
            Service("RELAY2_PORT", "RELAY2_PORT_BASE", "RELAY2_PORT_CANDIDATES", "relay-2 (federation-demo)"),
            Service("WEBHOOK_PORT", "WEBHOOK_PORT_BASE", "WEBHOOK_PORT_CANDIDATES", "the echo webhook (federation-demo)"),
        ],
        pinned=[
            'python3 "$DEMO_DIR/echo_webhook.py" "$WEBHOOK_PORT"',
            'CRIER_PORT="$RELAY2_PORT"',
            'CRIER_PORT="$RELAY1_PORT"',
            'CR_FED_LINKS="http://127.0.0.1:${RELAY2_PORT}"',
            'WEBHOOK_URL="http://127.0.0.1:${WEBHOOK_PORT}/webhook"',
            'RELAY1="http://127.0.0.1:${RELAY1_PORT}"',
            'RELAY2="http://127.0.0.1:${RELAY2_PORT}"',
            'assert_port_owned "$WEBHOOK_PORT"',
            'assert_port_owned "$RELAY2_PORT"',
            'assert_port_owned "$RELAY1_PORT"',
            'wait_http_or_die "$RELAY2/health"',
            'wait_http_or_die "$RELAY1/health"',
            '"$RELAY1/agents"',
            '"$RELAY1/fed/peers"',
            '"$RELAY2/fed/peers"',
        ],
    ),
    "hermes-gateway": dict(
        script=GATEWAY,
        services=[
            Service("CRIER_PORT", "CRIER_PORT_BASE", "CRIER_PORT_CANDIDATES", "the crier server (hermes-gateway-demo)"),
            Service("ADAPTER_PORT", "ADAPTER_PORT_BASE", "ADAPTER_PORT_CANDIDATES", "the gateway adapter (hermes-gateway-demo)"),
        ],
        pinned=[
            'WEBHOOK_URL="http://127.0.0.1:${ADAPTER_PORT}/webhook"',
            'CRIER_BASE="http://127.0.0.1:${CRIER_PORT}"',
            'ADAPTER_PORT="$ADAPTER_PORT"',
            'CRIER_PORT="$CRIER_PORT" CR_REQUIRE_AGENT_SIG=false',
            'wait_http_or_die "http://127.0.0.1:$ADAPTER_PORT/healthz"',
            'assert_port_owned "$ADAPTER_PORT"',
            'assert_port_owned "$CRIER_PORT"',
            'wait_http_or_die "$CRIER_BASE/health"',
            '"$CRIER_BASE/agents"',
            '"http://127.0.0.1:$ADAPTER_PORT/sessions"',
        ],
    ),
    "ws-mesh": dict(
        script=WS_MESH,
        services=[
            Service("DEMO_PORT", "DEMO_PORT_BASE", "DEMO_PORT_CANDIDATES", "the ws-mesh-demo relay"),
        ],
        pinned=[
            'BASE="http://127.0.0.1:${DEMO_PORT}"',
            'CRIER_PORT="$DEMO_PORT" CR_LOG_LEVEL=warn',
            'assert_port_owned "$DEMO_PORT" "$SERVER_PID"',
            'curl -sfS "$BASE/health"',
            '-url "$BASE" subscribe',
            '-url "$BASE" peer -agent demo-agent-b',
            '-url "$BASE" peer -agent demo-agent-a',
            '-url "$BASE" roundtrip',
        ],
    ),
}

# A literal port in a runner BODY is a component that will fall back to its own
# default. The only allowed literal is a first-candidate definition
# (`RELAY1_PORT_BASE="${RELAY1_PORT_BASE:-18771}"`, `PORT_BASE=18777`), and the
# range checked is the one these demos live in (18xxx) — 60000-style timeouts are
# not port literals.
BASE_DEF_RE = re.compile(r'^\s*[A-Z0-9_]*PORT_BASE="?(\$\{[A-Z0-9_]*:-)?\d+')
PORT_LITERAL_RE = re.compile(r"(?<![\w.:])(1[89]\d{3})(?![\w])")
HARD_DEFAULT_RE = re.compile(r'(?m)^\s*(?P<var>[A-Z][A-Z0-9_]*)="\$\{(?P=var):-(?P<port>[1-9]\d*)\}"')


def case_wiring(work: Path) -> CaseResult:
    res = CaseResult("wiring")
    del work

    # (0) the five-runner claim: every shipped scratch-service runner selects its
    #     port with the shared selector instead of hard-coding one.
    for label, (script, overrides) in ALL_RUNNERS.items():
        src = script.read_text(encoding="utf-8")
        res.check("scripts/lib/port-guard.sh" in src, f"{label}: sources the shared port-guard library")
        res.check("select_scratch_port" in src, f"{label}: chooses its port with select_scratch_port")
        res.check("PORT_GUARD_SELECTED" in src, f"{label}: takes the port from PORT_GUARD_SELECTED")
        calls = len(re.findall(r"(?m)^\s*select_scratch_port ", src))
        res.check(calls >= len(overrides),
                  f"{label}: one select_scratch_port call per service ({len(overrides)})",
                  f"found {calls} call(s)")
        for var in overrides:
            res.check(f'"${{{var}:-}}"' in src,
                      f"{label}: passes ${var} through as the explicit override (never defaulted to a port)")

    # (1) per-runner detail for the three runners fixed here.
    for label, spec in RUNNER_WIRING.items():
        script: Path = spec["script"]
        src = script.read_text(encoding="utf-8")
        services: list[Service] = spec["services"]

        for svc in services:
            res.check(f'select_scratch_port "${{{svc.port_var}:-}}"' in src,
                      f"{label}/{svc.port_var}: the selector is given this service's override variable")
            res.check(f"{svc.base_var}=" in src,
                      f"{label}/{svc.port_var}: names its first candidate ({svc.base_var})")
            res.check(f'{svc.port_var}="$PORT_GUARD_SELECTED"' in src,
                      f"{label}/{svc.port_var}: binds the SELECTED port to {svc.port_var}")
            res.check(f'"${svc.budget_var}"' in src,
                      f"{label}/{svc.port_var}: threads its candidate budget to the selector")

        offenders = [m[0] for m in HARD_DEFAULT_RE.findall(src) if m[0] in {s.port_var for s in services}]
        res.check(not offenders,
                  f"{label}: no service port is defaulted to a literal port",
                  f"offenders={offenders}")

        for marker in spec["pinned"]:
            res.check(marker in src, f"{label}: component/client invocation pinned — {marker!r}")

        strays: list[str] = []
        for lineno, line in enumerate(src.splitlines(), 1):
            if line.strip().startswith("#") or BASE_DEF_RE.match(line):
                continue
            m = PORT_LITERAL_RE.search(line)
            if m:
                strays.append(f"line {lineno}: {line.strip()}")
        res.check(not strays,
                  f"{label}: no port literal survives outside the candidate-base definitions",
                  "; ".join(strays))

    # (2) the two llm-mesh lanes keep their own (already proven) wiring; restate
    #     the core shape here so a regression shows up in this suite too.
    for label, script in (("llm-mesh-bridge", LLM_BRIDGE), ("llm-mesh-raw", LLM_MESH)):
        src = script.read_text(encoding="utf-8")
        res.check('PORT="$PORT_GUARD_SELECTED"' in src, f"{label}: uses the selected port")
        res.check(re.search(r"^\s*PORT_BASE=\d+", src, re.M) is not None,
                  f"{label}: names its first candidate")
        res.check(re.search(r'PORT="\$\{CRIER_PORT:-[0-9]', src) is None,
                  f"{label}: no hard-coded default port is left")
    return res


# --------------------------------------------------------------- live arms

@dataclass
class LiveRunner:
    name: str
    script: Path
    services: list[Service]
    live_env: dict[str, str]
    pass_marker: str
    refusal_asserts: tuple[str, ...]
    transcript_var: str = "DEMO_TRANSCRIPT"
    # False when the runner opens its capture BEFORE the port is settled (it pipes
    # its whole body through tee), so "no transcript" cannot evidence "nothing was
    # started" there — the refusal_asserts prove that instead.
    transcript_after_selection: bool = True

    @property
    def primary(self) -> Service:
        return self.services[0]


LIVE = {
    "federation": LiveRunner(
        name="federation",
        script=FEDERATION,
        services=RUNNER_WIRING["federation"]["services"],
        live_env={},
        pass_marker="DEMO PASS",
        refusal_asserts=("==> [1/8] build crier", "built "),
    ),
    "hermes-gateway": LiveRunner(
        name="hermes-gateway",
        script=GATEWAY,
        services=RUNNER_WIRING["hermes-gateway"]["services"],
        live_env={},
        pass_marker="DEMO PASSED",
        refusal_asserts=("[1/7] build crier server", "built:"),
        transcript_after_selection=False,
    ),
    "ws-mesh": LiveRunner(
        name="ws-mesh",
        script=WS_MESH,
        services=RUNNER_WIRING["ws-mesh"]["services"],
        live_env={"DEMO_KEEPALIVE_WAIT": "0"},
        pass_marker="DEMO PASS",
        refusal_asserts=("==> [1/10] build crier", "built "),
    ),
}


def live_env_for(runner: LiveRunner, work: Path, extra: dict[str, str], tag: str = "run") -> dict[str, str]:
    """Environment for one live run: no inherited override, capture out of repo.

    hermes-gateway-demo reads `~/.hermes/.env` for a DEEPSEEK_API_KEY and would
    make two real model calls if it found one, so it runs with a throwaway HOME
    (and the real Go caches, so the build stays warm). No arm needs a provider.

    `extra` is applied LAST, so the explicit port overrides and the candidate
    bases a case chooses are never filtered away.
    """
    work.mkdir(parents=True, exist_ok=True)
    env = dict(runner.live_env)
    env[runner.transcript_var] = str(work / f"{runner.name}-{tag}-transcript.md")
    if runner.name == "hermes-gateway":
        home = work / "home"
        home.mkdir(parents=True, exist_ok=True)
        env["HOME"] = str(home)
        env.update(go_cache_env())
    env = clean_env(env)
    env.update(extra)
    return env


def service_blocks(count_per_service: int, services: int) -> list[list[int]]:
    """One free run per service, each long enough for that service's budget."""
    blocks = []
    for _ in range(services):
        block = free_run(count_per_service)
        if not block:
            return []
        blocks.append(block)
    return blocks


def block_env(runner: LiveRunner, blocks: list[list[int]], budget: int) -> dict[str, str]:
    env = {}
    for svc, block in zip(runner.services, blocks):
        env[svc.base_var] = str(block[0])
        env[svc.budget_var] = str(budget)
    return env


def case_rotate(runner: LiveRunner, work: Path, budget: int = 3) -> CaseResult:
    """First default candidate occupied -> rotate to the next, run still PASSes.

    The collision is deterministic (a socket this selftest owns), and the run is
    the real runner: its own PASS line can only be printed when every component
    and client used the port the selector chose.
    """
    res = CaseResult(f"{runner.name}-rotate")
    blocks = service_blocks(budget, len(runner.services))
    if not blocks:
        res.check(False, f"{runner.name}: a run of {budget} consecutive free ports is available for the fixture")
        return res

    primary = runner.primary
    squatted = blocks[0][0]
    try:
        squatter = Squatter(squatted)
    except AssertionError as exc:
        res.check(False, f"{runner.name}: the first default candidate is squattable by this selftest", str(exc))
        return res
    try:
        res.check(ss_shows(squatted), f"{runner.name}: the squatted candidate is really occupied (ss agrees)",
                  f":{squatted}")
        env = live_env_for(runner, work, block_env(runner, blocks, budget), tag="rotate")
        rc, output, secs = run_script(runner.script, env, 300)
        res.detail = output
        chosen = selections(output)

        want = blocks[0][1]
        res.check(selected_for(output, primary.label) == want,
                  f"{runner.name}: the occupied FIRST candidate :{squatted} was rotated past to :{want}",
                  f"selected {chosen.get(primary.label)}")
        res.check(squatted in skipped_ports(output),
                  f"{runner.name}: the skipped candidate is named as the reason to rotate",
                  f"skipped={skipped_ports(output)}")
        res.check(f"port-guard:   holder pid : {squatter.pid}" in output,
                  f"{runner.name}: the skip names the holder pid ({squatter.pid}) of the squatted candidate")
        res.check(f"ss -tlnp | grep :{squatted}" in output,
                  f"{runner.name}: the skip prints the audit command for the squatted candidate")
        for svc, block in list(zip(runner.services, blocks))[1:]:
            res.check(selected_for(output, svc.label) == block[0],
                      f"{runner.name}: {svc.port_var} took its own free first candidate (no cross-service collision)",
                      f"selected {chosen.get(svc.label)} (want {block[0]})")
        res.check(rc == 0 and runner.pass_marker in output,
                  f"{runner.name}: the rotated run still succeeds — no false skip",
                  f"exit {rc} after {secs:.1f}s")
        res.check(squatter.still_listening(),
                  f"{runner.name}: the holder of :{squatted} was left running (a foreign listener is not ours to kill)")
        return res
    finally:
        squatter.close()


def case_exhaust(runner: LiveRunner, work: Path, budget: int = 3) -> CaseResult:
    """Every default candidate occupied -> named non-zero failure, nothing started."""
    res = CaseResult(f"{runner.name}-exhaust")
    blocks = service_blocks(budget, len(runner.services))
    if not blocks:
        res.check(False, f"{runner.name}: a run of {budget} consecutive free ports is available for the fixture")
        return res
    primary = runner.primary
    squatters: list[Squatter] = []
    try:
        squatters = squat_all(res, blocks[0])
        if not squatters:
            return res
        res.check(all(s.still_listening() for s in squatters),
                  f"{runner.name}: every {primary.port_var} candidate is occupied by a listener this selftest owns",
                  f":{blocks[0]}")
        res.check(all(ss_shows(p) for p in blocks[0]),
                  f"{runner.name}: ss agrees the candidates are occupied", f":{blocks[0]}")

        env = live_env_for(runner, work, block_env(runner, blocks, budget), tag="exhaust")
        transcript = Path(env[runner.transcript_var])
        rc, output, secs = run_script(runner.script, env, 120)
        res.detail = output
        listed = sorted(exhausted_ports(output))
        tried = sorted(attempted_ports(output))

        res.check(rc != 0, f"{runner.name}: an exhausted candidate budget is a non-zero failure",
                  f"exit {rc} after {secs:.1f}s")
        res.check(f"all {budget} scratch-port candidate(s) from :{blocks[0][0]} are in use" in output,
                  f"{runner.name}: the failure names the budget and its base",
                  f":{blocks[0][0]} x{budget}")
        res.check(tried == blocks[0] or listed == blocks[0],
                  f"{runner.name}: every attempted candidate is listed",
                  f"tried={tried} listed={listed} want={blocks[0]}")
        missing = [p for p in blocks[0] if f":{p} — holder pid" not in output]
        res.check(not missing, f"{runner.name}: each attempted candidate carries its holder", f"missing={missing}")
        res.check(f"holder pid {squatters[0].pid}," in output,
                  f"{runner.name}: the holder pid of an attempted candidate is named ({squatters[0].pid})")
        res.check("port-guard: selected" not in output, f"{runner.name}: no port was selected")
        if runner.transcript_after_selection:
            res.check(not transcript.exists(),
                      f"{runner.name}: nothing was started (no transcript was opened)", str(transcript))
        for marker in runner.refusal_asserts:
            res.check(marker not in output, f"{runner.name}: the refusal happened before {marker.strip()!r}")
        res.check(all(s.still_listening() for s in squatters), f"{runner.name}: no squatter was disturbed")
        return res
    finally:
        for s in squatters:
            s.close()


def case_explicit(runner: LiveRunner, work: Path, budget: int = 3) -> CaseResult:
    """A caller-named port is authoritative: checked, never rotated."""
    res = CaseResult(f"{runner.name}-explicit")
    blocks = service_blocks(budget, len(runner.services))
    free_block = free_run(1)
    if not blocks or not free_block:
        res.check(False, f"{runner.name}: a free block and a free port outside it are available for the fixture")
        return res
    primary = runner.primary
    occupied = blocks[0][0]
    want_port = free_block[0]
    squatters: list[Squatter] = []
    try:
        squatters = squat_all(res, blocks[0])
        if not squatters:
            return res

        # (a) an OCCUPIED explicit port -> refused, naming it and its holder. Every
        # service's block is on the fixture ports, so the arm cannot be decided by
        # whatever the host happens to be running on the runner's own defaults.
        env = live_env_for(runner, work, block_env(runner, blocks, budget) |
                           {primary.port_var: str(occupied)}, tag="explicit-busy")
        busy_transcript = Path(env[runner.transcript_var])
        rc, output, secs = run_script(runner.script, env, 120)
        res.detail = output
        res.check(rc != 0, f"{runner.name}: an occupied explicit {primary.port_var} is a non-zero failure",
                  f"exit {rc} after {secs:.1f}s")
        res.check(occupied in refused_ports(output),
                  f"{runner.name}: the refusal names the explicitly requested port", f":{occupied}")
        res.check(f"holder pid : {squatters[0].pid}" in output,
                  f"{runner.name}: the refusal names that port's holder ({squatters[0].pid})")
        res.check(not skipped_ports(output),
                  f"{runner.name}: the candidate list was NOT consulted (an explicit port is never rotated)",
                  f"skips={skipped_ports(output)}")
        res.check("port-guard: selected" not in output, f"{runner.name}: no port was selected")
        if runner.transcript_after_selection:
            res.check(not busy_transcript.exists(), f"{runner.name}: nothing was started")
        for marker in runner.refusal_asserts:
            res.check(marker not in output, f"{runner.name}: the refusal happened before {marker.strip()!r}")

        # (b) a FREE explicit port wins even while every default candidate is occupied.
        env = live_env_for(runner, work, block_env(runner, blocks, budget) |
                           {primary.port_var: str(want_port)}, tag="explicit-free")
        rc, output, secs = run_script(runner.script, env, 300)
        res.detail = output
        res.check(selected_for(output, primary.label) == want_port,
                  f"{runner.name}: the free explicit port is used verbatim", f":{want_port}")
        res.check("explicit (the port the caller named" in output,
                  f"{runner.name}: the selection states the port was the caller's (not a candidate)")
        res.check(not skipped_ports(output), f"{runner.name}: the occupied candidates were not consulted",
                  f"skips={skipped_ports(output)}")
        res.check(rc == 0 and runner.pass_marker in output,
                  f"{runner.name}: the run on the explicit port still succeeds",
                  f"exit {rc} after {secs:.1f}s")
        res.check(all(s.still_listening() for s in squatters),
                  f"{runner.name}: no squatter was disturbed by either run")
        return res
    finally:
        for s in squatters:
            s.close()


# ------------------------------------------------- runner-specific live evidence

def case_federation_rotate(work: Path) -> CaseResult:
    res = case_rotate(LIVE["federation"], work)
    if not res.detail:
        return res
    svcs = RUNNER_WIRING["federation"]["services"]
    r1 = selected_for(res.detail, svcs[0].label)
    r2 = selected_for(res.detail, svcs[1].label)
    wh = selected_for(res.detail, svcs[2].label)
    if not (r1 and r2 and wh):
        res.check(False, "federation: all three services selected a port", f"got {(r1, r2, wh)}")
        return res
    res.check(f"- relay-1: http://127.0.0.1:{r1} " in res.detail,
              "federation: the transcript addresses relay-1 on the SELECTED port", f":{r1}")
    res.check(f"CR_FED_LINKS=http://127.0.0.1:{r2}" in res.detail,
              "federation: relay-1 was started linked to relay-2's SELECTED port", f":{r2}")
    res.check(f"http://127.0.0.1:{wh}/webhook" in res.detail,
              "federation: the webhook the agent registered is the SELECTED webhook port", f":{wh}")
    res.check(f"start relay-2 on :{r2}" in res.detail,
              "federation: relay-2 was started on the SELECTED port")
    res.check(re.search(rf"127\.0\.0\.1:{r2}([^0-9]|$)", res.detail) is not None,
              "federation: the /fed/peers listing names the SELECTED relay-2 port")
    return res


def case_gateway_rotate(work: Path) -> CaseResult:
    res = case_rotate(LIVE["hermes-gateway"], work)
    if not res.detail:
        return res
    svcs = RUNNER_WIRING["hermes-gateway"]["services"]
    crier = selected_for(res.detail, svcs[0].label)
    adapter = selected_for(res.detail, svcs[1].label)
    if not (crier and adapter):
        res.check(False, "hermes-gateway: both services selected a port", f"got {(crier, adapter)}")
        return res
    res.check(f"crier up: http://127.0.0.1:{crier} " in res.detail,
              "hermes-gateway: the crier server came up on the SELECTED port", f":{crier}")
    res.check(f"adapter up: http://127.0.0.1:{adapter}/webhook" in res.detail,
              "hermes-gateway: the adapter came up on the SELECTED port", f":{adapter}")
    res.check(f"registered gateway-agent (webhook -> http://127.0.0.1:{adapter}/webhook" in res.detail,
              "hermes-gateway: the webhook the agent registered is the SELECTED adapter port")
    res.check("canned but session-aware" in res.detail,
              "hermes-gateway: the adapter answered canned — no provider was called")
    res.check("PASS: adapter recorded 2 turns under ONE session_id" in res.detail,
              "hermes-gateway: the session check ran against the SELECTED adapter port")
    return res


def case_ws_mesh_rotate(work: Path) -> CaseResult:
    res = case_rotate(LIVE["ws-mesh"], work)
    if not res.detail:
        return res
    port = selected_for(res.detail, RUNNER_WIRING["ws-mesh"]["services"][0].label)
    if not port:
        res.check(False, "ws-mesh: a port was selected")
        return res
    res.check(f":{port} is held by pid" in res.detail,
              "ws-mesh: the relay answering /health is the pid this run started", f":{port}")
    res.check(f"- relay: http://127.0.0.1:{port} (auth-disabled)" in res.detail,
              "ws-mesh: the transcript addresses the relay on the SELECTED port")
    res.check(f"[2/10] start relay on :{port}" in res.detail,
              "ws-mesh: the relay was started on the SELECTED port")
    res.check('"count":0' in res.detail,
              "ws-mesh: the relay reported an empty mesh at startup (it is the run's own)")
    return res


# ------------------------------------------------------------------- driver

def main() -> int:
    parser = argparse.ArgumentParser(description="scratch-port rotation selftest (QA-CRIER-10)")
    parser.add_argument("--case", action="append", default=[], choices=CASES,
                        help="run only these cases (repeatable)")
    parser.add_argument("--list", action="store_true", help="list the cases and exit")
    args = parser.parse_args()

    if args.list:
        for name in CASES:
            print(name)
        return 0

    required = ["bash", "go", "python3", "curl", "ss", "openssl", "sha256sum"]
    missing = [tool for tool in required if shutil.which(tool) is None]
    if missing:
        print(f"scratch-port-rotation-selftest: missing prerequisite tool(s): {', '.join(missing)}",
              file=sys.stderr)
        return 2

    selected = args.case or CASES
    results: list[CaseResult] = []
    with tempfile.TemporaryDirectory(prefix="scratch-port-rotation-") as tmp:
        work = Path(tmp)
        for name in selected:
            if name == "wiring":
                results.append(case_wiring(work / "wiring"))
            elif name == "federation-rotate":
                results.append(case_federation_rotate(work / "fed-rotate"))
            elif name == "federation-exhaust":
                results.append(case_exhaust(LIVE["federation"], work / "fed-exhaust"))
            elif name == "federation-explicit":
                results.append(case_explicit(LIVE["federation"], work / "fed-explicit"))
            elif name == "gateway-rotate":
                results.append(case_gateway_rotate(work / "gw-rotate"))
            elif name == "gateway-exhaust":
                results.append(case_exhaust(LIVE["hermes-gateway"], work / "gw-exhaust"))
            elif name == "gateway-explicit":
                results.append(case_explicit(LIVE["hermes-gateway"], work / "gw-explicit"))
            elif name == "ws-mesh-rotate":
                results.append(case_ws_mesh_rotate(work / "ws-rotate"))
            elif name == "ws-mesh-exhaust":
                results.append(case_exhaust(LIVE["ws-mesh"], work / "ws-exhaust"))
            elif name == "ws-mesh-explicit":
                results.append(case_explicit(LIVE["ws-mesh"], work / "ws-explicit"))
            else:  # pragma: no cover - argparse restricts this
                results.append(CaseResult(name))
            results[-1].report()

    failed = [r for r in results if r.failed or not r.checks]
    if failed:
        print(f"scratch-port-rotation-selftest: {len(results) - len(failed)}/{len(results)} case(s) behaved — FAIL")
        return 1
    print(f"scratch-port-rotation-selftest: {len(results)}/{len(results)} case(s) behaved")
    return 0


if __name__ == "__main__":
    sys.exit(main())
