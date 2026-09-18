#!/usr/bin/env python3
"""Minimal MCP (stdio) client — the generic harness↔bridge interface.

This is deliberately tiny: a real harness (Claude Code, Hermes, OpenCode)
speaks this same JSON-RPC protocol to the crier-mcp bridge. Everything the
harness knows about Crier is a tool name and its JSON arguments — no
WebSocket frames, no signing, no leases, no correlation ids. Those live in
the bridge.

Every call is BOUNDED. A bridge that stops answering — wedged on a mesh
connect, blocked on a write, crashed without a clean EOF — raises MCPError
naming the tool and the budget instead of blocking the caller forever. That
matters for the demo: the DF-CRIER-214 failure was a harness-level run that
produced no output because a process sat on a bridge read with no timeout.
Replies are read by a dedicated thread, so a timeout never leaves a
half-consumed pipe behind.

`ensure_identity()` lives here too, because "does my identity exist on the
server" is a BRIDGE question: the bridge registers `CRIER_AGENT_ID` itself at
startup (crier-mcp `ensureBridgeIdentity`), so a harness's own
`register_agent` call answers "agent already registered" on the happy path.
Treating that as a fatal error is the DF-CRIER-214 harness-level death.

Per-request budget: ``CRIER_MCP_TIMEOUT_S`` (default 60s).
"""

from __future__ import annotations

import json
import os
import queue
import subprocess
import threading

DEFAULT_TIMEOUT_S = 60.0

# Sentinel queued by the reader thread when the bridge's stdout closes.
EOF = None


class MCPError(Exception):
    pass


def default_timeout() -> float:
    raw = os.environ.get("CRIER_MCP_TIMEOUT_S", "").strip()
    if not raw:
        return DEFAULT_TIMEOUT_S
    try:
        value = float(raw)
    except ValueError:
        return DEFAULT_TIMEOUT_S
    return value if value > 0 else DEFAULT_TIMEOUT_S


class MCPClient:
    def __init__(self, bridge_cmd: str, env_overrides: dict, name: str,
                 log_dir: str | None = None, timeout_s: float | None = None):
        env = dict(os.environ)
        env.update(env_overrides)
        stderr = subprocess.DEVNULL
        if log_dir:
            os.makedirs(log_dir, exist_ok=True)
            stderr = open(os.path.join(log_dir, f"{name}-bridge.log"), "w")
        self.name = name
        self.timeout_s = float(timeout_s) if timeout_s else default_timeout()
        self.proc = subprocess.Popen(
            [bridge_cmd], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=stderr, text=True, bufsize=1, env=env,
        )
        self._next_id = 0
        # Replies are drained by one reader thread; _request() takes a bounded
        # wait on the queue. Without this a wedged bridge blocks the caller
        # forever (the pre-fix behaviour) instead of failing loudly.
        self._replies: "queue.Queue[str | None]" = queue.Queue()
        self._reader = threading.Thread(target=self._read_loop, name=f"mcp-reader-{name}",
                                        daemon=True)
        self._reader.start()

    # -------------------------------------------------------------- plumbing

    def _read_loop(self) -> None:
        try:
            while True:
                line = self.proc.stdout.readline() if self.proc.stdout else ""
                if not line:
                    self._replies.put(EOF)
                    return
                self._replies.put(line)
        except Exception:
            self._replies.put(EOF)

    def _write(self, line: str, call: str) -> None:
        if self.proc.stdin is None:
            raise MCPError(f"bridge '{self.name}': stdin is closed (writing {call})")
        try:
            self.proc.stdin.write(line + "\n")
            self.proc.stdin.flush()
        except (BrokenPipeError, ValueError, OSError) as exc:
            raise MCPError(
                f"bridge '{self.name}': write of {call} failed "
                f"(rc={self.proc.poll()}): {exc}") from exc

    def _read_reply(self, call: str) -> dict:
        """Read the reply to ``call``, bounded by the per-request budget.

        ``call`` NAMES the call being waited on — for a tool call that is
        ``tools/call <tool>``, so a wedged bridge reports the tool the caller
        was blocked on instead of only the JSON-RPC method it rides on. That
        distinction is the whole diagnostic value of the error (DF-CRIER-214:
        an unnamed 'tools/call' left the operator guessing which of the four
        harness tools had hung).
        """
        try:
            line = self._replies.get(timeout=self.timeout_s)
        except queue.Empty:
            raise MCPError(
                f"bridge '{self.name}': no reply to {call} within "
                f"{self.timeout_s:g}s — the bridge is hung or blocked "
                f"(set CRIER_MCP_TIMEOUT_S to change the budget)"
            ) from None
        if line is EOF:
            raise MCPError(
                f"bridge '{self.name}': exited (rc={self.proc.poll()}) before answering {call}")
        try:
            return json.loads(line)
        except json.JSONDecodeError as exc:
            raise MCPError(
                f"bridge '{self.name}': non-JSON reply to {call}: {line[:200]!r} ({exc})") from exc

    def _request(self, method: str, params: dict | None,
                 call: str | None = None) -> dict:
        where = call or method
        self._next_id += 1
        msg = {"jsonrpc": "2.0", "id": self._next_id, "method": method}
        if params is not None:
            msg["params"] = params
        self._write(json.dumps(msg), where)
        resp = self._read_reply(where)
        if "error" in resp:
            raise MCPError(f"{where}: {resp['error'].get('message', 'mcp error')}")
        return resp.get("result") or {}

    def _notify(self, method: str) -> None:
        """Fire-and-forget notification (no id, no response)."""
        self._write(json.dumps({"jsonrpc": "2.0", "method": method}), method)

    # ---------------------------------------------------------------- public

    def initialize(self) -> None:
        self._request("initialize", {"protocolVersion": "2024-11-05",
                                     "capabilities": {}})
        self._notify("notifications/initialized")

    def tools_list(self) -> list[str]:
        result = self._request("tools/list", {}, call="tools/list")
        return [t["name"] for t in result.get("tools", [])]

    def call(self, name: str, args: dict | None = None) -> tuple[str, bool]:
        """Returns (text, is_error) — the bridge's tool result.

        Every failure names the TOOL (``tools/call get_messages``), so a
        timeout tells the caller which of its calls the bridge stopped
        answering, not merely that some JSON-RPC method was in flight.
        """
        result = self._request("tools/call", {"name": name,
                                              "arguments": args or {}},
                               call=f"tools/call {name}")
        content = result.get("content") or []
        text = "".join(c.get("text", "") for c in content)
        return text, bool(result.get("isError"))

    def close(self) -> None:
        try:
            if self.proc.stdin:
                self.proc.stdin.close()
        except Exception:
            pass
        try:
            self.proc.wait(timeout=3)
        except Exception:
            self.proc.kill()


