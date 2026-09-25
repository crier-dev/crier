#!/usr/bin/env bash
# e2e-battery.sh — E2E-001 live battery: builds HEAD, starts an env-isolated
# crier server (in-memory registry) on a scratch port, runs the probe gates,
# kills the server, and asserts the port came back free.
#
# Gates (each is a PASS/FAIL line; exit code = FAIL count):
#   health / version / status posture (memory backend, sig required, auth off)
#   SECURITY negatives: unauthenticated /agents, keyless publish (rate limit on)
#   existence-vs-auth: signed inbox GET for a never-registered id -> 404
#   register -> duplicate 409 -> deliver 201 -> stats -> signed retrieve ->
#   signed ack (lease_id + message_ids) -> empty retrieve
#   relay: publish 202 (X-Agent-ID) -> topics listing -> WS receive ->
#   wildcard WS receive (bat.* matches bat.<ts>)
#   ttl: deliver expires_at=1s -> retrieve after 2s -> zero messages
#   foreign-agent inbox isolation: A and B register with their OWN keypairs ->
#   deliver into A (no signature) -> A signed retrieve 200 with the payload
#   round-trip INTACT (base64-decoded == delivered) -> B signed retrieve of A's
#   inbox -> 403 (cross-agent, B's own valid signature) -> B's own inbox 200
#   empty -> A signed ack 204
#
# Usage: scripts/e2e-battery.sh [port]     (default 18782; falls back if taken)
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"
PORT_CANDIDATES=()
for a in "$@"; do PORT_CANDIDATES+=("$a"); done
PORT_CANDIDATES+=(18782 18783 18784 18785)
PORT=""
for p in "${PORT_CANDIDATES[@]}"; do
  [ -z "$p" ] && continue
  if ! ss -tln 2>/dev/null | grep -q ":${p} "; then PORT="$p"; break; fi
done
if [ -z "$PORT" ]; then echo "FATAL: no free scratch port"; exit 2; fi

CRIER="http://localhost:${PORT}"
BAT_TOKEN="e2e-battery-token"
EVID="${EVID:-/tmp/e2e-battery-evidence.jsonl}"
: > "$EVID"
PASS=0; FAIL=0
SERVER_PID=""
WORKDIR="$(mktemp -d)"
trap '[ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null; rm -rf "$WORKDIR"' EXIT

say() { echo "== $*"; }
ev() { printf '{"ts":"%s","probe":"%s","http":%s,"want":%s,"ok":%s}\n' \
  "$(date -u +%FT%TZ)" "$1" "${2:-0}" "${3:-0}" "$4" >> "$EVID"; }

CODE=""; BODY=""; NOAUTH=0
req() { # method path [data] [extra curl args...]
  local method="$1" path="$2" data="${3:-}"; shift 3 2>/dev/null || shift $#
  local out auth_args=()
  [ "$NOAUTH" = 0 ] && auth_args=(-H "Authorization: Bearer ${BAT_TOKEN}")
  if [ -n "$data" ]; then
    out="$(curl -s -w '\n%{http_code}' -X "$method" "$CRIER$path" -H 'Content-Type: application/json' "${auth_args[@]}" "$@" -d "$data")"
  else
    out="$(curl -s -w '\n%{http_code}' -X "$method" "$CRIER$path" "${auth_args[@]}" "$@")"
  fi
  CODE="${out##*$'\n'}"; BODY="${out%$'\n'*}"
}
check() { # desc want [body-contains]
  local desc="$1" want="$2" contains="${3:-}"
  if [ "$CODE" = "$want" ] && { [ -z "$contains" ] || [ "${BODY#*"$contains"}" != "$BODY" ]; }; then
    PASS=$((PASS+1)); echo "PASS  $desc (HTTP $CODE)"; ev "$desc" "$CODE" "$want" true; return 0
  fi
  FAIL=$((FAIL+1)); echo "FAIL  $desc (HTTP ${CODE:-none} want $want)"
  echo "      body: $(printf '%s' "$BODY" | head -c 300)"; ev "$desc" "${CODE:-0}" "$want" false; return 1
}

say "build HEAD"
make build >/dev/null 2>&1 || { echo "FATAL: make build failed"; exit 2; }

say "start server on :${PORT} (memory backend, sig ON, guard OFF, rate limit ON)"
env -i PATH="$PATH" HOME="$HOME" \
  CRIER_PORT="$PORT" CR_GUARD_ENABLED=false CR_REQUIRE_AGENT_SIG=true \
  CR_RATE_LIMIT_PER_MINUTE=600 CR_AUTH_TOKEN="$BAT_TOKEN" \
  ./bin/crier > "$WORKDIR/server.log" 2>&1 &
