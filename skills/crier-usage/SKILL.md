---
name: crier-usage
description: >-
  How to use the Crier agent-to-agent message bus (relay pub/sub, agent
  registry, durable lease-based inboxes, webhook push delivery, federation,
  mesh, MCP server) for real — entry points, run commands, the signed-request
  scheme, the ack contract, webhook delivery modes + HMAC, fed-link caveats,
  and common pitfalls. Load this when working in the crier repo or
  integrating with a running crier server.
version: 1.1.0
---

# Crier Usage — field guide for agents

Crier is a Go message bus: agents register with ed25519 identities, exchange
durable lease-based inbox messages, pub/sub over WebSocket relay, and talk P2P
over the mesh. One server binary + an MCP server front-end.

## Entry points

| What | How |
|------|-----|
| HTTP API | `./bin/crier` — 14 endpoints, `docs/openapi.yaml` |
| MCP | `make build-mcp && ./bin/crier-mcp` — stdio, 8 tools |
| Demo | `./examples/demo.sh` (needs `CR_AUTH_TOKEN` exported if auth is on) |
| Specs | `docs/specs.md`, `docs/architecture.md`, `specs/ci-003b-postgresql-persistence.md` |
| Board | `.coding-hermes/board/tasks.jsonl` (JSONL v2.1 — append rows, commit) |

## Run commands

```bash
make build                      # -> bin/crier (0.6s)
CR_AUTH_TOKEN=secret ./bin/crier                # auth ON (documented default posture)
CR_REQUIRE_AGENT_SIG=false ./bin/crier          # dev escape hatch (skip signing)
CR_DATABASE_URL=postgres://... ./bin/crier      # durable registry+inboxes
make test / make test-short / make lint
```

## The signed-request scheme (inbox endpoints + DELETE /agents/{id})

Headers: `X-Agent-ID`, `X-Agent-Ts` (unix seconds, ±30s of server clock),
`X-Agent-Sig` = hex(ed25519_sign(`"METHOD\n/path\nunix-seconds"`, privkey)).
Method+path are bound into the signature. No sig → 401. Wrong key → 401.

## ⚠️ The ack gotcha (CR-GAP-014 — check if still open)

`POST /agents/{id}/inbox/ack` with only `{"lease_id": L}` returns **204 but
acks NOTHING** (server bug: lease only validated when `message_ids` present).
Always send `{"lease_id": L, "message_ids": [ids from the retrieve response]}`.
Verify with `GET .../inbox/stats` (`queue_depth: 0`), never with a re-retrieve
(leased messages are hidden from retrieves until lease expiry + purge tick).

## Mesh (raw WS) essentials

- Endpoint: `ws://host:8767/mesh/connect/{agentID}`; frames are newline-terminated JSON.
- Timestamps must be RFC3339 strings (`2026-08-09T12:00:00Z`), not floats —
  a float makes the server silently drop the frame.
- `target`/`source` are objects: `{"agent_id": "beta"}`.
- Response routing: the RESPONSE's `request_id` MUST equal the REQUEST's
  `message_id`, or the server drops it and the requester times out.
- The server never sends REGISTER_ACK and ignores KEEPALIVE (claims in
  README/specs are overstated — see CR-GAP-016).

## Webhook push delivery (verified 2026-09-08)

Register an agent with a `webhook` object (or add one later via **signed**
`PATCH /agents/{id}` — sign `"PATCH\n/agents/{id}\n<ts>"`, same scheme as
DELETE; a PATCH without the sig headers → 401):

```json
{"id":"wb-agent","public_key":"<64hex>","capabilities":["echo"],
 "webhook":{"url":"http://127.0.0.1:9101/hooks/wb","schema_template":"generic-custom",
            "delivery_mode":"blocking","timeout_ms":5000,"retries":2}}
```

- **blocking** — deliver call waits; receiver's 2xx body becomes `"reply"` in
  the deliver response. Extract via schema `response_map`; `openai-compatible`
  expects `$.choices[0].message.content` (map `payload.text` → the prompt).
  A wrong reply body → 504 with the exact missing-key path (good error).
