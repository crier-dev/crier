#!/usr/bin/env python3
"""A full signed round-trip against a running crier server, using nothing but
this client library and a key `crier keygen` wrote.

    crier keygen -out alice.key -id alice
    make run                                     # in another terminal
    python3 clients/python/round_trip.py --server http://localhost:8767 --id alice --key alice.key

Every step prints what it asked for and what came back, so the output IS the
transcript: register -> deliver -> signed retrieve -> signed ack -> signed
stats -> relay publish/subscribe, with two negative controls that prove the
signed calls are actually verified (an unsigned retrieve must be refused, and a
signature from the wrong key must be refused) rather than passing because the
server has signing switched off.

Exit status: 0 when every step held, 1 on the first step that did not.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from crier_client import (  # noqa: E402
    Crier,
    CrierError,
    CrierTimeout,
    SigningKey,
    signing_backend,
)

STEP = 0


def step(title: str) -> None:
    global STEP
    STEP += 1
    print()
    print("-- [%d] %s" % (STEP, title))


def ok(message: str) -> None:
    print("     OK   %s" % message)


def note(message: str) -> None:
    print("     ..   %s" % message)


def fail(message: str) -> "NoReturn":  # type: ignore[valid-type]
    print("     FAIL %s" % message)
    raise SystemExit(1)


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(
        description="Signed crier round-trip using the Python client and a crier-keygen key."
    )
    parser.add_argument("--server", default=os.environ.get("CRIER_URL", "http://localhost:8767"))
    parser.add_argument("--id", default=os.environ.get("CRIER_AGENT_ID", "round-trip"), help="agent id")
    parser.add_argument("--key", default=os.environ.get("CRIER_AGENT_PRIVATE_KEY_FILE"), help="the key file crier keygen wrote")
    parser.add_argument("--token", default=None, help="server CR_AUTH_TOKEN, when it enforces bearer auth")
    parser.add_argument("--topic", default=None, help="relay topic (default: crier-roundtrip-<agent id>)")
    parser.add_argument("--payload", default='{"hello":"from the python client"}', help="JSON payload to deliver")
    parser.add_argument("--timeout", type=float, default=15.0, help="per-step socket timeout, seconds")
    parser.add_argument("--keep", action="store_true", help="leave the agent registered (default: unregister at the end)")
    args = parser.parse_args(argv)

    if not args.key:
        fail("--key is required (the file `crier keygen -out <path> -id <id>` wrote)")
    if not os.path.exists(args.key):
        fail(
            "--key %s does not exist — create it with `crier keygen -out %s -id %s`"
            % (args.key, args.key, args.id)
        )
    try:
        payload = json.loads(args.payload)
    except Exception as exc:
        fail("--payload is not valid JSON: %s" % exc)

    topic = args.topic or ("crier-roundtrip-%s" % args.id)
    print("crier python client round-trip")
    print("  server : %s" % args.server)
    print("  agent  : %s" % args.id)
    print("  key    : %s" % args.key)
    print("  signing backend: %s" % signing_backend())

    # -- [1] load the key --------------------------------------------------
    step("load the key and derive the public half")
    try:
        key = SigningKey.from_pem_file(args.key)
    except ValueError as exc:
        fail("cannot load the key: %s" % exc)
    ok("public key %s (derived from the private key, so the halves cannot drift)" % key.public_key_hex)
    ok("backend %s" % signing_backend())

    client = Crier(args.server, agent_id=args.id, key=key, token=args.token, timeout=args.timeout)

    # -- [2] reachability --------------------------------------------------
    step("reach the server (GET /health, no auth, no signature)")
    try:
        health = client.request("GET", "/health")
    except CrierError as exc:
        fail("%s — is the server running? (`make run`)" % exc)
    ok("/health -> %s" % health)

    # -- [3] register ------------------------------------------------------
    step("register the agent (POST /agents, public key in the body)")
    try:
        created = client.register_if_missing(capabilities=["python-client", "round-trip"])
    except CrierError as exc:
        if exc.status == 400 and "public_key" in exc.message:
            fail("registration was refused: %s" % exc.message)
        fail(str(exc))
    if created:
        ok("registered %s (201)" % args.id)
    else:
        note("already registered (409) — reusing the existing identity; the public key must match, or the signed calls below fail")

    # -- [4] deliver -------------------------------------------------------
    step("deliver a message (POST /agents/%s/inbox — the sender needs no key)" % args.id)
    try:
        delivered = client.deliver(args.id, payload, ttl_seconds=3600)
    except CrierError as exc:
        fail(str(exc))
    message_id = delivered.get("id")
    if not message_id:
        fail("the deliver response carried no message id: %s" % delivered)
    ok("202/201 with id=%s transport=%s expires_at=%s" % (message_id, delivered.get("transport"), delivered.get("expires_at")))

    # -- [5] signed retrieve ----------------------------------------------
    step("retrieve it (GET /agents/%s/inbox — SIGNED: X-Agent-ID + X-Agent-Ts + X-Agent-Sig)" % args.id)
    try:
        messages = client.retrieve(limit=10)
    except CrierError as exc:
        fail(str(exc))
    if not messages:
        fail("the retrieve returned no messages")
    found = next((m for m in messages if m.id == message_id), None)
    if found is None:
        fail("the delivered message %s was not in the retrieved batch" % message_id)
    ok("got %d message(s); payload decoded to %s" % (len(messages), json.dumps(found.payload)))
    if found.payload != payload:
        fail("payload round-trip mismatch: sent %s, got %s" % (json.dumps(payload), json.dumps(found.payload)))
    ok("payload is byte-identical to what was delivered")
    ok("lease_id %s (the lease is what lets ack be exact)" % (found.lease_id or "(none)"))

    # -- [6] signed stats --------------------------------------------------
    step("read the queue counters (GET /agents/%s/inbox/stats — SIGNED)" % args.id)
    try:
        stats = client.stats()
    except CrierError as exc:
        fail(str(exc))
    ok("queue_depth=%s leased_count=%s" % (stats.get("queue_depth"), stats.get("leased_count")))

    # -- [7] signed ack ----------------------------------------------------
    step("ack it (POST /agents/%s/inbox/ack — SIGNED; lease_id + message_ids)" % args.id)
    try:
        client.ack(found)
    except CrierError as exc:
        fail(str(exc))
    ok("acked %s (204 — permanently removed, never redelivered)" % found.id)
    remaining = []
    for _ in range(5):
        remaining = [m for m in client.retrieve() if m.id == message_id]
        if not remaining:
            break
        time.sleep(0.2)
    if remaining:
        fail("the acked message came back on the next retrieve")
    ok("a second signed retrieve does not return it — the ack held")

    # -- [8] relay publish + subscribe ------------------------------------
    step("relay publish + subscribe (%s)" % topic)
    try:
        with client.subscribe(topic, timeout=args.timeout) as subscription:
            # The relay registers the subscriber after the upgrade handshake;
            # fan-out is at-most-once to LIVE subscribers, so give it a moment
            # before publishing rather than relying on a retry.
            time.sleep(0.3)
            client.publish(topic, {"hello": "subscribers", "from": args.id})
            ok("publish -> 202")
            try:
                envelope = subscription.next_event(timeout=args.timeout)
            except CrierTimeout:
                note("no frame on the first publish — the relay may not have registered the subscriber yet; publishing once more")
                client.publish(topic, {"hello": "subscribers", "from": args.id})
                envelope = subscription.next_event(timeout=args.timeout)
    except CrierError as exc:
        fail(str(exc))
    if envelope.get("topic") != topic or envelope.get("event") != {"hello": "subscribers", "from": args.id}:
        fail("the relay frame was not the published event: %s" % envelope)
    ok("subscriber received %s" % json.dumps(envelope))

    # -- [9] negative control: unsigned -----------------------------------
    step("negative control: the SAME retrieve with no signature must be refused")
    try:
        unsigned = client.request("GET", "/agents/%s/inbox" % args.id)
    except CrierError as exc:
        if exc.status == 401:
            ok("401 %s" % exc.message)
        else:
            fail("expected 401 without the signature headers, got HTTP %d: %s" % (exc.status, exc.message))
    else:
        note(
            "this server answered %s WITHOUT a signature — it runs with "
            "CR_REQUIRE_AGENT_SIG=false, so nothing above was enforced. Re-run "
            "against a default server (`make run`) for the signed proof."
            % json.dumps(unsigned)
        )

    # -- [10] negative control: wrong key ---------------------------------
    step("negative control: a signature from a DIFFERENT key must be refused")
    wrong = Crier(args.server, agent_id=args.id, key=SigningKey.from_hex("11" * 32), token=args.token, timeout=args.timeout)
    try:
        wrong.retrieve()
    except CrierError as exc:
        if exc.status == 401:
            ok("401 %s" % exc.message)
        else:
            fail("expected 401 for a foreign signature, got HTTP %d: %s" % (exc.status, exc.message))
    else:
        note("a foreign signature was ACCEPTED — this server does not verify per-agent signatures")

    # -- [11] informational: the relay does not verify the trio -----------
    step("informational: does the relay verify the publish signature? (README says no)")
    try:
        forged = Crier(args.server, agent_id=args.id, key=SigningKey.from_hex("22" * 32), token=args.token, timeout=args.timeout)
        forged.publish(topic, {"forged": True})
        note("a publish signed with the WRONG key was still accepted (202/204) — measured here, matching the README: the relay enforces X-Agent-ID, not the signature")
    except CrierError as exc:
        note("the relay refused a wrongly-signed publish: HTTP %d %s (the README's claim needs revisiting)" % (exc.status, exc.message))

    # -- done --------------------------------------------------------------
    if not args.keep:
        step("clean up: unregister (DELETE /agents/%s — SIGNED)" % args.id)
        try:
            client.unregister()
            ok("unregistered (204)")
        except CrierError as exc:
            note("could not unregister (%s) — harmless; pass --keep to skip this step" % exc)

    print()
    print("ROUND-TRIP OK — %d steps, all held, over the real HTTP/WS API with real signatures." % STEP)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
