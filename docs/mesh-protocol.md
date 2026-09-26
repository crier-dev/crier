# Crier Mesh Protocol — Wire Format

The mesh is Crier's peer-to-peer agent communication layer. This document is the
authoritative reference for the wire protocol used by `GET /mesh/connect/{agentID}`.
All claims below are verified against `internal/mesh` (live-probed 2026-08-10).

## Transport

- WebSocket upgrade to `GET /mesh/connect/{agentID}` — the `agentID` in the path is
  the connecting agent's identity.
- **Whether that identity is verified depends on configuration (DF-CRIER-287).**
  By default (`CR_REQUIRE_MESH_AUTH` unset) no bearer token and no signature are
  required on this endpoint, unlike the registry API; with
  `CR_REQUIRE_MESH_AUTH=true` the server challenges the connection and the peer
  must sign the challenge with the ed25519 key the registry holds for that agent
  id before it is admitted as a peer. See §Authentication.
- The server accepts connections from any origin (the upgrader's origin check
  defaults to allow-all; `mesh.SetWSCheckOrigin` can restrict it, and the server
  wires `CR_MESH_ALLOWED_ORIGINS` into it — §Origin policy).
- Connected peers are listed via `GET /mesh/peers` → `{"peers":[{"agent_id":"..."}],"count":N}`
  — a peer that has not completed the handshake is not listed.
- On the default configuration peer identity is asserted by the URL path only, and
  there is no cryptographic authentication on mesh frames — do not use the mesh
  for privileged operations without an additional application-level auth layer or
  without turning the handshake on.
- `?inbox_notify=1` on the connect URL opts **that connection** into the
  new-message ping (§INBOX_NOTIFY): a tick the server writes to this socket when
  a message lands in the agent's durable inbox. It is opt-in because the frame is
  unsolicited from the client's point of view — without it the server never
  writes anything to this socket that the client did not ask for. The value must
  be a boolean (`1`/`0`/`true`/`false`) or the connect is refused `400` with
  `{"error":"inbox_notify must be a boolean (1/0/true/false)"}`.

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
| `type` | string | One of `REGISTER`, `REGISTER_ACK`, `KEEPALIVE`, `REQUEST`, `RESPONSE`, `ERROR`, `INBOX_NOTIFY`, plus `AUTH_CHALLENGE`, `AUTH_RESPONSE` and `AUTH_OK`, which exist only on a connection to a server started with `CR_REQUIRE_MESH_AUTH=true` (§Authentication) |
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
`Mesh.keepaliveLoop`). The server **records it as liveness evidence and sends
nothing back**: `handleMessage` advances the registry `last_seen` of the peer the
frame arrived FROM (CR-FEAT-024) and draws no reply of any kind — not even the
`INVALID_MESSAGE` an actual malformed frame draws (see §Error handling and silent
drops), because the frame is well-formed and the absence of a response is part of
the protocol.

Two things that evidence is NOT. It is attributed to the **socket**, never to the
frame's own `agent_id` field — a peer that names another agent in a KEEPALIVE
refreshes nothing but its own row, so the field cannot be used to keep a dead
agent looking alive. And it enforces nothing: the server never disconnects a peer
for missing heartbeats, so a peer that goes quiet keeps its connection and stays
listed by `GET /mesh/peers` while its registry row goes `stale` once
`CR_PRESENCE_STALE_AFTER_S` (default 90s = three missed beats) passes — the
registry half of that is README §3, and the record is one write per heartbeat per
agent.

The server sends a KEEPALIVE to every accepted peer every 30 seconds: the accept
path starts the same loop (`Mesh.AcceptPeer` → `Mesh.keepaliveLoop`), on the same
`KeepaliveInterval` (`internal/mesh/peer.go`, `MeshConfig.KeepaliveInterval`) the
client loop ticks on. The frame's `agent_id` is the
SERVER's mesh identity (`Mesh.keepaliveLoop` builds it from `m.agentID`;
the server runs `mesh.DefaultMeshConfig("crier")`, `cmd/server/main.go`) — not
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

