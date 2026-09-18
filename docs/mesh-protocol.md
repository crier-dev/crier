# Crier Mesh Protocol — Wire Format

The mesh is Crier's peer-to-peer agent communication layer. This document is the
authoritative reference for the wire protocol used by `GET /mesh/connect/{agentID}`.
All claims below are verified against `internal/mesh` (live-probed 2026-08-10).

## Transport

- WebSocket upgrade to `GET /mesh/connect/{agentID}` — the `agentID` in the path is
  the connecting agent's identity. No bearer token or signature is required on this
  endpoint (unlike the registry API).
- The server accepts connections from any origin (the upgrader's origin check
  defaults to allow-all; `mesh.SetWSCheckOrigin` can restrict it).
- Connected peers are listed via `GET /mesh/peers` → `{"peers":[{"agent_id":"..."}],"count":N}`.
- Peer identity is asserted by the URL path only. There is no cryptographic
  authentication on mesh frames — do not use the mesh for privileged operations
  without an additional application-level auth layer.

## Framing

Every message is a single JSON object sent as one WebSocket **text frame**, with a
trailing newline appended (`json.Marshal` + `\n`, `internal/mesh/message.go:Marshal`).
This is newline-delimited JSON (NDJSON): a reader splits the stream on `\n` and
parses each line independently. There is no length prefix, no binary framing, and no
multi-frame messages.

## Envelope

Every message embeds an envelope with four fields:

| Field | Type | Notes |
|-------|------|-------|
| `type` | string | One of `REGISTER`, `REGISTER_ACK`, `KEEPALIVE`, `REQUEST`, `RESPONSE`, `ERROR` |
| `version` | int | Protocol version, currently `1` |
| `message_id` | string | Unique ID (24 hex chars, crypto/rand) — see correlation contract |
| `timestamp` | string | RFC3339 with nanosecond precision, e.g. `2026-08-10T01:55:00.123456789-05:00` (Go `time.Time` JSON encoding) |

## Message types

### REGISTER (client → server)

Sent once immediately after the WebSocket upgrade. **Fire-and-forget**: the client
does not wait for a reply, and the server never sends one (see "Not implemented").

```json
{"type":"REGISTER","version":1,"message_id":"0e64c4d94d584d3fb9d09fcf",
 "timestamp":"2026-08-10T01:55:00.123456789-05:00","agent_id":"agent-a",
 "lease_id":"","lease_ttl_ms":3600000,
 "capabilities":{"version":"0.1.0","topics":[],"max_concurrent_sessions":10}}
```

| Field | Type | Notes |
|-------|------|-------|
| `agent_id` | string | Agent identity (should match the URL path) |
| `lease_id` | string | Registry lease; empty on mesh connect |
| `lease_ttl_ms` | int | Requested lease TTL |
| `capabilities` | object | `version`, `topics[]`, `max_concurrent_sessions` |

### REGISTER_ACK (defined, never sent)

The `REGISTER_ACK` **type exists in the codebase** (`internal/mesh/message.go`) with
`expires_at` and `keepalive_interval_ms` fields, but **no server code path ever
emits it**. Treat any received `REGISTER_ACK` as a protocol extension, not part of
the contract.

### KEEPALIVE (both directions)

Sent by each connected client every 30 seconds (`KeepaliveInterval`,
`internal/mesh/peer.go:keepaliveLoop`). The server **does not process it** — there is
no liveness bookkeeping and no reply: an inbound KEEPALIVE reaches the `default`
branch of the message switch (`internal/mesh/handleMessage`,
`internal/mesh/peer.go:267`). The loop keeps the socket warm and detects dead
connections via read errors; it has no other effect.

The server sends a KEEPALIVE to every accepted peer every 30 seconds: the accept
path starts the same loop (`internal/mesh/handler.go:46` →
`internal/mesh/peer.go:186`), on the same `KeepaliveInterval`
(`internal/mesh/peer.go:42`) the client loop ticks on. The frame's `agent_id` is the
SERVER's mesh identity (`internal/mesh/peer.go:237-243` builds it from `m.agentID`;
the server runs `mesh.DefaultMeshConfig("crier")`, `cmd/server/main.go:149`) — not
the peer's id — so a raw WebSocket client accepted as some other agent still reads
`"agent_id":"crier"` on its own socket:

```json
{"type":"KEEPALIVE","version":1,"message_id":"9d8f0a1b2c3d4e5f6a7b8c9d",
 "timestamp":"2026-08-10T01:55:00.123456789-05:00","lease_id":"","agent_id":"agent-a"}
```

The same frame as the server sends it, captured live from a raw client (nothing
below is a placeholder — this is a frame observed arriving on the socket):