# ------------------------------------------------------------------ identity

ALREADY_REGISTERED = "already registered"


def ensure_identity(client: MCPClient, agent_id: str, capabilities: list[str] | None = None,
                    public_key: str | None = None) -> str:
    """Ensure ``agent_id`` exists on the server; returns a note for the log.

    The crier-mcp bridge registers `CRIER_AGENT_ID` on the server before it
    serves a single request (`ensureBridgeIdentity`, crier-mcp/main.go), so the
    explicit `register_agent` call this demo makes is answered
    ``agent already registered`` on the happy path — that is SUCCESS, the
    identity the bridge sends and receives as is exactly the one asked for.
    Treating it as fatal is what killed the dogfood run at the harness level.

    So: register, accept success, accept "already registered" after confirming
    the identity is really there with `get_agent`, and raise MCPError naming
    the cause for anything else. Nothing is fabricated — a missing identity is
    still a loud failure, it is only the *duplicate* that is no longer one.
    """
    payload: dict = {"id": agent_id, "capabilities": list(capabilities or [])}
    if public_key:
        payload["public_key"] = public_key
    try:
        text, is_err = client.call("register_agent", payload)
    except MCPError as exc:  # RPC-level error, not an in-band tool error
        text, is_err = str(exc), True

    if not is_err:
        return f"registered on crier as '{agent_id}'"
    if ALREADY_REGISTERED not in text.lower():
        raise MCPError(f"register_agent({agent_id}) failed: {text}")

    text, is_err = client.call("get_agent", {"id": agent_id})
    if is_err:
        raise MCPError(
            f"register_agent({agent_id}) answered 'already registered' but "
            f"get_agent({agent_id}) could not confirm the identity: {text}")
    try:
        row = json.loads(text)
    except json.JSONDecodeError:
        raise MCPError(
            f"register_agent({agent_id}) answered 'already registered' but "
            f"get_agent({agent_id}) returned non-JSON: {text[:200]!r}") from None
    if row.get("id") != agent_id:
        raise MCPError(
            f"register_agent({agent_id}) answered 'already registered' but "
            f"get_agent returned id={row.get('id')!r}")
    return (f"identity '{agent_id}' already registered on the server "
            f"(the bridge registered its own id at startup); confirmed via get_agent")
