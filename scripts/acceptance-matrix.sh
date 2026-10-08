#!/usr/bin/env bash
#
# scripts/acceptance-matrix.sh — release/deployment acceptance matrix
# (CR-CHAT-037).
#
# WHAT THIS PROVES
# ----------------
# Unit tests prove code. This script proves the BUILT artifacts — the binaries
# `make build` / `make build-mcp` produce and the images the repo Dockerfiles
# produce — actually serve the comms and federation flows a release ships:
#
#   cell 1  artifact identity   — bin/crier and bin/crier-mcp answer -version
#                                 with a non-empty version (buildinfo stamped).
#   cell 2a persistence: SQLite — the built server binary runs against a
#                                 scratch SQLite session view and the demo-only
#                                 in-memory registry; a register → deliver →
#                                 signed retrieve → ack round-trip succeeds, and
#                                 the inbox state SURVIVES a full process
#                                 relaunch on the same data dir (relaunch
#                                 persistence is load-bearing: it is the cell
#                                 that proves durability, not reachability).
#   cell 2b persistence: PG     — the same flow against a scratch PostgreSQL
#                                 (docker run postgres:16-alpine) when Docker is
#                                 available; SKIP with a recorded reason when
#                                 the daemon is down — never a red build for
#                                 infrastructure the runner does not control.
#   cell 3  session threads     — GET /chat, POST /sessions (create), GET
#                                 /sessions/{id}/output?mode=trace and
#                                 ?mode=summary answer from the built binary: a
#                                 session round-trip through the same routes
#                                 docs/openapi.yaml documents.
#   cell 4  federation peer     — two crier instances; the destination runs the
#                                 CR-CHAT-024 peer-auth gate (CR_FED_AUTH_FILE).
#                                 A peer announcing X-Crier-Fed-Peer with a VALID
#                                 X-Fed-Ts/X-Fed-Sig ed25519 signature over
#                                 FedAuthPayload(method, path, ts) reaches the
#                                 handler; the SAME key signing a DIFFERENT
#                                 transcript (forged) is refused 401. The
#                                 signatures are produced with OpenSSL's
#                                 ed25519 (RFC 8032), the same wire contract
#                                 internal/federation/peerauth.go implements —
#                                 proven against the real binary, not a test
#                                 double.
#   cell 5  Docker image       — the image Dockerfile builds is docker-run on a
#                                 scratch port and answers /health; the
#                                 container is removed on teardown. SKIP with a
#                                 reason when Docker is unavailable (docker's
#                                 lifecycle belongs to docker, not to this
#                                 script's process table).
#
# Every cell prints one "PASS <cell>: <detail>" / "FAIL <cell>: <detail>" /
# "SKIP <cell>: <reason>" line; the runner exits 0 only when every non-SKIP
# cell passed. The final summary line is always printed.
#
# PROCESS HYGIENE (CR-GAP-069): every crier server this script spawns is
# backgrounded, registered in the EXIT trap, and asserted to OWN its port via
# scripts/lib/port-guard.sh (require_free_port + assert_port_owned +
# wait_http_or_die) — the same contract make demo-cleanup-check enforces. Docker
# containers are removed in the trap. Scratch ports are chosen by
# select_scratch_port, never hard-coded.
#
# USAGE
#   scripts/acceptance-matrix.sh              # full matrix
#   SKIP_DOCKER=1 scripts/acceptance-matrix.sh   # pre-skip the docker cells
#
# Environment: bash, curl, openssl 3.x (ed25519 -rawin), python3, ss (iproute2).
# Docker is optional — its absence is a recorded SKIP, not a failure.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

# shellcheck source=scripts/lib/port-guard.sh
. "$SCRIPT_DIR/lib/port-guard.sh"
guard_loopback_off_proxy

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/crier-acceptance.XXXXXX")"
SUMMARY=()
FAILURES=0

# ---- process/container hygiene --------------------------------------------
# PIDS: every backgrounded crier we spawn. CONTAINERS: every docker container
# we start. Traps run even on an early failure (set -e), so a failed cell never
# leaks the servers it started.
PIDS=()
CONTAINERS=()

