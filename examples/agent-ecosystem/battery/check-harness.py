#!/usr/bin/env python3
"""Offline execution binding. Body metadata alone is never a runtime observation.

The verifier hashes capture-file bytes independently and binds the body to them.
The HTTP observer is still served by the consumer: this is not hostile-consumer
attestation. Only the trusted shipped observer tees actual child stdout pipes.

CLI: HTTP NONCE PREVIOUS_DIGEST REQUEST_JSON CAPTURE_FILE NONCE_LEDGER
The ledger is verifier-owned {minted: [...], used: [...]}; a nonce is single use.
"""
import hashlib
import json
from pathlib import Path
import re
import sys
import uuid

MARKER = re.compile(r"\[crier-battery-nonce:([0-9a-f-]{36})\]")
DIGEST_MARKER = re.compile(r"\[crier-battery-request-sha256:([0-9a-f]{64})\]")


# ECMAScript String.trim, matching liveReply without Python's wider whitespace
# set (e.g. Python strips U+0085 but JavaScript does not). Raw hashing is FIRST.
JS_WHITESPACE = "\u0009\u000a\u000b\u000c\u000d\u0020\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"


def capture_bytes(output):
    # Text callers remain useful for offline fixtures; production passes bytes.
    return output.encode("utf-8") if isinstance(output, str) else output


def request_digest(request):
    payload = request["payload"]
    canonical = json.dumps({"battery_nonce": payload["battery_nonce"], "text": payload["text"]},
                           ensure_ascii=False, separators=(",", ":"))
    return hashlib.sha256(canonical.encode()).hexdigest()


def execution_proof(body, nonce):
    result = body.get("reply") if isinstance(body, dict) else None
    if not isinstance(result, dict):
        result = body if isinstance(body, dict) else {}
    text = result.get("reply")
    text = text if isinstance(text, str) else ""
    content = DIGEST_MARKER.sub("", MARKER.sub("", text)).strip()
    return {
        "nonce": nonce, "nonce_echo": result.get("nonce"),
        "nonce_matched": bool(nonce) and result.get("nonce") == nonce and MARKER.findall(text) == [nonce],
        "stdout_sha256": hashlib.sha256(text.encode()).hexdigest(),
        "content_sha256": hashlib.sha256(content.encode()).hexdigest(),
        "runtime_content_bytes": len(content.encode()),
    }


def assert_execution(status, body, nonce, previous_digest="", *, request=None,
                     runtime_output="", minted_nonces=(), used_nonces=None):
    result = body.get("reply") if isinstance(body, dict) else None
    result = result if isinstance(result, dict) else body
    detail = ""
    if isinstance(result, dict):
        reason = result.get("reason") or result.get("error")
        if isinstance(reason, str) and reason:
            prefix = f"{result['status']}: " if result.get("status") in ("REFUSED", "FAILED", "TIMEOUT") else ""
            detail = f"; {prefix}{reason}"
    if str(uuid.UUID(nonce)) != nonce:
        raise ValueError("invalid request nonce")
    if used_nonces is None:
        used_nonces = set()
    if nonce not in minted_nonces or nonce in used_nonces:
        raise ValueError("NONCE_REPLAY: nonce was not minted for this run or was already consumed")
    # Consume even failed attempts; retries need a new challenge.
    used_nonces.add(nonce)
    if status != "200":
        raise ValueError(f"HTTP {status}, want 200{detail}")
    if not isinstance(body, dict) or "error" in body:
        raise ValueError(f"invalid response or delivery error{detail}")
    result = body["reply"] if isinstance(body.get("reply"), dict) else body
    if result.get("status") in ("FAILED", "REFUSED", "TIMEOUT"):
        raise ValueError(f"harness did not succeed{detail}")
    if "error" in result:
        raise ValueError(f"harness error, not a reply{detail}")
    if result.get("live") is False:
        raise ValueError(f"live=false (canned/non-executed harness){detail}")
    text = result.get("reply") if isinstance(result.get("reply"), str) else ""
    echoes = MARKER.findall(text)
    if any(echo != nonce for echo in echoes) or (result.get("nonce") and result["nonce"] != nonce):
        raise ValueError("NONCE_REPLAY: response contains a nonce other than this request's fresh nonce")
    captured = capture_bytes(runtime_output)
    # Inspect the capture too: a fresh body must not relabel an old pipe capture.
    observed_nonces = MARKER.findall(captured.decode("utf-8", errors="replace"))
    if any(observed != nonce for observed in observed_nonces):
        raise ValueError("NONCE_REPLAY: captured runtime output contains another request's nonce")
    if nonce not in observed_nonces:
        raise ValueError("NOT_EXECUTED: no current nonce in captured runtime stdout")
    proof = execution_proof(body, nonce)
    if not proof["nonce_matched"]:
        raise ValueError("ECHOED_WITHOUT_RUN: captured runtime nonce but body lacks the exact nonce echo")
    if not request or request.get("payload", {}).get("battery_nonce") != nonce:
        raise ValueError("DIGEST_MISMATCH: missing verifier-owned request or request nonce mismatch")
    expected = request_digest(request)
    if result.get("request_sha256") != expected or DIGEST_MARKER.findall(text) != [expected]:
        raise ValueError("DIGEST_MISMATCH: body request digest differs from verifier-sent payload")
    if result.get("live") is not True or result.get("canned", False) is not False:
        raise ValueError("missing/invalid live=true execution marker")
    if not isinstance(result.get("exec"), str) or not result["exec"].strip():
        raise ValueError("missing resolved exec/runtime")
    if type(result.get("stdout_bytes")) is not int or result["stdout_bytes"] <= 0:
        raise ValueError("missing/invalid positive stdout_bytes")
    if result.get("stdout_sha256") != proof["stdout_sha256"]:
        raise ValueError("DIGEST_MISMATCH: missing/mismatched stdout_sha256 (trimmed runtime text)")
    # Hash the ORIGINAL file bytes, before decoding, trimming or newline changes.
    # Keep stdout_sha256's existing trimmed-reply meaning; the additive raw field
    # binds stdout_bytes and the reply to the separate stdout-only capture.
    captured_digest = hashlib.sha256(captured).hexdigest()
    if (result.get("stdout_raw_sha256") != captured_digest
            or result["stdout_bytes"] != len(captured)):
        raise ValueError("DIGEST_MISMATCH: raw captured stdout digest/byte count differs from body")
    try:
        captured_text = captured.decode("utf-8").strip(JS_WHITESPACE)
    except UnicodeDecodeError as error:
        raise ValueError("DIGEST_MISMATCH: captured stdout is not valid UTF-8") from error
    if (captured_text != text or len(observed_nonces) != 1
            or hashlib.sha256(captured_text.encode()).hexdigest() != proof["stdout_sha256"]):
        raise ValueError("DIGEST_MISMATCH: captured stdout differs from delivery reply")
    if not proof["runtime_content_bytes"]:
        raise ValueError("missing runtime text beyond nonce marker")
    if previous_digest:
        if not re.fullmatch(r"[0-9a-f]{64}", previous_digest):
            raise ValueError("invalid previous runtime stdout digest")
        if proof["content_sha256"] == previous_digest:
            raise ValueError("same runtime stdout digest for different prompts (nonce markers removed)")
    return f"live=true exec={result['exec']} stdout_bytes={result['stdout_bytes']}"


