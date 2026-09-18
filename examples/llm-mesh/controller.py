#!/usr/bin/env python3
"""Demo controller — also a harness-side peer, using ONLY the bridge tools.

It registers itself on the shared server through its own crier-mcp bridge,
waits (bounded) for both agents to appear on the live mesh and exercises the
mesh lane through the bridge — mesh_peers + one mesh_request round-trip —
while they are still connected, then polls its inbox for {kind:final}
submissions from both agents, verifies each against the ground truth, sends
verdicts back through the bridge, and writes the run's PRODUCT:
``<out>/verdict.json``.

The product is derived from the verification alone:

    {"result": "SOLVED", ...}          both agents' submissions matched ground truth
    {"result": "NOT_ALL_CORRECT", ...} at least one did not (or never submitted)

There is no other way to get "SOLVED" out of this file. A bounded wait that
expires names its cause instead of pretending: the run exits non-zero with the
missing submissions spelled out (DF-CRIER-214).
"""

from __future__ import annotations

import argparse
import json
import os
import re
import secrets
import sys
import time

try:  # logs must survive a kill: never sit in a block buffer
    sys.stdout.reconfigure(line_buffering=True)
    sys.stderr.reconfigure(line_buffering=True)
except Exception:  # pragma: no cover - non-reconfigurable stream
    pass

from mcp_client import MCPClient, MCPError, ensure_identity

AGENT_IDS = ["agent-a", "agent-b"]

DEFAULT_DEADLINE_S = 150.0
DEFAULT_VERDICT_GRACE_S = 3.0
DEFAULT_MESH_WAIT_S = 20.0
MESH_PING_ATTEMPTS = 3


def parse_assignment(text: str) -> dict:
    out = {}
    for m in re.finditer(r"([ABCD])\s*=\s*([a-zA-Záéíóúñü ]+)", text):
        plot = m.group(1).upper()
        veg = m.group(2).strip().lower().rstrip("s")
        out[plot] = veg
    return out


def stamp() -> str:
    return time.strftime("%H:%M:%S")


def env_float(name: str, default: float) -> float:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError:
        return default
    return value if value > 0 else default