- **async/batch** — 202 immediately; batch coalesces N deliveries into ONE
  POST (`X-Crier-Event: batch`, body `{"messages":[envelopes]}`).
- **HMAC**: set `CR_WEBHOOK_SECRET`; every outbound POST carries
  `X-Crier-Signature = hex(hmac_sha256(secret, raw_body))` plus
  `X-Crier-Event/Agent/Session/Retry` headers. Verify against the raw bytes.
- Envelope body: `crier.{version,message_id,session_id,thread_id,
  delivery_mode,sender,kind,guard}` + `payload`. NOTE: `sender` is a plain
  STRING on the wire (spec §3 draws an object — drift, DF-CRIER-11).
- ⚠️ **Failure paths are lossy** (as of 2026-09-08): async retries exhaust →
  silent drop, no ERROR to sender (DF-CRIER-8); per-agent `retries` is
  ignored (server default wins, DF-CRIER-9). For must-not-lose messages, use
  inbox pull or wrap async sends with your own correlation+timeout.

## Federation (CR_FED_LINKS) — LAN-only plumbing for now

- `CR_FED_LINKS=http://relay-b:8767` → deliver to an agent unknown locally is
  forwarded to the link; agent tables exchange on a 60s TTL and show in
  `GET /fed/peers` with the link's agents; blocking webhook replies route
  back through the originating relay with the same `message_id` (verified).
- ⚠️ **Links carry NO credentials unless `CR_FED_TOKEN` is set** (DF-CRIER-6): with
  no token the forward is a bare POST, so an auth-enabled (`CR_AUTH_TOKEN`)
  remote relay 401s every federated delivery and the sender sees that 401
  verbatim. Set the source relay's `CR_FED_TOKEN` equal to the destination's
  `CR_AUTH_TOKEN` to federate with an auth-enabled relay.
- ✅ **Link down = held, not lost** (DF-CRIER-7): a transient outage
  (unreachable link, or a retryable 5xx/408/429) gets `202
  {"status":"held","id":…,"target":…,"max_hold_s":…}` — the delivery is queued
  at the source and retried inside `CR_FED_MAX_HOLD_S` (default 300s). If it
  still cannot be delivered the sender gets exactly one durable
  `FEDERATION_FAILED` entry in its own inbox (`{kind:error, code, message_id,
  target, sender, request_id, session_id, attempts, status, error}`). A
  definitive all-links-404 still answers `404` immediately. Durability: set
  `CR_FED_QUEUE_FILE` or held deliveries die with the process (memory queue) —
  the same contract as the in-memory inbox backend.
- `GET /fed/peers` lists YOUR OWN relay as a peer (DF-CRIER-12) — filter self
  before parsing.

## Common pitfalls

1. ~~Ack without `message_ids` → false success~~ **FIXED (verified 2026-09-08):** lease-only ack now → 400. Still always ack with `lease_id` + the `message_ids` from the retrieve, verify via `/inbox/stats`.
2. Clock skew > 30s → 401 on signed requests.
3. No `CR_DATABASE_URL` → all state lost on restart (documented, but easy to miss).
4. `GET /relay/topics` shows only topics with live subscribers.
5. Register endpoint is `POST /agents` (id in body), NOT `POST /agents/{id}` (405).
6. `./bin/crier-mcp --help` starts the server (no flags — CR-GAP-017).
7. Python: use websockets v17 `additional_headers` for the Bearer header.

## Verified-good examples (2026-08-09 live run)

- Full signed round-trip: `examples/demo.sh` (0.1s) — but see gotcha #1; the
  correct ack adds `message_ids`.
- Relay: subscribe WS → `POST /relay/publish` → event arrives on the socket.
- MCP: `mcp_client` with stdio — register/deliver/retrieve/ack all work;
  shares state with the HTTP server when both use the same Postgres.
- Durability: agent + undelivered message survive server restart with
  `CR_DATABASE_URL` (verified against a scratch postgres container).

See `docs/dogfood/2026-08-09-integration.md` and
`docs/dogfood/2026-09-08-integration.md` (webhook + federation leg) for the
full integration reports and `docs/dogfood/diagnostics.md` for the
build/behavior trail.
