#!/usr/bin/env python3
"""Fail-closed execution assertion, shared by the battery and offline fixtures.

Usage: python3 check-harness.py HTTP_STATUS NONCE [PREVIOUS_CONTENT_SHA256] < response.json
Accepts a consumer body or crier's generic/raw {"reply": <consumer body>}.
Emits a JSON proof on both success and failure; exit 0 means all assertions passed.
A nonce binds output to a request, not to a trusted/attested runtime implementation.
"""

import hashlib
import json
import re
import sys
import uuid

MARKER = re.compile(r"\[crier-battery-nonce:([0-9a-f-]{36})\]")


def execution_proof(body, nonce):
    result = body.get("reply") if isinstance(body, dict) else None
    if not isinstance(result, dict):
        result = body if isinstance(body, dict) else {}
    text = result.get("reply")
    text = text if isinstance(text, str) else ""
    echoes = MARKER.findall(text)
    # Remove challenge markers before comparing the two answers: a consumer that
    # adds a fresh marker to the same canned text must not earn a different digest.
    content = MARKER.sub("", text).strip()
    return {
        "nonce": nonce,
        "nonce_echo": result.get("nonce"),
        "nonce_matched": bool(nonce) and result.get("nonce") == nonce and echoes == [nonce],
        "stdout_sha256": hashlib.sha256(text.encode()).hexdigest(),
        "content_sha256": hashlib.sha256(content.encode()).hexdigest(),
        "runtime_content_bytes": len(content.encode()),
    }


def assert_execution(status, body, nonce, previous_digest=""):
    if status != "200":
        raise ValueError(f"HTTP {status}, want 200")
    if not isinstance(body, dict) or "error" in body:
        raise ValueError("invalid response or delivery error")
    # Only unwrap the known transport envelope, never search arbitrary nested text.
    result = body["reply"] if isinstance(body.get("reply"), dict) else body
    if "error" in result:
        raise ValueError("harness error, not a reply")
    if result.get("live") is False:
        raise ValueError("live=false (canned/non-executed harness)")
    if result.get("live") is not True or result.get("canned", False) is not False:
        raise ValueError("missing/invalid live=true execution marker")
    if not isinstance(result.get("exec"), str) or not result["exec"].strip():
        raise ValueError("missing resolved exec/runtime")
    if type(result.get("stdout_bytes")) is not int or result["stdout_bytes"] <= 0:
        raise ValueError("missing/invalid positive stdout_bytes")
    if not isinstance(result.get("reply"), str) or not result["reply"].strip():
        raise ValueError("missing/non-text runtime reply")
    if str(uuid.UUID(nonce)) != nonce:
        raise ValueError("invalid request nonce")
    proof = execution_proof(body, nonce)
    if not proof["nonce_matched"]:
        raise ValueError("missing/mismatched runtime nonce echo")
    if result.get("stdout_sha256") != proof["stdout_sha256"]:
        raise ValueError("missing/mismatched stdout_sha256 (trimmed runtime text)")
    if not proof["runtime_content_bytes"]:
        raise ValueError("missing runtime text beyond nonce marker")
    if previous_digest:
        if not re.fullmatch(r"[0-9a-f]{64}", previous_digest):
            raise ValueError("invalid previous runtime stdout digest")
        if proof["content_sha256"] == previous_digest:
            raise ValueError("same runtime stdout digest for different prompts (nonce markers removed)")
    return f"live=true exec={result['exec']} stdout_bytes={result['stdout_bytes']}"


def main():
    proof = {"nonce": "", "nonce_echo": None, "nonce_matched": False}
    try:
        if len(sys.argv) not in (3, 4):
            raise ValueError("expected HTTP status, request nonce and optional previous content digest")
        nonce = sys.argv[2]
        proof["nonce"] = nonce
        body = json.load(sys.stdin)
        proof.update(execution_proof(body, nonce))
        proof["reason"] = assert_execution(sys.argv[1], body, nonce, sys.argv[3] if len(sys.argv) == 4 else "")
        proof["result"] = "PASS"
    except (ValueError, TypeError) as error:
        proof.update(result="FAIL", reason=str(error))
    print(json.dumps(proof))
    return 0 if proof["result"] == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())
