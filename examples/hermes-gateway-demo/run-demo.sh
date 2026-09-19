#!/usr/bin/env bash
#
# CR-FEAT-008 — Hermes HTTP gateway cross-backend demo.
#
# Proves the vision end-to-end with two agents of DIFFERENT backends
# collaborating through crier webhook delivery:
#
#   harness-agent (inbox-backed — the crier-mcp harness side)
#        |
#        |  POST /agents/gateway-agent/inbox  (blocking, session_id + request_id)
#        v
#   crier server (memory backend, port <crier-port>, webhook driver enabled)
#        |
#        |  POST http://127.0.0.1:<adapter-port>/webhook  (hermes-http-gateway template)
#        v
#   adapter.py (fake Hermes gateway — verifies X-Crier-Signature, keeps the
#        |       per-session context window, replies in OpenAI shape)
#        |
#        |  optional forward: DeepSeek chat completions / live Hermes gateway
#        v
#   {"choices": [{"message": {"content": "<reply>"}}]}
#
# The blocking reply comes back to the sender correlated: 200 {id, reply,
# session_id, request_id}. Two messages in the SAME session prove session
# continuity (the adapter sees the same session_id and answers turn 2 with the
# turn-1 context in the window).
#
# Requirements: go, python3 (stdlib only), curl, ss (iproute2).
#   Port guards (QA-CRIER-9): never measures a server it did not start — after
#   /health the run asserts each listener is the pid it started.
#   SCRATCH PORTS (QA-CRIER-10): both ports are CHOSEN, not hard-coded. With
#   CRIER_PORT / ADAPTER_PORT unset the run walks two bounded candidate blocks —
#   18788..18792 (the crier server) and 18793..18797 (the adapter), 5 candidates
#   each — and uses the first free one, naming the holder pid, command line and
#   audit command of every candidate it skips (scripts/lib/port-guard.sh). A run
#   whose candidates are ALL occupied fails naming every attempted port and its
#   holder instead of skipping. Nothing is built or started until both are settled.
#   CR_AUTH_TOKEN        must NOT be set (demo runs auth-disabled)
#   CR_REQUIRE_AGENT_SIG is forced false (no per-agent signing in the demo)
#   CRIER_PORT           override crier port (default: first free of 18788+)
#   ADAPTER_PORT         override adapter port (default: first free of 18793+)
#   CRIER_PORT_CANDIDATES / ADAPTER_PORT_CANDIDATES
#                        how many candidates that service's default rotation may
#                        try (default 5 each)
#   CRIER_PORT_BASE / ADAPTER_PORT_BASE
#                        first candidate of that service's rotation
#                        (default 18788 / 18793)
#   DEMO_TRANSCRIPT      write the capture here instead of
#                        TRANSCRIPT-<date>.md next to this script
#   An EXPLICIT port is checked and never rotated away from: an occupied one
#   aborts the run naming its holder, because a run on a port the operator did
#   not name would misreport what was measured.
#   DEMO_HERMES_GATEWAY_URL  if set, the adapter forwards to this live Hermes
#                           gateway (OpenAI-compatible /chat/completions)
#   DEEPSEEK_API_KEY     picked up from ~/.hermes/.env automatically when
#                        present (2 LLM calls max — one per demo message);
#                        otherwise the adapter answers canned but
#                        session-aware, so the demo runs offline.
#
# Output: TRANSCRIPT-<date>.md in this directory (real output, teed live).
#
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"

# ── Scratch-port rotation (QA-CRIER-10) ───────────────────────────────────────
# Two services, two INDEPENDENT candidate blocks so a rotation on one can never
# land on a port the other owns. <SVC>_PORT_BASE names the first candidate of that
# service's rotation (overridable so a test can move the block onto ports it has
# proved free); the port itself is settled at the top of main(), before anything
# is built, started or announced.
CRIER_PORT_BASE="${CRIER_PORT_BASE:-18788}"
CRIER_PORT_CANDIDATES="${CRIER_PORT_CANDIDATES:-5}"
ADAPTER_PORT_BASE="${ADAPTER_PORT_BASE:-18793}"
ADAPTER_PORT_CANDIDATES="${ADAPTER_PORT_CANDIDATES:-5}"
# Mirrored, not defaulted: an empty value means "walk the candidates", and a
# caller-named port is carried through untouched (checked, never rotated).
CRIER_PORT="${CRIER_PORT:-}"
ADAPTER_PORT="${ADAPTER_PORT:-}"
WEBHOOK_URL=""
CRIER_BASE=""

