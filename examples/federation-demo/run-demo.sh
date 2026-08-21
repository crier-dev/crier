#!/usr/bin/env bash
#
# CR-FEAT-006 — relay-to-relay federation demo.
#
# Proves the board PASS line end-to-end with two real crier relays:
#
#   relay-1 (port 18771, CR_FED_LINKS=http://127.0.0.1:18772, CR_FED_NAME=relay-1)
#     |  POST /agents/relay-2-agent/inbox   (blocking, sender=relay-1-agent)
#     v  [agent not found locally -> federation fallback]
#   relay-2 (port 18772, no links)
#     |  webhook delivery (blocking, openai-compatible schema template)
#     v
#   echo_webhook.py (port 18773)  ->  {"choices":[{"message":{"content":"echo: ..."}}]}
#
# The blocking reply rides inside relay-2's HTTP response, which relay-1
# relays back verbatim to the original sender (curl on relay-1). Then
# GET /fed/peers on relay-1 lists BOTH relays: the local relay-1 (with its
# own agent) and the linked relay-2 (with its agent) — cross-relay agent
# discovery. relay-2's own /fed/peers lists just itself (no links — no full
# mesh required).
#
# Requirements: go, python3 (stdlib only), openssl, curl.
#   CR_AUTH_TOKEN        must NOT be set (demo runs auth-disabled)
#   CR_REQUIRE_AGENT_SIG is forced false (no per-agent signing in the demo)
#   RELAY1_PORT          override relay-1 port (default 18771)
#   RELAY2_PORT          override relay-2 port (default 18772)
#   WEBHOOK_PORT         override echo webhook port (default 18773)
#
# Output: TRANSCRIPT-<date>.md in this directory (real output, teed live).
#
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"
RELAY1_PORT="${RELAY1_PORT:-18771}"
RELAY2_PORT="${RELAY2_PORT:-18772}"
WEBHOOK_PORT="${WEBHOOK_PORT:-18773}"
RELAY1="http://127.0.0.1:${RELAY1_PORT}"
RELAY2="http://127.0.0.1:${RELAY2_PORT}"
WEBHOOK_URL="http://127.0.0.1:${WEBHOOK_PORT}/webhook"
WORKDIR="$(mktemp -d)"
TRANSCRIPT="$DEMO_DIR/TRANSCRIPT-$(date +%Y-%m-%d).md"

RELAY1_PID=""
RELAY2_PID=""
WEBHOOK_PID=""

