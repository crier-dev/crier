#!/usr/bin/env bash
# openapi-client-muster.sh — drive the INT-MUSTER-005 flow with a client GENERATED
# by Muster's `openapi-cli` (the generator the battery prefers when it is on PATH).
#
# The battery's cell uses this driver only when a `muster`/`openapi-cli` binary is
# on PATH; otherwise it drives the flow with the client emitted by
# scripts/openapi-client-gen.py. Both drivers write the SAME per-step artifacts,
# so the cell's assertions and evidence lines do not depend on which one ran:
#
#   <out>/step-<register|deliver|retrieve|ack>.json
#   <out>/retrieve-body.json
#   <out>/driver.log
#
# WHERE EACH FACT COMES FROM (nothing here is hand-written):
#   * endpoints (method + path) — Muster's own `generate` metadata for
#     docs/openapi.yaml (the client Muster built from the spec), re-copied to
#     <out>/muster-endpoints.json so the cell can diff it against the
#     spec-derived client. The battery asserts the two agree.
#   * request-body key names and signature-header names — the spec-derived
#     contract in <out>/client.json (the same document, read by the battery's own
#     generator). Muster's generated commands cannot send the deliver body: its
#     object-typed request-body properties are registered as flags but dropped at
#     execution (measured on v0.1.0: `openapi-cli inbox-deliver --payload '{"a":1}'`
#     answers 400 "payload is required" — nothing is stored), and its stored base
#     URL is the spec's servers[0] rather than anything --base-url sets (also
#     measured). So this driver issues every call through the same generated
#     client's `request` verb, pointed at the battery's scratch port, using the
#     endpoints from Muster's metadata. See the cell's comment block.
#
# Usage:
#   openapi-client-muster.sh --generator BIN --meta FILE --contract FILE --out DIR \
#     --base-url URL --token T --agent ID --key KEYFILE --payload-file FILE
set -uo pipefail

GENERATOR=""; META=""; CONTRACT=""; OUT=""; BASE_URL=""; TOKEN=""; AGENT=""; KEYFILE=""; PAYLOAD_FILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --generator) GENERATOR="$2"; shift 2 ;;
    --meta) META="$2"; shift 2 ;;
    --contract) CONTRACT="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    --base-url) BASE_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --agent) AGENT="$2"; shift 2 ;;
    --key) KEYFILE="$2"; shift 2 ;;
    --payload-file) PAYLOAD_FILE="$2"; shift 2 ;;
    *) echo "openapi-client-muster: unknown argument: $1" >&2; exit 2 ;;
  esac
done
for pair in "generator:$GENERATOR" "meta:$META" "contract:$CONTRACT" "out:$OUT" \
            "base-url:$BASE_URL" "token:$TOKEN" "agent:$AGENT" "key:$KEYFILE" "payload-file:$PAYLOAD_FILE"; do
  name="${pair%%:*}"; value="${pair#*:}"
  [ -n "$value" ] || { echo "openapi-client-muster: missing --$name" >&2; exit 2; }
done
command -v "$GENERATOR" >/dev/null 2>&1 || { echo "openapi-client-muster: generator not found: $GENERATOR" >&2; exit 2; }
[ -f "$META" ] || { echo "openapi-client-muster: no generated metadata at $META" >&2; exit 2; }
[ -f "$CONTRACT" ] || { echo "openapi-client-muster: no spec-derived contract at $CONTRACT" >&2; exit 2; }
mkdir -p "$OUT"

# ── endpoints from Muster's own metadata (by endpoint shape, not by command name)
eval "$(python3 - "$META" <<'PYEOF'
import json, sys
meta = json.load(open(sys.argv[1]))
commands = meta.get("commands") or []
def pick(method, suffix, exact=None):
    for c in commands:
        if (c.get("Method") or "").upper() != method:
            continue
        path = c.get("Path") or ""
        if exact is not None and path != exact:
            continue
        if exact is None and not path.endswith(suffix):
            continue
        return path
    return ""
wanted = {
    "REGISTER": pick("POST", "", exact="/agents"),
    "DELIVER": pick("POST", "/inbox"),
    "RETRIEVE": pick("GET", "/inbox"),
    "ACK": pick("POST", "/inbox/ack"),
}
if not all(wanted.values()):
    print("missing endpoints in metadata: %s" % wanted, file=sys.stderr)
    raise SystemExit(3)
