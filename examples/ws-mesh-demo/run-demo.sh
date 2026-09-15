#!/usr/bin/env bash
#
# CR-GAP-050 / DF-CRIER-152 — no-install WebSocket subscribe + mesh demo.
#
# Proves live end-to-end against ONE real crier relay that this script starts
# itself on 127.0.0.1:${DEMO_PORT} (scratch port; override with DEMO_PORT):
#
#   [3/8] POST /agents {"id":"demo-agent-a","capabilities":[...],"public_key":"<64-hex>"} -> 201
#         POST /agents {"id":"demo-agent-b", ...}                                         -> 201
#   [4/8] subscriber   ws://127.0.0.1:<port>/relay/subscribe/demo   (-once)
#           ^
#           |  POST /relay/publish  {"topic":"demo","event":{"msg":"hello from run-demo",...}}
#           |  (X-Agent-ID: demo-publisher — required when rate limiting is on)
#   [5/8] peer A       ws://127.0.0.1:<port>/mesh/connect/demo-agent-a
#         peer B       ws://127.0.0.1:<port>/mesh/connect/demo-agent-b
#           -> GET /mesh/peers returns count 2 with both agent IDs
#
# Every demo client is spawned with `-url "$BASE"`, so DEMO_PORT moves the
# server and the clients together. (Regression DF-CRIER-152: the clients used to
# be spawned without -url and silently fell back to the compiled-in default, so
# a DEMO_PORT run measured whatever else owned that default port.)
#
# Two guards keep the run honest:
#   * the script ABORTS if anything already listens on $DEMO_PORT, and
#   * the relay's peer list must be EMPTY right after startup — a foreign server
#     answering on the same port (docker-published crier, stale demo run, …)
#     would already list peers, and would otherwise be measured by mistake.
#
# Requirements: go (toolchain only) + curl + sha256sum (coreutils, used to derive
# the registry public_key). gorilla/websocket v1.5.3 comes from go.mod — zero
# external installs, zero new dependencies.
#   CR_AUTH_TOKEN        must NOT be set (demo runs auth-disabled)
#   CR_REQUIRE_AGENT_SIG forced false (no per-agent signing in the demo)
#   DEMO_PORT            override relay port (default 18961 — a scratch port)
#   DEMO_TRANSCRIPT      override transcript path (default below)
#
# Output: the transcript is teed OUTSIDE the repo, to
#   ${TMPDIR:-/tmp}/ws-mesh-demo-TRANSCRIPT-<date>.XXXXXX.md
# and its path is printed at the end of the run. New runs never write into the
# repo (the historical, tracked TRANSCRIPT-2026-08-31.md stays untouched).
#
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"
DEMO_PORT="${DEMO_PORT:-18961}"
BASE="http://127.0.0.1:${DEMO_PORT}"
WORKDIR="$(mktemp -d)"

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

usage() {
  cat <<EOF
ws-mesh-demo run-demo.sh — crier relay pub/sub + mesh demo (CR-GAP-050)

Usage:
  bash run-demo.sh [-h|--help]

What it does — one crier relay started by this script on 127.0.0.1:${DEMO_PORT}
(override the port: DEMO_PORT=<free port> bash run-demo.sh):
  [1/8] build ./cmd/server and ./examples/ws-mesh-demo into a mktemp dir
  [2/8] abort if anything already listens on :${DEMO_PORT}, then start the relay
        auth-disabled and verify it answers /health with an EMPTY peer list
  [3/8] HTTP-register demo-agent-a and demo-agent-b: POST /agents -> 201, so
        both peers exist in the agent registry BEFORE they join the mesh
  [4/8] spawn the subscriber on /relay/subscribe/demo
  [5/8] spawn the two mesh peers on /mesh/connect/<agent>   (each with -url \$BASE)
  [6/8] assert GET /mesh/peers reports count 2 listing both agent ids
  [7/8] publish one event to topic demo (POST /relay/publish)
  [8/8] assert the subscriber received that event over WebSocket

Notes:
  * registration in [3/8] is done by this demo so each peer is a known agent as
    well as a mesh connection; /mesh/peers itself reports live WebSocket
    connections (a peer that never registered still shows up there).
  * every demo client is passed -url \$BASE, so DEMO_PORT moves the server and
    the clients together.
  * transcript (live tee, written outside the repo):
      \${TMPDIR:-/tmp}/ws-mesh-demo-TRANSCRIPT-<date>.XXXXXX.md
    override with DEMO_TRANSCRIPT=/path/to/file; the path is printed at the end.

Exit status: 0 only when every step PASSes.
EOF
}

case "${1:-}" in
  -h|--help)
    usage
    exit 0
    ;;
  "")
    ;;
  *)
    echo "run-demo.sh: unknown argument '$1'" >&2
    usage >&2
    exit 2
    ;;
esac

# ── Pre-flight (before the transcript redirect, so aborts are plainly visible) ──
for tool in go curl sha256sum; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "FAIL: '$tool' is required on PATH" >&2; exit 1; }
done

