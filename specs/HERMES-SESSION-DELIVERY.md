# HERMES-SESSION-DELIVERY.md — Hermes gateway session delivery

Status: **DRAFT v1** · 2026-11-04 · Owner: Bane · Covers: the `hermes-session`
delivery transport — per-agent config, session policies, the named failures,
the replay window, and what is deliberately out of scope.
Code: `internal/hermesession/` (transport) + `internal/registry/hermes_delivery.go`
and the `hermes_session` branch of `registry.Handler.Dispatch` (delivery contract).

## 1. Goal

Crier can already hand a message to a durable inbox or push it at a webhook
endpoint. Neither is "a live agent session": an inbox is a queue, and a webhook
is an endpoint that has to exist on the other side. A Hermes agent (the gateway
that serves the desktop/TUI/chat surfaces) already exposes exactly the primitive
that was missing:

| Gateway route | What it does |
| --- | --- |
| `POST /api/sessions` | create one empty session row, answer `201` with the session |
| `GET /api/sessions/{id}` | read one session row (existence, `ended_at`, `end_reason`) |
| `POST /api/sessions/{id}/chat` | run ONE agent turn in that session, answer with the assistant reply |

This transport composes those three calls into a delivery target: a message
becomes one turn in a session that crier creates, or appends to one it already
knows. Everything about the destination is per-agent configuration, so one relay
can serve agents that want a session per message, agents that own a long-running
session, and agents that want exactly one session for their lifetime.

Non-goals (v1): no streaming (`/chat/stream`), no reply routing back into crier
(a turn's answer is returned to the SENDER as `reply`, not posted into an
inbox), no multimodal/attachment handling beyond the payload text, no
per-session turn queueing beyond what the gateway does itself, and no
cross-relay session ownership — the replay window and the per-agent session map
are process-scoped (see §7).

## 2. Where it sits in a delivery

`registry.Handler.Dispatch` is the one guarded choke point. The session branch is
taken when the TARGET agent has a `hermes_session` block:

1. **Guard** — the LLM message guard runs first, exactly as for every other
   transport. A blocked message never reaches the gateway; a sanitized one
   delivers the quarantine notice, not the original payload.
2. **Namespace / TTL / payload validation** — unchanged, and identical for both
   the HTTP and in-process callers.
3. **Session branch** — resolves/creates the session, posts the turn, records
   the correlation and answers.
4. The **durable inbox is bypassed**: no inbox entry is written, so a later
   `GET /agents/{id}/inbox` is empty — the same statement the webhook accept
   makes with `"transport":"webhook"`.

Precedence: a `hermes_session` block wins over a `webhook` block on the same
agent. Both are explicit destinations; the session one is the narrower, so it is
the one that runs, and the webhook is left untouched (not deleted, not called).

An agent configured with `hermes_session` on a relay that never wired the
transport is **refused**, never silently inboxed:
`502 {"error":"HERMES_SESSION_TRANSPORT_UNAVAILABLE"}`.

## 3. Configuration

Registered on the agent — `POST /agents`, `POST /agents/{id}`, or
`PATCH /agents/{id}` with a `hermes_session` object. `PATCH` follows the webhook
contract: an object replaces the block, `null`/absent removes it.

```json
{
  "id": "agent-b",
  "hermes_session": {
    "base_url": "http://127.0.0.1:8642",
    "token_file": "/run/secrets/hermes-gateway.token",
    "policy": "per-agent",
    "title": "crier delivery",
    "timeout_ms": 30000
  }
}
```

| key | required | meaning |
| --- | --- | --- |
| `base_url` | yes | gateway origin, `http(s)://host:port`. The session routes are appended; a trailing slash is tolerated. |
| `token_file` | yes | **path** of the file holding the gateway bearer token. `0600` or stricter. |
| `policy` | no (`new`) | `new` \| `existing` \| `per-agent` — see §4. |
| `session_id` | for `existing` | the session to append to. Refused when set under `per-agent`. |
| `title` | no | title for a session this transport **creates**. Blank lets the gateway mint one. Under `per-agent` the default is `crier: <agent id>`. |
| `timeout_ms` | no (`30000`) | per-call bound for each gateway request. |

