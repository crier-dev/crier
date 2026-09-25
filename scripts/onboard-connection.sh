#!/usr/bin/env bash
#
# scripts/onboard-connection.sh — one-command onboarding for a platform
# connection (a Telegram bot, a WhatsApp number, a Signal number) as EXACTLY ONE
# crier agent identity.
#
# WHY THIS EXISTS (INT-MUSTER-004, Bane 2026-09-24)
# -------------------------------------------------
# "make it super easy to set up onboarding across platforms so they can send to
# crier." Before this script, standing up one platform connection meant hand-
# running the README "Try it" keygen → register → deliver dance, and re-running
# it was unsafe: registration was a blind POST whose only possible answers were
# 201 or 409, so a re-run after a re-key silently left the connection pointing at
# a mailbox whose registered public key no longer matched the private key on
# disk. Every one of those states was the operator's to notice. This script makes
# the first run one command and makes every re-run IDEMPOTENT: it looks before it
# writes, never re-registers, and never clobbers a registration it did not make.
#
# THE MODEL — one connection == exactly one crier agent identity
# -------------------------------------------------------------
# A platform connection IS an agent. Its agent id is the connection's mailbox
# (POST /agents/<id>/inbox is where that platform's traffic lands) and its tags
# carry the routing metadata. Nothing else is invented: crier has no separate
# "connection" object, and this script creates none.
#
# NAMING / TAG CONVENTION
# -----------------------
#   agent id   <platform>-<name>        e.g. telegram-ops-bot
#   tags       platform:<platform>      e.g. platform:telegram
#              connection:<name>        e.g. connection:ops-bot
#              topic:<platform>-<name>  e.g. topic:telegram-ops-bot
#
#   <platform> comes from the built-in set — telegram | whatsapp | signal —
#   extendable with CRIER_ONBOARD_PLATFORMS="…" (space-separated, ADDED to the
#   built-in set, so a new platform never requires editing this script).
#   <name> must be non-empty and shell-safe: [A-Za-z0-9._-] only, so the agent
#   id, the key filename and every printed command are quotable-safe by
#   construction.
#   GET /agents?capability=platform:telegram filters on these tags, which is what
#   makes them ROUTING metadata rather than a comment.
#
# MEASURED PREMISE — WHERE THE TAGS ACTUALLY RIDE (probed, not assumed)
# ---------------------------------------------------------------------
# The convention above is aligned with the message-tags feature as it exists in
# THIS tree. Measured on the branch this script lands on (base a40e8ea):
#
#   $ grep -n 'tags' docs/openapi.yaml | grep -v 'tags: \['
#     (no output — every hit is an OpenAPI OPERATION tag, e.g. `tags: [inbox]`)
#   $ grep -rn 'Tags\b' --include='*.go' internal/ cmd/
#     (no output)
#
# The deliver request body (POST /agents/{id}/inbox) declares exactly payload,
# sender, session_id, thread_id, delivery_mode, timeout_ms, request_id, kind,
# priority, ttl_seconds — there is NO `tags` member. Register declares exactly
# id, public_key, capabilities, webhook, guard.
#
# So this script does NOT put a `tags` array on the deliver call: a member the
# schema does not carry is a silent no-op, which is precisely the failure mode
# the convention exists to prevent (the same fail-loud doctrine as DF-CRIER-279 —
# never let a silent drop be the user's first signal). The tags ride on the
# IDENTITY instead — registered as the agent's `capabilities`, the one
# list-valued, queryable routing surface the registry has — and are recorded
# verbatim in the connection config file. If the deliver schema ever gains a
# `tags` member, scripts/onboard-connection-selftest.sh FAILS on that drift (it
# pins the measurement above), and this block is the thing to update.
#
# WHAT A RUN DOES
# ---------------
#   1. validates <platform> and <name> (see the convention), and CRIER_URL;
#   2. generates an ed25519 keypair at <connections>/<agent-id>.key — or REUSES
#      one that is already there (a re-run never silently re-keys; --force-key
#      rotates it and says so);
#   3. derives the 64-hex public key with the exact recipe the README "Try it"
#      block uses — `openssl pkey -in <key> -pubout -outform DER | tail -c 32`
#      then `xxd -p -c 64`, or the `od -An -tx1 | tr -d ' \n'` fallback when xxd
#      is absent (xxd is the documented preference, od the documented fallback);
#   4. registers agent '<platform>-<name>' IDEMPOTENTLY: GET /agents/<id> first —
#        * 200 → already registered. NO POST is issued and the registration is
#          left untouched. The stored public_key is compared with the key on
#          disk: equal → normal re-run; DIFFERENT → a loud KEY MISMATCH block
#          and READ-ONLY ADVISORY mode (still no write, exit 3, because a mailbox
#          whose registered key you no longer hold silently 401s every signed
#          call and silence is the one outcome this script refuses to produce);
#        * 404 → POST /agents with the public key and the three tags as
#          `capabilities` → 201. A 409 (registered between the GET and the POST)
#          is re-read with the same comparison instead of being reported as a
#          failure;
#   5. prints the Muster/CLI config lines and a ready-to-run send command, and
#   6. writes the connection config file (default alongside the key) so the next
#      tool does not have to re-derive any of it.
#
# USAGE
#   scripts/onboard-connection.sh <platform> <name> [--force-key]
#   scripts/onboard-connection.sh --help
#   scripts/onboard-connection.sh --selftest      # runs the sibling selftest
#
# ENVIRONMENT
#   CRIER_URL                 crier base URL (default http://127.0.0.1:8767)
#   CR_AUTH_TOKEN             bearer token, when the server enforces auth. The
#                             printed send command then carries the Bearer header
#                             (as a ${CR_AUTH_TOKEN} reference — the token itself
#                             is NEVER printed and NEVER written to the config).
#   CRIER_CONNECTIONS_DIR     where the key and the config live
#                             (default ./connections, i.e. alongside the key)
#   CRIER_KEY_PATH            override the key path (default
#                             <connections>/<platform>-<name>.key)
#   CRIER_CONFIG_PATH         override the config path (default
#                             <connections>/<platform>-<name>.json)
#   CRIER_ONBOARD_PLATFORMS   extra platforms (space-separated), added to
#                             telegram|whatsapp|signal
#
# THE PRINTED SEND COMMAND
#   Deliver needs NO per-agent signature in any auth configuration (measured and
#   re-measured by the selftest: POST /agents/<id>/inbox answers 201 with no
#   X-Agent-* headers at all) — the only optional header is the bearer one above.
#   The command is printed twice: human-readable (continuations), and as a single
#   machine-readable line
#       SEND_CMD_ONE_LINE=curl -sS -X POST '…' … -w '\n%{http_code}\n'
#   which is the exact line scripts/onboard-connection-selftest.sh extracts and
#   executes. It prints the response body followed by the HTTP status code.
#
# THE CONNECTION CONFIG FILE (again: never contains the token)
#   {
#     "agent_id": "telegram-ops-bot",       "platform": "telegram",
#     "name": "ops-bot",                    "server_url": "http://…",
#     "key_path": "…/telegram-ops-bot.key",
#     "public_key_hex": "…64 hex…",
#     "registry_public_key_hex": "…64 hex…",  # what the registry holds
#     "tags": ["platform:telegram", "connection:ops-bot", "topic:telegram-ops-bot"],
#     "registration": "registered" | "already-registered" | "conflict-key-mismatch",
#     "bearer_auth_configured": false,
#     "send_command": "curl -sS -X POST …",
#     "updated_at": "…Z"
#   }
#
# DEPENDENCIES: bash 4+, curl, openssl 3.x, python3 (stdlib only, JSON), and a
#               hex encoder — xxd, else od. A missing tool is exit 2, never a
#               silent skip.
#
# EXIT CODES
#   0  registered, or already registered with the key on disk (idempotent re-run)
#   1  operational failure (unreachable server, keygen/hex failure, unexpected
#      HTTP status) — the reason is printed
#   2  usage error or a missing dependency
#   3  KEY MISMATCH — read-only advisory, the registration was NOT touched
#
set -uo pipefail

