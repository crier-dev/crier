#!/usr/bin/env python3
"""Crier mesh client — the glue between the wire protocol and an application.

Implements the full client side of docs/mesh-protocol.md:

  * WebSocket connect to GET /mesh/connect/{agent_id} + one-way REGISTER
  * 30s KEEPALIVE loop (client-side; the server ignores them — keepalive only
    keeps the socket warm and detects dead connections via read errors)
  * REQUEST -> RESPONSE correlation via the message_id <-> request_id contract
    (a RESPONSE carrying a non-matching request_id is silently dropped by the
    server, so the client MUST echo the request's message_id as request_id)
  * ERROR frames surfaced as synthetic 500-style MeshError exceptions
  * responder dispatch: incoming REQUESTs go to a handler; RESPONSEs are built
    and sent automatically (body JSON-encoded as a string, per the wire quirk)

Thread model: each MeshClient owns one asyncio loop in a background thread;
the public API (request / respond / close) is synchronous and safe to call
from any thread. This is the piece Crier does not ship — a usable client
runtime — and the LLM agents in this directory are built on top of it.
"""

from __future__ import annotations

import asyncio
import datetime
import json
import logging
import secrets
import threading
import time
from typing import Any, Callable, Optional

import websockets

log = logging.getLogger("crier-mesh")

KEEPALIVE_INTERVAL_S = 30.0
PROTOCOL_VERSION = 1


def _now() -> str:
    # RFC3339 with timezone COLON (-05:00) and microseconds — Go's time.Time
    # JSON unmarshal rejects the no-colon form (-0500) and the server then
    # silently drops the frame. This exact bug broke the whole raw lane.
    return datetime.datetime.now(datetime.timezone.utc).astimezone().isoformat(
        timespec="microseconds")


def _msg_id() -> str:
    return secrets.token_hex(12)  # 24 hex chars, crypto/rand


class MeshError(Exception):
    """Raised when a REQUEST cannot be completed (ERROR frame, send failure)."""

    def __init__(self, code: str, message: str):
        super().__init__(f"[{code}] {message}")
        self.code = code
        self.message = message


