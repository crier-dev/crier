#!/usr/bin/env bash
#
# scripts/onboard-connection-selftest.sh — prove the onboarding path really
# works, end to end, against a LIVE crier server this script starts itself.
#
# WHAT IT PROVES (INT-MUSTER-004)
# -------------------------------
#   (a) IDEMPOTENT RE-RUN registers nothing new: register once (agent count 1),
#       re-run, and assert the count is unchanged AND the agent's `registered_at`
#       is unchanged — the registry's own timestamp, not just the script's word
#       for it, so "nothing new" cannot be a re-registration that happened to
#       land on the same id;
#   (b) THE PRINTED SEND COMMAND DELIVERS (201): the command is EXTRACTED from
#       the onboarding run's own output (the flush-left `SEND_CMD_ONE_LINE=…`
#       line) and executed verbatim — not a hand-written curl that happens to
#       match the docs — and the delivered message is then confirmed inside the
#       connection's inbox with a SIGNED stats call made with the connection's
#       own private key, which is what proves the registered public key and the
#       key on disk are one identity;
#   (c) A RE-RUN CANNOT CLOBBER an existing registration: the key on disk is
#       deliberately swapped for a fresh one, the re-run must exit 3 with the
#       loud KEY MISMATCH block, and the REGISTRY must still hold the ORIGINAL
#       public key and the original registered_at — read-only advisory means
#       read-only;
#   (d) the tag convention is real routing metadata: the tags registered as
#       capabilities are queryable (GET /agents?capability=platform:telegram);
#   (e) the bearer form appears when a token is configured, and that command
#       delivers too;
#   (f) argument validation refuses what it says it refuses (unknown platform,
#       non-shell-safe name, missing arguments) — so the validation is not
#       vacuous;
#   (g) THE MEASURED PREMISE THE TAG CONVENTION RESTS ON STILL HOLDS: the
#       deliver request body carries NO `tags` member in the LIVE served spec.
#       This is a PIN, not decoration — the onboarding script deliberately does
#       not send a tags field, and if the schema ever gains one this check fails
#       and names the file to update;
#   (h) NO LEAKED PROCESS OR PORT (CR-GAP-069): the server this script starts is
#       killed and the port is asserted free at the end, with an EXIT trap as the
#       abnormal-exit safety net.
#
# HOW IT RUNS (no flags at all: the server is configured by environment only)
#   * builds the worktree's server to a scratch path and spawns it with
#     `env -i PATH=… HOME=… CRIER_PORT=… CR_GUARD_ENABLED=false` on the MEMORY
#     backend (no CR_DATABASE_URL), so nothing outside the scratch directory is
#     touched. The guard is switched off deliberately: this selftest is about the
#     onboarding path, and a guard that reached out to an LLM provider would make
#     a 201 depend on a provider key being present (no key → the guard errors and
#     fails open today, but that is the provider's behaviour, not a contract this
#     harness should rest on);
#   * CHOOSES its port: a $RANDOM-derived base (18795 + 0..40) walked by the
#     shared selector scripts/lib/port-guard.sh, which prints every candidate it
#     skips and the holder that took it. The chosen port is printed;
#   * asserts, after the start, that the pid HOLDING the port is the pid it
#     started (assert_port_owned) — a squatter is never mistaken for the server;
#   * everything it creates lives in a mktemp directory that the EXIT trap
#     removes. The only writes outside it are the connection key/config files,
#     which the onboarding script itself is being tested for.
#
# ENVIRONMENT
#   ONBOARD_PORT              explicit port (checked, never rotated)
#   ONBOARD_PORT_BASE         first candidate of the rotation (default
#                             18795 + $RANDOM % 41)
#   ONBOARD_PORT_CANDIDATES   how many candidates the rotation may try (default 5)
#
# DEPENDENCIES: bash 4+, go, curl, openssl 3.x, python3, ss, xxd (or od).
#
# EXIT CODES: 0 every check passed; 1 at least one check failed; 2 the harness
#             itself could not run (missing tool, no free port, server never
#             became healthy).
#
set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ONBOARD="$SCRIPT_DIR/onboard-connection.sh"
PROG="onboard-connection-selftest"