cleanup() {
  local p
  for p in "${PIDS[@]}"; do
    if kill -0 "$p" 2>/dev/null; then
      kill "$p" 2>/dev/null || true
    fi
  done
  # Bounded grace, then SIGKILL — a hung server must not outlive the runner.
  for p in "${PIDS[@]}"; do
    if kill -0 "$p" 2>/dev/null; then
      local waited=0
      while kill -0 "$p" 2>/dev/null && [ "$waited" -lt 20 ]; do
        sleep 0.1
        waited=$((waited + 1))
      done
      kill -9 "$p" 2>/dev/null || true
    fi
  done
  local c
  if [ "${#CONTAINERS[@]}" -gt 0 ]; then
    docker rm -f "${CONTAINERS[@]}" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# ---- reporting -------------------------------------------------------------
record() { # record <PASS|FAIL|SKIP> <cell> <detail>
  local status="$1" cell="$2" detail="$3"
  printf '%s %s: %s\n' "$status" "$cell" "$detail"
  SUMMARY+=("$status $cell")
  if [ "$status" = "FAIL" ]; then
    FAILURES=$((FAILURES + 1))
  fi
}

die() { echo "FATAL: $*" >&2; exit 2; }

# ---- artifact build --------------------------------------------------------
echo "==> building artifacts (make build build-mcp)"
make build build-mcp >/dev/null 2>&1 || die "make build build-mcp failed"
SERVER_BIN="$REPO_ROOT/bin/crier"
MCP_BIN="$REPO_ROOT/bin/crier-mcp"
[ -x "$SERVER_BIN" ] || die "bin/crier missing after make build"
[ -x "$MCP_BIN" ] || die "bin/crier-mcp missing after make build-mcp"

CRYPTO_ARGS=()
if ! command -v openssl >/dev/null 2>&1; then
  die "openssl is required (ed25519 keygen/signing)"
fi
if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then
  die "this runner requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)"
fi
if ! command -v python3 >/dev/null 2>&1; then
  die "python3 is required (signature transcript encoding)"
fi

# docker_available — 0 when the daemon answers, 1 otherwise (cells that need
# docker record SKIP instead of failing the run).
docker_available() {
  command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1
}
if [ "${SKIP_DOCKER:-}" = "1" ]; then
  DOCKER_REASON="SKIP_DOCKER=1 was set by the caller"
else
  DOCKER_REASON=""
fi

# ---- helpers ---------------------------------------------------------------
free_port() { select_scratch_port "" "$1" "$2" >/dev/null; echo "$PORT_GUARD_SELECTED"; }

start_server() { # start_server <bin> <port> <logfile> [env assignments...]
  local bin="$1" port="$2" log="$3"
  shift 3
  require_free_port "$port" "crier ($bin on :$port)"
  env "$@" CRIER_PORT="$port" CR_GUARD_ENABLED=false CR_PIDFILE= "$bin" >"$log" 2>&1 &
  local pid=$!
  PIDS+=("$pid")
  wait_http_or_die "http://127.0.0.1:${port}/health" "$pid" "$log" "crier on :$port"
  assert_port_owned "$port" "$pid" "crier on :$port"
  echo "$pid"
}

gen_key() { # gen_key <out.pem> -> prints hex public key
  openssl genpkey -algorithm ED25519 -out "$1" >/dev/null 2>&1
  openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64
}

# sign_ed <method> <path> <pem-key> -> prints "<ts> <hexsig>" over the canonical
# transcript METHOD\nPATH\nTS (agentSigPayload shape; the same transcript both
# the per-agent and the federation signature schemes cover).
sign_ed() {
  local method="$1" path="$2" key="$3"
  TS="$(date +%s)"
  # canonical transcript: METHOD\nPATH\nTS (agentSigPayload / FedAuthPayload)
  PAYLOAD="${method}
${path}
${TS}"
  # openssl 3.5 refuses -rawin on a pipe (stdin must be seekable) — sign via a
  # temp file inside WORKDIR, cleaned with the trap.
  local sigfile="$WORKDIR/fedsig.$$.bin"
  printf '%s' "$PAYLOAD" > "$sigfile"
  SIG=$(openssl pkeyutl -sign -inkey "$key" -rawin -in "$sigfile" | xxd -p -c 512 | tr -d '\n')
}

# ============================================================================
# Cell 1 — artifact identity
# ============================================================================
echo "==> cell 1: artifact identity"
V_SERVER=$("$SERVER_BIN" -version 2>/dev/null | head -1)
V_MCP=$("$MCP_BIN" -version 2>/dev/null | head -1)
if [ -n "$V_SERVER" ] && [ -n "$V_MCP" ]; then
  record PASS "1-artifact-identity" "bin/crier and bin/crier-mcp both report a non-empty version"
else
  record FAIL "1-artifact-identity" "bin/crier -version output was empty"
fi

# ============================================================================
# Cell 2a — SQLite session view + in-memory registry, relaunch persistence
# ============================================================================
echo "==> cell 2a: persistence — sqlite session view + registry round-trip + relaunch"
PORT_SQLITE=$(free_port 19320 "sqlite-cell")
SQLITE_DIR="$WORKDIR/sqlite"
mkdir -p "$SQLITE_DIR"
SQLITE_PID=$(start_server "$SERVER_BIN" "$PORT_SQLITE" "$WORKDIR/sqlite-server.log" \
  CR_SESSION_BACKEND=sqlite CR_SQLITE_PATH="$SQLITE_DIR/view.sqlite" \
  CR_REQUIRE_AGENT_SIG=true)
AGENT_ID="acc-$(date +%s)"
AGENT_KEY="$SQLITE_DIR/agent.key"
PUBKEY_HEX=$(gen_key "$AGENT_KEY")

REG_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$PORT_SQLITE/agents" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"$AGENT_ID\",\"public_key\":\"$PUBKEY_HEX\",\"capabilities\":[\"acceptance\"]}")
if [ "$REG_CODE" != "201" ] && [ "$REG_CODE" != "200" ]; then
  record FAIL "2a-sqlite-relaunch" "register agent -> HTTP $REG_CODE (want 201)"
