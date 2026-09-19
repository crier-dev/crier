#!/usr/bin/env bash
#
# CR-FEAT-006 — relay-to-relay federation demo.
#
# Proves the board PASS line end-to-end with two real crier relays:
#
#   relay-1 (<relay1-port>, CR_FED_LINKS=http://127.0.0.1:<relay2-port>, CR_FED_NAME=relay-1)
#     |  POST /agents/relay-2-agent/inbox   (blocking, sender=relay-1-agent)
#     v  [agent not found locally -> federation fallback]
#   relay-2 (<relay2-port>, no links)
#     |  webhook delivery (blocking, openai-compatible schema template)
#     v
#   echo_webhook.py (<webhook-port>) ->  {"choices":[{"message":{"content":"echo: ..."}}]}
#
# The three ports are CHOSEN by the shared selector, never hard-coded — see
# "SCRATCH PORTS" below.
#
# The blocking reply rides inside relay-2's HTTP response, which relay-1
# relays back verbatim to the original sender (curl on relay-1). Then
# GET /fed/peers on relay-1 lists BOTH relays: the local relay-1 (with its
# own agent) and the linked relay-2 (with its agent) — cross-relay agent
# discovery. relay-2's own /fed/peers lists just itself (no links — no full
# mesh required).
#
# Requirements: go, python3 (stdlib only), openssl, curl, ss (iproute2).
#   Port guards (QA-CRIER-9): never measures a server it did not start — after
#   /health the run asserts each listener is the pid it started.
#   SCRATCH PORTS (QA-CRIER-10): the three ports are CHOSEN, not hard-coded. With
#   RELAY1_PORT / RELAY2_PORT / WEBHOOK_PORT unset the run walks three bounded
#   candidate blocks — 18771..18775 (relay-1), 18776..18780 (relay-2),
#   18781..18785 (the echo webhook), <N>_PORT_CANDIDATES candidates each — and
#   uses the first free one, naming the holder pid, command line and audit
#   command of every candidate it skips (scripts/lib/port-guard.sh). A run whose
#   candidates are ALL occupied fails naming every attempted port and its holder
#   instead of skipping. Nothing is built or started until all three are settled.
#   CR_AUTH_TOKEN        must NOT be set (demo runs auth-disabled)
#   CR_REQUIRE_AGENT_SIG is forced false (no per-agent signing in the demo)
#   RELAY1_PORT          override relay-1 port (default: first free of 18771+)
#   RELAY2_PORT          override relay-2 port (default: first free of 18776+)
#   WEBHOOK_PORT         override echo webhook port (default: first free of 18781+)
#   RELAY1_PORT_CANDIDATES / RELAY2_PORT_CANDIDATES / WEBHOOK_PORT_CANDIDATES
#                        how many candidates that service's default rotation may
#                        try (default 5 each)
#   RELAY1_PORT_BASE / RELAY2_PORT_BASE / WEBHOOK_PORT_BASE
#                        first candidate of that service's rotation
#                        (default 18771 / 18776 / 18781)
#   DEMO_TRANSCRIPT      write the capture here instead of
#                        TRANSCRIPT-<date>.md next to this script
#   An EXPLICIT port is checked and never rotated away from: an occupied one
#   aborts the run naming its holder, because a run on a port the operator did
#   not name would misreport what was measured.
#
# Output: TRANSCRIPT-<date>.md in this directory (real output, teed live).
#
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"

# ── Scratch-port rotation (QA-CRIER-10) ───────────────────────────────────────
# Three services, three INDEPENDENT candidate blocks so the blocks can never
# collide with each other. <SVC>_PORT_BASE names the first candidate of that
# service's rotation (overridable so a test can move the block onto ports it has
# proved free); the port itself is settled below, before the transcript is opened
# and before anything is built.
RELAY1_PORT_BASE="${RELAY1_PORT_BASE:-18771}"
RELAY1_PORT_CANDIDATES="${RELAY1_PORT_CANDIDATES:-5}"
RELAY2_PORT_BASE="${RELAY2_PORT_BASE:-18776}"
RELAY2_PORT_CANDIDATES="${RELAY2_PORT_CANDIDATES:-5}"
WEBHOOK_PORT_BASE="${WEBHOOK_PORT_BASE:-18781}"
WEBHOOK_PORT_CANDIDATES="${WEBHOOK_PORT_CANDIDATES:-5}"
# Mirrored, not defaulted: an empty value means "walk the candidates", and a
# caller-named port is carried through untouched (it is checked, never rotated).
RELAY1_PORT="${RELAY1_PORT:-}"
RELAY2_PORT="${RELAY2_PORT:-}"
WEBHOOK_PORT="${WEBHOOK_PORT:-}"
RELAY1=""
RELAY2=""
WEBHOOK_URL=""
WORKDIR="$(mktemp -d)"
# DEMO_TRANSCRIPT (as in the ws-mesh demo) points the capture outside the repo —
# the selftest uses it so a test run cannot dirty git status; the default keeps
# writing the historical TRANSCRIPT-<date>.md next to this script.
TRANSCRIPT="${DEMO_TRANSCRIPT:-$DEMO_DIR/TRANSCRIPT-$(date +%Y-%m-%d).md}"

# Port guards (QA-CRIER-9): this harness starts all three servers it measures.
# The guards come from the shared library — require_free_port refuses to start on
# a taken port, assert_port_owned proves after /health that the listener is the
# pid started here, and wait_http_or_die aborts when a started process dies
# instead of letting the poll be answered by a stale or foreign server.
. "$REPO_ROOT/scripts/lib/port-guard.sh"

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

