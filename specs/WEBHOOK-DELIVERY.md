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
- `delivery_mode` — `async` (default) | `blocking` | `batch`. A mode left absent on BOTH the agent config and
  the message resolves to `async` (fire-and-forget: the sender gets the `202` accept); an explicit value on
  either side wins, the per-message one over the agent default.
- `batch` — only for `batch` mode: flush at `max_messages` OR `flush_interval_s`, whichever first.
- `retries` — bounded retries for transient failures (default 5). `timeout_ms` — per POST timeout (default 30000).

Registration with a webhook does NOT disable the durable inbox or mesh identity — an agent can be reached by
any of its surfaces; the webhook is the *preferred push surface* for `deliver` when configured.

## 3. Outbound envelope (what the server POSTs)

```
POST {url} HTTP/1.1
Content-Type: application/json
X-Crier-Event: message | reply | configure | batch
X-Crier-Agent: {sender_agent_id}         # the agent this delivery came FROM; omitted when it names none
X-Crier-Target: {target_agent_id}        # the agent this delivery is FOR (the endpoint's own agent)
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
    "target": "agent-b",
    "kind": "message|reply|configure"
  },
  "payload": {…}
}
```

`X-Crier-Signature` = `hex(hmac_sha256(secret, canonical_body))` where `canonical_body` is the raw POST body.
Endpoints may verify; server always sends when `CR_WEBHOOK_SECRET` is set (env, server-wide v1).

### Which agent a delivery is for (DF-CRIER-175)

Two identities travel with every outbound POST, and they answer different questions:

- **FOR whom** — `X-Crier-Target` on the wire, `crier.target` in the body. This is the agent whose endpoint
  was POSTed: the same id the sender addressed, known to the server at the deliver call (the `{id}` of
  `POST /agents/{id}/inbox`) and at a batch flush (the endpoint the coalesced POST goes to). It is never
  derived from the webhook URL — one endpoint may serve several agents, and a URL path is not an identity.
- **FROM whom** — `X-Crier-Agent` on the wire, `crier.sender` in the body. This is the agent the delivery
  came FROM and its meaning is unchanged (it has always carried the sender). `crier.sender` stays omitted
  from the body when the delivery names no sender, and the header is then **omitted entirely — never sent
  blank**: a blank `X-Crier-Agent` would read downstream as a real-but-empty identity instead of as
  "unknown", so absence is the only signal for it.

A sink that serves several agents therefore never has to guess from the URL or the payload: the endpoint
receives "this delivery is for me" (`X-Crier-Target` / `crier.target`) and "it came from this agent"
(`X-Crier-Agent` / `crier.sender`) on the wire itself.