```json
{"type":"KEEPALIVE","version":1,"message_id":"13f7ac9931a32857e537d1fe",
 "timestamp":"2026-09-18T13:33:50.921105103-05:00","lease_id":"","agent_id":"crier"}
```

### REQUEST (agent → agent, relayed by server)

An RPC-style request. The server looks up the target in its connection table and
forwards the frame verbatim; it records a route keyed by `message_id` so the
response can find its way back.

```json
{"type":"REQUEST","version":1,"message_id":"0e64c4d94d584d3fb9d09fcf",
 "timestamp":"2026-08-10T01:55:00.123456789-05:00",
 "source":{"agent_id":"agent-a"},"target":{"agent_id":"agent-b"},
 "method":"GET","path":"/ping","body":{"hello":"world"},
 "trace_id":"f1e2d3c4b5a69788796a5b4c","timeout_ms":5000}
```

| Field | Type | Notes |
|-------|------|-------|
| `source` / `target` | `PeerRef` | `{"agent_id": string}` — the only field |
| `method` / `path` | string | Application-level route (no server-side routing semantics) |
| `body` | any | Opaque application payload, omitted when empty |
| `trace_id` | string | Correlation ID echoed in responses |
| `timeout_ms` | int | Sender's request timeout; the server does not enforce it |

### RESPONSE (agent → agent, relayed by server)

```json
{"type":"RESPONSE","version":1,"message_id":"1ee40f32eefc4b5fb8653d53",
 "timestamp":"2026-08-10T01:55:00.123456789-05:00",
 "request_id":"0e64c4d94d584d3fb9d09fcf","source":{"agent_id":"agent-b"},
 "status_code":200,"body":{"pong":true},"trace_id":"f1e2d3c4b5a69788796a5b4c"}
```

| Field | Type | Notes |
|-------|------|-------|
| `request_id` | string | **Must echo the REQUEST's `message_id`** — see correlation contract |
| `source` | `PeerRef` | Responding agent |
| `status_code` | int | HTTP-style status code |
| `body` | any | Opaque JSON value (`json.RawMessage` server-side), relayed verbatim — the responder decides the JSON type: an object body arrives as an object, a string body as a string (see below) |
| `trace_id` | string | Echo of the request's `trace_id` |

**Bodies are responder-controlled.** A RESPONSE `body` is an opaque JSON value, not a
string: the server holds the frame's raw bytes (`Response.Body json.RawMessage`,
`internal/mesh/message.go:79`) and hands the frame it received to the requester's
connection unchanged (`forwardResponse` calls `conn.Send(data)` with those bytes,
`internal/mesh/peer.go:402-420`). Nothing unwraps the body, re-encodes it, or
stringifies it, so the requester decodes the bytes the responder wrote and the JSON
type is the responder's choice. A responder that puts an object in the field
(`{"pong": true}`, as in the example above) sends an object and the requester gets an
object back; a responder that stringifies its own payload — Python
`json.dumps({"pong": True})`, as the worked example at the end of this document sends —
puts a JSON **string** on the wire, and a string is what the requester gets back. Both
are valid frames; neither is the server "encoding" anything. `TestResponseBodyRelayedVerbatim`
pins this over the relayed path in `internal/mesh`, including that the body bytes come
back unchanged.

Consumer note: an MCP client can read the body only when it is an object — the
`mesh_request` tool decodes `reply.body` into a `map[string]any`
(`internal/mcp/messaging.go:266-271`, `internal/mcp/types.go:204-208`) and discards the
decode error, so a STRING body reaches that client as a `null` `body` field rather than
as a string.

### ERROR (server → agent, or agent → agent)

Sent by the server when a REQUEST cannot be delivered (target offline, route table
full). Also used by agents to fail a request.

```json
{"type":"ERROR","version":1,"message_id":"e57206b16d39e6e28a01e286",
 "timestamp":"2026-08-10T01:47:24.332210795-05:00",
 "request_id":"a084c5f8fe35491bb4791a46",
 "error":{"code":"CONTROLLER_OFFLINE","message":"peer agent-c not connected"},
 "trace_id":"f1e2d3c4b5a69788796a5b4c"}
```

`request_id` carries the failed REQUEST's `message_id` — the same value a RESPONSE
echoes, because the server routes by the original `message_id` and looks the frame up
by its `request_id` on the one `forwardResponse` path that serves both types. ERROR
frames are therefore correlated exactly like RESPONSE frames: match on `request_id`
(the reply's correlation field), never on `message_id` (the frame's own id). A client
that correlates on `message_id` alone never matches a server-sent ERROR and hangs
until its own timeout.

`error` is an `ErrorDetail`: `{code, message, retry_after_ms?}`. Defined codes:

| Code | Meaning |
|------|---------|
| `CONTROLLER_OFFLINE` | Target peer is not connected |
| `RATE_LIMITED` | Rate limit hit |
| `INVALID_MESSAGE` | Malformed frame |
| `AUTH_FAILED` | Authentication failed |
| `FORBIDDEN` | Not authorized |
| `INTERNAL` | Server-side failure (e.g. route table full) |