### INBOX_NOTIFY (server → agent, opt-in)

Sent by the server to an agent whose **connection asked for it** with
`?inbox_notify=1`, immediately after a delivery has been stored in that agent's
durable inbox. It is the mesh half of the durable lane's wake-up: the inbox is
poll-only by history, so an agent that would otherwise poll on a timer can be
tapped instead. The other half — `?wait_seconds=` on
`GET /agents/{id}/inbox`, which parks the read until a message is claimable —
needs no mesh socket at all.

```json
{"type":"INBOX_NOTIFY","version":1,"message_id":"7a1c9f2e5b6d80314a2f6b90",
 "timestamp":"2026-09-25T16:31:31.032118-05:00","agent_id":"agent-b",
 "inbox_message_id":"5dd711e2f6845fd33e64e077","sender":"agent-a"}
```

| Field | Type | Notes |
|-------|------|-------|
| `agent_id` | string | The PINGED agent — the owner of the inbox, not the sender |
| `inbox_message_id` | string | The id the delivery was accepted with, i.e. the same id `GET /agents/{id}/inbox` returns in the entry. Absent only if the store minted no id |
| `sender` | string | Originating agent id when the delivery named one, else absent |

The frame carries **no payload**: it is a tick, not a delivery. The message is
already durable when the frame is written, so a client that ignores the ping (or
never receives it) loses nothing but latency — it still finds the message on its
next retrieve. There is no reply and no acknowledgement: the client answers by
retrieving its inbox (and may then ack as usual).

Properties worth relying on, each pinned by a test in `internal/mesh`:

- **Opt-in per connection.** The grant is made by the connect URL and dies with
  that socket; a client that did not ask receives no unsolicited frame, and a
  reconnect that does not ask again is not pinged on the old grant.
- **Best effort, no queue.** A ping written when the socket write fails (or when
  the agent is not connected here) is dropped with a debug log. It is a
  notification about a message that is already durable, never a delivery
  channel, so it is never retried and never buffered.
- **One frame per delivery.** A burst of deliveries produces a burst of pings;
  an agent that wants one wake-up per batch should long-poll
  (`?wait_seconds=`) instead, which answers with the whole claimable batch.
- **Inbound `INBOX_NOTIFY` is ignored.** The frame has a single direction; a
  client that echoes one back is recognized and ignored, never answered with
  `INVALID_MESSAGE` (§Error handling and silent drops).

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
connection unchanged (`forwardResponse` calls `conn.Send(data)` with those bytes —
`internal/mesh/peer.go`, the `Mesh.forwardResponse` reply path). Nothing unwraps
the body, re-encodes it, or
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
full) or when an inbound frame is malformed (`INVALID_MESSAGE`). Also used by agents
to fail a request.

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

A **malformed** inbound frame is refused with that same shape, code `INVALID_MESSAGE`
(`internal/mesh/peer.go`: one helper, `reportInvalidMessage`, feeds the same `sendErrorTo`
that emits `CONTROLLER_OFFLINE` and `INTERNAL`). A frame that does not decode as an
envelope carries no id to echo, so `request_id` is **absent** — the field is
`omitempty` and is never written blank. Captured from the live path:

```json
{"type":"ERROR","version":1,"message_id":"a2b8e3095b0eab8d1017dad2","timestamp":"2026-09-19T14:52:12.540838707-05:00","error":{"code":"INVALID_MESSAGE","message":"malformed frame: not a JSON envelope (invalid character 'o' in literal null (expecting 'u'))"}}
```

A frame that *does* decode as an envelope but does not match the shape its `type`
declares — or whose `type` the protocol does not define — is refused with `request_id`
set to that envelope's `message_id`, so the refusal correlates with what you sent:

```json
{"type":"ERROR","version":1,"message_id":"0f1c8c782783ce351e27b5fc","timestamp":"2026-09-19T14:52:19.129445128-05:00","request_id":"reg-bad-1","error":{"code":"INVALID_MESSAGE","message":"malformed REGISTER frame: json: cannot unmarshal string into Go struct field Register.lease_ttl_ms of type int"}}
```