class MeshClient:
    def __init__(self, agent_id: str, url: str, debug: bool = False):
        self.agent_id = agent_id
        self.url = url
        self.debug = debug
        self._loop: Optional[asyncio.AbstractEventLoop] = None
        self._ws: Any = None
        self._thread: Optional[threading.Thread] = None
        self._lock = threading.Lock()
        self._pending: dict[str, threading.Event] = {}   # message_id -> event
        self._results: dict[str, Any] = {}               # message_id -> frame | exception
        self._responder: Optional[Callable[[dict], dict]] = None
        self._closed = threading.Event()

    # ---------------------------------------------------------------- lifecycle

    def connect(self, timeout_s: float = 10.0) -> None:
        self._loop = asyncio.new_event_loop()
        self._thread = threading.Thread(
            target=self._loop.run_forever, daemon=True, name=f"mesh-{self.agent_id}"
        )
        self._thread.start()
        try:
            self._run_coro(self._open(), timeout_s)
        except Exception:
            self.close()
            raise

    async def _open(self) -> None:
        self._ws = await websockets.connect(self.url)
        await self._ws.send(json.dumps({
            "type": "REGISTER", "version": PROTOCOL_VERSION,
            "message_id": _msg_id(), "timestamp": _now(),
            "agent_id": self.agent_id, "lease_id": "", "lease_ttl_ms": 3600000,
            "capabilities": {"version": "0.1.0", "topics": [],
                             "max_concurrent_sessions": 4},
        }))
        asyncio.get_running_loop().create_task(self._reader())
        asyncio.get_running_loop().create_task(self._keepalive())

    def close(self) -> None:
        self._closed.set()
        if self._loop is not None and self._loop.is_running():
            try:
                self._run_coro(self._shutdown(), 3.0)
            except Exception:
                pass
            self._loop.call_soon_threadsafe(self._loop.stop)

    async def _shutdown(self) -> None:
        if self._ws is not None:
            try:
                await self._ws.close()
            except Exception:
                pass

    def _run_coro(self, coro, timeout_s: float) -> Any:
        fut = asyncio.run_coroutine_threadsafe(coro, self._loop)
        return fut.result(timeout=timeout_s)

    def _log(self, direction: str, frame: dict) -> None:
        if self.debug:
            compact = {k: v for k, v in frame.items() if k != "body"}
            log.info("%s %s %s", self.agent_id, direction, json.dumps(compact))

    # ---------------------------------------------------------------- reader

    async def _reader(self) -> None:
        try:
            async for raw in self._ws:
                try:
                    frame = json.loads(raw)
                except json.JSONDecodeError:
                    # The server silently drops malformed frames; mirror it.
                    log.warning("%s dropped unparseable frame", self.agent_id)
                    continue
                self._log("<<", frame)
                ftype = frame.get("type")
                if ftype == "REQUEST":
                    await self._dispatch_request(frame)
                elif ftype == "RESPONSE":
                    self._resolve(frame, error=None)
                elif ftype == "ERROR":
                    err = frame.get("error") or {}
                    self._resolve(frame, error=MeshError(
                        err.get("code", "UNKNOWN"), err.get("message", "mesh error")))
                # REGISTER_ACK / KEEPALIVE / anything else: ignored per spec.
        except Exception as exc:
            log.debug("%s reader stopped: %s", self.agent_id, exc)

    async def _keepalive(self) -> None:
        while not self._closed.is_set():
            await asyncio.sleep(KEEPALIVE_INTERVAL_S)
            try:
                await self._ws.send(json.dumps({
                    "type": "KEEPALIVE", "version": PROTOCOL_VERSION,
                    "message_id": _msg_id(), "timestamp": _now(),
                    "lease_id": "", "agent_id": self.agent_id,
                }))
            except Exception:
                return

    async def _dispatch_request(self, frame: dict) -> None:
        if self._responder is None:
            return  # no handler -> drop (protocol-style silent drop)
        body = await self._loop.run_in_executor(None, self._responder, frame)
        resp = {
            "type": "RESPONSE", "version": PROTOCOL_VERSION,
            "message_id": _msg_id(), "timestamp": _now(),
            "request_id": frame["message_id"],          # THE correlation contract
            "source": {"agent_id": self.agent_id},
            "status_code": 200,
            "body": json.dumps(body),                   # wire quirk: string-encoded
            "trace_id": frame.get("trace_id", ""),
        }
        self._log(">>", resp)
        try:
            await self._ws.send(json.dumps(resp))
        except Exception:
            pass

    def _resolve(self, frame: dict, error: Optional[MeshError]) -> None:
        rid = frame.get("request_id")
        if not rid:
            return
        with self._lock:
            ev = self._pending.pop(rid, None)
            if ev is not None:
                self._results[rid] = error if error is not None else frame
        if ev is None:
            log.debug("%s frame for unknown request_id %s (silent drop)",
                      self.agent_id, rid)
        else:
            ev.set()

    # ---------------------------------------------------------------- public API

    def respond(self, handler: Callable[[dict], dict]) -> None:
        """Register the handler for incoming REQUEST frames.

        The handler receives the parsed frame and must return a dict that
        becomes the RESPONSE body (JSON-encoded as a string on the wire).
        """
        self._responder = handler

    def request(self, target: str, method: str, path: str, body: Any = None,
                timeout_ms: int = 15000, trace_id: str = "") -> dict:
        """Send a REQUEST and block for the RESPONSE.

        Returns the RESPONSE frame. Raises MeshError when the server returns an
        ERROR frame (e.g. CONTROLLER_OFFLINE) and TimeoutError when no response
        arrives within timeout_ms.
        """
        mid = _msg_id()
        ev = threading.Event()
        with self._lock:
            self._pending[mid] = ev
        frame = {
            "type": "REQUEST", "version": PROTOCOL_VERSION,
            "message_id": mid, "timestamp": _now(),
            "source": {"agent_id": self.agent_id},
            "target": {"agent_id": target},
            "method": method, "path": path,
            "trace_id": trace_id or mid, "timeout_ms": timeout_ms,
        }
        if body is not None:
            frame["body"] = body
        self._log(">>", frame)
        try:
            self._run_coro(self._ws.send(json.dumps(frame)), 5.0)
        except Exception as exc:
            with self._lock:
                self._pending.pop(mid, None)
            raise MeshError("SEND_FAILED", str(exc)) from exc
        if not ev.wait(timeout_ms / 1000.0):
            with self._lock:
                self._pending.pop(mid, None)
            raise TimeoutError(f"no RESPONSE for {mid} within {timeout_ms}ms")
        with self._lock:
            result = self._results.pop(mid)
        if isinstance(result, BaseException):
            raise result
        return result
