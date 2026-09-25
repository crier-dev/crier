# A2A-OPTION.md — A2A interoperability as an OPT-IN extra

Status: **DRAFT v2** · 2026-09-25 · Owner: Bane · Tickets: INT-A2A-001 (this doc + the opt-in gate),
INT-A2A-002 (**SHIPPED** — the Agent Card discovery route, §5.3), INT-A2A-003..006 (the remaining A2A
surfaces — NOT shipped)
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
  SET, exactly one route exists: the Agent Card discovery route of §5.3, serving only agents that
  opted in.)
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
| `AgentCard` (§4.4.1) | the agent registry row — `GET /agents/{id}` | The card is a **projection** of the row, not a second store. Discovery path `GET /.well-known/agent-card.json` (§8.2, §14.3) serves the same projection for the process's own identity. |
| `AgentSkill[]` (§4.4.5) | `agent.capabilities[]` | Each capability tag becomes a skill entry; the projection is one-way (crier's registry stays the source of truth). |
| `SendMessage` (§9.4.1) | deliver — `POST /agents/{id}/inbox` | The A2A message becomes a crier delivery to the target agent. |
| `Task` + `TaskState` (§4.1.1, §4.1.3) | the inbox entry + its lease/ack lifecycle | `submitted`/`working` = stored or leased; `completed` = acked (retrieved + acknowledged); `canceled` = removed before ack. The crier entry id is the A2A `task.id`. |
| `SubscribeToTask` / `SendStreamingMessage` (SSE) (§9.4.2, §9.4.6) | relay WS subscribe | A2A's stream is the SSE view of the same subscription surface; the mesh is untouched. |
| `Part` — `text` \| `file` \| `data` — plus `Part.metadata` (§4.1.6) | crier message parts plus `alt`/`tags` | No new payload model: an A2A part maps onto the delivery payload's part metadata. |
| `TaskPushNotificationConfig` (§4.3.1) | `webhook.Config` (url / retries / timeout / batch) | A2A push-notification config is expressed with the webhook fields crier already has; no second push mechanism. |
| `Message.metadata` / extensions (§4.1.4, §9.2) | the envelope's metadata | The envelope stays the carrier; A2A extension URIs ride the `A2A-Extensions` header. |
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

### 5.1 With the switch OFF: zero new routes — with it ON: exactly one

With `CR_A2A_ENABLED` unset (the default, and every existing deployment's posture) the route table is
what it was before the option existed: **no A2A route is registered**, and every A2A path answers the
router's ordinary `404`. That half is checked live in
`cmd/server/a2a_optin_test.go` (every A2A path probed in both switch positions) and needs no
maintenance: it is the statement "the option is separable".

With `CR_A2A_ENABLED=true` the server registers **exactly one** A2A route — the Agent Card discovery
route of §5.3 — and nothing else. The two positions differ in exactly that one path; every
pre-existing route answers identically in both (§6).

### 5.2 Route surface inventory

| Surface | Served for | Registered only when | State |
|---|---|---|---|
| `GET /.well-known/agent-card.json?agent_id=<id>` — the AgentCard of one registry row | AgentCard discovery (§8.2, §14.3) | `CR_A2A_ENABLED=true` **and** the named row opted in | **SHIPPED (INT-A2A-002), §5.3** |
| A JSON-RPC 2.0 endpoint accepting `SendMessage`, `SendStreamingMessage`, `GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask`, the push-notification-config methods and `GetExtendedAgentCard` | §9.4 core methods | `CR_A2A_ENABLED=true` | FUTURE (INT-A2A-003/004/005) — not registered, answers 404 |
| SSE streaming (`text/event-stream`) for `SendStreamingMessage` / `SubscribeToTask` | §9.4.2, §9.4.6 | `CR_A2A_ENABLED=true` | FUTURE (INT-A2A-003) — not registered |
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
| `supportedInterfaces[]` | one entry: `url` = the origin the client reached + `/a2a`, `protocolBinding` = `JSONRPC`, `protocolVersion` = `1.0`, `tenant` = the agent id. **The URL is a forward declaration**: the JSON-RPC endpoint it names lands with INT-A2A-003, and this document fixes the path so that row serves it rather than inventing another |
| `version` | the serving build's identity (`internal/buildinfo`). The registry row carries no version of its own, and a second, invented one would be a source of truth that could drift |
| `documentationUrl` | the same origin + `/docs` (the live API documentation of the surface being described) |
| `capabilities.streaming` | **always `false` today, and always serialized**: crier serves no A2A streaming binding yet, so `true` would promise a stream this server cannot answer. The relay's WebSocket subscribe is not this — it is crier's own protocol |
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
- **It does not make crier A2A-first.** Zero A2A code runs with the switch off, and the single
  surface that exists with it on serves a description of an agent that asked to be described.

## 6. Non-regression contract

"Behaves exactly as today" is asserted over the following, with the switch **off** and with it **on**
(and it must hold identically in both positions):

1. **Status codes and body shapes of the existing read routes** — `GET /health`, `GET /version`,
   `GET /status`, `GET /agents` (and `GET /agents/{id}`): unchanged status, unchanged JSON key
   set/order-relevant shape. `GET /status`'s key set is pinned by its own test, so the option adds
   no key there — the switch is deliberately absent from `/status`.
2. **The registered route table** — with the switch OFF, no new `HandleFunc`/`Handle` line vs. the
   pre-change tree; with it ON, exactly the one A2A route §5.2 marks SHIPPED, and nothing else
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

The gate for all of this is the non-regression test suite shipped with INT-A2A-001 — still the gate,
amended only where this row's route moved the surface it describes:
`internal/registry/a2a_optin_test.go` (wire shape, strict decode, round-trip) and
`cmd/server/a2a_optin_test.go` (every pre-existing route contract booted twice — switch unset and
switch `true` — compared byte for byte, plus the A2A route surface pinned per switch position from
§5.2). The card itself is gated by `internal/a2a/card_test.go` (the projection, the security posture
matrix, the ETag) and `cmd/server/a2acard_test.go` (the route booted for real: 404/400/405/200, the
refusals, the caching contract, and a structural validation of the served card against the A2A
v1.0.0 AgentCard shape).

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
