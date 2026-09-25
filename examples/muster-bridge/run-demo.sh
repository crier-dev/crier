#!/usr/bin/env bash
#
# INT-MUSTER-002 / INT-MUSTER-003 — the "Using crier from Muster" onboarding path,
# as a runnable script.
#
# Muster is an external platform that generates an HTTP client from an OpenAPI
# document. Crier ships that document (docs/openapi.yaml, served at /openapi.json
# and /openapi.yaml), so the onboarding path a Muster-driven integration walks is:
#
#   [3/9] discover the spec      GET /openapi.json -> 200, GET /openapi.yaml -> 200
#                                (both auth-exempt: a spec fetch needs no token)
#   [4/9] register an agent      POST /agents                      -> 201 (bearer, no signature)
#   [5/9] deliver into its inbox POST /agents/{id}/inbox           -> 201 (bearer, no signature)
#   [6/9] retrieve               GET /agents/{id}/inbox            -> 401 unsigned (config C)
#                                                                  -> 200 with the §3 sig helper
#   [8/9] ack the lease          POST /agents/{id}/inbox/ack       -> 204 (signed)
#   [9/9] the MEASURED authorization matrix (INT-MUSTER-003): every endpoint the
#         bearer-only client can drive, and the five that need a per-agent
#         ed25519 signature — statuses measured live, asserted, never transcribed.
#
# The one thing a spec-generated client CANNOT do is the signature: the spec
# declares `agentSignature` as an apiKey-style header (X-Agent-Sig) but its value
# is a per-request ed25519 signature over "METHOD\n<path>\n<unix-seconds>", which
# only the holder of the agent's PRIVATE key can produce (a generated client
# signs nothing). This script therefore does that leg with the repo's documented
# OpenSSL 3 helper (the `sig()` helper of README.md "Try it" / §3 of
# docs/integration-guide.md) — and prints, for every other endpoint, the status a
# bearer-only generated client actually gets.
#
# CONFIGURATION (the two auth configurations of §1 of the integration guide)
#   * default            — config C, the production default:
#                          CR_AUTH_TOKEN=$MUSTER_BRIDGE_TOKEN, CR_REQUIRE_AGENT_SIG=true.
#                          deliver/register are unsigned, the five agent-owned
#                          reads/writes are NOT: retrieve/ack/stats need the sig.
#   * MUSTER_BRIDGE_REQUIRE_SIG=false — config B: the SAME script then runs the
#                          whole round trip unsigned, which is exactly the path a
#                          Muster-generated bearer-only client drives
#                          (register 201, deliver 201, retrieve 200, ack 204).
#
# Every status this script prints is MEASURED — each one is an assertion with the
# expected code, and a mismatch fails the run. Nothing is transcribed.
#
# Requirements: go (toolchain only), curl, openssl 3+ (pkeyutl -sign -rawin),
# xxd, ss (iproute2 — how the port guard finds the holder), sed/grep (coreutils).
#   CR_GUARD_ENABLED is forced false here (no DEEPSEEK_API_KEY needed and no
#   provider call in the loop — see the integration guide's guard section).
#   CR_AUTH_TOKEN is forced to $MUSTER_BRIDGE_TOKEN; an ambient token is ignored.
#
# SCRATCH PORT + PORT GUARDS (QA-CRIER-9 / QA-CRIER-10, scripts/lib/port-guard.sh)
#   MUSTER_BRIDGE_PORT            use THIS relay port. Checked, never rotated: an
#                                 occupied one aborts naming its holder.
#   MUSTER_BRIDGE_PORT_BASE       first candidate of the default rotation (18801)
#   MUSTER_BRIDGE_PORT_CANDIDATES how many candidates the rotation may try (5)
#   MUSTER_BRIDGE_TOKEN           bearer token the relay requires (default below)
#   MUSTER_BRIDGE_AGENT           the consumer id this run registers
#   MUSTER_BRIDGE_REQUIRE_SIG     true (config C, default) | false (config B)
#   MUSTER_BRIDGE_TRANSCRIPT      override the transcript path
# The script registers an EXIT trap that kills the pid it started, and before that
# it asserts that the pid HOLDING the port is the pid it started — a server this
# run did not start is never measured (CR-GAP-069).
#
# Output: the run is teed to a transcript OUTSIDE the repo, at
#   ${TMPDIR:-/tmp}/muster-bridge-TRANSCRIPT-<date>.XXXXXX.md
# whose path is printed at the end (nothing is written into the working tree).
#
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"

