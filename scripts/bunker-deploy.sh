#!/bin/bash
# bunker-deploy.sh — build the crier image, transfer to a bunker agent,
# load into its rootless dockerd, run the container on the agent's port range.
# Usage:
#   bunker-deploy.sh [--skip-build] [--agent crier-lab] [--host 100.95.199.98]
#                    [--port 30001] [--image crier:test]
# Config matrix envs: CR_ENV_FILE=/path/to/env-file (one KEY=VALUE per line,
# '#' comments allowed) — each line becomes a docker -e flag.
set -euo pipefail

AGENT=crier-lab
HOST=100.95.199.98
PORT=30001
IMAGE=crier:test
SKIP_BUILD=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-build) SKIP_BUILD=1 ;;
    --agent) AGENT=$2; shift ;;
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

cd "$REPO"
if [[ $SKIP_BUILD -eq 0 ]]; then
  docker build -q -t "$IMAGE" .
  docker save "$IMAGE" | gzip > "$TARBALL"
fi

scp -q -i "$KEY" -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes \
  "$TARBALL" "bunker-$AGENT@$HOST:/home/bunker-$AGENT/"
bunker exec "$AGENT" -- docker load -i "/home/bunker-$AGENT/crier-image.tar.gz" >/dev/null
bunker exec "$AGENT" -- docker rm -f crier-relay >/dev/null 2>&1 || true

ENVFLAGS=()
if [[ -n "${CR_ENV_FILE:-}" && -f "$CR_ENV_FILE" ]]; then
  while IFS= read -r l; do
    [[ -z "$l" || "$l" == \#* ]] && continue
    ENVFLAGS+=(-e "$l")
  done < "$CR_ENV_FILE"
fi

bunker exec "$AGENT" -- docker run -d --name crier-relay --restart unless-stopped \
  -p "$PORT:8767" -e CRIER_PORT=8767 "${ENVFLAGS[@]}" "$IMAGE" >/dev/null
sleep 2
curl -sf "http://$HOST:$PORT/health" >/dev/null && echo "bunker-deploy: $AGENT container health OK on :$PORT"
