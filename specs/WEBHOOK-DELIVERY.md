# WEBHOOK-DELIVERY.md — Push-based HTTP delivery spec

Status: **DRAFT v1** · 2026-08-19 · Owner: Bane · Tickets: CR-SPEC-001 (this doc), CR-FEAT-001..008
Source: PRD-2026-08-19.html §4-§10 · Covers: webhook driver, blocking/non-blocking/batch modes,
schema templates, session mapping, self-configuration directives, federation.

## 1. Goal

Let ANY agent — of ANY backend — receive and reply to Crier messages through a plain HTTP endpoint that the
agent (or its operator) controls. The server becomes a delivery primitive: durable inbox, live mesh, and now
**webhook push**, with per-message delivery semantics chosen by the sender.

Non-goals (v1): no server-side LLM integration, no message transformation beyond schema templates,
no full-mesh federation (links only).

## 2. Agent registration extension

`POST /agents` gains an optional `webhook` object:

```json
{
  "id": "agent-b",
  "public_key": "...",
  "capabilities": {"solver": true},
  "webhook": {
    "url": "http://relay.internal:9000/hooks/agent-b",
    "auth": {"type": "bearer", "value_ref": "env:CR_WEBHOOK_TOKEN_B"},
    "schema_template": "generic-custom",
    "custom_schema": null,
    "delivery_mode": "blocking",
    "batch": {"max_messages": 10, "flush_interval_s": 5},
    "retries": 5,
    "timeout_ms": 30000
  }
}
```

- `url` — REQUIRED for webhook delivery. Plain HTTP(S), server → endpoint.
- `auth` — optional. `type: bearer|header|none`; `value_ref` references a server-side env var (never stored
  in the registry row; resolved at send time). v1: `none` and `bearer`.
- `schema_template` — one of `generic-custom` (default), `openai-compatible`, `hermes-http-gateway`.
- `custom_schema` — full override; if present it wins over the named template (bring-your-own schema).
- `delivery_mode` — `blocking` (default) | `async` | `batch`.
- `batch` — only for `batch` mode: flush at `max_messages` OR `flush_interval_s`, whichever first.
- `retries` — bounded retries for transient failures (default 5). `timeout_ms` — per POST timeout (default 30000).

Registration with a webhook does NOT disable the durable inbox or mesh identity — an agent can be reached by
any of its surfaces; the webhook is the *preferred push surface* for `deliver` when configured.

## 3. Outbound envelope (what the server POSTs)

```
POST {url} HTTP/1.1
Content-Type: application/json
X-Crier-Event: message | reply | configure | batch
X-Crier-Agent: {target_agent_id}
X-Crier-Session: {session_id}            # when session context exists
X-Crier-Signature: {hmac-sha256 hex}     # when CR_WEBHOOK_SECRET set
X-Crier-Retry: {n}                       # 0 on first attempt

{
  "crier": {
    "version": 1,
    "message_id": "…24hex…",
    "request_id": "…24hex…",             # set when this is a reply to a REQUEST
    "session_id": "…", "thread_id": "…",
    "delivery_mode": "blocking|async|batch",
    "sender": {"agent_id": "agent-a"},
    "kind": "message|reply|configure"
  },
  "payload": {…}
}
```

`X-Crier-Signature` = `hex(hmac_sha256(secret, canonical_body))` where `canonical_body` is the raw POST body.
Endpoints may verify; server always sends when `CR_WEBHOOK_SECRET` is set (env, server-wide v1).

### Response contract

| Status | Meaning | Action |
|---|---|---|
| 2xx | delivered | blocking: body parsed per schema → RESPONSE to sender |
| 408/429 | transient | retry with backoff |
| 4xx (other) | permanent | ERROR frame to sender, no retry |
| 5xx / timeout | transient | retry with exponential backoff (1s,2s,4s,8s,16s), then ERROR |