# The shared "the server we measure is the server we started" guards (QA-CRIER-9)
# plus the scratch-port selector (QA-CRIER-10). Sourced, never re-implemented.
. "$REPO_ROOT/scripts/lib/port-guard.sh"

# Loopback never rides an ambient proxy (QA-CRIER-21): curl has no built-in
# loopback exemption, so an exported HTTP_PROXY would swallow the probes below.
guard_loopback_off_proxy

MUSTER_BRIDGE_PORT_BASE="${MUSTER_BRIDGE_PORT_BASE:-18801}"
MUSTER_BRIDGE_PORT_CANDIDATES="${MUSTER_BRIDGE_PORT_CANDIDATES:-5}"
MUSTER_BRIDGE_PORT="${MUSTER_BRIDGE_PORT:-}"
MUSTER_BRIDGE_TOKEN="${MUSTER_BRIDGE_TOKEN:-muster-bridge-token}"
MUSTER_BRIDGE_AGENT="${MUSTER_BRIDGE_AGENT:-muster-bridge}"
MUSTER_BRIDGE_REQUIRE_SIG="${MUSTER_BRIDGE_REQUIRE_SIG:-true}"
WORKDIR="$(mktemp -d)"
TRANSCRIPT="${MUSTER_BRIDGE_TRANSCRIPT:-}"
SERVER_PID=""

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

for tool in go curl openssl xxd ss sed grep; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "FAIL: '$tool' is required on PATH" >&2; exit 1; }
done

case "$MUSTER_BRIDGE_REQUIRE_SIG" in
  true | false) ;;
  *) echo "FAIL: MUSTER_BRIDGE_REQUIRE_SIG must be true or false (got '$MUSTER_BRIDGE_REQUIRE_SIG')" >&2; exit 2 ;;
esac

# Settle the scratch port BEFORE anything is built or started.
select_scratch_port "$MUSTER_BRIDGE_PORT" "$MUSTER_BRIDGE_PORT_BASE" \
  "the muster-bridge relay" "$MUSTER_BRIDGE_PORT_CANDIDATES" "MUSTER_BRIDGE_PORT"
PORT="$PORT_GUARD_SELECTED"
BASE="http://127.0.0.1:$PORT"

if [ -n "$TRANSCRIPT" ]; then
  mkdir -p "$(dirname "$TRANSCRIPT")"
  : > "$TRANSCRIPT"
else
  TRANSCRIPT="$(mktemp "${TMPDIR:-/tmp}/muster-bridge-TRANSCRIPT-$(date +%Y-%m-%d).XXXXXX.md")"
fi
exec > >(tee "$TRANSCRIPT") 2>&1

FAILS=0
check() { # <label> <expected> <observed> — assert one measured status
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    printf '    %-46s -> %s\n' "$label" "$got"
  else
    printf '    %-46s -> %s  ** WANT %s **\n' "$label" "$got" "$want"
    FAILS=$((FAILS + 1))
  fi
}

# sig <METHOD> <PATH> <TS> — hex(ed25519 over "METHOD\nPATH\nTS"), the §3 helper.
# Requires OpenSSL >= 3 (`pkeyutl -sign -rawin`); it refuses loudly rather than
# sending an empty signature the server would answer 401 for, blaming the headers.
sig() {
  if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then
    echo "ERROR: this signing helper requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)" >&2
    return 1
  fi
  printf '%s\n%s\n%s' "$1" "$2" "$3" > "$WORKDIR/payload.txt"
  _sig=$(openssl pkeyutl -sign -rawin -inkey "$WORKDIR/agent.key" -in "$WORKDIR/payload.txt" 2>/dev/null | xxd -p -c 128)
  if [ -z "$_sig" ]; then
    echo "ERROR: signing produced an EMPTY signature (the payload must be a seekable file passed with -in)" >&2
    return 1
  fi
  printf '%s\n' "$_sig"
}