# Port guards (QA-CRIER-9): this harness starts BOTH servers it measures.
# require_free_port refuses to start on a taken port; assert_port_owned proves,
# after /health answers, that the listener is the pid started here; and
# wait_http_or_die aborts if a started process dies before answering instead of
# letting the poll be answered by a stale or foreign server.
. "$REPO_ROOT/scripts/lib/port-guard.sh"

# Every probe below is a LOOPBACK probe on a port this script chose, and curl
# has no built-in loopback exemption: with an ambient HTTP_PROXY (a corporate
# default, a sandbox egress proxy, a CI image) curl would send them to the proxy
# and never reach the services started here (QA-CRIER-21). Merge the loopback
# names into no_proxy/NO_PROXY before the first curl; a genuinely external host
# still honours the proxy.
guard_loopback_off_proxy

TS="$(date +%Y%m%d-%H%M%S)"
SESSION_ID="sess-demo-${TS}"
THREAD_ID="thr-demo-${TS}"
REQ1="req-${TS}-1"
REQ2="req-${TS}-2"

WORKDIR="$(mktemp -d /tmp/crier-demo.XXXXXX)"
CRIER_BIN="$WORKDIR/crier"
CRIER_LOG="$WORKDIR/crier.log"
ADAPTER_LOG="$WORKDIR/adapter.log"
# DEMO_TRANSCRIPT (as in the ws-mesh demo) points the capture outside the repo —
# the selftest uses it so a test run cannot dirty git status; the default keeps
# writing the historical TRANSCRIPT-<date>.md next to this script.
TRANSCRIPT="${DEMO_TRANSCRIPT:-$DEMO_DIR/TRANSCRIPT-$(date +%Y-%m-%d).md}"

CRIER_PID=""
ADAPTER_PID=""

cleanup() {
  if [ -n "$CRIER_PID" ]; then kill "$CRIER_PID" 2>/dev/null || true; fi
  if [ -n "$ADAPTER_PID" ]; then kill "$ADAPTER_PID" 2>/dev/null || true; fi
  sleep 0.3
}

step() { echo; echo "══════════════════════════════════════════════════════════"; echo "== $*"; }
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; echo "  run artifacts kept in: $WORKDIR" >&2; exit 1; }
assert_eq() { # name got want
  if [ "$2" = "$3" ]; then pass "$1 = $2"; else fail "$1: got '$2', want '$3'"; fi
}
# pyget <json-file> <python-expr> — print a field, e.g. pyget f "['request_id']"
pyget() { python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d$2)" "$1"; }

