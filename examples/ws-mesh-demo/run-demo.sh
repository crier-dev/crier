#!/usr/bin/env bash
#
# CR-GAP-050 — no-install WebSocket subscribe + mesh demo.
#
# Proves live end-to-end against one real crier relay:
#
#   subscriber   ws://127.0.0.1:18767/relay/subscribe/demo   (-once)
#     ^
#     |  POST /relay/publish  {"topic":"demo","event":{"msg":"hello from run-demo",...}}
#     |  (X-Agent-ID: demo-publisher — required when rate limiting is on)
#   peer A       ws://127.0.0.1:18767/mesh/connect/demo-agent-a
#   peer B       ws://127.0.0.1:18767/mesh/connect/demo-agent-b
#     -> GET /mesh/peers returns count 2 with both agent IDs
#
# Requirements: go (toolchain only) + curl. gorilla/websocket v1.5.3 comes
# from go.mod — zero external installs, zero new dependencies.
#   CR_AUTH_TOKEN        must NOT be set (demo runs auth-disabled)
#   CR_REQUIRE_AGENT_SIG forced false (no per-agent signing in the demo)
#   DEMO_PORT            override relay port (default 18767)
#
# Output: TRANSCRIPT-<date>.md in this directory (real output, teed live).
#
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"
DEMO_PORT="${DEMO_PORT:-18767}"
BASE="http://127.0.0.1:${DEMO_PORT}"
WORKDIR="$(mktemp -d)"
TRANSCRIPT="$DEMO_DIR/TRANSCRIPT-$(date +%Y-%m-%d).md"

SERVER_PID=""
SUB_PID=""
PEER_A_PID=""
PEER_B_PID=""

cleanup() {
  [ -n "$SUB_PID" ] && kill "$SUB_PID" 2>/dev/null || true
  [ -n "$PEER_A_PID" ] && kill "$PEER_A_PID" 2>/dev/null || true
  [ -n "$PEER_B_PID" ] && kill "$PEER_B_PID" 2>/dev/null || true
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# Everything below is teed into the transcript (real output, not simulated).
exec > >(tee "$TRANSCRIPT") 2>&1

echo "# CR-GAP-050 WS subscribe + mesh demo — TRANSCRIPT"
echo
echo "- date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "- repo: $(cd "$REPO_ROOT" && git rev-parse --short HEAD) ($(cd "$REPO_ROOT" && git log -1 --format=%s))"
echo "- relay: ${BASE} (auth-disabled)"
echo

echo "==> [1/7] build crier + demo client (go toolchain only)"
( cd "$REPO_ROOT" && go build -o "$WORKDIR/crier" ./cmd/server )
( cd "$REPO_ROOT" && go build -o "$WORKDIR/ws-mesh-demo" ./examples/ws-mesh-demo )
echo "    built $WORKDIR/crier and $WORKDIR/ws-mesh-demo"
echo

echo "==> [2/7] start relay on :${DEMO_PORT} (auth-disabled)"
env -u CR_AUTH_TOKEN CR_AUTH_TOKEN= CR_REQUIRE_AGENT_SIG=false \
  CRIER_PORT="$DEMO_PORT" CR_LOG_LEVEL=warn "$WORKDIR/crier" &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -sfS "$BASE/health" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -sS "$BASE/health" && echo " <- relay healthy"
echo

echo "==> [3/7] spawn subscriber on /relay/subscribe/demo"
"$WORKDIR/ws-mesh-demo" subscribe -topic demo -once > "$WORKDIR/sub.out" 2>&1 &
SUB_PID=$!
for _ in $(seq 1 50); do
  grep -q '^SUBSCRIBED demo' "$WORKDIR/sub.out" 2>/dev/null && break
  sleep 0.2
done
grep -q '^SUBSCRIBED demo' "$WORKDIR/sub.out" \
  || { echo "FAIL: subscriber did not reach SUBSCRIBED state" >&2; exit 1; }
echo "    subscriber ready (pid $SUB_PID)"
echo

echo "==> [4/7] spawn two mesh peers"
"$WORKDIR/ws-mesh-demo" peer -agent demo-agent-a > "$WORKDIR/peer-a.out" 2>&1 &
PEER_A_PID=$!
"$WORKDIR/ws-mesh-demo" peer -agent demo-agent-b > "$WORKDIR/peer-b.out" 2>&1 &
PEER_B_PID=$!
for _ in $(seq 1 50); do
  grep -q '^PEER CONNECTED demo-agent-a' "$WORKDIR/peer-a.out" 2>/dev/null \
    && grep -q '^PEER CONNECTED demo-agent-b' "$WORKDIR/peer-b.out" 2>/dev/null && break
  sleep 0.2
done
grep -q '^PEER CONNECTED demo-agent-a' "$WORKDIR/peer-a.out" \
  || { echo "FAIL: peer A (demo-agent-a) did not connect" >&2; exit 1; }
grep -q '^PEER CONNECTED demo-agent-b' "$WORKDIR/peer-b.out" \
  || { echo "FAIL: peer B (demo-agent-b) did not connect" >&2; exit 1; }
echo "    demo-agent-a and demo-agent-b connected"
echo

echo "==> [5/7] GET /mesh/peers (must show count 2 with both agent IDs)"
PEERS=""
for _ in $(seq 1 50); do
  PEERS=$(curl -sS "$BASE/mesh/peers")
  echo "$PEERS" | grep -q '"agent_id":"demo-agent-a"' \
    && echo "$PEERS" | grep -q '"agent_id":"demo-agent-b"' \
    && echo "$PEERS" | grep -q '"count":2' && break
  sleep 0.2
done
echo "$PEERS" | grep -q '"agent_id":"demo-agent-a"' \
  || { echo "FAIL: demo-agent-a missing from /mesh/peers" >&2; exit 1; }
echo "$PEERS" | grep -q '"agent_id":"demo-agent-b"' \
  || { echo "FAIL: demo-agent-b missing from /mesh/peers" >&2; exit 1; }
echo "$PEERS" | grep -q '"count":2' \
  || { echo "FAIL: /mesh/peers count != 2 (got: $PEERS)" >&2; exit 1; }
echo "    $PEERS"
echo "    PASS: both mesh peers registered, count 2"
echo

echo "==> [6/7] publish an event (X-Agent-ID: demo-publisher)"
PUB=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/relay/publish" \
  -H 'Content-Type: application/json' -H 'X-Agent-ID: demo-publisher' \
  -d '{"topic":"demo","event":{"msg":"hello from run-demo","ts":"crier-demo"}}')
echo "    POST /relay/publish -> HTTP $PUB"
[ "$PUB" = "202" ] || { echo "FAIL: expected 202 from publish, got $PUB" >&2; exit 1; }
echo

echo "==> [7/7] subscriber must receive the event over WS /relay/subscribe"
wait "$SUB_PID" || { echo "FAIL: subscriber exited non-zero" >&2; exit 1; }
grep -q 'hello from run-demo' "$WORKDIR/sub.out" \
  || { echo "FAIL: subscriber did not receive the published event" >&2; exit 1; }
echo "    subscriber received: $(grep '^EVENT ' "$WORKDIR/sub.out")"
echo "    PASS: event fanned out to subscriber over WebSocket"
echo

echo "==> DEMO PASS: WS subscribe fan-out + mesh peer registration verified live"