SERVER_PID=$!

READY=0
for _ in $(seq 1 30); do
  curl -sf "$CRIER/health" >/dev/null 2>&1 && { READY=1; break; }
  sleep 1
done
if [ "$READY" != 1 ]; then echo "FATAL: server never became ready"; tail -5 "$WORKDIR/server.log"; exit 2; fi
HOLDER="$(ss -tlnp 2>/dev/null | grep ":${PORT} " | grep -o "pid=${SERVER_PID}," | head -1)"
if [ "$HOLDER" != "pid=${SERVER_PID}," ]; then
  echo "FATAL: :${PORT} holder is not the pid we started (got '${HOLDER:-none}')"; exit 2
fi
echo "server up: pid $SERVER_PID on :${PORT} (ownership asserted)"

# --- signing helpers (ed25519 trio, OpenSSL 3 -rawin) ---
openssl genpkey -algorithm ED25519 -out "$WORKDIR/agent.key" 2>/dev/null
PUBHEX="$(openssl pkey -in "$WORKDIR/agent.key" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)"
sign_with() { # KEYFILE METHOD PATH TS  -> SIGN_SIG / SIGN_TS
  printf '%s\n%s\n%s' "$2" "$3" "$4" > "$WORKDIR/pl.txt"
  SIGN_SIG="$(openssl pkeyutl -sign -rawin -inkey "$1" -in "$WORKDIR/pl.txt" 2>/dev/null | xxd -p -c 128)"
  SIGN_TS="$4"
}
sign() { sign_with "$WORKDIR/agent.key" "$@"; }   # the battery identity's own key
sreq_with() { # KEYFILE AGENTID METHOD PATH [data] — signed as AGENTID with ITS OWN key
  local key="$1" agent="$2" method="$3" path="$4" data="${5:-}"
  sign_with "$key" "$method" "$path" "$(date +%s)"
  local args=(-H "X-Agent-ID: $agent" -H "X-Agent-Ts: $SIGN_TS" -H "X-Agent-Sig: $SIGN_SIG")
  if [ -n "$data" ]; then req "$method" "$path" "$data" "${args[@]}"; else req "$method" "$path" "" "${args[@]}"; fi
}
sreq() { # METHOD PATH [data] — signed with the battery key as $AGENT
  sreq_with "$WORKDIR/agent.key" "$AGENT" "$@"
}

AGENT="e2e-bat-$(date +%s)"
TOPIC="bat.${AGENT}"

say "posture"
req GET /health;                              check "health 200" 200
req GET /version;                             check "version 200 json" 200 '"commit"'
req GET /status;                              check "status: memory backend" 200 '"registry_backend":"memory"'
check "status: signatures required" "$CODE" '"require_agent_signature":true'
check "status: auth enforced (battery token)" "$CODE" '"auth_enabled":true'

say "security negatives"
NOAUTH=1 req GET /agents; NOAUTH=0
check "unauthenticated /agents -> 401" 401
req POST /relay/publish "{\"topic\":\"$TOPIC\",\"event\":{\"x\":1}}"; check "bearer publish without X-Agent-ID -> 401 (rate limiter on)" 401
NOAUTH=1 req POST /agents "{\"id\":\"nope-$AGENT\"}"; NOAUTH=0
check "unauthenticated register -> 401" 401
sreq GET "/agents/ghost-e2e-xyz/inbox";       check "signed inbox of never-registered id -> 404 (existence before sig window)" 404

say "agent lifecycle"
req POST /agents "{\"id\":\"$AGENT\",\"public_key\":\"$PUBHEX\",\"capabilities\":[\"e2e\"]}"; check "register 201" 201
req POST /agents "{\"id\":\"$AGENT\",\"public_key\":\"$PUBHEX\"}";                            check "duplicate register 409" 409
req POST "/agents/$AGENT/inbox" '{"payload":{"e2e":true,"n":1}}';                             check "deliver 201" 201 '"id"'
sreq GET "/agents/$AGENT/inbox/stats";       check "signed stats 200 (queue_depth>=1)" 200 '"queue_depth":1'
sreq GET "/agents/$AGENT/inbox";             check "signed retrieve 200 (leased message)" 200 '"lease_id"'
printf '%s' "$BODY" > "$WORKDIR/retrieve.json"
LEASE_ID="$(python3 -c "import json;print(json.load(open('$WORKDIR/retrieve.json'))['lease_id'])" 2>/dev/null)"
MSG_ID="$(python3 -c "import json;print(json.load(open('$WORKDIR/retrieve.json'))['messages'][0]['id'])" 2>/dev/null)"
if [ -n "$LEASE_ID" ] && [ -n "$MSG_ID" ]; then
  sreq POST "/agents/$AGENT/inbox/ack" "{\"lease_id\":\"$LEASE_ID\",\"message_ids\":[\"$MSG_ID\"]}"
  check "signed ack 204 (lease+ids)" 204
  sreq GET "/agents/$AGENT/inbox"; check "retrieve after ack: zero messages" 200 '"messages":[]'