def verdict(status, body, nonce, previous_digest="", **evidence):
    proof = execution_proof(body, nonce)
    output = capture_bytes(evidence.get("runtime_output", b""))
    proof.update(runtime_nonce_observed=f"[crier-battery-nonce:{nonce}]".encode() in output,
                 runtime_output=output.decode("utf-8", errors="replace"),
                 captured_runtime_bytes=len(output),
                 captured_runtime_sha256=hashlib.sha256(output).hexdigest())
    if evidence.get("request"):
        proof["expected_request_sha256"] = request_digest(evidence["request"])
    try:
        proof["reason"] = assert_execution(status, body, nonce, previous_digest, **evidence)
        proof.update(result="PASS", outcome="SUPPORTED")
    except (ValueError, TypeError) as error:
        reason = str(error)
        outcome = next((name for name in ("NONCE_REPLAY", "NOT_EXECUTED", "ECHOED_WITHOUT_RUN", "DIGEST_MISMATCH")
                        if reason.startswith(name + ":")), "FAILED")
        result = body.get("reply") if isinstance(body, dict) else None
        result = result if isinstance(result, dict) else body
        if outcome == "FAILED" and isinstance(result, dict) and result.get("status") in ("REFUSED", "TIMEOUT", "FAILED"):
            outcome = result["status"]
        proof.update(result="FAIL", outcome=outcome, reason=reason)
    return proof


def main():
    try:
        nonce = sys.argv[2]
        body = json.load(sys.stdin)
        evidence = {}
        ledger = None
        if len(sys.argv) == 7:
            evidence["request"] = json.loads(Path(sys.argv[4]).read_text())
            evidence["runtime_output"] = Path(sys.argv[5]).read_bytes()
            ledger = json.loads(Path(sys.argv[6]).read_text())
            evidence["minted_nonces"] = ledger["minted"]
            evidence["used_nonces"] = set(ledger["used"])
        proof = verdict(sys.argv[1], body, nonce, sys.argv[3] if len(sys.argv) >= 4 else "", **evidence)
        if ledger is not None:
            ledger["used"] = sorted(evidence["used_nonces"])
            Path(sys.argv[6]).write_text(json.dumps(ledger))
    except (ValueError, TypeError, IndexError, OSError) as error:
        proof = {"result": "FAIL", "outcome": "FAILED", "reason": str(error)}
    print(json.dumps(proof))
    return 0 if proof["result"] == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())
