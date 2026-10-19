#!/usr/bin/env python3
"""Offline checker fixtures. --fixtures DIR retains bodies for manual CLI checks."""

import hashlib
import json
from pathlib import Path
import subprocess
import sys
import unittest

CHECKER = Path(__file__).with_name("check-harness.py")
NONCE = "4bb57a79-dd26-45be-a3ed-bf78d52e7b68"
OLD_NONCE = "abc81347-807b-4b10-aace-9ce6030d233f"


def live(nonce=NONCE, text="runtime answer"):
    reply = f"{text}\n[crier-battery-nonce:{nonce}]"
    return {"live": True, "exec": "/fixture/runtime", "stdout_bytes": len(reply.encode()),
            "reply": reply, "nonce": nonce,
            "stdout_sha256": hashlib.sha256(reply.encode()).hexdigest()}


FIXTURES = {
    "genuine": live(),
    "wrong": live(OLD_NONCE),
    "missing": {k: v for k, v in live().items() if k != "nonce"},
    "replayed": {"reply": live(OLD_NONCE)},
    "canned": {"live": False, "canned": True, "reply": "canned"},
    "metadata-only": {**live(), "reply": "runtime answer"},
    "same-text": live(text="one canned answer"),
    "different-text": live(text="a different runtime answer"),
}


def check(body, previous=""):
    args = ["python3", str(CHECKER), "200", NONCE]
    if previous:
        args.append(previous)
    return subprocess.run(args, input=json.dumps(body), text=True, capture_output=True, check=False)


class BindingTests(unittest.TestCase):
    def test_genuine(self):
        result = check(FIXTURES["genuine"])
        self.assertEqual(result.returncode, 0, result.stdout)
        proof = json.loads(result.stdout)
        self.assertIs(proof["nonce_matched"], True)
        self.assertEqual(proof["nonce"], NONCE)
        self.assertEqual(len(proof["content_sha256"]), 64)

    def test_fabricated_wrong_or_missing_nonce(self):
        for name in ("wrong", "missing", "metadata-only"):
            with self.subTest(name=name):
                result = check(FIXTURES[name])
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertIn("nonce", result.stdout)
                self.assertIs(json.loads(result.stdout)["nonce_matched"], False)

    def test_replayed(self):
        result = check(FIXTURES["replayed"])
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("nonce", result.stdout)

    def test_canned_reason_unchanged(self):
        result = check(FIXTURES["canned"])
        self.assertEqual(result.returncode, 1)
        self.assertEqual(json.loads(result.stdout)["reason"], "live=false (canned/non-executed harness)")

    def test_same_text_for_different_prompts(self):
        # Change the request nonce too: merely hashing the marker would mask the canned text.
        first = check(live(OLD_NONCE, "one canned answer"))
        self.assertEqual(first.returncode, 1, first.stdout)  # old run is independently refused
        digest = json.loads(first.stdout)["content_sha256"]
        result = check(FIXTURES["same-text"], digest)
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("same runtime stdout digest", result.stdout)
        self.assertIs(json.loads(result.stdout)["nonce_matched"], True)

    def test_different_text(self):
        first = json.loads(check(FIXTURES["genuine"]).stdout)
        result = check(FIXTURES["different-text"], first["content_sha256"])
        self.assertEqual(result.returncode, 0, result.stdout)

    def test_digest_integrity(self):
        result = check({**live(), "stdout_sha256": "0" * 64})
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("stdout_sha256", result.stdout)

    def test_empty_or_duplicated_marker_not_proof(self):
        for text in (f"[crier-battery-nonce:{NONCE}]", live()["reply"] * 2):
            body = {**live(), "reply": text, "stdout_sha256": hashlib.sha256(text.encode()).hexdigest()}
            self.assertEqual(check(body).returncode, 1)


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--fixtures":
        target = Path(sys.argv[2])
        target.mkdir(parents=True, exist_ok=True)
        for name, body in FIXTURES.items():
            (target / f"{name}.json").write_text(json.dumps(body) + "\n")
        (target / "nonce.txt").write_text(NONCE + "\n")
        print(f"fixtures: {target}; nonce: {NONCE}")
    else:
        unittest.main()
