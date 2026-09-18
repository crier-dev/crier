#!/usr/bin/env python3
"""LLM harness that collaborates with a peer through Crier — using ONLY
crier-mcp bridge tools. It never touches the wire: no WebSockets, no HTTP to
the registry, no signing, no leases. Everything rides the MCP tools:

    get_messages()                 -> bridge get_messages (own inbox, auto-acked)
    ask_peer(question)             -> bridge ask_agent (blocking, correlated)
    answer_peer(cid, answer)       -> bridge send_message (reply_to=cid)
    submit_final(answer)           -> bridge send_message to the controller

The puzzle: each agent holds private clues; solving requires asking the peer
and combining answers. The final answer is verified by the controller, which
also communicates through the same bridge interface.

Termination contract (DF-CRIER-214): every exit from ``run()`` is BOUNDED and
NAMED —
  * 0 — a verdict arrived and it was ACCEPTED;
  * 1 — a fatal error (identity, LLM transport), printed with its cause;
  * 2 — the turn budget ran out before a final answer was accepted, printed
        with the tools it did call, so the run can never end silently without
        an output a caller can act on.
"""

from __future__ import annotations

import argparse
import json
import os
import secrets
import sys
import time
import urllib.request

try:  # logs must survive a kill: never sit in a block buffer
    sys.stdout.reconfigure(line_buffering=True)
    sys.stderr.reconfigure(line_buffering=True)
except Exception:  # pragma: no cover - non-reconfigurable stream
    pass

from mcp_client import MCPClient, MCPError, ensure_identity

DEFAULT_ASK_TIMEOUT_S = 60.0

EXIT_SOLVED = 0
EXIT_FATAL = 1
EXIT_BUDGET_EXHAUSTED = 2

