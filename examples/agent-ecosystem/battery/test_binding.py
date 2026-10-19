#!/usr/bin/env python3
"""Provider-free verifier tests, including real fake-consumer HTTP + subprocesses."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import queue
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import unittest

HERE = Path(__file__).parent
CHECKER = Path(os.environ.get("DF_CRIER_CHECKER", HERE / "check-harness.py"))
spec = importlib.util.spec_from_file_location("checker", CHECKER)
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)
NONCE = "4bb57a79-dd26-45be-a3ed-bf78d52e7b68"
OLD_NONCE = "abc81347-807b-4b10-aace-9ce6030d233f"
REQUEST = {"payload": {"text": "hello", "battery_nonce": NONCE}}


def live(nonce=NONCE, text="runtime answer", request=None):
    request = request or {"payload": {"text": "hello", "battery_nonce": nonce}}
    digest = checker.request_digest(request)
    reply = f"{text}\n[crier-battery-nonce:{nonce}]\n[crier-battery-request-sha256:{digest}]"
    return {"live": True, "exec": "/fixture/runtime", "stdout_bytes": len(reply.encode()),
            "reply": reply, "nonce": nonce, "request_sha256": digest,
            "stdout_sha256": hashlib.sha256(reply.encode()).hexdigest()}


def verify(body, output=None, **kwargs):
    return checker.verdict("200", body, NONCE, request=REQUEST,
                           runtime_output=live()["reply"] if output is None else output,
                           minted_nonces={NONCE}, **kwargs)


class BindingTests(unittest.TestCase):
    def test_echo_without_run(self):
        proof = verify(live(), output="")
        self.assertEqual(proof["outcome"], "NOT_EXECUTED", proof)
        self.assertEqual(proof["result"], "FAIL")

    def test_genuine(self):
        self.assertEqual(verify({"reply": live()})["outcome"], "SUPPORTED")

    def test_body_missing_nonce(self):
        for body in ({**live(), "reply": "runtime answer", "nonce": None},
                     {k: v for k, v in live().items() if k != "nonce"}):
            self.assertEqual(verify(body)["outcome"], "ECHOED_WITHOUT_RUN")

    def test_request_digest_not_consumer_reply_digest(self):
        # Self-consistent response hash used to pass; it cannot override sent text.
        forged = live(request={"payload": {"text": "changed by consumer", "battery_nonce": NONCE}})
        self.assertEqual(verify(forged, output=forged["reply"])["outcome"], "DIGEST_MISMATCH")
        self.assertEqual(verify({**live(), "stdout_sha256": "0" * 64})["outcome"], "DIGEST_MISMATCH")

    def test_replayed_and_unminted(self):
        self.assertEqual(verify(live(OLD_NONCE))["outcome"], "NONCE_REPLAY")
        used = set()
        self.assertEqual(verify(live(), used_nonces=used)["outcome"], "SUPPORTED")
        self.assertEqual(verify(live(), used_nonces=used)["outcome"], "NONCE_REPLAY")
        proof = checker.verdict("200", live(), NONCE, request=REQUEST,
                                runtime_output=live()["reply"], minted_nonces=set())
        self.assertEqual(proof["outcome"], "NONCE_REPLAY")

    def test_refused_failed_timeout_keep_diagnostic(self):
        for status in ("REFUSED", "FAILED", "TIMEOUT"):
            proof = verify({"status": status, "reason": "exit 3; stderr: boom-347", "live": False}, output="")
            self.assertEqual(proof["outcome"], status)
            self.assertIn("boom-347", proof["reason"])

    def test_status_not_inferred_from_untrusted_diagnostic(self):
        proof = verify({"status": "FAILED", "reason": "NONCE_REPLAY is provider prose"}, output="")
        self.assertEqual(proof["outcome"], "FAILED")

    def test_canned_reason_unchanged(self):
        proof = verify({"live": False, "canned": True, "reply": "canned"})
        self.assertEqual(proof["reason"], "live=false (canned/non-executed harness)")

    def test_same_content_even_with_new_digest_marker(self):
        previous = checker.execution_proof(live(OLD_NONCE), OLD_NONCE)["content_sha256"]
        self.assertIn("same runtime stdout digest", verify(live(), previous_digest=previous)["reason"])
        self.assertEqual(verify(live(text="different"), previous_digest=previous)["result"], "PASS")

    def test_no_capture_cannot_pass_cli(self):
        reply = f"runtime answer\n[crier-battery-nonce:{NONCE}]"
        forged = {"live": True, "exec": "/fixture/runtime", "stdout_bytes": len(reply.encode()),
                  "reply": reply, "nonce": NONCE,
                  "stdout_sha256": hashlib.sha256(reply.encode()).hexdigest()}
        run = subprocess.run([sys.executable, str(CHECKER), "200", NONCE],
                             input=json.dumps(forged), text=True, capture_output=True)
        print("body-only control: " + run.stdout.strip(), flush=True)
        self.assertEqual(run.returncode, 1)

    def test_empty_duplicate_marker_and_metadata_only(self):
        for text in (f"[crier-battery-nonce:{NONCE}]", live()["reply"] * 2):
            body = {**live(), "reply": text, "stdout_sha256": hashlib.sha256(text.encode()).hexdigest()}
            self.assertEqual(verify(body)["result"], "FAIL")
        # The observer URL/header contains the nonce, but no raw bytes do.
        self.assertEqual(verify(live(), output="")["outcome"], "NOT_EXECUTED")

    def test_real_http_fake_consumers(self):
        for mode, expected in (("echo-without-run", "NOT_EXECUTED"), ("run-and-echo", "SUPPORTED"),
                               ("runtime-only", "ECHOED_WITHOUT_RUN"), ("digest-mismatch", "DIGEST_MISMATCH"),
                               ("replay", "NONCE_REPLAY")):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                streams = {}
                previous = []
                runtime = Path(directory) / "runtime.py"
                # Real fixture subprocess sees exactly the delivered request.
                runtime.write_text('import json,sys\np=json.load(sys.stdin)["payload"]\nprint("fixture ran: "+p["text"])\nprint("[crier-battery-nonce:"+p["battery_nonce"]+"]")\n')
                class Handler(BaseHTTPRequestHandler):
                    def log_message(self, *_):
                        pass

                    def do_GET(self):
                        nonce = self.path.rsplit("/", 1)[-1]
                        stream = queue.Queue()
                        streams[nonce] = stream
                        self.send_response(200)
                        self.end_headers()
                        self.wfile.flush()
                        output = stream.get(timeout=10)
                        self.wfile.write(output.encode())

                    def do_POST(self):
                        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                        nonce = request["payload"]["battery_nonce"]
                        body = live(nonce, "fixture answer", request)
                        output = ""
                        if mode != "echo-without-run":
                            output = subprocess.run([sys.executable, str(runtime)], input=json.dumps(request),
                                                    text=True, capture_output=True, check=True).stdout
                        if mode == "runtime-only":
                            body.update(reply="answer without nonce", nonce=None)
                        if mode == "digest-mismatch":
                            body = live(nonce, request={"payload": {"text": "forged", "battery_nonce": nonce}})
                        if mode == "replay" and previous:
                            body = previous[0]
                        previous.append(body)
                        streams[nonce].put(output)
                        self.send_response(200)
                        self.end_headers()
                        self.wfile.write(json.dumps({"reply": body}).encode())
                server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                try:
                    url = f"http://127.0.0.1:{server.server_port}"
                    env = {**os.environ, "BATTERY_RUNTIME_BASE": url}
                    args = [sys.executable, str(HERE / "probe-runtime.py"), url, "opencode", "hello", "", directory]
                    if mode == "replay":
                        first = subprocess.run(args, env=env, text=True, capture_output=True, timeout=15)
                        self.assertEqual(json.loads(first.stdout)["outcome"], "SUPPORTED", first.stdout)
                    run = subprocess.run(args, env=env, text=True, capture_output=True, timeout=15)
                    proof = json.loads(run.stdout)
                    self.assertEqual(proof["outcome"], expected, proof)
                    self.assertEqual(run.returncode, 0 if expected == "SUPPORTED" else 1)
                    ledger = json.loads((Path(directory) / "nonces.json").read_text())
                    self.assertEqual(len(ledger["minted"]), 2 if mode == "replay" else 1)
                    self.assertEqual(set(ledger["minted"]), set(ledger["used"]))
                    self.assertEqual(proof["capture_ok"], True)
                    self.assertEqual(proof["runtime_nonce_observed"], mode != "echo-without-run")
                    print(f"fixture {mode}: {proof['result']} / {proof['outcome']}; runtime_nonce_observed={proof['runtime_nonce_observed']}; exit {run.returncode}", flush=True)
                finally:
                    server.shutdown()
                    server.server_close()
                    thread.join()


if __name__ == "__main__":
    unittest.main()
