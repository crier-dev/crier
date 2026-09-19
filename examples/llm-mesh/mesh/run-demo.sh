#!/usr/bin/env bash
#
# Crier LLM-mesh demo (raw protocol lane): two LLM agents solve a puzzle
# together by speaking the mesh protocol DIRECTLY through the Python client
# (crier_mesh.py) — WebSocket frames, correlation ids, the whole wire.
#
# This is the "transport lane": it demonstrates the protocol itself. The
# harness-facing lane (../run-demo.sh) shows the same collaboration through
# the crier-mcp bridge, where harnesses never touch the wire.
#
# Bounded and named (DF-CRIER-214): the run always ends with an outcome, never
# a hang. PRODUCT <json> is the controller's verification of both agents
# against the ground truth (also $OUT/verdict.json); exit 0 only if both
# solved, 1 if a verdict was reached and it was not SOLVED, 124 if the
# wall-clock budget fired before any verdict.
#
# SCRATCH PORT (QA-CRIER-10). The relay runs on a scratch port that is CHOSEN,
# not hard-coded: with CRIER_PORT unset the runner walks 18778..18782
# (CRIER_PORT_CANDIDATES candidates, default 5), reports every candidate it
# skips together with that candidate's holder pid/command/audit line, and uses
# the first free one. An EXPLICIT CRIER_PORT is honored literally — it is
# checked, never rotated: an occupied one aborts the run naming its holder.
# Every candidate occupied is a named failure, never a silent skip. After
# /health the sharing guard asserts the process holding the port is the pid
# started here (scripts/lib/port-guard.sh).
#
# Requires: DEEPSEEK_API_KEY in the environment (a stub base URL may override
# the live model; see README "Deterministic self-test").
#
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"

PORT_BASE=18778                       # first candidate of the default rotation
PORT_CANDIDATES="${CRIER_PORT_CANDIDATES:-5}"
PORT=""                               # selected below, never hard-coded
REPO="$(cd ../../.. && pwd)"
OUT="${DOGFOOD_OUT:-out}"
TIMEOUT_S="${DOGFOOD_TIMEOUT_S:-360}"
AGENT_WAIT_S="${DOGFOOD_AGENT_WAIT_S:-60}"
FINALS_WAIT_S="${DOGFOOD_CONTROLLER_DEADLINE_S:-300}"
BASE_URL="${DOGFOOD_BASE_URL:-https://api.deepseek.com/v1}"
MODEL="${DOGFOOD_MODEL:-deepseek-v4-flash}"
MAX_TURNS="${DOGFOOD_MAX_TURNS:-6}"

