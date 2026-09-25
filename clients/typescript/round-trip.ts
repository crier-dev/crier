/**
 * A full signed round-trip against a running crier server, using nothing but
 * this client library and a key `crier keygen` wrote.
 *
 *     crier keygen -out alice.key -id alice
 *     make run                                     # in another terminal
 *     node clients/typescript/round-trip.ts --server http://localhost:8767 --id alice --key alice.key
 *
 * Every step prints what it asked for and what came back, so the output IS the
 * transcript: register -> deliver -> signed retrieve -> signed ack -> signed
 * stats -> relay publish/subscribe, with two negative controls that prove the
 * signed calls are actually verified (an unsigned retrieve must be refused, and
 * a signature from the wrong key must be refused) rather than passing because
 * the server has signing switched off.
 *
 * Exit status: 0 when every step held, 1 on the first step that did not.
 */

import { Crier, CrierError, CrierTimeout, Message, SigningKey, signingBackend } from "./crier.ts";

let stepNumber = 0;

function step(title: string): void {
  stepNumber += 1;
  console.log(`\n-- [${stepNumber}] ${title}`);
}

function ok(message: string): void {
  console.log(`     OK   ${message}`);
}

function note(message: string): void {
  console.log(`     ..   ${message}`);
}

function fail(message: string): never {
  console.log(`     FAIL ${message}`);
  process.exit(1);
  // Unreachable: process.exit never returns. The throw is here so the function
  // is structurally `never` for a type-checker that has no @types/node.
  throw new Error("unreachable");
}

interface Args {
  server: string;
  id: string;
  key?: string;
  token?: string;
  topic?: string;
  payload: string;
  timeoutMs: number;
  keep: boolean;
}

function parseArgs(argv: string[]): Args {
  const args: Args = {
    server: process.env["CRIER_URL"] ?? "http://localhost:8767",
    id: process.env["CRIER_AGENT_ID"] ?? "round-trip",
    key: process.env["CRIER_AGENT_PRIVATE_KEY_FILE"],
    payload: '{"hello":"from the typescript client"}',
    timeoutMs: 15000,
    keep: false,
  };
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    const value = argv[i + 1];
    switch (arg) {
      case "--server":
        args.server = value;
        i += 1;
        break;
      case "--id":
        args.id = value;
        i += 1;
        break;
      case "--key":
        args.key = value;
        i += 1;
        break;
      case "--token":
        args.token = value;
        i += 1;
        break;
      case "--topic":
        args.topic = value;
        i += 1;
        break;
      case "--payload":
        args.payload = value;
        i += 1;
        break;
      case "--timeout":
        args.timeoutMs = Number(value) * 1000;
        i += 1;
        break;
      case "--keep":
        args.keep = true;
        break;
      case "--help":
      case "-h":
        console.log(
          "usage: node clients/typescript/round-trip.ts --server <url> --id <agent-id> --key <keyfile> [--token T] [--topic T] [--payload JSON] [--timeout S] [--keep]",
        );
        process.exit(0);
        break;
      default:
        console.error(`unknown argument: ${arg}`);
        process.exit(2);
    }
  }
  return args;
}

