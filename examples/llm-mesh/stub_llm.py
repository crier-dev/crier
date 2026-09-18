#!/usr/bin/env python3
"""Local, deterministic OpenAI-compatible model endpoint for the llm-mesh
self-test (selftest.py). NOT the demo's LLM — the demo uses DeepSeek.

Why it exists: the demo's termination/output contract must be provable
without a paid API key, so the self-test points ``--base-url`` at this stub
while everything else stays real (crier server binary, crier-mcp bridge over
stdio, HTTP inbox lane, WebSocket mesh lane, harnesses, controller).

It is a MODEL double, not a verdict double:

* it never emits a verdict — the controller computes that from what the
  agents actually put on the wire;
* it does not carry a canned answer — it solves the puzzle from the CLUES it
  is handed (its own, plus the peer's answers that arrived as tool results),
  with a small exhaustive constraint solver. If the harness stopped passing
  the private clues, or the peer's answer never crossed the bridge, the stub
  cannot produce a correct assignment and the controller REJECTS.

Modes (``--mode`` / ``STUB_MODE``):

  solved   default; asks the peer, then submits the deduced assignment
  wrong    asks the peer, then submits a deliberately wrong assignment
           (proves the controller's verification is not a canned success path)
  stall    never submits; keeps asking (proves the bounded-failure contract)

Run standalone:

    python3 stub_llm.py --mode solved --port 8099
"""

from __future__ import annotations

import argparse
import itertools
import json
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

VEGETABLES = ["tomato", "lettuce", "carrot", "pepper"]
PLOTS = ["A", "B", "C", "D"]

# --------------------------------------------------------------- clue parsing


def parse_clues(clues: list[str]) -> tuple[dict, set, list]:
    """Parse the task-garden clue sentence forms into constraints."""
    fixed: dict[str, str] = {}
    banned: set[tuple[str, str]] = set()
    pairs: list[tuple[str, str, str, str]] = []
    for clue in clues:
        text = clue.strip().rstrip(".")
        m = re.match(r"Plot ([ABCD]) is not ([a-z]+)$", text)
        if m:
            banned.add((m.group(1), m.group(2).lower()))
            continue
        m = re.match(r"Plot ([ABCD]) is (?:the )?([a-z]+)$", text)
        if m:
            fixed[m.group(1)] = m.group(2).lower()
            continue
        m = re.match(r"The ([a-z]+) is not in plot ([ABCD])$", text)
        if m:
            banned.add((m.group(2), m.group(1).lower()))
            continue
        m = re.match(r"The ([a-z]+) is in plot ([ABCD])$", text)
        if m:
            fixed[m.group(2)] = m.group(1).lower()
            continue
        m = re.match(r"([A-Za-z]+) and ([A-Za-z]+) are in plots ([ABCD]) and ([ABCD]),"
                     r" in some order$", text)
        if m:
            pairs.append((m.group(1).lower(), m.group(2).lower(),
                          m.group(3), m.group(4)))
            continue
    return fixed, banned, pairs


def solve_from_clues(clues: list[str]) -> dict | None:
    """Return the unique A/B/C/D assignment consistent with the clues, if any."""
    fixed, banned, pairs = parse_clues(clues)
    solutions = []
    for perm in itertools.permutations(VEGETABLES):
        assignment = dict(zip(PLOTS, perm))
        if any(assignment[p] != v for p, v in fixed.items()):
            continue
        if any(assignment[p] == v for p, v in banned):
            continue
        if any({assignment[p1], assignment[p2]} != {v1, v2} for v1, v2, p1, p2 in pairs):
            continue
        solutions.append(assignment)
    if len(solutions) != 1:
        return None
    return solutions[0]


def format_assignment(assignment: dict) -> str:
    return ", ".join(f"{p}={assignment[p]}" for p in PLOTS)


def wrong_assignment(assignment: dict) -> dict:
    """Deterministically wrong: swap two plots. Never equal to ground truth."""
    out = dict(assignment)
    out["A"], out["B"] = assignment.get("B"), assignment.get("A")
    return out


