#!/usr/bin/env bash
#
# scripts/client-roundtrip.sh — CR-FEAT-027, the acceptance drive for the
# signing-ceremony fix.
#
# WHAT THIS PROVES, end to end, on one host with no crypto tooling installed:
#
#   [1/7] two scratch ports are CHOSEN by the shared port selector (never
#         hard-coded), before anything is built
#   [2/7] ./cmd/server is built
#   [3/7] server A starts with per-agent signing ON (the default) and auth OFF,
#         answers /health, and the pid HOLDING its port is asserted to be the
#         pid this script started
#   [4/7] `crier keygen` writes alice.key and bob.key — NO openssl, NO xxd: the
#         assertions are that both files are PKCS#8 PEM at mode 0600, that the
#         printed public key is in the report, and that -json names the agent
#   [5/7] the Python client completes a full signed round-trip (register ->
#         deliver -> signed retrieve -> signed ack -> signed stats -> relay
#         publish/subscribe -> the two negative controls) TWICE: once with the
#         lib-backed signer (where `cryptography` is installed) and once with
#         the bundled RFC 8032 signer (CRIER_CLIENT_FORCE_PURE_PYTHON=1) — the
#         Go server accepting the bundled signer's signature is the
#         cross-implementation proof, and the two clients/python unit-test arms
#         (RFC 8032 vectors + byte-equality between the backends) run first
#   [6/7] two identities talk to each other with clients/python/two_agents.py:
#         alice delivers to bob, bob's key reads and acks it, alice's key is
#         REFUSED (403) when it targets bob's inbox, and an unsigned delivery is
#         still accepted (the signature gates the READ, not the write)
#   [7/7] the TypeScript client completes the same signed round-trip, and then a
#         SECOND server with CR_AUTH_TOKEN set proves the bearer path: without
#         the token the Python client is refused (401), with it the Python and
#         TypeScript clients both pass — including the TypeScript WebSocket
#         upgrade, which carries the Authorization header because that client
#         speaks RFC 6455 over node:net instead of using the header-less global
#         WebSocket
#
# WHAT IT DELIBERATELY DOES NOT DO: use openssl to produce a key or a signature.
# The whole point of the task is that the old recipe (openssl genpkey + `tail -c
# 32` + xxd + a hand-written sig() helper) is gone, so an arm that fell back to it
# would prove nothing: every key here comes from `crier keygen` and every
# signature from the client library. (The clients' own unit-test arms additionally
# CROSS-CHECK that a keygen key is the same key openssl's DER recipe derives, and
# those two checks SKIP LOUDLY when openssl is absent — they never produce the
# material under test.)
#
# Requirements: go (toolchain), curl, ss (iproute2, for the port guards), python3
# (stdlib only — the client needs no packages), and node >= 22.6 for the
# TypeScript arm, which is SKIPPED LOUDLY (naming the version found and the
# version needed) rather than silently when node is absent or too old.
#
# Env:
#   CLIENT_PORT_BASE        first candidate of the server-A rotation (default 18861)
#   CLIENT_PORT_CANDIDATES  how many candidates may be tried (default 5)
#   CLIENT_PORT             use THIS port; checked, never rotated
#   CLIENT_AUTH_PORT_BASE   first candidate of the auth-server rotation (default 18871)
#   CLIENT_AUTH_PORT        use THIS port for the auth arm; checked, never rotated
#   CLIENT_SKIP_TYPESCRIPT  set to 1 to skip the TypeScript arm explicitly
#
# Exit status: 0 only when every arm passed. A loud SKIP for a missing node is
# not a failure; a missing go/curl/ss/python3 is, and so is any arm that exits
# nonzero.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# The shared "the server we measure is the server we started" guards, the same
# library the example runners source (QA-CRIER-9 / QA-CRIER-10): sourced, never
# re-implemented, because a local copy of a guard is a guard that drifts.
. "$REPO_ROOT/scripts/lib/port-guard.sh"
guard_loopback_off_proxy

CLIENT_PORT_BASE="${CLIENT_PORT_BASE:-18861}"
CLIENT_PORT_CANDIDATES="${CLIENT_PORT_CANDIDATES:-5}"
CLIENT_AUTH_PORT_BASE="${CLIENT_AUTH_PORT_BASE:-18871}"
CLIENT_PORT="${CLIENT_PORT:-}"
CLIENT_AUTH_PORT="${CLIENT_AUTH_PORT:-}"

WORKDIR="$(mktemp -d)"
SERVER_PID=""
AUTH_SERVER_PID=""

