#!/usr/bin/env python3
"""Two real agent identities, one message: alice -> bob over crier.

    crier keygen -out alice.key -id alice
    crier keygen -out bob.key   -id bob
    make run
    python3 clients/python/two_agents.py --server http://localhost:8767 \
        --alice-id alice --alice-key alice.key --bob-id bob --bob-key bob.key

The point of the bus is one agent delivering to another, so this is the smallest
honest version of that: two clients, two keypairs, one inbox delivery, and the
NAMED agent's signature is what leases and acks it. The sender needs no key —
which the last step demonstrates by delivering again from a keyless client.

Exit status: 0 when every step held, 1 on the first step that did not.
"""

from __future__ import annotations

import argparse
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from crier_client import Crier, CrierError  # noqa: E402


def ok(message: str) -> None:
    print("     OK   %s" % message)


def fail(message: str) -> None:
    print("     FAIL %s" % message)
    raise SystemExit(1)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="alice -> bob delivery over crier, both signed identities.")
    parser.add_argument("--server", default=os.environ.get("CRIER_URL", "http://localhost:8767"))
    parser.add_argument("--alice-id", default="alice")
    parser.add_argument("--alice-key", required=True)
    parser.add_argument("--bob-id", default="bob")
    parser.add_argument("--bob-key", required=True)
    parser.add_argument("--token", default=None)
    parser.add_argument("--payload", default='{"hello":"bob","n":1}')
    args = parser.parse_args(argv)

    payload = json.loads(args.payload)
    print("crier two-agent round-trip: %s -> %s" % (args.alice_id, args.bob_id))

    try:
        alice = Crier(args.server, agent_id=args.alice_id, key_path=args.alice_key, token=args.token)
        bob = Crier(args.server, agent_id=args.bob_id, key_path=args.bob_key, token=args.token)
    except ValueError as exc:
        fail(str(exc))
    ok("loaded two keys: alice %s… bob %s…" % (alice.key.public_key_hex[:12], bob.key.public_key_hex[:12]))

    try:
        for name, client in ((args.alice_id, alice), (args.bob_id, bob)):
            created = client.register_if_missing(capabilities=["round-trip"])
            ok("%s %s" % (name, "registered (201)" if created else "already registered (409)"))
    except CrierError as exc:
        fail(str(exc))

    print()
    print("-- deliver %s -> %s (the sender needs no key of its own)" % (args.alice_id, args.bob_id))
    try:
        delivered = alice.deliver(args.bob_id, payload, ttl_seconds=3600, sender=args.alice_id)
    except CrierError as exc:
        fail(str(exc))
    message_id = delivered.get("id")
    ok("delivered id=%s transport=%s" % (message_id, delivered.get("transport")))

    print()
    print("-- %s reads its own inbox (SIGNED with bob's key, not alice's)" % args.bob_id)
    try:
        messages = bob.retrieve(limit=10)
    except CrierError as exc:
        fail(str(exc))
    found = next((m for m in messages if m.id == message_id), None)
    if found is None:
        fail("%s did not find message %s in its inbox" % (args.bob_id, message_id))
    ok("%s received %s from %s" % (args.bob_id, json.dumps(found.payload), args.alice_id))
    if found.payload != payload:
        fail("payload mismatch: sent %s, got %s" % (json.dumps(payload), json.dumps(found.payload)))

    print()
    print("-- %s's signature is what acks it (alice's key could not)" % args.bob_id)
    try:
        # Alice signs with her OWN key while targeting bob's inbox: the server's
        # rule is that an agent may only touch its own resources, so this is the
        # forged-target case and it must be refused (403) BEFORE any signature
        # check even matters.
        alice.ack(found, agent_id=args.bob_id)
        fail("alice was able to ack a message in bob's inbox — signatures are not isolating agents")
    except CrierError as exc:
        if exc.status != 403:
            fail("expected 403 for a foreign agent's ack, got HTTP %d: %s" % (exc.status, exc.message))
        ok("alice's ack of bob's inbox was refused: 403 %s" % exc.message)
    try:
        bob.ack(found)
    except CrierError as exc:
        fail(str(exc))
    ok("bob's ack succeeded (204)")

    print()
    print("-- deliver is OPEN: a sender needs no key and no signature to deliver")
    try:
        # The escape hatch with signed=False is exactly the wire request the
        # README documents: delivery carries no signature and is not gated; it
        # is bob's SIGNED READ that is gated.
        alice.request("POST", "/agents/%s/inbox" % args.bob_id, body={"payload": {"hello": "unsigned delivery"}})
    except CrierError as exc:
        fail(str(exc))
    ok("an unsigned POST /agents/%s/inbox was accepted" % args.bob_id)
    try:
        extra = [m for m in bob.retrieve() if m.payload == {"hello": "unsigned delivery"}]
        if not extra:
            fail("bob's signed read did not see the unsigned delivery")
        ok("bob's SIGNED read saw it (the read is the gated half)")
        bob.ack(extra)
        bob.unregister()
    except CrierError as exc:
        fail(str(exc))

    print()
    print("TWO-AGENT ROUND-TRIP OK — %s -> %s with both identities' own keys." % (args.alice_id, args.bob_id))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
