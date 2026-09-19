#!/usr/bin/env bash
#
# CR-GAP-050 / DF-CRIER-152 / DF-CRIER-3 — no-install WebSocket subscribe + mesh
# REQUEST/RESPONSE demo.
#
# Proves live end-to-end against ONE real crier relay that this script starts
# itself on 127.0.0.1:${DEMO_PORT} (scratch port; override with DEMO_PORT):
#
#   [3/10] POST /agents {"id":"demo-agent-a","capabilities":[...],"public_key":"<64-hex>"} -> 201
#          POST /agents {"id":"demo-agent-b", ...}                                         -> 201
#   [4/10] subscriber   ws://127.0.0.1:<port>/relay/subscribe/demo   (-once)
#            ^
#            |  POST /relay/publish  {"topic":"demo","event":{"msg":"hello from run-demo",...}}
#            |  (X-Agent-ID: demo-publisher — required when rate limiting is on:
#            |   [7/10] also proves the requirement NEGATIVELY, a publish without
#            |   that header must answer 401)
#            |  POST /relay/publish  {"topic":"demo-other",...}  — 202, and it must
#            |   NOT reach this subscriber: the exact topic is the only one the
#            |   subscription matches, and the subscriber (-once) would take a
#            |   fan-out to every subscriber as its first event
#   [5/10] peer A       ws://127.0.0.1:<port>/mesh/connect/demo-agent-a
#          peer B       ws://127.0.0.1:<port>/mesh/connect/demo-agent-b
#            -> GET /mesh/peers returns count 2 with both agent IDs
#            (peer B runs with -respond: it answers inbound REQUEST frames)
#   [9/10] the exchange the README documents (DF-CRIER-3), driven by the
#          repo-shipped client — nothing to install:
#            REQUEST  demo-agent-a -> demo-agent-b   (message_id=<id>)
#            RESPONSE demo-agent-b -> demo-agent-a   (request_id=<the same id>)
#          The requester loop-receives and switches on `type`, so the server's
#          own KEEPALIVE frames arriving on the same socket are ignored instead
#          of being mistaken for the reply.
#
# Every demo client is spawned with `-url "$BASE"`, so DEMO_PORT moves the
# server and the clients together. (Regression DF-CRIER-152: the clients used to
# be spawned without -url and silently fell back to the compiled-in default, so
# a DEMO_PORT run measured whatever else owned that default port.)
#
# Three guards keep the run honest, and they come from the shared library
# scripts/lib/port-guard.sh (the same one federation-demo and
# hermes-gateway-demo source):
#   * select_scratch_port CHOOSES the relay port before anything is built or
#     started (QA-CRIER-10): with DEMO_PORT unset it walks
#     $DEMO_PORT_BASE..+$DEMO_PORT_CANDIDATES-1, names the holder pid, command
#     line and `ss -tlnp | grep :<port>` audit command of every candidate it
#     skips, and uses the first free one; an EXPLICIT DEMO_PORT is checked and
#     NEVER rotated (an occupied one aborts naming its holder), because a run on
#     a port the operator did not name would misreport what was measured; every
#     candidate occupied is a named failure, never a silent skip.
#   * assert_port_owned then proves, after the relay answered /health, that the
#     process HOLDING $DEMO_PORT is the pid this script started — presence is not
#     ownership, and
#   * the relay's peer list must be EMPTY right after startup — a foreign server
#     answering on the same port (docker-published crier, stale demo run, …)
#     would already list peers, and would otherwise be measured by mistake.
#
# Requirements: go (toolchain only) + curl + sha256sum (coreutils, used to derive
# the registry public_key) + ss (iproute2 — how the port guards find the holder).
# gorilla/websocket v1.5.3 comes from go.mod — zero
# external installs, zero new dependencies.
#   CR_AUTH_TOKEN        must NOT be set (demo runs auth-disabled)
#   CR_REQUIRE_AGENT_SIG forced false (no per-agent signing in the demo)
#   DEMO_PORT            use THIS relay port. Checked, never rotated: an occupied
#                        one aborts naming its holder.
#   DEMO_PORT_CANDIDATES how many candidates the default rotation may try
#                        (default 5); all occupied = a named failure
#   DEMO_PORT_BASE       first candidate of the default rotation (default 18961 —
#                        a scratch port, overridable so a test can move the block)
#   DEMO_KEEPALIVE_WAIT  bound for the live KEEPALIVE observation in [9/10]
#                        (default 35s — the server's keepalive interval is 30s,
#                        internal/mesh/peer.go; set 0 to skip that ~30s wait)
#   DEMO_REQUEST_TIMEOUT how long the requester waits for the RESPONSE (20s)
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

