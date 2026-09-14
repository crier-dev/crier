---
name: crier-usage
description: >-
  How to use the Crier agent-to-agent message bus (relay pub/sub, agent
  registry, durable lease-based inboxes, webhook push delivery, federation,
  mesh, MCP server) for real — entry points, run commands, the signed-request
  scheme, the ack contract, webhook delivery modes + HMAC, fed-link caveats,
  and common pitfalls. Load this when working in the crier repo or
  integrating with a running crier server.
version: 1.2.0
---

# Crier Usage — field guide for agents

Crier is a Go message bus: agents register with ed25519 identities, exchange
durable lease-based inbox messages, pub/sub over WebSocket relay, and talk P2P
over the mesh. One server binary + an MCP server front-end.

## Entry points

| What | How |
|------|-----|
| HTTP API | `./bin/crier` — 16 endpoints, `docs/openapi.yaml` |
| MCP | `make build-mcp && ./bin/crier-mcp` — stdio; run `tools/list` for the live tool inventory (the old "8 tools" figure is stale) |
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

## Federation (CR_FED_LINKS) — proven outage-safe at HEAD 2026-09-14

- `CR_FED_LINKS=http://relay-b:8767` → deliver to an agent unknown locally is
  forwarded to the link; agent tables exchange on a 60s TTL and show in
  `GET /fed/peers` with the link's agents; blocking webhook replies route
  back through the originating relay with the same `message_id` (verified).
- **Link auth (fixed since DF-CRIER-6, verified live 2026-09-14):** set the
  source relay's `CR_FED_TOKEN` equal to the destination relay's
  `CR_AUTH_TOKEN` (ONE shared secret on both sides — two different values
  was my first mistake; the sender sees `{"error":"invalid token"}` 401).
- ✅ **Link down = held, not lost** (DF-CRIER-7, verified live incl. crash
  recovery): a transient outage (unreachable link, or a retryable
  5xx/408/429) gets `202 {"status":"held","id":…,"target":…,"max_hold_s":…}`
  immediately (no blocking). The delivery is queued and retried with backoff
  inside `CR_FED_MAX_HOLD_S` (default 300s). With `CR_FED_QUEUE_FILE=<path>`
  the queue is an atomically rewritten JSON doc — held deliveries survive a
  source-relay crash and flush on restart (verified: A killed mid-hold, both
  relays restarted, message delivered exactly once). Without the file, held
  deliveries die with the process (memory queue) — same contract as the
  in-memory inbox backend.
- ✅ **Terminal outcomes are inbox entries, not drops** (verified live): on
  budget expiry or a definitive all-links-404 the sender gets exactly one
  `FEDERATION_FAILED` entry in its OWN inbox (`{kind:error, code, message_id,
  target, sender, attempts, status, error}`). ⚠️ **This only happens if the
  original deliver body carried `"sender":"<agent-id>"`** — the report is
  routed via that field; with no sender the outcome is refused with a log
  line only (DF-CRIER-129). The distinct 404-vs-budget failure reasons are
  preserved in the report's `error` field.
- `GET /fed/peers` requires the Bearer header on an auth-enabled relay (it is
  not exempt like `/health`) and lists YOUR OWN relay as a peer
  (DF-CRIER-12) — filter self before parsing.
- Operator recipe that worked (scratch ports):
  ```bash
  # relay B (destination)
  CR_AUTH_TOKEN=relayb-secret ./bin/crier -port 18872
  # relay A (source) — CR_FED_TOKEN == B's CR_AUTH_TOKEN
  CR_FED_NAME=relay-a CR_FED_LINKS=http://localhost:18872 \
  CR_FED_TOKEN=relayb-secret CR_FED_MAX_HOLD_S=60 \
  CR_FED_QUEUE_FILE=/tmp/holdq.json ./bin/crier -port 18871
  # deliver — include "sender" to enable failure reports
  curl -X POST localhost:18871/agents/eve/inbox -H 'Content-Type: application/json' \
    -d '{"payload":{"k":"v"},"sender":"workshop"}'
  ```

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
