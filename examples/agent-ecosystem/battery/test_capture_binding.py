"""DF-CRIER-392 controls: real child pipes and files, no sockets/providers.

DF_CRIER_CHECKER can point at the pre-change checker to reproduce each failure.
"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

import pytest

HERE = Path(__file__).parent
spec = importlib.util.spec_from_file_location(
    "capture_checker", os.environ.get("DF_CRIER_CHECKER", HERE / "check-harness.py")
)
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)
NONCE = "4bb57a79-dd26-45be-a3ed-bf78d52e7b68"
OLD_NONCE = "abc81347-807b-4b10-aace-9ce6030d233f"


@pytest.fixture(name="tmp_path")
def capture_directory():
    # Keep fixture artifacts under /tmp even when the runner exports TMPDIR.
    with tempfile.TemporaryDirectory(prefix="crier-capture-", dir="/tmp") as directory:
        yield Path(directory)


def request(nonce):
    # Correlation differs from the challenge; a tee no longer carries a nonce.
    return {"payload": {"text": "echo hello", "battery_id": "old" if nonce == OLD_NONCE else "current"}}


def reply(sent, answer="actual child answer"):
    nonce = OLD_NONCE if sent["payload"]["battery_id"] == "old" else NONCE
    text = (f"{answer}\n[crier-battery-nonce:{nonce}]\n"
            f"[crier-battery-request-sha256:{checker.request_digest(sent)}]")
    raw = (text + "\n").encode()
    return {"reply": text, "live": True, "exec": sys.executable, "nonce": nonce,
            "request_sha256": checker.request_digest(sent), "stdout_bytes": len(raw),
            "stdout_sha256": hashlib.sha256(text.encode()).hexdigest(),
            "stdout_raw_sha256": hashlib.sha256(raw).hexdigest()}, raw


def child_output(raw):
    run = subprocess.run([sys.executable, "-c", "import sys; sys.stdout.buffer.write(sys.stdin.buffer.read())"],
                         input=raw, capture_output=True, check=True)
    return run.stdout


def check_file(tmp_path, sent, body, captured):
    # Drive the production CLI so the file is read as bytes, not just a helper.
    (tmp_path / "request").write_text(json.dumps(sent))
    (tmp_path / "capture").write_bytes(captured)
    nonce = OLD_NONCE if sent["payload"]["battery_id"] == "old" else NONCE
    (tmp_path / "ledger").write_text(json.dumps({"minted": [nonce], "used": []}))
    run = subprocess.run([sys.executable, str(Path(checker.__file__)), "200",
                          nonce, "", str(tmp_path / "request"),
                          str(tmp_path / "capture"), str(tmp_path / "ledger")],
                         input=json.dumps(body), text=True, capture_output=True, check=False)
    proof = json.loads(run.stdout)
    print(f"capture control: {proof['outcome']}; {proof['reason']}")
    return proof


def test_request_body_tee_without_child(tmp_path):
    sent = request(NONCE)
    body, _ = reply(sent)
    proof = check_file(tmp_path, sent, body, json.dumps(sent).encode())
    assert proof["outcome"] == "NOT_EXECUTED", proof
    assert "out-of-band challenge" in proof["reason"]
    assert proof["result"] == "FAIL"


def test_child_spawn_with_fabricated_reply(tmp_path):
    sent = request(NONCE)
    _, raw = reply(sent)
    captured = child_output(raw)
    body, _ = reply(sent, "fabricated by consumer, not in child stdout")
    proof = check_file(tmp_path, sent, body, captured)
    assert proof["outcome"] == "DIGEST_MISMATCH", proof


def test_successful_capture_replayed_under_fresh_body(tmp_path):
    old = request(OLD_NONCE)
    old_body, raw = reply(old)
    captured = child_output(raw)
    assert check_file(tmp_path, old, old_body, captured)["outcome"] == "SUPPORTED"
    sent = request(NONCE)
    body, _ = reply(sent)
    proof = check_file(tmp_path, sent, body, captured)
    assert proof["outcome"] == "NONCE_REPLAY", proof


def test_capture_bytes_not_lossy_decoded_or_trimmed_before_raw_hash(tmp_path):
    sent = request(NONCE)
    body, raw = reply(sent)
    # A body cannot hide changed whitespace, invalid UTF-8 or CRLF translation.
    for captured in (b" " + raw, raw.replace(b"\n", b"\r\n"), raw + b"\xff"):
        proof = check_file(tmp_path, sent, body, captured)
        assert proof["outcome"] == "DIGEST_MISMATCH", proof


def test_good_child_raw_capture_and_trimmed_reply(tmp_path):
    sent = request(NONCE)
    body, raw = reply(sent, "héllo world")
    captured = child_output(raw)
    proof = check_file(tmp_path, sent, body, captured)
    assert proof["outcome"] == "SUPPORTED", proof
    assert proof["captured_runtime_sha256"] == hashlib.sha256(captured).hexdigest()
    assert proof["captured_runtime_bytes"] == len(captured)


def test_fabricated_channels_render_supported_ceiling(tmp_path):
    # No runtime child is spawned: the same consumer can fabricate capture and
    # reply. Option B declares this ceiling; it does not pretend to close it.
    sent = request(NONCE)
    body, raw = reply(sent, "fabricated in both consumer channels")
    proof = check_file(tmp_path, sent, body, raw)
    assert proof["outcome"] == "SUPPORTED", proof
    qualifier = (
        "SUPPORTED means the capture bytes reported by the consumer equal its reply, "
        "hashed before decode; it is not attestation against a consumer fabricating both channels."
    )
    assert proof["reason"].startswith(qualifier), proof


def test_capture_hash_cannot_legitimize_different_body_text(tmp_path):
    sent = request(NONCE)
    _, raw = reply(sent)
    body, _ = reply(sent, "x" * len("actual child answer"))
    body["stdout_raw_sha256"] = hashlib.sha256(raw).hexdigest()
    assert body["stdout_bytes"] == len(raw), "hold digest and count consistent with capture"
    proof = check_file(tmp_path, sent, body, child_output(raw))
    assert proof["outcome"] == "DIGEST_MISMATCH", proof
    assert "captured stdout differs" in proof["reason"]


def test_raw_digest_and_byte_count_are_both_required(tmp_path):
    sent = request(NONCE)
    body, raw = reply(sent)
    for field in ("stdout_bytes", "stdout_raw_sha256"):
        changed = {**body, field: len(raw) + 1 if field == "stdout_bytes" else "0" * 64}
        assert check_file(tmp_path, sent, changed, raw)["outcome"] == "DIGEST_MISMATCH"


def test_exact_ecmascript_trim_not_python_strip(tmp_path):
    sent = request(NONCE)
    for prefix in ("\ufeff", "\u0085"):
        body, raw = reply(sent, prefix + "answer")
        # BOM is JavaScript trim whitespace, U+0085 deliberately is not.
        if prefix == "\ufeff":
            body["reply"] = body["reply"].removeprefix(prefix)
            body["stdout_sha256"] = hashlib.sha256(body["reply"].encode()).hexdigest()
        assert check_file(tmp_path, sent, body, raw)["outcome"] == "SUPPORTED"


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-q"]))