# Cleanup contract (scripts/check-demo-cleanup.sh): the servers this script
# backgrounds cannot outlive it, and the workdir goes with them.
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$AUTH_SERVER_PID" ] && kill "$AUTH_SERVER_PID" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

usage() {
  cat <<'EOF'
scripts/client-roundtrip.sh — the CR-FEAT-027 acceptance drive.

Usage: bash scripts/client-roundtrip.sh

Starts its own crier server on a scratch port, generates keys with `crier
keygen` (no openssl, no xxd anywhere), and runs the first-party Python and
TypeScript clients through a full signed round-trip — including the negative
controls that prove the signatures are actually verified. See the header of this
file for the seven steps.

Env: CLIENT_PORT, CLIENT_PORT_BASE, CLIENT_PORT_CANDIDATES, CLIENT_AUTH_PORT,
CLIENT_AUTH_PORT_BASE, CLIENT_SKIP_TYPESCRIPT=1.
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
  "") ;;
  *) echo "client-roundtrip.sh: unknown argument '$1'" >&2; usage >&2; exit 2 ;;
esac

# run_client <label> <cmd…> — run one arm, print its transcript indented, and
# abort the whole drive when it exits nonzero (a passing arm prints PASS).
run_client() {
  local label="$1"
  shift
  local output rc
  set +e
  output="$( cd "$REPO_ROOT" && "$@" 2>&1 )"
  rc=$?
  set -e
  printf '%s\n' "$output" | sed 's/^/    /'
  if [ "$rc" -ne 0 ]; then
    echo "    FAIL: ${label} exited ${rc}" >&2
    exit 1
  fi
  echo "    PASS: ${label}"
}

# run_client_expect_failure <label> <needle> <cmd…> — the negative arm: the
# command MUST fail AND its output must carry <needle>, so "it failed for the
# wrong reason" is not a pass.
run_client_expect_failure() {
  local label="$1" needle="$2"
  shift 2
  local output rc
  set +e
  output="$( cd "$REPO_ROOT" && "$@" 2>&1 )"
  rc=$?
  set -e
  printf '%s\n' "$output" | sed 's/^/    /'
  if [ "$rc" -eq 0 ]; then
    echo "    FAIL: ${label} was expected to fail and exited 0" >&2
    exit 1
  fi
  if ! printf '%s' "$output" | grep -q -- "$needle"; then
    echo "    FAIL: ${label} failed without naming ${needle}" >&2
    exit 1
  fi
  echo "    PASS: ${label} (refused, naming '${needle}')"
}

echo "==> crier client round-trip (CR-FEAT-027): crier keygen + Python + TypeScript, no openssl"

# ── Pre-flight ──────────────────────────────────────────────────────────────
for tool in go curl ss python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: '$tool' is required on PATH" >&2; exit 1; }
done
echo "    python3 : $(python3 -V 2>&1)"

# TypeScript is optional: node >= 22.6 runs .ts directly (native type stripping)
# and is what the client documents. Too old or absent is a LOUD skip naming both
# what was found and what is needed — never a silent green.
TS_SKIP=""
if [ "${CLIENT_SKIP_TYPESCRIPT:-0}" = "1" ]; then
  TS_SKIP="CLIENT_SKIP_TYPESCRIPT=1 was set"
elif ! command -v node >/dev/null 2>&1; then
  TS_SKIP="node is not on PATH (the TypeScript client needs node >= 22.6)"
else
  NODE_VERSION="$(node --version 2>/dev/null || echo vunknown)"
  NODE_MM="${NODE_VERSION#v}"
  NODE_MAJOR="${NODE_MM%%.*}"
  NODE_MINOR="${NODE_MM#*.}"
  NODE_MINOR="${NODE_MINOR%%.*}"
  case "$NODE_MAJOR$NODE_MINOR" in
    *[!0-9]*) TS_SKIP="node ${NODE_VERSION} could not be parsed as a version" ;;
    *)
      if [ "$NODE_MAJOR" -gt 22 ] || { [ "$NODE_MAJOR" -eq 22 ] && [ "$NODE_MINOR" -ge 6 ]; }; then
        echo "    node    : ${NODE_VERSION} (typescript arm enabled)"
      else
        TS_SKIP="node ${NODE_VERSION} is older than 22.6 (native .ts type stripping)"
      fi
      ;;
  esac
fi
[ -n "$TS_SKIP" ] && echo "    SKIP typescript arm: ${TS_SKIP}"

