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
# Requires: DEEPSEEK_API_KEY in the environment (a stub base URL may override
# the live model; see README "Deterministic self-test").
#
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"

PORT="${CRIER_PORT:-18778}"
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
for tool in python3 curl make timeout; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "ERROR: '$tool' is required for a bounded run (install it and retry)" >&2
    exit 3
  }
done

echo "==> building crier"
make -C "$REPO" build >/dev/null

if ss -tln 2>/dev/null | grep -q ":$PORT "; then
  echo "ERROR: port $PORT is busy (set CRIER_PORT to another port)" >&2
  exit 1
fi

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

HEALTHY=0
for _ in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then HEALTHY=1; break; fi
  kill -0 "$SERVER_PID" 2>/dev/null || break
  sleep 0.2
done
if [ "$HEALTHY" != 1 ]; then
  echo "ERROR: crier server never became healthy on :$PORT (see $OUT/server.log)" >&2
  exit 1
fi

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
