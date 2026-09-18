#!/usr/bin/env bash
#
# Crier integration demo — register → deliver → signed retrieve → ack round-trip.
#
# Requirements:
#   - a running crier server (default http://localhost:8767, override with CRIER_URL)
#   - openssl 3.x+ (ed25519 support; the -rawin flag used below is OpenSSL 3+ only) and xxd on PATH
#
# Run the server first:
#   make run                                  # default port 8767, in-memory backend
#   # or with signing disabled for a quick look:
#   CR_REQUIRE_AGENT_SIG=false make run
#   # The LLM message guard is ON by default (CR_GUARD_ENABLED=true). For a
#   # deterministic keyless run, start the server with:
#   CR_GUARD_ENABLED=false make run
#
# Usage:
#   ./examples/demo.sh
#
# The inbox endpoints require per-agent ed25519 request signatures by default
# (CR_REQUIRE_AGENT_SIG=true). This script generates an ephemeral keypair,
# registers the agent, and signs every retrieve/ack request with it:
#   X-Agent-Sig = hex(ed25519_sign("<METHOD>\n<path>\n<unix-seconds>", privkey))
# Timestamps must be within ±30s of the server clock.
#
set -euo pipefail

CRIER_URL="${CRIER_URL:-http://localhost:8767}"
AGENT_ID="demo-$(date +%s)"
WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# If the server was started with CR_AUTH_TOKEN set (auth enabled), every request
# except /health must carry the Bearer header below. If CR_AUTH_TOKEN is unset,
# auth is disabled and the header is omitted. Export the same token you started
# the server with:
#   CR_AUTH_TOKEN=secret ./examples/demo.sh
AUTH_ARGS=()
if [ -n "${CR_AUTH_TOKEN:-}" ]; then
  AUTH_ARGS=(-H "Authorization: Bearer ${CR_AUTH_TOKEN}")
fi

if ! command -v openssl >/dev/null 2>&1; then
  echo "ERROR: openssl required (ed25519 keygen/signing)" >&2
  exit 1
fi
if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then
  echo "ERROR: this signing helper requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)" >&2
  exit 1
fi
if ! command -v xxd >/dev/null 2>&1; then
  echo "ERROR: xxd required (hex encoding)" >&2
  exit 1
fi

echo "==> crier demo against ${CRIER_URL} (agent ${AGENT_ID})"

# The LLM message guard (CR-FEAT-010) runs SERVER-side and is ON by default
# (CR_GUARD_ENABLED=true): every inbound delivery is classified by a guard LLM
# before webhook POST / inbox store. CR_GUARD_ENABLED and the provider key
# belong to the crier SERVER process — this script's environment cannot change
# the guard state of an already-running server. This note is therefore
# server-side CONFIGURATION GUIDANCE only; the verdict for THIS run is reported
# further down, measured from the deliver RESPONSE BODY:
#   - CR_GUARD_ENABLED=false  -> no classification runs at all; the deliver
#     response carries no "guard" object (see the header above).
#   - key set on the server   -> the guard classifies the delivery; a block
#     answers 403 {"error":"GUARD_BLOCKED","guard":{…}} and allowed ones carry
#     X-Crier-Guard-* verdict headers, on outbound webhook POSTs only.
#   - key unset on the server -> the guard FAILS OPEN: the delivery below still
#     succeeds, but it can burn up to CR_GUARD_TIMEOUT_MS (default 10000ms) on
#     the failing LLM call, and the verdict rides in the RESPONSE BODY as
#     "guard":{…,"errored":true}. An inbox delivery answers no
#     X-Crier-Guard-* header; those exist only on an outbound webhook POST,
#     which this demo's agent never gets (no webhook).
echo "==> note: the LLM message guard (CR-FEAT-010) is SERVER-side configuration — this script's environment cannot change an already-running server. Start the SERVER with CR_GUARD_ENABLED=false for a deterministic keyless round-trip, or with CR_GUARD_ENABLED=true plus a usable provider key (e.g. DEEPSEEK_API_KEY) on the SERVER to exercise classification. This script asserts nothing about whether the guard ran: the verdict it actually measured is printed below, from the deliver response body."