# The shared "the server we measure is the server we started" guards (QA-CRIER-9),
# the same library federation-demo and hermes-gateway-demo source. Sourced, not
# re-implemented: a local copy of a guard is a guard that drifts.
. "$REPO_ROOT/scripts/lib/port-guard.sh"

# Loopback never rides an ambient proxy (QA-CRIER-21). curl has NO built-in
# loopback exemption: on a host that exports HTTP_PROXY (a corporate default, a
# sandbox egress proxy, a CI image), `curl -sfS "$BASE/health"` below is sent to
# the PROXY, so it never reaches the relay this script just started — [2/10]
# aborts with "relay never became healthy", and a caller that waits for the
# readiness line (the Go E2E arm) hangs until its own bound instead of failing.
# The shared guard merges 127.0.0.1/localhost/::1 into no_proxy AND NO_PROXY; it
# does NOT unset HTTP_PROXY, so a genuinely external host still honours the
# proxy. Called here, before ANY curl (including the port selector's preflight).
guard_loopback_off_proxy

DEMO_PORT_BASE="${DEMO_PORT_BASE:-18961}"   # first candidate of the default rotation
DEMO_PORT_CANDIDATES="${DEMO_PORT_CANDIDATES:-5}"
# DEMO_PORT is mirrored, not defaulted: empty means "walk the candidates", and a
# caller-named port is carried through untouched (checked, never rotated). It is
# settled below, before the transcript is opened and before anything is built.
DEMO_PORT="${DEMO_PORT:-}"
BASE=""
WORKDIR="$(mktemp -d)"

# The server's mesh keepalive interval is mesh.DefaultMeshConfig().KeepaliveInterval
# = 30s (internal/mesh/peer.go:39-48, used by cmd/server/main.go:149), and it
# sends one to every accepted peer. [9/10] holds the socket slightly longer than
# that so the transcript carries a real KEEPALIVE frame being ignored, not just a
# description of the rule. DEMO_KEEPALIVE_WAIT=0 keeps the run short and leans on
# the Go test for that proof instead (the script says so out loud when it does).
DEMO_KEEPALIVE_WAIT="${DEMO_KEEPALIVE_WAIT:-35s}"
DEMO_REQUEST_TIMEOUT="${DEMO_REQUEST_TIMEOUT:-20s}"

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
ws-mesh-demo run-demo.sh — crier relay pub/sub + mesh REQUEST/RESPONSE demo
(CR-GAP-050, DF-CRIER-3)

Usage:
  bash run-demo.sh [-h|--help]

What it does — one crier relay started by this script on 127.0.0.1:<selected port>
(override the port: DEMO_PORT=<free port> bash run-demo.sh):
  [1/10] build ./cmd/server and ./examples/ws-mesh-demo into a mktemp dir
  [2/10] select_scratch_port CHOOSES the relay port before anything is built:
         it walks DEMO_PORT_BASE(=${DEMO_PORT_BASE})..+${DEMO_PORT_CANDIDATES}-1,
         names the holder of every candidate it skips, and uses the first free
         one; an explicitly named DEMO_PORT is checked and never rotated (an
         occupied one aborts naming its holder); every candidate occupied is a
         named failure. The relay then starts auth-disabled with rate limiting
         on (100/min), and once it answers /health the script asserts with
         assert_port_owned that the pid HOLDING that port is the pid it
         started, together with an EMPTY peer list — a foreign server on that
         port would already list peers (presence is not ownership)
  [3/10] HTTP-register demo-agent-a and demo-agent-b: POST /agents -> 201, so
         both peers exist in the agent registry BEFORE they join the mesh
  [4/10] spawn the subscriber on /relay/subscribe/demo
  [5/10] spawn peer B with -respond (it answers inbound REQUESTs) and peer A on
         /mesh/connect/<agent>   (each with -url \$BASE)
  [6/10] assert GET /mesh/peers reports count 2 listing both agent ids
  [7/10] publish: a publish WITHOUT X-Agent-ID must be 401 (the per-agent rate
         limiter keys on that header), a publish to topic demo-other must be
         202, then the event under test goes to topic demo
  [8/10] assert the subscriber received EXACTLY ONE event naming the exact
         topic demo (it runs with -once, so a relay that fanned out to every
         subscriber would hand it the demo-other event first)
  [9/10] run the mesh exchange: REQUEST demo-agent-a -> demo-agent-b, then assert
         the RESPONSE's request_id is the REQUEST's message_id, its status_code
         is 200, and that a KEEPALIVE frame arriving on the same socket was
         ignored rather than mistaken for the reply
  [10/10] cross-check the responder's log: it received that same message_id and
         echoed it as request_id