main() {
  trap cleanup EXIT

  # ── Settle both scratch ports BEFORE anything is built, started or announced ─
  # select_scratch_port (scripts/lib/port-guard.sh) is the shared selector the
  # other example runners use: it walks <base>..<base>+<budget>-1 in order, prints
  # the holder pid/command/audit line of every candidate it skips, and selects the
  # first free one. A hard-coded scratch port made this demo abort whenever
  # anything already listened there — a long-lived unrelated listener, or a
  # squatter that took the port between two runs of this same script
  # (QA-CRIER-10). An EXPLICIT CRIER_PORT / ADAPTER_PORT is honored literally and
  # never rotated: an occupied one aborts the run naming its holder, because a run
  # on a port the operator did not name would misreport what was measured. Every
  # candidate occupied is a named failure, never a silent skip.
  #
  # Nothing has been built or started at this point, so a refusal here costs
  # nothing and measures nothing.
  select_scratch_port "${CRIER_PORT:-}" "$CRIER_PORT_BASE" \
    "the crier server (hermes-gateway-demo)" "$CRIER_PORT_CANDIDATES" "CRIER_PORT"
  CRIER_PORT="$PORT_GUARD_SELECTED"
  select_scratch_port "${ADAPTER_PORT:-}" "$ADAPTER_PORT_BASE" \
    "the gateway adapter (hermes-gateway-demo)" "$ADAPTER_PORT_CANDIDATES" "ADAPTER_PORT"
  ADAPTER_PORT="$PORT_GUARD_SELECTED"
  # Every client, server and webhook below is addressed through these two values —
  # there is no other place a port is spelled out (a component left on its own
  # default would be measured on a port nothing selected).
  WEBHOOK_URL="http://127.0.0.1:${ADAPTER_PORT}/webhook"
  CRIER_BASE="http://127.0.0.1:${CRIER_PORT}"

  step "CR-FEAT-008 demo — $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
  echo "  crier port   : $CRIER_PORT (memory backend, CR_AUTH_TOKEN unset, CR_REQUIRE_AGENT_SIG=false)"
  echo "  adapter port : $ADAPTER_PORT"
  echo "  session_id   : $SESSION_ID"
  echo "  thread_id    : $THREAD_ID (carried in the deliver payload — see README §session mapping)"
  echo "  transcript   : $TRANSCRIPT"

  # 0. Tooling + port sanity
  step "[0/7] preflight"
  command -v go >/dev/null || fail "go not on PATH"
  command -v python3 >/dev/null || fail "python3 not on PATH"
  command -v curl >/dev/null || fail "curl not on PATH"
  command -v ss >/dev/null || fail "ss not on PATH (iproute2 — the port guards need it)"
  if [ -n "${CR_AUTH_TOKEN:-}" ]; then
    fail "CR_AUTH_TOKEN is set in the environment — the demo runs auth-disabled (unset it)"
  fi
  # The two ports were settled above, before anything was built or started: the
  # crier server can no longer die on "address already in use" while the /health
  # poll below is answered by the squatter that held the port (QA-CRIER-9/10).
  echo "  crier server   : :$CRIER_PORT (selected by port-guard)"
  echo "  gateway adapter: :$ADAPTER_PORT (selected by port-guard)"

  # 1. Build the crier server binary
  step "[1/7] build crier server"
  (cd "$REPO_ROOT" && go build -o "$CRIER_BIN" ./cmd/server)
  echo "  built: $CRIER_BIN"

  # 2. LLM availability (optional — continuity + correlation is the point)
  step "[2/7] reply backend selection"
  DEEPSEEK_KEY="$(grep '^DEEPSEEK_API_KEY=' "$HOME/.hermes/.env" 2>/dev/null | cut -d= -f2- | sed -e 's/^"//' -e 's/"$//' || true)"
  BACKEND_URL=""
  if [ -n "${DEMO_HERMES_GATEWAY_URL:-}" ]; then
    BACKEND_URL="$DEMO_HERMES_GATEWAY_URL"
    echo "  live Hermes gateway: $BACKEND_URL (adapter forwards session history there)"
  elif [ -n "$DEEPSEEK_KEY" ]; then
    BACKEND_URL="https://api.deepseek.com/chat/completions"
    echo "  DEEPSEEK_API_KEY found in ~/.hermes/.env — live LLM replies (exactly 2 calls)"
  else
    echo "  no DEEPSEEK_API_KEY / DEMO_HERMES_GATEWAY_URL — adapter answers canned but session-aware"
  fi

  # 3. Start the adapter (fake Hermes gateway)
  step "[3/7] start gateway adapter (fake Hermes endpoint)"
  ADAPTER_ENV=(DEMO_WEBHOOK_SECRET=demo-webhook-secret ADAPTER_PORT="$ADAPTER_PORT")
  [ -n "$BACKEND_URL" ] && ADAPTER_ENV+=(DEMO_BACKEND_URL="$BACKEND_URL")
  [ -n "$DEEPSEEK_KEY" ] && ADAPTER_ENV+=(DEEPSEEK_API_KEY="$DEEPSEEK_KEY")
  env "${ADAPTER_ENV[@]}" python3 "$DEMO_DIR/adapter.py" >"$ADAPTER_LOG" 2>&1 &
  ADAPTER_PID=$!
  wait_http_or_die "http://127.0.0.1:$ADAPTER_PORT/healthz" "$ADAPTER_PID" "$ADAPTER_LOG" "gateway adapter"
  assert_port_owned "$ADAPTER_PORT" "$ADAPTER_PID" "gateway adapter"
  echo "  adapter up: $WEBHOOK_URL (pid $ADAPTER_PID, log $ADAPTER_LOG)"

  # 4. Start the crier server
  step "[4/7] start crier server"
  env -u CR_AUTH_TOKEN -u CR_DATABASE_URL -u CR_FED_LINKS \
      CRIER_PORT="$CRIER_PORT" CR_REQUIRE_AGENT_SIG=false \
      CR_WEBHOOK_SECRET=demo-webhook-secret \
      "$CRIER_BIN" >"$CRIER_LOG" 2>&1 &
  CRIER_PID=$!
  wait_http_or_die "$CRIER_BASE/health" "$CRIER_PID" "$CRIER_LOG" "crier server"
  assert_port_owned "$CRIER_PORT" "$CRIER_PID" "crier server"
  echo "  crier up: $CRIER_BASE (pid $CRIER_PID, log $CRIER_LOG)"

  # 5. Register the two agents (different backends)
  step "[5/7] register agents"
  PUBKEY="deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
  HTTP="$(curl -sS -o "$WORKDIR/reg-harness.json" -w '%{http_code}' -X POST \
    "$CRIER_BASE/agents" -H 'Content-Type: application/json' \
    --data "{\"id\":\"harness-agent\",\"public_key\":\"$PUBKEY\",\"capabilities\":[\"messenger\",\"harness\"]}")"
  [ "$HTTP" = "201" ] || fail "register harness-agent: HTTP $HTTP ($(cat "$WORKDIR/reg-harness.json"))"
  echo "  registered harness-agent (inbox-backed — crier-mcp harness side)"
  HTTP="$(curl -sS -o "$WORKDIR/reg-gateway.json" -w '%{http_code}' -X POST \
    "$CRIER_BASE/agents" -H 'Content-Type: application/json' \
    --data "{\"id\":\"gateway-agent\",\"public_key\":\"$PUBKEY\",\"capabilities\":[\"solver\"],\"webhook\":{\"url\":\"$WEBHOOK_URL\",\"schema_template\":\"hermes-http-gateway\",\"delivery_mode\":\"blocking\",\"timeout_ms\":60000}}")"
  [ "$HTTP" = "201" ] || fail "register gateway-agent: HTTP $HTTP ($(cat "$WORKDIR/reg-gateway.json"))"
  echo "  registered gateway-agent (webhook -> $WEBHOOK_URL, schema_template=hermes-http-gateway, delivery_mode=blocking)"

  # 6. Puzzle round trip 1 — harness -> gateway, blocking, session S
  step "[6/7] puzzle round trip 1 (harness-agent -> gateway-agent, blocking)"
  cat >"$WORKDIR/deliver1.json" <<JSON
{"payload": {"text": "What is 17 times 23?", "thread_id": "$THREAD_ID"}, "sender": "harness-agent", "session_id": "$SESSION_ID", "request_id": "$REQ1", "delivery_mode": "blocking", "timeout_ms": 60000}
JSON
  echo "  deliver: POST /agents/gateway-agent/inbox $REQ1 session=$SESSION_ID"
  HTTP="$(curl -sS -o "$WORKDIR/resp1.json" -w '%{http_code}' -X POST \
    "$CRIER_BASE/agents/gateway-agent/inbox" -H 'Content-Type: application/json' \
    --data @"$WORKDIR/deliver1.json")"
  [ "$HTTP" = "200" ] || fail "blocking deliver round 1: HTTP $HTTP ($(cat "$WORKDIR/resp1.json" 2>/dev/null || true))"
  ID1="$(pyget "$WORKDIR/resp1.json" "['id']")"
  R1OUT="$(pyget "$WORKDIR/resp1.json" "['request_id']")"
  S1OUT="$(pyget "$WORKDIR/resp1.json" "['session_id']")"
  REPLY1="$(pyget "$WORKDIR/resp1.json" "['reply']")"
  echo "  200 response: id=$ID1 request_id=$R1OUT session_id=$S1OUT"
  echo "  reply: $REPLY1"
  [[ "$ID1" =~ ^[0-9a-f]{24}$ ]] || fail "response id not a 24-hex message id: $ID1"
  assert_eq "blocking reply correlated (request_id echoed)" "$R1OUT" "$REQ1"
  assert_eq "blocking reply session_id echoed" "$S1OUT" "$SESSION_ID"
  case "$REPLY1" in *"session=$SESSION_ID"*) pass "reply echoes session_id (sender sees the session mapping)";; *) fail "reply missing session echo: $REPLY1";; esac
  case "$REPLY1" in *"turn=1"*) pass "adapter counted this as turn 1 of the session";; *) fail "reply missing turn marker: $REPLY1";; esac

  # Envelope session-mapping evidence (spec §3): X-Crier-Session header vs body slot
  step "envelope session mapping evidence (X-Crier-Session header vs body slot)"
  python3 - "$ADAPTER_LOG" "$SESSION_ID" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    lines = [json.loads(l) for l in f if '"path": "/webhook"' in l]