PLATFORM="telegram"
NAME="ops-bot"
AGENT_ID="$PLATFORM-$NAME"
TOKEN="onboard-selftest-token"
AUTH_LABEL="Bearer"

WORKDIR="$(mktemp -d)" || { echo "$PROG: ERROR: cannot create a scratch directory" >&2; exit 2; }
CONN_DIR="$WORKDIR/connections"
CRIER_BIN="$WORKDIR/crier-onboard-server"

SERVER_PID=""
PORT=""

# ── the cleanup contract (CR-GAP-069) ─────────────────────────────────────────
# This script SPAWNS a crier server, so it owes both halves: an EXIT trap that
# kills the pid it started, and (below, after the start) a port-ownership
# assertion. The trap is the safety net for the abnormal exits; the normal path
# kills and re-checks the port before the summary.
cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null
  fi
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

# ── shared harness helpers ────────────────────────────────────────────────────
# shellcheck source=lib/port-guard.sh
. "$REPO_ROOT/scripts/lib/port-guard.sh"
# curl has no built-in loopback exemption: with an ambient HTTP_PROXY the probes
# below would be sent to the proxy and never reach the server started here
# (QA-CRIER-21). Merge 127.0.0.1/localhost/::1 into no_proxy.
guard_loopback_off_proxy

step() { printf '\n== %s\n' "$*"; }
PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); printf 'PASS  %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf 'FAIL  %s\n' "$*"; }
check_eq() { # <want> <got> <desc>
  if [ "$1" = "$2" ]; then ok "$3 (got '$2')"; else bad "$3 (want '$1', got '$2')"; fi
}
check_contains() { # <haystack> <needle> <desc>
  case "$1" in
    *"$2"*) ok "$3" ;;
    *) bad "$3 (output does not contain '$2')" ;;
  esac
}

# ── preflight ─────────────────────────────────────────────────────────────────
for tool in go curl openssl python3 ss; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$PROG: ERROR: '$tool' is required on PATH" >&2; exit 2; }
done
if ! command -v xxd >/dev/null 2>&1 && ! command -v od >/dev/null 2>&1; then
  echo "$PROG: ERROR: neither 'xxd' nor 'od' is on PATH" >&2
  exit 2
fi
[ -f "$ONBOARD" ] || { echo "$PROG: ERROR: $ONBOARD is missing" >&2; exit 2; }

# ── scratch port: a $RANDOM-derived base, rotated by the shared selector ───────
ONBOARD_PORT_BASE="${ONBOARD_PORT_BASE:-$((18795 + RANDOM % 41))}"
ONBOARD_PORT_CANDIDATES="${ONBOARD_PORT_CANDIDATES:-5}"
select_scratch_port "${ONBOARD_PORT:-}" "$ONBOARD_PORT_BASE" \
  "crier (onboard-connection selftest)" "$ONBOARD_PORT_CANDIDATES" "ONBOARD_PORT"
PORT="$PORT_GUARD_SELECTED"
CRIER="http://127.0.0.1:$PORT"
printf '%s: [port] base %s (18795 + $RANDOM %% 41) -> selected :%s\n' \
  "$PROG" "$ONBOARD_PORT_BASE" "$PORT"

# ── build + spawn (env-driven: no CLI flags) ──────────────────────────────────
step "build and start crier on :$PORT (memory backend, env-isolated)"
( cd "$REPO_ROOT" && go build -o "$CRIER_BIN" ./cmd/server ) \
  || { echo "$PROG: ERROR: go build ./cmd/server failed" >&2; exit 2; }
echo "built $CRIER_BIN"

env -i PATH="$PATH" HOME="$HOME" CRIER_PORT="$PORT" CR_GUARD_ENABLED=false \
  "$CRIER_BIN" > "$WORKDIR/server.log" 2>&1 &
SERVER_PID=$!

