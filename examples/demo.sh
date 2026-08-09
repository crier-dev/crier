#!/usr/bin/env bash
#
# Crier integration demo — register → deliver → signed retrieve → ack round-trip.
#
# Requirements:
#   - a running crier server (default http://localhost:8767, override with CRIER_URL)
#   - openssl 1.1.1+ (ed25519 support) and xxd on PATH
#
# Run the server first:
#   make run                                  # default port 8767, in-memory backend
#   # or with signing disabled for a quick look:
#   CR_REQUIRE_AGENT_SIG=false make run
#
# Usage:
#   ./examples/demo.sh
#
# The inbox endpoints require per-agent ed25519 request signatures by default
# (CR_REQUIRE_AGENT_SIG=true). This script generates an ephemeral keypair,
# registers the agent, and signs every retrieve/ack request with it:
#   X-Agent-Sig = hex(ed25519_sign("<METHOD>\n<path>\n<unix-seconds>", privkey))
# Timestamps must be within ±30s of the server clock.
#
set -euo pipefail

CRIER_URL="${CRIER_URL:-http://localhost:8767}"
AGENT_ID="demo-$(date +%s)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# If the server was started with CR_AUTH_TOKEN set (auth enabled), every request
# except /health must carry the Bearer header below. If CR_AUTH_TOKEN is unset,
# auth is disabled and the header is omitted. Export the same token you started
# the server with:
#   CR_AUTH_TOKEN=secret ./examples/demo.sh
AUTH_ARGS=()
if [ -n "${CR_AUTH_TOKEN:-}" ]; then
  AUTH_ARGS=(-H "Authorization: Bearer ${CR_AUTH_TOKEN}")
fi

if ! command -v openssl >/dev/null 2>&1; then
  echo "ERROR: openssl required (ed25519 keygen/signing)" >&2
  exit 1
fi
if ! command -v xxd >/dev/null 2>&1; then
  echo "ERROR: xxd required (hex encoding)" >&2
  exit 1
fi

echo "==> crier demo against ${CRIER_URL} (agent ${AGENT_ID})"

# 0. Health check
echo "==> [1/6] health"
curl -sS -o /dev/null -w "    GET /health -> %{http_code}\n" "${AUTH_ARGS[@]}" "${CRIER_URL}/health"
curl -sfS "${AUTH_ARGS[@]}" "${CRIER_URL}/health" >/dev/null || {
  echo "ERROR: crier not reachable at ${CRIER_URL} — start it first (make run)" >&2
  exit 1
}

# 1. Generate an ed25519 keypair
echo "==> [2/6] generating ed25519 keypair"
openssl genpkey -algorithm ED25519 -out "${WORKDIR}/agent.key" >/dev/null 2>&1
PUBKEY_HEX=$(openssl pkey -in "${WORKDIR}/agent.key" -pubout -outform DER 2>/dev/null \
  | tail -c 32 | xxd -p -c 64)

# 2. Register the agent
echo "==> [3/6] register agent"
curl -sS -X POST "${AUTH_ARGS[@]}" "${CRIER_URL}/agents" -H 'Content-Type: application/json' \
  -d "{\"id\":\"${AGENT_ID}\",\"public_key\":\"${PUBKEY_HEX}\",\"capabilities\":[\"demo\"]}" \
  -w "\n    POST /agents -> %{http_code}\n"

# 3. Deliver a message to its inbox
echo "==> [4/6] deliver message"
curl -sS -X POST "${AUTH_ARGS[@]}" "${CRIER_URL}/agents/${AGENT_ID}/inbox" -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world","n":42}}' \
  -w "\n    POST /agents/${AGENT_ID}/inbox -> %{http_code}\n"

# 4. Signed retrieve
echo "==> [5/6] signed retrieve"
TS=$(date +%s)
printf 'GET\n/agents/%s/inbox\n%s' "${AGENT_ID}" "$TS" > "${WORKDIR}/payload.txt"
SIG=$(openssl pkeyutl -sign -rawin -inkey "${WORKDIR}/agent.key" -in "${WORKDIR}/payload.txt" 2>/dev/null \
  | xxd -p -c 128)
RETRIEVE=$(curl -sS "${AUTH_ARGS[@]}" "${CRIER_URL}/agents/${AGENT_ID}/inbox" \
  -H "X-Agent-ID: ${AGENT_ID}" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}" \
  -w "\nHTTP_CODE:%{http_code}")
echo "    GET /agents/${AGENT_ID}/inbox -> $(echo "${RETRIEVE}" | grep -o 'HTTP_CODE:[0-9]*')"
echo "    response: $(echo "${RETRIEVE}" | grep -v 'HTTP_CODE:')"
MESSAGE_COUNT=$(echo "${RETRIEVE}" | grep -v 'HTTP_CODE:' | grep -o '"id"' | wc -l | tr -d ' ')
[ "${MESSAGE_COUNT}" -ge 1 ] || { echo "ERROR: expected >=1 message, got ${MESSAGE_COUNT}" >&2; exit 1; }

# 5. Ack the message (signed — lease_id AND message_ids from the retrieve response;
#    message_ids is required — a lease-only ack is rejected with 400)
echo "==> [6/6] signed ack (lease_id + message_ids)"
LEASE_ID=$(echo "${RETRIEVE}" | grep -v 'HTTP_CODE:' | grep -o '"lease_id":"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "${LEASE_ID}" ] || { echo "ERROR: no lease_id in retrieve response" >&2; exit 1; }
MESSAGE_ID=$(echo "${RETRIEVE}" | grep -v 'HTTP_CODE:' | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "${MESSAGE_ID}" ] || { echo "ERROR: no message id in retrieve response" >&2; exit 1; }
TS=$(date +%s)
printf 'POST\n/agents/%s/inbox/ack\n%s' "${AGENT_ID}" "$TS" > "${WORKDIR}/payload.txt"
SIG=$(openssl pkeyutl -sign -rawin -inkey "${WORKDIR}/agent.key" -in "${WORKDIR}/payload.txt" 2>/dev/null \
  | xxd -p -c 128)
curl -sS -X POST "${AUTH_ARGS[@]}" "${CRIER_URL}/agents/${AGENT_ID}/inbox/ack" -H 'Content-Type: application/json' \
  -H "X-Agent-ID: ${AGENT_ID}" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}" \
  -d "{\"lease_id\":\"${LEASE_ID}\",\"message_ids\":[\"${MESSAGE_ID}\"]}" \
  -w "\n    POST /agents/${AGENT_ID}/inbox/ack -> %{http_code}\n"

# 6. Verify the inbox is now empty (signed retrieve again)
echo "==> verify empty after ack"
TS=$(date +%s)
printf 'GET\n/agents/%s/inbox\n%s' "${AGENT_ID}" "$TS" > "${WORKDIR}/payload.txt"
SIG=$(openssl pkeyutl -sign -rawin -inkey "${WORKDIR}/agent.key" -in "${WORKDIR}/payload.txt" 2>/dev/null \
  | xxd -p -c 128)
EMPTY=$(curl -sS "${AUTH_ARGS[@]}" "${CRIER_URL}/agents/${AGENT_ID}/inbox" \
  -H "X-Agent-ID: ${AGENT_ID}" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}" \
  -w "\nHTTP_CODE:%{http_code}")
echo "    response: $(echo "${EMPTY}" | grep -v 'HTTP_CODE:')"
echo
echo "==> demo round-trip complete: register -> deliver -> signed retrieve -> signed ack"