Read that second example as the contract for the field: a client that correlates on
`request_id` must treat an `ERROR` **without** one as "what I sent could not be
parsed" (there is nothing to correlate to), not as a missing reply.

`error` is an `ErrorDetail`: `{code, message, retry_after_ms?}`. Defined codes:

| Code | Meaning |
|------|---------|
| `CONTROLLER_OFFLINE` | Target peer is not connected |
| `RATE_LIMITED` | Rate limit hit |
| `INVALID_MESSAGE` | Malformed frame |
| `AUTH_FAILED` | Authentication failed |
| `FORBIDDEN` | Not authorized |
| `INTERNAL` | Server-side failure (e.g. route table full) |

## Authentication (opt-in, DF-CRIER-287)

Whether the agent id in the connect URL is *verified* or merely *asserted* is a
configuration choice. Two settings, one of which changes the wire protocol:

| setting | default | effect |
|---|---|---|
| `CR_REQUIRE_MESH_AUTH` | `false` | Connect handshake (below). Unset, no `AUTH_*` frame is ever sent, the path id is taken at face value, and every client in this repository works unchanged. |
| `CR_MESH_AUTH_TIMEOUT_S` | `10` | How long a challenge stays valid: the peer must answer within it, and the server also enforces it as a read deadline on the pre-auth socket. |

### Handshake

When the handshake is required, the identity is proven before ANY other frame is
processed, and a connection that has not proven it is not a peer:

1. the WebSocket upgrade succeeds, but the connection is **not** added to the peer
   table and is **not** listed by `GET /mesh/peers`;
2. the server sends `AUTH_CHALLENGE`, naming the path identity and a single-use
   nonce (32 hex chars from `crypto/rand`):

```json
{"type":"AUTH_CHALLENGE","version":1,"message_id":"5b4c3d2e1f0a9b8c7d6e5f4a",
 "timestamp":"2026-09-25T11:02:00.123456789-05:00","agent_id":"agent-1",
 "nonce":"9f1c0a7e4b2d6835a0c1e9f7b4d2638a",
 "expires_at":"2026-09-25T11:02:10.123456789-05:00"}
```

3. the peer answers `AUTH_RESPONSE`, echoing the nonce and carrying the hex
   ed25519 signature over the payload below:

```json
{"type":"AUTH_RESPONSE","version":1,"message_id":"2e1f0a9b8c7d6e5f4a3b2c1d",
 "timestamp":"2026-09-25T11:02:00.234567890-05:00","agent_id":"agent-1",
 "nonce":"9f1c0a7e4b2d6835a0c1e9f7b4d2638a",
 "signature":"<128 hex chars: the 64-byte ed25519 signature>"}
```

4. the signature is verified against the public key the registry holds for
   `agent_id` (`mesh.AgentKeyProvider`, wired to the same store that gates the
   inbox lane). On success the server sends `AUTH_OK` and admits the connection —
   starting its read loop, registering it and beginning its keepalive:

```json
{"type":"AUTH_OK","version":1,"message_id":"1d0e9f8a7b6c5d4e3f2b1a0c",
 "timestamp":"2026-09-25T11:02:00.345678901-05:00","agent_id":"agent-1"}
```

### The signed payload

Raw ed25519 over exactly these bytes — no pre-hash, no PKCS#1 framing
(`mesh.MeshAuthPayload`):

```
mesh-auth-v1\n<agent_id>\n<nonce>
```

The prefix domain-separates the handshake from the registry lane's inbox
signature, which covers `"<METHOD>\n<path>\n<unix-seconds>"` — neither lane's
signature can be replayed on the other. The payload binds the CLAIMED identity to
THIS challenge, so a signature captured from an earlier connection verifies
against neither a different agent id nor a different nonce.