wait_http_or_die "$CRIER/health" "$SERVER_PID" "$WORKDIR/server.log" "crier (onboard-connection selftest)"
# (f)/(h) enough of a start to prove ownership: the pid holding :PORT is the pid
# started here, never a squatter that answered the health poll.
assert_port_owned "$PORT" "$SERVER_PID" "crier (onboard-connection selftest)"
echo "server up: pid $SERVER_PID on :$PORT — ownership asserted"
curl -sS "$CRIER/health" && echo " <- health"

# ── request helpers ───────────────────────────────────────────────────────────
CODE=""
BODY=""
api() { # <method> <path> [json-body] -> CODE/BODY
  local method="$1" path="$2" data="${3:-}" raw
  if [ -n "$data" ]; then
    raw="$(curl -sS --max-time 20 -w $'\n%{http_code}' -X "$method" "$CRIER$path" \
      -H 'Content-Type: application/json' --data-binary "$data" 2>/dev/null)"
  else
    raw="$(curl -sS --max-time 20 -w $'\n%{http_code}' -X "$method" "$CRIER$path" 2>/dev/null)"
  fi
  CODE="${raw##*$'\n'}"
  BODY="${raw%$'\n'*}"
}

agent_count() {
  # GET /agents answers {"agents":[…]}: the wire shape is an OBJECT with the list
  # under "agents" (internal/registry/handler.go agentsResponse). A bare array is
  # accepted too, so this helper cannot silently count the object's KEYS (which is
  # 1 no matter how many agents exist — a vacuous "count unchanged").
  curl -sS "$CRIER/agents" 2>/dev/null \
    | python3 -c '
import json, sys
doc = json.load(sys.stdin)
if isinstance(doc, dict):
    doc = doc.get("agents") or []
print(len(doc))
' 2>/dev/null
}
agent_field() { # <id> <field>
  curl -sS "$CRIER/agents/$1" 2>/dev/null \
    | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$2" 2>/dev/null
}
capability_count() { # <capability>
  curl -sS "$CRIER/agents?capability=$1" 2>/dev/null \
    | python3 -c '
import json, sys
doc = json.load(sys.stdin)
if isinstance(doc, dict):
    doc = doc.get("agents") or []
print(len(doc))
' 2>/dev/null
}
config_field() { # <configfile> <key>
  python3 - "$1" "$2" <<'PYEOF'
import json, sys
try:
    with open(sys.argv[1]) as fh:
        doc = json.load(fh)
except Exception:
    sys.exit(0)
val = doc.get(sys.argv[2])
if val is None:
    sys.exit(0)
if isinstance(val, list):
    print(",".join(str(x) for x in val))
elif isinstance(val, bool):
    print("true" if val else "false")
else:
    print(val)
PYEOF
}
pubkey_of() { # <keyfile>
  if command -v xxd >/dev/null 2>&1; then
    openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64
  else
    openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | tail -c 32 | od -An -tx1 | tr -d ' \n'
  fi
}

export CRIER_URL="$CRIER"
export CRIER_CONNECTIONS_DIR="$CONN_DIR"
KEY_PATH="$CONN_DIR/$AGENT_ID.key"
CONFIG_PATH="$CONN_DIR/$AGENT_ID.json"

# ── 1. first run: registers the connection ────────────────────────────────────
step "[1] first onboarding run (register)"
bash "$ONBOARD" "$PLATFORM" "$NAME" > "$WORKDIR/run1.out" 2>&1
RC1=$?
cat "$WORKDIR/run1.out"
check_eq 0 "$RC1" "[1] first run exits 0"
check_contains "$(cat "$WORKDIR/run1.out")" "registered (POST /agents → 201)" \
  "[1] first run reports the 201 registration"
check_contains "$(cat "$WORKDIR/run1.out")" "agent id   : $AGENT_ID" \
  "[1] printed agent id is <platform>-<name>"
check_contains "$(cat "$WORKDIR/run1.out")" \
  "tags       : platform:$PLATFORM connection:$NAME topic:$AGENT_ID" \
  "[1] printed tag convention is the documented triple"
[ -f "$KEY_PATH" ] && ok "[1] keypair written to $KEY_PATH" || bad "[1] keypair missing at $KEY_PATH"
[ -f "$CONFIG_PATH" ] && ok "[1] connection config written to $CONFIG_PATH" || bad "[1] config missing at $CONFIG_PATH"