Notes:
  * no external websocket client is needed: every leg is driven by this repo's
    own Go client (gorilla/websocket, from go.mod). The only tools required on
    PATH are go, curl, sha256sum and ss.
  * the three port guards are the shared library scripts/lib/port-guard.sh (the
    same one federation-demo and hermes-gateway-demo source): select_scratch_port
    chooses the port and names every candidate it skips with that candidate's
    holder pid, command line and the "ss -tlnp | grep :<port>" audit command, and
    exits 1 when every candidate is occupied (an explicitly named DEMO_PORT is
    checked and never rotated); assert_port_owned exits 1 when the holder is not
    the pid this script started; a relay that dies before answering /health
    aborts the run instead of being papered over.
  * registration in [3/10] is done by this demo so each peer is a known agent as
    well as a mesh connection; /mesh/peers itself reports live WebSocket
    connections (a peer that never registered still shows up there).
  * every demo client is passed -url \$BASE, so DEMO_PORT moves the server and
    the clients together.
  * [9/10] waits up to \$DEMO_KEEPALIVE_WAIT (default 35s) for the server's real
    30s KEEPALIVE so the probe is a live frame, not an assertion about one;
    DEMO_KEEPALIVE_WAIT=0 skips the wait.
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
for tool in go curl sha256sum ss; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "FAIL: '$tool' is required on PATH" >&2; exit 1; }
done

case "${DEMO_PORT:-}" in
  '') ;; # empty: rotate the default candidates below
  *[!0-9]*)
    echo "FAIL: DEMO_PORT must be a TCP port number (got '$DEMO_PORT')" >&2
    exit 2
    ;;
esac
if [ -n "$DEMO_PORT" ]; then
  [ "$DEMO_PORT" -ge 1 ] && [ "$DEMO_PORT" -le 65535 ] \
    || { echo "FAIL: DEMO_PORT out of range: $DEMO_PORT" >&2; exit 2; }
fi

# ── Settle the scratch port BEFORE anything is built or started ──────────────
# select_scratch_port (scripts/lib/port-guard.sh) is the shared selector the
# llm-mesh, federation and hermes-gateway runners use: it walks
# $DEMO_PORT_BASE..+$DEMO_PORT_CANDIDATES-1 in order, prints the holder
# pid/command/audit line of every candidate it skips, and selects the first free
# one. The port used to be one fixed default (18961), so a long-lived unrelated
# listener on it — or a squatter that took it between two runs of this same
# script — made the whole demo abort even though every other port was free
# (QA-CRIER-10). An EXPLICIT DEMO_PORT is honored literally and never rotated: an
# occupied one aborts the run naming its holder, because a run on a port the
# operator did not name would misreport what was measured. Every candidate
# occupied is a named failure, not a silent skip.
#
# It is called DIRECTLY (not in a subshell): the selected port comes back in
# PORT_GUARD_SELECTED, and the guard's own report is the operator's evidence.
select_scratch_port "${DEMO_PORT:-}" "$DEMO_PORT_BASE" "the ws-mesh-demo relay" \
  "$DEMO_PORT_CANDIDATES" "DEMO_PORT"