class Controller:
    def __init__(self, task: dict, bridge: str, port: int, out_dir: str,
                 deadline_s: float = DEFAULT_DEADLINE_S,
                 verdict_grace_s: float = DEFAULT_VERDICT_GRACE_S,
                 mesh_wait_s: float = DEFAULT_MESH_WAIT_S):
        self.task = task
        self.solution = {k: v.lower() for k, v in task["solution"].items()}
        self.out_dir = out_dir
        self.deadline_s = deadline_s
        self.verdict_grace_s = verdict_grace_s
        self.mesh_wait_s = mesh_wait_s
        self.bridge = MCPClient(
            bridge,
            {
                "CRIER_AGENT_ID": "controller",
                "CRIER_HTTP_URL": f"http://127.0.0.1:{port}",
                "CRIER_MESH_URL": f"ws://127.0.0.1:{port}/mesh/connect/controller",
            },
            "controller", out_dir,
        )
        self.finals: dict[str, dict] = {}
        self.verdicts: dict[str, str] = {}
        self.mesh: dict = {}

    # ------------------------------------------------------------- product

    def product(self) -> dict:
        """The run's product: the controller's verification, nothing else."""
        # SOLVED is the conjunction of two independent facts per agent: the
        # submission was RECEIVED, and the received assignment equals the
        # task's ground truth. Verdicts are only ever set from a received
        # final, but the assignments are re-checked here so no code path can
        # print SOLVED from a flag alone (DF-CRIER-214 rework).
        accepted = [a for a in AGENT_IDS
                    if self.verdicts.get(a) == "ACCEPTED"
                    and self.finals.get(a) == self.solution]
        solved = len(accepted) == len(AGENT_IDS)
        if solved:
            result, cause = "SOLVED", None
        else:
            missing = [a for a in AGENT_IDS if a not in self.finals]
            rejected = [a for a in AGENT_IDS if a in self.finals and a not in accepted]
            result = "NOT_ALL_CORRECT"
            cause = (f"missing finals from {missing}" if missing else "") + \
                    (f"; rejected: {rejected}" if rejected else "")
        out = {
            "result": result,
            "agents": {a: {"verdict": self.verdicts.get(a, "NO_SUBMISSION"),
                           "submitted": self.finals.get(a, {})} for a in AGENT_IDS},
            "ground_truth": self.solution,
            "finals_received": len(self.finals),
            "expected_finals": len(AGENT_IDS),
            "mesh": self.mesh,
            "verified_by": "controller (bridge lane) against task-garden ground truth",
        }
        if cause:
            out["cause"] = cause
        return out

    def write_product(self, product: dict) -> None:
        path = os.path.join(self.out_dir, "verdict.json")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(json.dumps(product, sort_keys=True) + "\n")
        print(f"[{stamp()}] controller: PRODUCT {json.dumps(product, sort_keys=True)}")

    # ------------------------------------------------------------- main

    def run(self) -> int:
        self.bridge.initialize()
        tools = self.bridge.tools_list()
        print(f"[{stamp()}] controller: bridge tools: {', '.join(tools)}")

        try:
            note = ensure_identity(self.bridge, "controller", ["controller"],
                                   secrets.token_hex(32))
        except MCPError as exc:
            print(f"[{stamp()}] controller: FATAL identity: {exc}", file=sys.stderr)
            self.bridge.close()
            return 1
        print(f"[{stamp()}] controller: {note}")

        # The live mesh lane is exercised BEFORE the finals loop, because an
        # agent leaves the mesh the moment its harness exits — which is right
        # after it reads ACCEPTED. A probe run after the verdicts reached
        # nobody: mesh_peers showed only the controller and the PING was
        # answered CONTROLLER_OFFLINE ("peer agent-a not connected") by the
        # server, not by the peer (DF-CRIER-214 rework). Probing here keeps
        # the real wire path and runs it while both agents are connected.
        # Bounded: mesh_wait_s, then the finals deadline below, then the
        # verdict grace — the whole run is still finite and named.
        self.exercise_mesh()

        deadline = time.time() + self.deadline_s
        while time.time() < deadline:
            text, is_err = self.bridge.call("get_messages", {"max": 10})
            if not is_err:
                try:
                    msgs = json.loads(text).get("messages", [])
                except json.JSONDecodeError:
                    msgs = []
                for m in msgs:
                    self.handle_message(m)
                if len(self.finals) == len(AGENT_IDS):
                    break
            time.sleep(1)

        if len(self.finals) < len(AGENT_IDS):
            missing = [a for a in AGENT_IDS if a not in self.finals]
            print(f"[{stamp()}] controller: TIMEOUT after {self.deadline_s:g}s — "
                  f"only {len(self.finals)}/{len(AGENT_IDS)} finals "
                  f"(missing: {', '.join(missing)})", file=sys.stderr)
        else:
            # Both verdicts are on the agents' inboxes now; give them a moment
            # to read them (harness wait_for_verdict polls every 2s) before
            # this process exits, so a fast solve still shows the agents
            # receiving ACCEPTED instead of looking unanswered.
            time.sleep(self.verdict_grace_s)

        print("\n=== VERDICT ===")
        for aid in AGENT_IDS:
            print(f"  {aid}: {self.verdicts.get(aid, 'NO_SUBMISSION')}  "
                  f"(submitted {self.finals.get(aid, {})})")
        print(f"  ground truth: {self.solution}")

        # The live mesh probe already ran before the finals loop while both
        # agents were connected. Do not probe again after the agents consume
        # their verdicts: that second probe would overwrite successful evidence
        # with CONTROLLER_OFFLINE after normal process shutdown.
        product = self.product()
        all_ok = product["result"] == "SOLVED"
        print(f"  RESULT: {'BOTH AGENTS SOLVED IT' if all_ok else 'NOT ALL CORRECT'}")
        if not all_ok:
            print(f"  cause: {product.get('cause', '')}", file=sys.stderr)
        self.write_product(product)

        self.bridge.close()
        return 0 if all_ok else 1

    def wait_for_mesh_peers(self) -> tuple[list[str], str]:
        """Poll ``mesh_peers`` until both agents are connected; bounded.

        This is the synchronization the probe needs — not a blind sleep. It
        returns as soon as both agents are visible (which the bridge does at
        its own startup) and gives up after ``self.mesh_wait_s`` with a NAMED
        reason instead of probing a lane that has nobody on it.

        Returns ``(peers, error)``; ``error`` is ``""`` only when both agents
        were present.
        """
        deadline = time.time() + self.mesh_wait_s
        peers: list[str] = []
        last_err = ""
        while True:
            try:
                text, is_err = self.bridge.call("mesh_peers", {})
                if is_err:
                    last_err = f"mesh_peers: {text}"
                    peers = []
                else:
                    try:
                        peers = sorted(json.loads(text).get("peers", []))
                    except json.JSONDecodeError:
                        last_err = f"mesh_peers returned non-JSON: {text[:120]!r}"
                        peers = []
            except MCPError as exc:
                last_err = f"mesh_peers: {exc}"
                peers = []
            if all(a in peers for a in AGENT_IDS):
                print(f"[{stamp()}] controller: mesh_peers -> {json.dumps(peers)} "
                      f"(both agents connected)")
                return peers, ""
            if time.time() >= deadline:
                reason = last_err or (f"mesh_peers showed {peers} — both agents not "
                                      f"connected within {self.mesh_wait_s:g}s")
                print(f"[{stamp()}] controller: mesh_peers -> {json.dumps(peers)} "
                      f"(incomplete: {reason})")
                return peers, reason
            time.sleep(0.5)

    def exercise_mesh(self) -> None:
        """Live mesh lane, through the bridge; recorded in the product.

        Called BEFORE the finals loop (an agent is gone from the mesh once its
        harness exits), so a successful probe here means the lane really
        carried a REQUEST/RESPONSE round-trip from the controller's bridge to a
        live peer bridge, and the product's ``mesh.peers`` is the evidence that
        both agents were connected at that moment. A mesh that is not exercised
        is recorded with its cause — never silently passed.
        """
        peers, wait_err = self.wait_for_mesh_peers()
        self.mesh["peers"] = peers
        self.mesh["mesh_wait_s"] = self.mesh_wait_s
        self.mesh["peers_ok"] = all(a in peers for a in AGENT_IDS)
        if not self.mesh["peers_ok"]:
            self.mesh["peers_error"] = wait_err
            print(f"[{stamp()}] controller: mesh probe skipped/mis-targeted: {wait_err}",
                  file=sys.stderr)

        for attempt in range(1, MESH_PING_ATTEMPTS + 1):
            try:
                text, is_err = self.bridge.call("mesh_request", {
                    "target": "agent-a", "method": "PING", "path": "/ping",
                    "timeout_ms": 5000,
                })
            except MCPError as exc:
                text, is_err = str(exc), True
            print(f"[{stamp()}] controller: mesh_request PING agent-a (attempt "
                  f"{attempt}) -> {text if not is_err else 'ERR ' + text}")
            # Recorded verbatim: ok=false with the target's own error
            # (e.g. CONTROLLER_OFFLINE "peer agent-a not connected") is
            # evidence of an unexercised lane, not a success.
            self.mesh["ping_agent_a"] = {"ok": not is_err, "reply": text[:400],
                                         "attempts": attempt}
            if not is_err:
                return
            if "agent-a" not in peers:
                # A retry cannot help: the target is not on the mesh.
                return
            time.sleep(0.5)

    def handle_message(self, m: dict) -> None:
        payload = m.get("payload", {})
        kind = payload.get("kind")
        if kind == "final":
            agent = payload.get("agent", "?")
            answer = payload.get("answer", "")
            assignment = parse_assignment(answer)
            self.finals[agent] = assignment
            ok = assignment == self.solution
            self.verdicts[agent] = "ACCEPTED" if ok else "REJECTED"
            print(f"[{stamp()}] controller: FINAL from {agent}: {assignment} -> {self.verdicts[agent]}")
            text, is_err = self.bridge.call("send_message", {
                "agent_id": agent,
                "payload": {
                    "kind": "verdict", "accepted": ok,
                    "message": ("Correct! The puzzle is solved."
                                if ok else "Rejected — the assignment is not correct. You may keep working and try again."),
                },
            })
            if is_err:
                print(f"[{stamp()}] controller: verdict send to {agent} failed: {text}", file=sys.stderr)


