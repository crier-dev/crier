#!/usr/bin/env python3
"""
adapter.py — fake Hermes HTTP gateway adapter for the CR-FEAT-008 demo.

What this is
------------
The crier webhook driver POSTs a rendered `hermes-http-gateway` schema body to
this endpoint. The body looks like what Hermes' api_server chat endpoint would
receive:

    {
      "model": "deepseek-v4-flash",
      "messages": [{"role": "user", "content": "..."}],
      "stream": false,
      "session_id": "<crier.session_id>",
      "thread_id": "<crier.thread_id>"
    }

This adapter plays the role of the Hermes gateway side of the demo:

  1. It verifies the optional `X-Crier-Signature` HMAC (when DEMO_WEBHOOK_SECRET
     is set — the demo sets it, so the full envelope contract is exercised).
  2. It keeps a per-session message history, keyed by `session_id` — the same
     context-window continuity a real Hermes gateway would provide. The
     session id is resolved per the envelope contract (spec §3): the
     `X-Crier-Session` header (authoritative today) with the body
     `session_id` slot preferred when the template engine populates it.
  3. It produces the reply:
       - live backend (DEMO_BACKEND_URL, e.g. a real Hermes gateway or
         DeepSeek's OpenAI-compatible endpoint): forwards the FULL session
         history plus session_id/thread_id, extracts choices.0.message.content.
       - otherwise a canned but session-aware reply (turn counter, history
         length) so the demo runs offline.
  4. It answers with the OpenAI-chat-completions shape the
     `hermes-http-gateway` template's ResponseMap (`choices.0.message.content`)
     extracts:
         {"choices": [{"message": {"content": "<reply>"}}]}
     and echoes session_id/thread_id inside the reply text.

Every request is logged as one JSON line on stdout (the run-demo.sh transcript
captures it). GET /sessions dumps the recorded per-session turns so the demo
script can assert session continuity programmatically.

Stdlib only (http.server, urllib.request, hmac, hashlib, json). No pip deps.

Environment:
    ADAPTER_PORT / --port   listen port (default 18789)
    DEMO_WEBHOOK_SECRET     if set, verify X-Crier-Signature (HMAC-SHA256 of
                            the raw POST body, hex) against this secret
    DEMO_BACKEND_URL        if set, forward the session history here
                            (OpenAI-compatible /chat/completions)
    DEEPSEEK_API_KEY        if DEMO_BACKEND_URL is unset but this is set,
                            forward to https://api.deepseek.com/chat/completions
    DEMO_BACKEND_MODEL      model name sent to the backend (default
                            "deepseek-chat"; the incoming crier model name is
                            mapped: anything starting with "deepseek" ->
                            DEMO_BACKEND_MODEL)
    DEMO_BACKEND_TIMEOUT_S  forward timeout (default 60)
"""

import hashlib
import hmac
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("ADAPTER_PORT", "18789"))
WEBHOOK_SECRET = os.environ.get("DEMO_WEBHOOK_SECRET", "")
BACKEND_URL = os.environ.get("DEMO_BACKEND_URL", "")
DEEPSEEK_KEY = os.environ.get("DEEPSEEK_API_KEY", "")
BACKEND_MODEL = os.environ.get("DEMO_BACKEND_MODEL", "deepseek-chat")
BACKEND_TIMEOUT = int(os.environ.get("DEMO_BACKEND_TIMEOUT_S", "60"))

# session_id -> {"thread_id": str, "messages": [...], "turns": [...]}
SESSIONS = {}
SESSIONS_LOCK = threading.Lock()