assert lines, "no webhook lines in adapter log"
first = lines[0]
print("  X-Crier-Session header : %s" % first.get("session_header"))
print("  body session_id slot   : %r" % first.get("session_body"))
print("  resolved session_id    : %s" % first.get("session_id"))
if first.get("session_body"):
    print("PASS: template body carried the session_id (body slot populated)")
else:
    print("NOTE: body session_id slot rendered empty — the server predates CR-GAP-037")
    print("      (the JSON round-trip in buildContext, internal/webhook/schema.go);")
    print("      X-Crier-Session header carried the session per spec §3).")
    print("      See README \"Session-id body slot\". Demo continues — continuity")
    print("      proven via the header channel.")
PY

  # 7. Follow-up in the SAME session — proves continuity
  step "[7/7] follow-up round trip 2 (same session, turn 2)"
  cat >"$WORKDIR/deliver2.json" <<JSON
{"payload": {"text": "Add 11 to your previous answer.", "thread_id": "$THREAD_ID"}, "sender": "harness-agent", "session_id": "$SESSION_ID", "request_id": "$REQ2", "delivery_mode": "blocking", "timeout_ms": 60000}
JSON
  echo "  deliver: POST /agents/gateway-agent/inbox $REQ2 session=$SESSION_ID (same session)"
  HTTP="$(curl -sS -o "$WORKDIR/resp2.json" -w '%{http_code}' -X POST \
    "$CRIER_BASE/agents/gateway-agent/inbox" -H 'Content-Type: application/json' \
    --data @"$WORKDIR/deliver2.json")"
  [ "$HTTP" = "200" ] || fail "blocking deliver round 2: HTTP $HTTP ($(cat "$WORKDIR/resp2.json" 2>/dev/null || true))"
  ID2="$(pyget "$WORKDIR/resp2.json" "['id']")"
  R2OUT="$(pyget "$WORKDIR/resp2.json" "['request_id']")"
  S2OUT="$(pyget "$WORKDIR/resp2.json" "['session_id']")"
  REPLY2="$(pyget "$WORKDIR/resp2.json" "['reply']")"
  echo "  200 response: id=$ID2 request_id=$R2OUT session_id=$S2OUT"
  echo "  reply: $REPLY2"
  [[ "$ID2" =~ ^[0-9a-f]{24}$ ]] || fail "response id not a 24-hex message id: $ID2"
  assert_eq "follow-up correlated (request_id echoed)" "$R2OUT" "$REQ2"
  assert_eq "follow-up same session_id" "$S2OUT" "$SESSION_ID"
  case "$REPLY2" in *"turn=2"*) pass "adapter saw turn 2 in the SAME session — session continuity on the wire";; *) fail "reply missing turn=2 marker: $REPLY2";; esac

  # 8. Adapter-side session store — both messages under one session_id
  step "adapter session continuity check (GET /sessions)"
  curl -sS "http://127.0.0.1:$ADAPTER_PORT/sessions" >"$WORKDIR/sessions.json"
  python3 - "$WORKDIR/sessions.json" "$SESSION_ID" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