DEMO_PORT="$PORT_GUARD_SELECTED"
# Every server probe and every demo client below is addressed through $BASE —
# there is no other place a port is spelled out (a client left on its compiled-in
# default would be measured on a port nothing selected).
BASE="http://127.0.0.1:${DEMO_PORT}"

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

echo "# CR-GAP-050 WS subscribe + mesh REQUEST/RESPONSE demo — TRANSCRIPT"
echo
echo "- date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "- repo: $(cd "$REPO_ROOT" && git rev-parse --short HEAD) ($(cd "$REPO_ROOT" && git log -1 --format=%s))"
echo "- relay: ${BASE} (auth-disabled)"
echo "- loopback proxy bypass: no_proxy=${no_proxy:-<unset>} (every curl below targets \$BASE and is pinned with --noproxy '*')"
echo "- keepalive wait bound: ${DEMO_KEEPALIVE_WAIT}"
echo "- transcript: ${TRANSCRIPT}"
echo

echo "==> [1/10] build crier + demo client (go toolchain only)"
( cd "$REPO_ROOT" && go build -o "$WORKDIR/crier" ./cmd/server )
( cd "$REPO_ROOT" && go build -o "$WORKDIR/ws-mesh-demo" ./examples/ws-mesh-demo )
echo "    built $WORKDIR/crier and $WORKDIR/ws-mesh-demo"
echo

echo "==> [2/10] start relay on :${DEMO_PORT} (auth-disabled)"
echo "    port :${DEMO_PORT} was selected by port-guard (nothing was listening on it)"
# CR_RATE_LIMIT_PER_MINUTE is stated explicitly: [7/10] asserts the 401 its
# header requirement produces, so the demo must not inherit a softened setting
# from the ambient environment (0 there would turn the limiter — and the
# requirement — off).
env -u CR_AUTH_TOKEN CR_AUTH_TOKEN= CR_REQUIRE_AGENT_SIG=false \
  CR_RATE_LIMIT_PER_MINUTE=100 \
  CRIER_PORT="$DEMO_PORT" CR_LOG_LEVEL=warn "$WORKDIR/crier" &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl --noproxy '*' -sfS "$BASE/health" >/dev/null 2>&1 && break
  kill -0 "$SERVER_PID" 2>/dev/null \
    || { echo "FAIL: relay exited during startup (pid $SERVER_PID)" >&2; exit 1; }
  sleep 0.2
done
curl --noproxy '*' -sfS "$BASE/health" >/dev/null 2>&1 \
  || { echo "FAIL: relay never became healthy at $BASE" >&2; exit 1; }
echo "    $(curl --noproxy '*' -sS "$BASE/health") <- relay healthy (our pid $SERVER_PID)"

# ── Guard 2/3: the listener we just polled must be OUR process ───────────────
# assert_port_owned asks ss who HOLDS :$DEMO_PORT and exits 1 unless that pid is
# $SERVER_PID. "Something answered /health" is not "the process we started
# answered /health"; without this check a squatter that took the port in the
# window between the selection and the bind would be measured in our place.
PORT_OWNER="$(port_holder_pid "$DEMO_PORT")"
assert_port_owned "$DEMO_PORT" "$SERVER_PID" "the ws-mesh-demo relay"
echo "    :${DEMO_PORT} is held by pid $PORT_OWNER == our relay pid $SERVER_PID (ss -tlnp)"

# ── Guard 3/3: the server we are about to measure reports an EMPTY mesh ──────
PEERS0=$(curl --noproxy '*' -sS "$BASE/mesh/peers")
echo "$PEERS0" | grep -q '"count":0' \
  || { echo "FAIL: $BASE/mesh/peers is not empty at startup (got: $PEERS0) — this is not our relay" >&2; exit 1; }
echo "    GET /mesh/peers -> $PEERS0 (empty: measured server is the one we started)"
echo