else
  FAIL=$((FAIL+1)); echo "FAIL  ack path (could not extract lease_id/message id from retrieve body)"
fi

say "relay (publish + topics + WS)"
python3 - "$PORT" "$BAT_TOKEN" "$TOPIC" > "$WORKDIR/ws.out" 2>&1 <<'PYEOF' &
import sys, json
from websockets.sync.client import connect
port, token, topic = sys.argv[1], sys.argv[2], sys.argv[3]
auth = {"Authorization": f"Bearer {token}"}
ws1 = connect(f"ws://localhost:{port}/relay/subscribe/{topic}", additional_headers=auth)
print("READY exact", flush=True)
ws2 = connect(f"ws://localhost:{port}/relay/subscribe/bat.*", additional_headers=auth)
print("READY wildcard", flush=True)
m1 = json.loads(ws1.recv(timeout=20))
print("RECV_EXACT", json.dumps(m1.get("topic")), flush=True)
m2 = json.loads(ws2.recv(timeout=20))
print("RECV_WILDCARD", json.dumps(m2.get("topic")), flush=True)
PYEOF
WS_PID=$!
for _ in $(seq 1 20); do grep -q "READY wildcard" "$WORKDIR/ws.out" 2>/dev/null && break; sleep 1; done
req POST /relay/publish "{\"topic\":\"$TOPIC\",\"event\":{\"hello\":\"ws\"}}" -H "X-Agent-ID: e2e-battery"
check "publish with X-Agent-ID 202" 202
req GET /relay/topics;                        check "topics lists published topic" 200 "$TOPIC"
wait "$WS_PID" 2>/dev/null
WS_OUT="$(cat "$WORKDIR/ws.out" 2>/dev/null)"
if printf '%s' "$WS_OUT" | grep -q "RECV_EXACT \"$TOPIC\""; then
  PASS=$((PASS+1)); echo "PASS  ws subscriber received exact topic"; ev "ws exact topic" 0 0 true
else
  FAIL=$((FAIL+1)); echo "FAIL  ws subscriber missed exact topic"; echo "      ws log: $(printf '%s' "$WS_OUT" | head -c 200)"; ev "ws exact topic" 0 0 false
fi
if printf '%s' "$WS_OUT" | grep -q "RECV_WILDCARD \"$TOPIC\""; then
  PASS=$((PASS+1)); echo "PASS  wildcard ws (bat.*) received publish"; ev "ws wildcard" 0 0 true
else
  FAIL=$((FAIL+1)); echo "FAIL  wildcard ws (bat.*) missed publish"; ev "ws wildcard" 0 0 false
fi

say "ttl expiry"
TTL_AGENT="e2e-ttl-${AGENT}"
req POST /agents "{\"id\":\"$TTL_AGENT\",\"public_key\":\"$PUBHEX\"}";  check "ttl agent register 201" 201
req POST "/agents/$TTL_AGENT/inbox" '{"payload":{"ttl":true},"ttl_seconds":1}'; check "ttl deliver 201" 201
sleep 2
sign GET "/agents/$TTL_AGENT/inbox" "$(date +%s)"
req GET "/agents/$TTL_AGENT/inbox" "" -H "X-Agent-ID: $TTL_AGENT" -H "X-Agent-Ts: $SIGN_TS" -H "X-Agent-Sig: $SIGN_SIG"
if [ "$CODE" = "200" ] && [ "${BODY#*'"messages":[]'}" != "$BODY" ]; then
  PASS=$((PASS+1)); echo "PASS  expired message no longer retrievable"; ev "ttl expiry" 200 200 true
else
  FAIL=$((FAIL+1)); echo "FAIL  ttl expiry (HTTP $CODE)"; echo "      body: $(printf '%s' "$BODY" | head -c 200)"; ev "ttl expiry" "${CODE:-0}" 200 false
fi