sid = sys.argv[2]
rows = [s for s in d["sessions"] if s["session_id"] == sid]
assert len(rows) == 1, "expected exactly 1 session row for %s, got %d" % (sid, len(rows))
s = rows[0]
assert s["turn_count"] == 2, "expected 2 turns under one session, got %d" % s["turn_count"]
assert s["turns"][0]["content"] != s["turns"][1]["content"], "turns must differ"
print("PASS: adapter recorded %d turns under ONE session_id %s (thread=%s)"
      % (s["turn_count"], sid, s["thread_id"]))
for t in s["turns"]:
    print("       turn %d: %r -> %r [%s]" % (t["turn"], t["content"], t["reply"], t["backend"]))
PY

  # 9. Inbox round trip — the harness (crier-mcp) side still works
  step "inbox round trip (gateway-agent -> harness-agent inbox, retrieve + ack)"
  cat >"$WORKDIR/deliver-inbox.json" <<JSON
{"payload": {"text": "Puzzle solved in session $SESSION_ID — thanks!"}, "sender": "gateway-agent"}
JSON
  HTTP="$(curl -sS -o "$WORKDIR/resp-inbox.json" -w '%{http_code}' -X POST \
    "$CRIER_BASE/agents/harness-agent/inbox" -H 'Content-Type: application/json' \
    --data @"$WORKDIR/deliver-inbox.json")"
  [ "$HTTP" = "201" ] || fail "inbox deliver: HTTP $HTTP ($(cat "$WORKDIR/resp-inbox.json" 2>/dev/null || true))"
  INBOX_ID="$(pyget "$WORKDIR/resp-inbox.json" "['id']")"
  echo "  delivered to harness-agent inbox: id=$INBOX_ID (HTTP 201)"
  curl -sS "$CRIER_BASE/agents/harness-agent/inbox?max=10&lease=30" >"$WORKDIR/retrieve.json"
  python3 - "$WORKDIR/retrieve.json" "$INBOX_ID" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert any(m["id"] == sys.argv[2] for m in d["messages"]), \
    "message %s missing from retrieve" % sys.argv[2]
