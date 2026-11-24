#!/usr/bin/env python3
"""Real consumer: durable task queue on crier — produce (worker->queue),
ack as the queue owner, leave unacked, restart the server, verify survival."""

import json
import sys
import time

sys.path.insert(0, "/home/kara/crier/clients/python")
from crier_client import Crier  # noqa: E402

H = "/tmp/dogfood-crier"
S = "http://localhost:18876"
W = "dog-worker-1124"
P = "dog-queue-1124"
TASKS = 20


def lat(label, t0):
    print(f"LAT {label} {((time.perf_counter() - t0) * 1000):.2f} ms")


phase = sys.argv[1] if len(sys.argv) > 1 else "produce"

if phase == "produce":
    worker = Crier(S, agent_id=W, key_path=f"{H}/worker.key")
    queue = Crier(S, agent_id=P, key_path=f"{H}/queue.key")
    print(f"worker.register_if_missing -> {worker.register_if_missing()}")
    print(f"queue.register_if_missing  -> {queue.register_if_missing()}")
    ids = []
    for i in range(TASKS):
        t = time.perf_counter()
        m = worker.deliver(
            P,
            {"task": f"run-{phase}-{i}", "kind": "render", "ts": time.time()},
            sender=W,
        )
        lat("deliver", t)
        ids.append(m["id"])
    t = time.perf_counter()
    st = queue.stats(P)
    lat("stats_as_owner", t)
    print(f"PRODUCED {len(ids)} stats={st.get('unacked_count', st)}")
    json.dump(ids, open(f"{H}/task_ids.json", "w"))
    t = time.perf_counter()
    got = queue.retrieve(limit=100)
    lat("retrieve_batch", t)
    acked = 0
    for m in got:
        if m.id in ids[:15]:
            t = time.perf_counter()
            queue.ack(m)
            lat("ack", t)
            acked += 1
    print(f"ACKED {acked} LEAVING {TASKS - acked} UNACKED (this run)")
    sys.exit(0)

if phase == "verify":
    queue = Crier(S, agent_id=P, key_path=f"{H}/queue.key")
    print(f"queue.register_if_missing after restart -> {queue.register_if_missing()}")
    t = time.perf_counter()
    got = queue.retrieve(limit=100)
    lat("retrieve_after_restart", t)
    want = set(
        json.load(open(f"{H}/task_ids.json"))[15 : 2 * 15 - TASKS]
        if TASKS == 0
        else [json.load(open(f"{H}/task_ids.json"))[i] for i in range(15, TASKS)]
    )
    survived = sorted(m.id for m in got if m.id in want)
    print(f"SURVIVED_UNACKED {len(survived)}/{TASKS - 15}")
    for m in got:
        t = time.perf_counter()
        queue.ack(m)
        lat("ack", t)
    st = queue.stats(P)
    print(f"FINAL stats={st.get('unacked_count', st)}")
    sys.exit(0 if len(survived) == TASKS - 15 else 2)