# 0. Health check
echo "==> [1/6] health"
curl -sS -o /dev/null -w "    GET /health -> %{http_code}\n" "${AUTH_ARGS[@]}" "${CRIER_URL}/health"
curl -sfS "${AUTH_ARGS[@]}" "${CRIER_URL}/health" >/dev/null || {
  echo "ERROR: crier not reachable at ${CRIER_URL} — start it first (make run)" >&2
  exit 1
}

# 1. Generate an ed25519 keypair
echo "==> [2/6] generating ed25519 keypair"
openssl genpkey -algorithm ED25519 -out "${WORKDIR}/agent.key" >/dev/null 2>&1
PUBKEY_HEX=$(openssl pkey -in "${WORKDIR}/agent.key" -pubout -outform DER 2>/dev/null \
  | tail -c 32 | xxd -p -c 64)

# 2. Register the agent
echo "==> [3/6] register agent"
curl -sS -X POST "${AUTH_ARGS[@]}" "${CRIER_URL}/agents" -H 'Content-Type: application/json' \
  -d "{\"id\":\"${AGENT_ID}\",\"public_key\":\"${PUBKEY_HEX}\",\"capabilities\":[\"demo\"]}" \
  -w "\n    POST /agents -> %{http_code}\n"

# 3. Deliver a message to its inbox.
#    The response body is captured rather than streamed straight to stdout so
#    the guard verdict it carries can be reported below (DF-CRIER-241). The
#    printed transcript keeps the pre-existing shape: body, blank line, then
#    the "POST /agents/<id>/inbox -> <code>" status line.
echo "==> [4/6] deliver message"
DELIVER=$(curl -sS -X POST "${AUTH_ARGS[@]}" "${CRIER_URL}/agents/${AGENT_ID}/inbox" -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world","n":42}}' \
  -w '\nHTTP_CODE:%{http_code}')
DELIVER_BODY=$(printf '%s' "${DELIVER}" | grep -v '^HTTP_CODE:')
DELIVER_CODE=$(printf '%s' "${DELIVER}" | grep -o 'HTTP_CODE:[0-9]*' | head -1 | cut -d: -f2)
printf '%s\n' "${DELIVER_BODY}"
echo
echo "    POST /agents/${AGENT_ID}/inbox -> ${DELIVER_CODE}"

# ---- Guard verdict, MEASURED from this response (DF-CRIER-241) -----------
# Everything printed below comes from the deliver RESPONSE BODY above. Nothing
# here is derived from this script's environment: CR_GUARD_ENABLED and the
# provider key belong to the SERVER process, and the shell this script runs in
# cannot observe them.
#
# Wire shape (docs/openapi.yaml GuardVerdictMeta — the object
# internal/registry/handler.go's guardInDeliverResponse attaches): "guard":{…}
# rides in the body only when the verdict was NOT a clean pass (a sanitize, an
# errored fail-open, or an allow carrying a risk marker). A clean allow omits
# it, and a disabled guard omits it too — so an absent "guard" is ambiguous by
# design and this script reports the absence instead of inventing a reason.
# Pull a field out of the verdict object. The string matcher is escape-aware
# (a reason carrying \" — e.g. a provider error quoting a JSON body — is read
# whole instead of being cut at the first escaped quote), and every helper ends
# in a command that always exits 0 (|| true) so `set -e -o pipefail` cannot
# abort the demo on a field the response simply does not carry.
guard_str() {
  printf '%s' "${GUARD_RAW}" \
    | grep -oE "\"$1\":\"([^\"\\\\]|\\\\.)*\"" | head -1 \
    | sed -e "s/^\"$1\":\"//" -e 's/"$//' -e 's/\\"/"/g' -e 's/\\\\/\\/g' || true
}
guard_arr() { printf '%s' "${GUARD_RAW}" | grep -o "\"$1\":\[[^]]*\]" | head -1 | sed 's/^[^:]*://' || true; }
guard_true() { printf '%s' "${GUARD_RAW}" | grep -o "\"$1\":true" | head -1 | cut -d: -f2 || true; }
guard_field() {
  if [ -n "$2" ]; then
    printf '      %-17s : %s\n' "$1" "$2"
  else
    printf '      %-17s : (not carried in this response)\n' "$1"
  fi
}
# Print every field the wire verdict object can carry, read out of GUARD_RAW.
# The caller sets GUARD_RAW to the response text being reported, so the same
# reader serves both sources (deliver body, stored inbox entry) without merging
# them into one claim.
verdict_fields() {
  GUARD_DECISION=$(guard_str decision)
  GUARD_RISK=$(guard_str risk_level)
  GUARD_REASON=$(guard_str reason)
  GUARD_PATTERNS=$(guard_arr matched_patterns)
  GUARD_POLICY=$(guard_str policy)
  GUARD_PROVIDER=$(guard_str provider)
  GUARD_MODEL=$(guard_str model)
  GUARD_ERRORED=$(guard_true errored)
  guard_field decision "${GUARD_DECISION}"
  guard_field risk_level "${GUARD_RISK}"
  if [ -n "${GUARD_ERRORED}" ]; then
    printf '      %-17s : %s\n' errored "${GUARD_ERRORED}"
  else
    printf '      %-17s : %s\n' errored "not carried (omitempty — only ever present as true, on the guard-error path)"
  fi
  guard_field reason "${GUARD_REASON}"
  guard_field matched_patterns "${GUARD_PATTERNS}"
  guard_field policy "${GUARD_POLICY}"
  guard_field provider "${GUARD_PROVIDER}"
  guard_field model "${GUARD_MODEL}"
  # duration_ms is NOT part of this wire object: guard.Result carries it
  # internally, guard.Meta (the type attached to this body) does not.
  printf '      %-17s : %s\n' duration_ms "(not part of this wire object — GuardVerdictMeta has no duration_ms)"
}
# errored:true is a fail-open verdict — say exactly that and nothing stronger.
verdict_fail_open() {
  if [ -n "${GUARD_ERRORED}" ]; then
    echo "      fail-open         : YES — errored:true means the guard produced NO classification for this delivery (the reason field names the failure) and the bus delivered the message anyway. This verdict is NOT evidence that the payload was classified."
  else
    echo "      fail-open         : no — errored is not true, so this verdict did not come from the guard-error path."
  fi
}
if printf '%s' "${DELIVER_BODY}" | grep -q '"guard"'; then
  # "guard" is the LAST field of the deliver body (struct field order:
  # id, transport, delivery_mode, expires_at, guard), so the verdict object is
  # the body text from that key onward, minus the body's own closing brace.
  # The object itself is printed VERBATIM as well, so a field this parser
  # cannot split out is still on screen.
  GUARD_RAW=$(printf '%s' "${DELIVER_BODY}" | sed -e 's/.*"guard"://')
  GUARD_RAW="${GUARD_RAW%\}}"
  echo "    guard verdict measured in THIS deliver response (not from this shell's environment):"
  echo "      guard object      : ${GUARD_RAW}"
  verdict_fields
  verdict_fail_open
else
  echo "    guard verdict: this deliver response carries NO \"guard\" object, so this demo makes NO claim that the guard ran for this delivery."
  echo "      (Documented ambiguity — docs/openapi.yaml GuardVerdictMeta: the object is omitted for a CLEAN ALLOW, and is omitted just the same when no guard ran at all or the payload tripped the over-cap path. The response cannot tell those apart, so this line does not either. A verdict that did ride with the message is reported from the retrieve body at step [5/6] below.)"
fi

# 4. Signed retrieve
echo "==> [5/6] signed retrieve"
TS=$(date +%s)
printf 'GET\n/agents/%s/inbox\n%s' "${AGENT_ID}" "$TS" > "${WORKDIR}/payload.txt"
SIG=$(openssl pkeyutl -sign -rawin -inkey "${WORKDIR}/agent.key" -in "${WORKDIR}/payload.txt" 2>/dev/null \
  | xxd -p -c 128)
RETRIEVE=$(curl -sS "${AUTH_ARGS[@]}" "${CRIER_URL}/agents/${AGENT_ID}/inbox" \
  -H "X-Agent-ID: ${AGENT_ID}" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}" \
  -w "\nHTTP_CODE:%{http_code}")
echo "    GET /agents/${AGENT_ID}/inbox -> $(echo "${RETRIEVE}" | grep -o 'HTTP_CODE:[0-9]*')"
echo "    response: $(echo "${RETRIEVE}" | grep -v 'HTTP_CODE:')"
MESSAGE_COUNT=$(echo "${RETRIEVE}" | grep -v 'HTTP_CODE:' | grep -o '"id"' | wc -l | tr -d ' ')
[ "${MESSAGE_COUNT}" -ge 1 ] || { echo "ERROR: expected >=1 message, got ${MESSAGE_COUNT}" >&2; exit 1; }

# A verdict that rode with the STORED message is visible here when the deliver
# body did not carry one (a clean pass omits it there — see step [4/6]). This
# is a second, separately attributed reading of the response bodies: the
# retrieve body carries the stored entry's "guard" object when the delivery was
# classified (docs/openapi.yaml, GET /agents/{id}/inbox). It is reported
# verbatim from that response, never merged into the deliver-body claim above.
if printf '%s' "${RETRIEVE}" | grep -q '"guard"'; then
  GUARD_RAW=$(printf '%s' "${RETRIEVE}" | grep -v '^HTTP_CODE:')
  echo "    guard verdict carried on the STORED inbox entry (read out of the retrieve response above — a different source from the deliver body):"
  verdict_fields
  verdict_fail_open
else
  echo "    guard verdict carried on the STORED inbox entry: this retrieve body carries no \"guard\" object either, so no verdict is attributable to this delivery."
fi

# 5. Ack the message (signed — lease_id AND message_ids from the retrieve response;
#    message_ids is required — a lease-only ack is rejected with 400)
echo "==> [6/6] signed ack (lease_id + message_ids)"
LEASE_ID=$(echo "${RETRIEVE}" | grep -v 'HTTP_CODE:' | grep -o '"lease_id":"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "${LEASE_ID}" ] || { echo "ERROR: no lease_id in retrieve response" >&2; exit 1; }
MESSAGE_ID=$(echo "${RETRIEVE}" | grep -v 'HTTP_CODE:' | grep -o '"id":"[^"]*"' | head -1 | cut -d'"' -f4)
[ -n "${MESSAGE_ID}" ] || { echo "ERROR: no message id in retrieve response" >&2; exit 1; }
TS=$(date +%s)
printf 'POST\n/agents/%s/inbox/ack\n%s' "${AGENT_ID}" "$TS" > "${WORKDIR}/payload.txt"
SIG=$(openssl pkeyutl -sign -rawin -inkey "${WORKDIR}/agent.key" -in "${WORKDIR}/payload.txt" 2>/dev/null \
  | xxd -p -c 128)
curl -sS -X POST "${AUTH_ARGS[@]}" "${CRIER_URL}/agents/${AGENT_ID}/inbox/ack" -H 'Content-Type: application/json' \
  -H "X-Agent-ID: ${AGENT_ID}" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}" \
  -d "{\"lease_id\":\"${LEASE_ID}\",\"message_ids\":[\"${MESSAGE_ID}\"]}" \
  -w "\n    POST /agents/${AGENT_ID}/inbox/ack -> %{http_code}\n"

# 6. Verify the inbox is now empty (signed retrieve again)
echo "==> verify empty after ack"
TS=$(date +%s)
printf 'GET\n/agents/%s/inbox\n%s' "${AGENT_ID}" "$TS" > "${WORKDIR}/payload.txt"
SIG=$(openssl pkeyutl -sign -rawin -inkey "${WORKDIR}/agent.key" -in "${WORKDIR}/payload.txt" 2>/dev/null \
  | xxd -p -c 128)
EMPTY=$(curl -sS "${AUTH_ARGS[@]}" "${CRIER_URL}/agents/${AGENT_ID}/inbox" \
  -H "X-Agent-ID: ${AGENT_ID}" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}" \
  -w "\nHTTP_CODE:%{http_code}")
echo "    response: $(echo "${EMPTY}" | grep -v 'HTTP_CODE:')"
echo
echo "==> demo round-trip complete: register -> deliver -> signed retrieve -> signed ack"