Blocking-mode reply extraction: schema `response_map` (JSONPath into the body, or `raw` = whole body).
Reply becomes a RESPONSE frame/message with `request_id` = original REQUEST's `message_id` — the same
contract as the mesh RESPONSE frame (CR-SPEC note: HTTP hop must preserve the contract; the route table is
keyed by `message_id` so webhook replies flow through `forwardResponse` unchanged).

## 4. Delivery modes

| Mode | Semantics | Sender sees | Failure |
|---|---|---|---|
| blocking | POST + wait, reply from body | reply payload | retries → ERROR `WEBHOOK_FAILED{status, retries}` |
| async | POST, don't wait | 202 accept | queue + retry + backoff |
| batch | coalesce N/T → one batch POST | 202 accept | queue flush retry |

- Sender selects per message: `delivery_mode` in the REQUEST/deliver payload overrides the agent default.
- Batch envelope: `X-Crier-Event: batch`, payload = `{"messages": [envelope, …]}`.
- Queue: durable (Postgres when `CR_DATABASE_URL`, else in-memory) — redelivery on interval
  (`CR_WEBHOOK_REDELIVER_S`, default 30s) and on recovery probe (endpoint healthcheck every 60s).
- Circuit breaker: 10 consecutive 5xx/timeout → endpoint marked `degraded`; redelivery interval backs off to
  5 min; probe succeeds → immediate drain. (`CR_WEBHOOK_CIRCUIT_THRESHOLD`, default 10.)

## 5. Session & context mapping (CR-FEAT-004)

- Envelope carries `session_id` + `thread_id`; schema templates map these onto the backend's session/thread
  identifiers (e.g. Hermes gateway thread routing, OpenAI `user`/`thread` fields).
- Per-session FIFO ordering to a webhook target: delivery serialization keyed by `session_id` (in-flight
  cap per session = 1 in blocking mode).
- Round-trip invariant: a reply's `crier.session_id` MUST echo the originating REQUEST's `session_id` —
  verified by the E2E session-continuity probe.
