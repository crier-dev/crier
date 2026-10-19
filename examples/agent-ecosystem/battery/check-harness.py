#!/usr/bin/env python3
"""Fail-closed execution assertion, shared by the battery and offline fixtures.

Usage: python3 check-harness.py HTTP_STATUS < response.json
Accepts a consumer body or crier's generic/raw {"reply": <consumer body>}.
"""

import json
import sys


def assert_execution(status, body):
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
    return f"live=true exec={result['exec']} stdout_bytes={result['stdout_bytes']}"


def main():
    try:
        if len(sys.argv) != 2:
            raise ValueError("expected HTTP status argument")
        print(assert_execution(sys.argv[1], json.load(sys.stdin)))
    except (ValueError, TypeError) as error:
        print(str(error))
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
