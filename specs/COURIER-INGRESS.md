# COURIER-INGRESS.md — push ingress: one call, three destinations

Status: **DRAFT v1 · 2026-11-03** · Owner: Bane · Row: **CRIER-INGRESS-1**
Series: `specs/CHAT-SESSIONS.md` (session model), `specs/COURIER-HARNESS.md` (harness delivery),
`specs/COURIER-TAGS.md` (tags on the same message), `specs/COURIER-READS.md` (read cursors).

## 1. Purpose

Give a caller one endpoint that pushes a message to exactly one of three destinations — an
existing session, a **new session created from a template**, or an **agent inbox** — so a
message can start a conversation instead of only landing in a queue an agent must poll.

The inbox path is unchanged and stays first-class. This spec adds the other two modes and the
routing that chooses between them.

## 2. Interface

### 2.1 HTTP

```
POST /ingress/messages
Authorization: Bearer <CR_AUTH_TOKEN>            (only when the relay has auth enabled)
Content-Type: application/json
Idempotency-Key: <caller key>                    (optional; body.idempotency_key also accepted)
```

Request body — one struct, `mode` selects the destination:

```jsonc
{
  "mode": "session" | "new_session" | "inbox",   // required, exact enum
  "target": {
    // mode=session
    "session_id": "s_abc123",
    // mode=new_session
    "template": {
      "title": "nightly build triage",
      "kind": "channel",                          // channel | direct (CHAT-SESSIONS §1.2)
      "namespace": "crier",
      "members": [                                // optional; author auto-joins
        { "member_type": "agent", "member_id": "alpha" }
      ]
    },
    // mode=inbox
    "agent_id": "alpha",
    "capability": "build"                       // optional; capability inbox (existing path)
  },
  "author": { "agent": "ci-bot" },              // required; same AuthorRef shape as /sessions/{id}/messages
  "kind": "plain" | "addressed",                // never "task" — task creation stays on its own route
  "text": "build 4412 failed on main",
  "tags": ["build", "urgent"],                  // COURIER-TAGS; validated here
  "payload": { },                               // optional opaque JSON, ≤ 256 KiB
  "reply_to_thread": "m_root123"                // optional; only valid with mode=session
}
```

Response `200` (existing session) / `201` (new session created):

```jsonc
{
  "mode": "new_session",
  "session_id": "s_9f2c",                       // null when mode=inbox
  "message_id": "m_71a4",
  "seq": 1,
  "outcomes": [ { "member_type": "agent", "member_id": "alpha", "result": "accepted" } ],
  "created_session": true,
  "delivery": { "mode": "harness", "harness": "opencode", "state": "dispatched" }
}
```

`delivery` is present only when a harness adapter claimed the message (COURIER-HARNESS §4).
When no adapter is configured the object is `{"mode":"store","state":"stored"}` — the message is
in the session and every participant reads it from the transcript, exactly as today.

### 2.2 Go

New package `internal/ingress`, additive. It imports `internal/session` and `internal/delivery`,
and nothing imports it except `cmd/server/main.go`.

```go
package ingress

// Mode is the destination selector. Unknown values are refused, never defaulted.
type Mode string

const (
    ModeSession    Mode = "session"
    ModeNewSession Mode = "new_session"
    ModeInbox      Mode = "inbox"
)

// Request is the parsed and validated form of the POST body.
type Request struct {
    Mode      Mode
    SessionID string           // mode=session
    Template  *SessionTemplate // mode=new_session
    AgentID   string           // mode=inbox
    Capability string          // mode=inbox, optional
    Author    session.AuthorRef
    Kind      session.MessageKind
    Text      string
    Tags      []string
    Payload   json.RawMessage
    ReplyTo   string
    Key       string           // idempotency key, "" allowed
}

// SessionTemplate is the minimum a new session needs. Memory is NOT a step:
// a member joins with whatever context the runtime grants it.
type SessionTemplate struct {
    Title     string
    Kind      string // "channel" | "direct"
    Namespace string
    Members   []session.MemberRef
}

// Handler serves POST /ingress/messages.
type Handler struct {
    Repo     session.Repository
    Inbox    InboxWriter      // narrow: Deliver(ctx, agentID, capability string, msg InboxMessage) ([]session.DeliveryOutcome, error)
    Dispatch DeliveryDispatch // nil-able; see COURIER-HARNESS §4
    Idem     IdempotencyStore // records (namespace, key) -> prior Result
    Now      func() time.Time
}

// Result is what the handler persists under the idempotency key and returns.
type Result struct {
    Mode           Mode                     `json:"mode"`
    SessionID      string                   `json:"session_id,omitempty"`
    MessageID      string                   `json:"message_id"`
    Seq            int64                    `json:"seq,omitempty"`
    Outcomes       []session.DeliveryOutcome `json:"outcomes,omitempty"`
    CreatedSession bool                     `json:"created_session"`
    Delivery       DeliveryReport           `json:"delivery"`
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request)
```