check_eq "1" "$(agent_count)" "[1] registry holds exactly 1 agent (clean server baseline)"
check_eq "$AGENT_ID" "$(agent_field "$AGENT_ID" id)" "[1] registered id is $AGENT_ID"
check_eq "$(pubkey_of "$KEY_PATH")" "$(agent_field "$AGENT_ID" public_key)" \
  "[1] registered public_key == the key on disk"
check_eq "$AGENT_ID" "$(config_field "$CONFIG_PATH" agent_id)" "[1] config records agent_id"
check_eq "$PLATFORM" "$(config_field "$CONFIG_PATH" platform)" "[1] config records platform"
check_eq "$NAME" "$(config_field "$CONFIG_PATH" name)" "[1] config records name"
check_eq "$KEY_PATH" "$(config_field "$CONFIG_PATH" key_path)" "[1] config records key_path"
check_eq "$(pubkey_of "$KEY_PATH")" "$(config_field "$CONFIG_PATH" public_key_hex)" \
  "[1] config records public_key_hex"
check_eq "platform:$PLATFORM,connection:$NAME,topic:$AGENT_ID" \
  "$(config_field "$CONFIG_PATH" tags)" "[1] config records the three routing tags"
check_eq "registered" "$(config_field "$CONFIG_PATH" registration)" "[1] config records the registration state"
check_eq "false" "$(config_field "$CONFIG_PATH" bearer_auth_configured)" \
  "[1] config records that no bearer token was configured"

# ── 2. (a) idempotent re-run registers nothing new ────────────────────────────
step "[2] (a) idempotent re-run — nothing new may be registered"
REG_AT_1="$(agent_field "$AGENT_ID" registered_at)"
COUNT_1="$(agent_count)"
bash "$ONBOARD" "$PLATFORM" "$NAME" > "$WORKDIR/run2.out" 2>&1
RC2=$?
cat "$WORKDIR/run2.out"
check_eq 0 "$RC2" "[2] re-run exits 0"
check_contains "$(cat "$WORKDIR/run2.out")" "already registered (GET /agents/$AGENT_ID → 200)" \
  "[2] re-run reports the existing registration"
check_contains "$(cat "$WORKDIR/run2.out")" "NO POST issued, registration untouched" \
  "[2] re-run states that no POST was issued"
case "$(cat "$WORKDIR/run2.out")" in
  *"registered (POST /agents → 201)"*)
    bad "[2] re-run issued a POST — it must not re-register"
    ;;
  *) ok "[2] re-run contains no 201 registration line" ;;
esac
check_eq "$COUNT_1" "$(agent_count)" "[2] agent count unchanged by the re-run"
check_eq "$REG_AT_1" "$(agent_field "$AGENT_ID" registered_at)" \
  "[2] registered_at unchanged — the registry was not re-written"
check_eq "already-registered" "$(config_field "$CONFIG_PATH" registration)" \
  "[2] config updated to already-registered (a read, not a clobber)"

# ── 3. (d) the tags are real routing metadata ─────────────────────────────────
# The CONTROL first: the counting method used by [2] and [6] must be able to SEE
# a new registration, or "count unchanged" would be vacuous. A decoy agent is
# registered directly against the API (different id, different capability) and
# the count MUST move 1 -> 2.
step "[3] (d) the tags are routing metadata — and the counter is not vacuous"
openssl genpkey -algorithm ED25519 -out "$WORKDIR/decoy.key" 2>/dev/null
DECOY_PUB="$(pubkey_of "$WORKDIR/decoy.key")"
DECOY_ID="decoy-$$"
api POST /agents "{\"id\":\"$DECOY_ID\",\"public_key\":\"$DECOY_PUB\",\"capabilities\":[\"demo:decoy\"]}"
check_eq "201" "$CODE" "[3] control: a second agent is registered directly (201)"
check_eq "2" "$(agent_count)" \
  "[3] control: the counter DETECTS the new registration (1 -> 2), so 'count unchanged' means something"
