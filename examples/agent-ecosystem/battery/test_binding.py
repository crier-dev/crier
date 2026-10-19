#!/usr/bin/env python3
"""Provider-free verifier tests, including file-backed transport and real child subprocesses."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
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
            "stdout_sha256": hashlib.sha256(reply.encode()).hexdigest(),
            "stdout_raw_sha256": hashlib.sha256(reply.encode()).hexdigest()}


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
        self.assertEqual(verify(live(text="different"), output=live(text="different")["reply"], previous_digest=previous)["result"], "PASS")

    def test_no_capture_cannot_pass_cli(self):
        reply = f"runtime answer\n[crier-battery-nonce:{NONCE}]"
        forged = {"live": True, "exec": "/fixture/runtime", "stdout_bytes": len(reply.encode()),
                  "reply": reply, "nonce": NONCE,
                  "stdout_sha256": hashlib.sha256(reply.encode()).hexdigest(),
                  "stdout_raw_sha256": hashlib.sha256(reply.encode()).hexdigest()}
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

    def test_probe_file_transport_fake_consumers(self):
        for mode, expected in (("echo-without-run", "NOT_EXECUTED"), ("run-and-echo", "SUPPORTED"),
                               ("runtime-only", "ECHOED_WITHOUT_RUN"), ("digest-mismatch", "DIGEST_MISMATCH"),
                               ("replay", "NONCE_REPLAY")):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory(dir="/tmp") as directory:
                transport = Path(directory) / "curl"
                transport.write_text(f"#!{sys.executable}\n" + FAKE_TRANSPORT)
                transport.chmod(0o755)
                env = {**os.environ, "PATH": directory + os.pathsep + os.environ["PATH"],
                       "FIXTURE_DIR": directory, "FIXTURE_MODE": mode}
                args = [sys.executable, str(HERE / "probe-runtime.py"), "http://fixture.invalid",
                        "opencode", "hello", "", directory]
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
                self.assertEqual(proof["runtime_nonce_observed"], mode not in ("echo-without-run", "replay"))
                print(f"fixture {mode}: {proof['result']} / {proof['outcome']}; exit {run.returncode}", flush=True)


FAKE_TRANSPORT = r'''import hashlib, json, os, subprocess, sys, time
from pathlib import Path
args = sys.argv[1:]
root = Path(os.environ["FIXTURE_DIR"])
mode = os.environ["FIXTURE_MODE"]
url = next(a for a in args if a.startswith("http"))
if "/battery-output/" in url:
    nonce = url.split("/battery-output/", 1)[1].split("?")[0]
    assert url.endswith("?stream=stdout")
    Path(args[args.index("-D") + 1]).write_bytes(b"HTTP/1.1 200 OK\r\n\r\n")
    capture = root / (nonce + ".pipe")
    deadline = time.monotonic() + 8
    while not capture.exists():
        if time.monotonic() >= deadline: sys.exit(28)
        time.sleep(.01)
    sys.stdout.buffer.write(capture.read_bytes())
else:
    request = json.loads(args[args.index("-d") + 1])
    nonce = request["payload"]["battery_nonce"]
    digest = hashlib.sha256(json.dumps(request["payload"], ensure_ascii=False,
        separators=(",", ":"), sort_keys=True).encode()).hexdigest()
    text = "fixture ran: " + request["payload"]["text"] + "\n[crier-battery-nonce:" + nonce + "]\n[crier-battery-request-sha256:" + digest + "]"
    raw = text.encode()
    # A real child, not a network listener, supplies the good fixture capture.
    output = b"" if mode == "echo-without-run" else subprocess.run(
        [sys.executable, "-c", "import sys; sys.stdout.buffer.write(sys.stdin.buffer.read())"],
        input=raw, capture_output=True, check=True).stdout
    body = dict(reply=text, live=True, exec=sys.executable, stdout_bytes=len(raw), nonce=nonce,
                request_sha256=digest, stdout_sha256=hashlib.sha256(raw).hexdigest(),
                stdout_raw_sha256=hashlib.sha256(raw).hexdigest())
    if mode == "runtime-only": body.update(reply="answer without nonce", nonce=None)
    if mode == "digest-mismatch": body["request_sha256"] = "0" * 64
    cached = root / "previous.json"
    if mode == "replay" and cached.exists():
        body = json.loads(cached.read_text())
        output = (root / "previous.pipe").read_bytes()
    else:
        cached.write_text(json.dumps(body))
        (root / "previous.pipe").write_bytes(output)
    # Atomic publish avoids the observer reading a half-written fixture file.
    pending = root / (nonce + ".pending")
    pending.write_bytes(output)
    pending.rename(root / (nonce + ".pipe"))
    print(json.dumps({"reply": body}) + "\n200", end="")
'''

# These are also collected by the explicitly documented test_binding.py command.
from test_capture_binding import (
    capture_directory,
    test_request_body_tee_without_child,
    test_child_spawn_with_fabricated_reply,
    test_successful_capture_replayed_under_fresh_body,
    test_capture_bytes_not_lossy_decoded_or_trimmed_before_raw_hash,
    test_good_child_raw_capture_and_trimmed_reply,
    test_capture_hash_cannot_legitimize_different_body_text,
    test_raw_digest_and_byte_count_are_both_required,
    test_exact_ecmascript_trim_not_python_strip,
)


if __name__ == "__main__":
    unittest.main()
