#!/bin/bash
# bunker-deploy.sh — build the crier image, transfer to a bunker agent,
# load into its rootless dockerd, run the container on the agent's port range.
# Usage:
#   bunker-deploy.sh [--skip-build] [--agent <agent>] [--host <host>]
#                    [--server <bunker-server>] [--port <port>] [--image crier:test]
# All targets are operator-supplied via flags or BUNKER_AGENT/BUNKER_HOST/
# BUNKER_SERVER env vars — no private infrastructure is referenced.
# The bunker CLI is $HOME/go/bin/bunker unless BUNKER_BIN names another binary
# (that override is what lets this script be exercised with no real bunker host).
# Config matrix envs: CR_ENV_FILE=/path/to/env-file (one KEY=VALUE per line,
# '#' comments allowed) — each line becomes a docker -e flag.
set -euo pipefail

AGENT="${BUNKER_AGENT:-}"
HOST="${BUNKER_HOST:-127.0.0.1}"
SERVER="${BUNKER_SERVER:-}"
PORT=8767
IMAGE=crier:test
SKIP_BUILD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-build) SKIP_BUILD=1 ;;
    --agent) AGENT=$2; shift ;;
    --server) SERVER=$2; shift ;;
    --host) HOST=$2; shift ;;
    --port) PORT=$2; shift ;;
    --image) IMAGE=$2; shift ;;
    *) echo "bunker-deploy: unknown arg $1" >&2; exit 1 ;;
  esac
  shift
done

REPO="$(cd "$(dirname "$0")/.." && pwd)"
KEY="$HOME/.bunker/keys/$AGENT"
TARBALL=/tmp/crier-image.tar.gz
BUNKER="${BUNKER_BIN:-$HOME/go/bin/bunker}"

# INT-CI-001: CI run 35302932314 lost the whole `blocking` cell to ONE transient
# reset on the transfer hop below ("Read from remote host …: Connection reset by
# peer" / "scp: Connection closed"), because this script ran scp once with no
# retry. The library retries ONLY a transient transport failure, classifies every
# other failure as NON_TRANSPORT and does NOT retry it, and leaves its verdict in
# TRANSPORT_RESULT_CLASS / _ATTEMPTS / _BUDGET / _RC / _REASON / _VERDICT so each
# fatal path below is attributable without re-running the job.
# shellcheck source=lib/transport-retry.sh
. "$REPO/scripts/lib/transport-retry.sh"

cd "$REPO"
if [[ $SKIP_BUILD -eq 0 ]]; then
  docker build -q -t "$IMAGE" .
  docker save "$IMAGE" | gzip > "$TARBALL"
fi

# ── the transfer hop (the leg CI run 35302932314 died on) ─────────────────────
if ! retry_transport "image tarball -> $AGENT" -- \
     scp -q -i "$KEY" -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes \
     "$TARBALL" "bunker-$AGENT@$HOST:/home/bunker-$AGENT/"; then
  DEPLOY_RC="${TRANSPORT_RESULT_RC:-1}"
  [[ "$DEPLOY_RC" -ne 0 ]] || DEPLOY_RC=1
  echo "bunker-deploy: FATAL: image transfer to $AGENT failed — ${TRANSPORT_RESULT_VERDICT}: ${TRANSPORT_RESULT_REASON}" >&2
  exit "$DEPLOY_RC"
fi

# ── the load hop: `docker load` is idempotent, so a transient hop here is safe
#    to retry too. The wrapper captures the command's output (relaying it when
#    the load fails), so no `>/dev/null` is needed on this line any more. ──────
if ! retry_transport "docker load on $AGENT" -- \
     "$BUNKER" exec "$AGENT" --server "$SERVER" -- docker load -i "/home/bunker-$AGENT/crier-image.tar.gz"; then
  DEPLOY_RC="${TRANSPORT_RESULT_RC:-1}"
  [[ "$DEPLOY_RC" -ne 0 ]] || DEPLOY_RC=1
  echo "bunker-deploy: FATAL: docker load on $AGENT failed — ${TRANSPORT_RESULT_VERDICT}: ${TRANSPORT_RESULT_REASON}" >&2
  exit "$DEPLOY_RC"
fi

# ── NOT wrapped, deliberately (INT-CI-001): `docker rm -f` is best-effort
#    (`|| true`) and `docker run -d` below is NOT idempotent — a retry after a
#    create that may have half-succeeded would collide on the container name
#    instead of recovering. A transient hop here is handled one level up: the
#    cell deploy leg in scripts/bunker-matrix.sh re-runs this whole script, which
#    removes the container first. ───────────────────────────────────────────────
"$BUNKER" exec "$AGENT" --server "$SERVER" -- docker rm -f crier-relay >/dev/null 2>&1 || true

ENVFLAGS=()
if [[ -n "${CR_ENV_FILE:-}" && -f "$CR_ENV_FILE" ]]; then
  while IFS= read -r l; do
    [[ -z "$l" || "$l" == \#* ]] && continue
    ENVFLAGS+=(-e "$l")
  done < "$CR_ENV_FILE"
fi

"$BUNKER" exec "$AGENT" --server "$SERVER" -- docker run -d --name crier-relay --restart unless-stopped \
  -p "$PORT:8767" -e CRIER_PORT=8767 "${ENVFLAGS[@]}" "$IMAGE" >/dev/null
sleep 2
curl -sf "http://$HOST:$PORT/health" >/dev/null && echo "bunker-deploy: $AGENT container health OK on :$PORT"