COUNT_STEADY="$(agent_count)"
check_eq "1" "$(capability_count "platform:$PLATFORM")" \
  "[3] GET /agents?capability=platform:$PLATFORM finds the connection"
check_eq "1" "$(capability_count "topic:$AGENT_ID")" \
  "[3] GET /agents?capability=topic:$AGENT_ID finds the connection"
check_eq "1" "$(capability_count "demo:decoy")" \
  "[3] the filter finds the decoy by ITS capability"
check_eq "0" "$(capability_count "platform:whatsapp")" \
  "[3] GET /agents?capability=platform:whatsapp finds nothing (the filter is not a no-op)"

# ── 4. (b) the PRINTED send command really delivers ───────────────────────────
step "[4] (b) execute the printed send command verbatim"
SEND_CMD="$(grep -m1 '^SEND_CMD_ONE_LINE=' "$WORKDIR/run1.out" | sed 's/^SEND_CMD_ONE_LINE=//')"
if [ -n "$SEND_CMD" ]; then
  ok "[4] the onboarding output carries a machine-readable send command"
  printf '      %s\n' "$SEND_CMD"
else
  bad "[4] no SEND_CMD_ONE_LINE= line in the onboarding output"
fi
SEND_OUT="$(bash -c "$SEND_CMD" 2>&1)"
SEND_CODE="$(printf '%s\n' "$SEND_OUT" | tail -1)"
printf '      response: %s\n' "$(printf '%s' "$SEND_OUT" | head -c 300)"
check_eq "201" "$SEND_CODE" "[4] the PRINTED send command delivers (expect 201)"
check_contains "$SEND_OUT" '"transport":"inbox"' "[4] the delivery landed in the inbox transport"
case "$SEND_CMD" in
  *X-Agent-*) bad "[4] the printed command carries X-Agent-* headers — deliver needs no signature" ;;
  *) ok "[4] the printed command carries no signature headers (deliver needs none)" ;;
esac

# The delivered message must be in the connection's inbox, and the connection's
# OWN private key must open it — that is what proves the registered public key,
# the key on disk and the printed identity are one identity (and it is the
# signed call INT-MUSTER-003 documents as unreachable from a bearer-only client,
# so it is driven here with the real keypair).
step "[4b] confirm the delivery inside the inbox, signed with the connection key"
TS="$(date +%s)"
printf '%s\n%s\n%s' "GET" "/agents/$AGENT_ID/inbox/stats" "$TS" > "$WORKDIR/payload.txt"
SIG="$(openssl pkeyutl -sign -rawin -inkey "$KEY_PATH" -in "$WORKDIR/payload.txt" 2>/dev/null | xxd -p -c 128)"
if [ -z "$SIG" ]; then
  bad "[4b] signing produced an empty signature (openssl < 3 or a non-seekable payload)"
else
  STATS="$(curl -sS --max-time 20 -w $'\n%{http_code}' "$CRIER/agents/$AGENT_ID/inbox/stats" \
    -H "X-Agent-ID: $AGENT_ID" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG" 2>/dev/null)"
  S_CODE="${STATS##*$'\n'}"
  S_BODY="${STATS%$'\n'*}"
  printf '      stats: %s\n' "$S_BODY"
  check_eq "200" "$S_CODE" "[4b] signed stats with the connection key answers 200"
  check_eq "1" "$(python3 -c 'import json,sys; print(json.load(sys.stdin)["queue_depth"])' <<<"$S_BODY" 2>/dev/null)" \
    "[4b] queue_depth is 1 — the delivered message really is in this inbox"
fi