`internal/session.MemberRef` is the existing participant shape; if it is named differently in
tree, use the existing type and do not introduce a second one.

## 3. Data Model

No new tables for the message itself: a session message is appended through the existing
`Repository` (`NextSeq` → `CreateSession` when new → `AddMember` per member → append message
record). Reuse `LockMessage(sessionID, key)` so a duplicate ingress call cannot double-append.

One new record kind, written only for inbox mode:

```
ingress.route  (JSONL store: one line; SQL store: row in chat_messages with kind=ingress.route)
{
  "id": "r_...",
  "agent_id": "alpha",
  "capability": "build",          // "" when addressed at the agent
  "message_id": "m_...",          // the inbox entry id returned by Inbox
  "author": {"agent": "ci-bot"},
  "tags": ["build"],
  "created_at": "2026-11-03T21:40:00Z"
}
```

Idempotency is recorded beside the session store, one row per key:

```
ingress_idempotency (SQL) / .coding-hermes/ingress-idempotency.jsonl (JSONL)
  namespace TEXT, key TEXT, request_hash TEXT, result_json TEXT, created_at TIMESTAMPTZ
  PRIMARY KEY (namespace, key)
```

If a replay arrives with the same key and an identical `request_hash`, return the stored
`result_json` with the same status code. If the hash differs, `409 INGRESS_KEY_REUSED`.

## 4. Wiring

`cmd/server/main.go`, next to the session routes at the line that registers
`r.HandleFunc("/sessions", sessHandler.HandleCreateSession).Methods("POST")`:

```go
r.HandleFunc("/ingress/messages", ingressHandler.ServeHTTP).Methods("POST")
```

The route sits inside the authenticated subrouter, so it inherits the same bearer check as
`/sessions`. It must NOT be added to the public path list (`/health`, `/version`,
`/openapi.json`, `/openapi.yaml`, `/docs`).

`docs/openapi.yaml`: add the path with the two response shapes, plus component schemas
`IngressRequest`, `IngressResult`, `SessionTemplate`. `make readme-api-check` must stay green,
so bump the README's path/operation counts in the same commit.

Config: none required. Optional `ingress.max_body_bytes` (default `1048576`) and
`ingress.allowed_modes` (default all three) are read through the existing config struct; a mode
excluded there returns `403 INGRESS_MODE_DISABLED`.

CLI: `crier ingress send --mode new_session --template-title "..." --text "..."` is **out of
scope for this row**; the HTTP surface is the contract and the CLI is a thin client added later
if a caller needs it.

## 5. Error Catalog

| Code | Status | Condition |
|---|---|---|
| `INGRESS_INVALID_JSON` | 400 | Body is not JSON, or has unknown top-level fields (strict decode). |
| `INGRESS_MODE_REQUIRED` | 400 | `mode` missing. |
| `INGRESS_MODE_UNKNOWN` | 400 | `mode` not one of the three. Never defaulted to `session`. |
| `INGRESS_MODE_DISABLED` | 403 | `mode` excluded by `ingress.allowed_modes`. |
| `INGRESS_SESSION_REQUIRED` | 400 | `mode=session` with empty `target.session_id`. |
| `INGRESS_TEMPLATE_REQUIRED` | 400 | `mode=new_session` with no `target.template`. |
| `INGRESS_TEMPLATE_INVALID` | 400 | Empty title, `kind` outside `channel|direct`, or a namespace that does not exist. |
| `INGRESS_AGENT_REQUIRED` | 400 | `mode=inbox` with empty `target.agent_id`. |
| `INGRESS_AGENT_UNKNOWN` | 404 | `mode=inbox` and the agent is not registered. |
| `INGRESS_AUTHOR_REQUIRED` | 400 | `author` empty. |
| `INGRESS_AUTHOR_NOT_MEMBER` | 403 | `mode=session` and the author is neither a participant nor permitted to post (CHAT-PERMISSIONS). |
| `INGRESS_TASK_KIND_REFUSED` | 400 | `kind=task`; use `/sessions/{id}/tasks`. |
| `INGRESS_REPLY_TOO_DEEP` | 409 | `reply_to_thread` does not exist in that session, or would deepen a thread (CHAT-SESSIONS §4.5 — a reply stays in thread). |
| `INGRESS_SESSION_NOT_FOUND` | 404 | `mode=session` and no such session. |
| `INGRESS_KEY_REUSED` | 409 | Same idempotency key, different request hash. |
| `INGRESS_BODY_TOO_LARGE` | 413 | Body exceeds `ingress.max_body_bytes`. |
| `INGRESS_TAGS_INVALID` | 400 | See COURIER-TAGS §5 (bad grammar, too many, too long). |
| `INGRESS_STORE_ERROR` | 500 | Backend append failed; nothing is claimed delivered. |

