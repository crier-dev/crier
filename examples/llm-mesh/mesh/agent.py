#!/usr/bin/env python3
"""LLM agent wired to the Crier mesh.

Each agent runs two loops over one mesh connection:

  * responder  — answers incoming /ask REQUESTs from its peer using only its
                 own private clues (a small, fact-bound LLM call)
  * solver     — drives the task with tool calls; the mesh operations ARE the
                 LLM's tools:
                     ask_peer(question)     -> REQUEST over the mesh, returns
                                               the peer's answer
                     submit_final(answer)   -> REQUEST to the controller, which
                                               verifies and replies
                 The solver starts when the controller sends a /start frame.

This is the missing glue demonstrated end-to-end: Crier is the transport,
the mesh client (crier_mesh.py) is the runtime, and this file is the agent.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import time
import urllib.request

from crier_mesh import MeshClient, MeshError

ASK_TOOL = {
    "type": "function",
    "function": {
        "name": "ask_peer",
        "description": "Ask your peer agent a question about the facts it holds. "
                       "Use this whenever you need information you do not have.",
        "parameters": {
            "type": "object",
            "properties": {"question": {"type": "string",
                                        "description": "The question, in plain language."}},
            "required": ["question"],
        },
    },
}

FINAL_TOOL = {
    "type": "function",
    "function": {
        "name": "submit_final",
        "description": "Submit your final answer to the controller. "
                       "Only call this when you are certain and have all the information you need.",
        "parameters": {
            "type": "object",
            "properties": {"answer": {"type": "string",
                                      "description": "The complete answer, formatted exactly as requested."}},
            "required": ["answer"],
        },
    },
}

TOOLS = [ASK_TOOL, FINAL_TOOL]


def llm_chat(base_url: str, api_key: str, model: str, messages: list,
             tools: list | None = None, max_tokens: int = 512,
             temperature: float = 0.3) -> dict:
    body: dict = {
        "model": model, "messages": messages, "max_tokens": max_tokens,
        "temperature": temperature, "stream": False,
    }
    if tools:
        body["tools"] = tools
        body["tool_choice"] = "auto"
    req = urllib.request.Request(
        base_url.rstrip("/") + "/chat/completions",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json",
                 "Authorization": f"Bearer {api_key}"},
    )
    with urllib.request.urlopen(req, timeout=90) as r:
        return json.loads(r.read())


def stamp() -> str:
    return time.strftime("%H:%M:%S")


class LLMAgent:
    def __init__(self, agent_id: str, peer: str, controller: str, task: dict,
                 base_url: str, api_key: str, model: str,
                 max_turns: int = 6, debug: bool = False):
        self.agent_id = agent_id
        self.peer = peer
        self.controller = controller
        self.task = task
        self.base_url = base_url
        self.api_key = api_key
        self.model = model
        self.max_turns = max_turns
        self.debug = debug
        me = task["agents"][agent_id]
        self.clues = me["clues"]
        self.responder_rules = me["responder_rules"]

        self.client: MeshClient | None = None
        self.solver_done = threading.Event()
        self.solver_messages: list[dict] = []
        self.start_received = threading.Event()

    # ------------------------------------------------------------- prompts

    @property
    def solver_system(self) -> str:
        clues = "\n".join(f"- {c}" for c in self.clues)
        return (
            f"You are {self.agent_id}, one of two agents solving a puzzle together. "
            f"The other agent is {self.peer}.\n\n"
            f"TASK: {self.task['public_prompt']}\n\n"
            f"PRIVATE CLUES YOU HOLD (the peer does NOT have these):\n{clues}\n\n"
            "You do not have all the information needed to solve the puzzle alone. "
            "Ask your peer targeted questions with the ask_peer tool until you can "
            "deduce the complete answer. Do not guess: only submit when you are "
            "certain. Your peer will answer only what its own clues cover.\n\n"
            "Tools:\n"
            "- ask_peer(question): asks your peer a question; the answer comes back.\n"
            "- submit_final(answer): submits your final answer to the controller. "
            "The controller will tell you if it is accepted or rejected."
        )

    @property
    def responder_system(self) -> str:
        clues = "\n".join(f"- {c}" for c in self.clues)
        return (
            f"You are {self.agent_id}, part of a two-agent team solving a puzzle.\n\n"
            f"PRIVATE FACTS YOU KNOW:\n{clues}\n\n"
            f"RULES: {self.responder_rules}"
        )

    # ------------------------------------------------------------- entry

    def run(self, ws_url: str) -> int:
        self.client = MeshClient(self.agent_id, ws_url, debug=self.debug)
        self.client.respond(self.handle_request)
        self.client.connect()
        print(f"[{stamp()}] {self.agent_id}: registered on mesh as '{self.agent_id}'")

        # Wait for the controller to start the task, then let the solver drive.
        if not self.start_received.wait(timeout=180):
            print(f"[{stamp()}] {self.agent_id}: TIMEOUT waiting for /start")
            self.client.close()
            return 1
        self.solver_done.wait(timeout=240)
        self.client.close()
        print(f"[{stamp()}] {self.agent_id}: done")
        return 0 if self.solver_done.is_set() else 1

    # ------------------------------------------------------------- responder

    def handle_request(self, frame: dict) -> dict:
        method = frame.get("method", "")
        path = frame.get("path", "")
        body = frame.get("body") or {}
        if isinstance(body, str):
            try:
                body = json.loads(body)
            except json.JSONDecodeError:
                body = {}

        if method == "SOLVE" and path == "/start":
            if not self.start_received.is_set():
                self.start_received.set()
                threading.Thread(target=self.solver_loop, daemon=True,
                                 name=f"solver-{self.agent_id}").start()
            return {"status": "started"}

        if method == "QUERY" and path == "/ask":
            question = body.get("question", "")
            print(f"[{stamp()}] {self.agent_id}: <- question from {frame.get('source', {}).get('agent_id')}: {question}")
            answer = self.answer_question(question)
            print(f"[{stamp()}] {self.agent_id}: -> answer: {answer}")
            return {"answer": answer}

        return {"status": "ignored"}

    def answer_question(self, question: str) -> str:
        messages = [
            {"role": "system", "content": self.responder_system},
            {"role": "user", "content": question},
        ]
        try:
            resp = llm_chat(self.base_url, self.api_key, self.model, messages,
                            max_tokens=256, temperature=0.2)
            content = (resp["choices"][0]["message"].get("content") or "").strip()
            return content or "I don't know."
        except Exception as exc:
            print(f"[{stamp()}] {self.agent_id}: responder LLM error: {exc}")
            return "I don't know."

    # ------------------------------------------------------------- solver

    def solver_loop(self) -> None:
        self.solver_messages = [{"role": "system", "content": self.solver_system}]
        print(f"[{stamp()}] {self.agent_id}: task started, solver engaged")
        for turn in range(1, self.max_turns + 1):
            try:
                resp = llm_chat(self.base_url, self.api_key, self.model,
                                self.solver_messages, tools=TOOLS,
                                max_tokens=512, temperature=0.3)
            except Exception as exc:
                print(f"[{stamp()}] {self.agent_id}: solver LLM error: {exc}")
                break
            msg = resp["choices"][0]["message"]
            self.solver_messages.append(msg)

            tool_calls = msg.get("tool_calls") or []
            if not tool_calls:
                # No tool call: treat any prose as a last-resort final answer.
                content = (msg.get("content") or "").strip()
                if content:
                    print(f"[{stamp()}] {self.agent_id}: no tool call, submitting prose as final")
                    self.submit_final(content)
                break

            for tc in tool_calls:
                name = tc["function"]["name"]
                try:
                    args = json.loads(tc["function"]["arguments"] or "{}")
                except json.JSONDecodeError:
                    args = {}
                if name == "ask_peer":
                    result = self.ask_peer(args.get("question", ""))
                elif name == "submit_final":
                    result = self.submit_final(args.get("answer", ""))
                else:
                    result = f"Unknown tool: {name}"
                self.solver_messages.append({
                    "role": "tool", "tool_call_id": tc["id"], "content": result,
                })
                if self.solver_done.is_set():
                    return
        print(f"[{stamp()}] {self.agent_id}: solver budget exhausted ({self.max_turns} turns)")

    def ask_peer(self, question: str) -> str:
        print(f"[{stamp()}] {self.agent_id}: -> ask {self.peer}: {question}")
        # Retry a few times in case the peer is still registering.
        for attempt in range(4):
            try:
                resp = self.client.request(
                    self.peer, "QUERY", "/ask",
                    {"question": question}, timeout_ms=25000)
                raw = resp.get("body")
                if isinstance(raw, str):
                    try:
                        raw = json.loads(raw)
                    except json.JSONDecodeError:
                        pass
                answer = raw.get("answer") if isinstance(raw, dict) else str(raw)
                print(f"[{stamp()}] {self.agent_id}: <- {self.peer}: {answer}")
                return f"Peer {self.peer} answered: {answer}"
            except (MeshError, TimeoutError) as exc:
                if attempt < 3:
                    time.sleep(1.0)
                    continue
                print(f"[{stamp()}] {self.agent_id}: peer unreachable: {exc}")
                return f"Could not reach peer {self.peer}: {exc}"
        return "Peer unreachable."

    def submit_final(self, answer: str) -> str:
        print(f"[{stamp()}] {self.agent_id}: -> submit final to {self.controller}: {answer}")
        try:
            resp = self.client.request(
                self.controller, "SOLVE", "/final",
                {"answer": answer, "agent": self.agent_id}, timeout_ms=25000)
            raw = resp.get("body")
            if isinstance(raw, str):
                try:
                    raw = json.loads(raw)
                except json.JSONDecodeError:
                    pass
            body = raw if isinstance(raw, dict) else {"status": "UNKNOWN", "message": str(raw)}
            status = body.get("status", "UNKNOWN")
            print(f"[{stamp()}] {self.agent_id}: <- controller: {status} — {body.get('message', '')}")
            if status == "ACCEPTED":
                self.solver_done.set()
            return f"Controller: {status} — {body.get('message', '')}"
        except (MeshError, TimeoutError) as exc:
            return f"Controller unreachable: {exc}"


def main() -> int:
    ap = argparse.ArgumentParser(description="Crier-mesh LLM agent")
    ap.add_argument("--id", required=True, help="agent id (must match task JSON)")
    ap.add_argument("--peer", required=True, help="peer agent id")
    ap.add_argument("--controller", default="controller", help="controller agent id")
    ap.add_argument("--task", required=True, help="task JSON path")
    ap.add_argument("--server", default="ws://127.0.0.1:18777", help="mesh base url")
    ap.add_argument("--model", default="deepseek-v4-flash")
    ap.add_argument("--base-url", default="https://api.deepseek.com/v1")
    ap.add_argument("--max-turns", type=int, default=6)
    ap.add_argument("--debug", action="store_true")
    args = ap.parse_args()

    api_key = os.environ.get("DEEPSEEK_API_KEY")
    if not api_key:
        print("DEEPSEEK_API_KEY not set", file=sys.stderr)
        return 1
    task = json.load(open(args.task, encoding="utf-8"))
    if args.id not in task["agents"]:
        print(f"task JSON has no agent '{args.id}'", file=sys.stderr)
        return 1

    agent = LLMAgent(args.id, args.peer, args.controller, task,
                     args.base_url, api_key, args.model,
                     max_turns=args.max_turns, debug=args.debug)
    return agent.run(args.server)


if __name__ == "__main__":
    sys.exit(main())