def log_line(record: dict) -> None:
    """Emit one JSON line to stdout (captured into the demo transcript)."""
    record["ts"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    print(json.dumps(record), flush=True)


def verify_signature(raw_body: bytes, header: str) -> str:
    """Verify X-Crier-Signature when a secret is configured.

    Returns "verified" | "mismatch" | "missing". When no secret is configured
    the header is logged as-is (the contract makes signing optional).
    """
    if not WEBHOOK_SECRET:
        return "unchecked (no secret configured)"
    if not header:
        return "missing (server sent none)"
    expect = hmac.new(WEBHOOK_SECRET.encode(), raw_body, hashlib.sha256).hexdigest()
    if hmac.compare_digest(expect, header.strip()):
        return "verified"
    return "MISMATCH"


def map_model(model: str) -> str:
    """Map the crier template's model name onto the backend's accepted names."""
    if not model:
        return BACKEND_MODEL
    if model.startswith("deepseek") or model == "deepseek-chat":
        return BACKEND_MODEL
    return model


def forward_to_backend(session_id: str, thread_id: str, model: str,
                       messages: list) -> str:
    """Forward the full session history to the backend.

    Returns the backend's reply text, or raises on failure (caller falls back
    to a canned reply so the demo never hard-fails on network hiccups).
    """
    url = BACKEND_URL
    if not url and DEEPSEEK_KEY:
        url = "https://api.deepseek.com/chat/completions"
    if not url:
        raise RuntimeError("no backend configured")

    payload = {
        "model": model,
        "messages": messages,
        "stream": False,
        "session_id": session_id,
        "thread_id": thread_id,
    }
    headers = {"Content-Type": "application/json"}
    if url.startswith("https://api.deepseek.com") and DEEPSEEK_KEY:
        headers["Authorization"] = "Bearer " + DEEPSEEK_KEY
    req = urllib.request.Request(
        url, data=json.dumps(payload).encode(), headers=headers, method="POST"
    )
    with urllib.request.urlopen(req, timeout=BACKEND_TIMEOUT) as resp:
        body = json.loads(resp.read().decode())
    try:
        return body["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as exc:
        raise RuntimeError(
            "backend response missing choices.0.message.content: %r" % body
        ) from exc


def canned_reply(session_id: str, thread_id: str, turn: int,
                 content: str, history_len: int) -> str:
    """Canned but session-aware reply for offline runs.

    The turn counter + history length make session continuity visible in the
    transcript even without a live LLM.
    """
    if turn == 1:
        return (
            f"first message in session {session_id} received: '{content}'. "
            f"(canned reply — no LLM backend reachable)"
        )
    return (
        f"turn {turn} in session {session_id}: '{content}'. "
        f"Session context: {history_len - 1} prior message(s) in this window. "
        f"(canned reply — no LLM backend reachable)"
    )


def handle_webhook(raw_body: bytes, headers: dict) -> dict:
    """Process one crier webhook POST. Returns the JSON response dict."""
    sig = verify_signature(raw_body, headers.get("x-crier-signature", ""))
    event = headers.get("x-crier-event", "")
    agent = headers.get("x-crier-agent", "")
    session_hdr = headers.get("x-crier-session", "")

    try:
        body = json.loads(raw_body.decode())
    except (ValueError, UnicodeDecodeError) as exc:
        log_line({"path": "/webhook", "event": event, "agent": agent,
                  "error": "invalid json body: %r" % exc, "signature": sig})
        return {"error": "invalid json body"}

    model = body.get("model", "")
    messages = body.get("messages") or []
    session_body = body.get("session_id", "") or ""
    # Session mapping, envelope contract (spec §3): the X-Crier-Session header
    # is set directly from the envelope and is authoritative today. The body
    # slot ({{crier.session_id}} template placeholder) is preferred when the
    # template engine populates it — see README "known gap" (resolvePath does
    # not descend into the struct-typed EnvelopeMeta, so named templates
    # currently render it empty).
    session_id = session_body or session_hdr or ""
    thread_id = body.get("thread_id", "") or ""
    stream = body.get("stream", False)

    # Pull the newest user content out of the templated messages array.
    content = ""
    for msg in reversed(messages):
        if isinstance(msg, dict) and isinstance(msg.get("content"), str):
            content = msg["content"]
            break
    if not content:
        log_line({"path": "/webhook", "event": event, "agent": agent,
                  "session_id": session_id, "thread_id": thread_id,
                  "error": "no user content in messages", "signature": sig})
        return {"error": "no user content in messages"}

    with SESSIONS_LOCK:
        sess = SESSIONS.setdefault(
            session_id, {"thread_id": thread_id, "messages": [], "turns": []}
        )
        if not sess["thread_id"] and thread_id:
            sess["thread_id"] = thread_id
        sess["messages"].append({"role": "user", "content": content})
        turn = len(sess["turns"]) + 1

    backend = "canned"
    try:
        answer = forward_to_backend(
            session_id, thread_id, map_model(model), list(sess["messages"])
        )
        backend = BACKEND_URL or "deepseek"
        # Keep the session window a faithful transcript: the next forward
        # carries [user Q1, assistant A1, user Q2, ...].
        with SESSIONS_LOCK:
            sess["messages"].append({"role": "assistant", "content": answer})
    except Exception as exc:  # noqa: BLE001 — demo resilience: fall back
        log_line({"path": "/webhook", "event": event, "agent": agent,
                  "session_id": session_id, "thread_id": thread_id,
                  "turn": turn, "backend_error": str(exc), "signature": sig})
        answer = canned_reply(session_id, thread_id, turn, content,
                              len(sess["messages"]))

    # Echo session_id/thread_id in the reply so the blocking response the
    # sender sees carries both (template ResponseMap = choices.0.message.content).
    reply = f"[session={session_id} | thread={thread_id} | turn={turn}] {answer}"

    with SESSIONS_LOCK:
        sess["turns"].append({
            "turn": turn, "content": content, "reply": reply, "backend": backend,
            "event": event, "agent": agent, "model": model, "signature": sig,
            "session_body": session_body, "session_header": session_hdr,
        })

    log_line({"path": "/webhook", "event": event, "agent": agent,
              "session_id": session_id, "session_body": session_body,
              "session_header": session_hdr, "thread_id": thread_id,
              "turn": turn, "model": model, "stream": stream,
              "content": content, "backend": backend, "reply": reply,
              "signature": sig})
    return {"choices": [{"message": {"content": reply}}]}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):  # noqa: A002 — http.server API
        pass

    def _send(self, status: int, obj: dict) -> None:
        payload = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):  # noqa: N802 — http.server API
        if self.path == "/healthz":
            self._send(200, {"status": "ok"})
            return
        if self.path == "/sessions":
            with SESSIONS_LOCK:
                sessions = [
                    {"session_id": sid, "thread_id": s["thread_id"],
                     "turn_count": len(s["turns"]), "turns": s["turns"]}
                    for sid, s in sorted(SESSIONS.items())
                ]
            self._send(200, {"sessions": sessions})
            return
        self._send(404, {"error": "not found"})

    def do_POST(self):  # noqa: N802 — http.server API
        if self.path != "/webhook":
            self._send(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        try:
            # Lowercase header names so lookups are case-insensitive.
            headers = {k.lower(): v for k, v in self.headers.items()}
            result = handle_webhook(raw, headers)
        except Exception as exc:  # noqa: BLE001 — never crash the handler
            log_line({"path": "/webhook", "error": "handler crash: %r" % exc})
            self._send(500, {"error": "internal error"})
            return
        if "error" in result:
            self._send(400, result)
            return
        self._send(200, result)


def main() -> int:
    port = PORT
    if "--port" in sys.argv:
        try:
            port = int(sys.argv[sys.argv.index("--port") + 1])
        except (IndexError, ValueError):
            print("usage: adapter.py [--port N]", file=sys.stderr)
            return 2
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    log_line({"startup": True, "port": port, "backend_url": BACKEND_URL,
              "webhook_secret": bool(WEBHOOK_SECRET)})
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
