#!/usr/bin/env bash
# DF-CRIER-309: boot real crier binaries and prove federated room fan-out.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/df-crier-309.XXXXXX")
ALPHA_PID=
BETA_PID=
cleanup() {
  for pid in "$ALPHA_PID" "$BETA_PID"; do
    if [[ -n "$pid" ]]; then
      kill "$pid" 2>/dev/null || true
    fi
  done
  for pid in "$ALPHA_PID" "$BETA_PID"; do
    if [[ -n "$pid" ]]; then
      for _ in {1..50}; do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
      done
      kill -KILL "$pid" 2>/dev/null || true
    fi
  done
}
trap cleanup EXIT

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}
ALPHA_PORT=$(free_port)
BETA_PORT=$(free_port)
while [[ "$ALPHA_PORT" == "$BETA_PORT" ]]; do BETA_PORT=$(free_port); done
SESSION_ID="df-crier-309-$$-$ALPHA_PORT"
ALPHA="http://127.0.0.1:$ALPHA_PORT"
BETA="http://127.0.0.1:$BETA_PORT"

BIN="$WORK/crier"
(cd "$ROOT" && go build -o "$BIN" ./cmd/server)

# A's outbound policy and B's inbound policy are configured through the same
# production CR_FED_PEERS_FILE loader used by the server entry point.
printf '{"peers":[{"peer":"peer_beta","url":"%s","self_as":"peer_alpha","namespaces_allow":[""]}]}' "$BETA" >"$WORK/alpha-peers.json"
printf '{"peers":[{"peer":"peer_alpha","url":"%s","self_as":"peer_beta","namespaces_allow":[""]}]}' "$ALPHA" >"$WORK/beta-peers.json"

source "$ROOT/scripts/lib/port-guard.sh"
guard_loopback_off_proxy
require_free_port "$ALPHA_PORT" "DF-CRIER-309 alpha"
require_free_port "$BETA_PORT" "DF-CRIER-309 beta"
CRIER_BIN="$BIN"
export CRIER_BIN
CRIER_PORT="$ALPHA_PORT" CR_DATABASE_URL= CR_AUTH_TOKEN= CR_REQUIRE_AGENT_SIG=false \
  CR_SESSION_LOG_ROOT="$WORK/alpha-sessions" CR_GROUP_ROOT="$WORK/alpha-groups" \
  CR_FED_PEERS_FILE="$WORK/alpha-peers.json" CR_FED_LINKS="$BETA" "$CRIER_BIN" >"$WORK/alpha.log" 2>&1 &
ALPHA_PID=$!
CRIER_PORT="$BETA_PORT" CR_DATABASE_URL= CR_AUTH_TOKEN= CR_REQUIRE_AGENT_SIG=false \
  CR_SESSION_LOG_ROOT="$WORK/beta-sessions" CR_GROUP_ROOT="$WORK/beta-groups" \
  CR_FED_PEERS_FILE="$WORK/beta-peers.json" CR_FED_LINKS="$ALPHA" "$CRIER_BIN" >"$WORK/beta.log" 2>&1 &
BETA_PID=$!

wait_ready() {
  local url=$1 pid=$2 log=$3
  for _ in {1..100}; do
    if ! kill -0 "$pid" 2>/dev/null; then
      printf 'server exited before readiness (%s):\n' "$url" >&2
      cat "$log" >&2
      return 1
    fi
    if curl --silent --show-error --fail "$url/health" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  printf 'server did not become ready (%s):\n' "$url" >&2
  cat "$log" >&2
  return 1
}
wait_ready "$ALPHA" "$ALPHA_PID" "$WORK/alpha.log"
assert_port_owned "$ALPHA_PORT" "$ALPHA_PID" "DF-CRIER-309 alpha"
wait_ready "$BETA" "$BETA_PID" "$WORK/beta.log"
assert_port_owned "$BETA_PORT" "$BETA_PID" "DF-CRIER-309 beta"

# Register the local author and the actual inbox recipient.
curl --silent --show-error --fail -H 'Content-Type: application/json' \
  -d '{"id":"atlas-a"}' "$ALPHA/agents" >/dev/null
curl --silent --show-error --fail -H 'Content-Type: application/json' \
  -d '{"id":"atlas-b"}' "$BETA/agents" >/dev/null

# Create the room on alpha and join beta's agent as a remote participant.
curl --silent --show-error --fail -H 'Content-Type: application/json' \
  -d "{\"id\":\"$SESSION_ID\",\"created_by\":{\"agent\":\"atlas-a\"}}" \
  "$ALPHA/sessions" >/dev/null
curl --silent --show-error --fail -H 'Content-Type: application/json' \
  -d '{"member_type":"agent","member_id":"remote:peer_beta:atlas-b","role":"member"}' \
  "$ALPHA/sessions/$SESSION_ID/participants" >/dev/null

MARKER="DF-CRIER-309-federated-fanout-$(date +%s%N)"
curl --silent --show-error --fail -H 'Content-Type: application/json' \
  -d "{\"sender\":\"atlas-a\",\"payload\":{\"text\":\"$MARKER\"}}" \
  "$ALPHA/sessions/$SESSION_ID/messages" >/dev/null

# Inbox reads lease entries; decode each payload and require the unique message
# marker, proving delivery into beta rather than just successful server startup.
for _ in {1..50}; do
  if curl --silent --show-error --fail "$BETA/agents/atlas-b/inbox" >"$WORK/inbox.json" && \
    python3 - "$WORK/inbox.json" "$MARKER" <<'PY'
import base64, json, sys
with open(sys.argv[1], encoding="utf-8") as f:
    response = json.load(f)
marker = sys.argv[2]
for entry in response.get("messages", []):
    payload = entry.get("payload", "")
    try:
        decoded = base64.b64decode(payload).decode("utf-8")
    except Exception:
        continue
    if marker in decoded:
        raise SystemExit(0)
raise SystemExit(1)
PY
  then
    printf 'PASS DF-CRIER-309: session message reached beta inbox (%s)\n' "$MARKER"
    exit 0
  fi
  sleep 0.1
done
printf 'FAIL DF-CRIER-309: fan-out marker was not found in beta inbox\n' >&2
cat "$WORK/inbox.json" >&2 || true
printf '\nalpha log:\n' >&2; cat "$WORK/alpha.log" >&2
printf '\nbeta log:\n' >&2; cat "$WORK/beta.log" >&2
exit 1
