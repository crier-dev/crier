#!/usr/bin/env bash
#
# Crier LLM-mesh demo (bridge lane): two LLM harnesses solve a puzzle
# together, communicating ONLY through crier-mcp bridge tools.
#
#   harness-a ──MCP stdio──> crier-mcp (agent-a) ─┐
#                                                  ├─> crier server (inboxes + mesh)
#   harness-b ──MCP stdio──> crier-mcp (agent-b) ─┘
#   controller ─MCP stdio──> crier-mcp (controller)
#
# The harnesses never see the transport: no WebSockets, no HTTP registry
# calls, no signing, no leases. All of that lives in the bridge.
#
# BOUNDED AND NAMED (DF-CRIER-214). The run always ends with an outcome a
# caller can act on, and never hangs:
#
#   PRODUCT <json>   one line, the controller's verification of both agents
#                    against task-garden ground truth (also written to
#                    $OUT/verdict.json). "SOLVED" can only come from there.
#   exit 0           both agents' submissions matched the ground truth
#   exit 1           a verdict was reached and it was not SOLVED, or a child
#                    died with a named cause
#   exit 2           a harness ran out of turns (see $OUT/agent-*.log)
#   exit 124         the wall-clock budget (DOGFOOD_TIMEOUT_S) fired before a
#                    verdict — a wedged process, not a slow model
#
# Requires: DEEPSEEK_API_KEY in the environment (a stub base URL may override
# the live model; see README "Deterministic self-test").
#
set -euo pipefail
cd "$(dirname "$0")"
HERE="$(pwd)"

PORT="${CRIER_PORT:-18777}"
REPO="$(cd ../.. && pwd)"
OUT="${DOGFOOD_OUT:-out}"
BRIDGE="$REPO/bin/crier-mcp"

# Bounds. The controller owns the verdict; the outer wall clock is the hard
# stop for a process that would otherwise never answer (its budget defaults
# above the controller's on purpose).
TIMEOUT_S="${DOGFOOD_TIMEOUT_S:-180}"
CTRL_DEADLINE_S="${DOGFOOD_CONTROLLER_DEADLINE_S:-150}"
MESH_WAIT_S="${DOGFOOD_MESH_WAIT_S:-20}"
MAX_TURNS="${DOGFOOD_MAX_TURNS:-8}"
ASK_TIMEOUT_S="${DOGFOOD_ASK_TIMEOUT_S:-60}"
BASE_URL="${DOGFOOD_BASE_URL:-https://api.deepseek.com/v1}"
MODEL="${DOGFOOD_MODEL:-deepseek-v4-flash}"

# --- environment ------------------------------------------------------------

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

echo "==> building crier + crier-mcp"
make -C "$REPO" build build-mcp >/dev/null

if ss -tln 2>/dev/null | grep -q ":$PORT "; then
  echo "ERROR: port $PORT is busy (set CRIER_PORT to another port)" >&2
  exit 1
fi

rm -rf "$OUT"
mkdir -p "$OUT"

SERVER_PID=""
CONTROLLER_PID=""
HARNESS_A=""
HARNESS_B=""
cleanup() {
  for pid in "$CONTROLLER_PID" "$HARNESS_A" "$HARNESS_B" "$SERVER_PID"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  pkill -f "$BRIDGE" 2>/dev/null || true
}
trap cleanup EXIT

# --- server -----------------------------------------------------------------

echo "==> starting crier server on :$PORT (memory backend, signing disabled)"
env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  HOME="$HOME" CRIER_PORT="$PORT" CR_REQUIRE_AGENT_SIG=false \
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

# --- children ---------------------------------------------------------------

echo "==> launching controller + 2 LLM harnesses (each spawns its own bridge)"
echo "    model=$MODEL base_url=$BASE_URL controller_deadline=${CTRL_DEADLINE_S}s mesh_wait=${MESH_WAIT_S}s wall_clock=${TIMEOUT_S}s"
timeout --kill-after=5 --signal=TERM "$TIMEOUT_S" \
  python3 -u controller.py --task task-garden.json --bridge "$BRIDGE" \
  --port "$PORT" --out "$OUT" --deadline "$CTRL_DEADLINE_S" --mesh-wait "$MESH_WAIT_S" \
  >"$OUT/controller.log" 2>&1 &
