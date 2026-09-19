#!/bin/sh
# s6-overlay cont-init script: register the hermes agent with crier.
# Runs once at container boot (see hermes/README.md).
#
# NOTE: POST /agents decodes the `webhook` object STRICTLY — an unknown key is a
# 400 at registration, not a silent drop (d97b777, DF-CRIER-150). Accepted keys:
# url, auth_type, auth_value_ref, schema_template, custom_schema, delivery_mode,
# batch, retries, timeout_ms. So there is no webhook-level `response_map`
# (reply extraction lives on custom_schema.response_map).
CRIER="${CRIER_URL:-http://crier:8767}"
PUB="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PAYLOAD_FILE="${CRIER_REGISTRATION_PAYLOADS:-/etc/crier/registration-payloads.json}"
PAYLOAD="$(python3 - "$PAYLOAD_FILE" "$PUB" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    body = json.load(stream)["hermes"]
body["public_key"] = sys.argv[2]
print(json.dumps(body, separators=(",", ":")))
PY
)" || {
  echo "hermes registration payload could not be rendered from $PAYLOAD_FILE" >&2
  exit 1
}
curl -s -o /dev/null -X POST "$CRIER/agents" -H 'Content-Type: application/json' \
  --data-binary "$PAYLOAD"
echo "hermes registered with crier (exit $?)"