# ── Pre-flight (before the transcript redirect, so aborts are plainly visible) ──
for tool in go python3 openssl curl ss; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "FAIL: '$tool' is required on PATH" >&2; exit 1; }
done

# ── Settle all three scratch ports BEFORE anything is built or started ────────
# select_scratch_port (scripts/lib/port-guard.sh) is the shared selector the
# llm-mesh runners use: it walks <base>..<base>+<budget>-1 in order, prints the
# holder pid/command/audit line of every candidate it skips, and selects the
# first free one. A hard-coded scratch port made this demo abort (or measure a
# squatter) whenever anything already listened there — a long-lived unrelated
# listener, or a squatter that took the port between two runs of this same script
# (QA-CRIER-10). An EXPLICIT RELAY1_PORT / RELAY2_PORT / WEBHOOK_PORT is honored
# literally and never rotated: an occupied one aborts the run naming its holder,
# because a run on a port the operator did not name would misreport what was
# measured. Every candidate occupied is a named failure, not a silent skip.
#
# The three services get independent, non-overlapping candidate blocks, so a
# rotation on one can never land on a port another service owns.
select_scratch_port "${RELAY1_PORT:-}" "$RELAY1_PORT_BASE" "relay-1 (federation-demo)" \
  "$RELAY1_PORT_CANDIDATES" "RELAY1_PORT"
RELAY1_PORT="$PORT_GUARD_SELECTED"
select_scratch_port "${RELAY2_PORT:-}" "$RELAY2_PORT_BASE" "relay-2 (federation-demo)" \
  "$RELAY2_PORT_CANDIDATES" "RELAY2_PORT"
RELAY2_PORT="$PORT_GUARD_SELECTED"
select_scratch_port "${WEBHOOK_PORT:-}" "$WEBHOOK_PORT_BASE" "the echo webhook (federation-demo)" \
  "$WEBHOOK_PORT_CANDIDATES" "WEBHOOK_PORT"
WEBHOOK_PORT="$PORT_GUARD_SELECTED"

# Every client, relay and webhook below is addressed through these three values —
# there is no other place a port is spelled out (a component left on its own
# default would be measured on a port nothing selected).
RELAY1="http://127.0.0.1:${RELAY1_PORT}"
RELAY2="http://127.0.0.1:${RELAY2_PORT}"
WEBHOOK_URL="http://127.0.0.1:${WEBHOOK_PORT}/webhook"

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
# The echo webhook has no health route (it answers POST /webhook), so readiness
# is "the port became a listener", with the started pid checked for survival —
# then assert_port_owned proves the listener is that pid.
for _ in $(seq 1 50); do
  [ -n "$(port_holder_pid "$WEBHOOK_PORT")" ] && break
  kill -0 "$WEBHOOK_PID" 2>/dev/null || break
  sleep 0.2
done
assert_port_owned "$WEBHOOK_PORT" "$WEBHOOK_PID" "echo webhook"
echo "    webhook pid $WEBHOOK_PID"
echo

echo "==> [3/8] start relay-2 on :${RELAY2_PORT} (no federation links)"
CRIER_PORT="$RELAY2_PORT" CR_REQUIRE_AGENT_SIG=false "$WORKDIR/crier" &
RELAY2_PID=$!
# The relays log straight into this transcript (it is tee'd live), so the
# transcript is their log — a died relay is reported with its last lines.
wait_http_or_die "$RELAY2/health" "$RELAY2_PID" "$TRANSCRIPT" "relay-2"
assert_port_owned "$RELAY2_PORT" "$RELAY2_PID" "relay-2"
curl -sS "$RELAY2/health" && echo " <- relay-2 healthy"
echo

echo "==> [4/8] start relay-1 on :${RELAY1_PORT} (linked to relay-2)"
CRIER_PORT="$RELAY1_PORT" CR_REQUIRE_AGENT_SIG=false \
  CR_FED_LINKS="http://127.0.0.1:${RELAY2_PORT}" CR_FED_NAME="relay-1" \
  "$WORKDIR/crier" &
RELAY1_PID=$!
wait_http_or_die "$RELAY1/health" "$RELAY1_PID" "$TRANSCRIPT" "relay-1"
assert_port_owned "$RELAY1_PORT" "$RELAY1_PID" "relay-1"
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
# The listing must name the relay-2 port THIS run selected — not a hard-coded
# scratch literal, which would keep passing after a rotation moved relay-2.
echo "$PEERS1" | grep -Eq "127\.0\.0\.1:$RELAY2_PORT([^0-9]|$)" \
  || { echo "FAIL: linked relay-2 (:$RELAY2_PORT) missing from relay-1's /fed/peers" >&2; exit 1; }
echo "    PASS: peers listing shows both relays with their agents (relay-2 on the selected :$RELAY2_PORT)"
echo
echo "    relay-2 /fed/peers (no links -> just itself):"
curl -sS "$RELAY2/fed/peers" | python3 -m json.tool
echo

echo "==> local agent lists (for completeness)"
echo "    relay-1 /agents: $(curl -sS "$RELAY1/agents")"
echo "    relay-2 /agents: $(curl -sS "$RELAY2/agents")"
echo
echo "==> DEMO PASS: relay-1 -> relay-2 delivery via webhook, reply to sender, peers listing shows both"