Registration validates the **shape** only. The credential is resolved per
delivery, on purpose: a rotation needs no re-registration, and a relay that
registers an agent need not be able to read the token *yet*.

### 3.1 Credential discipline

- The token is read from `token_file` at delivery time and used only in an
  `Authorization: Bearer …` header. It is never part of config, never echoed in
  a response body, never written to a log line, and never included in an error
  string — errors name the file's **path** and its **mode**, never its contents.
- The file must be a **regular** file (a directory or device is refused), no
  larger than 8 KiB, non-empty, single-line, and carry **no group/other
  permission bits** (`mode & 0077 == 0`). A `0644` token file is refused with
  `HERMES_CREDENTIAL_UNAVAILABLE` rather than used.
- The HTTP client **does not follow redirects** (`CheckRedirect` returns the
  last response). A `3xx` is `HERMES_GATEWAY_REJECTED` with the gateway's status
  verbatim: the credential must not be replayed at a host the operator never
  handed it to.

## 4. Session policies

| policy | per message | session created | recorded |
| --- | --- | --- | --- |
| `new` | one new session | on every delivery (unless a retry reuses its own) | message id → session |
| `existing` | appends to `session_id` | never | message id → session |
| `per-agent` | appends to the agent's session | on first use, then never | agent id → session **and** message id → session |

Under `existing` and `per-agent` the session is **inspected** (`GET`) before the
turn. That call is what makes the two remaining failures distinguishable:

- the gateway answers `404` → `HERMES_SESSION_NOT_FOUND`;
- the row exists but has **ended** (`ended_at` non-null, or a non-empty
  `end_reason`) → `HERMES_SESSION_CLOSED`. The gateway itself would still run a
  turn in an ended session; this transport refuses rather than appending to a
  closed transcript.

A `per-agent` agent whose recorded session has since been deleted (gateway
`404`) starts a fresh one on the next delivery instead of failing forever. A
recorded session that has **ended** is a refusal, not a silent recreation: an
operator ended it for a reason.

## 5. The four named failures

Every refusal is a named code with its own status. The gateway's own status
rides alongside **verbatim** in `gateway_status` (`0`/absent when no HTTP
exchange happened), so an operator can see both what crier decided and what the
far end said.

| condition | crier code | crier status | `gateway_status` |
| --- | --- | --- | --- |
| gateway unreachable (dial/DNS/TLS/timeout; nothing delivered) | `HERMES_GATEWAY_UNREACHABLE` | `502` | absent |
| gateway answered `401`/`403` — crier's credential is wrong | `HERMES_GATEWAY_UNAUTHORIZED` | `502` | `401` or `403` |
| the addressed session does not exist | `HERMES_SESSION_NOT_FOUND` | `404` | `404` |
| the session exists but has ended | `HERMES_SESSION_CLOSED` | `409` | the `200` the GET answered |

Two further codes exist so no gateway answer is ever flattened into one of the
four:

| condition | code | status |
| --- | --- | --- |
| token file missing/unreadable/empty/oversized/loose mode | `HERMES_CREDENTIAL_UNAVAILABLE` | `502` |
| any other gateway answer: `5xx`, other `4xx`, a refused `3xx`, or a `2xx` whose body carries no session id | `HERMES_GATEWAY_REJECTED` | `502` |

Wire shape:

```json
502 {"error":"HERMES_GATEWAY_UNAUTHORIZED","detail":"the gateway rejected the credential crier was configured with","gateway_status":401}
```

A Go caller does not parse that prose: `Dispatch` returns a `*DispatchError`
whose `Code` is the same string and whose `StatusCode` is the same status.

Note the deliberate asymmetry: a `401` from the gateway is **crier's** fault, not
the sender's, so the sender gets `502` (a bad upstream), never `401` (which on
crier's own surface means "your request to crier was unauthenticated").

## 6. Accept, correlation and replay

A successful delivery answers `200` with the blocking shape:

```json
{
  "id": "3f2c…",                       // crier message id
  "transport": "hermes-session",
  "hermes_session_id": "api_1730…",    // the Hermes session the turn ran in
  "reply": "…",                        // the assistant's answer, as a JSON string
  "session_id": "chat-7",              // the CRIER session context, echoed
  "request_id": "req-9",               // the sender's correlation id, echoed
  "replayed": true                     // only on a replay-by-message-id
}
```

**Correlation.** The crier message id travels WITH every gateway call as
`X-Crier-Message-Id` (plus `X-Crier-Target`, `X-Crier-Agent`, `X-Crier-Event`
and `X-Crier-Session-Id` for the crier chat session). What comes back — the
session id in the chat response body, else the `X-Hermes-Session-Id` header,
else the session that was addressed — is recorded as the delivery's
**correlation id**, so a later reply quoting either id can be matched to the
message that caused the turn. `Client.Lookup(messageID)` returns that record.

**Replay by message id.** The same message id delivered twice is answered from
the replay window: nothing is created, nothing is posted, and the second answer
carries `"replayed":true` with the SAME session and correlation. The window is
24h by default (`SetReplayWindow`) and process-scoped.

A *failed* turn does not burn a session: if the create succeeded but the chat
failed, the created session is recorded against the message id and a retry
writes into THAT session rather than minting a second one.

**Sender idempotency keys.** A delivery carrying `idempotency_key` also records
its accept in the relay's existing CR-FEAT-025 replay window, which is keyed per
(target agent, key) rather than per message id — the HTTP ingress mints a fresh
message id per request, so without this a retried request would start a second
session. A replay under the same key answers `"idempotent_replay":true` and
names both the same crier message id and the same `hermes_session_id`.

## 7. Stated limits

- **Process-scoped.** Both the replay window and the per-agent session map live
  in the serving process, like the sender idempotency window and the capability
  rotation cursor. A restart closes the replay window (the next delivery of the
  same message id delivers again, into a new session under policy `new`), and a
  `per-agent` map restarts empty (the next delivery creates ONE new session).
  Neither is a durable ledger.
- **One relay.** Session ids are not shared across relays; a federated delivery
  to an agent owned elsewhere applies THAT relay's configuration and window.
- **No queue.** The turn is waited for inside the sender's request, bounded by
  `timeout_ms` (default 30s, the deliver API's documented blocking budget). A
  turn slower than that answers `HERMES_GATEWAY_UNREACHABLE` — the honest
  classification of "no answer in the budget" — and the recorded session is
  reused by a retry.
- **Reply text only.** The turn's `message.content` is returned as `reply`.
  Tool calls, usage and runtime metadata in the gateway's response are not
  forwarded.
- **`new` policy is per message, not per conversation.** Two related messages
  under `new` are two sessions; an agent that wants a conversation must use
  `existing` or `per-agent`.
- **Session fan-out does not adopt the turn's answer.** A chat-session fan-out
  (`internal/session`) records the delivery outcome for a hermes-configured
  member exactly as it does for an inbox target — accepted, no answer bubble —
  because the answer-adoption path is the webhook reply schema. The turn's reply
  IS returned to the direct sender (`POST /agents/{id}/inbox`, MCP, in-process
  callers).
- **The opt-in detection layer keeps its webhook vocabulary.** CR-FEAT-030's
  delivery log files a session delivery under the existing webhook
  verdict/transport labels: its vocabulary is a closed, audited enum pinned by
  `detection_vocabulary_test.go`, and extending it is a separate change.

## 8. Tests

`internal/hermesession/hermesession_test.go` — httptest fake gateway, no real
Hermes, no network: all three policies, the four failures with their statuses
and verbatim gateway status, the replay, the retry-after-chat-failure reuse,
the credential file discipline (including "the error never carries the token"),
concurrent same-id and distinct-id behaviour, redirect refusal, and the request
headers.

`internal/registry/hermes_delivery_test.go` — the same transport through the
real `Dispatch` choke point: the `200` accept's transport/session/reply, the
inbox bypass, all four failures over HTTP, the replay by message id, the
idempotency-key replay naming the same session, the unwired-transport refusal,
and the registration-time `400`.