print("PASS: harness-agent inbox retrieved the message (lease_id=%s)" % d["lease_id"])
PY
  LEASE="$(pyget "$WORKDIR/retrieve.json" "['lease_id']")"
  HTTP="$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
    "$CRIER_BASE/agents/harness-agent/inbox/ack" -H 'Content-Type: application/json' \
    --data "{\"lease_id\":\"$LEASE\",\"message_ids\":[\"$INBOX_ID\"]}")"
  [ "$HTTP" = "204" ] || fail "inbox ack: HTTP $HTTP"
  pass "inbox ack (204) — harness side round trip complete"

  # 10. Adapter webhook log (envelope + signature evidence)
  step "adapter webhook log (envelope contract evidence)"
  grep '/webhook' "$ADAPTER_LOG" | sed 's/^/  /' || true

  # 11. Summary
  step "summary"
  echo "  crier server : $CRIER_BASE (memory backend, webhook driver, HMAC signing on)"
  echo "  adapter      : $WEBHOOK_URL (fake Hermes gateway, signature verified)"
  echo "  reply backend: ${BACKEND_URL:-canned (no LLM)}"
  echo "  round 1      : $ID1 -> reply: $REPLY1"
  echo "  round 2      : $ID2 -> reply: $REPLY2 (same session_id $SESSION_ID — continuity)"
  echo "  inbox        : $INBOX_ID delivered + retrieved + acked by harness-agent"
  echo "  run artifacts: $WORKDIR (crier.log, adapter.log, json payloads)"
  echo
  echo "DEMO PASSED — transcript: $TRANSCRIPT"
}

main 2>&1 | tee "$TRANSCRIPT"
status=${PIPESTATUS[0]}
exit "$status"