echo "==> [3/10] HTTP-register demo-agent-a and demo-agent-b (POST /agents)"
for AGENT in demo-agent-a demo-agent-b; do
  # 64-hex ed25519-style public key derived from the id — deterministic, no keys
  # to manage, and it satisfies the registry's 64-hex validation.
  PUBKEY=$(printf '%s' "$AGENT" | sha256sum | cut -c1-64)
  REG_CODE=$(curl --noproxy '*' -sS -o "$WORKDIR/register-${AGENT}.json" -w '%{http_code}' \
    -X POST "$BASE/agents" -H 'Content-Type: application/json' \
    -d "{\"id\":\"${AGENT}\",\"capabilities\":[\"demo\",\"mesh\"],\"public_key\":\"${PUBKEY}\"}")
  echo "    POST /agents ${AGENT} -> HTTP ${REG_CODE} $(cat "$WORKDIR/register-${AGENT}.json")"
  [ "$REG_CODE" = "201" ] \
    || { echo "FAIL: registering ${AGENT} returned HTTP ${REG_CODE} (expected 201)" >&2; exit 1; }
done
REGISTERED=$(curl --noproxy '*' -sS "$BASE/agents")
for AGENT in demo-agent-a demo-agent-b; do
  echo "$REGISTERED" | grep -q "\"id\":\"${AGENT}\"" \
    || { echo "FAIL: ${AGENT} missing from GET /agents (got: $REGISTERED)" >&2; exit 1; }