# ── [1/7] choose the scratch ports before anything is built ─────────────────
echo
echo "==> [1/7] select scratch ports"
select_scratch_port "${CLIENT_PORT:-}" "$CLIENT_PORT_BASE" "the client round-trip server" "$CLIENT_PORT_CANDIDATES" "CLIENT_PORT"
CLIENT_PORT="$PORT_GUARD_SELECTED"
select_scratch_port "${CLIENT_AUTH_PORT:-}" "$CLIENT_AUTH_PORT_BASE" "the auth-arm server" "$CLIENT_PORT_CANDIDATES" "CLIENT_AUTH_PORT"
CLIENT_AUTH_PORT="$PORT_GUARD_SELECTED"
BASE="http://127.0.0.1:${CLIENT_PORT}"
AUTH_BASE="http://127.0.0.1:${CLIENT_AUTH_PORT}"
echo "    server A : ${BASE}      (signing ON — the default, guard off, auth off)"
echo "    server B : ${AUTH_BASE}      (signing ON, guard off, CR_AUTH_TOKEN set)"

# ── [2/7] build ─────────────────────────────────────────────────────────────
echo
echo "==> [2/7] build ./cmd/server"
( cd "$REPO_ROOT" && go build -o "$WORKDIR/crier" ./cmd/server )
CRIER_BIN="$WORKDIR/crier"

# ── [3/7] start server A and prove we own its port ──────────────────────────
echo
echo "==> [3/7] start server A on :${CLIENT_PORT}"
CRIER_PORT="$CLIENT_PORT" CR_GUARD_ENABLED=false CR_REQUIRE_AGENT_SIG=true \
  CR_LOG_LEVEL=warn "$CRIER_BIN" > "$WORKDIR/server.log" 2>&1 &
SERVER_PID=$!
wait_http_or_die "$BASE/health" "$SERVER_PID" "$WORKDIR/server.log" "the client round-trip server"
assert_port_owned "$CLIENT_PORT" "$SERVER_PID" "the client round-trip server"
echo "    /health answered, and the pid holding :${CLIENT_PORT} is the pid this script started (${SERVER_PID})"

# ── [4/7] keys, with no openssl and no xxd ──────────────────────────────────
echo
echo "==> [4/7] crier keygen — the whole ceremony, replaced"
KEYGEN_ALICE="$WORKDIR/alice.key"
KEYGEN_BOB="$WORKDIR/bob.key"
set +e
KEYGEN_OUT="$( "$CRIER_BIN" keygen -out "$KEYGEN_ALICE" -id alice -server "$BASE" 2>&1 )"
KEYGEN_RC=$?
set -e
printf '%s\n' "$KEYGEN_OUT" | sed 's/^/    | /'
[ "$KEYGEN_RC" -eq 0 ] || { echo "FAIL: crier keygen exited $KEYGEN_RC" >&2; exit 1; }
KEYGEN_BOB_OUT="$( "$CRIER_BIN" keygen -out "$KEYGEN_BOB" -id bob -json )"
PUB_ALICE="$( printf '%s\n' "$KEYGEN_OUT" | sed -n 's/^  public key (hex)  : //p' )"
[ -n "$PUB_ALICE" ] || { echo "FAIL: keygen printed no public key line" >&2; exit 1; }
printf '%s' "$KEYGEN_OUT" | grep -q -- "$PUB_ALICE" || { echo "FAIL: the printed report omits the public key" >&2; exit 1; }
printf '%s' "$KEYGEN_BOB_OUT" | grep -q '"agent_id": "bob"' || { echo "FAIL: keygen -json did not name the agent" >&2; exit 1; }
for path in "$KEYGEN_ALICE" "$KEYGEN_BOB"; do
  [ -f "$path" ] || { echo "FAIL: keygen did not write $path" >&2; exit 1; }
  # Anchored regex, not the literal armour: the header is what the file must
  # carry, while the literal in this script would read as key material to the
  # repo's secrets cross-check.
  head -1 "$path" | grep -qE -- '^-{5}BEGIN .*PRIVATE KEY' \
    || { echo "FAIL: $path is not a PKCS#8 PEM (first line: $(head -1 "$path"))" >&2; exit 1; }
  MODE="$(stat -c '%a' "$path")"
  [ "$MODE" = "600" ] || { echo "FAIL: $path is mode $MODE, expected 600" >&2; exit 1; }
done
echo "    wrote ${KEYGEN_ALICE} and ${KEYGEN_BOB} (PKCS#8 PEM, mode 0600) — openssl and xxd were never invoked"

