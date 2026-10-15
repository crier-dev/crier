"""Delivery signing at the real urllib HTTP boundary (loopback only)."""
import hashlib
import json
import os
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(__file__))
from crier_client import Crier, SigningKey, ed25519_verify


class ServiceDeliverySigning(unittest.TestCase):
    def setUp(self):
        self.seen = []
        seen = self.seen

        class Capture(BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                seen.append((self.path, dict(self.headers), body))
                self.send_response(201)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{}')

            def log_message(self, *_args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Capture)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.key = SigningKey.from_bytes(os.urandom(32))
        self.client = Crier("http://127.0.0.1:%d" % self.server.server_port,
                            agent_id="scheduler", key=self.key)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def test_own_sender_is_body_signed(self):
        self.client.deliver("router", {"task": "dispatch", "unicode": "\u2603"}, sender="scheduler")
        path, headers, body = self.seen[-1]
        headers = {k.lower(): v for k, v in headers.items()}
        digest = hashlib.sha256(body).hexdigest()
        self.assertEqual(headers.get("x-agent-body-sha256"), digest)
        self.assertEqual(headers.get("x-agent-id"), "scheduler")
        transcript = "POST\n%s\n%s\nsha256:%s" % (path, headers["x-agent-ts"], digest)
        self.assertTrue(ed25519_verify(bytes.fromhex(headers["x-agent-sig"]), transcript.encode(), self.key.public_key))
        self.assertEqual(json.loads(body)["sender"], "scheduler")

    def test_escaped_recipient_uses_server_path_transcript(self):
        self.client.deliver("router:service", {}, sender="scheduler")
        path, headers, body = self.seen[-1]
        self.assertEqual(path, "/agents/router%3Aservice/inbox")
        headers = {k.lower(): v for k, v in headers.items()}
        transcript = "POST\n/agents/router:service/inbox\n%s\nsha256:%s" % (
            headers.get("x-agent-ts"), hashlib.sha256(body).hexdigest())
        self.assertTrue(ed25519_verify(bytes.fromhex(headers.get("x-agent-sig", "")),
                                       transcript.encode(), self.key.public_key))

    def test_impersonation_is_never_signed(self):
        # Compatibility allows metadata-only delivery, but it must carry NO
        # identity proof for someone else's sender; a service recipient refuses it.
        self.client.deliver("router", {}, sender="outsider")
        _, headers, body = self.seen[-1]
        self.assertEqual(json.loads(body)["sender"], "outsider")
        self.assertFalse(any(k.lower().startswith("x-agent-") for k in headers))

    def test_missing_sender_remains_unsigned(self):
        self.client.deliver("ordinary", {})
        _, headers, body = self.seen[-1]
        self.assertNotIn("sender", json.loads(body))
        self.assertFalse(any(k.lower().startswith("x-agent-") for k in headers))

    def test_generic_signed_delivery_cannot_impersonate(self):
        for path in ("/agents/router/inbox", "/agents/router/inbox?trace=1", "/agents/router/%69nbox"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                self.client.request("POST", path,
                                    {"sender": "outsider", "payload": {}}, signed=True)
        self.assertEqual(self.seen, [])


if __name__ == "__main__":
    unittest.main()