done
echo "    GET /agents -> $(echo "$REGISTERED" | grep -o '"id":"[^"]*"' | paste -sd' ' -)"
echo "    demo-agent-a and demo-agent-b are registered before any mesh connection"
echo

echo "==> [4/10] spawn subscriber on /relay/subscribe/demo (-url $BASE)"
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

echo "==> [5/10] spawn two mesh peers (-url $BASE, agents from step 3)"
# Peer B is the RESPONDER: -respond makes it answer every inbound REQUEST with a
# RESPONSE whose request_id is the REQUEST's message_id (the correlation
# contract), carrying -body verbatim as an object.
"$WORKDIR/ws-mesh-demo" -url "$BASE" peer -agent demo-agent-b -respond -status 200 \
  -body '{"pong":true,"from":"demo-agent-b"}' > "$WORKDIR/peer-b.out" 2>&1 &
PEER_B_PID=$!
"$WORKDIR/ws-mesh-demo" -url "$BASE" peer -agent demo-agent-a > "$WORKDIR/peer-a.out" 2>&1 &
PEER_A_PID=$!
for _ in $(seq 1 50); do
  grep -q '^PEER CONNECTED demo-agent-a' "$WORKDIR/peer-a.out" 2>/dev/null \
    && grep -q '^PEER CONNECTED demo-agent-b' "$WORKDIR/peer-b.out" 2>/dev/null && break
  sleep 0.2
done
grep -q '^PEER CONNECTED demo-agent-a' "$WORKDIR/peer-a.out" \
  || { echo "FAIL: peer A (demo-agent-a) did not connect ($(cat "$WORKDIR/peer-a.out"))" >&2; exit 1; }
grep -q '^PEER CONNECTED demo-agent-b' "$WORKDIR/peer-b.out" \
  || { echo "FAIL: peer B (demo-agent-b) did not connect ($(cat "$WORKDIR/peer-b.out"))" >&2; exit 1; }
echo "    demo-agent-a and demo-agent-b connected to $BASE (B answers REQUESTs)"
echo

echo "==> [6/10] GET /mesh/peers (must show count 2 with both agent IDs)"
PEERS=""
for _ in $(seq 1 50); do
  PEERS=$(curl --noproxy '*' -sS "$BASE/mesh/peers")
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

echo "==> [7/10] publish: X-Agent-ID requirement + exact-topic fan-out"
# (a) NEGATIVE: the per-agent rate limiter keys on X-Agent-ID and is on by
#     default (100/min), so a publish without the header must be refused with
#     401 BEFORE the body is read. Asserted, not assumed: this is the half of
#     the requirement a run can silently lose by inheriting
#     CR_RATE_LIMIT_PER_MINUTE=0 from its environment.
PUB_NO_HEADER=$(curl --noproxy '*' -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/relay/publish" \
  -H 'Content-Type: application/json' \
  -d '{"topic":"demo","event":{"msg":"never delivered: no X-Agent-ID"}}')
echo "    POST /relay/publish without X-Agent-ID -> HTTP $PUB_NO_HEADER (expected 401)"
[ "$PUB_NO_HEADER" = "401" ] \
  || { echo "FAIL: a publish without X-Agent-ID answered $PUB_NO_HEADER, expected 401 — the header is required only while rate limiting is on (CR_RATE_LIMIT_PER_MINUTE is forced to 100 above)" >&2; exit 1; }

# (b) A publish to a DIFFERENT topic must not reach this subscriber, and the
#     subscriber is the assertion: it runs with -once, so it exits on the FIRST
#     event it receives. A relay that fanned every event out to every subscriber
#     would hand it this one first and [8/10] would fail on the payload — which
#     makes "the subscription matched exactly topic demo" a provable claim
#     instead of an assumption.
PUB_OTHER=$(curl --noproxy '*' -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/relay/publish" \
  -H 'Content-Type: application/json' -H 'X-Agent-ID: demo-publisher' \
  -d '{"topic":"demo-other","event":{"msg":"must not reach the demo subscriber"}}')
echo "    POST /relay/publish topic=demo-other -> HTTP $PUB_OTHER (202: accepted, no subscriber for it)"
[ "$PUB_OTHER" = "202" ] \
  || { echo "FAIL: publishing to demo-other answered $PUB_OTHER, expected 202" >&2; exit 1; }

# (c) The event under test, on the exact topic the subscriber listens on.
PUB=$(curl --noproxy '*' -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/relay/publish" \
  -H 'Content-Type: application/json' -H 'X-Agent-ID: demo-publisher' \
  -d '{"topic":"demo","event":{"msg":"hello from run-demo","ts":"crier-demo"}}')
echo "    POST /relay/publish topic=demo with X-Agent-ID: demo-publisher -> HTTP $PUB"
[ "$PUB" = "202" ] || { echo "FAIL: expected 202 from publish, got $PUB" >&2; exit 1; }
echo

echo "==> [8/10] subscriber must receive that one event over WS /relay/subscribe"
wait "$SUB_PID" || { echo "FAIL: subscriber exited non-zero" >&2; exit 1; }
EVENT_LINES=$(grep -c '^EVENT ' "$WORKDIR/sub.out" || true)
[ "$EVENT_LINES" = "1" ] \
  || { echo "FAIL: the subscriber received $EVENT_LINES events; exactly one publish went to its topic (the demo-other publish above must not reach it)" >&2; exit 1; }
grep -q '"topic":"demo"' "$WORKDIR/sub.out" \
  || { echo "FAIL: subscriber frame did not name the literal topic (got: $(grep '^EVENT ' "$WORKDIR/sub.out"))" >&2; exit 1; }
grep -q 'hello from run-demo' "$WORKDIR/sub.out" \
  || { echo "FAIL: subscriber did not receive the published event" >&2; exit 1; }
echo "    subscriber received: $(grep '^EVENT ' "$WORKDIR/sub.out")"
echo "    PASS: exactly one event, on the exact subscribed topic demo, fanned out over WebSocket"
echo "          (a publish without X-Agent-ID was 401, and topic demo-other reached no subscriber here)"
echo

echo "==> [9/10] mesh REQUEST/RESPONSE round-trip, demo-agent-a -> demo-agent-b"
# The request leg reconnects as demo-agent-a, so its holding socket is closed
# first and the mesh must have forgotten it: the server keys connections by agent
# ID (internal/mesh/handler.go -> Mesh.AcceptPeer), so a second socket under the
# same id replaces the first in the map — and a late OnClose from the old socket
# would then delete the NEW entry and drop the RESPONSE.
kill "$PEER_A_PID" 2>/dev/null || true
wait "$PEER_A_PID" 2>/dev/null || true
PEER_A_PID=""
PEERS_AFTER=""
for _ in $(seq 1 50); do
  PEERS_AFTER=$(curl --noproxy '*' -sS "$BASE/mesh/peers")
  echo "$PEERS_AFTER" | grep -q '"agent_id":"demo-agent-a"' || break
  sleep 0.2
done
echo "$PEERS_AFTER" | grep -q '"agent_id":"demo-agent-a"' \
  && { echo "FAIL: demo-agent-a is still listed after its socket closed (got: $PEERS_AFTER)" >&2; exit 1; }
echo "$PEERS_AFTER" | grep -q '"agent_id":"demo-agent-b"' \
  || { echo "FAIL: the responder demo-agent-b left the mesh when peer A closed (got: $PEERS_AFTER)" >&2; exit 1; }
echo "    peer A's holding socket closed -> $(echo "$PEERS_AFTER" | grep -o '"count":[0-9]*'), responder still connected"

ROUNDTRIP_OUT="$WORKDIR/roundtrip.out"
set +e
"$WORKDIR/ws-mesh-demo" -url "$BASE" roundtrip -agent demo-agent-a -target demo-agent-b -method GET -path /ping -body '{"hello":"world"}' -expect-status 200 -timeout "$DEMO_REQUEST_TIMEOUT" -keepalive-wait "$DEMO_KEEPALIVE_WAIT" > "$ROUNDTRIP_OUT" 2>&1
ROUNDTRIP_RC=$?
set -e
sed 's/^/    /' "$ROUNDTRIP_OUT"
[ "$ROUNDTRIP_RC" = "0" ] \
  || { echo "FAIL: the roundtrip client exited $ROUNDTRIP_RC (see the transcript above)" >&2; exit 1; }

REQ_ID=$(sed -n 's/^REQUEST SENT message_id=\([0-9a-f][0-9a-f]*\).*/\1/p' "$ROUNDTRIP_OUT" | head -1)
[ -n "$REQ_ID" ] \
  || { echo "FAIL: no 'REQUEST SENT message_id=…' line in the requester output" >&2; exit 1; }
[ "${#REQ_ID}" = "24" ] \
  || { echo "FAIL: the REQUEST's message_id is not a 24-hex id: '$REQ_ID'" >&2; exit 1; }

OK_LINE=$(grep -m1 '^ROUNDTRIP OK ' "$ROUNDTRIP_OUT" || true)
[ -n "$OK_LINE" ] \
  || { echo "FAIL: the requester never reported ROUNDTRIP OK" >&2; exit 1; }
RESP_REQUEST_ID=$(printf '%s\n' "$OK_LINE" | sed -n 's/.* request_id=\([0-9a-f][0-9a-f]*\).*/\1/p')
RESP_MSG_ID=$(printf '%s\n' "$OK_LINE" | sed -n 's/.* response_message_id=\([0-9a-f][0-9a-f]*\).*/\1/p')
RESP_STATUS=$(printf '%s\n' "$OK_LINE" | sed -n 's/.* status_code=\([0-9][0-9]*\).*/\1/p')

# (a) the documented correlation: RESPONSE.request_id == REQUEST.message_id
[ "$RESP_REQUEST_ID" = "$REQ_ID" ] \
  || { echo "FAIL: RESPONSE.request_id ($RESP_REQUEST_ID) != REQUEST.message_id ($REQ_ID)" >&2; exit 1; }
# (b) the status code the responder was told to send
[ "$RESP_STATUS" = "200" ] \
  || { echo "FAIL: RESPONSE.status_code is '$RESP_STATUS', expected 200" >&2; exit 1; }
# (c) the reply's OWN message_id must differ from the correlation id — otherwise
#     this run could not tell `request_id` from `message_id` and would pass on a
#     frame that correlates on the wrong field.
[ -n "$RESP_MSG_ID" ] && [ "$RESP_MSG_ID" != "$REQ_ID" ] \
  || { echo "FAIL: RESPONSE.message_id ('$RESP_MSG_ID') is not distinct from the correlation id ('$REQ_ID') — the assertion cannot distinguish the two fields" >&2; exit 1; }
# (d) the frame the requester accepted was a RESPONSE frame, and the body was
#     relayed verbatim as an object (never stringified).
grep -q "^RESPONSE RECEIVED request_id=${REQ_ID} " "$ROUNDTRIP_OUT" \
  || { echo "FAIL: the requester did not report a RESPONSE correlated with $REQ_ID" >&2; exit 1; }
grep -q 'body={"pong":true,"from":"demo-agent-b"}' "$ROUNDTRIP_OUT" \
  || { echo "FAIL: the RESPONSE body was not relayed verbatim as an object (got: $(grep '^RESPONSE RECEIVED' "$ROUNDTRIP_OUT"))" >&2; exit 1; }

echo "    PASS: RESPONSE.request_id == REQUEST.message_id == $REQ_ID, status_code=200, body object relayed verbatim"

# (e) KEEPALIVE frames on the same socket must be ignored, never mistaken for
#     the reply. With DEMO_KEEPALIVE_WAIT>0 the client holds the socket past the
#     server's 30s keepalive tick and exits 0 only after one really arrived.
#     Both lines count: KEEPALIVE IGNORED is a frame classified during the await
#     phase, KEEPALIVE OBSERVED is the live one from the wait phase — either way
#     the frame was classified as a KEEPALIVE, never accepted as the RESPONSE.
KEEPALIVE_FRAMES=$(grep -Ec '^KEEPALIVE (IGNORED|OBSERVED) ' "$ROUNDTRIP_OUT" || true)
case "$DEMO_KEEPALIVE_WAIT" in
  "0"|"0s"|"0m"|"0h")
    echo "    KEEPALIVE live wait DISABLED (DEMO_KEEPALIVE_WAIT=$DEMO_KEEPALIVE_WAIT): no KEEPALIVE frame was"
    echo "      observed in this run ($KEEPALIVE_FRAMES classified). The filter itself is still exercised"
    echo "      live by the Go tests that drive the same code paths on an in-process mesh"
    echo "      (examples/ws-mesh-demo: TestClassifyInboundFiltersKeepaliveAndForeignReplies,"
    echo "      TestRoundtripIgnoresKeepaliveFramesMidAwait)."
    ;;
  *)
    OBSERVED=$(grep -m1 '^KEEPALIVE OBSERVED ' "$ROUNDTRIP_OUT" || true)
    [ -n "$OBSERVED" ] \
      || { echo "FAIL: no live KEEPALIVE frame within $DEMO_KEEPALIVE_WAIT (keepalives classified: $KEEPALIVE_FRAMES) — the server sends one every keepalive_interval_ms" >&2; exit 1; }
    [ "$KEEPALIVE_FRAMES" -ge 1 ] \
      || { echo "FAIL: a KEEPALIVE was observed but no frame was classified as ignored" >&2; exit 1; }
    echo "    $OBSERVED"
    echo "    PASS: a live KEEPALIVE frame arrived on the requester's socket and was ignored ($KEEPALIVE_FRAMES classified, 0 mistaken for the reply)"
    ;;
esac
echo

echo "==> [10/10] cross-check the responder's log (peer B echoed the REQUEST's message_id)"
# The responder is a separate process: it must have seen the SAME message_id and
# put it in request_id, which is what the server's route table keys on.
B_REQ_ID=$(sed -n 's/^REQUEST RECEIVED message_id=\([0-9a-f][0-9a-f]*\).*/\1/p' "$WORKDIR/peer-b.out" | head -1)
B_SENT_ID=$(sed -n 's/^RESPONSE SENT request_id=\([0-9a-f][0-9a-f]*\).*/\1/p' "$WORKDIR/peer-b.out" | head -1)
[ -n "$B_REQ_ID" ] && [ "$B_REQ_ID" = "$REQ_ID" ] \
  || { echo "FAIL: responder saw message_id '$B_REQ_ID', requester sent '$REQ_ID'" >&2; exit 1; }
[ -n "$B_SENT_ID" ] && [ "$B_SENT_ID" = "$REQ_ID" ] \
  || { echo "FAIL: responder answered with request_id '$B_SENT_ID', expected '$REQ_ID'" >&2; exit 1; }
echo "    $(grep -m1 '^REQUEST RECEIVED ' "$WORKDIR/peer-b.out")"
echo "    $(grep -m1 '^RESPONSE SENT ' "$WORKDIR/peer-b.out")"
echo "    PASS: the responder echoed $REQ_ID as request_id"
B_KEEPALIVES=$(grep -c '^PEER KEEPALIVE IGNORED ' "$WORKDIR/peer-b.out" || true)
echo "    responder also ignored $B_KEEPALIVES server KEEPALIVE frame(s) (same rule, other socket)"
echo

echo "==> DEMO PASS: relay WS subscribe/publish (exact topic + X-Agent-ID 401) + mesh peers + REQUEST/RESPONSE"
echo "    transcript: ${TRANSCRIPT}"