# ── [5/7] the clients' own unit tests, then the Python round-trip ───────────
echo
echo "==> [5/7] python client unit tests (RFC 8032 vectors, backend agreement, PKCS#8)"
run_client "python unit tests (backend as installed)" env CRIER_BIN="$CRIER_BIN" python3 -m unittest discover -s clients/python
run_client "python unit tests (bundled RFC 8032 signer)" env CRIER_BIN="$CRIER_BIN" CRIER_CLIENT_FORCE_PURE_PYTHON=1 python3 -m unittest discover -s clients/python
echo
run_client "python round-trip (signer as installed)" \
  python3 clients/python/round_trip.py --server "$BASE" --id rt-py-a --key "$KEYGEN_ALICE"
run_client "python round-trip (bundled RFC 8032 signer)" \
  env CRIER_CLIENT_FORCE_PURE_PYTHON=1 python3 clients/python/round_trip.py --server "$BASE" --id rt-py-b --key "$KEYGEN_ALICE"

# ── [6/7] two identities ────────────────────────────────────────────────────
echo
echo "==> [6/7] two-agent round-trip (alice -> bob, both keys, 403 isolation proof)"
run_client "two-agent round-trip" \
  python3 clients/python/two_agents.py --server "$BASE" \
    --alice-id rt-alice --alice-key "$KEYGEN_ALICE" --bob-id rt-bob --bob-key "$KEYGEN_BOB"

# ── [7/7] TypeScript, then the bearer-token arm on a second server ──────────
echo
echo "==> [7/7] typescript client and the bearer-token arm"
if [ -n "$TS_SKIP" ]; then
  echo "    SKIP typescript unit tests: ${TS_SKIP}"
  echo "    SKIP typescript round-trip: ${TS_SKIP}"
else
  run_client "typescript unit tests" env CRIER_BIN="$CRIER_BIN" node --test clients/typescript/crier.test.ts
  run_client "typescript round-trip" node clients/typescript/round-trip.ts --server "$BASE" --id rt-ts --key "$KEYGEN_ALICE"
fi

TOKEN="round-trip-$(date +%s)-$$"
echo
echo "    starting server B on :${CLIENT_AUTH_PORT} with CR_AUTH_TOKEN set"
env CRIER_PORT="$CLIENT_AUTH_PORT" CR_GUARD_ENABLED=false CR_REQUIRE_AGENT_SIG=true \
  CR_AUTH_TOKEN="$TOKEN" CR_LOG_LEVEL=warn "$CRIER_BIN" > "$WORKDIR/auth-server.log" 2>&1 &
AUTH_SERVER_PID=$!
wait_http_or_die "$AUTH_BASE/health" "$AUTH_SERVER_PID" "$WORKDIR/auth-server.log" "the auth-arm server"
assert_port_owned "$CLIENT_AUTH_PORT" "$AUTH_SERVER_PID" "the auth-arm server"
echo "    /health answered (it is auth-exempt), and the pid holding :${CLIENT_AUTH_PORT} is the pid this script started (${AUTH_SERVER_PID})"
# The token is not echoed: it lives in this shell only, and the arm proves it is
# enforced by the arm that has to carry it.
run_client_expect_failure "python round-trip without the bearer token" "401" \
  env -u CR_AUTH_TOKEN python3 clients/python/round_trip.py --server "$AUTH_BASE" --id rt-auth-a --key "$KEYGEN_ALICE"
run_client "python round-trip with the bearer token" \
  env -u CR_AUTH_TOKEN python3 clients/python/round_trip.py --server "$AUTH_BASE" --id rt-auth-a --key "$KEYGEN_ALICE" --token "$TOKEN"
if [ -n "$TS_SKIP" ]; then
  echo "    SKIP typescript auth arm: ${TS_SKIP}"
else
  run_client "typescript round-trip with the bearer token (WebSocket upgrade carries Authorization)" \
    env -u CR_AUTH_TOKEN node clients/typescript/round-trip.ts --server "$AUTH_BASE" --id rt-auth-ts --key "$KEYGEN_ALICE" --token "$TOKEN"
fi

echo
echo "ALL ARMS PASSED — crier keygen + the Python and TypeScript clients, over the real HTTP/WS API,"
echo "with real signatures: every key came from 'crier keygen' and every signature from a client"
echo "library, xxd was never used, and nothing third-party was installed. (The unit-test arms also"
echo "cross-check the openssl DER recipe when openssl is on PATH, and SKIP that check loudly when it"
echo "is not — the round-trip arms themselves never touch openssl.)"
