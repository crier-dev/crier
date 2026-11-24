"""Sign a crier signed-request trio for DELETE /agents/{id} (no body) with a fresh key."""

import sys
from pathlib import Path

from cryptography.hazmat.primitives.serialization import load_pem_private_key

if len(sys.argv) != 3:
    sys.exit("usage: sign_delete.py <id> <unix-ts>")
agent_id, ts = sys.argv[1], sys.argv[2]
key = load_pem_private_key(
    Path("/tmp/dogfood-crier/alice.key").read_bytes(), password=None
)
msg = f"DELETE\n/agents/{agent_id}\n{ts}".encode()
sig = key.sign(msg).hex()
print(sig)
