#!/usr/bin/env python3
"""Verifier-owned request, nonce ledger and independent runtime-output capture.

Usage: probe-runtime.py CRIER AGENT PROMPT PREVIOUS_DIGEST RUN_STATE_DIR
No provider keys needed by the verifier. The consumer owns runtime credentials.
"""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid

spec = importlib.util.spec_from_file_location("checker", Path(__file__).with_name("check-harness.py"))
checker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(checker)
PORTS = {"pi-agent": 9101, "opencode": 9102, "claude-code": 9103, "codex": 9104, "aider": 9105, "goose": 9106}


def probe(crier, agent, prompt, previous, state):
    state = Path(state)
    ledger_path = state / "nonces.json"
    ledger = json.loads(ledger_path.read_text()) if ledger_path.exists() else {"minted": [], "used": []}
    nonce = str(uuid.uuid4())
    if nonce in ledger["minted"]:
        raise ValueError("NONCE_REPLAY: random generator repeated a minted nonce")
    ledger["minted"].append(nonce)
    # Record BEFORE any outbound call. The response cannot choose the challenge.
    ledger_path.write_text(json.dumps(ledger))
    request = {"payload": {"text": prompt, "battery_nonce": nonce}, "sender": "battery",
               "session_id": f"eco-{agent}-{nonce}", "delivery_mode": "blocking", "timeout_ms": 30000}
    data = json.dumps(request, ensure_ascii=False)
    (state / f"{nonce}.request.json").write_text(data)
    output_path = state / f"{nonce}.runtime"
    header_path = state / f"{nonce}.headers"
    base = os.environ.get("BATTERY_RUNTIME_BASE", f"http://{agent}:{PORTS[agent]}")
    # The GET is opened before delivery, and only child stdout/stderr is streamed
    # on it. Do not use a body-provided stdout, digest, log URL or capture path.
    with output_path.open("wb") as capture, (state / f"{nonce}.capture-errors").open("wb") as errors:
        observer = subprocess.Popen(["curl", "-s", "--no-buffer", "--max-time", "36", "-D", str(header_path),
                                     f"{base}/battery-output/{nonce}"], stdout=capture, stderr=errors)
        try:
            deadline = time.monotonic() + 3
            while observer.poll() is None and time.monotonic() < deadline:
                if header_path.exists() and b"\r\n\r\n" in header_path.read_bytes():
                    break
                time.sleep(0.01)
            delivery = subprocess.run(["curl", "-s", "--max-time", "35", "-w", "\n%{http_code}",
                                       "-X", "POST", f"{crier}/agents/{agent}/inbox",
                                       "-H", "Content-Type: application/json", "-d", data],
                                      capture_output=True, text=True, timeout=37, check=False)
            try:
                observer.wait(timeout=2)
            except subprocess.TimeoutExpired:
                observer.kill()
                observer.wait()
            capture.flush()
        finally:
            if observer.poll() is None:
                observer.kill()
                observer.wait()
    raw, _, status = delivery.stdout.rpartition("\n")
    try:
        body = json.loads(raw)
    except ValueError:
        body = {"error": "invalid delivery JSON: " + raw[:300]}
    headers = header_path.read_text(errors="replace") if header_path.exists() else ""
    capture_ok = observer.returncode == 0 and headers.startswith("HTTP/") and headers.splitlines()[0].split()[1:2] == ["200"]
    output = output_path.read_text(errors="replace") if capture_ok else ""
    used = set(ledger["used"])
    proof = checker.verdict(status, body, nonce, previous, request=request,
                            runtime_output=output, minted_nonces=ledger["minted"], used_nonces=used)
    ledger["used"] = sorted(used)
    ledger_path.write_text(json.dumps(ledger))
    proof.update(http=status, body=raw, request=request, capture_ok=capture_ok,
                 capture_exit_code=observer.returncode, delivery_exit_code=delivery.returncode)
    return proof


if __name__ == "__main__":
    try:
        proof = probe(*sys.argv[1:])
    except (OSError, ValueError, TypeError, subprocess.SubprocessError) as error:
        proof = {"result": "FAIL", "outcome": "FAILED", "reason": str(error)}
    print(json.dumps(proof))
    sys.exit(0 if proof["result"] == "PASS" else 1)