def clues_from_text(text: str) -> list[str]:
    """Pull '- <clue sentence>' bullets out of arbitrary message text.

    Peer answers arrive as a JSON string inside the tool result (and as
    ``\\n``-escaped newlines on the wire), so normalize those back first and
    drop the closing JSON punctuation that rides on the last bullet.
    """
    if not text:
        return []
    normalized = text.replace("\\n", "\n")
    out = []
    for line in normalized.splitlines():
        line = line.strip()
        if line.startswith("- "):
            clue = line[2:].split('"')[0].strip()   # cut the JSON envelope
            if clue:
                out.append(clue)
    return out


def detect_agent(system: str) -> str:
    m = re.search(r"You are (agent-[ab])", system or "")
    return m.group(1) if m else "unknown"


# --------------------------------------------------------------- HTTP server


class StubState:
    def __init__(self, mode: str = "solved"):
        self.mode = mode
        self.requests: list[dict] = []
        self.lock = threading.Lock()

    def record(self, entry: dict) -> None:
        with self.lock:
            self.requests.append(entry)

    def solver_calls(self, agent: str) -> int:
        with self.lock:
            return sum(1 for r in self.requests
                       if r["agent"] == agent and r["kind"] == "solver")


def build_reply(message: dict) -> dict:
    return {"choices": [{"index": 0, "message": message, "finish_reason": "tool_calls"
                         if message.get("tool_calls") else "stop"}],
            "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}}


def tool_call(name: str, args: dict) -> dict:
    return {"role": "assistant", "content": None, "tool_calls": [{
        "id": f"call_{name}_{abs(hash(json.dumps(args, sort_keys=True))) % 10**8}",
        "type": "function",
        "function": {"name": name, "arguments": json.dumps(args)},
    }]}


def responder_reply(agent: str, system: str, question: str) -> dict:
    """Fact-bound answer: echo the private facts the responder was handed."""
    facts = clues_from_text(system)
    if facts:
        body = "Facts I hold:\n" + "\n".join(f"- {f.rstrip('.')}." for f in facts)
    else:
        body = "I don't know."
    return build_reply({"role": "assistant", "content": body})


def solver_reply(agent: str, system: str, messages: list[dict],
                 state: StubState) -> dict:
    own = clues_from_text(system)
    peer_facts: list[str] = []
    for m in messages:
        if m.get("role") == "tool":
            peer_facts.extend(clues_from_text(str(m.get("content", ""))))
    assignment = solve_from_clues(own + peer_facts)

    if state.mode == "stall" or assignment is None:
        return build_reply(tool_call("ask_peer", {
            "question": "Which plots do you know the vegetable for? "
                        "Answer with every fact you hold."}))
    if state.mode == "wrong":
        return build_reply(tool_call("submit_final",
                                     {"answer": format_assignment(wrong_assignment(assignment))}))
    return build_reply(tool_call("submit_final",
                                 {"answer": format_assignment(assignment)}))


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):  # keep the self-test output readable
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        if not self.path.rstrip("/").endswith("/chat/completions"):
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        try:
            body = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            body = {}
        messages = body.get("messages") or []
        system = next((m.get("content", "") for m in messages
                       if m.get("role") == "system"), "")
        agent = detect_agent(system)
        tools = body.get("tools") or []
        state: StubState = self.server.state  # type: ignore[attr-defined]

        if tools:
            state.record({"agent": agent, "kind": "solver"})
            reply = solver_reply(agent, system, messages, state)
        else:
            state.record({"agent": agent, "kind": "responder"})
            question = next((m.get("content", "") for m in reversed(messages)
                             if m.get("role") == "user"), "")
            reply = responder_reply(agent, system, question)

        payload = json.dumps(reply).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def start_server(mode: str = "solved", port: int = 0) -> tuple[ThreadingHTTPServer, StubState, str]:
    """Start the stub in a background thread; returns (server, state, base_url)."""
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    state = StubState(mode)
    server.state = state  # type: ignore[attr-defined]
    thread = threading.Thread(target=server.serve_forever, daemon=True,
                              name="stub-llm")
    thread.start()
    return server, state, f"http://127.0.0.1:{server.server_address[1]}/v1"


def main() -> int:
    ap = argparse.ArgumentParser(description="local stub model for the llm-mesh demo")
    ap.add_argument("--mode", default="solved", choices=["solved", "wrong", "stall"])
    ap.add_argument("--port", type=int, default=8099)
    args = ap.parse_args()
    server, _, base_url = start_server(args.mode, args.port)
    print(f"stub model ({args.mode}): {base_url}  (ctrl-c to stop)")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
