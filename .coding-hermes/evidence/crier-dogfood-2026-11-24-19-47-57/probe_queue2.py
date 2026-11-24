#!/usr/bin/env python3
"""Probe: list current inbox entries (ids + lease state) of dog-queue2-1124, no ack."""

import json
import sys

sys.path.insert(0, "/home/kara/crier/clients/python")
from crier_client import Crier  # noqa: E402

H = "/tmp/dogfood-crier"
S = "http://localhost:18876"
Q = "dog-queue2-1124"
queue = Crier(S, agent_id=Q, key_path=f"{H}/queue2.key")
st = queue.stats(Q)
print("STATS", json.dumps(st))
got = queue.retrieve(limit=100)
print(f"RETURNS {len(got)}")
want = set(json.load(open(f"{H}/clean_ids.json")))
mine = 0
for m in got:
    tag = "TARGET" if m.id in want else "stray"
    mine += 1 if tag == "TARGET" else 0
    print(
        f"  {tag} id={m.id} lease={'yes' if m.lease_id else 'NO'} payload={m.payload}"
    )
print(f"OF_MINE {mine} total_targets={len(want)}")
