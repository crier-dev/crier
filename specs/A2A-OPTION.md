# A2A-OPTION.md — A2A interoperability as an OPT-IN extra

Status: **DRAFT v1** · 2026-09-25 · Owner: Bane · Tickets: INT-A2A-001 (this doc + the opt-in gate),
INT-A2A-002..006 (the A2A surfaces themselves — NOT shipped by this row)
Source: **A2A v1.0.0** (`github.com/a2aproject/a2a`) — section numbers below are that specification's.
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
  requirements and storage schema are exactly what they were before this option existed.
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

### 5.1 With the switch ON and no further rows landed: **zero new routes**

INT-A2A-001 registers **no** route, handler, middleware or header. This is the checked property, not
an intention: `cmd/server/main.go`'s route table with `CR_A2A_ENABLED=true` is the same table as with
it unset, and `GET`-probing the A2A-ish paths below answers the same `404` the router answered before
the option existed. The switch is *carried* — that is all this row ships.

### 5.2 What the later rows add (all FUTURE — not implemented, not registered, not advertised)

Listed so the surface is agreed before it exists; each entry names the A2A section it serves. None of
these is reachable today, under either switch position.

| Future surface | Served for | Registered only when |
|---|---|---|
| `GET /.well-known/agent-card.json` — the process's own AgentCard | AgentCard discovery (§8.2, §14.3) | `CR_A2A_ENABLED=true` |
| A JSON-RPC 2.0 endpoint accepting `SendMessage`, `SendStreamingMessage`, `GetTask`, `ListTasks`, `CancelTask`, `SubscribeToTask`, the push-notification-config methods and `GetExtendedAgentCard` | §9.4 core methods | `CR_A2A_ENABLED=true` |
| SSE streaming (`text/event-stream`) for `SendStreamingMessage` / `SubscribeToTask` | §9.4.2, §9.4.6 | `CR_A2A_ENABLED=true` |
| `agent/authenticatedExtendedCard` (auth-gated extended card) | §3.1.11, §9.4.8 | `CR_A2A_ENABLED=true` |

Every future surface is gated by the server switch **and** targets an agent that opted in with the
`a2a` block (§4.2). A future row that needs a wider gate must amend this document first.

## 6. Non-regression contract

"Behaves exactly as today" is asserted over the following, with the switch **off** and with it **on**
(and it must hold identically in both positions):

1. **Status codes and body shapes of the existing read routes** — `GET /health`, `GET /version`,
   `GET /status`, `GET /agents` (and `GET /agents/{id}`): unchanged status, unchanged JSON key
   set/order-relevant shape. `GET /status`'s key set is pinned by its own test, so the option adds
   no key there — the switch is deliberately absent from `/status`.
2. **The registered route table** — no new `HandleFunc`/`Handle` line vs. the pre-change tree, in
   either switch position (§5.1).
3. **The agent wire shape** — an agent registered without the `a2a` block serializes exactly as
   before (no new key); an agent registered with it gains exactly one optional key, and its core
   fields (`id`, `public_key`, `capabilities`, `status`, `registered_at`, `last_seen`) are unchanged
   in name, type and meaning. Round-trip is asserted both ways through `POST /agents` → `GET
   /agents/{id}`.
4. **Auth** — the exempt-path list, the bearer requirement and the ed25519 agent-signature
   requirement are untouched: no existing route gains an A2A auth requirement, and the `a2a` block
   does not require signing to be relaxed.
5. **Strict-decode discipline** — the `a2a` block is rejected (400, key named) on a malformed
   member rather than silently ignored, and a rejected registration leaves the registry untouched.
6. **Storage** — the durable schema change is one additive nullable column
   (`ALTER TABLE agents ADD COLUMN a2a JSONB NULL`); existing rows read back exactly as before, and
   an agent with no block stores SQL `NULL`, not an empty object.
7. **Everything else** — relay, mesh, federation, guard, webhook delivery, MCP, openapi
   (`docs/openapi.yaml` + its generated `cmd/server/openapi.yaml` copy) and the docs-claims gate are
   unchanged, because no route, field or default they describe moved.

The gate for all of this is the non-regression test suite shipped with INT-A2A-001:
`internal/registry/a2a_optin_test.go` (wire shape, strict decode, round-trip) and
`cmd/server/a2a_optin_test.go` (the same route contract booted twice — switch unset and switch
`true` — and compared, plus a route-table census).

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

## 8. What INT-A2A-001 shipped

| Deliverable | Where |
|---|---|
| The server switch | `config/config.go` — `A2AEnabled` + `CR_A2A_ENABLED`, default false |
| The per-agent block | `internal/a2a/config.go` (`Config`, strict `DecodeConfig`); `internal/registry/types.go` (`Agent.A2A`), `internal/registry/handler.go` (`registerRequest.A2A`, `patchRequest.A2A`, `strictA2AMember`) |
| Durable persistence of the block | `internal/registry/migrations/005_add_agent_a2a_column.{up,down}.sql`, `internal/registry/postgres_store.go` |
| Non-regression proof | `internal/registry/a2a_optin_test.go`, `cmd/server/a2a_optin_test.go` |
| Docs | `README.md` (`CR_A2A_ENABLED` in the environment table), `docs/claims.yaml` + `cmd/server/docsclaims_test.go` (the default is pinned to the production constant), this file, `specs/_index.md` |

Deliberately **not** shipped: any route, any handler, any middleware, any header, any AgentCard
renderer, and any read of `cfg.A2AEnabled` outside tests. INT-A2A-002..006 start from §5.2.