AUTH=(-H "Authorization: Bearer ${MUSTER_BRIDGE_TOKEN}")
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

if [ "$MUSTER_BRIDGE_REQUIRE_SIG" = true ]; then
  CONFIG_LETTER="C"
  SIG_MODE="C (CR_AUTH_TOKEN + CR_REQUIRE_AGENT_SIG=true — the production default)"
else
  CONFIG_LETTER="B"
  SIG_MODE="B (CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG=false)"
fi

echo "# Using crier from Muster — muster-bridge runnable transcript"
echo
echo "- date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "- repo: $(cd "$REPO_ROOT" && git rev-parse --short HEAD 2>/dev/null || echo 'unknown')"
echo "- config: $SIG_MODE"
echo "- relay: $BASE (scratch port, chosen from $MUSTER_BRIDGE_PORT_BASE+$MUSTER_BRIDGE_PORT_CANDIDATES)"
echo "- consumer: $MUSTER_BRIDGE_AGENT"
echo "- transcript: $TRANSCRIPT"
echo

echo "==> [1/9] build the relay from this checkout"
( cd "$REPO_ROOT" && go build -o "$WORKDIR/crier" ./cmd/server )
echo "    built $WORKDIR/crier"
echo

echo "==> [2/9] start it on :$PORT and prove the listener is the pid we started"
# The guard env is forced (an ambient CR_AUTH_TOKEN or CR_REQUIRE_AGENT_SIG must
# not decide what this run measures), the guard LLM is off so the loop is
# deterministic and keyless, and the backend is in-memory (no CR_DATABASE_URL).
CRIER_PORT="$PORT" CR_AUTH_TOKEN="$MUSTER_BRIDGE_TOKEN" \
  CR_REQUIRE_AGENT_SIG="$MUSTER_BRIDGE_REQUIRE_SIG" CR_GUARD_ENABLED=false \
  "$WORKDIR/crier" &
SERVER_PID=$!
# wait_http_or_die aborts if the started process dies; assert_port_owned then
# proves the port's holder IS that pid (presence is not ownership).
wait_http_or_die "$BASE/health" "$SERVER_PID" "$TRANSCRIPT" "the muster-bridge relay"
assert_port_owned "$PORT" "$SERVER_PID" "the muster-bridge relay"
echo "    pid $SERVER_PID owns :$PORT (asserted), GET /health -> $(code "$BASE/health")"
echo