print("MUSTER_REGISTER_PATH=%s" % wanted["REGISTER"])
print("MUSTER_DELIVER_PATH=%s" % wanted["DELIVER"])
print("MUSTER_RETRIEVE_PATH=%s" % wanted["RETRIEVE"])
print("MUSTER_ACK_PATH=%s" % wanted["ACK"])
PYEOF
)" || { echo "openapi-client-muster: could not read endpoints from $META" >&2; exit 2; }

python3 - "$META" "$MUSTER_REGISTER_PATH" "$MUSTER_DELIVER_PATH" "$MUSTER_RETRIEVE_PATH" "$MUSTER_ACK_PATH" \
  > "$OUT/muster-endpoints.json" <<'PYEOF'
import json, sys
meta = json.load(open(sys.argv[1]))
roles = ["register", "deliver", "retrieve", "ack"]
methods = {"register": "POST", "deliver": "POST", "retrieve": "GET", "ack": "POST"}
out = {role: {"method": methods[role], "path": sys.argv[2 + i]} for i, role in enumerate(roles)}
out["generated_at"] = meta.get("generated_at", "")
out["spec_path"] = meta.get("spec_path", "")
print(json.dumps(out, indent=2, sort_keys=True))
PYEOF

# ── spec-derived key names (contract) + agent public key
cfg() { # dotted key -> value (single line)
  python3 - "$CONTRACT" "$1" <<'PYEOF'
import json, sys
node = json.load(open(sys.argv[1]))
for part in sys.argv[2].split("."):
    node = node.get(part) if isinstance(node, dict) else None
print(node if isinstance(node, (str, int)) else "")
PYEOF
}
PUBHEX="$(openssl pkey -in "$KEYFILE" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)"
[ -n "$PUBHEX" ] || { echo "openapi-client-muster: could not derive the public key from $KEYFILE" >&2; exit 2; }
DELIVER_PAYLOAD_KEY="$(cfg operations.deliver.body_payload_key)"
ACK_LEASE_KEY="$(cfg operations.ack.body_lease_key)"
ACK_IDS_KEY="$(cfg operations.ack.body_ids_key)"
REGISTER_BODY_KEYS="$(python3 - "$CONTRACT" <<'PYEOF'
import json, sys
print(" ".join(json.load(open(sys.argv[1])).get("register_body_keys") or []))
PYEOF
)"
SIGN_HEADERS="$(python3 - "$CONTRACT" <<'PYEOF'
import json, sys
print("\n".join(json.load(open(sys.argv[1]))["operations"]["retrieve"]["sign_headers"]))
PYEOF
)"
for v in DELIVER_PAYLOAD_KEY ACK_LEASE_KEY ACK_IDS_KEY REGISTER_BODY_KEYS SIGN_HEADERS; do
  eval "value=\$$v"
  [ -n "$value" ] || { echo "openapi-client-muster: contract is missing $v" >&2; exit 2; }
done

# ── helpers
sign() { # METHOD PATH -> SIG_TS / SIG_SIG (the battery's ed25519 convention)
  local ts; ts="$(date +%s)"
  printf '%s\n%s\n%s' "$1" "$2" "$ts" > "$OUT/muster-pl.txt"
  SIG_SIG="$(openssl pkeyutl -sign -rawin -inkey "$KEYFILE" -in "$OUT/muster-pl.txt" 2>/dev/null | xxd -p -c 128)"
  SIG_TS="$ts"
}
sig_args() { # METHOD PATH -> SIG_ARGS, named by the spec's header parameters
  sign "$1" "$2"
  SIG_ARGS=()
  local h id_name ts_name sig_name
  id_name="$(printf '%s\n' $SIGN_HEADERS | sed -n 1p)"
  ts_name="$(printf '%s\n' $SIGN_HEADERS | sed -n 2p)"
  sig_name="$(printf '%s\n' $SIGN_HEADERS | sed -n 3p)"
  for h in "$id_name" "$ts_name" "$sig_name"; do
    case "$h" in
      *[Ii][Dd]*) SIG_ARGS+=(-H "$h: $AGENT") ;;
      *[Tt][Ss]*) SIG_ARGS+=(-H "$h: $SIG_TS") ;;
      *)          SIG_ARGS+=(-H "$h: $SIG_SIG") ;;
    esac
  done
}
call() { # METHOD URL [data] [extra...] -> HTTP_CODE / BODY
  local method="$1" url="$2" data="${3:-}"; shift 3 2>/dev/null || shift $#
  local out rc=0
  if [ -n "$data" ]; then
    out="$("$GENERATOR" request "$method" "$url" -b "$data" -H "Authorization: Bearer $TOKEN" \
      -H 'Content-Type: application/json' "$@" -o json -v 2>&1)" || rc=$?
  else
    out="$("$GENERATOR" request "$method" "$url" -H "Authorization: Bearer $TOKEN" \
      "$@" -o json -v 2>&1)" || rc=$?
  fi
  printf '%s\n' "$out" >> "$OUT/driver.log"
  HTTP_CODE="$(printf '%s\n' "$out" | grep -m1 '^Status:' | awk '{print $2}')"
  case "$HTTP_CODE" in ''|*[!0-9]*) HTTP_CODE=0 ;; esac
  BODY="$(printf '%s\n' "$out" | python3 -c 'import sys