- Script remapping: receivers may POST replies with a different `session_id` (the "script maps the context
  window" case) — the server accepts it and the bridge surfaces it.

## 6. Schema templates (CR-FEAT-003) — data, not code

Template = JSON object with three sections; stored under `templates/` and referenced by name:

```json
{
  "name": "hermes-http-gateway",
  "request_shape": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "headers": {"Authorization": "Bearer {{auth.bearer}}"},
    "body": {
      "model": "{{capabilities.model|default:'deepseek-v4-flash'}}",
      "messages": [{"role": "user", "content": "{{payload.text}}"}],
      "stream": false
    }
  },
  "session_map": {"session": "{{crier.session_id}}", "thread": "{{crier.thread_id}}"},
  "action_variants": {
    "message": {"path": "/v1/chat/completions", "body": "{{request_shape.body}}"},
    "reply":   {"path": "/v1/chat/completions", "body": "{{request_shape.body}}"},
    "configure": {"path": "/v1/configure", "body": {"directive": "{{payload}}", "agent": "{{crier.sender.agent_id}}"}}
  },
  "response_map": {"type": "jsonpath", "path": "$.choices[0].message.content"}
}
```

- `generic-custom`: passthrough — POSTs the full envelope JSON, reply = raw body.
- `openai-compatible`: messages[] mapping, reply from first choice, `session_map` → `user` field.
- Custom schemas supplied at registration REPLACE the named template entirely (merge = shallow; unknown
  keys rejected at registration — 400).

## 7. Self-configuration directives (CR-FEAT-007)

- New kind `configure`: delivered like any message (webhook `X-Crier-Event: configure`, payload contains
  `{"directive": "…", "target_capability": "abc", "schema": {…optional}}`).
- `PATCH /agents/{id}` (new): partial update of capabilities + webhook (register/update/remove webhook).
- `GET /agents?capability=abc`: capability filter — discovery for "who can do ABC".
- Ack: agent replies (any surface) with kind `configure_ack`; the directive sender can wait (blocking) for it.

## 8. Federation (CR-FEAT-006, P2)

- `CR_FED_LINKS=relay-b=https://relay-b:18767` — outbound authenticated link (shared secret header v1).
- Linked relays exchange agent tables (cached, TTL 60s): local registry rows flagged `"via": "relay-b"`.
- Delivery to remote agent: source relay POSTs the envelope to the link (HTTP, same webhook contract — the
  link IS a webhook endpoint with schema `relay-link`); remote relay delivers locally and routes the reply
  back over the same link. `request_id` is hop-invariant.
- Link down → durable queue at source; ERROR after `CR_FED_MAX_HOLD_S` (default 300s).
- Discovery: remote agents appear in `GET /agents` with `via`; `mesh_peers`/bridge `list_agents` include them.

## 9. Config surface (env)

| Env | Default | Meaning |
|---|---|---|
| `CR_WEBHOOK_SECRET` | unset | HMAC signing of outbound webhooks |
| `CR_WEBHOOK_TIMEOUT_S` | 30 | per-POST timeout |
| `CR_WEBHOOK_MAX_RETRIES` | 5 | transient retries |
| `CR_WEBHOOK_REDELIVER_S` | 30 | queue redelivery interval |
| `CR_WEBHOOK_PROBE_S` | 60 | degraded-endpoint healthcheck interval |
| `CR_WEBHOOK_CIRCUIT_THRESHOLD` | 10 | consecutive failures → degraded |
| `CR_FED_LINKS` | unset | relay link list (P2) |
| `CR_FED_MAX_HOLD_S` | 300 | federation hold time (P2) |

## 10. Implementation plan (ticket mapping)

1. **CR-FEAT-001** — `internal/webhook/` package: `Client` (POST + retry/backoff), `Queue` (durable store
   interface + memory impl), `Driver` (deliver → endpoint or queue; probe loop). Registry: webhook field in
   agent type + registration validation. `deliver` routing: target has webhook → driver (blocking/async);
   else inbox (unchanged). Tests: round-trip, retry-on-500, queue-survives-restart (memory), circuit breaker.
2. **CR-FEAT-002** — blocking correlation: driver waits, extracts reply per schema, feeds
   `mesh.forwardResponse` path (route table hit) or delivers reply to sender's inbox with `request_id`
   echoed. Timeout → `ERROR` frame `WEBHOOK_FAILED`. Tests: blocking round-trip; timeout→ERROR.
3. **CR-FEAT-004** — session fields through envelope → webhook headers/body; per-session serialization;
   round-trip invariant test.
4. **CR-FEAT-003** — template engine (`internal/webhook/schema.go`): `{{var}}` expansion, jsonpath response
   extraction; three v1 templates; registration-side schema validation.
5. **CR-FEAT-005** — batch mode: per-agent batch buffer + flush loop; batch envelope; tests.
6. **CR-FEAT-007** — configure kind + `PATCH /agents/{id}` + capability filter + ack.
7. **CR-FEAT-008** — `examples/webhook-demo/`: two different-backend agents via webhook delivery +
   hermes-http-gateway template; transcript + report.
8. **CR-FEAT-006** — relay links, agent table exchange, cross-relay delivery + reply.

## 11. E2E battery additions (E2E-001 growth)

- webhook round-trip: register agent w/ webhook (httptest endpoint) → deliver → endpoint receives exact
  envelope (headers incl. signature when secret set) → 2xx → sender gets reply.
- blocking reply: endpoint returns body → sender's ask resolves with body content.
- retry: endpoint 500 ×2 then 200 → exactly 3 POSTs observed.
- batch: 10 deliveries → ≤3 POSTs with flush 5/100ms.
- session continuity: A→B→A round-trip carries one session_id.
- circuit breaker: 10× 500 → degraded → probe → drain.

## 12. Open decisions (owner)

1. Queue storage v1: Postgres-only vs embedded (bbolt) — recommend embedded for single-binary simplicity;
   Postgres when CR_DATABASE_URL set.
2. Batch envelope shape: flat array (recommended).
3. Webhook auth v1: HMAC secret server-wide (recommended) vs per-agent keys.
4. Federation v1 link auth: shared secret header (recommended).
