# crier first-party clients

Registering an agent on a crier server used to mean an openssl incantation
(`openssl genpkey -algorithm ED25519`), a DER-offset recipe to extract the public
half (`openssl pkey … -pubout -outform DER | tail -c 32 | xxd -p`), and a
hand-written `sig()` shell helper to build the `X-Agent-Sig` header. None of that
is a cryptographic requirement — it was the absence of a first-party tool that
speaks the wire format, and it is what the external review named as the
number-one adoption killer and what the last three testers each tripped over
(Bharat's report, the dogfood xxd trap, `DISPATCH · CRI-001`).

There are two halves to the replacement, and neither needs openssl, xxd, or a
package manager:

| | what it does |
|---|---|
| **`crier keygen`** (the server binary, no arguments beyond flags) | writes an ed25519 keypair as PKCS#8 PEM at mode `0600` and prints the agent config: the agent id, the public key, the exact `POST /agents` body, and the one command that finishes the job |
| **`clients/python/crier_client.py`** | a stdlib-only Python client: `register` / `deliver` / `retrieve` / `ack` / `stats` / `publish` / `subscribe`, with the signature trio built for you |
| **`clients/typescript/crier.ts`** | the same surface for Node 22.6+, using only `node:crypto`, `fetch`, and `node:net`/`node:tls` |

## The whole thing, from nothing

```bash
make build                                   # or: go build -o bin/crier ./cmd/server
./bin/crier keygen -out alice.key -id alice  # <- no openssl, no xxd, no hex surgery
docker compose up -d postgres                # durable backend (README § Run)
CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' make run

python3 clients/python/round_trip.py --server http://localhost:8767 --id alice --key alice.key
# or the same round-trip in TypeScript:
node clients/typescript/round-trip.ts --server http://localhost:8767 --id alice --key alice.key
```

`round_trip.py` / `round-trip.ts` are transcripts, not tests-with-a-checkmark:
every step prints what it asked for and what came back — register → deliver →
signed retrieve → signed ack → signed stats → relay publish/subscribe — and then
two **negative controls** that are the reason to believe the rest: the same
retrieve with no signature must be refused, and the same retrieve signed with a
different key must be refused. A server that ignored signatures would pass every
positive step and fail those two.

Two identities on one bus (`clients/python/two_agents.py`) is the smallest
honest version of what crier is for: alice delivers to bob, bob's key reads and
acks it, alice's key is refused (403) when it targets bob's inbox, and an
unsigned delivery is still accepted — the signature gates the **read**, not the
write.

```bash
./bin/crier keygen -out alice.key -id alice
./bin/crier keygen -out bob.key   -id bob
python3 clients/python/two_agents.py --server http://localhost:8767 \
    --alice-id alice --alice-key alice.key --bob-id bob --bob-key bob.key
```

## Using the client in your own code

Python:

```python
from crier_client import Crier            # stdlib only; add clients/python to sys.path

c = Crier("http://localhost:8767", agent_id="alice", key_path="alice.key")
c.register()                              # POST /agents with the key's public half
c.deliver("bob", {"hello": "world"})      # anyone may deliver; the sender needs no key
for message in c.retrieve():              # signed retrieve
    print(message.payload)                # already decoded from base64
    c.ack(message)                        # signed ack (lease_id + message_ids)
c.publish("demo-topic", {"tick": 1})      # relay publish
sub = c.subscribe("demo-topic")           # relay subscribe (WebSocket, opened here)
print(next(sub))                          # {"topic": "demo-topic", "event": {...}}
sub.close()
```

TypeScript / Node 22.6+:

```ts
import { Crier } from "./crier.ts";

const c = new Crier("http://localhost:8767", { agentId: "alice", keyPath: "alice.key" });
await c.register();
await c.deliver("bob", { hello: "world" });
for (const message of await c.retrieve()) {
  console.log(message.payload);
  await c.ack(message);
}
await c.publish("demo-topic", { tick: 1 });
const sub = await c.subscribe("demo-topic");
console.log(await sub.next());
sub.close();
```

Both clients also read the same environment the MCP bridge does, so an
orchestrator can hand a process its identity and nothing else:

```python
c = Crier.from_env()   # CRIER_URL (or CRIER_HTTP_URL), CRIER_AGENT_ID,
                       # CRIER_AGENT_PRIVATE_KEY_FILE, CR_AUTH_TOKEN
```

## What the clients do on the wire

Nothing is hidden, because a client that hides the protocol cannot be debugged
against a server:

*   Agent-scoped routes (`retrieve` / `ack` / `stats` / `DELETE /agents/{id}`)
    carry the signature trio when the server runs with `CR_REQUIRE_AGENT_SIG=true`
    (the default):

    ```
    X-Agent-ID   the agent id
    X-Agent-Ts   unix seconds, within ±30s of the SERVER clock
    X-Agent-Sig  hex ed25519 signature over "<METHOD>\n<path>\n<ts>"
    ```

    The path is the URL path only — the query string is **not** covered, so
    `GET /agents/alice/inbox?limit=5` is signed over `/agents/alice/inbox`.
*   `POST /agents` (registration) and `POST /agents/{id}/inbox` (delivery) are
    **not** per-agent signed: registration carries the public key in the body and
    delivery is open to any sender.
*   `POST /relay/publish` needs `X-Agent-ID` (the relay's rate limiter keys on
    it); the relay does not verify the signature there. `GET
    /relay/subscribe/{topic}` is a WebSocket whose frames are the envelopes
    `{"topic": …, "event": …}`.
*   If the server runs with `CR_AUTH_TOKEN`, every request except the five exempt
    paths (`/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs`) needs
    `Authorization: Bearer <token>` — including the WebSocket upgrade, which is
    why the TypeScript client speaks RFC 6455 over `node:net` instead of using
    Node's global `WebSocket` (the WHATWG API cannot set request headers).

## Signing backends, and what is proven

The Python client prefers the `cryptography` package when it is installed and
otherwise uses a bundled RFC 8032 implementation, so it has **no required
dependency**. ed25519 signatures are deterministic, so the two backends produce
byte-identical signatures for the same key and message, and both are checked
against the RFC 8032 §7.1 test vectors — plus, live, by the Go server accepting
them. Set `CRIER_CLIENT_FORCE_PURE_PYTHON=1` to pin the bundled implementation
(it is slower and not constant-time — fine for test and dev traffic, which is why
the library-backed path is preferred when present).

The TypeScript client signs with `node:crypto` (OpenSSL under the hood) and needs
no packages at all.

Run the unit suites:

```bash
python3 -m unittest discover -s clients/python                       # 21 tests
CRIER_CLIENT_FORCE_PURE_PYTHON=1 python3 -m unittest discover -s clients/python
node --test clients/typescript/crier.test.ts                         # 13 tests
```

Set `CRIER_BIN=<path to a built crier>` so the suites generate their fixtures with
`crier keygen` (the documented path); without it they fall back to `openssl
genpkey`. Two tests in each suite additionally CROSS-CHECK that a generated key is
the same key openssl's DER recipe derives — those skip loudly when openssl is not
on PATH, because the client does not need it.

Run the whole acceptance drive — keys, both clients, both backends, the two
negative controls, the two-agent exchange and the bearer-token arm — against a
server it starts itself:

```bash
make client-roundtrip-check
```

## Requirements and known limits

*   **Python**: 3.8+ (f-strings, `typing`), stdlib only. No `pip install`.
*   **TypeScript**: Node **22.6+** to run the `.ts` files directly (native type
    stripping), e.g. `node clients/typescript/round-trip.ts`; `tsx` also works.
    No `package.json`, no `node_modules`, no build step. Imports carry the `.ts`
    extension (what Node's resolver needs), so `tsc --noEmit` wants
    `allowImportingTsExtensions` — and editor types want `@types/node`. Neither
    is needed to run anything.
*   **Keys**: the private key file must be the PKCS#8 PEM that `crier keygen`
    writes, which is exactly what `openssl genpkey -algorithm ED25519` writes —
    so an existing key keeps working and openssl can still read the new one.
    Both clients also accept a 32-byte hex seed (`SigningKey.from_hex` /
    `SigningKey.fromHex`).
*   **Not covered by the clients**: message-guard policy, webhooks, federation,
    the mesh, agent `PATCH`, and the LLM-message-guard config. The `request()`
    escape hatch on both clients takes any route with this client's auth and
    signing applied; everything else is the server's own OpenAPI surface
    (`GET /docs` on a running server).
*   **`subscribe` needs a topic that routes as one path segment** (the server's
    route is `/relay/subscribe/{topic}`). Measured on a live server: subscribing
    to `multi/segment` is refused with `404` (both raw and percent-encoded), while
    a single-segment topic works. Wildcard *patterns* (`*`, `>`) are accepted by
    the route and validated by the server; a multi-segment literal is publishable
    (`202`) but not subscribable through this endpoint. The raw WebSocket client
    has the same limit — it is the route, not the client.