**Batch.** A batch POST is one POST to one endpoint, so it carries that endpoint's single `X-Crier-Target`
(the sender header keeps its existing meaning: the first inner envelope's sender) and every inner envelope
of `{"messages":[…]}` repeats its own `crier.target` — a sink that fans the batch back out to several
identities still sees, per message, which agent it was addressed to.

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

Sender-visible status for a blocking delivery on the deliver API (`POST /agents/{id}/inbox`, DF-CRIER-157):
a row above that is PERMANENT (a non-retryable `4xx (other)`) answers **502 Bad Gateway** with
`{"error":"webhook: permanent failure: status 400"}`, and so does a `2xx` whose body the reply schema cannot
map (`{"error":"webhook: permanent failure: reply extraction: …"}`) — the same envelope will get the same
answer, so retrying cannot succeed. A transient `5xx / 408 / 429 / timeout` that exhausts the caller's budget
answers **504 Gateway Timeout** with `{"error":"webhook: blocking delivery timed out after 30s (last: status 503)"}`;
only that class can still land on a later attempt. (Timeout and budget exhaustion are the only 504 cases.)

## 4. Delivery modes

| Mode | Semantics | Sender sees | Failure |
|---|---|---|---|
| blocking | POST + wait, reply from body | `200 {"id","transport":"webhook","reply",…}` | permanent endpoint rejection → 502; retries exhausted / timeout → 504 |
| async | POST, don't wait | `202 {"id","transport":"webhook","delivery_mode":"async"}` accept | queue + retry + backoff; retries exhausted → durable `WEBHOOK_FAILED` inbox notification to the sender |
| batch | coalesce N/T → one batch POST | `202 {"id","transport":"webhook","delivery_mode":"batch"}` accept | queue flush retry (exhaustion notifies like async) |

- **Transport signaling (DF-CRIER-157):** every accept names where the message actually went, so a sender
  never infers the destination from the status code. `"transport":"webhook"` on the 200/202 webhook paths —
  the durable inbox is BYPASSED by design, so `GET /agents/{id}/inbox` for that agent stays empty and an empty
  retrieve is not evidence that the message was never sent; `"transport":"inbox"` on the 201 store path, which
  also carries `expires_at` (no inbox entry exists on the webhook paths, so no expiry applies there).
  `delivery_mode` is echoed on the 202 accept (`async` | `batch`, the RESOLVED mode — a per-message
  `delivery_mode` override wins over the agent default) so the sender learns the queue semantics it entered.
- Async exhaustion notification (DF-CRIER-8): when a queued delivery exceeds `CR_WEBHOOK_MAX_RETRIES`, the
  driver drops it AND emits exactly one notification into the originating sender's durable inbox (direct
  store write — never webhook-routed, so it cannot recurse, and it lands there even when the SENDER itself is
  a webhook-configured agent). Payload:
  `{"kind":"error","code":"WEBHOOK_FAILED","message_id":"<original>","target":"<agent>","retries":N,"status_code":S,"error":"…"}`
  (`status_code` omitted on transport failure; `error` carries the transport error string). A missing sender
  or a failed notification write is logged best-effort: the delivery is not requeued and the notification is
  not retried or duplicated.
  Timing: one attempt per `CR_WEBHOOK_REDELIVER_S` tick (default 30s) and the item is dropped once
  `item.Retries > CR_WEBHOOK_MAX_RETRIES`, so with the defaults (5 / 30s) the notification arrives roughly
  two to three minutes after the accept. While the endpoint is degraded (circuit open after
  `CR_WEBHOOK_CIRCUIT_THRESHOLD` consecutive failures) queued items are re-queued WITHOUT a POST, so
  exhaustion is delayed until the probe (`CR_WEBHOOK_PROBE_S`, default 60s) succeeds and drains the queue.

- Sender selects per message: `delivery_mode` in the REQUEST/deliver payload overrides the agent default.
- Batch envelope: `X-Crier-Event: batch`, payload = `{"messages": [envelope, …]}`.
- Queue: durable (Postgres when `CR_DATABASE_URL`, else in-memory) — redelivery on interval
  (`CR_WEBHOOK_REDELIVER_S`, default 30s) and on recovery probe (endpoint healthcheck every 60s).
- Circuit breaker: `CR_WEBHOOK_CIRCUIT_THRESHOLD` consecutive 5xx/timeout failures (default 10) → endpoint
  marked `degraded`; while degraded the queued items are re-queued WITHOUT a POST (the poisoned endpoint is not
  hammered), and a successful probe (`CR_WEBHOOK_PROBE_S`, default 60s) clears the mark and drains the queue
  immediately. The redelivery ticker keeps running at `CR_WEBHOOK_REDELIVER_S` throughout — there is no separate
  longer backoff interval; the pause is what lengthens the time to exhaustion.

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

**Where the target identity is available per template (DF-CRIER-175).** The two identity headers
(`X-Crier-Target`, `X-Crier-Agent`) are set on the outbound request for EVERY template — a schema shapes the
request BODY, not the contract headers — so any endpoint can identify a delivery regardless of the template
it registered:

- `generic-custom` (the default; also what a `custom_schema` with no `request_shape` falls back to) POSTs the
  full envelope, so the body carries the `crier` object and with it `crier.target` **and** `crier.sender`.
- `openai-compatible` and `hermes-http-gateway` render their own body and carry **no `crier` object at all**:
  for them the target identity is available on the header only (they map session/thread context, not
  identity — this spec does not change their bodies).
