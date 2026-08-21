#!/usr/bin/env python3
"""Tiny echo webhook for the CR-FEAT-006 federation demo.

Receives relay-2's webhook POST (openai-compatible schema template:
{"model": ..., "messages": [{"role": "user", "content": "..."}]}) and
answers in OpenAI shape so the schema template's response map
(choices.0.message.content) extracts the reply:

    {"choices": [{"message": {"role": "assistant", "content": "echo: <text>"}}]}

Stdlib only. Each request is logged to stdout so the demo transcript shows
the relay-2 -> webhook hop.

Usage: python3 echo_webhook.py [port]   (default 18773)
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 18773


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):  # noqa: N802 (http.server naming)
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw)
        except ValueError:
            body = {}
        content = ""
        messages = body.get("messages") or []
        if messages:
            content = messages[-1].get("content", "")
        print(f"[webhook] POST {self.path} model={body.get('model')!r} "
              f"content={content!r}", flush=True)
        if self.path != "/webhook":
            self.send_response(404)
            self.end_headers()
            return
        reply = {"choices": [{"message": {"role": "assistant",
                                          "content": f"echo: {content}"}}]}
        data = json.dumps(reply).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, format, *args):  # noqa: A002 — silence default access log
        pass


if __name__ == "__main__":
    print(f"[webhook] listening on :{PORT}", flush=True)
    HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