lines = sys.stdin.read().splitlines()
start = None
for i, l in enumerate(lines):
    if l[:1] in "{[":
        start = i
        break
print("\n".join(lines[start:]) if start is not None else "")')"
  CLIENT_EXIT="$rc"
}
mount() { printf '%s\n' "$1" | sed "s/{id}/$AGENT/g"; }
step() { # role method path template
  printf '{"step":"%s","via":"%s request","method":"%s","path":"%s","template":"%s","http":%s,"client_exit":%s}\n' \
    "$1" "$GENERATOR" "$2" "$3" "$4" "$HTTP_CODE" "$CLIENT_EXIT" > "$OUT/step-$1.json"
}

# ── 1. register
REG_PATH="$(mount "$MUSTER_REGISTER_PATH")"
REG_BODY="{"
first=1
for k in $REGISTER_BODY_KEYS; do
  case "$k" in
    id) v="$AGENT" ;;
    public_key) v="$PUBHEX" ;;
    *) echo "openapi-client-muster: spec-derived register key without a value: $k" >&2; exit 2 ;;
  esac
  [ "$first" = 1 ] || REG_BODY="$REG_BODY,"
  REG_BODY="$REG_BODY\"$k\":\"$v\""; first=0
done
REG_BODY="$REG_BODY}"
call POST "$BASE_URL$REG_PATH" "$REG_BODY"
step register POST "$REG_PATH" "$MUSTER_REGISTER_PATH"

# ── 2. deliver
DEL_PATH="$(mount "$MUSTER_DELIVER_PATH")"
DEL_BODY="{\"$DELIVER_PAYLOAD_KEY\":$(cat "$PAYLOAD_FILE")}"
call POST "$BASE_URL$DEL_PATH" "$DEL_BODY"
step deliver POST "$DEL_PATH" "$MUSTER_DELIVER_PATH"

# ── 3. signed retrieve
RET_PATH="$(mount "$MUSTER_RETRIEVE_PATH")"
sig_args GET "$RET_PATH"
call GET "$BASE_URL$RET_PATH" "" "${SIG_ARGS[@]}"
step retrieve GET "$RET_PATH" "$MUSTER_RETRIEVE_PATH"
printf '%s' "$BODY" > "$OUT/retrieve-body.json"

# ── 4. signed ack
LEASE="$(python3 - "$OUT/retrieve-body.json" "$ACK_LEASE_KEY" <<'PYEOF'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit(0)
value = body.get(sys.argv[2], "")
print(value if isinstance(value, str) else "")
PYEOF
)"
MSG_IDS="$(python3 - "$OUT/retrieve-body.json" <<'PYEOF'
import json, sys
try:
    body = json.load(open(sys.argv[1]))
except Exception:
    print(""); raise SystemExit(0)
print(",".join(m.get("id", "") for m in (body.get("messages") or []) if isinstance(m, dict)))
PYEOF
)"
ACK_PATH="$(mount "$MUSTER_ACK_PATH")"
ACK_BODY="{\"$ACK_LEASE_KEY\":\"$LEASE\",\"$ACK_IDS_KEY\":["
first=1
IFS=',' read -r -a _ids <<< "$MSG_IDS"
for m in "${_ids[@]:-}"; do
  [ -n "$m" ] || continue
  [ "$first" = 1 ] || ACK_BODY="$ACK_BODY,"
  ACK_BODY="$ACK_BODY\"$m\""; first=0
done
ACK_BODY="$ACK_BODY]}"
sig_args POST "$ACK_PATH"
call POST "$BASE_URL$ACK_PATH" "$ACK_BODY" "${SIG_ARGS[@]}"
step ack POST "$ACK_PATH" "$MUSTER_ACK_PATH"
exit 0