SELF="${BASH_SOURCE[0]}"
case "$SELF" in /*) ;; *) SELF="$PWD/$SELF" ;; esac
SCRIPT_DIR="$(cd "$(dirname "$SELF")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." 2>/dev/null && pwd || printf '%s' "$SCRIPT_DIR")"
PROG="onboard-connection"
SELFTEST_SCRIPT="$SCRIPT_DIR/onboard-connection-selftest.sh"

BUILTIN_PLATFORMS="telegram whatsapp signal"
PLATFORMS="$BUILTIN_PLATFORMS${CRIER_ONBOARD_PLATFORMS:+ $CRIER_ONBOARD_PLATFORMS}"

DEFAULT_URL="http://127.0.0.1:8767"
DEFAULT_CONNECTIONS_DIR="$PWD/connections"

EX_OK=0
EX_FAIL=1
EX_USAGE=2
EX_CONFLICT=3

_err() { printf '%s: ERROR: %s\n' "$PROG" "$*" >&2; }
_warn() { printf '%s: WARNING: %s\n' "$PROG" "$*" >&2; }
note() { printf '%s: %s\n' "$PROG" "$*"; }
step() { printf '\n── %s\n' "$*"; }

usage() {
  cat <<'EOF'
onboard-connection.sh — onboard a platform connection as ONE crier agent.

Usage:
  scripts/onboard-connection.sh <platform> <name> [--force-key]
  scripts/onboard-connection.sh --help
  scripts/onboard-connection.sh --selftest

A "platform connection" is a Telegram bot, a WhatsApp number or a Signal number.
Each one becomes EXACTLY ONE crier agent identity: the agent id is the
connection's mailbox (POST /agents/<id>/inbox) and the tags are its routing
metadata. Re-running is safe and idempotent — an existing registration is read
and compared, never re-registered and never clobbered.

NAMING / TAG CONVENTION
  agent id   <platform>-<name>              e.g. telegram-ops-bot
  tags       platform:<platform>            e.g. platform:telegram
             connection:<name>              e.g. connection:ops-bot
             topic:<platform>-<name>        e.g. topic:telegram-ops-bot

  <platform>   telegram | whatsapp | signal, extendable with
               CRIER_ONBOARD_PLATFORMS="myplatform …" (space-separated; ADDED
               to the built-in set).
  <name>       non-empty, shell-safe: [A-Za-z0-9._-] only — no spaces, quotes,
               slashes or shell metacharacters, so the id, the key filename and
               the printed commands never need escaping.

  The tags are the routing keys: they are registered as the agent's
  `capabilities` (crier's one list-valued, queryable routing surface, e.g.
  `GET /agents?capability=platform:telegram`) and recorded in the connection
  config file. MEASURED: the deliver request body carries NO `tags` member in
  this tree (see the header of this script for the probe), so the tags ride on
  the identity rather than being sent as a field the schema would silently drop.

WHAT IT DOES
  1. generate (or reuse) an ed25519 keypair at <connections>/<agent-id>.key;
  2. derive the 64-hex public key with the README "Try it" recipe (xxd, or the
     od -An -tx1 fallback);
  3. GET /agents/<id> first: 200 -> already registered (no POST, nothing
     written; a mismatching public key is a loud warning and read-only advisory
     mode, exit 3) | 404 -> POST /agents -> 201;
  4. print the Muster/CLI config lines and a ready-to-run send command;
  5. write the connection config JSON (default ./connections/<agent-id>.json).

ENVIRONMENT
  CRIER_URL                crier base URL (default http://127.0.0.1:8767)
  CR_AUTH_TOKEN            bearer token; when set, the printed send command
                           carries the Bearer header as ${CR_AUTH_TOKEN}
                           (the token itself is never printed or stored)
  CRIER_CONNECTIONS_DIR    key + config directory (default ./connections)
  CRIER_KEY_PATH           override the key path
  CRIER_CONFIG_PATH        override the config path
  CRIER_ONBOARD_PLATFORMS  extra platforms, space-separated

DELIVER NEEDS NO SIGNATURE — the send command this script prints carries the
Content-Type header and, when a token is configured, the Bearer header. Nothing
else: an unsigned POST /agents/<id>/inbox answers 201.

EXIT CODES
  0 registered / already registered with the key on disk (idempotent re-run)
  1 operational failure (unreachable server, keygen failure, unexpected status)
  2 usage error or missing dependency
  3 KEY MISMATCH — read-only advisory; the registration was NOT touched

The machine-readable form of the send command is printed on one flush-left line,
SEND_CMD_ONE_LINE=… — that is the exact line scripts/onboard-connection-selftest.sh
extracts and executes to prove the printed command really delivers.
EOF
}

# ── dependencies ──────────────────────────────────────────────────────────────

require_tools() {
  local missing=0 t
  for t in curl openssl python3; do
    command -v "$t" >/dev/null 2>&1 || { _err "'$t' is required on PATH"; missing=1; }
  done
  if ! command -v xxd >/dev/null 2>&1 && ! command -v od >/dev/null 2>&1; then
    _err "neither 'xxd' nor 'od' is on PATH — one is required to hex-encode the public key"
    missing=1
  fi
  [ "$missing" = 0 ] || exit "$EX_USAGE"
}

# ── key helpers ───────────────────────────────────────────────────────────────
#
# The public half is derived with the README "Try it" recipe VERBATIM, as a
# PIPELINE: the raw 32 bytes are never round-tripped through a shell variable,
# because a bash variable cannot hold a NUL byte and an ed25519 public key is 32
# random bytes that routinely contain one (a $(...) round-trip would silently
# truncate the key and register a wrong one).
pubkey_hex() { # <keyfile> -> 64 lowercase hex chars on stdout
  if command -v xxd >/dev/null 2>&1; then
    openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64
  else
    openssl pkey -in "$1" -pubout -outform DER 2>/dev/null | tail -c 32 | od -An -tx1 | tr -d ' \n'
  fi
}

# ── HTTP helpers ──────────────────────────────────────────────────────────────

AUTH_TOKEN="${CR_AUTH_TOKEN:-}"
AUTH_ARGS=()
if [ -n "$AUTH_TOKEN" ]; then
  AUTH_ARGS=(-H "Authorization: Bearer $AUTH_TOKEN")
fi

# _http <method> <path> <datafile|""> <body-out> -> prints the HTTP status code.
# An empty return means curl could not reach the server at all (connection
# refused, DNS, timeout) — the caller must not read that as an HTTP answer.
_http() {
  local method="$1" path="$2" data="$3" out="$4"
  local args=(-sS --max-time 20 -o "$out" -w '%{http_code}' -X "$method")
  if [ "${#AUTH_ARGS[@]}" -gt 0 ]; then
    args+=("${AUTH_ARGS[@]}")
  fi
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' --data-binary "@$data")
  fi
  curl "${args[@]}" "$CRIER_URL$path" 2>/dev/null
}

_json_get() { # <file> <key> -> value or empty
  python3 - "$1" "$2" <<'PYEOF'
import json, sys
try:
    with open(sys.argv[1]) as fh:
        doc = json.load(fh)
except Exception:
    sys.exit(0)
val = doc.get(sys.argv[2]) if isinstance(doc, dict) else None
if val is None:
    sys.exit(0)
if isinstance(val, bool):
    print("true" if val else "false")
elif isinstance(val, (list, dict)):
    print(json.dumps(val))
else:
    print(val)
PYEOF
}

_lower() { printf '%s' "$1" | tr 'A-F' 'a-f'; }

_write_register_body() { # <outfile> <id> <pubkey> <cap1> <cap2> <cap3>
  python3 - "$@" <<'PYEOF'
import json, sys
out, ident, pubkey = sys.argv[1], sys.argv[2], sys.argv[3]
caps = list(sys.argv[4:])
with open(out, "w") as fh:
    json.dump({"id": ident, "public_key": pubkey, "capabilities": caps}, fh)
PYEOF
}

# The connection config file. NEVER carries the token: only whether one is
# configured, so the file is safe to keep next to the key (and in a repo).
_write_config() { # <outfile> <id> <platform> <name> <url> <keypath> <pubkey> <sendcmd> <registration> <authbool> <registry-pubkey|"">
  python3 - "$@" <<'PYEOF'
import datetime, json, sys
(out, agent_id, platform, name, server_url, key_path, pubkey, send_cmd,
 registration, auth_configured, registry_pubkey) = sys.argv[1:12]
doc = {
    "agent_id": agent_id,
    "platform": platform,
    "name": name,
    "server_url": server_url,
    "key_path": key_path,
    "public_key_hex": pubkey,
    "registry_public_key_hex": registry_pubkey,
    "tags": ["platform:%s" % platform, "connection:%s" % name,
             "topic:%s-%s" % (platform, name)],
    "registration": registration,
    "bearer_auth_configured": auth_configured == "true",
    "send_command": send_cmd,
    "updated_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
}
with open(out, "w") as fh:
    json.dump(doc, fh, indent=2)
    fh.write("\n")
PYEOF
}

# ── argument parsing ──────────────────────────────────────────────────────────

FORCE_KEY=0
POSITIONAL=()
for arg in "$@"; do
  case "$arg" in
    -h | --help | help) usage; exit "$EX_OK" ;;
    --selftest)
      if [ ! -f "$SELFTEST_SCRIPT" ]; then
        _err "--selftest: $SELFTEST_SCRIPT is missing"
        exit "$EX_USAGE"
      fi
      exec bash "$SELFTEST_SCRIPT"
      ;;
    --force-key) FORCE_KEY=1 ;;
    --) ;;
    -)
      _err "option '-' is not understood (see --help)"
      exit "$EX_USAGE"
      ;;
    -?*)
      _err "unknown option '$arg' (see --help)"
      exit "$EX_USAGE"
      ;;
    *) POSITIONAL+=("$arg") ;;
  esac
done

if [ "${#POSITIONAL[@]}" -ne 2 ]; then
  _err "expected exactly 2 arguments: <platform> <name> — got ${#POSITIONAL[@]}"
  printf 'Usage: scripts/onboard-connection.sh <platform> <name> [--force-key]\n' >&2
  printf "Known platforms: %s\n" "$PLATFORMS" >&2
  exit "$EX_USAGE"
fi

PLATFORM="${POSITIONAL[0]}"
NAME="${POSITIONAL[1]}"

# ── validation (fail loud, before anything is generated or written) ───────────

_platform_known() {
  local want="$1" known
  for known in $PLATFORMS; do
    [ "$known" = "$want" ] && return 0
  done
  return 1
}

if ! _platform_known "$PLATFORM"; then
  _err "unknown platform '$PLATFORM' — known: $PLATFORMS"
  _err "  extend the set with CRIER_ONBOARD_PLATFORMS=\"myplatform …\" (space-separated)"
  exit "$EX_USAGE"
fi
case "$PLATFORM" in
  *[!a-z0-9]*)
    _err "platform '$PLATFORM' must be lowercase [a-z0-9] only (it is part of the agent id)"
    exit "$EX_USAGE"
    ;;
esac
case "$NAME" in
  "" | "." | "..")
    _err "name '$NAME' is not a usable connection name"
    exit "$EX_USAGE"
    ;;
  *[!A-Za-z0-9._-]*)
    _err "name '$NAME' is not shell-safe — allowed characters are [A-Za-z0-9._-]"
    _err "  no spaces, quotes, slashes, '$' or other shell metacharacters (the name"
    _err "  becomes the agent id, the key filename and part of the printed command)"
    exit "$EX_USAGE"
    ;;
esac

CRIER_URL="${CRIER_URL:-$DEFAULT_URL}"
case "$CRIER_URL" in
  http://* | https://*) ;;
  *)
    _err "CRIER_URL must start with http:// or https:// (got '$CRIER_URL')"
    exit "$EX_USAGE"
    ;;
esac
case "$CRIER_URL" in
  *"'"* | *'"'* | *" "* | *"	"*)
    _err "CRIER_URL must not contain quotes or whitespace (got '$CRIER_URL')"
    exit "$EX_USAGE"
    ;;
esac
CRIER_URL="${CRIER_URL%/}"

AGENT_ID="$PLATFORM-$NAME"
CONN_DIR="${CRIER_CONNECTIONS_DIR:-$DEFAULT_CONNECTIONS_DIR}"
KEY_PATH="${CRIER_KEY_PATH:-$CONN_DIR/$AGENT_ID.key}"
CONFIG_PATH="${CRIER_CONFIG_PATH:-$CONN_DIR/$AGENT_ID.json}"

require_tools

mkdir -p "$CONN_DIR" 2>/dev/null || { _err "cannot create the connections directory $CONN_DIR"; exit "$EX_FAIL"; }
mkdir -p "$(dirname "$KEY_PATH")" 2>/dev/null || { _err "cannot create the key directory $(dirname "$KEY_PATH")"; exit "$EX_FAIL"; }

WORKDIR="$(mktemp -d)" || { _err "cannot create a scratch directory"; exit "$EX_FAIL"; }
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT

# ── 1. keypair (generated once, reused forever after) ─────────────────────────

step "connection $AGENT_ID → $CRIER_URL"

if [ -f "$KEY_PATH" ] && [ "$FORCE_KEY" != 1 ]; then
  note "key already present — REUSING $KEY_PATH (a re-run never silently re-keys)"
elif [ -f "$KEY_PATH" ]; then
  _warn "--force-key: rotating the key at $KEY_PATH (the registered public key will no longer match it; the script will report the mismatch and stay read-only)"
  ( umask 077; openssl genpkey -algorithm ED25519 -out "$KEY_PATH" ) >/dev/null 2>&1 \
    || { _err "openssl genpkey failed — cannot write $KEY_PATH"; exit "$EX_FAIL"; }
else
  ( umask 077; openssl genpkey -algorithm ED25519 -out "$KEY_PATH" ) >/dev/null 2>&1 \
    || { _err "openssl genpkey failed — cannot write $KEY_PATH"; exit "$EX_FAIL"; }
  note "generated an ed25519 keypair: $KEY_PATH"
fi
chmod 600 "$KEY_PATH" 2>/dev/null || true

PUBKEY="$(pubkey_hex "$KEY_PATH")"
pubrc=$?
if [ "$pubrc" != 0 ] || [ "${#PUBKEY}" -ne 64 ]; then
  _err "could not derive a 64-hex ed25519 public key from $KEY_PATH (got '${PUBKEY}' — $( [ -n "$PUBKEY" ] && printf 'len %s' "${#PUBKEY}" || printf 'empty' ))"
  _err "  the recipe is: openssl pkey -in <key> -pubout -outform DER | tail -c 32 | xxd -p -c 64"
  exit "$EX_FAIL"
fi
case "$PUBKEY" in
  *[!0-9a-f]*)
    _err "public key hex from $KEY_PATH is not lowercase hex ('$PUBKEY')"
    exit "$EX_FAIL"
    ;;
esac

# ── 2. registration — GET first, POST only when absent ────────────────────────

REG_PUBKEY=""
REGISTRATION=""
REGISTERED_AT=""

step "register agent '$AGENT_ID' (idempotent: GET first, POST only when absent)"

CODE="$(_http GET "/agents/$AGENT_ID" "" "$WORKDIR/get.json")"
case "$CODE" in
  "" )
    _err "cannot reach a crier server at $CRIER_URL (curl could not connect)."
    _err "  start one, or point CRIER_URL at a running server."
    exit "$EX_FAIL"
    ;;
  401)
    _err "the server at $CRIER_URL answered 401 to GET /agents/$AGENT_ID — auth is enforced."
    _err "  set CR_AUTH_TOKEN to the server's token and re-run (register/deliver"
    _err "  themselves need no per-agent signature, but they do need this bearer token)."
    exit "$EX_FAIL"
    ;;
  200)
    REG_PUBKEY="$(_json_get "$WORKDIR/get.json" public_key)"
    REGISTERED_AT="$(_json_get "$WORKDIR/get.json" registered_at)"
    if [ "$(_lower "$REG_PUBKEY")" = "$PUBKEY" ]; then
      REGISTRATION="already-registered"
      note "already registered (GET /agents/$AGENT_ID → 200) — NO POST issued, registration untouched"
      note "  stored public_key matches the key on disk"
    else
      REGISTRATION="conflict-key-mismatch"
    fi
    ;;
  404)
    _write_register_body "$WORKDIR/register.json" "$AGENT_ID" "$PUBKEY" \
      "platform:$PLATFORM" "connection:$NAME" "topic:$AGENT_ID"
    CODE="$(_http POST "/agents" "$WORKDIR/register.json" "$WORKDIR/register.out")"
    case "$CODE" in
      201)
        REGISTRATION="registered"
        REG_PUBKEY="$PUBKEY"
        note "registered (POST /agents → 201)"
        ;;
      409)
        # Registered between our GET and our POST. Re-read and grade it exactly
        # like the 200 path rather than reporting a failure we caused ourselves.
        CODE="$(_http GET "/agents/$AGENT_ID" "" "$WORKDIR/get.json")"
        REG_PUBKEY="$(_json_get "$WORKDIR/get.json" public_key)"
        REGISTERED_AT="$(_json_get "$WORKDIR/get.json" registered_at)"
        if [ "$(_lower "$REG_PUBKEY")" = "$PUBKEY" ]; then
          REGISTRATION="already-registered"
          note "already registered (409 on POST, GET → 200 with the same key) — registration untouched"
        else
          REGISTRATION="conflict-key-mismatch"
        fi
        ;;
      "" )
        _err "cannot reach a crier server at $CRIER_URL while registering"
        exit "$EX_FAIL"
        ;;
      *)
        _err "unexpected HTTP $CODE from POST /agents (want 201)"
        printf '%s:   body: %s\n' "$PROG" "$(head -c 400 "$WORKDIR/register.out" 2>/dev/null)" >&2
        exit "$EX_FAIL"
        ;;
    esac
    ;;
  *)
    _err "unexpected HTTP $CODE from GET /agents/$AGENT_ID (want 200 or 404)"
    printf '%s:   body: %s\n' "$PROG" "$(head -c 400 "$WORKDIR/get.json" 2>/dev/null)" >&2
    exit "$EX_FAIL"
    ;;
esac

# ── 3. the printed send command (deliver needs no signature) ──────────────────

SEND_URL="$CRIER_URL/agents/$AGENT_ID/inbox"
SEND_BODY="{\"payload\":{\"text\":\"hello from $AGENT_ID\"},\"sender\":\"$AGENT_ID\"}"
AUTH_PART=""
HUMAN_AUTH_LINE=""
if [ -n "$AUTH_TOKEN" ]; then
  AUTH_PART="-H \"Authorization: Bearer \${CR_AUTH_TOKEN}\" "
  HUMAN_AUTH_LINE=1
fi
SEND_CMD_ONE_LINE="curl -sS -X POST '$SEND_URL' -H 'Content-Type: application/json' ${AUTH_PART}-d '$SEND_BODY' -w '\\n%{http_code}\\n'"

AUTH_STATE="disabled (no CR_AUTH_TOKEN set — requests need no Authorization header)"
[ -n "$AUTH_TOKEN" ] && AUTH_STATE="bearer (CR_AUTH_TOKEN is set — the printed command carries the Bearer header)"

TAG_PLATFORM="platform:$PLATFORM"
TAG_CONNECTION="connection:$NAME"
TAG_TOPIC="topic:$AGENT_ID"

_write_config "$CONFIG_PATH" "$AGENT_ID" "$PLATFORM" "$NAME" "$CRIER_URL" \
  "$KEY_PATH" "$PUBKEY" "$SEND_CMD_ONE_LINE" "$REGISTRATION" \
  "$( [ -n "$AUTH_TOKEN" ] && printf true || printf false )" "$REG_PUBKEY"

# ── 4. report ─────────────────────────────────────────────────────────────────

step "connection"
printf '  platform   : %s\n' "$PLATFORM"
printf '  name       : %s\n' "$NAME"
printf '  agent id   : %s   (the connection mailbox)\n' "$AGENT_ID"
printf '  tags       : %s %s %s\n' "$TAG_PLATFORM" "$TAG_CONNECTION" "$TAG_TOPIC"
printf '  key        : %s\n' "$KEY_PATH"
printf '  public key : %s\n' "$PUBKEY"
printf '  registry   : %s%s\n' "$REGISTRATION" \
  "$( [ -n "$REGISTERED_AT" ] && printf ' (registered_at %s)' "$REGISTERED_AT" || printf '' )"
printf '  server     : %s  auth: %s\n' "$CRIER_URL" "$AUTH_STATE"
printf '  config     : %s\n' "$CONFIG_PATH"

step "Muster / CLI config"
printf '  # the tags above are registered as this agent'"'"'s capabilities, so a\n'
printf '  # client can discover the connection by route:\n'
printf "  #   GET %s/agents?capability=%s\n" "$CRIER_URL" "$TAG_PLATFORM"
printf '  # 1) teach Muster the surface — it generates the client from crier'"'"'s spec\n'
printf '  openapi-cli discover %s/openapi.json\n' "$CRIER_URL"
if [ -n "$AUTH_TOKEN" ]; then
  printf '  #    (with auth on: openapi-cli auth add bearer --token "$CR_AUTH_TOKEN")\n'
fi
printf '  # 2) Muster MCP server config (spec + base URL)\n'
printf '  curl -sS -o crier-openapi.json %s/openapi.json\n' "$CRIER_URL"
printf '  openapi-mcp -spec ./crier-openapi.json -base-url %s\n' "$CRIER_URL"

step "send a message to this connection (ready to run)"
printf "  curl -sS -X POST '%s' \\\\\n" "$SEND_URL"
printf "    -H 'Content-Type: application/json' \\\\\n"
if [ -n "$HUMAN_AUTH_LINE" ]; then
  printf '    -H "Authorization: Bearer ${CR_AUTH_TOKEN}" \\\n'
fi
printf "    -d '%s' \\\\\n" "$SEND_BODY"
printf "    -w '\\\\n%%{http_code}\\\\n'\n"
printf '  # deliver needs NO per-agent signature in any auth config — the 201 below\n'
printf '  # is the whole contract. Expect: 201 {"id":"…","transport":"inbox",…}\n'
printf '\n'
printf '  # machine-readable one-line form (the selftest executes exactly this line):\n'
printf '%s\n' "SEND_CMD_ONE_LINE=$SEND_CMD_ONE_LINE"

if [ "$REGISTRATION" = "conflict-key-mismatch" ]; then
  step "KEY MISMATCH — read-only advisory"
  printf '%s\n' "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
  printf '!! The registered public key for %s is NOT the key on disk.\n' "$AGENT_ID"
  printf '!!   key on disk      : %s\n' "$PUBKEY"
  printf '!!   registered key   : %s\n' "${REG_PUBKEY:-<none reported by the server>}"
  printf '!! This run did NOT re-register and did NOT modify the registration.\n'
  printf '!! Consequence: the mailbox stays reachable and DELIVER still works (it is\n'
  printf '!! unsigned), but every SIGNED call as %s (inbox retrieve, ack, PATCH,\n' "$AGENT_ID"
  printf '!! DELETE) will 401, because the private half of the registered key is not\n'
  printf '!! the one at %s.\n' "$KEY_PATH"
  printf '!! Resolve by choosing one deliberately:\n'
  printf '!!   * keep the registered key: restore that private key at %s, or\n' "$KEY_PATH"
  printf '!!   * move to the key on disk: unregister the agent, then re-run this\n'
  printf '!!     script (a fresh registration takes the key on disk).\n'
  printf '%s\n' "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
  exit "$EX_CONFLICT"
fi

exit "$EX_OK"
