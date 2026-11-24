#!/usr/bin/env python3
"""Clean durability arm, v2 — one variable at each step.
produce2 : deliver 10, retrieve, ack 7, leave 3 (do NOT re-retrieve).
verify2  : after stop/start + lease expiry, the 3 unacked must be claimable.
"""

import json
import sys

sys.path.insert(0, "/home/kara/crier/clients/python")
from crier_client import Crier  # noqa: E402

H = "/tmp/dogfood-crier"
S = "http://localhost:18876"
W = "dog-worker-1124"
Q = "dog-queue2-1124"
TASKS = 10
ACKN = 7

phase = sys.argv[1]
worker = Crier(S, agent_id=W, key_path=f"{H}/worker.key")
queue = Crier(S, agent_id=Q, key_path=f"{H}/queue2.key")
worker.register_if_missing()
queue.register_if_missing()

if phase == "produce2":
    ids = []
    for i in range(TASKS):
        m = worker.deliver(Q, {"n": i, "run": "clean-1124-v2"}, sender=W)
        ids.append(m["id"])
    json.dump(ids, open(f"{H}/clean_ids_v2.json", "w"))
    got = queue.retrieve(limit=100)
    acked = 0
    for m in got:
        if m.id in ids[:ACKN]:
            queue.ack(m)
            acked += 1
    left = [i for i in ids[ACKN:]]
    st = queue.stats(Q)
    print(
        f"DELIVERED={len(ids)} ACTUALLY_ACKED={acked} TARGETS_LEFT={len(left)} unacked_now={st.get('unacked_count', st)}"
    )
    sys.exit(0 if acked == ACKN and len(ids) == TASKS else 2)

if phase == "verify2":
    got = queue.retrieve(limit=100)
    want = set(json.load(open(f"{H}/clean_ids_v2.json"))[ACKN:])
    survived = sorted(m.id for m in got if m.id in want)
    payload_ok = all(
        isinstance(m.payload, dict) and m.payload.get("run") == "clean-1124-v2"
        for m in got
        if m.id in want
    )
    print(
        f"SURVIVED_AFTER_RESTART {len(survived)}/{TASKS - ACKN} payload_ok={payload_ok}"
    )
    for m in got:
        queue.ack(m)
    st = queue.stats(Q)
    print(f"FINAL unacked={st.get('unacked_count', st)}")
    sys.exit(0 if len(survived) == TASKS - ACKN and payload_ok else 2)
