#!/usr/bin/env bash
# OPS-CRIER-001: fleet bus liveness probe.
#
# Probes the crier fleet bus health endpoint. Exits 0 ONLY on HTTP 200 within
# the timeout; curl failures, non-200 codes, and timeouts all exit 1. Never
# hangs longer than ~6s wall clock (curl hard timeout, default 5s).
#
# Env overrides:
#   BUS_ALIVE_URL     default http://127.0.0.1:8767/health
#   BUS_ALIVE_TIMEOUT default 5 (seconds)

URL="${BUS_ALIVE_URL:-http://127.0.0.1:8767/health}"
TIMEOUT="${BUS_ALIVE_TIMEOUT:-5}"

code=$(curl -sS -o /dev/null -w '%{http_code}' -m "$TIMEOUT" "$URL")
rc=$?
if [ "$rc" -ne 0 ]; then
    # curl transport failure: connection refused, timeout, DNS, TLS, etc.
    echo "bus-alive: FAIL curl rc=${rc} code=${code:-none} url=${URL}" >&2
    exit 1
fi
if [ "$code" != "200" ]; then
    echo "bus-alive: FAIL http_code=${code} url=${URL}" >&2
    exit 1
fi
echo "bus-alive: OK http_code=200 url=${URL}"
exit 0
