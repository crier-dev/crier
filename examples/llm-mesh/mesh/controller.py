#!/usr/bin/env python3
"""Task controller for the Crier-mesh LLM demo.

The controller is also a mesh peer. It:
  1. waits until both agents appear in /mesh/peers
  2. sends each a /start REQUEST
  3. collects /final REQUESTs from both agents and verifies them against the
     task's ground-truth solution
  4. writes the run's PRODUCT (``<out>/verdict.json``) and exits 0 only if
     both agents solved the puzzle

Everything an agent submits travels over the same mesh protocol the agents
use between themselves — the controller is just another peer.

Bounded and named (DF-CRIER-214): the two waits are finite and every one of
them writes a product naming its cause, so a run can end in "no agents on the
mesh" or "no finals within Ns" instead of silence. Only a comparison of the
submitted assignments against the ground truth can produce "SOLVED".
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import threading
import time
import urllib.request

try:  # logs must survive a kill: never sit in a block buffer
    sys.stdout.reconfigure(line_buffering=True)
    sys.stderr.reconfigure(line_buffering=True)
except Exception:  # pragma: no cover - non-reconfigurable stream
    pass

from crier_mesh import MeshClient

AGENT_IDS = ["agent-a", "agent-b"]

DEFAULT_AGENT_WAIT_S = 60.0
DEFAULT_FINALS_WAIT_S = 300.0


def env_float(name: str, default: float) -> float:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError:
        return default
    return value if value > 0 else default


def parse_assignment(text: str) -> dict:
    """Extract A=.. B=.. C=.. D=.. from free-form agent answers."""
    out = {}
    for m in re.finditer(r"([ABCD])\s*=\s*([a-zA-Záéíóúñü ]+)", text):
        plot = m.group(1).upper()
        veg = m.group(2).strip().lower().rstrip("s")
        out[plot] = veg
    return out


def stamp() -> str:
    return time.strftime("%H:%M:%S")


class Controller:
    def __init__(self, ws_url: str, http_url: str, task: dict, out_dir: str = "out",
                 agent_wait_s: float = DEFAULT_AGENT_WAIT_S,
                 finals_wait_s: float = DEFAULT_FINALS_WAIT_S):
        self.ws_url = ws_url
        self.http_url = http_url
        self.task = task
        self.out_dir = out_dir
        self.agent_wait_s = agent_wait_s
        self.finals_wait_s = finals_wait_s
        self.solution = {k: v.lower() for k, v in task["solution"].items()}
        self.client: MeshClient | None = None
        self.finals: dict[str, dict] = {}          # agent_id -> parsed assignment
        self.verdicts: dict[str, str] = {}
        self.all_finals = threading.Event()
        self.wait_error = ""

    # ------------------------------------------------------------- product

    def product(self, result: str, cause: str | None = None) -> dict:
        out = {
            "result": result,
            "agents": {a: {"verdict": self.verdicts.get(a, "NO_SUBMISSION"),
                           "submitted": self.finals.get(a, {})} for a in AGENT_IDS},
            "ground_truth": self.solution,
            "finals_received": len(self.finals),
            "expected_finals": len(AGENT_IDS),
            "verified_by": "controller (raw mesh lane) against task-garden ground truth",
        }
        if cause:
            out["cause"] = cause
        return out

    def write_product(self, product: dict) -> None:
        os.makedirs(self.out_dir, exist_ok=True)
        path = os.path.join(self.out_dir, "verdict.json")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(json.dumps(product, sort_keys=True) + "\n")
        print(f"[{stamp()}] controller: PRODUCT {json.dumps(product, sort_keys=True)}")

    # ------------------------------------------------------------- mesh side

    def handle_final(self, frame: dict) -> dict:
        method = frame.get("method", "")
        path = frame.get("path", "")
        body = frame.get("body") or {}
        if isinstance(body, str):
            try:
                body = json.loads(body)
            except json.JSONDecodeError:
                body = {}
        if method == "SOLVE" and path == "/final":
            agent = body.get("agent", "?")
            answer = body.get("answer", "")
            assignment = parse_assignment(answer)
            self.finals[agent] = assignment
            ok = assignment == self.solution
            self.verdicts[agent] = "ACCEPTED" if ok else "REJECTED"
            print(f"[{time.strftime('%H:%M:%S')}] controller: FINAL from {agent}: "
                  f"{assignment} -> {self.verdicts[agent]}")
            if len(self.finals) == 2:
                self.all_finals.set()
            return {
                "status": self.verdicts[agent],
                "message": ("Correct! The puzzle is solved."
                            if ok else "Rejected — the assignment is not correct. "
                                       "You may keep working and try again."),
            }
        return {"status": "ignored"}

    # ------------------------------------------------------------- HTTP side

    def peers(self) -> set[str]:
        with urllib.request.urlopen(f"{self.http_url}/mesh/peers", timeout=5) as r:
            data = json.loads(r.read())
        return {p["agent_id"] for p in data.get("peers", [])}

    def wait_for_agents(self, timeout_s: float | None = None) -> bool:
        """Wait for both agents on the mesh; returns False on timeout."""
        if timeout_s is None:
            timeout_s = self.agent_wait_s
        self.wait_error = ""
        deadline = time.time() + timeout_s
        while time.time() < deadline:
            try:
                present = self.peers()
            except Exception as exc:  # named, not a traceback
                self.wait_error = f"{type(exc).__name__}: {exc}"
                time.sleep(0.5)
                continue
            if AGENT_IDS[0] in present and AGENT_IDS[1] in present:
                print(f"[{stamp()}] controller: both agents on mesh: {sorted(present)}")
                return True
            time.sleep(0.5)
        return False

    # ------------------------------------------------------------- main

    def run(self) -> int:
        self.client = MeshClient("controller", self.ws_url)
        self.client.respond(self.handle_final)
        self.client.connect()
        print(f"[{stamp()}] controller: registered on mesh")

        if not self.wait_for_agents():
            cause = (f"no both-agents-on-mesh within {self.agent_wait_s:g}s"
                     + (f" (last /mesh/peers error: {self.wait_error})" if self.wait_error else ""))
            print(f"controller: TIMEOUT — {cause}", file=sys.stderr)
            self.write_product(self.product("NO_VERDICT", cause))
            self.client.close()
            return 1

        print(f"[{stamp()}] controller: sending /start to both agents")
        for aid in AGENT_IDS:
            threading.Thread(
                target=self._send_start, args=(aid,), daemon=True).start()

        cause = None
        if not self.all_finals.wait(timeout=self.finals_wait_s):
            missing = [a for a in AGENT_IDS if a not in self.finals]
            cause = (f"only {len(self.finals)}/{len(AGENT_IDS)} finals within "
                     f"{self.finals_wait_s:g}s (missing: {', '.join(missing)})")
            print(f"controller: TIMEOUT — {cause}", file=sys.stderr)
        time.sleep(1.0)  # let in-flight RESPONSEs land

        print("\n=== VERDICT ===")
        for aid in AGENT_IDS:
            got = self.finals.get(aid, {})
            print(f"  {aid}: {self.verdicts.get(aid, 'NO_SUBMISSION')}  (submitted {got})")
        print(f"  ground truth: {self.solution}")
        rejected = [a for a in AGENT_IDS if self.verdicts.get(a) == "REJECTED"]
        all_ok = all(self.verdicts.get(a) == "ACCEPTED" for a in AGENT_IDS)
        if not all_ok and rejected:
            cause = (cause + "; " if cause else "") + f"rejected: {rejected}"
        print(f"  RESULT: {'BOTH AGENTS SOLVED IT' if all_ok else 'NOT ALL CORRECT'}")
        if not all_ok:
            print(f"  cause: {cause}", file=sys.stderr)
        self.write_product(self.product("SOLVED" if all_ok else "NOT_ALL_CORRECT", None if all_ok else cause))
        self.client.close()
        return 0 if all_ok else 1

    def _send_start(self, aid: str) -> None:
        try:
            self.client.request(aid, "SOLVE", "/start",
                                {"prompt": self.task["public_prompt"]},
                                timeout_ms=10000)
        except Exception as exc:
            print(f"controller: /start to {aid} failed: {exc}", file=sys.stderr)


def main() -> int:
    ap = argparse.ArgumentParser(description="Crier-mesh LLM demo controller")
    ap.add_argument("--task", required=True, help="task JSON path")
    ap.add_argument("--port", type=int, default=18777)
    ap.add_argument("--out", default=os.environ.get("DOGFOOD_OUT", "out"),
                    help="directory for verdict.json")
    ap.add_argument("--agent-wait", type=float,
                    default=env_float("DOGFOOD_AGENT_WAIT_S", DEFAULT_AGENT_WAIT_S),
                    help="seconds to wait for both agents on the mesh (default 60)")
    ap.add_argument("--finals-wait", type=float,
                    default=env_float("DOGFOOD_CONTROLLER_DEADLINE_S", DEFAULT_FINALS_WAIT_S),
                    help="seconds to wait for both finals (default 300)")
    args = ap.parse_args()
    task = json.load(open(args.task, encoding="utf-8"))
    c = Controller(f"ws://127.0.0.1:{args.port}/mesh/connect/controller",
                   f"http://127.0.0.1:{args.port}", task,
                   out_dir=args.out, agent_wait_s=args.agent_wait,
                   finals_wait_s=args.finals_wait)
    return c.run()


if __name__ == "__main__":
    sys.exit(main())