LLM_TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "get_messages",
            "description": "Check your inbox: questions from your peer (kind=question, with crier_correlation_id), verdicts from the controller (kind=verdict).",
            "parameters": {"type": "object", "properties": {}, "required": []},
        },
    },
    {
        "type": "function",
        "function": {
            "name": "ask_peer",
            "description": "Ask your peer a targeted question about the facts it holds. Blocks until the peer answers.",
            "parameters": {
                "type": "object",
                "properties": {"question": {"type": "string", "description": "The question, in plain language."}},
                "required": ["question"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "answer_peer",
            "description": "Answer a question you received from your peer. Pass the question's id exactly as it appeared in the message (field ask_id if present, otherwise crier_correlation_id), and answer only from your own clues.",
            "parameters": {
                "type": "object",
                "properties": {
                    "correlation_id": {"type": "string", "description": "The crier_correlation_id from the question message."},
                    "answer": {"type": "string", "description": "Your answer, concise, from your clues only."},
                },
                "required": ["correlation_id", "answer"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "submit_final",
            "description": "Submit your final solution to the controller. Only when you are certain. Format: A=<vegetable>, B=<vegetable>, C=<vegetable>, D=<vegetable>",
            "parameters": {
                "type": "object",
                "properties": {"answer": {"type": "string", "description": "The complete assignment."}},
                "required": ["answer"],
            },
        },
    },
]


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


class Harness:
    def __init__(self, agent_id: str, peer: str, controller: str, task: dict,
                 bridge: str, port: int, out_dir: str,
                 base_url: str, api_key: str, model: str, max_turns: int,
                 ask_timeout_s: float = DEFAULT_ASK_TIMEOUT_S):
        self.agent_id = agent_id
        self.peer = peer
        self.controller = controller
        self.task = task
        self.model = model
        self.max_turns = max_turns
        self.ask_timeout_s = ask_timeout_s
        self.tool_trace: list[str] = []
        me = task["agents"][agent_id]
        self.clues = me["clues"]
        self.responder_rules = me["responder_rules"]
        self.bridge = MCPClient(
            bridge,
            {
                "CRIER_AGENT_ID": agent_id,
                "CRIER_HTTP_URL": f"http://127.0.0.1:{port}",
                "CRIER_MESH_URL": f"ws://127.0.0.1:{port}/mesh/connect/{agent_id}",
            },
            agent_id, out_dir,
        )
        self.base_url = base_url
        self.api_key = api_key
        self.messages: list[dict] = []

    # ------------------------------------------------------------- prompts

    @property
    def system_prompt(self) -> str:
        clues = "\n".join(f"- {c}" for c in self.clues)
        return (
            f"You are {self.agent_id}, one of two agents solving a puzzle together. "
            f"Your peer is {self.peer}. The controller is {self.controller}.\n\n"
            f"TASK: {self.task['public_prompt']}\n\n"
            f"PRIVATE CLUES YOU HOLD (the peer does NOT have these):\n{clues}\n\n"
            "You do not have all the information needed to solve the puzzle alone. "
            "Work like this, every turn:\n"
            "1. Call get_messages() first. If there are questions "
            "(kind=question), answer each with answer_peer() using its "
            "id (ask_id if present, else crier_correlation_id) — answer only "
            "from your clues, never invent. "
            "If there is a verdict (kind=verdict), read it.\n"
            "2. If you still lack information, call ask_peer() with a targeted "
            "question about what you need.\n"
            "3. When you can deduce the complete assignment with certainty, "
            "call submit_final().\n\n"
            "Never guess. Combine your clues with the peer's answers. Your "
            "answers to the peer must come from your clues only."
        )

    # ------------------------------------------------------------- tools

    def tool_get_messages(self) -> str:
        text, is_err = self.bridge.call("get_messages", {"max": 10})
        if is_err:
            return f"error: {text}"
        return text

    def tool_ask_peer(self, question: str, timeout_s: float | None = None) -> str:
        """Ask the peer and wait for its reply WITHOUT blocking on the bridge's
        ask_agent: the harness polls its own inbox while waiting and answers
        any incoming questions on the spot. (Blocking ask_agent from both
        sides deadlocks: each waits for a reply the other never sends because
        the other is also blocking — see README 'protocol notes'.)"""
        if timeout_s is None:
            timeout_s = self.ask_timeout_s
        ask_id = secrets.token_hex(12)
        print(f"[{stamp()}] {self.agent_id}: -> ask {self.peer}: {question}")
        text, is_err = self.bridge.call("send_message", {
            "agent_id": self.peer,
            "payload": {"kind": "question", "text": question, "ask_id": ask_id},
        })
        if is_err:
            print(f"[{stamp()}] {self.agent_id}: <- send failed: {text}")
            return f"send failed: {text}"

        deadline = time.time() + timeout_s
        while time.time() < deadline:
            text, is_err = self.bridge.call("get_messages", {"max": 10})
            if not is_err:
                try:
                    msgs = json.loads(text).get("messages", [])
                except json.JSONDecodeError:
                    msgs = []
                for m in msgs:
                    p = m.get("payload", {})
                    if p.get("crier_reply_to") == ask_id:
                        print(f"[{stamp()}] {self.agent_id}: <- {self.peer} answered: {json.dumps(p)}")
                        return f"Peer answer: {json.dumps(p)}"
                    if p.get("kind") == "question":
                        # Be responsive while waiting: answer the peer's
                        # question immediately with a fact-bound LLM call.
                        qid = p.get("ask_id") or p.get("crier_correlation_id", "")
                        answer = self.answer_question(p.get("text", ""))
                        print(f"[{stamp()}] {self.agent_id}: <- question from {self.peer}: {p.get('text')} -> answering: {answer}")
                        self.bridge.call("send_message", {
                            "agent_id": self.peer,
                            "payload": {"kind": "answer", "text": answer},
                            "reply_to": qid,
                        })
            time.sleep(1.5)
        print(f"[{stamp()}] {self.agent_id}: <- ask timeout ({timeout_s}s)")
        return f"ask timeout: no reply from {self.peer} within {timeout_s}s"

    def answer_question(self, question: str) -> str:
        """Fact-bound responder call: answers from the private clues only."""
        clues = "\n".join(f"- {c}" for c in self.clues)
        messages = [
            {"role": "system", "content": (
                f"You are {self.agent_id}, part of a two-agent team solving a puzzle.\n\n"
                f"PRIVATE FACTS YOU KNOW:\n{clues}\n\n"
                f"RULES: {self.responder_rules}")},
            {"role": "user", "content": question},
        ]
        try:
            resp = llm_chat(self.base_url, self.api_key, self.model, messages,
                            max_tokens=256, temperature=0.2)
            return (resp["choices"][0]["message"].get("content") or "").strip() or "I don't know."
        except Exception as exc:
            print(f"[{stamp()}] {self.agent_id}: responder LLM error: {exc}")
            return "I don't know."

    def tool_answer_peer(self, correlation_id: str, answer: str) -> str:
        print(f"[{stamp()}] {self.agent_id}: -> answer to {self.peer} (cid={correlation_id[:8]}...): {answer}")
        text, is_err = self.bridge.call("send_message", {
            "agent_id": self.peer,
            "payload": {"kind": "answer", "text": answer},
            "reply_to": correlation_id,
        })
        if is_err:
            return f"error sending answer: {text}"
        return "Answer sent."

    def tool_submit_final(self, answer: str) -> str:
        print(f"[{stamp()}] {self.agent_id}: -> SUBMIT final to {self.controller}: {answer}")
        text, is_err = self.bridge.call("send_message", {
            "agent_id": self.controller,
            "payload": {"kind": "final", "answer": answer, "agent": self.agent_id},
        })
        if is_err:
            return f"error submitting: {text}"
        return "Final submitted. Now poll get_messages() until the controller's verdict arrives."

    # ------------------------------------------------------------- main loop

    def run(self) -> int:
        self.bridge.initialize()
        tools = self.bridge.tools_list()
        print(f"[{stamp()}] {self.agent_id}: bridge tools available: {', '.join(tools)}")

        # Register our identity on the shared server (key is throwaway:
        # the demo server runs with per-agent signing disabled). The bridge
        # has usually registered this same id at its own startup, so
        # "already registered" is success, not a fatal error (DF-CRIER-214).
        key = secrets.token_hex(32)
        try:
            note = ensure_identity(self.bridge, self.agent_id, ["llm-harness"], key)
        except MCPError as exc:
            print(f"[{stamp()}] {self.agent_id}: FATAL identity: {exc}", file=sys.stderr)
            self.bridge.close()
            return EXIT_FATAL
        print(f"[{stamp()}] {self.agent_id}: {note}")

        self.messages = [{"role": "system", "content": self.system_prompt}]
        accepted = False
        reason = f"turn budget exhausted ({self.max_turns} turns)"
        for turn in range(1, self.max_turns + 1):
            try:
                resp = llm_chat(self.base_url, self.api_key, self.model,
                                self.messages, tools=LLM_TOOLS,
                                max_tokens=600, temperature=0.3)
            except Exception as exc:
                print(f"[{stamp()}] {self.agent_id}: FATAL LLM transport on turn "
                      f"{turn}: {exc}", file=sys.stderr)
                self.bridge.close()
                return EXIT_FATAL
            msg = resp["choices"][0]["message"]
            self.messages.append(msg)

            tool_calls = msg.get("tool_calls") or []
            if not tool_calls:
                print(f"[{stamp()}] {self.agent_id}: no tool call in turn {turn} — ending")
                reason = f"the model stopped calling tools at turn {turn}"
                break
            for tc in tool_calls:
                name = tc["function"]["name"]
                try:
                    args = json.loads(tc["function"]["arguments"] or "{}")
                except json.JSONDecodeError:
                    args = {}
                if name == "get_messages":
                    result = self.tool_get_messages()
                elif name == "ask_peer":
                    result = self.tool_ask_peer(args.get("question", ""))
                elif name == "answer_peer":
                    result = self.tool_answer_peer(args.get("correlation_id", ""),
                                                   args.get("answer", ""))
                elif name == "submit_final":
                    result = self.tool_submit_final(args.get("answer", ""))
                else:
                    result = f"unknown tool {name}"
                self.tool_trace.append(name)
                self.messages.append({"role": "tool", "tool_call_id": tc["id"],
                                      "content": result})
                if name == "submit_final":
                    accepted = self.wait_for_verdict()
                    if accepted:
                        print(f"[{stamp()}] {self.agent_id}: SOLVED — verdict ACCEPTED")
                        self.bridge.close()
                        return EXIT_SOLVED
                    print(f"[{stamp()}] {self.agent_id}: verdict REJECTED — continuing")
            if accepted:
                break

        print(f"[{stamp()}] {self.agent_id}: FAILED — {reason} without an accepted "
              f"final answer; tools called: [{', '.join(self.tool_trace) or 'none'}]",
              file=sys.stderr)
        self.bridge.close()
        return EXIT_BUDGET_EXHAUSTED

    def wait_for_verdict(self, polls: int = 12) -> bool:
        for _ in range(polls):
            text, is_err = self.bridge.call("get_messages", {"max": 10})
            if not is_err:
                try:
                    msgs = json.loads(text).get("messages", [])
                except json.JSONDecodeError:
                    msgs = []
                for m in msgs:
                    p = m.get("payload", {})
                    if p.get("kind") == "verdict":
                        print(f"[{stamp()}] {self.agent_id}: <- verdict: {json.dumps(p)}")
                        return bool(p.get("accepted"))
            time.sleep(2)
        return False


def main() -> int:
    ap = argparse.ArgumentParser(description="Crier MCP-bridge LLM harness")
    ap.add_argument("--id", required=True)
    ap.add_argument("--peer", required=True)
    ap.add_argument("--controller", default="controller")
    ap.add_argument("--task", required=True)
    ap.add_argument("--bridge", required=True, help="path to crier-mcp binary")
    ap.add_argument("--port", type=int, default=18777)
    ap.add_argument("--out", default="out")
    ap.add_argument("--model", default=os.environ.get("DOGFOOD_MODEL", "deepseek-v4-flash"))
    ap.add_argument("--base-url", default=os.environ.get("DOGFOOD_BASE_URL", "https://api.deepseek.com/v1"))
    ap.add_argument("--max-turns", type=int, default=8)
    ap.add_argument("--ask-timeout", type=float, default=DEFAULT_ASK_TIMEOUT_S,
                    help="seconds to wait for a peer reply before giving up on the ask")
    args = ap.parse_args()

    api_key = os.environ.get("DEEPSEEK_API_KEY")
    if not api_key:
        print("DEEPSEEK_API_KEY not set", file=sys.stderr)
        return EXIT_FATAL
    task = json.load(open(args.task, encoding="utf-8"))
    if args.id not in task["agents"]:
        print(f"task JSON has no agent '{args.id}'", file=sys.stderr)
        return EXIT_FATAL
    h = Harness(args.id, args.peer, args.controller, task, args.bridge,
                args.port, args.out, args.base_url, api_key, args.model,
                args.max_turns, args.ask_timeout)
    return h.run()


if __name__ == "__main__":
    sys.exit(main())