echo "==> [3/9] discover the spec — /openapi.json and /openapi.yaml are auth-exempt"
check "GET /openapi.json (no token)" 200 "$(code "$BASE/openapi.json")"
check "GET /openapi.yaml (no token)" 200 "$(code "$BASE/openapi.yaml")"
echo "    served spec: openapi $(curl -s "$BASE/openapi.json" | sed -n 's/.*"openapi":"\([^"]*\)".*/\1/p' | head -1)"
echo "    security schemes declared: $(curl -s "$BASE/openapi.json" | grep -o '"[a-zA-Z]*Auth"\|"agentSignature"' | sort -u | tr '\n' ' ')"
echo "    operations carrying agentSignature: $(curl -s "$BASE/openapi.yaml" | grep -c 'agentSignature' ) security entries;"
echo "    the documented source of truth for this document is docs/openapi.yaml in the repo"
echo "    (cmd/server/openapi.yaml is the byte-identical generated copy)."
echo

echo "==> [4/9] register the consumer — bearer token only, NO signature (201)"
openssl genpkey -algorithm ED25519 -out "$WORKDIR/agent.key" >/dev/null 2>&1
PUBKEY=$(openssl pkey -in "$WORKDIR/agent.key" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)
REG_BODY=$(curl -s -X POST "$BASE/agents" "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$MUSTER_BRIDGE_AGENT\",\"public_key\":\"$PUBKEY\",\"capabilities\":[\"inbox\"]}" -w '\n%{http_code}')
check "POST /agents (bearer, no signature)" 201 "$(printf '%s' "$REG_BODY" | tail -1)"
echo "    $(printf '%s' "$REG_BODY" | head -1)"
# The public half is what config C verifies the signature against, so with
# enforcement on a registration WITHOUT it is refused — measured here, because a
# generated client that omits the field gets a 400 and not a client.
NOKEY_BODY=$(curl -s -X POST "$BASE/agents" "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"id":"muster-bridge-no-key"}' -w '\n%{http_code}')
if [ "$MUSTER_BRIDGE_REQUIRE_SIG" = true ]; then
  check "POST /agents without public_key (config C)" 400 "$(printf '%s' "$NOKEY_BODY" | tail -1)"
  echo "    $(printf '%s' "$NOKEY_BODY" | head -1)"
else
  check "POST /agents without public_key (config B)" 201 "$(printf '%s' "$NOKEY_BODY" | tail -1)"
fi
echo

echo "==> [5/9] deliver into its inbox — bearer token only, NO signature (201, 201)"
for n in 1 2; do
  D_BODY=$(curl -s -X POST "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox" "${AUTH[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"payload\":{\"source\":\"muster\",\"text\":\"hello from a Muster-driven integration\",\"n\":$n}}" \
    -w '\n%{http_code}')
  check "POST /agents/$MUSTER_BRIDGE_AGENT/inbox #$n (no signature)" 201 "$(printf '%s' "$D_BODY" | tail -1)"
done
echo "    $(printf '%s' "$D_BODY" | head -1)"
echo "    (the inbox is durable and pull-based: a delivery is stored until it is acked)"
echo

echo "==> [6/9] retrieve — GET /agents/$MUSTER_BRIDGE_AGENT/inbox"
if [ "$MUSTER_BRIDGE_REQUIRE_SIG" = true ]; then
  UNSIGNED_BODY=$(curl -s "${AUTH[@]}" -w '\n%{http_code}' "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox")
  check "unsigned retrieve (config C)" 401 "$(printf '%s' "$UNSIGNED_BODY" | tail -1)"
  echo "    $(printf '%s' "$UNSIGNED_BODY" | head -1)"
  echo "    ^ this is the boundary for a spec-generated (bearer-only) client."
else
  UNSIGNED_BODY=$(curl -s "${AUTH[@]}" -w '\n%{http_code}' "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox")
  check "unsigned retrieve (config B)" 200 "$(printf '%s' "$UNSIGNED_BODY" | tail -1)"
  LEASE_ID=$(printf '%s' "$UNSIGNED_BODY" | head -1 | sed -n 's/.*"lease_id":"\([a-f0-9]*\)".*/\1/p')
  ACK_IDS=$(printf '%s' "$UNSIGNED_BODY" | head -1 | sed -n 's/.*"messages":\[//p' | grep -o '"id":"[a-f0-9]*"' | sed 's/"id":"\([a-f0-9]*\)"/"\1"/' | paste -sd, -)
  echo "    envelope: $(printf '%s' "$UNSIGNED_BODY" | head -1 | sed -n 's/.*"queue_depth":[0-9]*,"leased_count":[0-9]*/&/p' | head -1)"
fi
echo

echo "==> [7/9] sign the request — the leg no generated client can do"
if [ "$MUSTER_BRIDGE_REQUIRE_SIG" = true ]; then
  TS=$(date +%s)
  SIG=$(sig GET "/agents/$MUSTER_BRIDGE_AGENT/inbox" "$TS")
  SIG_BODY=$(curl -s "${AUTH[@]}" -H "X-Agent-ID: $MUSTER_BRIDGE_AGENT" \
    -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG" -w '\n%{http_code}' \
    "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox")
  check "signed retrieve" 200 "$(printf '%s' "$SIG_BODY" | tail -1)"
  LEASE_ID=$(printf '%s' "$SIG_BODY" | head -1 | sed -n 's/.*"lease_id":"\([a-f0-9]*\)".*/\1/p')
  ACK_IDS=$(printf '%s' "$SIG_BODY" | head -1 | sed -n 's/.*"messages":\[//p' | grep -o '"id":"[a-f0-9]*"' | sed 's/"id":"\([a-f0-9]*\)"/"\1"/' | paste -sd, -)
  echo "    signed as: hex(ed25519(\"GET\\n/agents/$MUSTER_BRIDGE_AGENT/inbox\\n$TS\"), agent.key)"
  echo "    lease_id=$LEASE_ID  message_ids=[$ACK_IDS]"
  echo "    envelope: $(printf '%s' "$SIG_BODY" | head -1 | grep -o '"queue_depth":[0-9]*,"leased_count":[0-9]*')"
  echo "    (a signature is bound to METHOD and path: the same key signing a"
  echo "     different path is refused 401, and X-Agent-ID must be the {id} in"
  echo "     the path or the request is refused 403.)"
else
  echo "    skipped — config B: X-Agent-ID / X-Agent-Ts / X-Agent-Sig are ignored"
  BOGUS=$(code "${AUTH[@]}" -H "X-Agent-ID: $MUSTER_BRIDGE_AGENT" -H 'X-Agent-Ts: 1' -H 'X-Agent-Sig: deadbeef' "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox")
  check "a BOGUS signature trio is ignored too (still a read)" 200 "$BOGUS"
  echo "    (enforcement off means the trio is not merely optional — it is not read)"
fi
echo

echo "==> [8/9] ack the lease — 204, and the inbox is empty again"
if [ "$MUSTER_BRIDGE_REQUIRE_SIG" = true ]; then
  TS=$(date +%s)
  SIG=$(sig POST "/agents/$MUSTER_BRIDGE_AGENT/inbox/ack" "$TS")
  check "signed ack (POST /agents/$MUSTER_BRIDGE_AGENT/inbox/ack)" 204 \
    "$(code -X POST "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox/ack" "${AUTH[@]}" \
      -H 'Content-Type: application/json' -H "X-Agent-ID: $MUSTER_BRIDGE_AGENT" \
      -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG" \
      -d "{\"lease_id\":\"$LEASE_ID\",\"message_ids\":[$ACK_IDS]}")"
  TS=$(date +%s)
  SIG=$(sig GET "/agents/$MUSTER_BRIDGE_AGENT/inbox/stats" "$TS")
  check "signed drain check (GET /agents/$MUSTER_BRIDGE_AGENT/inbox/stats)" 200 \
    "$(code "${AUTH[@]}" -H "X-Agent-ID: $MUSTER_BRIDGE_AGENT" -H "X-Agent-Ts: $TS" \
      -H "X-Agent-Sig: $SIG" "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox/stats")"
else
  check "unsigned ack (POST /agents/$MUSTER_BRIDGE_AGENT/inbox/ack)" 204 \
    "$(code -X POST "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox/ack" "${AUTH[@]}" \
      -H 'Content-Type: application/json' \
      -d "{\"lease_id\":\"$LEASE_ID\",\"message_ids\":[$ACK_IDS]}")"
  check "unsigned drain check" 200 "$(code "${AUTH[@]}" "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox/stats")"
fi
echo

echo "==> [9/9] the MEASURED authorization matrix (INT-MUSTER-003)"
echo "    bearer-only client — every request carries Authorization: Bearer <token> and NO"
echo "    X-Agent-ID / X-Agent-Ts / X-Agent-Sig unless the line says otherwise."
echo
if [ "$MUSTER_BRIDGE_REQUIRE_SIG" = true ]; then
  echo "    the five signature-required operations, unsigned (DELETE is last, below):"
  W_INBOX=401; W_STATS=401; W_ACK=401; W_PATCH=401; W_DELETE=401
else
  echo "    the five signature-required operations, unsigned (enforcement OFF in config B; DELETE is last, below):"
  W_INBOX=200; W_STATS=200; W_ACK=404; W_PATCH=200; W_DELETE=204
fi
check "GET  /agents/{id}/inbox (unsigned)" "$W_INBOX" "$(code "${AUTH[@]}" "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox")"
check "GET  /agents/{id}/inbox/stats (unsigned)" "$W_STATS" "$(code "${AUTH[@]}" "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox/stats")"
check "POST /agents/{id}/inbox/ack (unsigned, dummy ids)" "$W_ACK" \
  "$(code -X POST "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox/ack" "${AUTH[@]}" -H 'Content-Type: application/json' \
     -d '{"lease_id":"00000000000000000000000000000000","message_ids":["000000000000000000000000"]}')"
check "PATCH  /agents/{id} (unsigned)" "$W_PATCH" \
  "$(code -X PATCH "$BASE/agents/$MUSTER_BRIDGE_AGENT" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{"capabilities":["inbox"]}')"
echo
echo "    everything else a bearer-only client drives:"
check "GET /health (no token)" 200 "$(code "$BASE/health")"
check "GET /version (no token)" 200 "$(code "$BASE/version")"
check "GET /docs (no token)" 200 "$(code "$BASE/docs")"
check "GET /status (bearer)" 200 "$(code "${AUTH[@]}" "$BASE/status")"
check "GET /status (no token -> refused)" 401 "$(code "$BASE/status")"
check "GET /agents (bearer)" 200 "$(code "${AUTH[@]}" "$BASE/agents")"
check "GET /agents (no token -> refused)" 401 "$(code "$BASE/agents")"
check "GET /agents/{id} (bearer)" 200 "$(code "${AUTH[@]}" "$BASE/agents/$MUSTER_BRIDGE_AGENT")"
check "POST /agents (bearer, new id + public_key)" 201 \
  "$(code -X POST "$BASE/agents" "${AUTH[@]}" -H 'Content-Type: application/json' \
     -d "{\"id\":\"muster-bridge-probe\",\"public_key\":\"$PUBKEY\"}")"
check "POST /agents/{id}/inbox (bearer, deliver)" 201 \
  "$(code -X POST "$BASE/agents/$MUSTER_BRIDGE_AGENT/inbox" "${AUTH[@]}" -H 'Content-Type: application/json' -d '{"payload":{"matrix":"deliver"}}')"
check "GET /relay/topics (bearer)" 200 "$(code "${AUTH[@]}" "$BASE/relay/topics")"
check "GET /mesh/peers (bearer)" 200 "$(code "${AUTH[@]}" "$BASE/mesh/peers")"
check "GET /fed/peers (bearer)" 200 "$(code "${AUTH[@]}" "$BASE/fed/peers")"
check "POST /relay/publish (bearer + X-Agent-ID)" 202 \
  "$(code -X POST "$BASE/relay/publish" "${AUTH[@]}" -H 'Content-Type: application/json' \
     -H "X-Agent-ID: $MUSTER_BRIDGE_AGENT" -d '{"topic":"muster.bridge","event":{"n":1}}')"
echo
# DELETE last: in config B it removes the agent this run registered.
check "DELETE /agents/{id} (unsigned, the 5th signed op)" "$W_DELETE" "$(code -X DELETE "$BASE/agents/$MUSTER_BRIDGE_AGENT" "${AUTH[@]}")"
echo

echo "==> summary"
if [ "$FAILS" -eq 0 ]; then
  echo "    PASS — every measured status matched its claimed code (config $CONFIG_LETTER, CR_REQUIRE_AGENT_SIG=$MUSTER_BRIDGE_REQUIRE_SIG)"
  echo "    transcript: $TRANSCRIPT"
else
  echo "    FAIL — $FAILS measured status(es) did not match the code this run claims" >&2
  echo "    transcript: $TRANSCRIPT" >&2
  exit 1
fi
echo
echo "    A generated client stops at the signature: register, deliver, relay,"
echo "    health/version/status/docs and the agent reads work bearer-only, while"
echo "    retrieve / stats / ack / PATCH / DELETE need the ed25519 signature of"
echo "    the agent's own private key (config C) — or run config B."