Signing it with OpenSSL ≥ 3 (the payload must be a file: `pkeyutl -sign -rawin`
fails on a pipe with `unable to determine file size for oneshot operation`, and
yields a zero-byte signature):

```bash
printf 'mesh-auth-v1\n%s\n%s' "$AGENT_ID" "$NONCE" > payload.txt
openssl pkeyutl -sign -rawin -inkey agent.key -in payload.txt | xxd -p -c 128
```

The `openssl genpkey -algorithm ED25519 -out agent.key` key is the one whose
hex public half was registered with `POST /agents`; a client can sign with
`cryptography`'s `Ed25519PrivateKey.sign(MeshAuthPayload)` equivalently.

### Refusals

| situation | answer | socket |
|---|---|---|
| no answer within the window | `ERROR` `AUTH_FAILED` ("no AUTH_RESPONSE within …") | closed |
| first frame is anything but `AUTH_RESPONSE` (e.g. a pre-fix client's `REGISTER`) | `ERROR` `AUTH_FAILED`, naming the frame and the required one | stays open for a correct answer within the same window |
| frame is not JSON | `ERROR` `INVALID_MESSAGE` (no `request_id`: nothing to correlate to) | stays open |
| `agent_id` is not the path identity | `ERROR` `AUTH_FAILED` ("an agent may only authenticate as itself") | closed |
| nonce does not match the challenge | `ERROR` `AUTH_FAILED` ("nonce does not match") | closed |
| signature is not 128 hex chars | `ERROR` `AUTH_FAILED` ("malformed") | closed |
| signature does not verify against the registered key | `ERROR` `AUTH_FAILED` ("signature verification failed") | closed |
| the agent has no registered key (unknown id, or a keyless registration) | `ERROR` `AUTH_FAILED` naming the missing registration | closed |
| the server requires the handshake but has no key source wired | `ERROR` `AUTH_FAILED` ("no agent key provider") — fail closed | closed |

An `AUTH_*` frame that arrives OUTSIDE the handshake — on a connection that is
already admitted, or on a server that does not require authentication — is
refused `INVALID_MESSAGE` naming the frame, not ignored.

### Frame-level identity rules (authenticated mesh only)

Because the socket's identity is now proven, `agent_id` in a frame stops being a
claim. Two rules are enforced **only** while `CR_REQUIRE_MESH_AUTH=true`:

- a `REQUEST` whose `source.agent_id` is **not** the authenticated peer is refused
  `FORBIDDEN` ("an agent may only request as itself") and is **not** forwarded —
  the target never sees a request that names another agent. A `REQUEST` with no
  `source.agent_id` at all is refused the same way.
- a `RESPONSE` or `ERROR` may only be sent by the peer the request was addressed
  to; from any other connection it is refused `FORBIDDEN` and **not** forwarded to
  the requester.

With the handshake off, neither rule applies: there is no verified identity to
bind the fields to, and the frames are forwarded exactly as before.

### Migration note

Turning the handshake on is a **client-side** change, not a transparent one:

- every connecting agent must hold the private half of the key registered for it
  (`POST /agents` with `public_key`); a keyless registration — legal while
  `CR_REQUIRE_AGENT_SIG=false` (§DF-CRIER-192) — can never authenticate;
- clients that do not implement the handshake are refused with `AUTH_FAILED`
  naming the missing `AUTH_RESPONSE`; the server logs the refusal and
  `GET /status` reports `"mesh_auth_required": true`;
- the Go client in this repository performs the handshake when
  `mesh.MeshAuthConfig.SigningKey` is set (and expects the server to challenge it,
  because `SigningKey` and `Required` describe the same deployment). A client sent
  to a server that does not challenge it simply proceeds, unchanged;
- the MCP bridge (`internal/mcp`) is a mesh client too: with the flag on, its
  socket needs a key, so give it the same `CRIER_AGENT_PRIVATE_KEY_FILE` identity
  the registry lane uses.

### Replay protection on the signed lanes — the ±30 s window is the whole defence (DF-CRIER-294)

Crier has two independent signature lanes, and they answer the replay question
DIFFERENTLY. Neither difference is an accident, and the second one is a bound a client has to
size its own trust against:

| lane | what is signed | what stops a replay |
|---|---|---|
| mesh handshake (`AUTH_RESPONSE`, DF-CRIER-287) | `mesh-auth-v1\n<agent_id>\n<nonce>` (§ The signed payload) | a **single-use nonce** minted per connection (32 hex chars from `crypto/rand`) plus the `CR_MESH_AUTH_TIMEOUT_S` answer window (default 10 s). A captured frame is not reusable: the nonce is never reissued, and a stale or mismatched one is refused `AUTH_FAILED` (§ Refusals). |
| registry, the agent-scoped HTTP routes that require the trio — `GET /agents/{id}/inbox`, `POST /agents/{id}/inbox/ack`, `GET /agents/{id}/inbox/stats`, `GET /agents/{id}/inbox/dead-letters`, `POST /agents/{id}/inbox/transfer`, `DELETE /agents/{id}` and `PATCH /agents/{id}` — via `X-Agent-ID` / `X-Agent-Ts` / `X-Agent-Sig` | `"<METHOD>\n<path>\n<unix-seconds>"` | **only the freshness of `X-Agent-Ts`**: it must be within **±30 s** of server time (`sigWindow`, `internal/registry/agentsig.go`; the constant's own comment reads "Bounded replay protection: a captured request is only valid for sigWindow seconds"). A timestamp outside that window — stale or future — is refused `401 {"error":"request timestamp outside allowed window (±30s)"}`. |

**A byte-identical signed request REPLAYED INSIDE that ±30 s window is ACCEPTED, and the window is
the WHOLE replay defence on those routes.** The server keeps no per-signature state there —
`authorizeAgent` decides on the method, the path, the timestamp and the signature alone — so it
cannot tell a replay from the original, and does not pretend to. Measured (DF-CRIER-294): one signed
`GET /agents/{id}/inbox` presented twice in a row with the SAME timestamp and signature was
authorized twice (`200` both times, on the authorization path); the same signature carrying a
10-minute-old timestamp was refused `401` naming the window. `POST /relay/publish` is outside this
lane entirely — the trio is not verified there at all (README § Try it; measured).

A nonce/signature cache that would close the gap is deliberately NOT implemented (DF-CRIER-294
judged it out of scope): a shared cache on the auth path is a new availability dependency, i.e. a
design change rather than a bug fix. The practical rule for a client is therefore: keep the
transport confidential (TLS) so a signature cannot be captured in the first place, and treat a
captured signed request as usable for at most 30 seconds. The handshake lane needs no such caveat —
its nonce makes each signature usable exactly once.

## Origin policy

The WebSocket upgrade rejects a request whose `Origin` header the policy does not
allow, with `403` and no frame — before the handshake and before any peer exists.
Two knobs, deliberately separate, because the two lanes have different clients:

| setting | default | lane | rule |
|---|---|---|---|
| `CR_WS_ALLOWED_ORIGINS` | unset (all) | relay | when set, the request's `Origin` must match a listed value — an ABSENT `Origin` does not match |
| `CR_MESH_ALLOWED_ORIGINS` | unset (all) | mesh | when set, a request that CARRIES an `Origin` must match a listed value; a request with NO `Origin` is allowed |

The mesh rule differs on purpose: agent clients (this repository's Go client, the
Python example below, the MCP bridge, `websocat`) send no `Origin` header at all,
so applying the relay's rule to the mesh would refuse every legitimate agent the
moment an operator set an allowlist. The header is the only thing a policy can
bind, and browsers — the clients that always send it — are what it fences off. The
mode in force is published by `GET /status` as `mesh_origin_policy`:
`"allow-all"` or `"allowlist"`.

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
- **A malformed frame is answered with `ERROR` `INVALID_MESSAGE`, not swallowed.**
  Malformed means the frame does not decode as an envelope (unparseable JSON, or
  JSON that is not an object), the envelope names a `type` this protocol does not
  define, or the payload does not match the shape its `type` declares — a
  `REGISTER` whose `lease_ttl_ms` is a string, a `REQUEST` whose `source` is not a
  `PeerRef` object, a `RESPONSE` whose `request_id` is a number. One helper
  emits those refusals — `reportInvalidMessage`, called from `handleMessage`
  and its `handleAgentRequest` callee (`internal/mesh/peer.go`) — and one
  further branch emits the code directly: `handleAgentRequest`'s blank-target
  case (a REQUEST that decodes cleanly but names no `target.agent_id`; see the
  next bullet) calls `sendErrorTo` itself. `error.message` names the failure. The connection
  stays usable: a client that sent one bad frame can send a well-formed REQUEST on
  the same socket afterwards. Pinned by `TestMeshMalformedFramesGetInvalidMessage`
  and `TestMeshWellFormedFramesGetNoError` (`internal/mesh`).
- **Well-formed frames are untouched.** In particular an inbound `KEEPALIVE` is
  recognized, recorded as liveness evidence for the sender's registry row, and
  answered with nothing at all (§KEEPALIVE), an inbound
  `INBOX_NOTIFY` is recognized and ignored for the same reason — it is a
  server→agent frame and has no meaning in that direction (§INBOX_NOTIFY) — and
  a REQUEST that carries no `target.agent_id` is still refused
  `INVALID_MESSAGE` with a message naming the missing field.
- **On an authenticated mesh a frame's identity is checked too (DF-CRIER-287).**
  With `CR_REQUIRE_MESH_AUTH=true` the socket's identity was proven at connect, so
  a `REQUEST` whose `source.agent_id` is not that peer is answered
  `ERROR` `FORBIDDEN` ("an agent may only request as itself") and is **not**
  forwarded — the target never sees it — and a `RESPONSE`/`ERROR` is only accepted
  from the peer the request was addressed to. With the flag unset (the default)
  neither check applies, and the frames are forwarded exactly as before
  (§Authentication).
- `INVALID_MESSAGE`'s `request_id` is the malformed frame's own `message_id` when
  the envelope was readable; a frame that did not decode as an envelope carries
  none, so the field is **absent** rather than blank (§ERROR, with both captured
  frames).
- Still silent, by design: a RESPONSE or ERROR whose `request_id` matches no
  recorded route is dropped with a debug log (§Correlation contract — there is
  nothing left to correlate it to), and every mesh `ERROR` is best effort, so a
  peer that has already disconnected receives nothing.

## Not implemented (do not rely on it)

- Server-side `REGISTER_ACK` — the one-way REGISTER is the whole handshake.
- Keepalive ENFORCEMENT — a peer that stops sending KEEPALIVEs is never
  disconnected for it. The only server-side effect a heartbeat has is liveness
  evidence (the sender's registry `last_seen`, §KEEPALIVE): nothing times a peer
  out, and a registry row that goes `stale` does not close its socket or remove
  it from `GET /mesh/peers`.
- Ping guarantees — `INBOX_NOTIFY` is best effort: no queue, no retry, no ack,
  no ordering against other frames. It EXISTS to remove latency, not to promise
  a wake-up; the durable lane (`GET /agents/{id}/inbox`, with or without
  `?wait_seconds=`) remains the contract.
- Automatic reconnect — `DialerConfig` carries `ReconnectBackoff`/`MaxRetries`
  fields, but no code performs retries; a dropped connection is closed for good.
- Deregister message type (dropped during the CI-002 port; use the registry's
  `DELETE /agents/{id}`).
- Replay rejection on the agent-signed HTTP routes — that lane keeps no nonce cache, so a captured
  signed request is honoured for as long as its `X-Agent-Ts` stays inside the ±30 s window. That
  window IS the defence there (§ Replay protection on the signed lanes, DF-CRIER-294); the mesh
  handshake lane is the one with a single-use nonce.

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