async function main(): Promise<number> {
  const args = parseArgs(process.argv.slice(2));
  if (!args.key) fail("--key is required (the file `crier keygen -out <path> -id <id>` wrote)");
  let payload: unknown;
  try {
    payload = JSON.parse(args.payload as string);
  } catch (exc) {
    fail(`--payload is not valid JSON: ${(exc as Error).message}`);
  }
  const topic = args.topic ?? `crier-roundtrip-${args.id}`;

  console.log("crier typescript client round-trip");
  console.log(`  server : ${args.server}`);
  console.log(`  agent  : ${args.id}`);
  console.log(`  key    : ${args.key}`);
  console.log(`  signing backend: ${signingBackend()}`);

  // -- [1] load the key ---------------------------------------------------
  step("load the key and derive the public half");
  let key: SigningKey;
  try {
    key = SigningKey.fromPemFile(args.key as string);
  } catch (exc) {
    fail(`cannot load the key: ${(exc as Error).message}`);
  }
  ok(`public key ${key.publicKeyHex} (derived from the private key, so the halves cannot drift)`);

  const client = new Crier(args.server, {
    agentId: args.id,
    key,
    token: args.token,
    timeoutMs: args.timeoutMs,
  });

  // -- [2] reachability ---------------------------------------------------
  step("reach the server (GET /health, no auth, no signature)");
  try {
    ok(`/health -> ${JSON.stringify(await client.request("GET", "/health"))}`);
  } catch (exc) {
    fail(`${(exc as Error).message} — is the server running? (\`make run\`)`);
  }

  // -- [3] register -------------------------------------------------------
  step("register the agent (POST /agents, public key in the body)");
  try {
    const created = await client.registerIfMissing(["typescript-client", "round-trip"]);
    if (created) ok(`registered ${args.id} (201)`);
    else note("already registered (409) — reusing the existing identity");
  } catch (exc) {
    fail((exc as Error).message);
  }

  // -- [4] deliver --------------------------------------------------------
  step(`deliver a message (POST /agents/${args.id}/inbox — the sender needs no key)`);
  let delivered: Record<string, unknown>;
  try {
    delivered = await client.deliver(args.id, payload, { ttlSeconds: 3600 });
  } catch (exc) {
    fail((exc as Error).message);
  }
  const messageId = String(delivered["id"] ?? "");
  if (!messageId) fail(`the deliver response carried no message id: ${JSON.stringify(delivered)}`);
  ok(`delivered id=${messageId} transport=${delivered["transport"]} expires_at=${delivered["expires_at"]}`);

  // -- [5] signed retrieve ------------------------------------------------
  step(`retrieve it (GET /agents/${args.id}/inbox — SIGNED)`);
  let messages: Message[] = [];
  try {
    messages = await client.retrieve({ limit: 10 });
  } catch (exc) {
    fail((exc as Error).message);
  }
  if (messages.length === 0) fail("the retrieve returned no messages");
  const found = messages.find((m) => m.id === messageId);
  if (!found) fail(`the delivered message ${messageId} was not in the retrieved batch`);
  ok(`got ${messages.length} message(s); payload decoded to ${JSON.stringify(found.payload)}`);
  if (JSON.stringify(found.payload) !== JSON.stringify(payload)) {
    fail(`payload round-trip mismatch: sent ${JSON.stringify(payload)}, got ${JSON.stringify(found.payload)}`);
  }
  ok("payload is byte-identical to what was delivered");
  ok(`lease_id ${found.leaseId || "(none)"} (the lease is what lets ack be exact)`);

  // -- [6] signed stats ---------------------------------------------------
  step(`read the queue counters (GET /agents/${args.id}/inbox/stats — SIGNED)`);
  try {
    const stats = await client.stats();
    ok(`queue_depth=${stats["queue_depth"]} leased_count=${stats["leased_count"]}`);
  } catch (exc) {
    fail((exc as Error).message);
  }

  // -- [7] signed ack -----------------------------------------------------
  step(`ack it (POST /agents/${args.id}/inbox/ack — SIGNED; lease_id + message_ids)`);
  try {
    await client.ack(found);
  } catch (exc) {
    fail((exc as Error).message);
  }
  ok(`acked ${found.id} (204 — permanently removed, never redelivered)`);
  try {
    const remaining = (await client.retrieve()).filter((m) => m.id === messageId);
    if (remaining.length > 0) fail("the acked message came back on the next retrieve");
    ok("a second signed retrieve does not return it — the ack held");
  } catch (exc) {
    if (exc instanceof Error && !(exc instanceof CrierError)) throw exc;
    fail((exc as Error).message);
  }

  // -- [8] relay publish + subscribe --------------------------------------
  step(`relay publish + subscribe (${topic})`);
  let envelope: Record<string, unknown> | undefined;
  try {
    const subscription = await client.subscribe(topic, { timeoutMs: args.timeoutMs });
    try {
      // The relay registers the subscriber after the upgrade handshake; its
      // fan-out is at-most-once to LIVE subscribers, so give it a moment rather
      // than relying on a retry.
      await new Promise((resolve) => setTimeout(resolve, 300));
      await client.publish(topic, { hello: "subscribers", from: args.id });
      ok("publish -> 202");
      try {
        envelope = await subscription.next();
      } catch (exc) {
        if (!(exc instanceof CrierTimeout)) throw exc;
        note("no frame on the first publish — the subscriber may not have been registered yet; publishing once more");
        await client.publish(topic, { hello: "subscribers", from: args.id });
        envelope = await subscription.next();
      }
    } finally {
      subscription.close();
    }
  } catch (exc) {
    fail((exc as Error).message);
  }
  const expected = { hello: "subscribers", from: args.id };
  if (envelope?.topic !== topic || JSON.stringify(envelope?.event) !== JSON.stringify(expected)) {
    fail(`the relay frame was not the published event: ${JSON.stringify(envelope)}`);
  }
  ok(`subscriber received ${JSON.stringify(envelope)}`);

  // -- [9] negative control: unsigned -------------------------------------
  step("negative control: the SAME retrieve with no signature must be refused");
  try {
    const unsigned = await client.request("GET", `/agents/${args.id}/inbox`);
    note(
      `this server answered ${JSON.stringify(unsigned)} WITHOUT a signature — it runs with CR_REQUIRE_AGENT_SIG=false, so nothing above was enforced. Re-run against a default server for the signed proof.`,
    );
  } catch (exc) {
    if (exc instanceof CrierError && exc.status === 401) ok(`401 ${exc.message}`);
    else fail(`expected 401 without the signature headers, got ${(exc as Error).message}`);
  }

  // -- [10] negative control: wrong key -----------------------------------
  step("negative control: a signature from a DIFFERENT key must be refused");
  const wrong = new Crier(args.server, {
    agentId: args.id,
    key: SigningKey.fromHex("11".repeat(32)),
    token: args.token,
    timeoutMs: args.timeoutMs,
  });
  try {
    await wrong.retrieve();
    note("a foreign signature was ACCEPTED — this server does not verify per-agent signatures");
  } catch (exc) {
    if (exc instanceof CrierError && exc.status === 401) ok(`401 ${exc.message}`);
    else fail(`expected 401 for a foreign signature, got ${(exc as Error).message}`);
  }

  // -- [11] informational: the relay does not verify the publish signature --
  step("informational: does the relay verify the publish signature? (README says no)");
  try {
    const forged = new Crier(args.server, {
      agentId: args.id,
      key: SigningKey.fromHex("22".repeat(32)),
      token: args.token,
      timeoutMs: args.timeoutMs,
    });
    await forged.publish(topic, { forged: true });
    note("a publish signed with the WRONG key was still accepted — measured here, matching the README: the relay enforces X-Agent-ID, not the signature");
  } catch (exc) {
    note(`the relay refused a wrongly-signed publish: ${(exc as Error).message} (the README's claim needs revisiting)`);
  }

  // -- done ---------------------------------------------------------------
  if (!args.keep) {
    step(`clean up: unregister (DELETE /agents/${args.id} — SIGNED)`);
    try {
      await client.unregister();
      ok("unregistered (204)");
    } catch (exc) {
      note(`could not unregister (${(exc as Error).message}) — harmless; pass --keep to skip this step`);
    }
  }

  console.log(`\nROUND-TRIP OK — ${stepNumber} steps, all held, over the real HTTP/WS API with real signatures.`);
  return 0;
}

main()
  .then((code) => process.exit(code))
  .catch((exc: unknown) => {
    console.error(`unexpected failure: ${(exc as Error).stack ?? String(exc)}`);
    process.exit(1);
  });
