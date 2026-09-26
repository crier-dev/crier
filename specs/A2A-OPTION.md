# A2A-OPTION.md — A2A interoperability as an OPT-IN extra

Status: **DRAFT v3** · 2026-09-25 · Owner: Bane · Tickets: INT-A2A-001 (this doc + the opt-in gate),
INT-A2A-002 (**SHIPPED** — the Agent Card discovery route, §5.3), INT-A2A-003 (**SHIPPED** — the
JSON-RPC binding: SendMessage + SendStreamingMessage, §5.4), INT-A2A-004 (**SHIPPED** — the task
lifecycle: GetTask / ListTasks / CancelTask / SubscribeToTask mapped onto the inbox entry, §5.5),
INT-A2A-005 (**SHIPPED** — the push-notification configuration operations, §5.6), INT-A2A-006 (the
extended card — the remaining A2A surface, NOT shipped)
Source: **A2A v1.0.0** (`github.com/a2aproject/A2A`) — section numbers below are that specification's.
Precedent: `specs/WEBHOOK-DELIVERY.md` (same rigor + format).

## 1. Goal and standing constraint

Crier is an agent-to-agent message bus: relay pub/sub, a WebSocket mesh, an agent registry and
durable inboxes. A2A is the interoperable wire protocol other agent frameworks speak. This document
lets a crier agent be reachable by an A2A client — and, later, lets a crier agent be an A2A client —
**without crier becoming an A2A-first system**.

**The standing constraint (Bane, 2026-09-25), restated into every row of this series:**

> A2A is an **EXTRA**, not first-class support. It must be **OPT-IN** and **DEFAULT-OFF**,
> **additive-only**, and **MUST NOT change the behaviour of anything crier already does**.
> No new auth requirement may appear on an existing route; no existing field may change meaning.

### 1.1 Not first-class — not in the default path

This is a binding statement, not a slogan:

- **No A2A code is reachable with the option off.** The server-side switch is `CR_A2A_ENABLED`
  (default **false**, §4.1). With it unset — the default posture, and the posture every existing
  deployment is in — crier's registered route table, request handling, response bodies, auth
  requirements and storage schema are exactly what they were before this option existed. (With it
  SET, exactly two routes exist: the Agent Card discovery route of §5.3 and the JSON-RPC binding of
  §5.4, each serving only agents that opted in.)
- **A2A is not on the default path of any existing route.** Deliveries, inbox lifecycle, webhook
  push, the mesh, federation, the guard and the MCP surface are unchanged: nothing in any of them
  consults the A2A switch or the per-agent `a2a` block.
- **The A2A surfaces are additive**: new routes that exist only while the option is on, and only for
  agents that opted in (§4.2, §5). No existing route grows an A2A-only field, status code or header.
- **Adding the option does not make crier an A2A implementation.** §2 states exactly which part of
  the A2A specification is implemented and which parts are deliberately not.

## 2. Binding decision — JSON-RPC 2.0 over HTTP + SSE

The A2A v1.0.0 specification defines three protocol bindings (§8.3, §9, §10, §11). Crier implements
**exactly one**:

| A2A binding | Spec section | Crier decision |
|---|---|---|
| **JSON-RPC 2.0 over HTTP(S), with SSE for streaming** | §9 | **IMPLEMENTED** (this series). §9.1 fixes the contract: JSON-RPC 2.0, `Content-Type: application/json` for requests and responses, **PascalCase method names matching gRPC conventions** (`SendMessage`, `GetTask`, …), and Server-Sent Events (`text/event-stream`) for streaming. |
| gRPC | §10 | **MAY — not built.** Declared permitted-but-absent: crier advertises no gRPC interface in its AgentCard and serves no gRPC endpoint. |
| HTTP+JSON/REST | §11 | **MAY — not built.** Same: no REST binding routes, not advertised. |

Consequences of that decision, stated so no later row has to guess:

- An A2A client talks to crier with a JSON-RPC 2.0 POST; a streaming method answers `200` with
  `Content-Type: text/event-stream` and `data:` frames (§9.4.2, §9.4.6).
- A2A service parameters (§3.2.6, §9.2) travel as **HTTP headers** — including `A2A-Version` and
  `A2A-Extensions` (§14.2.1, §14.2.2) — never inside the JSON body's `params`.
- Errors use the JSON-RPC 2.0 error object: standard codes (`-32700`, `-32600`, `-32601`, `-32602`,
  `-32603`) plus the A2A-specific range `-32001..-32099` (§9.5, §5.4).
- A2A's **abstract operations** are what this document names (`SendMessage`, `SendStreamingMessage`,
  `SubscribeToTask`, …). In the implemented JSON-RPC binding the wire `method` string is the
  operation's PascalCase name (§9.4), which is the mapping the mapping table below assumes.

## 3. Object mapping — A2A ⇄ crier

The mapping is the normative contract for INT-A2A-002..006. Each row is
*A2A object/operation* ⇄ *the crier thing it is carried by*.

| A2A (v1.0.0) | crier | Notes |
|---|---|---|
| `AgentCard` (§4.4.1) | the agent registry row — `GET /agents/{id}` | The card is a **projection** of the row, not a second store. Discovery path `GET /.well-known/agent-card.json` (§8.2, §14.3) serves the same projection for the process's own identity. **SHIPPED (INT-A2A-002), §5.3.** |
| `AgentSkill[]` (§4.4.5) | `agent.capabilities[]` | Each capability tag becomes a skill entry; the projection is one-way (crier's registry stays the source of truth). |
| `SendMessage` (§9.4.1) | deliver — `POST /agents/{id}/inbox` | The A2A message becomes a crier delivery to the target agent. **SHIPPED (INT-A2A-003), §5.4** — the binding calls that route's own handler, so the guard, idempotency, detection, federation, webhook and inbox paths are the shipped ones. |
| `Task` + `TaskState` (§4.1.1, §4.1.3) | the inbox entry + its lease/ack lifecycle | `submitted` = stored and unclaimed; `working` = leased; `completed` = acked; `failed` = TTL elapsed unacknowledged (or the sweep recorded the expiry); `canceled` = closed by `CancelTask`. The crier entry id is the A2A `task.id`, and the mapping is the table in §5.5.2. **SHIPPED (INT-A2A-003 §5.4.5 for the streamed states, INT-A2A-004 §5.5 for the read operations)**: the states a send can observe are reported on the stream, and `GetTask`/`ListTasks`/`CancelTask` report and produce the same states from the same entry. |
| `SubscribeToTask` / `SendStreamingMessage` (SSE) (§9.4.2, §9.4.6) | relay WS subscribe | A2A's stream is the SSE view of the same subscription surface; the mesh is untouched. **SHIPPED (INT-A2A-003 §5.4.5 for `SendStreamingMessage`, INT-A2A-004 §5.5.1 for `SubscribeToTask`)** — one SSE adapter over `Relay.Subscribe` that also reports the task's own inbox lifecycle, reached both by a send that asked for a stream and by a subscription to an existing task id. |
| `Part` — `text` \| `file` \| `data` — plus `Part.metadata` (§4.1.6) | crier message parts plus `alt`/`tags` | No new payload model: an A2A part maps onto the delivery payload's part metadata. **SHIPPED (INT-A2A-003), §5.4.4** — the mapping is lossless in both directions. |
| `TaskPushNotificationConfig` (§4.3.1) | `webhook.Config` (url / retries / timeout / batch) | A2A push-notification config is expressed with the webhook fields crier already has; no second push mechanism, and no second configuration store. **SHIPPED (INT-A2A-005), §5.6** — the four §9.4.7 operations are a view over that config, and the §4.3.3 notification payload is the config's own schema. An inline `taskPushNotificationConfig` on a send is still REFUSED (§5.6.3) rather than silently accepted. |
| `Message.metadata` / extensions (§4.1.4, §9.2) | the envelope's metadata | The envelope stays the carrier; A2A extension URIs ride the `A2A-Extensions` header. **Shipped (INT-A2A-003)** for `SendMessage`: the message's metadata and extensions are preserved verbatim in the delivered payload (§5.4.4). |
| `AgentCard.securitySchemes` (`AgentCard` §4.4.1, `SecurityScheme` §4.5.1) | crier `bearerAuth` + the custom `agentSignature` scheme | **Additive to the projection only.** Signalling a scheme in a card never adds an auth requirement to an existing crier route (§1, §6). |

Where the A2A specification's identifiers are used on the wire (method names, error codes, the
well-known path), they are used **verbatim**; where crier's are used (inbox, lease, ack, registry
row), they are not renamed.

## 4. Capability flags — the two-half gate

A2A is reachable only when **both** halves of the gate are on. Either half alone is inert.

### 4.1 Server-side: `CR_A2A_ENABLED` (default false)

| | |
|---|---|
| Variable | `CR_A2A_ENABLED` |
| Default | **`false`** — unset means off |
| Parsed by | `config.Load()` (`config/config.go`), the shared tolerant-bool dialect `parseTolerantBool` (`true`/`1`/`yes`, `false`/`0`/`no`, case-insensitive; anything else fails startup naming the variable) |
| Struct field | `config.Config.A2AEnabled bool` — zero value `false`, so an unset flag leaves the option off |
| Consumed by | **nothing yet.** INT-A2A-001 carries the flag only (§8); INT-A2A-002..006 are the rows that read it |

### 4.2 Per-agent: the optional `a2a` block on a registry row

`POST /agents` (and `PATCH /agents/{id}`) accept an OPTIONAL `a2a` object:

```json
{
  "id": "agent-b",
  "public_key": "…",
  "capabilities": ["solver"],
  "a2a": {"enabled": true}
}
```

- **Absent** (the default, and every pre-existing registration): the agent takes no part in A2A.
  `omitempty` keeps the serialized row **byte-identical to a pre-A2A row**.
- **Strict member decode**, the same discipline as the `webhook` object: a member the A2A config
  does not declare is a **400** naming the offending key and the accepted keys
  (`a2a: unknown field "enabld" (accepted: enabled)`) — never a silently dropped member. The
  strictness is scoped to the `a2a` object; the surrounding agent body stays permissive.