CONTROLLER_PID=$!
sleep 0.7
python3 -u harness.py --id agent-a --peer agent-b --task task-garden.json --bridge "$BRIDGE" \
  --port "$PORT" --out "$OUT" --model "$MODEL" --base-url "$BASE_URL" \
  --max-turns "$MAX_TURNS" --ask-timeout "$ASK_TIMEOUT_S" >"$OUT/agent-a.log" 2>&1 &
HARNESS_A=$!
python3 -u harness.py --id agent-b --peer agent-a --task task-garden.json --bridge "$BRIDGE" \
  --port "$PORT" --out "$OUT" --model "$MODEL" --base-url "$BASE_URL" \
  --max-turns "$MAX_TURNS" --ask-timeout "$ASK_TIMEOUT_S" >"$OUT/agent-b.log" 2>&1 &
HARNESS_B=$!

set +e
wait "$CONTROLLER_PID"
RC=$?
set -e

# Name what each harness did: exited on its own (with its status) or was still
# running when the controller finished (then it is stopped here).
HARNESS_NOTES=""
HARNESS_BUDGET=0
for pair in "agent-a:$HARNESS_A" "agent-b:$HARNESS_B"; do
  label="${pair%%:*}"; pid="${pair##*:}"
  note=""
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    sleep 1
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    note="$label was still running when the controller finished (stopped)"
  else
    hrc=0
    wait "$pid" 2>/dev/null || hrc=$?
    case "$hrc" in
      0) note="$label exited 0 (its final answer was accepted)" ;;
      2) note="$label exited 2 — turn budget exhausted without an accepted answer"
         HARNESS_BUDGET=1 ;;
      *) note="$label exited $hrc" ;;
    esac
  fi
  HARNESS_NOTES="${HARNESS_NOTES}    ${note}"$'\n'
done
HARNESS_NOTES="${HARNESS_NOTES%$'\n'}"

# --- product ----------------------------------------------------------------

VERDICT_FILE="$OUT/verdict.json"
CAUSE=""
if [ ! -f "$VERDICT_FILE" ]; then
  case "$RC" in
    124|137)
      CAUSE="wall-clock budget ${TIMEOUT_S}s exhausted before the controller produced a verdict — a child stopped answering (wedged bridge or model endpoint); see $OUT/controller.log and $OUT/*-bridge.log" ;;
    *)
      CAUSE="controller exited $RC without writing a verdict; see $OUT/controller.log" ;;
  esac
  # A run that produced no verdict never claims success: this file can only
  # carry a failure result.
  python3 - "$VERDICT_FILE" "$RC" "$CAUSE" "$TIMEOUT_S" <<'PY'
import json, sys
path, rc, cause, budget = sys.argv[1:5]
product = {"result": "NO_VERDICT", "cause": cause, "controller_rc": int(rc),
           "wall_clock_budget_s": float(budget), "agents": {}, "mesh": {}}
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
FINALS="$(python3 - "$VERDICT_FILE" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as fh:
        print(json.load(fh).get("finals_received", 0))
except Exception:
    print(0)
PY
)"

# --- report -----------------------------------------------------------------

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
echo "harness outcomes:"
printf '%s\n' "$HARNESS_NOTES"
echo "logs: $OUT/ (controller.log, agent-a.log, agent-b.log, *-bridge.log, server.log, verdict.json)"
echo "verdict: $VERDICT_FILE"
echo "PRODUCT $(cat "$VERDICT_FILE")"

# --- exit status ------------------------------------------------------------
# 0 = verified SOLVED; 124 = the wall-clock bound fired before a verdict;
# 2 = no submission at all and a harness ran out of turns (the model never
# finished the puzzle); 1 = a verdict was reached and it was not SOLVED.

if [ "$RESULT" = "SOLVED" ]; then
  if [ "$RC" = 0 ]; then
    exit 0
  fi
  echo "INCONSISTENT: verdict says SOLVED but the controller exited $RC" >&2
  exit 1
fi
case "$RC" in
  124|137) exit 124 ;;
esac
if [ "$HARNESS_BUDGET" = 1 ] && [ "$FINALS" = 0 ]; then
  exit 2
fi
exit 1
