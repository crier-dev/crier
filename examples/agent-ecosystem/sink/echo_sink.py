# echo sink — the simplest agent in the mesh: receives webhook deliveries,
# replies ECHO:<text> (blocking), counts deliveries for the battery.
import json, os, threading, time
import requests
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(os.environ.get("PORT", "9002"))
CRIER = os.environ.get("CRIER_URL", "http://crier:8767")
LOCK = threading.Lock()
COUNT = {"deliveries": 0, "batches": 0}


def register():
    """Self-register with crier as the 'sink' agent (blocking webhook)."""
    import secrets
    pub = secrets.token_hex(32)
    body = {
        "id": "sink",
        "public_key": pub,
        "webhook": {
            "url": f"http://sink:{PORT}/hook",
            "delivery_mode": "blocking",
            "schema_template": "generic",
            "response_map": {"reply": "reply"},
        },
        "guard": {"policies": [{"id": "default"}]},
    }
    for _ in range(30):
        try:
            r = requests.post(f"{CRIER}/agents", json=body, timeout=3)
            if r.status_code in (200, 201, 409):
                with LOCK:
                    COUNT["registered"] = True
                print(f"sink registered with crier: {r.status_code}")
                return
        except Exception as e:
            pass
        time.sleep(1)
    print("sink: could not register with crier")


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
            # live check: re-register if crier lost us (restart/recreate)
            ok = False
            try:
                r = requests.get(f"{CRIER}/agents/sink", timeout=3)
                ok = r.ok
            except Exception:
                pass
            if not ok:
                register()
            with LOCK:
                self._send(200, {"registered": ok})
        else:
            self._send(404, {"error": "not found"})

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    print(f"sink listening :{PORT}")
    register()
    HTTPServer(("0.0.0.0", PORT), H).serve_forever()