Every error body is the project's existing `{"error":"CODE","message":"..."}` shape.

## 6. Edge Cases

- **new_session race:** two concurrent identical keys → `LockMessage(namespace+":"+key)`; the
  loser waits, then returns the winner's stored result. Exactly one session is created.
- **Partial member add:** if `AddMember` fails for the third of five members, the session and the
  already-added members stay (they are real records), the message is **not** appended, and the
  response is `500 INGRESS_STORE_ERROR` naming the member. A retry with the same key resumes
  rather than duplicating: the key is recorded only on success.
- **Empty text with a payload:** allowed when `payload` is non-empty (a structured message).
  Both empty → `400 INGRESS_EMPTY_MESSAGE`.
- **`reply_to_thread` on `new_session`:** `400 INGRESS_REPLY_TOO_DEEP`; a fresh session has no
  thread to reply into.
- **Inbox + tags:** tags are stored on the route record and passed to the inbox entry so inbox
  filters (COURIER-TAGS) work for queued messages too.
- **Harness dispatch fails:** the message is stored and its `outcomes` are real; `delivery`
  reports `{"mode":"harness","harness":"opencode","state":"failed","error":"..."}`. The call
  returns 201 — storage is the contract, dispatch is best-effort and reported honestly.
- **Auth-enabled relay:** covered by the shared bearer; no per-agent token is minted here.

## 7. Testing

Unit (`internal/ingress`):
1. `TestIngressModeRequired` — no mode → 400 `INGRESS_MODE_REQUIRED`; nothing appended.
2. `TestIngressModeUnknownNotDefaulted` — `mode:"channel"` → 400; assert no session created.
3. `TestIngressNewSessionCreatesAndPosts` — template with 2 members → 201, one `session.create`,
   two `session.member.add`, one message, `created_session:true`, outcomes length 2.
4. `TestIngressNewSessionIdempotent` — same key twice → one session, one message, identical body.
5. `TestIngressKeyReusedDifferentBody` — 409 `INGRESS_KEY_REUSED`.
6. `TestIngressSessionModeRequiresMembership` — author not a participant → 403.
7. `TestIngressInboxModeUnchangedBehaviour` — `mode=inbox` produces the same inbox entry id and
   ack semantics as `POST /agents/{id}/inbox` today (regression row: proves the old path is not
   altered).
8. `TestIngressTaskKindRefused`, `TestIngressEmptyMessage`, `TestIngressBodyTooLarge`.
9. `TestIngressPartialMemberAddLeavesNoMessage` — injected failure on member 3: session + 2
   members exist, zero messages, 500.

HTTP (`cmd/server` integration, real server, real store):
10. `TestIngressRouteRegisteredAndAuthenticated` — 401 without bearer on an auth-enabled relay,
    200/201 with it.
11. `TestIngressAppearsInTranscript` — after a `new_session` ingress, `GET /sessions/{id}/messages`
    returns the message at seq 1 with the same `message_id`.
12. `TestIngressDispatchesToHarnessWhenConfigured` — fake dispatcher records one call and the
    response carries `state:"dispatched"`.

Manual/live (documented in the row's evidence):
13. curl the three modes against a scratch server; capture the three responses and the transcript.

## 8. Hilo Impact

Depends on: `internal/session` (`Repository`, `Message`, `Audience`, `DeliveryOutcome`),
`internal/session` inbox handler (unchanged, reused), `internal/config`, `internal/httperr`.
Depended on by: `COURIER-HARNESS.md` (dispatch hook), `COURIER-TAGS.md` (tags carried here),
`COURIER-READS.md` (unread is computed over the messages this route creates).
Nothing existing changes behaviour: the route is additive and the inbox path is called, not
reimplemented.