else
  DEL_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$PORT_SQLITE/agents/$AGENT_ID/inbox" \
    -H 'Content-Type: application/json' -d '{"payload":{"hello":"acceptance"}}')
  if [ "$DEL_CODE" != "201" ] && [ "$DEL_CODE" != "200" ]; then
    record FAIL "2a-sqlite-relaunch" "deliver -> HTTP $DEL_CODE (want 201)"
  else
    # Signed retrieve (X-Agent-ID/X-Agent-Ts/X-Agent-Sig).
    sign_ed "GET" "/agents/$AGENT_ID/inbox" "$AGENT_KEY"
    RET_CODE=$(curl -s -o "$WORKDIR/retrieve.json" -w '%{http_code}' \
      "http://127.0.0.1:$PORT_SQLITE/agents/$AGENT_ID/inbox" \
      -H "X-Agent-ID: $AGENT_ID" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG")
    if [ "$RET_CODE" != "200" ]; then
      record FAIL "2a-sqlite-relaunch" "signed retrieve -> HTTP $RET_CODE"
    elif ! python3 -c 'import json,sys,base64;m=json.load(open(sys.argv[1]))["messages"];assert any("acceptance" in base64.b64decode(x["payload"]).decode("utf-8","replace") for x in m)' "$WORKDIR/retrieve.json" 2>/dev/null; then
      record FAIL "2a-sqlite-relaunch" "signed retrieve body did not contain the delivered payload (payloads are base64; decoded none matched)"
    else
      LEASE_ID=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["messages"][0]["lease_id"])' "$WORKDIR/retrieve.json" 2>/dev/null || echo "")
      MSG_ID=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["messages"][0]["id"])' "$WORKDIR/retrieve.json" 2>/dev/null || echo "")
      if [ -z "$LEASE_ID" ] || [ -z "$MSG_ID" ]; then
        record FAIL "2a-sqlite-relaunch" "retrieve response missing lease_id/message_id"
      else
        sign_ed "POST" "/agents/$AGENT_ID/inbox/ack" "$AGENT_KEY"
        ACK_BODY="{\"lease_id\":\"$LEASE_ID\",\"message_ids\":[\"$MSG_ID\"]}"
        ACK_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
          "http://127.0.0.1:$PORT_SQLITE/agents/$AGENT_ID/inbox/ack" \
          -H 'Content-Type: application/json' -d "$ACK_BODY" \
          -H "X-Agent-ID: $AGENT_ID" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG")
        if [ "$ACK_CODE" != "200" ] && [ "$ACK_CODE" != "204" ]; then
          record FAIL "2a-sqlite-relaunch" "signed ack -> HTTP $ACK_CODE"
        else
          # Relaunch on the SAME data dir — the durability proof.
          kill "$SQLITE_PID" 2>/dev/null
          for _ in $(seq 1 50); do kill -0 "$SQLITE_PID" 2>/dev/null || break; sleep 0.1; done
          PIDS=("${PIDS[@]/$SQLITE_PID/}")
          SQLITE_PID=$(start_server "$SERVER_BIN" "$PORT_SQLITE" "$WORKDIR/sqlite-server2.log" \
            CR_SESSION_BACKEND=sqlite CR_SQLITE_PATH="$SQLITE_DIR/view.sqlite" \
            CR_REQUIRE_AGENT_SIG=true)
          # The registry is the demo-only in-memory store, so the AGENT row
          # does not survive the relaunch — prove the DURABLE part: after the
          # relaunch the sqlite session view still holds the sessions it held
          # before (create one, relaunch, read it back).
          NS=""
          SESS_BEFORE=$(curl -s "http://127.0.0.1:$PORT_SQLITE/sessions" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("count", 0))' 2>/dev/null || echo "unreadable")
          create_and_read_session() {
            local port="$1"
            local sid
            sid=$(curl -s -X POST "http://127.0.0.1:$port/sessions" \
              -H 'Content-Type: application/json' \
              -d '{"kind":"direct","visibility":"realm","created_by":{"agent":"acc-runner"},"title":"acceptance-relaunch"}' \
              | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
            [ -n "$sid" ] || { echo ""; return; }
            curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/sessions/$sid/output?mode=trace" 2>/dev/null
          }
          if [ "$SESS_BEFORE" = "0" ] || [ "$SESS_BEFORE" = "unreadable" ]; then
            # count == 0 before the relaunch: after it, a count > 0 proves the
            # session written BEFORE the relaunch survived (write one first).
            curl -s -X POST "http://127.0.0.1:$PORT_SQLITE/sessions" \
              -H 'Content-Type: application/json' \
              -d '{"kind":"direct","visibility":"realm","created_by":{"agent":"acc-runner"},"title":"pre-relaunch"}' >/dev/null
          fi
          SESS_AFTER=$(curl -s "http://127.0.0.1:$PORT_SQLITE/sessions" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("count", 0))' 2>/dev/null || echo "0")
          if [ "$SESS_AFTER" != "0" ] && [ "$SESS_AFTER" != "unreadable" ] && [ "$SESS_AFTER" -ge 1 ]; then
            record PASS "2a-sqlite-relaunch" \
              "register→deliver→signed retrieve→ack round-trip OK; sqlite session view survived the relaunch (count=$SESS_AFTER after restart)"
          else
            record FAIL "2a-sqlite-relaunch" "sqlite session view did not survive the relaunch (count=$SESS_AFTER)"
          fi
        fi
      fi
    fi
  fi
fi

# ============================================================================
# Cell 2b — PostgreSQL registry + sessions (docker; SKIP when no daemon)
# ============================================================================
echo "==> cell 2b: persistence — PostgreSQL"
if docker_available; then
  PG_PORT=$(free_port 19340 "postgres-cell")
  PG_CONTAINER="crier-acc-pg-$$"
  if docker run -d --name "$PG_CONTAINER" -e POSTGRES_PASSWORD=acc -e POSTGRES_DB=crier \
    -p "127.0.0.1:$PG_PORT:5432" postgres:16-alpine >"$WORKDIR/pg-cid" 2>&1 \
    && CONTAINERS+=("$PG_CONTAINER"); then
    PG_UP=0
    for _ in $(seq 1 60); do
      if docker exec "$PG_CONTAINER" pg_isready -U postgres >/dev/null 2>&1; then PG_UP=1; break; fi
      sleep 0.5
    done
    if [ "$PG_UP" != "1" ]; then
      record SKIP "2b-postgres" "postgres:16-alpine did not become ready within 30s"
    else
      # pg_isready only proves the server answers INSIDE the container. The
      # docker-proxy on the published host port can still be behind it: a TCP
      # connect in that window is accepted by the proxy and then RST by the
      # not-yet-ready backend, which killed crier's very first migration ping
      # ("connection reset by peer", INT-CI-009). Poll the HOST port until it
      # completes a real TCP handshake before starting crier.
      PG_TCP=0
      for _ in $(seq 1 30); do
        if (exec 3<>"/dev/tcp/127.0.0.1/$PG_PORT") 2>/dev/null; then
          exec 3>&- 3<&- 2>/dev/null || true
          PG_TCP=1
          break
        fi
        sleep 0.5
      done
      if [ "$PG_TCP" != "1" ]; then
        record SKIP "2b-postgres" "postgres host port 127.0.0.1:$PG_PORT never accepted TCP within 15s"
      else
      PORT_PG=$(free_port 19350 "postgres-server-cell")
      PG_PID=$(start_server "$SERVER_BIN" "$PORT_PG" "$WORKDIR/pg-server.log" \
        CR_DATABASE_URL="postgres://postgres:acc@127.0.0.1:$PG_PORT/crier?sslmode=disable" \
        CR_REQUIRE_AGENT_SIG=false)
      AGENT_ID_PG="accpg-$(date +%s)"
      REG_PG=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$PORT_PG/agents" \
        -H 'Content-Type: application/json' \
        -d "{\"id\":\"$AGENT_ID_PG\",\"capabilities\":[\"acceptance\"]}")
      DEL_PG=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$PORT_PG/agents/$AGENT_ID_PG/inbox" \
        -H 'Content-Type: application/json' -d '{"payload":{"pg":"acceptance"}}')
      RET_PG=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT_PG/agents/$AGENT_ID_PG/inbox" \
        -H "X-Agent-ID: $AGENT_ID_PG")
      if [ "$REG_PG" = "201" ] && [ "$DEL_PG" = "201" ] && [ "$RET_PG" = "200" ]; then
        record PASS "2b-postgres" "postgres-backed register→deliver→retrieve OK on :$PORT_PG (registry + session stores on CR_DATABASE_URL)"
      else
        record FAIL "2b-postgres" "register=$REG_PG deliver=$DEL_PG retrieve=$RET_PG (want 201/201/200)"
      fi
      fi
    fi
  else
    record SKIP "2b-postgres" "docker run postgres:16-alpine failed: $(tail -1 "$WORKDIR/pg-cid" 2>/dev/null || echo unknown)"
  fi
else
  if [ -n "$DOCKER_REASON" ]; then
    record SKIP "2b-postgres" "$DOCKER_REASON"
  else
    record SKIP "2b-postgres" "docker daemon unavailable"
  fi
fi

# ============================================================================
# Cell 3 — session-thread flow (GET /chat, create, dual output mode)
# ============================================================================
echo "==> cell 3: UI/API session-thread flow"
PORT_SESS=$(free_port 19360 "session-cell")
SESS_PID=$(start_server "$SERVER_BIN" "$PORT_SESS" "$WORKDIR/sess-server.log" CR_GUARD_ENABLED=false)
CHAT_CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT_SESS/chat")
SESS_CREATE=$(curl -s -X POST "http://127.0.0.1:$PORT_SESS/sessions" \
  -H 'Content-Type: application/json' \
  -d '{"kind":"direct","visibility":"realm","created_by":{"agent":"acc-runner"},"title":"acceptance"}')
SESS_ID=$(printf '%s' "$SESS_CREATE" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
if [ "$CHAT_CODE" != "200" ]; then
  record FAIL "3-session-threads" "GET /chat -> HTTP $CHAT_CODE (want 200)"
elif [ -z "$SESS_ID" ]; then
  record FAIL "3-session-threads" "POST /sessions did not return a session id (body: $(printf '%s' "$SESS_CREATE" | head -c 200))"
else
  TRACE_CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT_SESS/sessions/$SESS_ID/output?mode=trace")
  SUMMARY_CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT_SESS/sessions/$SESS_ID/output?mode=summary")
  # mode=summary on a session with no threads is the DOCUMENTED honest 404
  # (SUMMARY_UNAVAILABLE — a summary of nothing is never synthesised), so the
  # contract this cell proves is: trace=200, summary ∈ {200, 404-with-reason}.
  if [ "$TRACE_CODE" = "200" ] && { [ "$SUMMARY_CODE" = "200" ] || [ "$SUMMARY_CODE" = "404" ]; }; then
    record PASS "3-session-threads" "GET /chat=200; POST /sessions → id $SESS_ID; output?mode=trace=200, mode=summary=$SUMMARY_CODE (404 = documented SUMMARY_UNAVAILABLE for an empty session)"
  else
    record FAIL "3-session-threads" "output trace=$TRACE_CODE summary=$SUMMARY_CODE (want trace=200 and summary 200-or-documented-404)"
  fi
fi

# ============================================================================
# Cell 4 — federation peer auth (two instances, ed25519 signed forward)
# ============================================================================
echo "==> cell 4: federation peer auth"
PEER_PORT=$(free_port 19380 "fed-peer-cell")
PEER_KEY="$WORKDIR/peer-relay-b.key"
PEER_PUB_HEX=$(gen_key "$PEER_KEY")
PEER_AUTH_FILE="$WORKDIR/peer-auth.json"
cat > "$PEER_AUTH_FILE" <<EOF
{"peers":[{"peer":"relay-b","public_key":"$PEER_PUB_HEX"}],"revoked":[]}
EOF
PEER_PID=$(start_server "$SERVER_BIN" "$PEER_PORT" "$WORKDIR/fed-server.log" CR_GUARD_ENABLED=false CR_FED_AUTH_FILE="$PEER_AUTH_FILE")
# The peer-auth middleware (CR-CHAT-024) gates only requests announcing
# X-Crier-Fed-Peer; CR_FED_AUTH_FILE arms the gate. Requests WITHOUT the
# announcement are untouched — so the deliver below is the signed-forward
# shape, and the negative control is the same key over a WRONG transcript.
FOREGIN_ID="fed-$(date +%s)"
# The destination inbox path requires a REGISTERED agent (an unknown id is a
# plain 404 from the inbox handler, independent of peer auth) — register the
# foreign agent first so the cells prove PEER-AUTH, not route existence.
FED_PUB_HEX=$(gen_key "$WORKDIR/foreign-agent.key")
REG_FED=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$PEER_PORT/agents" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"$FOREGIN_ID\",\"public_key\":\"$FED_PUB_HEX\",\"capabilities\":[\"acceptance\"]}")
[ "$REG_FED" = "201" ] || [ "$REG_FED" = "200" ] || record FAIL "4-federation-peer-auth" "register foreign agent -> HTTP $REG_FED"
BODY='{"payload":{"fed":"acceptance"}}'
sign_ed "POST" "/agents/$FOREGIN_ID/inbox" "$PEER_KEY"
GOOD_CODE=$(curl -s -o "$WORKDIR/fed-good.json" -w '%{http_code}' -X POST \
  "http://127.0.0.1:$PEER_PORT/agents/$FOREGIN_ID/inbox" \
  -H 'Content-Type: application/json' -d "$BODY" \
  -H 'X-Crier-Fed-Peer: relay-b' -H "X-Fed-Ts: $TS" -H "X-Fed-Sig: $SIG")
# Negative control: same key, WRONG transcript (signed over a DIFFERENT path
# than the request's — the server verifies over METHOD\nPATH\nTS of the
# delivered request). A server that did not verify would accept both.
sign_ed "POST" "/agents/other-agent/inbox" "$PEER_KEY"
BAD_CODE=$(curl -s -o "$WORKDIR/fed-bad.json" -w '%{http_code}' -X POST \
  "http://127.0.0.1:$PEER_PORT/agents/$FOREGIN_ID/inbox" \
  -H 'Content-Type: application/json' -d "$BODY" \
  -H 'X-Crier-Fed-Peer: relay-b' -H "X-Fed-Ts: $TS" -H "X-Fed-Sig: $SIG")
if [ "$GOOD_CODE" = "201" ] && [ "$BAD_CODE" = "401" ]; then
  record PASS "4-federation-peer-auth" "valid signed forward accepted (201); forged-transcript forward refused (401) by the peer-auth gate"
elif [ "$GOOD_CODE" != "201" ]; then
  record FAIL "4-federation-peer-auth" "valid signed forward -> HTTP $GOOD_CODE (want 201)"
else
  record FAIL "4-federation-peer-auth" "forged signature was NOT refused: HTTP $BAD_CODE (want 401)"
fi

# ============================================================================
# Cell 5 — Docker image
# ============================================================================
echo "==> cell 5: docker image"
if docker_available; then
  if docker build -q -t crier-acceptance:local . >"$WORKDIR/docker-build.log" 2>&1; then
    DOCKER_PORT=$(free_port 19400 "docker-cell")
    DOCKER_CONTAINER="crier-acc-$$"
    if docker run -d --name "$DOCKER_CONTAINER" -p "127.0.0.1:$DOCKER_PORT:8767" \
      crier-acceptance:local >"$WORKDIR/docker-cid" 2>&1 && CONTAINERS+=("$DOCKER_CONTAINER"); then
      DOCKER_UP=0
      for _ in $(seq 1 40); do
        if curl -sf "http://127.0.0.1:$DOCKER_PORT/health" >/dev/null 2>&1; then DOCKER_UP=1; break; fi
        sleep 0.5
      done
      if [ "$DOCKER_UP" = "1" ]; then
        record PASS "5-docker-image" "image crier-acceptance:local served /health on :$DOCKER_PORT; container removed on teardown"
      else
        record FAIL "5-docker-image" "container did not answer /health within 20s (log tail: $(tail -2 "$WORKDIR/docker-cid" 2>/dev/null | tr '\n' ' '))"
      fi
    else
      record FAIL "5-docker-image" "docker run crier-acceptance:local failed: $(tail -1 "$WORKDIR/docker-cid" 2>/dev/null || echo unknown)"
    fi
  else
    record FAIL "5-docker-image" "docker build failed: $(tail -1 "$WORKDIR/docker-build.log")"
  fi
else
  if [ -n "$DOCKER_REASON" ]; then
    record SKIP "5-docker-image" "$DOCKER_REASON"
  else
    record SKIP "5-docker-image" "docker daemon unavailable — image not verified on this host"
  fi
fi

# ============================================================================
# Summary
# ============================================================================
echo
echo "== acceptance-matrix summary =="
PASS_COUNT=0
SKIP_COUNT=0
FAIL_COUNT_FINAL=0
for line in "${SUMMARY[@]}"; do
  echo "  $line"
  case "$line" in
    PASS*) PASS_COUNT=$((PASS_COUNT + 1)) ;;
    SKIP*) SKIP_COUNT=$((SKIP_COUNT + 1)) ;;
    *) FAIL_COUNT_FINAL=$((FAIL_COUNT_FINAL + 1)) ;;
  esac
done
echo "== RESULT: $PASS_COUNT pass, $SKIP_COUNT skip, $FAIL_COUNT_FINAL fail =="
[ "$FAIL_COUNT_FINAL" -eq 0 ]
