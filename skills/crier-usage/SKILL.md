---
name: crier-usage
description: >-
  How to use the Crier agent-to-agent message bus (relay pub/sub, agent
  registry, durable lease-based inboxes, mesh, MCP server) for real — entry
  points, run commands, the signed-request scheme, the ack contract gotcha,
  and common pitfalls. Load this when working in the crier repo or integrating
  with a running crier server.
version: 1.0.0
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

## Common pitfalls

1. Ack without `message_ids` → false success, message redelivered later.
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

See `docs/dogfood/2026-08-09-integration.md` for the full integration report
and `docs/dogfood/diagnostics.md` for the build/behavior trail.