# ── 5. (e) the bearer form appears when a token is configured ─────────────────
step "[5] (e) with CR_AUTH_TOKEN set, the printed command carries the Bearer header"
CR_AUTH_TOKEN="$TOKEN" bash "$ONBOARD" "$PLATFORM" "$NAME" > "$WORKDIR/run3.out" 2>&1
RC3=$?
check_eq 0 "$RC3" "[5] token-configured run still re-reads the registration (exit 0)"
TOK_CMD="$(grep -m1 '^SEND_CMD_ONE_LINE=' "$WORKDIR/run3.out" | sed 's/^SEND_CMD_ONE_LINE=//')"
check_contains "$TOK_CMD" "Authorization: $AUTH_LABEL" "[5] printed command carries the bearer header form"
TOK_OUT="$(CR_AUTH_TOKEN="$TOKEN" bash -c "$TOK_CMD" 2>&1)"
check_eq "201" "$(printf '%s\n' "$TOK_OUT" | tail -1)" \
  "[5] the bearer-form printed command delivers too (201)"
check_eq "true" "$(config_field "$CONFIG_PATH" bearer_auth_configured)" \
  "[5] config records bearer_auth_configured=true"
case "$(cat "$WORKDIR/run3.out")" in
  *"$TOKEN"*) bad "[5] the token itself was printed — it must never be echoed or stored" ;;
  *) ok "[5] the token value never appears in the output" ;;
esac

# ── 6. (c) a re-run must not clobber an existing registration ─────────────────
step "[6] (c) deliberate key swap -> loud mismatch, read-only advisory, no clobber"
ORIG_PUB="$(pubkey_of "$KEY_PATH")"
ORIG_REG_AT="$(agent_field "$AGENT_ID" registered_at)"
cp "$KEY_PATH" "$WORKDIR/original.key"
# the swap: a BRAND NEW key at the same path, exactly what an operator who
# re-keyed out of band (or ran --force-key on a different box) would produce.
openssl genpkey -algorithm ED25519 -out "$KEY_PATH" 2>/dev/null
SWAPPED_PUB="$(pubkey_of "$KEY_PATH")"
printf '      original key : %s\n      swapped  key : %s\n' "$ORIG_PUB" "$SWAPPED_PUB"
if [ "$SWAPPED_PUB" != "$ORIG_PUB" ] && [ -n "$SWAPPED_PUB" ]; then
  ok "[6] the on-disk key was deliberately swapped (fixture is real)"
else
  bad "[6] the key swap did not change the public key — the fixture is broken"
fi
bash "$ONBOARD" "$PLATFORM" "$NAME" > "$WORKDIR/run4.out" 2>&1
RC4=$?
# print the advisory in full: the warning IS the evidence for this requirement.
cat "$WORKDIR/run4.out"
check_eq 3 "$RC4" "[6] mismatching re-run exits 3 (read-only advisory)"
check_contains "$(cat "$WORKDIR/run4.out")" "KEY MISMATCH" "[6] the mismatch is announced loudly"
check_contains "$(cat "$WORKDIR/run4.out")" "$ORIG_PUB" "[6] the warning names the REGISTERED key"
check_contains "$(cat "$WORKDIR/run4.out")" "$SWAPPED_PUB" "[6] the warning names the key ON DISK"
check_contains "$(cat "$WORKDIR/run4.out")" "did NOT re-register" "[6] the warning states nothing was written"
check_eq "$COUNT_STEADY" "$(agent_count)" "[6] agent count unchanged — nothing new was registered"
check_eq "$ORIG_REG_AT" "$(agent_field "$AGENT_ID" registered_at)" \
  "[6] registered_at unchanged — the registration was not re-written"
check_eq "$ORIG_PUB" "$(agent_field "$AGENT_ID" public_key)" \
  "[6] the registry still holds the ORIGINAL public key (not clobbered by the new one)"
check_eq "conflict-key-mismatch" "$(config_field "$CONFIG_PATH" registration)" \
  "[6] config records conflict-key-mismatch"
check_eq "$ORIG_PUB" "$(config_field "$CONFIG_PATH" registry_public_key_hex)" \
  "[6] config records the registry's key alongside the on-disk one"
# restore, so the harness leaves the fixture as it found it and re-runs are stable
cp "$WORKDIR/original.key" "$KEY_PATH"
chmod 600 "$KEY_PATH" 2>/dev/null || true