case "$OUT" in
  /*) ;;
  *) OUT="$HERE/$OUT" ;;
esac
case "$OUT" in
  ""|"/"|"$HOME"|"$REPO")
    echo "ERROR: refusing DOGFOOD_OUT='$OUT' (this script rm -rf's it)" >&2
    exit 3
    ;;
esac

if [ -z "${DEEPSEEK_API_KEY:-}" ]; then
  echo "ERROR: DEEPSEEK_API_KEY not set" >&2
  exit 1
fi
for tool in python3 curl make timeout ss; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "ERROR: '$tool' is required for a bounded run (install it and retry)" >&2
    exit 3
  }
done

# --- scratch port: rotate candidates instead of hard-coding one --------------
# A fixed scratch port made this demo skip or abort whenever anything already
# listened there: a long-lived unrelated listener that predates the run, or a
# squatter that took the port between two runs of this same script (QA-CRIER-10).
# select_scratch_port walks $PORT_BASE..+$PORT_CANDIDATES-1, prints the holder of
# every candidate it skips, and selects the first free one; an EXPLICIT
# CRIER_PORT is checked and fails closed (it is never silently rotated, because
# a run on a port the operator did not name would misreport what was measured).
# Nothing is started before the port is settled.
. "$REPO/scripts/lib/port-guard.sh"

# The crier relay this lane starts is polled on 127.0.0.1, and curl has no
# built-in loopback exemption: with an ambient HTTP_PROXY (a corporate default,
# a sandbox egress proxy, a CI image) that /health poll would be sent to the
# proxy and never reach the relay started here (QA-CRIER-21). Merge the loopback
# names into no_proxy/NO_PROXY before the first curl; a genuinely external host
# (and the model provider this lane may call) still honours the proxy.
guard_loopback_off_proxy
select_scratch_port "${CRIER_PORT:-}" "$PORT_BASE" "the llm-mesh raw-mesh-lane crier relay" \
  "$PORT_CANDIDATES" "CRIER_PORT"
PORT="$PORT_GUARD_SELECTED"

echo "==> building crier"
make -C "$REPO" build >/dev/null

rm -rf "$OUT"
mkdir -p "$OUT"

SERVER_PID=""
CONTROLLER_PID=""
AGENT_A=""
AGENT_B=""
cleanup() {
  for pid in "$CONTROLLER_PID" "$AGENT_A" "$AGENT_B" "$SERVER_PID"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

echo "==> starting crier server on :$PORT (memory backend)"
env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  HOME="$HOME" CRIER_PORT="$PORT" \
  "$REPO/bin/crier" >"$OUT/server.log" 2>&1 &
SERVER_PID=$!

# The relay that ANSWERS must be the relay we started: wait_http_or_die aborts
# (with the log tail) if this pid dies first, and assert_port_owned asks ss who
# holds :$PORT and refuses to continue when it is not $SERVER_PID (QA-CRIER-9).
wait_http_or_die "http://127.0.0.1:$PORT/health" "$SERVER_PID" "$OUT/server.log" "the crier relay"
SERVER_OWNER="$(port_holder_pid "$PORT")"
assert_port_owned "$PORT" "$SERVER_PID" "the crier relay"
echo "    :$PORT is held by pid $SERVER_OWNER == the relay pid $SERVER_PID (ss -tlnp)"

echo "==> launching controller + 2 raw-protocol LLM agents"
echo "    model=$MODEL base_url=$BASE_URL finals_wait=${FINALS_WAIT_S}s wall_clock=${TIMEOUT_S}s"
timeout --kill-after=5 --signal=TERM "$TIMEOUT_S" \
  python3 -u controller.py --task ../task-garden.json --port "$PORT" \
  --out "$OUT" --agent-wait "$AGENT_WAIT_S" --finals-wait "$FINALS_WAIT_S" \
  >"$OUT/controller.log" 2>&1 &
CONTROLLER_PID=$!
sleep 0.7
python3 -u agent.py --id agent-a --peer agent-b --task ../task-garden.json \
  --server "ws://127.0.0.1:$PORT/mesh/connect/agent-a" \
  --model "$MODEL" --base-url "$BASE_URL" --max-turns "$MAX_TURNS" \
  >"$OUT/agent-a.log" 2>&1 &
AGENT_A=$!
python3 -u agent.py --id agent-b --peer agent-a --task ../task-garden.json \
  --server "ws://127.0.0.1:$PORT/mesh/connect/agent-b" \
  --model "$MODEL" --base-url "$BASE_URL" --max-turns "$MAX_TURNS" \
  >"$OUT/agent-b.log" 2>&1 &
AGENT_B=$!

set +e
wait "$CONTROLLER_PID"
RC=$?
set -e
for pid in "$AGENT_A" "$AGENT_B"; do
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    sleep 1
    kill -KILL "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
done

VERDICT_FILE="$OUT/verdict.json"
CAUSE=""
if [ ! -f "$VERDICT_FILE" ]; then
  case "$RC" in
    124|137)
      CAUSE="wall-clock budget ${TIMEOUT_S}s exhausted before the mesh controller produced a verdict — a child stopped answering; see $OUT/controller.log" ;;
    *)
      CAUSE="mesh controller exited $RC without writing a verdict; see $OUT/controller.log" ;;
  esac
  python3 - "$VERDICT_FILE" "$RC" "$CAUSE" "$TIMEOUT_S" <<'PY'
import json, sys
path, rc, cause, budget = sys.argv[1:5]
product = {"result": "NO_VERDICT", "cause": cause, "controller_rc": int(rc),
           "wall_clock_budget_s": float(budget), "agents": {}}
with open(path, "w", encoding="utf-8") as fh:
    fh.write(json.dumps(product, sort_keys=True) + "\n")
PY
fi

RESULT="$(python3 - "$VERDICT_FILE" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        print(json.load(fh).get("result", "UNPARSEABLE"))
except Exception as exc:
    print(f"UNPARSEABLE({exc})")
PY
)"

echo
echo "================ CONTROLLER ================"
cat "$OUT/controller.log"
echo
echo "================ AGENT-A ================"
cat "$OUT/agent-a.log"
echo
echo "================ AGENT-B ================"
cat "$OUT/agent-b.log"
echo
if [ "$RC" = 124 ] || [ "$RC" = 137 ]; then
  echo "TIMEOUT: the wall-clock budget (${TIMEOUT_S}s) fired — $CAUSE"
fi
echo "logs: $OUT/"
echo "verdict: $VERDICT_FILE"
echo "PRODUCT $(cat "$VERDICT_FILE")"

if [ "$RESULT" = "SOLVED" ]; then
  if [ "$RC" = 0 ]; then
    exit 0
  fi
  echo "INCONSISTENT: verdict says SOLVED but the controller exited $RC" >&2
  exit 1
fi
case "$RC" in
  124|137) exit 124 ;;
  *) exit 1 ;;
esac
