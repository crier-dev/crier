# Crier Integration Report — 2026-08-09 (dogfood run)

How to use Crier for real, from a fresh consumer's perspective. Everything in
this report was live-verified against a running server on 2026-08-09 (auth +
signing enabled: `CR_AUTH_TOKEN` set, `CR_REQUIRE_AGENT_SIG=true`).

## The 60-second version

```bash
make build                          # 0.6s
CR_AUTH_TOKEN=secret CRIER_PORT=8767 ./bin/crier   # start (auth on)
CR_AUTH_TOKEN=secret ./examples/demo.sh            # full round-trip
```

The demo takes 0.1s and does: health → ed25519 keygen → register → deliver →
signed retrieve → ack → verify-empty.

## ⚠️ Read this first: the ack contract (bug CR-GAP-014)

The demo/README ack step sends only `{"lease_id": "..."}`. The server answers
**204 but does nothing** — `message_ids` is what actually removes messages.
A lease-only ack is a silent no-op: your message stays queued and is
**redelivered after ~30–60s**. The demo's "verify empty" passes only because
the message is hidden behind its active lease.

**The correct ack** (matching `docs/openapi.yaml`, which requires
`message_ids`):

```bash
# retrieve gives you messages[] with ids + a lease_id
curl -s -X POST localhost:8767/agents/$AGENT/inbox/ack -H "Authorization: Bearer $TOKEN" \
  -H "X-Agent-ID: $AGENT" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG" \
  -H 'Content-Type: application/json' \
  -d "{\"lease_id\":\"$LEASE\",\"message_ids\":[\"$MSG_ID\"]}"   # -> 204, really acked
```

Verify with `GET .../inbox/stats` → `queue_depth: 0` (not with a re-retrieve,
which hides leased messages).

## Recipes that work

### 1. Signed inbox round-trip (the correct way)

1. `openssl genpkey -algorithm ED25519 -out agent.key`
2. Public key: `openssl pkey -in agent.key -pubout -outform DER | tail -c 32 | xxd -p -c 64`
3. Register: `POST /agents` `{"id","public_key","capabilities"}` → 201
4. Deliver: `POST /agents/{id}/inbox` `{"payload":{...}}` → 201 `{"id"}`
5. Retrieve: `GET /agents/{id}/inbox` with signature headers → 200
   `{"messages":[{id,payload(base64),lease_id,...}],"lease_id"}`
6. Ack: `POST /agents/{id}/inbox/ack` with `lease_id` **and** `message_ids` → 204

Signature scheme (X-Agent-ID / X-Agent-Ts / X-Agent-Sig):
`X-Agent-Sig = hex(ed25519_sign("METHOD\n/path\nunix-seconds", privkey))`,
timestamp within ±30s of server clock. Method and path are bound into the
signature (a sig over the wrong method → 401). The signature gate also applies
to `DELETE /agents/{id}` (undocumented — see CR-GAP-015) and to
`GET .../inbox/stats`.

### 2. Relay pub/sub over WebSocket

```python
# pip install websockets
import asyncio, json, websockets
async def main():
    async with websockets.connect("ws://localhost:8767/relay/subscribe/my-topic",
            additional_headers={"Authorization": "Bearer TOKEN"}) as ws:
        # publish from another process:
        # POST /relay/publish {"topic":"my-topic","event":{...}} -> 202
        print(await asyncio.wait_for(ws.recv(), 5))   # the event
asyncio.run(main())
```

Note: `GET /relay/topics` only lists topics with live subscribers.

### 3. Mesh request/response (raw WS — the wire format is undocumented, see CR-GAP-016)

Connect two agents: `ws://host:8767/mesh/connect/{agentID}`. Frames are
newline-terminated JSON with RFC3339 timestamps:

```json
{"type":"REQUEST","version":1,"message_id":"q1","timestamp":"2026-08-09T12:00:00Z",
 "source":{"agent_id":"alpha"},"target":{"agent_id":"beta"},
 "method":"GET","path":"/intel","body":null,"trace_id":"t1","timeout_ms":5000}
```

The responder must reply with `request_id` **equal to the request's
`message_id`** — otherwise the server drops the response silently and the
requester times out. `{"type":"RESPONSE","version":1,"message_id":"r1",
"timestamp":"...","request_id":"q1","status_code":200,
"source":{"agent_id":"beta"},"body":{...}}`

### 4. MCP server

```bash
make build-mcp
./bin/crier-mcp          # stdio transport; 8 tools: register/list/get/unregister
                         # agent, deliver/retrieve/ack, inbox_stats
```

Point it at the same Postgres as the HTTP server and both interfaces share
state (verified: agent registered via HTTP is visible via MCP). There is no
`--help` (see CR-GAP-017). Env: `CR_DATABASE_URL` (else in-memory).

### 5. Durable setup (PostgreSQL)

```bash
docker compose up -d postgres    # project's own container, port 5437
CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' ./bin/crier
```

Migrations apply automatically on start. Verified: agents and undelivered
messages survive a full server restart. Without `CR_DATABASE_URL` everything
is process-lifetime only (documented).

## Friction log (what a new user hits)

1. **Ack no-op** (CR-GAP-014) — the documented path gives false success.
2. **DELETE needs a signature** (CR-GAP-015) — 401 with Bearer-only, no doc.
3. **Mesh wire format** (CR-GAP-016) — RFC3339 timestamps (a float timestamp
   makes the server silently drop the frame), PeerRef object shapes,
   message_id==request_id contract; none documented.
4. **crier-mcp ignores --help** (CR-GAP-017) — starts the server instead.
5. Python websockets v17 uses `additional_headers`, not `extra_headers` (client-side, not Crier's fault).
6. `CR_AUTH_TOKEN` unset in quickstart produces an empty `Authorization:` header — harmless (auth disabled server-side), just noisy.

## Verdict

🟡 **PROMISING-BUT-ROUGH** — the core (registry, inbox, relay, MCP, durability)
is real and works; the ack contract bug and undocumented mesh protocol are the
blockers to calling it shippable.