# ── 7. (f) validation is not vacuous ──────────────────────────────────────────
step "[7] (f) the argument validation refuses what it says it refuses"
bash "$ONBOARD" not-a-platform "$NAME" > "$WORKDIR/run5.out" 2>&1
check_eq 2 "$?" "[7] unknown platform exits 2"
check_contains "$(cat "$WORKDIR/run5.out")" "unknown platform 'not-a-platform'" \
  "[7] unknown platform is named"
bash "$ONBOARD" "$PLATFORM" "bad name" > "$WORKDIR/run6.out" 2>&1
check_eq 2 "$?" "[7] non-shell-safe name exits 2"
check_contains "$(cat "$WORKDIR/run6.out")" "is not shell-safe" "[7] the name rejection says why"
bash "$ONBOARD" "$PLATFORM" > "$WORKDIR/run7.out" 2>&1
check_eq 2 "$?" "[7] missing arguments exits 2"
# a rejected run must not have created anything
check_eq "$COUNT_STEADY" "$(agent_count)" "[7] no rejected run touched the registry"
bash "$ONBOARD" --help > "$WORKDIR/help.out" 2>&1
check_eq 0 "$?" "[7] --help exits 0"
check_contains "$(cat "$WORKDIR/help.out")" "NAMING / TAG CONVENTION" \
  "[7] --help documents the naming/tag convention"
check_contains "$(cat "$WORKDIR/help.out")" "platform:<platform>" \
  "[7] --help states the platform:<platform> tag"

# ── 8. (g) PIN: the tag convention's measured premise still holds ─────────────
step "[8] (g) pin: the LIVE spec's deliver body carries no 'tags' member"
curl -sS --max-time 20 "$CRIER/openapi.json" > "$WORKDIR/openapi.json" 2>/dev/null
PREMISE="$(python3 - "$WORKDIR/openapi.json" <<'PYEOF'
import json, sys
try:
    doc = json.load(open(sys.argv[1]))
except Exception as exc:
    print("unreadable:%s" % exc)
    sys.exit(0)
try:
    props = doc["paths"]["/agents/{id}/inbox"]["post"]["requestBody"]["content"]["application/json"]["schema"]["properties"]
except Exception as exc:
    print("noschema:%s" % exc)
    sys.exit(0)
print("has-tags" if "tags" in props else "no-tags")
PYEOF
)"
printf '      probe: %s (deliver body: %s)\n' "$PREMISE" \
  "$(python3 -c 'import json,sys; print(",".join(json.load(open(sys.argv[1]))["paths"]["/agents/{id}/inbox"]["post"]["requestBody"]["content"]["application/json"]["schema"]["properties"]))' "$WORKDIR/openapi.json" 2>/dev/null)"
case "$PREMISE" in
  no-tags)
    ok "[8] the deliver body carries no 'tags' member — the identity-carried convention is still the right one"
    ;;
  has-tags)
    bad "[8] the deliver schema NOW carries a 'tags' member — update the convention in scripts/onboard-connection.sh (send the tags array on deliver) and this pin"
    ;;
  *)
    bad "[8] could not read the deliver schema from the live spec ($PREMISE) — the pin verified nothing"
    ;;
esac

# ── 9. (h) teardown: kill the server we started, assert the port came back ────
step "[9] (h) teardown — the server must not outlive this script (CR-GAP-069)"
KILLED_PID="$SERVER_PID"
kill "$SERVER_PID" 2>/dev/null
wait "$SERVER_PID" 2>/dev/null
SERVER_PID=""
for _ in $(seq 1 25); do
  [ -z "$(port_holder_pid "$PORT")" ] && break
  sleep 0.2
done
HOLDER="$(port_holder_pid "$PORT")"
check_eq "" "$HOLDER" "[9] :$PORT is free after the kill (no leaked listener)"
if kill -0 "$KILLED_PID" 2>/dev/null; then
  bad "[9] pid $KILLED_PID is still alive"
else
  ok "[9] pid $KILLED_PID is gone"
fi

printf '\nSELFTEST RESULT: %s pass / %s fail (port %s, agent %s, %s)\n' \
  "$PASS" "$FAIL" "$PORT" "$AGENT_ID" "$WORKDIR"
exit "$FAIL"