- A `custom_schema.request_shape.headers` entry with one of those names is applied AFTER the server-set
  headers and wins. That is the documented bring-your-own-schema override: the integrator who sets it owns
  the consequence.

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

### 8.1 Hold/retry contract (DF-CRIER-7)

An outage must never be reported as `agent not found`, and a message must never be dropped silently. The
source relay classifies a failed forward attempt into exactly three outcomes:

| Outcome | Trigger | Response to the original sender |
|---|---|---|
| **Relayed** | a link answers anything except 404 with a non-retryable status | that link's status + body verbatim (unchanged blocking-webhook reply behavior) |
| **Agent not found** | every link answers 404 and none failed transiently | `404` immediately — the hold budget is never waited out |
| **Held** | at least one link was unreachable or answered a retryable status (5xx, 408, 429) and none delivered | `202 Accepted` `{"status":"held","id":…,"target":…,"max_hold_s":…}`; retried in the background |

- Retry runs until delivery succeeds or the hold budget (`CR_FED_MAX_HOLD_S`) expires. A retry re-POSTs the
  **same bytes** as the original attempt (same hop header, same link auth) and the delivery is removed from
  the queue the moment a 2xx is seen, so a recovery forwards it exactly once.
- **Terminal FEDERATION_FAILED.** When the budget expires — or a retry gets a definitive answer that is not a
  delivery (all-links-404, or any non-2xx rejection) — the sender gets exactly one durable notification in its
  own inbox, written directly through the store (never through webhook/federation routing, so it cannot
  recurse or requeue):

  ```json
  {"kind":"error","code":"FEDERATION_FAILED","message_id":"…","target":"agent-remote","sender":"agent-a",
   "request_id":"…","session_id":"…","attempts":4,"status":503,"error":"…"}
  ```

  Correlation fields (`message_id`, `target`, `sender`, `request_id`, `session_id`, `attempts`, `status`,
  `error`) are required; the message body is never echoed.
- With **no hold queue configured** (a bare `SetFederationClient` in an embedder, or a queue that refuses the
  delivery because it is full / the body exceeds the cap) the transient failure is reported synchronously as
  `502` with the stable body `{"error":"FEDERATION_FAILED","message_id":…,"target":…,"sender":…,
  "request_id":…,"session_id":…,"attempts":…,"status":…,"detail":…}`.
- Retry backoff is exponential (2s doubling, capped at 60s) inside the budget; every retry re-runs the same
  per-link classification as the first attempt, so a link that comes back is used immediately.

| Env | Default | Meaning |
|---|---|---|
| `CR_FED_MAX_HOLD_S` | `300` | hold budget (P2) |
| `CR_FED_QUEUE_FILE` | unset | durable hold-queue document path |

**What is durable.** With `CR_FED_QUEUE_FILE` set, held deliveries live in one atomically rewritten JSON
document (marshal → `fsync` `<path>.tmp` → rename `<path>` → `<path>.bak` → rename `<path>.tmp` → `<path>`,
`0600`), survive a source-relay restart, and are resumed with their original deadline. A crash at any point
leaves a parseable document: the new one at `<path>` or the previous good one at `<path>.bak`, recovered on
open. Without `CR_FED_QUEUE_FILE` the queue is **process-lifetime only** — held deliveries are lost on
restart, the same contract the in-memory registry backend documents for inboxes, so run the durable registry
(`CR_DATABASE_URL`) together with `CR_FED_QUEUE_FILE` when held work must survive a restart.

**Known limitations.** (a) Retry-on-5xx is at-least-once: a delivery the remote relay partially processed
before answering 5xx can be forwarded again, exactly as the webhook client's retry-on-5xx already can. (b) A
held **blocking** delivery cannot return its remote reply to the original caller — that HTTP request already
completed with `202`; the recovery is logged and the terminal case is the FEDERATION_FAILED notification.

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
| `CR_FED_QUEUE_FILE` | unset | durable hold-queue document (P2, DF-CRIER-7) |

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