- **Explicit JSON `null`** clears the block on `PATCH` (the webhook object's contract).
- **On durable backends** the block is persisted in its own nullable `a2a` JSONB column
  (migration `005_add_agent_a2a_column`); `NULL` is "not opted in" and reads back as an absent
  block, the same wire shape the in-memory backend serves.
- The block is **minimal by design**: today it carries `enabled` only. Later rows extend it
  additively, through the same strict decoder.

## 5. Route surface

### 5.1 With the switch OFF: zero new routes — with it ON: exactly two

With `CR_A2A_ENABLED` unset (the default, and every existing deployment's posture) the route table is
what it was before the option existed: **no A2A route is registered**, and every A2A path answers the
router's ordinary `404`. That half is checked live in
`cmd/server/a2a_optin_test.go` (every A2A path probed in both switch positions) and needs no
maintenance: it is the statement "the option is separable".

With `CR_A2A_ENABLED=true` the server registers **exactly two** A2A routes — the Agent Card discovery
route of §5.3 and the JSON-RPC binding of §5.4 — and nothing else. The two positions differ in
exactly those two paths; every pre-existing route answers identically in both (§6).

### 5.2 Route surface inventory

| Surface | Served for | Registered only when | State |
|---|---|---|---|
| `GET /.well-known/agent-card.json?agent_id=<id>` — the AgentCard of one registry row | AgentCard discovery (§8.2, §14.3) | `CR_A2A_ENABLED=true` **and** the named row opted in | **SHIPPED (INT-A2A-002), §5.3** |
| `POST /a2a` — a JSON-RPC 2.0 endpoint accepting `SendMessage`, `SendStreamingMessage`, `GetTask`, `ListTasks`, `CancelTask` and `SubscribeToTask`, with `Content-Type: text/event-stream` for the two streaming ones | §9.4 core methods | `CR_A2A_ENABLED=true` **and** the tenant's row opted in | **SHIPPED (INT-A2A-003 for the sends §5.4, INT-A2A-004 for the task lifecycle §5.5)** |
| The four push-notification configuration methods on the same endpoint — `CreateTaskPushNotificationConfig`, `GetTaskPushNotificationConfig`, `ListTaskPushNotificationConfigs`, `DeleteTaskPushNotificationConfig` | §9.4.7 | `CR_A2A_ENABLED=true` **and** the tenant's row opted in **and** that agent has a push channel (a webhook) | **SHIPPED (INT-A2A-005), §5.6** |
| `GetExtendedAgentCard` on the same endpoint | §9.4.8 | — | FUTURE (INT-A2A-006) — the endpoint is registered, and this one method answers `-32601 MethodNotFoundError` naming the row that lands it |
| `agent/authenticatedExtendedCard` (auth-gated extended card) | §3.1.11, §9.4.8 | `CR_A2A_ENABLED=true` | FUTURE — not registered |

Every A2A surface is gated by the server switch **and** targets an agent that opted in with the `a2a`
block (§4.2). A future row that needs a wider gate must amend this document first.

### 5.3 The Agent Card route (INT-A2A-002, SHIPPED)

`GET /.well-known/agent-card.json?agent_id=<id>` serves the A2A `AgentCard` (§4.4.1) of ONE registry
row, projected on every request from that row — no card store, no cache, no second source that could
disagree with `GET /agents/{id}`.

**Why a selector.** §8.2's discovery URL identifies one agent, because in the specification a server
IS an agent. A crier relay is a bus: many agents share one origin, so the well-known path alone
cannot name one. `agent_id` is crier's own vocabulary (the `{id}` of `GET /agents/{id}`), and the
served card carries the same value in its `AgentInterface.tenant` (§4.4.6/§8.3.2), so a client that
discovers a card here knows what to send back on every A2A request.

| Answer | When |
|---|---|
| `200` + `application/a2a+json` | the named row exists **and** its `a2a` block is present with `enabled: true` |
| `400` | no `agent_id` (or a blank one): the route is registered, the request cannot name an agent |
| `404` | the id is not a registry row, **or** the row did not opt in — one answer for both, because an A2A client gains nothing from this server listing ids that exist but stayed out (and `GET /agents/{id}` is where that question belongs) |
| `404` | any answer at all with the switch off: the route does not exist |
| `405` | any method but `GET` (the router's own JSON fallback) |

**The projection.** Field by field, what the card states and where the value comes from:

| AgentCard field | Source |
|---|---|
| `name` | the registry id (the identifier clients address the agent by) |
| `description` | generated from the row: the id, and the capability tags it advertises. crier's registry has no description field, and prose invented here would read as the agent's own claim |
| `supportedInterfaces[]` | one entry: `url` = the origin the client reached + `/a2a`, `protocolBinding` = `JSONRPC`, `protocolVersion` = `1.0`, `tenant` = the agent id. The URL is the JSON-RPC endpoint the client sends its methods to, and that endpoint is **now served** (`POST /a2a`, INT-A2A-003, §5.4) — the forward declaration this row made is what §5.4 implements, not a second path |
| `version` | the serving build's identity (`internal/buildinfo`). The registry row carries no version of its own, and a second, invented one would be a source of truth that could drift |
| `documentationUrl` | the same origin + `/docs` (the live API documentation of the surface being described) |
| `capabilities.streaming` | **always `true`, and always serialized**: the JSON-RPC binding served alongside this card answers `SendStreamingMessage` with `text/event-stream` (INT-A2A-003, §5.4). §3.3.4 makes the field load-bearing in the other direction — a card that said `false` would REQUIRE this server to refuse an operation it serves. The relay's WebSocket subscribe is not this: it is crier's own protocol |
| `capabilities.pushNotifications` | `true` exactly when the row carries a crier webhook — the push channel that agent actually has (the object INT-A2A-005 defines the A2A push config to be a view over) |
| `capabilities.extensions` | omitted (empty): crier declares no extension |
| `skills[]` | one skill per capability tag, in row order, duplicates collapsed; `id`, `name` and `tags` all carry the tag, and `description` states the tag and that crier's registry stores tags rather than skill prose. Never `null` — `skills` is REQUIRED |
| `securitySchemes` | the postures actually in force (§5.3.1) |
| `securityRequirements` | the subset of those a client must present (§5.3.1) |
| `defaultInputModes` / `defaultOutputModes` | `application/json`: the media type of crier's delivery and retrieval wire (the payload inside the envelope is opaque) |
| `provider`, `iconUrl`, `signatures` | **absent by construction**: crier has no provider identity to state, serves no icon, and computes no card signature (§7) |

#### 5.3.1 The auth posture (declared, never demanded where a client cannot comply)

The card describes the posture the server is in; it never sets it, and it never adds a requirement to
any route (§1, §6.4).

| Posture in force | `securitySchemes` | `securityRequirements` |
|---|---|---|
| no `CR_AUTH_TOKEN`, signatures off | *(none)* | *(none)* — crier requires nothing |
| no `CR_AUTH_TOKEN`, `CR_REQUIRE_AGENT_SIG` on | `agentSignature` | *(none)* |
| `CR_AUTH_TOKEN` set | `bearerAuth` | `bearerAuth` |
| `CR_AUTH_TOKEN` set, `CR_REQUIRE_AGENT_SIG` on | `bearerAuth` + `agentSignature` | `bearerAuth` only |

- **`bearerAuth`** is an HTTP bearer scheme (§4.5.3) named exactly as crier's own OpenAPI
  `components.securitySchemes` names it. With `CR_AUTH_TOKEN` set it is REQUIRED of an A2A client,
  because it is required of every caller — this discovery route included. The route is **not** added
  to the auth-exempt list in `internal/middleware/auth.go`.
- **`agentSignature`** is declared as an apiKey-style header scheme (§4.5.2) — the `X-Agent-Sig`
  header crier's OpenAPI spec already describes — whenever `CR_REQUIRE_AGENT_SIG` enforces it on the
  signed agent-scoped routes. It is **never** listed in `securityRequirements`: the value is an
  ed25519 signature over the agent id, a timestamp, the method and the path, signed with the agent's
  own private key, so a generic A2A client holds nothing that can compute it. Declaring it is honest
  (it IS enforced); demanding it would advertise a requirement that client is guaranteed to fail.

#### 5.3.2 Caching (§8.6)

- `ETag` — a strong entity tag over the exact bytes served (SHA-256). The spec allows a tag derived
  from the `version` field; the content hash is the stricter choice, because the card also moves when
  the ROW behind it moves — a new capability, a webhook added, the auth posture flipped — and those
  are the updates a client most needs to see.
- `Cache-Control: private, max-age=60` — `private` and not `public`, because the card is served
  behind crier's own auth: a shared cache must never hand one client's authorized response to another.
- A conditional request (`If-None-Match`, including a weak `W/"…"` echo and `*`) is answered `304`
  with the refreshed validators and no body.

#### 5.3.3 What this route is not

- **It is not a registry query.** It serves only opted-in rows, and it never lists ids.
- **It is not a second delivery path.** Nothing about delivery, inboxes, webhooks, the mesh or the
  guard consults it; the card is read-derived from existing state.
- **It does not make crier A2A-first.** Zero A2A code runs with the switch off, and the two surfaces
  that exist with it on serve: a description of an agent that asked to be described (§5.3), and the
  JSON-RPC binding that carries a send into crier's own delivery path for an agent that opted in
  (§5.4).

### 5.4 The JSON-RPC binding (INT-A2A-003, SHIPPED)

`POST /a2a` serves the A2A JSON-RPC 2.0 binding (§9). It is the path the Agent Card's
`supportedInterfaces[].url` already advertises, so a client that discovered an agent through §5.3
needs no second address.

| | |
|---|---|
| Path | `/a2a` (`internal/a2a.JSONRPCBindingPath`) — the constant §5.3's card projection uses |
| Method | `POST` only; any other HTTP method is the router's own `405` |
| Request `Content-Type` | `application/a2a+json` (§14.1.1) or `application/json` (§9.1). An empty `Content-Type` is accepted; any OTHER explicit media type is refused with `415` and a body naming the two that are read |
| Response `Content-Type` | `application/a2a+json` for every JSON-RPC response — **including an error response** |
| HTTP status of a JSON-RPC answer | **`200`**, error or result alike. The JSON-RPC 2.0 binding is id-correlated: a status code would be a second, coarser answer to a question the protocol answers precisely. Only failures that never reach the JSON-RPC layer keep an HTTP status (405, 413, 415) |
| Request body limit | 4 MiB (`413` beyond it). This is a request-size ceiling, not a message-size policy — crier's delivery itself has no payload limit |
| Auth | **none of its own**: the route inherits the same middleware chain as every other authenticated route (§6.4), so with `CR_AUTH_TOKEN` set it needs the same Bearer header. The auth-exempt list is untouched |
| Methods served | `SendMessage` (§9.4.1), `SendStreamingMessage` (§9.4.2), the task lifecycle — `GetTask` (§9.4.3), `ListTasks` (§9.4.4), `CancelTask` (§9.4.5), `SubscribeToTask` (§9.4.6), all mapped onto the inbox entry, §5.5 — and the four push-notification configuration methods (§9.4.7, §5.6). Any other method (the extended card) answers `-32601 MethodNotFoundError` naming the row that lands it |
| Target | `params.tenant` — the registry id the card publishes as `AgentInterface.tenant`. It must name a row that opted in (§4.2) |

#### 5.4.1 The delivery is crier's own

An A2A send is translated into the body `POST /agents/{id}/inbox` already accepts, and that route's
**handler** is then called with it. There is no second delivery engine, and nothing about delivery
is re-implemented for A2A: the guard choke point, sender idempotency, the detection layer and its
containment, federation hold/retry, webhook push, the durable inbox, TTL resolution and the
lease/ack lifecycle are the shipped code, reached unchanged. The consequences are visible on the
wire and stated so nobody has to infer them:

- a refusal crier would give any other sender — `400 payload is required`, `403 GUARD_BLOCKED`,
  `403 AGENT_QUARANTINED`, `404 agent not found`, the `409` of an in-flight idempotent duplicate —
  reaches the A2A client as a JSON-RPC error carrying crier's own body verbatim (§5.4.4);
- the delivery is logged, counted and (with detection on) observed exactly as any other delivery,
  under the A2A request's own correlation id;
- the delivered payload is what a crier consumer reads through `GET /agents/{id}/inbox` — including
  that route's **pre-existing** base64 encoding of the stored payload (`docs/openapi.yaml`:
  "payload objects travel as base64-encoded strings; decode each entry's payload before use"). This
  row changes nothing about that contract.

#### 5.4.2 Request mapping — `params` ⇄ the crier deliver body

| A2A | crier deliver field | Rule |
|---|---|---|
| `params.tenant` | the route's `{id}` | Required. Must name an opted-in row (§4.2); otherwise `-32602` |
| `params.message` | the payload (§5.4.3) | Required, with `messageId`, `role` and at least one part (§4.1.4) |
| `message.messageId` | `idempotency_key` | A2A §3.3.1's deduplication rule made executable: a retry of the same message id answers with the FIRST delivery's accept — the same task id — instead of delivering the same work twice. Bounded at 128 characters, crier's own limit for the field |
| `message.contextId` | `session_id` | Both mean "the conversation this belongs to" (CR-FEAT-004) |
| `message.role` | carried in the payload | Must be `ROLE_USER` or `ROLE_AGENT`; `ROLE_UNSPECIFIED` or absent is `-32602` (the field is REQUIRED) |
| `message.parts[]` | `payload.parts[]` | §5.4.3 |
| `message.metadata`, `message.extensions`, `message.referenceTaskIds` | `payload.metadata` / `payload.extensions` / `payload.reference_task_ids` | Preserved verbatim; never interpreted |
| `message.taskId` | — | Refused with `-32004 UnsupportedOperationError`: continuing an existing crier task means addressing an inbox entry's lifecycle, which is INT-A2A-004's surface. Refusing is the honest answer; silently starting a NEW task under a client's task id would not be |
| `params.metadata.<key>` | the deliver knobs below | A **closed** set; an unknown key is `-32602` naming the accepted ones |
| `params.metadata.deliveryMode` | `delivery_mode` | `blocking` \| `async` \| `batch` — crier's own vocabulary, validated by crier's own rule |
| `params.metadata.timeoutMs` | `timeout_ms` | Integer; crier's existing bound (0..120000) applies |
| `params.metadata.ttlSeconds` | `ttl_seconds` | Integer; 0 means never expires (crier's existing tri-state) |
| `params.metadata.sender` | `sender` | Else the `X-Agent-ID` header (crier's own relay convention), else absent. An unattributed delivery cannot be held by federation, which is why the mapping exists at all |
| `params.metadata.requestId` | `request_id` | Correlation id, echoed by crier's reply contract |
| `params.metadata.threadId` | `thread_id` | Guard per-channel policy resolution (spec §4.2 of LLM-MESSAGE-GUARD.md) |
| `configuration.historyLength` | — | §3.2.4 semantics applied to the returned Task's `history` (§5.4.5) |
| `configuration.returnImmediately` | `delivery_mode: async` | See §5.4.6 |
| `configuration.acceptedOutputModes` | — | **Read and not honoured**: crier does not re-encode a payload, and a part's media type is the one the agent that produced it chose. §3.2.2 makes tailoring a SHOULD on the server, and this binding states that it declines it rather than pretending |
| `configuration.taskPushNotificationConfig` | — | **Refused, never silently accepted**: an inline push configuration on a send is not honoured, because push configuration is its own operation surface (§9.4.7, §5.6) and a send never writes configuration. The refusal is `PushNotificationNotSupportedError` when the target agent has no push channel (§3.3.4's capability answer) and `UnsupportedOperationError` when it has one, naming the four operations. §5.6.3 states why the code differs between the two positions. |
| `A2A-Version` header (§9.2) | — | Absent, `1`, `1.0` or `1.x` are served; anything else is `-32009 VersionNotSupportedError` naming the version this server serves (§3.6) |
| `A2A-Extensions` header (§9.2) | — | Read; crier declares no extension (`capabilities.extensions` is empty, §5.3), so nothing is negotiated and nothing is refused: the header asks, it does not require |

#### 5.4.3 The part model, both directions

An A2A `Part` (§4.1.6) is a **oneof** — `text` \| `raw` \| `url` \| `data` — plus `metadata`,
`filename` and `mediaType` (all camelCase on the wire, §5.5). It projects onto a crier message part
inside the delivered payload:

```json
{"parts": [
  {"type": "text", "text": "deploy the canary", "media_type": "text/plain",
   "alt": "operator instruction", "tags": ["deploy", "canary"], "caption": "step 1"},
  {"type": "file", "file": {"uri": "https://assets.example.com/canary.png"}, "filename": "canary.png"},
  {"type": "file", "file": {"bytes": "Y2FuYXJ5", "size": 6, "hash": "sha256:…"}},
  {"type": "data", "data": {"replicas": 2}}
]}
```

| A2A `Part` | crier part | Rule |
|---|---|---|
| `text` | `{"type":"text","text":…}` | The oneof member is **presence-exact**: `{"text":""}` is a text part with empty content, and a part with no member at all is refused |
| `data` | `{"type":"data","data":…}` | The existing opaque payload, untouched (object, array, string, number, boolean or `null`) |
| `url` | `{"type":"file","file":{"uri":…}}` | A **reference**: crier stores it and never dereferences it |
| `raw` | `{"type":"file","file":{"bytes":…,"size":N,"hash":"sha256:…"}}` | `bytes` is the base64 it arrived as; `size` and `hash` are derived from the DECODED bytes, so a consumer can verify what it was sent without this server ever holding a file handle. Base64 that does not decode is `-32602` |
| `metadata.alt` | `alt` | Lifted onto the part's own field; a non-string is `-32602` |
| `metadata.tags` | `tags` | Lifted; must be an array of strings |
| `metadata.caption` | `caption` | Lifted; must be a string |
| `metadata` (verbatim) | `metadata` | **Preserved whole** — including the three lifted members — so nothing is lost, and any other member rides through untouched |
| `filename`, `mediaType` | `filename`, `media_type` | Carried verbatim on every part type |

The projection is **lossless in both directions**, and that is asserted as such: an A2A message taken
off the wire, projected onto the delivered payload and projected back is the same JSON
(`internal/a2a/parts_test.go`). One deviation from the row's own parenthetical — "role + alt + tags +
reference-or-inline" — is **`role`**: A2A's `Role` is MESSAGE-scoped (§4.1.5), so it is carried once,
on the payload envelope, rather than duplicated onto every part where the two copies could disagree.
That is the same "no second source that could disagree" rule the whole option is held to.

#### 5.4.4 Response and error mapping

`SendMessage` answers with exactly one of a `Task` or a direct `Message` (§3.1.1):

| crier's delivery answered | A2A result | Why |
|---|---|---|
| `201` inbox accept | `{"task": …}` | The message is durable and unclaimed: `TASK_STATE_SUBMITTED` |
| `202` queued webhook accept (`async`/`batch`) | `{"task": …}` | Accepted for push; the durable inbox was deliberately bypassed |
| `202` federation hold (`{"status":"held"}`) | `{"task": …}` | `TASK_STATE_SUBMITTED` **with a status message that says the delivery is held at the source** and for how long — "queued for bounded retry" is a fact a client must not have to guess |
| `200` blocking webhook reply | `{"message": …}` | §3.1.1's direct Message for a simple interaction: the target answered inline, so there is no task to track. The reply body is projected through §5.4.3 (one of this binding's envelopes contributes its parts; anything else becomes one `data` part) and its `messageId` is the delivery id it answers |

`TASK_STATE_WORKING` is **never** asserted in a `SendMessage` answer: at that instant nothing can have
claimed the message, and a state this server cannot observe is exactly the kind of claim this option
is forbidden from inventing. The Task carries the crier facts A2A has no field for under
`metadata.crier` (`transport`, `delivery_mode`, `expires_at`, `held`, `max_hold_s`,
`idempotent_replay`, `guard`), namespaced so they can never be confused with an agent's own metadata.
Its `history` is the message that created it, bounded by `historyLength` (§3.2.4: `0` asks for none).

Errors use the JSON-RPC error object with the specification's own detail shapes (§9.5): an
`InvalidParams` refusal carries `google.rpc.BadRequest` with one `fieldViolation` naming the offending
parameter (`message.parts[1].raw`), an A2A-specific refusal carries `google.rpc.ErrorInfo` with
crier's reason, and **every** refusal raised by the delivery engine additionally carries crier's own
status and body verbatim as a `crier.DeliveryRefusal` object — a client sees exactly what crier said,
never a re-telling of it.

| code | name | raised when |
|---|---|---|
| `-32700` | `JSONParseError` | the body is not JSON, or stops mid-value; empty body; trailing data |
| `-32600` | `InvalidRequestError` | not a JSON-RPC 2.0 request object: wrong `jsonrpc`, missing `method`, missing/`null` id (this binding refuses notifications — every A2A operation answers a correlated response), an unknown envelope member, a batch array |
| `-32601` | `MethodNotFoundError` | any method but the ten this series serves — `SendMessage`, `SendStreamingMessage`, `GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask` and the four push-notification configuration operations (§5.6) — naming the row that lands the rest |
| `-32602` | `InvalidParamsError` | a parameter this binding cannot honour: the missing tenant, a tenant that names no opted-in agent, a required message or task member, a part whose oneof is not exactly one member, a wrongly-typed metadata member, an unknown request-metadata key, a `pageSize` outside 1..100, an unknown `status` name, an unparseable `statusTimestampAfter`, a `pageToken` this server did not mint — and crier's own `400` from the delivery engine, with its message |
| `-32603` | `InternalError` | crier could not complete the delivery (a `409` in-flight duplicate, a `5xx`), the delivery path was unreachable, or an inbox read failed for a reason that is not "the task is gone" |
| `-32001` | `TaskNotFoundError` | the delivery engine answered `404` (the target agent is not on this relay or in its federation); or a task-lifecycle operation named a task id crier holds NO record of — including an acknowledged one, whose entry the ack removed (§5.5.2) |
| `-32002` | `TaskNotCancelableError` | `CancelTask` aimed at a task that is already terminal (§3.1.5, §5.5.4) |
| `-32003` | `PushNotificationNotSupportedError` | the agent has no push channel: a push-notification configuration operation against a row with no webhook (§5.6.3, §3.3.4), and an inline `configuration.taskPushNotificationConfig` on a send to such an agent |
| `-32004` | `UnsupportedOperationError` | `message.taskId` (task continuation, §5.5.3); an inline `configuration.taskPushNotificationConfig` on a send to an agent that HAS a push channel (§5.6.3); a push configuration whose agent row carries a bring-your-own `custom_schema` (§5.6.4); a `SubscribeToTask` aimed at a terminal task (§9.4.6); or a task-lifecycle operation on a relay whose inbox backend cannot answer the read or the close it needs |
| `-32009` | `VersionNotSupportedError` | `A2A-Version` names a version this server does not serve |
| `-32050` | *(crier's implementation-defined server error)* | crier refused the request for a reason A2A has no name for: a delivery blocked by the target agent's guard (`403 GUARD_BLOCKED`), a quarantined agent (`403 AGENT_QUARANTINED`), or an agent-owned write crier's own signature gate refused (a push-notification configuration write made with `CR_REQUIRE_AGENT_SIG` on, §5.6.3). The reason rides in `ErrorInfo` and crier's own status and body in the `DeliveryRefusal` (delivery) / `ConfigRefusal` (configuration) detail |

`-32005 ContentTypeNotSupportedError` is deliberately **not** produced: crier's delivery payload is
opaque and the §5.4.3 projection is lossless for any media type, so claiming a media type is
unsupported would be false. `-32006 InvalidAgentResponseError`, `-32007
ExtendedAgentCardNotConfiguredError` and `-32008 ExtensionSupportRequiredError` belong to operations
this series does not serve (INT-A2A-005/006).

#### 5.4.5 `SendStreamingMessage` — the SSE adapter

`SendStreamingMessage` is `SendMessage` plus a stream. The response is `200` with
`Content-Type: text/event-stream`, and every frame is a JSON-RPC response carrying a `StreamResponse`
(§3.2.3, §9.4.2):

```
data: {"jsonrpc":"2.0","id":"req-1","result":{"task":{…}}}
data: {"jsonrpc":"2.0","id":"req-1","result":{"artifactUpdate":{…}}}
data: {"jsonrpc":"2.0","id":"req-1","result":{"statusUpdate":{…}}}
```

The stream carries **two** things, and says which is which:

1. **The task's own lifecycle**, read from crier's inbox — the mapping table's "task state ⇄ inbox
   entry + lease/ack". The stream opens with the Task, then reports each state change it OBSERVES:
   `TASK_STATE_SUBMITTED` (stored, unclaimed) → `TASK_STATE_WORKING` (leased, i.e. a consumer
   retrieved it) → `TASK_STATE_COMPLETED` (acknowledged: the entry was removed before its expiry) or
   `TASK_STATE_FAILED` (its TTL passed unacknowledged, so it can never be retrieved). It closes when
   the task reaches a terminal state, as §3.1.2 requires.
   - `TASK_STATE_CANCELED` is not asserted **by this adapter's disappearance inference**, and the
     reason is stated rather than implied: an entry leaves crier's inbox two ways — an ack, and the
     expiry sweep — and, since INT-A2A-004, a third: a `CancelTask` (which releases the lease and
     closes the entry, §5.5.4). The third one is indistinguishable from the first HERE, because both
     remove the entry and crier keeps no tombstone; the observation is a poll of the store, and the
     store cannot tell the two apart. The consequences are bounded and are stated in §5.5.4: a live
     stream open across a cancel reports the task as leaving the inbox without a failure record
     (`TASK_STATE_COMPLETED`), and the client that cancelled is the one party that knows otherwise —
     it has the `CancelTask` answer. A `SubscribeToTask`/`SendStreamingMessage` client that did not
     cancel cannot be misled about its own action, and `GetTask` — the surface a returning client uses
     — answers `TaskNotFoundError` for both, never an invented state (§5.5.2).
   - The state is read through an **optional read-only store capability** (`registry.InboxPeeker`),
     because the only other read of an inbox — `Retrieve` — LEASES what it returns, and an observer
     built on it would steal the message from the agent whose work it is watching. A backend that
     cannot answer (`RemoteStore`) degrades to "no lifecycle reported" and never to an invented
     state.
   - Observation is a POLL (default 250 ms, one read per open stream): crier's inbox publishes no
     lease/ack notification — the long-poll notifier fires on DELIVERY — so a poll is the honest
     mechanism, and it is a read, never a claim.
2. **The relay subscription, as SSE** — the row's own mapping. Frames published to the task's relay
   topic (`a2a.task.<task id>`, published through the ordinary `POST /relay/publish`) are forwarded
   as `TaskArtifactUpdateEvent`s whose artifact parts are the same §5.4.3 projection, with the
   source topic recorded under `metadata.crier.relay_topic`. Nothing about crier's relay changes: the
   stream calls the same exported `Relay.Subscribe` the WebSocket handler uses, and a WebSocket
   subscriber of the same topic sees exactly the frames it always saw.

Three properties are stated because a stream that hides them would be worse than no stream:

- **The budget.** A task may stay non-terminal for its whole TTL, so a stream cannot stay open
  forever. It closes after `defaultStreamBudget` (120 s — the same ceiling crier already applies to
  its other request-level waits), and the closing frame is a `statusUpdate` in the CURRENT state
  saying the budget elapsed and that the task is still open. Reaching the budget is **not** a terminal
  state and is never reported as one.
- **A target that answered inline** (a blocking webhook) gives the message-only stream of §3.1.2
  pattern 1: exactly one `Message` frame, then the stream closes. There is no task to track.
- **A request that cannot be delivered** (an unknown tenant, an invalid part) is answered with the
  ordinary JSON-RPC error response and `application/a2a+json` — never as an empty stream, because the
  stream's content type is a promise about the body.

#### 5.4.6 The one deviation: §3.2.2's blocking default

§3.2.2 makes a `SendMessage` that does not set `returnImmediately` BLOCK until the task reaches a
terminal or interrupted state. crier cannot honour that literally, and this document says so instead
of approximating it silently:

- For the durable-inbox transport the operation IS complete when the message is durable — the
  terminal state is produced later by the receiving agent's ack, which may be minutes or hours away.
  Holding every send open until a stranger acks would make the default path unusable, and returning a
  fabricated terminal state would be a lie.
- What the binding does instead: `returnImmediately: true` selects crier's own request-level async
  override, and `SendStreamingMessage` selects it unconditionally (a client that asked for a stream
  asked for updates INSTEAD of one blocking answer). A target whose webhook is configured `blocking`
  still answers with a direct Message when the client did not ask otherwise — which is the
  spec-sanctioned simple-interaction shape, and is what a crier blocking delivery really is.
- A client that wants a terminal state polls (`GetTask`, INT-A2A-004) or watches the stream
  (§5.4.5), which is A2A's own model for a task it did not block on.

#### 5.4.7 What this row does not do

- **No A2A method left unserved but the extended card.** INT-A2A-004 landed the task lifecycle
  (§5.5) and INT-A2A-005 the push-notification configuration operations (§5.6), so `SendMessage`,
  `SendStreamingMessage`, `GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask` and the four
  `TaskPushNotificationConfig` methods are ALL served by this binding. `GetExtendedAgentCard` is the
  one method that still answers `-32601` (INT-A2A-006).
- **No restart resume.** A stream is a live view: an SSE stream does not survive the process, and a
  task's state is re-read from the inbox on each new stream. Nothing is buffered for a client that
  reconnects, because nothing in crier replays a stream.
- **No second delivery engine, no second task store, no second payload model.** All three are the
  existing ones, reached through the mapping above.
- **No change to the relay.** The WebSocket path is byte-for-byte what it was; the SSE adapter reads
  through the same subscription primitive, and the existing relay tests are untouched.
- **No new auth requirement anywhere**, and no existing field changes meaning (§6).

### 5.5 The task lifecycle (INT-A2A-004, SHIPPED)

The four task operations map onto **the inbox entry crier already keeps**. There is no task store, no
task index, no A2A column on any crier table and no A2A field on any crier route: a task IS a
delivery, its id IS crier's message id, and every state this section names is derived from the entry's
own lifecycle (stored → leased → acked / expired / closed) plus the one durable record the expiry
sweep writes.

The binding decision of §2 applies unchanged (JSON-RPC 2.0, `POST /a2a`, PascalCase method names), and
so does the gate: each operation names its target with `params.tenant` (the registry id the Agent Card
publishes as `AgentInterface.tenant`) and is reachable only while `CR_A2A_ENABLED` is set **and** that
row opted in. An id that is not an opted-in row gets the send operations' own answer (§5.4.4).

#### 5.5.1 The operations

| Method | §  | Request `params` | Result |
|---|---|---|---|
| `GetTask` | §9.4.3, §3.1.3 | `tenant` (required here: one origin hosts many agents), `id` (REQUIRED — the task's id), `historyLength` (optional int ≥ 0, §3.2.4) | the `Task` |
| `ListTasks` | §9.4.4, §3.1.4 | `tenant`, `contextId`, `status` (a `TaskState` name), `pageSize` (1..100, default 50), `pageToken` | `{tasks, nextPageToken, pageSize, totalSize}` |
| `CancelTask` | §9.4.5, §3.1.5 | `tenant`, `id`, `metadata` (read and carried, never interpreted) | the `Task` in `TASK_STATE_CANCELED` |
| `SubscribeToTask` | §9.4.6, §3.1.6 | `tenant`, `id` | `200` + `text/event-stream`, the same SSE contract as §5.4.5 |

Everything the §5.4 binding states about the transport applies to these methods too: `POST /a2a` only,
`application/a2a+json` (or `application/json`) accepted and answered, **HTTP `200` for every JSON-RPC
answer including an error**, the 4 MiB body ceiling, the `A2A-Version` header check, and no auth
requirement of its own (the route inherits the same middleware chain as every other authenticated
route — the auth-exempt list is untouched). Nothing here adds a route: the surface is the one
`POST /a2a` that INT-A2A-003 registered (§5.2).

Details that are decisions rather than restatements:

- **Every filter of `ListTasks` is honoured or refused, never ignored** (DF-CRIER-180's rule for a
  request-level parameter): a `pageSize` outside 1..100, a `status` that is not one of §4.1.3's names,
  a `statusTimestampAfter` that is not an ISO 8601 timestamp and a `pageToken` this server did not mint
  are each `-32602`, naming the field.
- **`pageToken` is an opaque KEYSET cursor**, not an offset: it carries the position of the last task
  of the previous page in the listing's own order (`statusTimestamp` descending, `task.id` descending).
  That is what keeps a page stable while messages are being claimed and acknowledged underneath it —
  an offset would skip or repeat tasks as the queue shrinks.
- **`pageSize` in the response is the size ACTUALLY used**, so a page that ran out of items is not
  reported as a full one, and `totalSize` is the number of matching tasks BEFORE pagination.
  `nextPageToken` is always present and empty on the final page (§3.1.4).
- **The listing is of the tasks crier's store holds** — the entries in that inbox, resolved exactly as
  `GetTask` resolves them, ordered by §3.1.4's status-timestamp rule. A message the expiry sweep
  removed is NOT listed (its task is no longer in the queue) even though `GetTask` still answers
  `FAILED` for it from the sweep's record (§5.5.2).
- **`history` is the creating message** — crier stores one payload per task, so the history is bounded
  at one message whatever `historyLength` asks for: absent means that message, `0` means none, `n > 0`
  means at most `n` of the messages crier holds. The message is projected from the delivery payload
  when that payload is one of this binding's own envelopes (§5.4.3); a task started by a non-A2A
  sender has whatever payload it was given, and no A2A message can be reconstructed from it.
- **`artifacts` is omitted entirely** unless `includeArtifacts` is true, in which case it is present as
  an empty array: crier keeps no artifact for a task (a task's outputs are relay frames an observing
  stream forwards, §5.4.5 — never stored rows), and §3.1.4's conditional member is answered rather
  than dropped.
- **`SubscribeToTask` reuses the §5.4.5 adapter** — the same lifecycle observation through the
  read-only peek, the same relay subscription written as `text/event-stream`, the same 120-second
  stream budget. The stream opens with the `Task` (§9.4.6: "The operation MUST return a Task object as
  the first event"), reports the states crier's store shows, and closes when the task reaches a
  terminal state. A task in a terminal state is refused with `-32004 UnsupportedOperationError`
  (there are no updates left to stream) **as a JSON-RPC answer, never as an empty stream** — the
  stream's content type is a promise about its body.

#### 5.5.2 The state mapping — the one table this row is held to

**This is where an interop adapter most easily lies about state**, so the mapping is stated as the
contract it is, is asserted one row at a time in `internal/a2a/task_test.go`, and is named on the wire:
every task a read returns carries `metadata.crier.state_basis` — the crier record its state was read
from.

| crier's record | A2A `TaskState` | `state_basis` | read by |
|---|---|---|---|
| an inbox entry, unacked, unleased, TTL not elapsed | `TASK_STATE_SUBMITTED` | `inbox-entry-unleased` | `Peek` |
| an inbox entry, unacked, **leased** (`lease_id` set), TTL not elapsed | `TASK_STATE_WORKING` | `inbox-entry-leased` | `Peek` |
| an inbox entry, unacked, **past its TTL** (the sweep has not run yet) | `TASK_STATE_FAILED` | `inbox-entry-ttl-elapsed-unacked` | `Peek` |
| an inbox entry the store reports as **closed** (`acked`) | `TASK_STATE_COMPLETED` | `inbox-entry-closed` | `Peek` |
| **no entry**, and the expiry sweep recorded this message as a dead letter | `TASK_STATE_FAILED` | `dead-letter-recorded` | `DeadLetterLookup` |
| **no entry**, and no failure record | **`-32001 TaskNotFoundError`** | *(no state)* | — |
| the `CancelTask` this request performed | `TASK_STATE_CANCELED` | `canceled-by-request` | the operation's own answer |

Four properties of that table are decisions, and each is stated so a client never has to infer it:

1. **`TASK_STATE_COMPLETED` is only asserted from a closed row, and both shipped backends delete the
   row when it is acknowledged.** `Ack` is a hard removal (DF-CRIER-32, "permanently removes") and
   crier keeps no tombstone, so **after an ack there is nothing left to read**. The honest answer for
   such a task id is therefore `TaskNotFoundError`, which is the specification's own definition of
   that error — "The specified task ID does not correspond to an existing or accessible task. It might
   be invalid, expired, or **already completed and purged**" (§3.3.2). Reporting `TASK_STATE_COMPLETED`
   there would be a claim crier cannot support: a removed id is indistinguishable from one that was
   transferred to another inbox, purged with its agent, or never delivered at all. The `COMPLETED`
   transition is observable where it happens — on an open stream (§5.4.5) and in the ack's own `204`;
   a client that needs a terminal state after the fact must be watching, which is A2A's own model for
   a task it did not block on. The `inbox-entry-closed` row exists so that a backend which ever
   retains a closed entry is mapped honestly rather than reported as missing.
2. **An entry PAST ITS TTL is `FAILED` even before the sweep removes it.** Every consumption path
   skips an expired row, so the message can never be retrieved and the work cannot complete; reporting
   the `SUBMITTED` state the row still stores would tell a client to keep waiting for a message nobody
   can claim. The sweep records the same outcome durably a moment later, which is why the row below it
   gives the same state from a different record.
3. **`FAILED` outlives the entry.** Crier's expiry sweep writes a dead letter and a receipt
   (CR-FEAT-025, "failure is a message, not a mystery"), so a swept task is still reported `FAILED`,
   with `metadata.crier.dead_lettered_at` naming when the sweep recorded it. This is the only terminal
   state that is re-readable after the fact, and it is re-readable because crier chose to record it —
   not because this adapter reconstructed it.
4. **A never-expiring entry never expires.** `ttl_seconds: 0` stores the zero `ExpiresAt` (DF-CRIER-37),
   and the mapping reads that as "no expiry", never as "expired long ago".

`Task.status.timestamp` is crier's own timestamp for the state it reports, so §3.1.4's ordering is
crier's ordering: a lease stamps the instant it was taken, an expiry stamps the instant that elapsed,
a swept record stamps when the sweep recorded it, and an unclaimed message is as old as its delivery.
`Task.status.message` explains the two states whose REASON is not in the state's name (`FAILED` and
`CANCELED`); `SUBMITTED` and `WORKING` carry none, because the state says everything crier knows.

#### 5.5.3 A message that names a task

A `SendMessage` (or `SendStreamingMessage`) whose `message.taskId` names a task is resolved against
crier's own store **before anything is delivered**, and answered with the specification's own case for
what it names (§3.4.2, §3.1.1):

| what the task id names | answer | why |
|---|---|---|
| nothing crier holds a record of | `-32001 TaskNotFoundError` | §3.4.2: "Agents **MUST** return a `TaskNotFoundError` if the provided `taskId` does not correspond to an existing task". After an ack this includes the acknowledged task itself (§5.5.2). |
| a task in a TERMINAL state (`COMPLETED`, `FAILED`, `CANCELED`, `REJECTED`) | `-32004 UnsupportedOperationError`, naming the state | §3.1.1/§3.1.2 make this mandatory: "Messages sent to Tasks that are in a terminal state cannot accept further messages." |
| an OPEN task (`SUBMITTED`, `WORKING`) | `-32004 UnsupportedOperationError`, with the reason stated | crier cannot represent a continuation: a task IS its inbox entry (one message), and no primitive appends a message to an existing entry. Accepting it would deliver a message that lands as a **separate** task under a separate id — which is not a continuation, and the client would not recognise it. |

None of the three is a `200` and none of them delivers anything: a refused message must not reach the
guard, the inbox or a webhook, and `cmd/server/a2a_lifecycle_test.go` proves it by measuring the
agent's own queue depth across the refusal.

#### 5.5.4 `CancelTask` — release the lease, close the entry

`CancelTask` is performed with crier's OWN lease/close semantics, not by inventing a task state:

- the lease on the message is **released** and the entry is **closed** — removed from the inbox,
  whatever its lease state — so after the answer nothing can retrieve or acknowledge the message;
- the answer is the `Task` in `TASK_STATE_CANCELED` with `state_basis: canceled-by-request`, i.e. the
  state the operation itself produced, as §3.1.5 requires ("Updated `Task` with cancellation status");
- a cancel reaches a **queued** message as well as a leased one. That is why the store capability this
  row added is a per-entry close rather than an `Ack` wrapper: `Ack` requires the caller to hold the
  lease, and an A2A client never holds one;
- a cancel aimed at a **terminal** task is `-32002 TaskNotCancelableError` (§3.1.5) — never a `200`
  reporting a cancellation that did not happen;
- a cancel aimed at a task with **no record** is `-32001 TaskNotFoundError`. A **duplicate** cancel
  therefore also answers `-32001`, which §3.1.5 explicitly allows: "A duplicate cancellation request
  MAY return `TaskNotFoundError` if the task has already been canceled and purged."

**The one residual ambiguity, stated rather than hidden.** Crier keeps no tombstone, so a closed entry
is indistinguishable from an acknowledged one *by looking at the store*. A stream that is already open
when a cancel happens therefore reports the task as leaving the inbox without a failure record
(`TASK_STATE_COMPLETED`, §5.4.5), and `GetTask` after a cancel answers `TaskNotFoundError`. Neither is
a fabrication — both are the same honest "this task is no longer in the queue" — and the party that is
never misled is the one that cancelled, because it holds the `CancelTask` answer. Closing this gap
would need a durable terminal record crier does not keep today; it is deliberately NOT invented here,
because a second store is exactly what this row forbids (§1.1).

#### 5.5.5 What this row added to crier's store (and what it did not)

Three OPTIONAL store capabilities, in the family `InboxPeeker` opened (INT-A2A-003), with no schema
change, no migration and no change to any existing method:

| capability | method | contract |
|---|---|---|
| `registry.InboxLister` | `PeekInbox(agentID)` | a READ-ONLY, LEASE-FREE snapshot of one agent's inbox, in delivery order. It takes no lock on any entry and mints no lease, because the only other multi-entry read — `Retrieve` — leases what it returns and would steal the messages a listing is describing. `ErrAgentNotFound` for an unknown agent; an empty slice (no error) for a registered agent with nothing queued. |
| `registry.InboxCloser` | `CloseEntry(agentID, messageID)` | the removal `Ack` performs WITHOUT its lease precondition: the entry is deleted whatever its lease state, and any outstanding lease goes with it. It writes no dead letter and sends no receipt — a closed entry is a message the caller decided would not be worked, not one crier gave up on. |
| `registry.DeadLetterLookup` | `LookupDeadLetter(agentID, messageID)` | whether the expiry sweep recorded THIS message id, in one lookup. It is not `ListDeadLetters` (a bounded, newest-first page of an archive, which cannot answer "was this one recorded?" once the record is older than a page — and answering "no" from a window that did not reach far enough would turn a recorded failure into an invented not-found). |

All three are implemented by both shipped backends and are OPTIONAL like their predecessors: the
remote proxy does not implement them, and a task-lifecycle operation on such a backend is refused with
`-32004 UnsupportedOperationError` naming the capability rather than answered with an invented state.

**No existing crier surface changed to make this possible.** The inbox row's columns, `Ack`,
`Retrieve`, `PurgeExpired`, the dead-letter destination, the long-poll notifier, the guard, delivery,
federation, the webhook driver, the mesh, the MCP surface, `docs/openapi.yaml` and its generated copy
and the docs-claims gate are all exactly what they were: nothing in any of them consults the A2A switch
or an agent's `a2a` block (§1.1, §6).

#### 5.5.6 What this row does not do

- **No second store, no task index, no tombstone.** A task is the inbox entry; what crier does not
  record is not reported (§5.5.2).
- **No task continuation.** A message that names an open task is refused (§5.5.3), because delivering
  it would create a second task rather than continue the named one.
- **No artifacts.** `Task.artifacts` is empty (and omitted unless asked for); crier stores no artifact
  for a task.
- **No push-notification configs, and no extended card.** Push configuration is INT-A2A-005's surface
  (§5.6); the extended card (INT-A2A-006) is the one remaining A2A surface and still answers `-32601`.
- **No change to the relay's WebSocket path, and none to the inbox's own lifecycle semantics** — a
  cancelled message is removed; nothing about ack, lease or TTL moved.

### 5.6 The push-notification configuration operations (INT-A2A-005, SHIPPED)

§4.3 and §9.4.7's `TaskPushNotificationConfig` operations are served by the SAME binding as every other
A2A operation: `POST /a2a`, as four JSON-RPC methods. **No new route is registered** — crier
implements exactly ONE binding (§2: JSON-RPC 2.0 over HTTP + SSE), so there is no
`/tasks/{id}/pushNotificationConfigs…` REST surface and adding one would be a third A2A route this
option does not have.

| | |
|---|---|
| Methods | `CreateTaskPushNotificationConfig` (§3.1.7), `GetTaskPushNotificationConfig` (§3.1.8), `ListTaskPushNotificationConfigs` (§3.1.9), `DeleteTaskPushNotificationConfig` (§3.1.10) — the §9.4.7 names, verbatim |
| Endpoint | `POST /a2a`, under §5.4's content-type, body limit, auth and `A2A-Version` rules |
| Target | `params.tenant` — the registry id the card publishes as `AgentInterface.tenant`, and it must name an opted-in row (§4.2). Required: one crier origin hosts many agents |
| The configuration | **crier's existing per-agent webhook config.** There is no second configuration store: the A2A config IS `webhook.Config` (the mapping table's row, §3), which is also the thing `capabilities.pushNotifications` in the Agent Card reports (§5.3) |
| Id | **Derived, never stored**: `wh-<sha256(tenant ‖ 0x00 ‖ url)[:8] hex>` (`a2a.PushConfigID`). crier has one push channel per agent, so there is nothing to enumerate |
| The payload | §4.3.3's `StreamResponse` envelope, `Content-Type: application/a2a+json`, sent by crier's EXISTING webhook driver (§5.6.5) |

#### 5.6.1 Why it is a view rather than a second surface

crier could already push to an agent: the webhook config is the push channel, with retries, a timeout,
a batch mode, a delivery mode, a schema and an optional named secret. INT-A2A-005 exposes that config
through the operations A2A defines, and **reuses crier's own write path to do it**: a create or a
delete is the body `PATCH /agents/{id}` already accepts, handed to that route's OWN handler
(`registry.Handler.HandleUpdateAgent` — the function the route is registered with), exactly as
`SendMessage` hands a delivery to `HandleDeliver` (§5.4.1). The consequences are stated rather than
discovered:

- **No second delivery engine, and no second payload model.** The notification is an ordinary webhook
  delivery: the guard, the queue, the retry budget, the circuit breaker, the batch coalescer and the
  signing/authentication headers are the shipped code, reached unchanged.
- **No second write path, and no weakened gate.** The strict `webhook` member decode,
  `webhook.Config.Validate`, the store update and the **agent-owned signature gate** (`requireAgent`,
  enforced when `CR_REQUIRE_AGENT_SIG` is on) are crier's own. §5.6.3 states what that means for a
  write made over A2A.
- **No changed field meaning.** The operation writes `webhook.url`, the notification schema
  (`custom_schema`) and — when the request asks — `webhook.auth_type` / `auth_value_ref`. Every other
  field of that config (retries, timeout_ms, batch, delivery_mode, schema_template) is PRESERVED:
  the A2A object has no field for it, and resetting what a request cannot express would be silent
  data loss of operator configuration (the DF-CRIER-279 class).
- **Reads never write.** `Get`/`List` project the row and change nothing.

#### 5.6.2 The object mapping, field by field

| A2A `TaskPushNotificationConfig` (§4.3.1) | crier | Rule |
|---|---|---|
| `tenant` | the registry id | Echoed. Required by this binding, and identical to `params.tenant` |
| `id` | the derived id (§5.6) | Returned by create, accepted by get/delete. A client-supplied `id` that is not the derived one is `-32602` naming the derived one: there is no second configuration for it to address |
| `taskId` | — (client addressing) | **Echoed, not verified.** crier's push configuration is per AGENT, so there is no per-task record to look up — and a pushed delivery creates no inbox entry, so "does this task exist" has no honest answer here (see §5.6.4). The response states the task the client addressed, and nothing more |
| `url` (REQUIRED) | `webhook.url` | Written on create — this is the endpoint notifications go to, and it is the same field that routes an ordinary delivery to that agent |
| `token` | — | **Refused** (`-32602`, "token"): crier's webhook config has no per-task notification-token field, and accepting one would promise a value that no delivery ever sends |
| `authentication.scheme` (§4.3.2) | `webhook.auth_type` | `none` → `auth_type: none` (the named secret is cleared: nothing would use it); `Bearer` → `auth_type: bearer`, which requires the row to already name a secret; anything else is `-32602` naming the two schemes crier's driver can emit. Scheme comparison is case-insensitive (RFC 9110 §11.1) |
| `authentication.credentials` | `webhook.auth_value_ref` (a NAME, never a value) | **Refused** (`-32602`) and never returned. crier stores no credential in a registry row: it references a secret by name and resolves it at send time. A credential posted here would either be stored where secrets do not belong or dropped while the client believed it was sent — so it is refused, loudly |
| the list response's `configs[]` | the one configuration | At most ONE entry: one push channel per agent. `nextPageToken` is never returned |
| the list request's `pageToken` | — | **Refused** (`-32602`): crier never issues a page token, so one a client sends did not come from this server. `pageSize` is accepted (and must be ≥ 1); it bounds a list that has at most one entry |
| the delete result | `{"deleted": true, "id": …, "tenant": …}` | §3.1.10 leaves the confirmation implementation-defined |

#### 5.6.3 The capability answer is a MUST, and the write gate is crier's own

**Capability (§3.3.4).** If `AgentCard.capabilities.pushNotifications` is false — which in crier means
*the row carries no webhook* — then all four operations MUST answer `-32003
PushNotificationNotSupportedError`, and they do: the error names the agent, the capability and the fix,
and carries `google.rpc.ErrorInfo` with reason `PUSH_NOTIFICATION_NOT_SUPPORTED`. There is no silent
success and no empty-but-200 list: an agent with no push channel has no configuration to describe, and
saying otherwise would report a channel that does not exist. This is the same honesty DF-CRIER-279
forced into the templating path — a green answer over a capability that is absent is worse than an
error, because the client codes against it.

**The write gate.** Create and delete reach crier's agent-owned update route. With
`CR_REQUIRE_AGENT_SIG` in force — crier's secure DEFAULT — that route requires the TARGET agent's
ed25519 signature, and an A2A client cannot compute it (the signature covers the method, the path and
a timestamp of the PATCH route; §5.3.1 already states that a generic A2A client holds nothing that can
produce `agentSignature`). The operations therefore answer crier's own `401`/`403` with its reason
verbatim, as `-32050` plus a `crier.ConfigRefusal` detail carrying the status and body crier wrote, and
**the row is left untouched**. This surface does not weaken that gate, and it does not invent a second
authorization for the same field: an A2A client that is allowed to write must be a deployment whose
posture allows it (signature enforcement off — crier's documented trusted-single-user posture) or must
have the agent itself perform the update. Reads (`Get`/`List`) are unaffected: they ask the relay a
question and rewrite nothing.

**The inline configuration on a send.** `configuration.taskPushNotificationConfig` stays REFUSED. A
send delivers; it never writes configuration. The code is the capability answer where that is true —
`-32003` for an agent with no push channel — and `-32004 UnsupportedOperationError` naming the four
operations for an agent that HAS one, because `-32003` would be a false statement about an agent whose
card says `pushNotifications: true`. Either way the request is refused rather than silently accepted,
and the row is unchanged.

#### 5.6.4 What the operations refuse, and why each refusal is loud

| Request | Answer | Why |
|---|---|---|
| the agent has no webhook | `-32003` | §3.3.4 capability MUST (§5.6.3) |
| `url` absent or blank | `-32602` (`url`) | §4.3.1 marks it REQUIRED, and crier's own `Validate` adds the `http(s)://` rule |
| a `url` crier's own route would refuse (not http(s)) | `-32602` with **crier's own message** | The write is validated by crier's route, so the A2A answer quotes it (`crier.ConfigRefusal`) |
| `id` that is not the derived id | `-32602` (`id`) | one configuration per agent (§5.6) |
| `token` | `-32602` (`token`) | no such field in crier's config, and no delivery would send it |
| `authentication.credentials` | `-32602` | crier references a secret by name; it never stores a credential (§5.6.2) |
| `authentication.scheme` that is neither `none` nor `Bearer` | `-32602` | those are the schemes crier's driver emits |
| `Bearer` with no secret named on the row | `-32602` | otherwise the configuration would claim authentication it does not have |
| an unknown member anywhere in `params` (including inside `authentication`) | `-32602` naming the member and the accepted set | the strict-decode discipline the send params and the `a2a` registration block are held to |
| the row already carries a bring-your-own `custom_schema` | `-32004`, reason `PUSH_CONFIG_CUSTOM_SCHEMA_SET` | crier resolves a `custom_schema` BEFORE any named template, so a configuration with one cannot also promise §4.3.3's payload shape. The alternative — overwriting the operator's schema without saying so — is exactly the silent loss this option refuses. The fix is named: clear it on the agent row, or keep it and configure the push channel there |
| `pageToken` on list; `taskId` missing on get/list/delete; `id` missing on get/delete | `-32602` naming the field | the required members of §3.1.8–§3.1.10, and a page token this server never issued |

**Not checked, and stated so:** the operations do NOT verify that `taskId` names an existing task. In
crier a push-configured agent's deliveries are pushed (transport `webhook`), which creates NO inbox
entry, so "the task id does not exist in the inbox" is indistinguishable from "the task was pushed" —
raising `TaskNotFoundError` on that basis would be inventing a fact. `TaskNotFoundError` IS raised
where crier really can answer: a configuration id that names no configuration (§3.1.8/§3.1.10's own
meaning for that error).

#### 5.6.5 The notification payload (§4.3.3)

> When a task update occurs, the agent sends an HTTP POST request to the configured webhook URL. The
> payload uses the same `StreamResponse` format as streaming operations.

The notification is the payload shape §4.3.3 fixes, delivered by crier's existing webhook driver. It is
expressed in crier's OWN bring-your-own-schema mechanism, so nothing about the driver changes: a create
installs the config's `custom_schema` (which crier's resolution rule already prefers over a named
template) with

- `request_shape.method` = `POST`,
- `request_shape.headers["Content-Type"]` = `application/a2a+json` (§14.1.1's media type — the same one
  a stream frame is served with), and
- `request_shape.body` = the template below, rendered by the driver from the envelope of the delivery it
  is sending.

```json
{"task": {
  "id": "{{crier.message_id}}",
  "contextId": "{{crier.session_id|default:}}",
  "status": {"state": "TASK_STATE_SUBMITTED"},
  "metadata": {"crier": {
    "transport": "webhook", "kind": "…", "delivery_mode": "…",
    "sender": "…", "target": "…", "thread_id": "…",
    "request_id": "…", "namespace": "…"}}}}
```

What that means, precisely:

- **The envelope is a `StreamResponse` with exactly one member set** — here `task`, the same shape a
  `SendMessage` answer carries for an accepted push delivery (§5.4.4's `202` row). The template is
  valid JSON before rendering and after, so the body cannot arrive unparseable.
- **`task.id` is the delivery's message id** — crier's one identifier for the work, the same value
  `SendMessage` returned as the Task id. `TASK_STATE_SUBMITTED` is what that state means here: the
  message is durable/queued for the target and nothing has claimed it yet. No state this server cannot
  observe is asserted.
- **The task's content is NOT duplicated into the notification.** crier's payload is opaque, a
  non-object payload does not survive crier's template context, and a notification that silently
  dropped the body would be worse than one that does not claim to carry it. The message itself is read
  where it already lives (`GET /agents/{id}/inbox`, §5.4.1) — a push notification is a notification,
  not a second payload store.
- **Every optional field carries an explicit `|default:`** because crier's templating FAILS a delivery
  loudly on a placeholder the context cannot fill (DF-CRIER-279): a delivery with no session id renders
  `"contextId": ""` rather than failing. `crier.message_id` is the one always-present field.
- **The delivery MODE is the agent's existing one.** A `blocking` config makes the delivery
  synchronous (the endpoint's reply is the sender's reply — crier's own contract, unchanged), an
  `async` config queues it, a `batch` config coalesces several deliveries into one POST. The
  notification is one `StreamResponse` per POST; under `batch` the POST carries the batch's own
  delivery id, and `metadata.crier.kind` states `batch` — the operation does not change a config's
  delivery semantics, and the spec says so rather than implying a per-message guarantee it cannot make.
- **crier's own headers travel with it** (`X-Crier-Event`, `X-Crier-Target`, `X-Crier-Session`,
  `X-Crier-Retry`, the guard headers, and `X-Crier-Signature` when `CR_WEBHOOK_SECRET` is set). They are
  additive to the A2A payload, and they are how the receiving side can tell a retry from a first
  attempt.

#### 5.6.6 What this row deliberately does not do

- **It does not build a second push mechanism.** No new route, no new store, no new delivery engine, no
  new queue. The A2A configuration is a view over `webhook.Config` and rides the shipped driver.
- **It does not give an agent a push channel it does not have.** An agent with no webhook has no push
  notifications, and every operation says so.
- **It does not store a credential.** `authentication.credentials` is refused; the secret stays a named
  reference resolved out of band.
- **It does not implement REST (`/tasks/{id}/pushNotificationConfigs…`)** — §11's binding is MAY and
  not built (§2). The same four operations are served as JSON-RPC methods, which is the binding crier
  implements.
- **It does not implement the extended Agent Card**, and no `extendedAgentCard` capability is claimed.
- **It does not make A2A first-class.** Every bit of it is behind `CR_A2A_ENABLED` and the per-agent
  opt-in, and with the switch off the route does not exist (§5.1).

## 6. Non-regression contract

"Behaves exactly as today" is asserted over the following, with the switch **off** and with it **on**
(and it must hold identically in both positions):

1. **Status codes and body shapes of the existing read routes** — `GET /health`, `GET /version`,
   `GET /status`, `GET /agents` (and `GET /agents/{id}`): unchanged status, unchanged JSON key
   set/order-relevant shape. `GET /status`'s key set is pinned by its own test, so the option adds
   no key there — the switch is deliberately absent from `/status`.
2. **The registered route table** — with the switch OFF, no new `HandleFunc`/`Handle` line vs. the
   pre-change tree; with it ON, exactly the two A2A routes §5.2 marks SHIPPED, and nothing else
   (§5.1). Every pre-existing path answers identically in both positions.
3. **The agent wire shape** — an agent registered without the `a2a` block serializes exactly as
   before (no new key); an agent registered with it gains exactly one optional key, and its core
   fields (`id`, `public_key`, `capabilities`, `status`, `registered_at`, `last_seen`) are unchanged
   in name, type and meaning. Round-trip is asserted both ways through `POST /agents` → `GET
   /agents/{id}`. The Agent Card is a READ of that row and adds no field to it.
4. **Auth** — the exempt-path list, the bearer requirement and the ed25519 agent-signature
   requirement are untouched: no existing route gains an A2A auth requirement, the A2A route
   inherits the same middleware chain as every other authenticated route (it is not added to the
   exempt list), and neither the `a2a` block nor the card requires signing to be relaxed.
5. **Strict-decode discipline** — the `a2a` block is rejected (400, key named) on a malformed
   member rather than silently ignored, and a rejected registration leaves the registry untouched.
6. **Storage** — the durable schema change is one additive nullable column
   (`ALTER TABLE agents ADD COLUMN a2a JSONB NULL`); existing rows read back exactly as before, and
   an agent with no block stores SQL `NULL`, not an empty object. The Agent Card row adds NO
   migration: it reads the columns that already exist (`capabilities`, `webhook`, `a2a`).
7. **Everything else** — relay, mesh, federation, guard, webhook delivery, MCP, openapi
   (`docs/openapi.yaml` + its generated `cmd/server/openapi.yaml` copy) and the docs-claims gate are
   unchanged, because no route, field or default they describe moved. The A2A route is documented as
   an OPT-IN surface in `README.md` and here — like `/metrics` and `/debug/pprof/*`, the other
   opt-in paths — rather than in the default-path OpenAPI document.
8. **The push-notification configuration operations (INT-A2A-005)** add no route and no field, so
   clause 7's surfaces stay as they are, and they are held to four further properties:
   - the notification is delivered by the EXISTING webhook driver — a webhook test that passes today
     passes unchanged, because nothing in `internal/webhook` moved;
   - the write goes through `PATCH /agents/{id}`'s own handler, so crier's webhook validation, strict
     member decode and agent-owned signature gate apply to it exactly as they do to a direct PATCH;
   - a write that is REFUSED leaves the row byte-identical (asserted per refusal in
     `cmd/server/a2apush_test.go`), so a refused A2A operation can never half-configure an agent;
   - every field of a webhook config the A2A object has no member for (retries, timeout_ms, batch,
     delivery_mode, schema_template) is PRESERVED by the write, so an existing agent's delivery
     behaviour is not changed as a side effect of configuring a push channel.

The gate for all of this is the non-regression test suite shipped with INT-A2A-001 — still the gate,
amended only where a row's route moved the surface it describes:
`internal/registry/a2a_optin_test.go` (wire shape, strict decode, round-trip) and
`cmd/server/a2a_optin_test.go` (every pre-existing route contract booted twice — switch unset and
switch `true` — compared byte for byte, plus the A2A route surface pinned per switch position from
§5.2). The card is gated by `internal/a2a/card_test.go` (the projection, the security posture
matrix, the ETag) and `cmd/server/a2acard_test.go` (the route booted for real: 404/400/405/200, the
refusals, the caching contract, and a structural validation of the served card against the A2A
v1.0.0 AgentCard shape). The JSON-RPC binding is gated by `internal/a2a/parts_test.go` and
`internal/a2a/send_test.go` (the part mapping both directions, the request mapping, the error table,
the streaming oneofs) and `cmd/server/a2a_jsonrpc_test.go` (the route booted for real: the 2-part
round trip to a consumer with `alt`/`tags` intact, every refusal, the SSE lifecycle transcript, and a
WebSocket relay round trip with the option ON). The push-notification configuration operations are
gated by `internal/a2a/push_test.go` (the decoders, the derived id, the projection, every refusal and
its code, the notification template) and `cmd/server/a2apush_test.go` (the CRUD transcript against
crier's own row, the §3.3.4 capability error from all four operations, the notification asserted on
the wire at the endpoint, crier's own retry loop observed, and a row-unchanged check after every
refusal). The task lifecycle is gated by `internal/a2a/task_test.go` (the mapping table of §5.5.2 row
by row, the parameters, the ordering and the cursor, the refusal table) and
`cmd/server/a2a_lifecycle_test.go` (the route booted for real: the lifecycle transcript —
undelivered → leased → acked → expired → canceled — the terminal-state refusals, the paginated
listing, the swept-task arm driven through crier's own sweep, a live `SubscribeToTask`, the per-agent
gate, and the switch-off 404). Only the two INT-A2A-003 test files whose subject a row's method
surface moved were amended (`a2a_jsonrpc_test.go`: the method the probe names as unimplemented is the
extended card, `GetExtendedAgentCard`, now that BOTH the task lifecycle and the push-notification
methods are served, and a `taskId` naming no task is covered as the `-32001` §5.5.3 makes it); the
non-regression gate and every crier route test are untouched.

## 7. Out of scope (binding non-goals for the whole series)

- **gRPC** and **HTTP+JSON/REST** bindings (§10, §11) — declared MAY, not built (§2).
- **A2A as an authentication or authorization mechanism.** A2A cards *describe* auth; they never
  grant it. Crier's bearer token and agent signatures remain the only auth surfaces.
- **Replacing crier's own primitives.** Inbox, lease/ack, mesh, federation and webhook delivery stay
  as they are; A2A rides on top of them (§3) rather than re-implementing them.
- **A second registry.** The AgentCard is a projection of the registry row — never a parallel store
  that could disagree with it.
- **Promoting A2A into the default path.** No A2A code runs unless the operator turned the switch on
  *and* the target agent opted in (§1.1, §4).
- **Signing Agent Cards (§8.4).** `AgentCardSignature` is a JWS over an RFC 8785-canonicalized card,
  and crier computes none: it would have to canonicalize with RFC 8785 and sign with an agent's
  ed25519 key, and a signature produced without a verifiable canonicalization path is worse than no
  signature at all — it looks verifiable and is not. The card therefore carries no `signatures`
  member. Crier's registry rows keep their ed25519 public keys (readable, authenticated, from
  `GET /agents/{id}`); publishing them in the card is a separate decision nobody has asked for.
- **A provider identity.** `AgentCard.provider` is omitted: the service provider of a crier relay is
  its OPERATOR, crier has no field for that, and a constant organization string would be a claim
  about someone else's deployment.

## 8. What has shipped

### 8.1 INT-A2A-001 — the option and the gate

| Deliverable | Where |
|---|---|
| The server switch | `config/config.go` — `A2AEnabled` + `CR_A2A_ENABLED`, default false |
| The per-agent block | `internal/a2a/config.go` (`Config`, `OptedIn`, strict `DecodeConfig`); `internal/registry/types.go` (`Agent.A2A`), `internal/registry/handler.go` (`registerRequest.A2A`, `patchRequest.A2A`, `strictA2AMember`) |
| Durable persistence of the block | `internal/registry/migrations/005_add_agent_a2a_column.{up,down}.sql`, `internal/registry/postgres_store.go` |
| Non-regression proof | `internal/registry/a2a_optin_test.go`, `cmd/server/a2a_optin_test.go` |
| Docs | `README.md` (`CR_A2A_ENABLED` in the environment table), `docs/claims.yaml` + `cmd/server/docsclaims_test.go` (the default is pinned to the production constant), this file, `specs/_index.md` |

Not shipped by that row: any route, any handler, any middleware, any header, any AgentCard renderer,
and any read of `cfg.A2AEnabled` outside tests.

### 8.2 INT-A2A-002 — the Agent Card route (§5.3)

| Deliverable | Where |
|---|---|
| The card model and projection | `internal/a2a/card.go` — `AgentCard` and its nested spec objects, `BuildCard` (pure: row evidence + server posture in, card out), `CardETag`, and the discovery constants (`AgentCardPath`, `AgentCardAgentQueryParam`, `ProtocolVersion`, `ProtocolBindingJSONRPC`, `JSONRPCBindingPath`, `CardMediaType`, `CardCacheMaxAgeSeconds`) |
| The route and its handler | `cmd/server/a2acard.go` — `registerA2ACardRoute` (called from `run()` only when `cfg.A2AEnabled`), the selector/opt-in refusals, the caching headers and the conditional-GET path |
| The projection's unit gate | `internal/a2a/card_test.go` — the row projection, the security posture matrix, the omission rules, the ETag |
| The route's gate (booted server) | `cmd/server/a2acard_test.go` — `404` with the switch off, `400`/`405`/`200`/`404` with it on, the refusals, the caching contract, the auth posture, and a structural validation of the served card against the A2A v1.0.0 AgentCard shape |
| The non-regression gate, amended | `cmd/server/a2a_optin_test.go` — the pre-existing surface comparison is unchanged; the A2A paths are now asserted per switch position from §5.2 |
| Docs | §5 of this file (the route, the projection table and the auth posture), `README.md` (`CR_A2A_ENABLED` row now names the route), `docs/claims.yaml` (`ROUTE-A2A-AGENT-CARD`) |

Not shipped by that row, and still absent (404) under both switch positions: the JSON-RPC binding,
streaming, the task lifecycle, the push-notification methods and the extended card — INT-A2A-003..006.

### 8.3 INT-A2A-003 — the JSON-RPC binding (§5.4)

| Deliverable | Where |
|---|---|
| The JSON-RPC 2.0 envelope, the error codes and the detail shapes | `internal/a2a/rpc.go` — `DecodeRequest`, `RPCResponse`, `RPCError`, `ErrorInfo` / `BadRequest` / `DeliveryRefusal`, the code constants, and the strict member decoder the params decode is built on |
| The A2A data model and the part mapping, both directions | `internal/a2a/parts.go` — `Part` (oneof by pointer, presence-exact), `Message`, `Task`, `TaskState`, `TaskStatusUpdateEvent`, `TaskArtifactUpdateEvent`, `StreamResponse`, `Envelope` / `MessagePart` / `FileRef` (the crier side), `EnvelopeFromMessage` / `Envelope.Message` / `PartsFromPayload` |
| The request translation, the Task/Message projection and the error table | `internal/a2a/send.go` — `DecodeSendMessageParams`, `Translate`, `DeliveryRequest`, `DeliverAccept`, `TaskFromAccept`, `MessageFromReply`, `ErrorFromStatus`, `CheckVersion`, `TaskTopic` |
| The route, the deliver REUSE and the SSE adapter | `cmd/server/a2a.go` — `registerA2ARoute` (called from `run()` only when `cfg.A2AEnabled`), the dispatch, `deliverTo` (calls `registry.Handler.HandleDeliver`, the function `POST /agents/{id}/inbox` is registered with), `a2aStream` (lifecycle observation + the relay subscription written as `text/event-stream`) |
| The read-only store capability the stream needs | `internal/registry/inbox_peek.go` — `InboxPeeker` and its `MemoryStore` / `PostgresStore` implementations. It exists because `Retrieve` LEASES: an observer built on it would steal the message |
| The middleware seam the stream needs | `internal/middleware/middleware.go` — `responseWriter.Flush`, the same pass-through `Hijack` already had for WebSocket upgrades. Without it every A2A stream answered `500 "this server cannot stream"` (measured) |
| The unit gate | `internal/a2a/parts_test.go` (the 2-part projection, the lossless round trip, the oneof and metadata refusals, the payload projection, the topic convention), `internal/a2a/send_test.go` (the params strictness, the request mapping, `returnImmediately` and streaming, the refusals, the metadata typing, the version check, the error table, the Task/Message projection, the JSON-RPC envelope rules) |
| The route's gate (booted server) | `cmd/server/a2a_jsonrpc_test.go` — the switch-off 404, the 2-part round trip to a signed consumer with `alt`/`tags` intact, crier's idempotency replay through A2A, every refusal (and the proof that none of them delivered), the content-type rule, the SSE lifecycle transcript with a relay artifact, and the WebSocket relay round trip with the option ON |
| The non-regression gate, amended | `cmd/server/a2a_optin_test.go` — the pre-existing surface comparison is unchanged; the A2A paths are asserted per switch position from §5.2 (the JSON-RPC path now answers `405` to the probe's GET, i.e. registered and POST-only) |
| Docs | §5.4 of this file (the binding, the mapping tables, the SSE contract, the deviation, the non-goals), §5.2's route table, §5.3's card projection (`capabilities.streaming` is now `true`), `README.md` (`CR_A2A_ENABLED` row now names the binding), `docs/claims.yaml` (`ROUTE-A2A-JSONRPC`), `CHANGELOG.md` |

Not shipped by that row, and still absent under both switch positions: the extended card
(INT-A2A-006 — the only surface this option has not built), and any change to the relay's WebSocket
path (deliberate: the stream is an adapter OVER it).

### 8.4 INT-A2A-004 — the task lifecycle (§5.5)

| Deliverable | Where |
|---|---|
| The state mapping, the request parameters, the ordering/cursor and the refusals | `internal/a2a/task.go` — `TaskEvidence` / `InboxView` and `Resolve` (the §5.5.2 table, as a pure function), `Task` (the projection, including `metadata.crier.state_basis`), `DecodeGetTaskParams` / `DecodeListTasksParams` / `DecodeCancelTaskParams` / `DecodeSubscribeToTaskParams`, `BuildListPage` with `EncodePageToken` / `DecodePageToken`, and the refusal constructors `TaskNotFoundError`, `TaskNotCancelableError`, `TerminalTaskMessageError`, `TaskContinuationUnsupportedError`, `SubscribeToTerminalTaskError`, `TaskCapabilityUnsupportedError` |
| The error code the row adds | `internal/a2a/rpc.go` — `CodeTaskNotCancelableError` (-32002), and the six method names (also in `task.go`) |
| The operations, the evidence read and the task-id resolution | `cmd/server/a2a.go` — the dispatch, `evidence` (peek → dead-letter lookup), `getTask`, `listTasks`, `cancelTask`, `subscribeToTask`, `refuseTaskTarget`; the stream adapter is INT-A2A-003's, reused unchanged |
| The store capabilities the reads and the close need | `internal/registry/inbox_list.go` (`InboxLister`), `internal/registry/inbox_close.go` (`InboxCloser`), `internal/registry/deadletter_lookup.go` (`DeadLetterLookup`) — all OPTIONAL, all implemented by both shipped backends, no migration |
| The mapping's unit gate | `internal/a2a/task_test.go` — §5.5.2 row by row (record → state → basis), the zero-TTL case, the status timestamps, the task projection, the parameter strictness, the cursor (including its stability when the queue shrinks), the conditional `artifacts` member, and the refusal table |
| The route's gate (booted server) | `cmd/server/a2a_lifecycle_test.go` — the lifecycle transcript (undelivered → leased → acked → expired → canceled, with every transition named), the terminal-state refusals with the queue measured across them, the queued-cancel case, pagination over a real store, the swept-task arm driven through `registry.Handler.PurgeExpired`, a live `SubscribeToTask`, the per-agent gate, and the switch-off 404 |
| The non-regression gate | `cmd/server/a2a_optin_test.go` — **unchanged** (the A2A paths are probed per switch position there; this row adds no route). The two INT-A2A-003 test expectations that named the surface this row implements were amended, and are the only test edits in the row: `cmd/server/a2a_jsonrpc_test.go` (a method that is still unimplemented, and the `-32001` for a task id naming no task) |
| Docs | §5.2's route table, §5.4.4's error table and §5.4.5's CANCELED note, §5.4.7, §3's mapping rows, §6's gate list, this section, `README.md` (`CR_A2A_ENABLED` row now names the lifecycle), `docs/claims.yaml` (`ANCHOR-A2A-LIFECYCLE-STATES`, `ROUTE-A2A-LIFECYCLE-SPEC-ANCHOR` — the second also scans this file, anchoring §5.5.2), `CHANGELOG.md` |

Not shipped by that row: the extended card (INT-A2A-006), any storage change (the three capabilities
read and close the SAME rows), and any change to the routing, auth, guard, federation, webhook or mesh
behaviour of the delivery path.

### 8.5 INT-A2A-005 — the push-notification configuration operations (§5.6)

| Deliverable | Where |
|---|---|
| The four §9.4.7 method names, the derived configuration id and the strict params decoders | `internal/a2a/push.go` — `MethodCreateTaskPushNotificationConfig` / `MethodGetTaskPushNotificationConfig` / `MethodListTaskPushNotificationConfigs` / `MethodDeleteTaskPushNotificationConfig`, `PushConfigID`, `Decode*Push*`, `CreatePushConfigParams` / `PushConfigRefParams` / `ListPushConfigsParams` |
| The A2A object and the projection out of a row | `internal/a2a/push.go` — `TaskPushNotificationConfig`, `PushAuthenticationInfo`, `ListPushConfigsResponse`, `DeletePushConfigResult`, `PushRow` (plain values: this package still imports nothing from `internal/registry` or `internal/webhook`), `ProjectPushConfig`, `MatchPushConfigID` |
| The validation of a write, and every refusal | `internal/a2a/push.go` — `ValidatePushCreate`, `PushWrite`, `AuthNone` / `AuthBearer`; `PushNotSupportedError` (the §3.3.4 capability answer), `PushWriteRefused` and the `crier.ConfigRefusal` detail that carries crier's own status and body verbatim |
| The §4.3.3 payload shape | `internal/a2a/push.go` — `pushNotificationBody` and `PushNotificationShape()` (body template, `Content-Type: application/a2a+json`, `raw` reply extraction). The shape is a CONSTANT here and is installed on the config's `custom_schema` by the handler; crier's template engine and driver are untouched |
| The four handlers, the deliver-style reuse and the config merge | `cmd/server/a2apush.go` — `pushCreate` / `pushGet` / `pushList` / `pushDelete`, `pushRef`, `pushRow`, `pushWebhookConfig`, `patchTo` (calls `registry.Handler.HandleUpdateAgent`, the function `PATCH /agents/{id}` is registered with, exactly as `deliverTo` calls `HandleDeliver`) |
| The dispatch and the wiring | `cmd/server/a2a.go` (four `case`s in `handle`, plus `refineSendRefusal` for the inline-on-send answer), `cmd/server/main.go` (passes `HandleUpdateAgent` alongside `HandleDeliver`) |
| The unit gate | `internal/a2a/push_test.go` — the strict decoders, the derived id, the projection, every refusal and its code, the write-refusal mapping, and the notification template's structure (one-of-four `StreamResponse`, crier-only placeholders, a `|default:` on every optional one) |
| The route's gate (booted server) | `cmd/server/a2apush_test.go` — the CRUD transcript asserted through BOTH the A2A binding and crier's own `GET /agents/{id}`; the four operations against an agent with no push channel (`-32003`); the notification ASSERTED ON THE WIRE from what the endpoint received (media type, one-of-four envelope, task id/state/context, crier's own dispatch headers); crier's retry loop observed (`X-Crier-Retry: 0`, then `1`); every refusal with the row compared byte-for-byte before and after; the write-gate case under crier's default signature posture (`-32050` + crier's body verbatim, row untouched, reads still work) |
| Docs | §5.6 of this file (the methods, the mapping table, the capability MUST, the refusals, the payload, the non-goals), §3's mapping row, §5.2's route table, §5.4's served-method list and error table, §6.8 (the non-regression clauses), `README.md` (`CR_A2A_ENABLED` now names the four methods), `docs/claims.yaml` (`ANCHOR-A2A-PUSH-CONFIG-OPERATIONS`), `CHANGELOG.md` |

Not shipped by that row: any new route (the four operations are methods of the existing binding), any
new field on a registry row (the configuration IS the existing `webhook` member), any change to
`internal/webhook` (the notification rides the shipped driver), and the extended card — INT-A2A-006,
still `-32601`.

Still absent under both switch positions: the extended card, and any change to the relay's WebSocket
path.
