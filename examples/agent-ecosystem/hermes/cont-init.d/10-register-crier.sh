#!/bin/sh
# s6-overlay cont-init script: register the hermes agent with crier.
# Runs once at container boot (see hermes/README.md).
CRIER="${CRIER_URL:-http://crier:8767}"
PUB="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
curl -s -o /dev/null -X POST "$CRIER/agents" -H 'Content-Type: application/json' \
  -d "{\"id\":\"hermes\",\"public_key\":\"$PUB\",\"webhook\":{\"url\":\"http://hermes:9000/hook\",\"delivery_mode\":\"async\",\"schema_template\":\"generic\",\"response_map\":{\"reply\":\"reply\"}},\"guard\":{\"policies\":[{\"id\":\"default\"}]}}"
echo "hermes registered with crier (exit $?)"