case "$DEMO_PORT" in
  ''|*[!0-9]*)
    echo "FAIL: DEMO_PORT must be a TCP port number (got '$DEMO_PORT')" >&2
    exit 2
    ;;
esac
[ "$DEMO_PORT" -ge 1 ] && [ "$DEMO_PORT" -le 65535 ] \
  || { echo "FAIL: DEMO_PORT out of range: $DEMO_PORT" >&2; exit 2; }

port_in_use() {
  local port="$1"
  if command -v ss >/dev/null 2>&1; then
    ss -tln 2>/dev/null | awk -v suffix=":$port" '$4 ~ suffix "$" { found = 1 } END { exit !found }'
    return $?
  fi
  (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null && return 0
  return 1
}

if port_in_use "$DEMO_PORT"; then
  cat >&2 <<EOF
FAIL: something is already listening on :${DEMO_PORT} — refusing to run.
      The demo would silently measure that server instead of its own.
      Pick a free port:  DEMO_PORT=<free port> bash $0
EOF
  exit 1
fi

# ── Transcript: outside the repo, mktemp-derived, never overwrites a previous run ──
TRANSCRIPT_DIR="${TMPDIR:-/tmp}"
if [ -n "${DEMO_TRANSCRIPT:-}" ]; then
  TRANSCRIPT="$DEMO_TRANSCRIPT"
  mkdir -p "$(dirname "$TRANSCRIPT")"
  : > "$TRANSCRIPT"
else
  TRANSCRIPT="$(mktemp "$TRANSCRIPT_DIR/ws-mesh-demo-TRANSCRIPT-$(date +%Y-%m-%d).XXXXXX.md")"
fi

# Everything below is teed into the transcript (real output, not simulated).
exec > >(tee "$TRANSCRIPT") 2>&1

echo "# CR-GAP-050 WS subscribe + mesh demo — TRANSCRIPT"
echo
echo "- date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "- repo: $(cd "$REPO_ROOT" && git rev-parse --short HEAD) ($(cd "$REPO_ROOT" && git log -1 --format=%s))"
echo "- relay: ${BASE} (auth-disabled)"
echo "- transcript: ${TRANSCRIPT}"
echo

echo "==> [1/8] build crier + demo client (go toolchain only)"
( cd "$REPO_ROOT" && go build -o "$WORKDIR/crier" ./cmd/server )
( cd "$REPO_ROOT" && go build -o "$WORKDIR/ws-mesh-demo" ./examples/ws-mesh-demo )
echo "    built $WORKDIR/crier and $WORKDIR/ws-mesh-demo"
echo

echo "==> [2/8] start relay on :${DEMO_PORT} (auth-disabled)"
echo "    port :${DEMO_PORT} was free before startup (checked with port_in_use)"
env -u CR_AUTH_TOKEN CR_AUTH_TOKEN= CR_REQUIRE_AGENT_SIG=false \
  CRIER_PORT="$DEMO_PORT" CR_LOG_LEVEL=warn "$WORKDIR/crier" &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -sfS "$BASE/health" >/dev/null 2>&1 && break
  kill -0 "$SERVER_PID" 2>/dev/null \
    || { echo "FAIL: relay exited during startup (pid $SERVER_PID)" >&2; exit 1; }
  sleep 0.2
done
curl -sfS "$BASE/health" >/dev/null 2>&1 \
  || { echo "FAIL: relay never became healthy at $BASE" >&2; exit 1; }
echo "    $(curl -sS "$BASE/health") <- relay healthy (our pid $SERVER_PID)"
PEERS0=$(curl -sS "$BASE/mesh/peers")
echo "$PEERS0" | grep -q '"count":0' \
  || { echo "FAIL: $BASE/mesh/peers is not empty at startup (got: $PEERS0) — this is not our relay" >&2; exit 1; }
echo "    GET /mesh/peers -> $PEERS0 (empty: measured server is the one we started)"
echo

echo "==> [3/8] HTTP-register demo-agent-a and demo-agent-b (POST /agents)"
for AGENT in demo-agent-a demo-agent-b; do
  # 64-hex ed25519-style public key derived from the id — deterministic, no keys
  # to manage, and it satisfies the registry's 64-hex validation.
  PUBKEY=$(printf '%s' "$AGENT" | sha256sum | cut -c1-64)
  REG_CODE=$(curl -sS -o "$WORKDIR/register-${AGENT}.json" -w '%{http_code}' \
    -X POST "$BASE/agents" -H 'Content-Type: application/json' \
    -d "{\"id\":\"${AGENT}\",\"capabilities\":[\"demo\",\"mesh\"],\"public_key\":\"${PUBKEY}\"}")
  echo "    POST /agents ${AGENT} -> HTTP ${REG_CODE} $(cat "$WORKDIR/register-${AGENT}.json")"
  [ "$REG_CODE" = "201" ] \
    || { echo "FAIL: registering ${AGENT} returned HTTP ${REG_CODE} (expected 201)" >&2; exit 1; }
done
REGISTERED=$(curl -sS "$BASE/agents")
for AGENT in demo-agent-a demo-agent-b; do
  echo "$REGISTERED" | grep -q "\"id\":\"${AGENT}\"" \
    || { echo "FAIL: ${AGENT} missing from GET /agents (got: $REGISTERED)" >&2; exit 1; }
done
echo "    GET /agents -> $(echo "$REGISTERED" | grep -o '"id":"[^"]*"' | paste -sd' ' -)"
echo "    demo-agent-a and demo-agent-b are registered before any mesh connection"
echo

echo "==> [4/8] spawn subscriber on /relay/subscribe/demo (-url $BASE)"
"$WORKDIR/ws-mesh-demo" -url "$BASE" subscribe -topic demo -once > "$WORKDIR/sub.out" 2>&1 &
SUB_PID=$!
for _ in $(seq 1 50); do
  grep -q '^SUBSCRIBED demo' "$WORKDIR/sub.out" 2>/dev/null && break
  sleep 0.2
done
grep -q '^SUBSCRIBED demo' "$WORKDIR/sub.out" \
  || { echo "FAIL: subscriber did not reach SUBSCRIBED state ($(cat "$WORKDIR/sub.out"))" >&2; exit 1; }
echo "    subscriber ready (pid $SUB_PID)"
echo

echo "==> [5/8] spawn two mesh peers (-url $BASE, agents from step 3)"
"$WORKDIR/ws-mesh-demo" -url "$BASE" peer -agent demo-agent-a > "$WORKDIR/peer-a.out" 2>&1 &
PEER_A_PID=$!
"$WORKDIR/ws-mesh-demo" -url "$BASE" peer -agent demo-agent-b > "$WORKDIR/peer-b.out" 2>&1 &
PEER_B_PID=$!
for _ in $(seq 1 50); do
  grep -q '^PEER CONNECTED demo-agent-a' "$WORKDIR/peer-a.out" 2>/dev/null \
    && grep -q '^PEER CONNECTED demo-agent-b' "$WORKDIR/peer-b.out" 2>/dev/null && break
  sleep 0.2
done
grep -q '^PEER CONNECTED demo-agent-a' "$WORKDIR/peer-a.out" \
  || { echo "FAIL: peer A (demo-agent-a) did not connect ($(cat "$WORKDIR/peer-a.out"))" >&2; exit 1; }
grep -q '^PEER CONNECTED demo-agent-b' "$WORKDIR/peer-b.out" \
  || { echo "FAIL: peer B (demo-agent-b) did not connect ($(cat "$WORKDIR/peer-b.out"))" >&2; exit 1; }
echo "    demo-agent-a and demo-agent-b connected to $BASE"
echo

echo "==> [6/8] GET /mesh/peers (must show count 2 with both agent IDs)"
PEERS=""
for _ in $(seq 1 50); do
  PEERS=$(curl -sS "$BASE/mesh/peers")
  echo "$PEERS" | grep -q '"agent_id":"demo-agent-a"' \
    && echo "$PEERS" | grep -q '"agent_id":"demo-agent-b"' \
    && echo "$PEERS" | grep -q '"count":2' && break
  sleep 0.2
done
echo "$PEERS" | grep -q '"agent_id":"demo-agent-a"' \
  || { echo "FAIL: demo-agent-a missing from /mesh/peers (got: $PEERS)" >&2; exit 1; }
echo "$PEERS" | grep -q '"agent_id":"demo-agent-b"' \
  || { echo "FAIL: demo-agent-b missing from /mesh/peers (got: $PEERS)" >&2; exit 1; }
echo "$PEERS" | grep -q '"count":2' \
  || { echo "FAIL: /mesh/peers count != 2 (got: $PEERS)" >&2; exit 1; }
echo "    $PEERS"
echo "    PASS: both mesh peers connected, count 2"
echo

echo "==> [7/8] publish an event (X-Agent-ID: demo-publisher)"
PUB=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/relay/publish" \
  -H 'Content-Type: application/json' -H 'X-Agent-ID: demo-publisher' \
  -d '{"topic":"demo","event":{"msg":"hello from run-demo","ts":"crier-demo"}}')
echo "    POST /relay/publish -> HTTP $PUB"
[ "$PUB" = "202" ] || { echo "FAIL: expected 202 from publish, got $PUB" >&2; exit 1; }
echo

echo "==> [8/8] subscriber must receive the event over WS /relay/subscribe"
wait "$SUB_PID" || { echo "FAIL: subscriber exited non-zero" >&2; exit 1; }
grep -q 'hello from run-demo' "$WORKDIR/sub.out" \
  || { echo "FAIL: subscriber did not receive the published event" >&2; exit 1; }
echo "    subscriber received: $(grep '^EVENT ' "$WORKDIR/sub.out")"
echo "    PASS: event fanned out to subscriber over WebSocket"
echo

echo "==> DEMO PASS: WS subscribe fan-out + mesh peer registration verified live"
echo "    transcript: ${TRANSCRIPT}"