say "foreign-agent inbox isolation"
# Two foreign identities (a second and third keypair, not the battery key):
# deliver needs NO signature, so B's delivery lands in A's inbox, but the
# signed reads are agent-owned — B's valid signature over A's path is refused
# 403, and B's own inbox stays empty. The payload round-trip (base64-decode
# the retrieved payload and compare to the delivered string) is the tick-142
# precedent: retrieval must hand back the delivered bytes, not a re-encoding.
FA="e2e-fa-${AGENT}"; FB="e2e-fb-${AGENT}"
RT_PAYLOAD='{"round_trip":"cr-consensus-1","text":"foreign agent payload"}'
openssl genpkey -algorithm ED25519 -out "$WORKDIR/fa.key" 2>/dev/null
PUBHEX_A="$(openssl pkey -in "$WORKDIR/fa.key" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)"
openssl genpkey -algorithm ED25519 -out "$WORKDIR/fb.key" 2>/dev/null
PUBHEX_B="$(openssl pkey -in "$WORKDIR/fb.key" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)"
req POST /agents "{\"id\":\"$FA\",\"public_key\":\"$PUBHEX_A\",\"capabilities\":[\"inbox\"]}"; check "foreign agent A register 201 (own keypair)" 201
req POST /agents "{\"id\":\"$FB\",\"public_key\":\"$PUBHEX_B\"}";                                check "foreign agent B register 201 (different keypair)" 201
req POST "/agents/$FA/inbox" "{\"payload\":$RT_PAYLOAD}"; check "deliver into A's inbox 201 (deliver needs no signature)" 201 '"id"'
sreq_with "$WORKDIR/fa.key" "$FA" GET "/agents/$FA/inbox"
check "A signed retrieve 200 (leased)" 200 '"lease_id"'
printf '%s' "$BODY" > "$WORKDIR/fa_retrieve.json"
python3 - "$WORKDIR/fa_retrieve.json" "$RT_PAYLOAD" > "$WORKDIR/fa_rt.out" 2>&1 <<'PYEOF'
import base64, json, sys
body = json.load(open(sys.argv[1]))
want = sys.argv[2]
got = base64.b64decode(body["messages"][0]["payload"]).decode()
print("MATCH" if got == want else "MISMATCH got=%r want=%r" % (got, want))
PYEOF
if head -1 "$WORKDIR/fa_rt.out" 2>/dev/null | grep -q '^MATCH$'; then
  PASS=$((PASS+1)); echo "PASS  foreign payload round-trip intact (base64-decoded == delivered)"; ev "foreign payload round-trip" 0 0 true
else
  FAIL=$((FAIL+1)); echo "FAIL  foreign payload round-trip (base64-decoded != delivered)"
  echo "      $(head -c 300 "$WORKDIR/fa_rt.out" 2>/dev/null)"; ev "foreign payload round-trip" 0 0 false
fi
sreq_with "$WORKDIR/fb.key" "$FB" GET "/agents/$FA/inbox"
check "B signed retrieve of A's inbox -> 403 (cross-agent, B's own valid sig)" 403
sreq_with "$WORKDIR/fb.key" "$FB" GET "/agents/$FB/inbox"
check "B's own inbox retrieve 200 empty (isolation is per-agent)" 200 '"messages":[]'
FA_LEASE="$(python3 -c "import json;print(json.load(open('$WORKDIR/fa_retrieve.json'))['lease_id'])" 2>/dev/null)"
FA_MSG="$(python3 -c "import json;print(json.load(open('$WORKDIR/fa_retrieve.json'))['messages'][0]['id'])" 2>/dev/null)"
if [ -n "$FA_LEASE" ] && [ -n "$FA_MSG" ]; then
  sreq_with "$WORKDIR/fa.key" "$FA" POST "/agents/$FA/inbox/ack" "{\"lease_id\":\"$FA_LEASE\",\"message_ids\":[\"$FA_MSG\"]}"
  check "A signed ack 204 (lease+ids)" 204
else
  FAIL=$((FAIL+1)); echo "FAIL  foreign ack path (no lease_id/message id in A's retrieve body)"; ev "foreign ack" 0 204 false
fi

say "teardown"
kill "$SERVER_PID" 2>/dev/null; wait "$SERVER_PID" 2>/dev/null; SERVER_PID=""
sleep 1
if ss -tln 2>/dev/null | grep -q ":${PORT} "; then
  FAIL=$((FAIL+1)); echo "FAIL  port :${PORT} still held after kill"
else
  PASS=$((PASS+1)); echo "PASS  server killed, port freed"
fi

echo ""
echo "BATTERY RESULT: $PASS pass / $FAIL fail (port $PORT, agent $AGENT)"
echo "{\"battery\":\"e2e-001\",\"ts\":\"$(date -u +%FT%TZ)\",\"port\":$PORT,\"pass\":$PASS,\"fail\":$FAIL}" >> "$EVID"
exit "$FAIL"