cleanup() {
  [ -n "$RELAY1_PID" ] && kill "$RELAY1_PID" 2>/dev/null || true
  [ -n "$RELAY2_PID" ] && kill "$RELAY2_PID" 2>/dev/null || true
  [ -n "$WEBHOOK_PID" ] && kill "$WEBHOOK_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# Everything below is teed into the transcript (real output, not simulated).
exec > >(tee "$TRANSCRIPT") 2>&1

echo "# CR-FEAT-006 relay-to-relay federation demo — TRANSCRIPT"
echo
echo "- date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "- repo: $(cd "$REPO_ROOT" && git rev-parse --short HEAD) ($(cd "$REPO_ROOT" && git log -1 --format=%s))"
echo "- relay-1: ${RELAY1} (CR_FED_LINKS=http://127.0.0.1:${RELAY2_PORT}, CR_FED_NAME=relay-1)"
echo "- relay-2: ${RELAY2} (no links)"
echo "- webhook: ${WEBHOOK_URL}"
echo

echo "==> [1/8] build crier"
( cd "$REPO_ROOT" && go build -o "$WORKDIR/crier" ./cmd/server )
echo "    built $WORKDIR/crier"
echo

echo "==> [2/8] start echo webhook on :${WEBHOOK_PORT}"
python3 "$DEMO_DIR/echo_webhook.py" "$WEBHOOK_PORT" &
WEBHOOK_PID=$!
sleep 0.5
echo "    webhook pid $WEBHOOK_PID"
echo

echo "==> [3/8] start relay-2 on :${RELAY2_PORT} (no federation links)"
CRIER_PORT="$RELAY2_PORT" CR_REQUIRE_AGENT_SIG=false "$WORKDIR/crier" &
RELAY2_PID=$!
for _ in $(seq 1 50); do
  curl -sfS "$RELAY2/health" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -sS "$RELAY2/health" && echo " <- relay-2 healthy"
echo

echo "==> [4/8] start relay-1 on :${RELAY1_PORT} (linked to relay-2)"
CRIER_PORT="$RELAY1_PORT" CR_REQUIRE_AGENT_SIG=false \
  CR_FED_LINKS="http://127.0.0.1:${RELAY2_PORT}" CR_FED_NAME="relay-1" \
  "$WORKDIR/crier" &
RELAY1_PID=$!
for _ in $(seq 1 50); do
  curl -sfS "$RELAY1/health" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -sS "$RELAY1/health" && echo " <- relay-1 healthy"
echo

# ed25519 keypair for registration (public_key is required).
make_keypair() {
  openssl genpkey -algorithm ED25519 -out "$1" >/dev/null 2>&1
  openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64
}

echo "==> [5/8] register agents"
PUBKEY=$(make_keypair "$WORKDIR/relay1.key")
curl -sS -X POST "$RELAY1/agents" -H 'Content-Type: application/json' \
  -d "{\"id\":\"relay-1-agent\",\"public_key\":\"${PUBKEY}\",\"capabilities\":[\"echo\"]}" \
  -w "\n    POST relay-1/agents -> %{http_code}\n"

PUBKEY=$(make_keypair "$WORKDIR/relay2.key")
curl -sS -X POST "$RELAY2/agents" -H 'Content-Type: application/json' \
  -d "{\"id\":\"relay-2-agent\",\"public_key\":\"${PUBKEY}\",\"capabilities\":[\"echo\",\"llm\"],\"webhook\":{\"url\":\"${WEBHOOK_URL}\",\"delivery_mode\":\"blocking\",\"schema_template\":\"openai-compatible\"}}" \
  -w "\n    POST relay-2/agents -> %{http_code}\n"
echo

echo "==> [6/8] deliver from relay-1 client to relay-2 agent (blocking)"
echo "    POST ${RELAY1}/agents/relay-2-agent/inbox  (sender=relay-1-agent, session_id=sess-1, request_id=req-1)"
DELIVER=$(curl -sS -X POST "$RELAY1/agents/relay-2-agent/inbox" -H 'Content-Type: application/json' \
  -d '{"payload":{"text":"hello from relay-1"},"sender":"relay-1-agent","session_id":"sess-1","request_id":"req-1","delivery_mode":"blocking"}' \
  -w "\nHTTP_CODE:%{http_code}")
echo "    response: $(echo "$DELIVER" | grep -v 'HTTP_CODE:')"
CODE=$(echo "$DELIVER" | grep -o 'HTTP_CODE:[0-9]*' | cut -d: -f2)
echo "    HTTP code: $CODE"
[ "$CODE" = "200" ] || { echo "FAIL: expected 200 from relay-1 deliver, got $CODE" >&2; exit 1; }
echo "$DELIVER" | grep -v 'HTTP_CODE:' | grep -q '"request_id":"req-1"' \
  || { echo "FAIL: reply did not echo request_id back to the original sender" >&2; exit 1; }
echo "$DELIVER" | grep -v 'HTTP_CODE:' | grep -q 'echo: hello from relay-1' \
  || { echo "FAIL: webhook reply (echo: hello from relay-1) missing from relay-1 response" >&2; exit 1; }
echo "    PASS: reply returned to the original sender (relay-2 webhook reply relayed verbatim)"
echo

echo "==> [7/8] ghost agent — all links 404 -> 404"
curl -sS -X POST "$RELAY1/agents/ghost/inbox" -H 'Content-Type: application/json' \
  -d '{"payload":{}}' -w "\n    POST relay-1/agents/ghost/inbox -> %{http_code}\n"
echo

echo "==> [8/8] cross-relay discovery: GET /fed/peers"
echo "    relay-1 /fed/peers (must show BOTH relays with their agents):"
PEERS1=$(curl -sS "$RELAY1/fed/peers")
echo "$PEERS1" | python3 -m json.tool
echo "$PEERS1" | grep -q '"relay-1-agent"' \
  || { echo "FAIL: relay-1's local agent missing from its own /fed/peers" >&2; exit 1; }
echo "$PEERS1" | grep -q '"relay-2-agent"' \
  || { echo "FAIL: relay-2's agent missing from relay-1's /fed/peers (cross-relay discovery broken)" >&2; exit 1; }
echo "$PEERS1" | grep -q '127.0.0.1:18772\|127.0.0.1:'"$RELAY2_PORT" \
  || { echo "FAIL: linked relay-2 missing from relay-1's /fed/peers" >&2; exit 1; }
echo "    PASS: peers listing shows both relays with their agents"
echo
echo "    relay-2 /fed/peers (no links -> just itself):"
curl -sS "$RELAY2/fed/peers" | python3 -m json.tool
echo

echo "==> local agent lists (for completeness)"
echo "    relay-1 /agents: $(curl -sS "$RELAY1/agents")"
echo "    relay-2 /agents: $(curl -sS "$RELAY2/agents")"
echo
echo "==> DEMO PASS: relay-1 -> relay-2 delivery via webhook, reply to sender, peers listing shows both"
