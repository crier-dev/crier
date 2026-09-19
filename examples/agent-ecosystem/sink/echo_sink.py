# echo sink — the simplest agent in the mesh: receives webhook deliveries,
# replies ECHO:<text> (blocking), counts deliveries for the battery.
import json, os, threading, time
import requests
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(os.environ.get("PORT", "9002"))
CRIER = os.environ.get("CRIER_URL", "http://crier:8767")
LOCK = threading.Lock()
COUNT = {"deliveries": 0, "batches": 0}
# Registration state, served on /ready: an unregistered sink must never look
# ready (INT-CI-007 — the battery used to see "ready" and then eat 404s).
REG = {"registered": False, "last_status": None, "last_body": ""}


def register():
    """Self-register with crier as the 'sink' agent (blocking webhook).

    `POST /agents` decodes the `webhook` object STRICTLY — an unknown key is a
    400 at registration, not a silent drop (d97b777, DF-CRIER-150). The accepted
    keys are exactly `url, auth_type, auth_value_ref, schema_template,
    custom_schema, delivery_mode, batch, retries, timeout_ms`, so a webhook-level
    `response_map` is refused. Reply extraction lives on
    `custom_schema.response_map`; `schema_template: "generic"` needs none (the
    template's own default, `raw`, returns the response body).
    """
    import secrets
    pub = secrets.token_hex(32)
    body = {
        "id": "sink",
        "public_key": pub,
        "webhook": {
            "url": f"http://sink:{PORT}/hook",
            "delivery_mode": "blocking",
            "schema_template": "generic",
        },
        "guard": {"policies": [{"id": "default"}]},
    }
    for _ in range(30):
        try:
            r = requests.post(f"{CRIER}/agents", json=body, timeout=3)
            with LOCK:
                REG["last_status"] = r.status_code
                REG["last_body"] = r.text[:300]
            if r.status_code in (200, 201, 409):
                with LOCK:
                    REG["registered"] = True
                print(f"sink registered with crier: {r.status_code}", flush=True)
                return
            # A rejected registration must be impossible to miss: the server's
            # response body names the exact field it refused. flush=True — the
            # container's stdout is block-buffered, so an unflushed line here
            # would never reach `docker compose logs sink`.
            print(f"sink registration REJECTED: HTTP {r.status_code} — {r.text[:300]}", flush=True)
        except Exception as e:
            with LOCK:
                REG["last_body"] = f"{type(e).__name__}: {e}"[:300]
            print(f"sink registration error: {type(e).__name__}: {e}", flush=True)
        time.sleep(1)
    print("sink: could not register with crier — "
          f"last HTTP {REG['last_status']} body {REG['last_body']}", flush=True)


class H(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n) if n else b"{}"
        try:
            data = json.loads(raw)
        except Exception:
            data = {"raw": raw.decode(errors="replace")}
        with LOCK:
            COUNT["deliveries"] += 1
            if self.headers.get("X-Crier-Event") == "batch":
                COUNT["batches"] += 1
                COUNT["deliveries"] += len(data.get("messages", [])) - 1
        # blocking reply: the message payload text, echoed
        text = "ECHO: no text"
        if isinstance(data, dict):
            payload = data.get("payload")
            if isinstance(payload, dict) and isinstance(payload.get("text"), str):
                text = payload["text"]
            elif isinstance(payload, str):
                text = payload
        self._send(200, {"reply": "ECHO: " + text})

    def do_GET(self):
        if self.path == "/stats":
            with LOCK:
                self._send(200, dict(COUNT))
        elif self.path == "/health":
            self._send(200, {"status": "ok"})
        elif self.path == "/ready":
            # Live check: re-register if crier lost us (restart/recreate).
            # 200 ONLY once registered; until then 503 carrying the last
            # registration status + body, so a failed registration cannot
            # masquerade as readiness (INT-CI-007). The process stays alive.
            ok = False
            try:
                ok = requests.get(f"{CRIER}/agents/sink", timeout=3).ok
            except Exception:
                pass
            if not ok:
                register()
                try:
                    ok = requests.get(f"{CRIER}/agents/sink", timeout=3).ok
                except Exception:
                    ok = False
            with LOCK:
                if ok:
                    REG["registered"] = True
                    self._send(200, {"ready": True, "registered": True})
                else:
                    self._send(503, {
                        "ready": False,
                        "registered": False,
                        "last_status": REG["last_status"],
                        "last_body": REG["last_body"],
                    })
        else:
            self._send(404, {"error": "not found"})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    print(f"sink listening :{PORT}", flush=True)
    register()
    HTTPServer(("0.0.0.0", PORT), H).serve_forever()