## Correlation contract (read this first)

The server routes replies **by the request's `message_id`**, but looks them up **by
their `request_id`**. That is one rule for both reply types — RESPONSE and ERROR frames
take the same `forwardResponse` path (`internal/mesh/peer.go`: routes keyed on
`req.MessageID`, `forwardResponse` looks up the reply's `request_id`).

**A responder MUST set `request_id` to the exact `message_id` of the REQUEST it is
answering.** If it does not, the response is silently dropped and the requester
hangs until its own timeout. This is live-verified: a RESPONSE carrying a
non-matching `request_id` never reaches the requester. The server's own ERROR frames
already follow this rule (see §ERROR).

The server-side flow: REQUEST arrives → route `message_id → requester` recorded →
frame forwarded to target. A RESPONSE or ERROR arrives → `request_id` looked up in the
route table → frame forwarded back to the original requester → route deleted. A reply
with an unknown `request_id` is dropped with no error and no log.

## Error handling and silent drops

- REQUEST to an offline target → server sends an `ERROR` frame
  (`CONTROLLER_OFFLINE`) back to the requester, in place of a response. The
  requester's pending channel treats ERROR as a synthetic RESPONSE with
  `status_code: 500` and the `ErrorDetail` JSON as the body.
- Route table full (`MaxPendingRequests` entries, default 50, flushed wholesale) → `ERROR` `INTERNAL`.
- **Malformed frames (unparseable JSON, wrong field shapes) are silently dropped** —
  no `ERROR` frame, no log line. This is a known limitation, not a feature: build
  validation into your client, or run a proxy that adds it.
- `INVALID_MESSAGE` is defined as a code but no current code path emits it.

## Not implemented (do not rely on it)

- Server-side `REGISTER_ACK` — the one-way REGISTER is the whole handshake.
- Keepalive liveness — KEEPALIVE frames have no server-side effect.
- Automatic reconnect — `DialerConfig` carries `ReconnectBackoff`/`MaxRetries`
  fields, but no code performs retries; a dropped connection is closed for good.
- Deregister message type (dropped during the CI-002 port; use the registry's
  `DELETE /agents/{id}`).

## Worked example (Python, verified live)

A complete two-agent request/response round-trip with the `websockets` library.
`RECEIVE` messages are prefixed so you can see the wire format:

```python
import asyncio, json, uuid
import websockets

WS = "ws://127.0.0.1:18767/mesh/connect/{}"

def frame(t, **kw):
    return json.dumps({"type": t, "version": 1,
                       "message_id": uuid.uuid4().hex[:24],
                       "timestamp": "2026-08-10T01:55:00.123456789-05:00",
                       **kw})

async def main():
    a = await websockets.connect(WS.format("agent-a"))
    b = await websockets.connect(WS.format("agent-b"))

    # One-way registration (no REGISTER_ACK will arrive)
    await a.send(frame("REGISTER", agent_id="agent-a", lease_id="", lease_ttl_ms=3600000,
                       capabilities={"version": "0.1.0", "max_concurrent_sessions": 10}))
    await b.send(frame("REGISTER", agent_id="agent-b", lease_id="", lease_ttl_ms=3600000,
                       capabilities={"version": "0.1.0", "max_concurrent_sessions": 10}))

    # agent-a -> agent-b REQUEST; the message_id must be echoed as request_id
    mid = uuid.uuid4().hex[:24]
    await a.send(frame("REQUEST", message_id=mid, source={"agent_id": "agent-a"},
                       target={"agent_id": "agent-b"}, method="GET", path="/ping",
                       body={"hello": "world"}, trace_id="tr-1", timeout_ms=5000))
    req = json.loads(await b.recv())
    print("B received REQUEST:", req["message_id"] == mid)

    await b.send(frame("RESPONSE", message_id=uuid.uuid4().hex[:24], request_id=mid,
                       source={"agent_id": "agent-b"}, status_code=200,
                       body=json.dumps({"pong": True}), trace_id="tr-1"))
    resp = json.loads(await a.recv())
    print("A received RESPONSE:", resp["status_code"] == 200 and resp["request_id"] == mid)

    await a.close()
    await b.close()

asyncio.run(main())
```

Run it against a local server (`CRIER_PORT=18767 ./bin/crier`), then check the peer
list:

```bash
curl -s http://127.0.0.1:18767/mesh/peers
# {"peers":[{"agent_id":"agent-a"},{"agent_id":"agent-b"}],"count":2}
```

## Protocol versioning

All frames carry `"version": 1`. Version mismatch is not currently checked
server-side; a future version may add a version negotiation step in REGISTER.