def main() -> int:
    ap = argparse.ArgumentParser(description="Crier MCP-bridge demo controller")
    ap.add_argument("--task", required=True)
    ap.add_argument("--bridge", required=True, help="path to crier-mcp binary")
    ap.add_argument("--port", type=int, default=18777)
    ap.add_argument("--out", default="out")
    ap.add_argument("--deadline", type=float,
                    default=env_float("DOGFOOD_CONTROLLER_DEADLINE_S", DEFAULT_DEADLINE_S),
                    help="seconds to wait for both agents' finals (default 150)")
    ap.add_argument("--verdict-grace", type=float,
                    default=env_float("DOGFOOD_VERDICT_GRACE_S", DEFAULT_VERDICT_GRACE_S),
                    help="seconds to let the agents read their verdicts before exiting")
    ap.add_argument("--mesh-wait", type=float,
                    default=env_float("DOGFOOD_MESH_WAIT_S", DEFAULT_MESH_WAIT_S),
                    help="seconds to wait for both agents on the live mesh before "
                         "the mesh PING probe (default 20)")
    args = ap.parse_args()
    os.makedirs(args.out, exist_ok=True)
    task = json.load(open(args.task, encoding="utf-8"))
    return Controller(task, args.bridge, args.port, args.out, args.deadline,
                      args.verdict_grace, args.mesh_wait).run()


if __name__ == "__main__":
    sys.exit(main())
