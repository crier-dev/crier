# CHAT-INTERFACE.md — the crier comms interface (spec of record)

Status: **DRAFT v2** · 2026-10-03 · Owner: Bane · Tickets: **CR-CHAT-001** (this document — the spec of
record for the series) and **CR-CHAT-009** (the web-client MVP this document scopes)
REVISION v2 · 2026-10-03 — folded Bane's detail pass into this document. **Added:** the channel /
direct-message / NAMED-GROUP surfaces, with the curated-group ≠ dynamic-capability rule stated where the
reader meets it (**CR-CHAT-013**, decision **D8**); an attachment carried **by reference** in the mapping
table only (**CR-CHAT-014**, **D9**, designed by `CHAT-PERMISSIONS.md` / `CHAT-STORAGE.md`); the
nested-thread + request→thread flow as this interface's **acceptance test** (**CR-CHAT-015**); the
late-join context-share prompt (**CR-CHAT-016**, **D10**); the navigability block — depth collapse,
timeline rail, generated per-thread summaries, search that returns LOCATION (**CR-CHAT-017**, second
half); and the three message **kinds** — plain / addressed / TASK-ACTION — with a tag as addressing only
(**CR-CHAT-018**, **D12**). **Corrected:** the depth rule (**CR-CHAT-017**, decision **D11**) — a
**reply STAYS IN THREAD**; agents talking to each other inside a thread are messages at the SAME level,
and a sub-thread exists only when **deliberately branched**. v1 left this open (`CHAT-SESSIONS.md` §6
item 5) and its reply-record mechanism (a reply's `parent_id` = the immediate parent) could be read as
"a reply-to-a-reply is a deeper level". **That reading is wrong**; it is corrected in place in
`CHAT-SESSIONS.md` §4.2 / §4.3 / §4.5 and its §6 item 5 is now ANSWERED. Nothing was changed silently:
every correction is stated at its point of correction and the new material is additive to v1.
Source material: the four approved UI options generated 2026-10-03 —
`A` channels + threads, `B` operator console, `C` triage inbox, `D` chat + capability map
(`~/.hermes/cache/images/openai_codex_gpt-image-2-medium_20261003_1243{19,30,31,32}_*.png`).
Per **CR-CHAT-011** those images are **feel / layout references only — their lettering is placeholder**.
An element below is cited by the label the image actually DRAWS where a label is drawn; where an image
renders its text as unreadable placeholder boxes (option `C` does this for most strings), this document
says so instead of inventing a label.
Companions in this series (different owners, not written here): `specs/CHAT-SESSIONS.md` (the
session + threading half of this document), `specs/CHAT-PERMISSIONS.md` (CR-CHAT-003),
`specs/CHAT-ADDRESSING.md` (CR-CHAT-004), `specs/CHAT-STORAGE.md` (CR-CHAT-006), and two rows authored
concurrently by their own owners — `specs/CHAT-FEDERATION.md` (CR-CHAT-023 + CR-CHAT-024) and
`specs/CHAT-TRUST.md` (CR-CHAT-024 + CR-CHAT-025). Neither of those two is on disk as of this line; they
are named here, and indexed in `_index.md` with the same honesty, so this document's boundaries cannot be
misread as gaps in the series.
Precedent: `specs/NAMESPACES.md`, `specs/A2A-OPTION.md` (same rigor + format).

---

## 1. Purpose & scope

### 1.1 What this document is normative for

This document is the **spec of record for crier's human↔agent comms interface**. It fixes:

1. **The object model** a comms interface is built from — Principal, Agent, Session, Thread, Address,
   Grant, Namespace (§2) — as *definitions*, so that five sibling specs cannot each mean something
   different by "session".
2. **The UI surface's shared structure** across all four approved options: the zones, the components,
   the states, and the colour roles (§3) — **structure, not a skin.** Bane has not picked an option;
   the normative content is what all four agree on plus the honest record of where they differ.
3. **The mapping from every drawn UI element to the crier primitive or endpoint that serves it** (§4),
   with `NOT BUILT` stated where nothing serves it yet **and what is owed named**.
4. **The interface's acceptance test** (§1.4) — the one test the whole interface is judged against
   (CR-CHAT-015), stated in Bane's own terms.
5. **The additions of the v2 detail pass** (§3.8): channels / direct messages / named groups
   (CR-CHAT-013), navigability without depth (CR-CHAT-017), message kinds (CR-CHAT-018) and the
   late-join context prompt (CR-CHAT-016) — each mapped to its endpoint and its true status in §4.
6. **The invariants** (§5) that no later row in this series may break.
7. **The decisions of record** for the decisions that belong to this document (§1.3).

### 1.2 What this document is NOT normative for

- **Not the session/threading model.** The Session object, membership, the transcript, the fan-out rule
  and reply semantics are `specs/CHAT-SESSIONS.md` (CR-CHAT-002, CR-CHAT-005). §2 defines the objects
  only far enough to be unambiguous.
- **Not permissions.** Principals-as-users, roles, grants, the delivery ACL and the four role names are
  `specs/CHAT-PERMISSIONS.md` (CR-CHAT-003). Where a drawn badge needs a role name to be quoted, this
  document quotes the image and defers the semantics.
- **Not the address grammar.** The parser and the resolution rules for `@agent`, `@team:x`, `@cap:y`,
  `@ns/*`, `#session` are `specs/CHAT-ADDRESSING.md` (CR-CHAT-004).
- **Not storage.** The dual backend, the JSONL bundle, the reconciler and the divergence rule are
  `specs/CHAT-STORAGE.md` (CR-CHAT-006).
- **Not a pixel design.** No spacing, radius, font, breakpoint or hex value is normative here; the
  legible companion mock is CR-CHAT-011.
- **Not a new API.** This document adds no route. It is the reason no route may be added that a
  documented public client could not already call (§5.1).

### 1.3 The decisions of record (D1–D6) and where each is recorded

CR-CHAT-001's acceptance requires each decision to name the alternative it beat. The six are split
across the series; each is recorded **at the place it is binding**, and each is restated here so the
spec of record is a usable index:

| # | Decision | Recorded in | Alternative it beat, and why |
|---|---|---|---|
| **D1** | The session is an **aggregate over durable inbox deliveries**, not a new bus: a message to a session fans out through the same guarded dispatcher as `POST /agents/{id}/inbox`, reusing guard / webhook / durable write / lease / ack / TTL / dead-letter primitives. Local webhook fan-out waits for a blocking transport result before recording `delivered`; a rejection is `refused`. Lease / ack / TTL apply to inbox deliveries, not webhook deliveries; remote session members use the session federation forwarder, not the local dispatcher's unknown-agent fallback. | `CHAT-SESSIONS.md` §3 (owner) | *A session-scoped queue and its own delivery path.* Rejected: it would be a second delivery path with its own truth — the exact thing §5.1 forbids — and would lose offline semantics, TTL expiry receipts and the federation fallback that the inbox already has. |
| **D2** | The human is a **`Principal`**, distinct from an Agent, and speaks **AS a bound Agent**. | `CHAT-PERMISSIONS.md` (CR-CHAT-003/007) — not this document | *A human holding an ed25519 key.* Rejected: hostile UX (the bus's own contract says keys are the agent's), and no per-human audit. |
| **D3** | A **`Grant`** binds a principal to an agent / group / session / capability, and delivery is refused without one (a named refusal, not a silent drop). | `CHAT-PERMISSIONS.md` (CR-CHAT-003) — not this document | *Trust-by-reach (today's posture: anyone who can reach the port can deliver to anyone).* Rejected: fine for a fleet, hostile for multi-team humans. |
| **D4** | Addressing is a tag grammar (`@agent`, `@team:x`, `@cap:y`, `@ns/*`, `#session`) resolved server-side; the capability tag resolves to **one holder**, never a fan-out. | `CHAT-ADDRESSING.md` (CR-CHAT-004) — not this document | *A capability tag that fans out to every holder.* Rejected: it changes delivered semantics, lease/ack consequences and sender expectations; `POST /capabilities/{capability}/inbox` is a selector, and §4 below pins the UI to that. |
| **D5** | **Where it ships:** the chat surface ships as a **client of the stock server** — a browser client (option: served by crier itself, under the existing server binary/landing surface) that calls only documented public REST/WS. | this document, §5.1 + §7 | *A separate service with its own backend (a bespoke BFF).* Rejected: a second truth, and the MVP's own acceptance (CR-CHAT-009) requires a full round-trip against a stock crier with no private endpoints. This document does not decide *static files from the existing binary* vs *a separately hosted client of the same API* — both satisfy §5.1; §6 carries it. |
| **D6** | **Dual backend:** JSONL is the **ordered append log and the transport form**; PostgreSQL is the **query view built from it**. No two-way live writes. | `CHAT-STORAGE.md` (CR-CHAT-006) — referenced by `CHAT-SESSIONS.md` §5 | *Two independent stores with live two-way writes.* Rejected: distributed-transaction territory; a conflict loses either way, and a mismatch would be silent drift instead of a finding. |

### 1.3.1 Decisions added by this revision (D10, D11, D12)

The decision ledger has since grown past D6. The decisions of record that bind **this document** beyond
D1–D6 are **D10–D12**; their normative text is in §3.8 and in `CHAT-SESSIONS.md`, and each names the
alternative it beat:

| # | Decision | Recorded in | Alternative it beat, and why |
|---|---|---|---|
| **D10** | **Late-join context** is an explicit choice — `none` / `summary` (**default**) / `since <message-id>` / `full` — recorded on the thread as a system event naming the mode and the boundary message id. | §3.8.4 + `CHAT-SESSIONS.md` §4.6 | *Quietly hand every joiner the whole history.* Rejected: an agent given everything is indistinguishable from one given nothing, so its answer cannot be explained and "what did it know when it replied" stops being answerable. |
| **D11** | **Addressing a non-member does NOT spawn a sub-thread.** A reply **stays in thread**; agents talking to each other inside a thread are messages at the **same level**; a sub-thread is created **only when deliberately branched** (an explicit action or shortcut) — never automatically and never merely because a tag was used. | §3.8.2 + `CHAT-SESSIONS.md` §4.5 | *Auto-spawn a level on a tag, or on every reply.* Rejected: it makes depth a function of who replied and how many replied, which fragments one conversation into a tree nobody asked for. **This reverses a v1-era recommendation and is stated as a correction, not quietly applied** (revision line, above; `CHAT-SESSIONS.md` §6 item 5 now ANSWERED). |
| **D12** | **A tag is ADDRESSING, not an action.** Three message kinds — plain / addressed / TASK-ACTION — are structurally distinguishable on the wire, in storage and in the transcript; only TASK-ACTION may create work or carry a lifecycle. | §3.8.3 + `CHAT-SESSIONS.md` §4.4 | *Treat `@agent` as "execute this".* Rejected: conflating *addressed* with *task* is how a chat becomes a system that silently does things — a tag would become a remote command and an addressed reply would fan out to strangers. |

Decisions **D8** (group kinds) and **D9** (asset permissions) are recorded by `CHAT-ADDRESSING.md` /
`CHAT-PERMISSIONS.md` and `CHAT-STORAGE.md` respectively; **D13/D14** and **D16** are recorded by the two
companion specs named at the top of this document. This document only states the two consequences the
reader of §2 and §4 needs — that a **named** group and a **capability** target are different address
kinds and are never conflated, and that an attachment is a **reference** — and does not restate their
design, which is not its to state.

### 1.3.2 Blocking webhook answers in the transcript (DF-CRIER-341)

A successful blocking webhook delivery's text-mappable `reply` is appended through the existing
session message store as a **same-thread reply authored by the target agent**. The stored record is
`session.thread.reply`, with `message_kind: plain`, `author: {"agent": "<target-id>"}`,
`session_id` / `thread_id` copied from the delivered message, `parent_id` pointing to that message,
and `payload: {"text": "<answer>"}`. It receives its own message id, sequence and timestamp. It does
not deliberately branch a thread, create a task, or impersonate the operator.

Text-mappable means a non-blank JSON string (including a schema-extracted reply or a raw plain-text
body), or an object with a non-blank string `text` or `reply` field (`text` takes precedence).
The single [outcome vocabulary](CHAT-SESSIONS.md#delivery-outcome-vocabulary)
owns the wire names, negative-marker precedence, evidence requirements and
answer-bubble rules. This section owns the local blocking text mapping and display:
a non-blank JSON string (including a schema-extracted reply or raw plain-text
body), or an object with a non-blank string `text` or `reply` (`text` first).

Runtime diagnostics retain `reason` and `error` without repeating identical
strings. Absent failed diagnostics use `webhook reported FAILED`; absent canned
diagnostics use the canned/demo default. An unreadable blocking body records
`reply body had no displayable text field; keys: <sorted top-level keys>`;
keys are comma-separated, or `(none)` without object keys. No body values are
stringified into an answer. Missing/null runtime flags are not false markers.
Diagnostics persist on send and transcript reads as `detail`. The reply has an
explicitly empty audience and no outcomes, so appending it is not another send
and cannot fan out to session members. None of this proves task completion.

The existing transcript poll and renderer display `payload.text`, `author.agent` and `parent_id`;
there is no separate reply store or rendering path. This implements D1 without losing the agent's
answer and preserves D11's same-thread attribution and D12's distinction between a plain reply and
an explicit task. Async/batch modes, A2A, guard ordering and delivery idempotency are unchanged.

### Session send authority and task completion controls (DF-CRIER-376, DF-CRIER-379)

2026-10-19 — The chosen option is signature enforcement, not a browser exemption.
The production session message, task creation, claim/complete and compile entrypoints
honor CR_REQUIRE_AGENT_SIG (default true) before replay, branch creation, sequence
allocation, transcript writes or dispatch. An unsigned request under enforcement is
HTTP 401 SESSION_SENDER_SIGNATURE_REQUIRED: "this deployment requires a signed
agent request; this browser cannot sign. No message was recorded." Every offered
proof is checked by the existing registry verifier even when enforcement is off;
invalid/partial/stale proof is SESSION_SENDER_SIGNATURE_INVALID. A valid signing
agent must equal the resolved sender, otherwise HTTP 403
SESSION_SENDER_IDENTITY_MISMATCH. An agent signature does not authenticate a human
principal or an as_agent binding. An enforced handler missing its verifier fails
closed with HTTP 503 SESSION_SENDER_SIGNATURE_UNAVAILABLE. The browser displays
these named refusals and retains the unsent draft. It obtains no private keys and
adds no authentication flow: DF-CRIER-333 remains the open owner decision on browser
authentication, so a Bearer header alone is not an enforced session-send grant.

When an operator explicitly sets CR_REQUIRE_AGENT_SIG=false, absent proof is
permitted by the deployment's existing access policy (Bearer when enabled, reach
access otherwise). The server stamps sender_identity_basis: sender-identity-claimed,
meaning deployment-asserted attribution, NOT key-verified identity. Accepted signed
requests instead stamp sender-identity-verified. This additive disclosure is stored
with the message, survives JSONL/SQLite/PostgreSQL projection reload, keyed replay
and empty audiences, and appears on POST/transcript GET and in the page. Historical
records lacking it show "sender identity basis not recorded", never inferred proof.
The legacy method/path/timestamp signature does not bind a body without the existing
optional X-Agent-Body-SHA256 header; exact raw request bytes are retained for that
check. Outcome names and publisher-identity-* labels remain unchanged. The visible
send-authority text reads require_agent_signature and auth_enabled from /status;
missing/failed policy reads disclose unknown rather than claiming an auth posture.

An open task offers Claim, never Complete. A claimed/running task shows a small
completion-evidence textarea and Complete; only the current claimed agent owner can
use that button. The required non-empty evidence is sent as payload.text. Task reads
expose owner_kind alongside owner so matching principal/agent spellings do not enable
the wrong actor. TASK_EVIDENCE_REQUIRED and TASK_OWNER_MISMATCH retain their server
diagnostics in the affected row after refresh. Evidence drafts survive background
reads. The browser harness claims and completes through these controls, reads back
the exact evidence, and its selftest removes the input text to prove the real 409
TASK_EVIDENCE_REQUIRED still rejects completion. No agent runtime execution is
claimed by that harness evidence.

### Operator posture and outcome reporting (DF-CRIER-377, DF-CRIER-378)

The embedded `/chat` page shows an always-visible Runtime posture block directly below its
Health header, outside Connection & identity. It consumes `/status` fields `registry_backend`,
`registry_durable`, `session_backend`, `auth_enabled`, `a2a_enabled` and `build.version` /
`build.commit` (plus `build.modified`). Registry durability is reported by the server: false
for memory, true for file-backed SQLite/PostgreSQL, null for an unknown backend. A non-durable
registry is described beside Health as "agents are lost on restart". Session backend and
registry durability are independent facts; durable session storage does not preserve memory
agents. Missing fields render "unknown"; a failed status read replaces stale posture with an
explicit unknown/error. No backend compatibility list is maintained in the client.

Send notices count each exact outcome name separately (for example `leased: 1; refused: 1`),
without changing outcomes or HTTP message shapes. Only `delivered` contributes to the server's
existing `delivered` counter. Only `refused` contributes to `skipped` / `skipped_agents`;
All other states retain their exact names from the
[owning vocabulary](CHAT-SESSIONS.md#delivery-outcome-vocabulary), rather than
being reclassified as delivered or refused.

`failed`, `canned` and `unreadable_reply` are negative in both transcript and send notice and
retain the draft and reply context, including mixed outcomes. Existing refused/budget-cut
negative styling and all-refused/failed/budget-cut retry eligibility remain unchanged. Keeping
a draft is not a retry grant: canned/unreadable or mixed results still clear the old request
key, and the notice warns that sending the retained draft records a new message. Delivery,
replay and storage rules are unchanged.

### Delivery cuts and retry evidence (DF-CRIER-354)

A blocking delivery whose full response did not arrive inside its per-target budget
has outcome `budget_cut`, never `delivered` or `refused`. This includes a response
whose HTTP 200 headers or partial body arrived before the budget, but whose body
read was cut. Upstream 408/504 timeout responses are cuts too. A target answering
503 is `refused`, even when repeated refusals/backoff consume the entire budget.
The existing driver enforces the same per-target deadline; no second timer is added.

The outcome `detail` persists on POST and transcript GET. It reports the actual
attempt count (initial call included), configured budget and whether that budget
was exhausted. A cut explicitly says the answer did not arrive inside the budget,
that partial or late answers are discarded, and names `RUNTIME_TIMEOUT_MS` /
`CR_SESSION_DELIVERY_TIMEOUT_MS` or retry as remedies. Keep runtime strictly less
than session; neither default nor maximum changes. Cuts suppress answer bubbles
and are excluded from both delivered and skipped/refused summary counts. The
transport cancels the response read; no detached callback can later append an
answer or replace the cut with success. Retrying all-negative outcomes includes
`budget_cut`, retaining the original intent and operator row (DF-CRIER-349).
The existing `canned` / `failed` verdict handling (DF-CRIER-348) is unchanged.

### In-flight delivery feedback (DF-CRIER-414)

Outcomes are recorded when a fan-out RETURNS, so a blocking delivery can spend
its whole per-target budget with nothing on the wire to show for it. The composer
therefore carries an explicit in-flight line, and the only values it may render
are the ones the server publishes for the message it is delivering —
`dispatch_live` on the transcript read
([CHAT-SESSIONS.md §3.2](CHAT-SESSIONS.md#in-flight-window)): the elapsed
milliseconds measured server-side, the per-target budget, each target's attempt
count, and that attempt's own failure class and reason.

1. The line reports PER TARGET, and every value is the server's own reading. A
   client may not render a countdown, an estimate, or an elapsed time it computed
   itself, and may not report a target unreachable unless the runtime said so.
2. A dial the runtime already knows failed (refused, no route, name resolution)
   is named while the budget is still running — the target and the transport's
   own reason — instead of being deferred to the terminal outcome.
3. It refreshes at least once a second while a pass is in flight.
4. The terminal outcome is unchanged and still comes from the recorded
   `outcomes`. `dispatch_live` disappearing IS the end of the pass: the in-flight
   line is REPLACED by the recorded outcome in the existing vocabulary (with its
   `detail`), and nothing is left spinning after the budget.
5. A target that has produced no attempt is reported as having produced none —
   never as a failure. A slow endpoint is not an unreachable one, and the budget
   is not shortened to make the panel lively.

### Busy dispatch and diagnostic ordering (DF-CRIER-353, DF-CRIER-358)

A held registry delivery idempotency key yields `in_progress`, with
`code: "DELIVERY_IN_PROGRESS"`, the existing `target` and `retryable: true`.
This is neither refusal nor delivery evidence, so neither summary count includes it.
The UI uses a pending warning rather than refusal styling and tells the operator to
refresh for confirmation, not send again. Retryability describes the condition;
it does not authorize duplicate work. Same-key session POST remains pure replay
while an outcome is `in_progress`; no automatic reconciliation is implied.

Outcome diagnostics add `code` (when known) and `retryable` to the existing human
`detail`. They survive transcript reads and storage reopen through the existing
JSONL record / SQL request projection; no table migration is required. Historical
records without diagnostics remain readable. A busy dispatch's HTTP 409 body is
unchanged; fan-out consumes its typed dispatch code, not a guessed status match.
A completed refusal leads with the target's status, for example
`target answered status 503; 2 attempts, 1.2s delivery budget exhausted: ...`
(`code: "TARGET_HTTP_503"`). A cut instead leads with
`answer did not arrive inside the budget: ...` (`code: "DELIVERY_BUDGET_CUT"`).
Neither human detail embeds the serialized registry JSON error or leads with
crier's wrapper 502/504. All existing outcome names and retry eligibility remain.

### 1.3.3 Retried message sends and browser waiting (DF-CRIER-343)

`POST /sessions/{id}/messages` honours a nonempty `idempotency_key` within that session.
The first send records the key with its message; later sends with the same key return HTTP 201
and the existing message's `id`, `seq` and audience. Replay actions, including
missing/unknown outcome evidence, are defined only in
[CHAT-SESSIONS.md §3.2](CHAT-SESSIONS.md#delivery-outcome-vocabulary). Incomplete
outcomes never authorize another delivery, even if the retried payload differs. When ALL persisted
outcomes are `refused`, `failed` or `budget_cut`, retry actually invokes the original resolved
targets with the stored intent and replaces outcomes on that SAME operator row
(`replay_action: "redelivered"`, whether the new attempt succeeds or not). A live
successful answer may append its own agent reply; canned/failed answers still
suppress bubbles (DF-CRIER-348). Original sends omit `replay_action`. Retry uses
an attempt-specific delivery key derived from each persisted prior outcome's time,
so an HTTP-successful runtime FAILED receipt cannot shadow the new attempt. The
message/entry IDs remain stable; one operator row does not promise exactly-once
execution after a timeout at a remote runtime.
A different key appends a new message. An absent/empty key preserves the append-on-every-send
contract. The same key in another session is independent. Realm/auth checks still precede replay.

The repository locks the session/key before looking up the transcript and holds it through the
intent, fan-out and final outcome update. Independent SQLite/JSONL handles and processes share
an OS advisory file lock (released on process death); PostgreSQL uses a database advisory lock
on a dedicated connection, not a pooled data slot. In-memory SQLite uses a per-handle semaphore.
SQL projections commit the intent and request-key metadata in one transaction. The transcript,
not an expiring reservation, remains the durable authority after restart. If a prior attempt died
after recording intent, replay returns its current outcomes and durable dispatch evidence
with the state defined by that table rather than resending.

The page keeps its 12-second HTTP wait, but a timed-out POST is **not** reported as a failed send:
it displays “Still delivering or awaiting confirmation”, retains the draft and refreshes the
transcript. For session-message POSTs this is a wait-only timer, not an AbortSignal:
the fetch remains attached and drains its eventual response, so a 12–25s runtime
is not canceled by the page. Other calls retain their aborting timeout. This is the simpler honest option: `CR_SESSION_DELIVERY_TIMEOUT_MS` is a per-target
budget, not a guarantee for the complete request including federation and persistence. Guessing
a larger fixed browser timeout would recreate the mismatch. Message retries of the unchanged
draft/author/realm/session/routing reuse one request key; an all-refused/failed/budget_cut
response also keeps that draft/key and explicitly offers another delivery attempt.
A successful/nonretryable send or changed draft gets a new key. The server budget
now defaults/caps at 30000ms and startup enforces `RUNTIME_TIMEOUT_MS` (default
20000, range 1..25000) strictly less than `CR_SESSION_DELIVERY_TIMEOUT_MS`
(range 1..30000), naming both numbers on contradiction. The runtime declaration
must match on server and consumer; remote environments are not discoverable.
Fan-out concurrency and the verdict checks before reply recording are unchanged.
Task creation is not made idempotent by this change; after a timed-out task POST the page tells
the operator to check for the task before retrying.

### 1.4 The acceptance test this interface is judged against (CR-CHAT-015)

This is the whole interface's acceptance, in Bane's own terms (2026-10-03), because it is the problem the
interface exists to solve:

> *"I want to talk to the same agent on the same project but different features … this interface allows
> me to shape multiple messages when I send a request to the agents, make that thread and then reply in
> that chat, so I can talk about different parts. Top level, but each thread breaks down deeper."*

Stated as a test, on **one project with one agent**:

1. **Two features are discussed in two PARALLEL threads with no cross-talk.** A message in one thread is
   not delivered to, ordered against, or rendered inside the other.
2. **The top level shows the feature conversations distinctly** — as separate threads, not as one
   interleaved stream of messages.
3. **A request becomes a thread.** Sending a request does not append to a flat log: it opens (or joins) a
   thread whose root is that request, and the replies live inside it.
4. **A thread nests deeper** — the tree supports at least three levels.
5. **Every message in the tree is reconstructable with its exact parent** from the transcript alone
   (`CHAT-SESSIONS.md` §4.3).

If Telegram could pass that test, this interface would not be needed. Tests 1–3 are what the
channel/thread structure owes (CR-CHAT-013/015); test 4 is bounded by the depth rule (D11 — a level is
**earned**, §3.8.2); test 5 is `CHAT-SESSIONS.md` §4.3.

---

## 2. The objects

Seven terms from v1, plus three added in v2 (Channel, Direct Message, Named Group), each one paragraph.
`Status` names what exists on the bus today.

**Principal** — a HUMAN user, distinct from an agent. A Principal can **speak AS a bound agent**.
*Status:* stored principal/role/status records, binding/grant management and admin-gated reads ship
(`internal/permissions`, `internal/registry/principals_api.go`, `permissions_read.go`; §7).
Browser login does not ship (DF-CRIER-333); an acting agent ID is not a principal credential.

**Agent** — a registered crier agent; it has a **CLASS** (`personal` = owned by one human, carries a
private context about its owner, default reach is its owner plus explicit grants; | `service` = shared /
scoped, may own systems, code or other resources, reachable per its scope), an **OWNER**, and
**capabilities**. Cross-owner is **DEFAULT-DENY**: I can talk to MY agents; talking to YOURS needs a
grant. *Status:* the registered agent, its `capabilities` and its derived presence (`online` / `stale` /
`offline`) are **SHIPPED**; the **`class`** and **`owner`** fields are **NOT BUILT** (owed to
CR-CHAT-003). Default reach is a permissions rule, not a UI rule.

**Session** — a **durable conversation**: its participants (principals + agents), its membership, and an
ordered transcript. *Status:* **PARTIAL.** A `session_id` already rides on the wire — the deliver body
(`session_id`, CR-FEAT-004), the webhook envelope (`crier.session_id`), the federation hold record — and
is a guard policy key (`session:<id>`, `specs/LLM-MESSAGE-GUARD.md` §4.2). A **stored** session object
(participants, membership, an ordered transcript) is **NOT BUILT**; that is CR-CHAT-002 and
`CHAT-SESSIONS.md`.

**Thread** — a **reply subtree inside a session**. *Status:* **PARTIAL**, exactly as Session: `thread_id`
is on the deliver body and is a guard policy key (`thread:<id>`), but it is **not stored on the inbox
entry** (`registry.InboxEntry` carries id, agent_id, payload, created_at, expires_at, leased_at,
lease_id, acked, sender, idempotency_key, priority, guard, namespace — and no session/thread). So a
thread is **not reconstructable from stored messages today**; it is reconstructable from the transcript,
which is why the transcript is the record (CHAT-SESSIONS.md §3, §4.3).

**Address** — a **tag**: `@agent`, `@team:x`, `@cap:y`, `@ns/*`, `#session`. *Status:* **PARTIAL** — the
grammar and parser are **NOT BUILT** (CR-CHAT-004); the *agent* and *capability* halves already resolve
server-side (`GET /agents`, `GET /agents?capability=`, `POST /capabilities/{capability}/inbox`). The UI
draws the popover; the tags it offers must be resolvable tokens, never display-only strings (§4).

**Grant** — an **ACL entry** binding a principal to an agent / group / session / capability.
*Status:* **SHIPPED** in `internal/permissions`; `POST /grants` creates, `GET /grants` lists current
unexpired/unrevoked grants and `POST /grants/{grantid}/revoke` revokes. Session permission/role reads
and a read-only browser rendering also ship (§4 rows 22–23, §7); browser sign-in and a grant editor do not.

**Namespace** — the **realm / tenant wall**, already shipped: auth posture (`shared` | `token` with
`token_ref`), rate limits (`rate_limit_per_minute`), guard settings (`guard_enabled`, `guard_policy`) and
retention (`retention_seconds`) per realm, plus migration 008's NULL-means-default storage. *Status:*
**SHIPPED** (`specs/NAMESPACES.md`, CR-FEAT-029). It is the outer wall in this series: a session lives in
exactly one realm, and no surface here may bridge two.

**Channel** *(v2)* — a **project/namespace-scoped top-level room** (CR-CHAT-013): the place a project
lives, and the top level the CR-CHAT-015 flow needs ("top level … each thread breaks down deeper"). A
channel is **not a new object** — it is a **session** whose `kind` is `channel` (`CHAT-SESSIONS.md` §1.2):
its top level is the project conversation, its threads carry the detail, and it is realm-scoped exactly
like every other session (no channel bridges two realms). *Status:* **NOT BUILT** — it is the session
object with a `kind`, owed to CR-CHAT-002 and its route to CR-CHAT-019. The drawn evidence is `A`'s
session index and `C`'s conversation cards; neither says "channel" and neither is treated as a label
source.

**Direct Message** *(v2)* — a **1:1 conversation between a Principal and one Agent** (CR-CHAT-013). Like a
channel it is **a session** with a `kind` (`direct`), not a third primitive: one membership model, one
transcript, one fan-out rule. It exists so "talk to this one agent about this one thing" stops being a room
everyone can see. *Status:* **NOT BUILT** — owed CR-CHAT-002 (the session object + `kind`) and CR-CHAT-003
(what a DM implies for grants, which this document does not decide).

**Named Group** *(v2)* — a **curated, editable set of agents that one message can target at once**
(CR-CHAT-013, decision **D8**). It is **not a session** and **not a role**: it has no transcript and no
state of its own. The distinction this document must never let muddy, because both are called "groups":

> A **NAMED group** is **CURATED**: an explicit roster, inspectable and editable by whoever holds the
> grant, and a message to it routes to the **CURRENT** members — the set is enumerable before the send.
> A **CAPABILITY target** is **DYNAMIC**: it already ships as a **SELECTOR**
> (`POST /capabilities/{capability}/inbox` — one live holder per delivery, round-robin). Both are
> addressable — `@team:x` vs `@cap:y` — and the two are never conflated, on the wire or in the UI
> (§3.7 item 4 pins the same rule for `D`'s halo).

*Status:* **NOT BUILT** — no group object, no roster and no group fan-out exists today. Owed: CR-CHAT-013
(the surface), CR-CHAT-022 (the group object + roster), CR-CHAT-004 (the `@team:x` grammar), CR-CHAT-003
(who may edit a roster). The drawn evidence is `A`'s "Add participants" group rows (`Engineering` 4 …) and
`D`'s `@Infra-Team` "Team · 6 members" row; their names and counts are placeholder.

---

## 3. The UI surface

### 3.1 The citation rule

The four images are **layout references whose lettering is placeholder** (CR-CHAT-011). This section is
therefore normative for **structure** — which zone exists, what it holds, which component is in it, which
states exist — and never for a mock's wording. Every element below carries the option(s) it was read
from: `(A)`, `(B)`, `(C)`, `(D)`. An element drawn **only** in one option is marked as that option's
variant, not as a requirement of the shared shell. Where an image is vague or its text is unreadable,
that is stated.

Width fractions are estimates read from the images and are **not** normative: `A`'s left rail reads
≈18–20 %, centre ≈54–56 %, participants panel ≈25–28 %; `B` reads ≈18 / 55 / 25 % under a full-width top
bar; `C` is a ≈8–10 % icon rail plus a ≈90 % card list; `D` is two ≈45 / 55 % columns.

### 3.2 The shared shell — zones

All four options are one shell with four zones, plus a fifth that only `B` and `D` draw.

| Zone | What it holds | Drawn in | Notes |
|---|---|---|---|
| **Z1 Status strip** (full width, top) | Breadcrumb (namespace → session), a **queue-depth** figure with a trend glyph, an overall **health** word with a dot and a chevron, a timestamp | **`B`** only | `D` draws the same *idea* as three per-panel stat cards inside Z4 (Agents 8 / Capability Groups 3 / Active Links 12). `A` and `C` draw no status strip. |
| **Z2 Left index column** | An index of *something*, plus nav and a workspace/tenant switcher | **`A`** (indexes **sessions**), **`B`** (indexes **agents**), **`C`** (icon rail + **conversation cards**) | The three disagree on what the index is; §3.5 records this as the options' main structural difference. `D` has no Z2 (its left column is Z3). |
| **Z3 Centre: room** | Session header, transcript, thread controls, tag chips, composer, mention popover | **all four** | The only zone every option draws. |
| **Z4 Right inspector** | One inspector at a time: participants (`A`), permission matrix + audit trail (`B`), capability map (`D`) | `A`, `B`, `D` | `C` draws no Z4 — its card list *is* the whole surface. |
| **Z5 Overlays** | Mention / command popover above the composer; a floating compose action | `A` (popover), `C` (floating action button), `D` (popover) | `B` draws **no** popover (its composer has no autocomplete in the image). |

### 3.3 Components

**Z2 — index column.**
1. **Workspace / tenant switcher** — a logo glyph, a workspace name, a subtitle with an agent count and
   an environment word, and a chevron; a pencil affordance beside it *(A: "Orion Lab", "12 agents ·
   Production")*.
2. **Primary nav** — four items in `A` (a conversations item with a count badge, an agent library, tools
   and integrations, settings) and five glyph-only items in `C` (a mail/inbox item with a mint status
   dot and an active bar, then chat, group, box and document glyphs). **`C`'s nav labels are drawn as
   unreadable placeholder boxes — no label is recorded for them.**
3. **Index rows** — `A`: robot avatar + presence dot + title + truncated preview + right-aligned time
   (10 rows drawn). `B`: robot avatar + name + a presence dot **with a status word** (`online` / `idle` /
   `degraded`) + a right-aligned capability chip (12 rows drawn, header count "AGENTS (12)"). `C`:
   conversation **cards** with a 1–2 robot avatar cluster, a two-line title/snippet, a right relative
   timestamp (`5m ago` … `1d ago`), a **role pill** with a coloured left stripe (`MEMBER`, `VIEWER`,
   `ADMIN`), a **stacked-layers glyph with a count** (3, 1, 7, 2, 4, 9) and, on two rows, an **amber
   shield with an exclamation mark**; the selected row carries a green curved **reply/return arrow** and
   an inline duration fragment drawn as placeholder boxes with a colon (`□□:□□ □□ □□`).
4. **Filter chips** (`C`) — five chips, each a label plus a count, only the counts legible:
   `(12)`, `(8)`, `(4)`, `(3)`, `(1)`; the first is the active chip. The labels are unreadable.
5. **Search** — `A` a search field with placeholder "Search messages, agents, or tools…" and a `⌘ K`
   keyboard chip; `C` a magnifier field whose placeholder is unreadable; `B` a "Search agents…" field in
   the *participants* section of Z4.
6. **A create affordance** — `A` a pencil icon beside the workspace row and a compose icon in the
   "Sessions" header; `B` a "+ New" button in the agent header; `C` a mint circular floating action
   button with a pencil glyph bottom-right.

**Z3 — room.**
7. **Session header** — avatar; the title (`A` "Build Plan"; `D` "SESSION" + subtitle "#A7F3 ·
   Multi-Agent Task"); a **privacy indicator** (`A` draws a padlock glyph beside the title, unlabelled);
   a **state badge** (`B` draws a green outlined `ACTIVE`; `D` draws a dot + the word `Live`); the session
   id and a start timestamp (`B`); audience counts (`A` "Participants (6)" lives in Z4; `D` "6
   participants · 4 agents"); a **view selector** (`D` "Thread view" with a chevron); and an overflow
   affordance (`A` and `B` draw three dots; `C` has none).
8. **Message** — author avatar with presence dot, author name, a **role / permission badge**, a
   timestamp, a body bubble, an optional **embedded block** (`A` draws two, labelled `txt` and `bash`,
   with line numbers and a copy glyph; `D` draws card-shaped blocks, §3.3 below), and a per-message
   overflow (`D` draws `•••` on each message).
9. **Message meta line** (`B` only) — a latency figure in milliseconds with a lightning glyph, a
   **lease** word (`active` / `closing`) and an **ack** state (a tick + `delivered`).
10. **In-message status / progress cards** (`D` only) — a spinner card with a label and a sub-line
    ("Running data sweep…" / "3/3 regions ・ ~2m"), a progress card with a label, a percentage and a bar
    ("Drift check in progress" / "62 %"), and a completion line with a tick ("Task queued ・ Will notify
    3 members").
11. **System event separator** (`B` only) — a rule with the words `SYSTEM EVENT` and a timestamp, and an
    event sentence (e.g. "Lease renewed for session …", "Session … marked complete"). Label text is
    placeholder; the *shape* — a non-author, non-message row that states something the bus did — is the
    normative part.
12. **Thread control** — a reply-count pill under the root message (`A` "4 replies" with an expand /
    collapse chevron) and the **connection visual**: a thin vertical rail descending at the replies'
    left edge with short horizontal **elbow** segments into each reply, replies indented about one avatar
    width (`A` and `D` both draw the rail + elbows; `D` draws connectors between nested levels). No
    per-reply reply-count and no per-reply collapse control is drawn in either.
13. **Tag chips** (`A` only) — a row of five hash chips above the composer (`# planning`, `# infra`,
    `# costs`, `# security`, `# release`) plus a circular `+` button; the first chip is visually
    accented. The image does not say whether these are filters, applied tags or suggestions.
14. **Composer** — a text input (`B` draws a placeholder, "Type a message to the agent bus…"); a mention
    or attachment affordance at the left (`A` an `@` token and a paperclip; `D` a `+` button, a paperclip
    and an emoji glyph); a **target chip** (`B` "Agent Bus" with a chevron); a keyboard hint (`A`
    "Shift + Enter for newline"); a send control (all of `A`, `B`, `D` draw a green send button; `A` adds
    a split/dropdown chevron beside it).
15. **Mention / autocomplete popover** — `A`: a header `Agents`, then rows of icon + handle + descriptor
    + right-aligned count, the first row highlighted: `@dev-agents` "Engineering agents" 4,
    `@ops-agents` "Operations agents" 3, `@research-agents` "Research agents" 2, `@design-agents`
    "Design agents" 2. `D`: four rows of icon + handle + a **type-prefixed** descriptor + a coloured
    presence dot: `@Infra-Team` "Team • 6 members • Ops & Platform", `@Data-Capabilities` "Capability
    Group • 3 agents • Data & Analytics", `@Kappa` "Agent • Data Processing", `@Echo` "Agent •
    Summarization". Neither image draws group headers beyond the descriptor line, or a keyboard hint
    inside the popover; `A`'s popover is clipped at the image edge.

**Z4 — inspector.**
16. **Participants list** (`A`) — header "Participants (6)" with an add-person affordance, then rows of
    avatar + presence dot + name + role badge + a capability line (`Atlas` / ADMIN / "Planning ·
    Architecture · Tools"); a section "Add participants" with a "Search agents…" field, then **group
    rows**: a group icon + name + count (`Engineering` 4, `Operations` 3, `Research` 2, `Design` 2,
    `Security` 2); and a **status card**: dot + "All systems operational" + "6/6 agents online" + a
    signal-bars glyph.
17. **Permission matrix** (`B`) — header `PERMISSION MATRIX` + an `Edit` button; columns `Principal`,
    `send`, `read`, `invite`, `admin`, `mint`; eight rows (seven robots + one row drawn with a **human
    silhouette** named `admin`), each cell a green tick `✓` or a muted dash `—`. Drawn content: every
    agent row is `✓ ✓` on send/read; `invite` is ticked for two agents and the human row; `mint` is
    ticked for one agent and the human row; one agent has a dash on `send`. There is no `unknown` cell,
    no lock, no cross and no deny glyph anywhere in the matrix.
18. **Audit trail** (`B`) — header `AUDIT TRAIL` + an `All Events` filter; columns `Time`, `Principal`,
    `Event`, `Details`; ten reverse-chronological rows carrying events such as `session.start`,
    `message.send`, `lease.renew`, `session.complete`, `permission.grant`, `agent.degraded`,
    `queue.depth`, `heartbeat`. Event names and the last column's values are placeholder-grade.
19. **Capability map** (`D`) — a header `AGENT / CAPABILITY MAP` + "Session #A7F3 · Live topology"; three
    stat cards (8 Agents, 3 Capability Groups, 12 Active Links); a canvas with a central **hub node**
    labelled with the session, 9 agent nodes each with a robot glyph, a coloured dot and a permission
    badge (`admin` / `invoke` / `read`); **solid** lines from the hub to four nodes and **dashed** lines
    to five; a green **halo** around a three-node cluster labelled `Data-Capabilities` with the subtext
    "3 agents • One tag. Many agents."; a **legend** (Active connection / Idle connection / Capability
    group halo / Permission: admin / Permission: invoke / Permission: read); and a callout card ("One
    capability. Multiple agents. Greater reach.").

### 3.4 States

The five states named in the brief, and what the images actually draw:

| State | Drawn? | Where, and how |
|---|---|---|
| **empty** | **NO** | None of `A`/`B`/`C`/`D` draws an empty state — no empty room, no empty index, no zero-result search. An MVP that only renders the happy path is the state the images leave undefined. |
| **loading** | **`D` only** | The spinner card and the progress card (item 10). `A`, `B` and `C` draw nothing for in-flight work. |
| **unread** | **counts only** | `A`'s nav badge (3) and `C`'s filter chips (12)(8)(4)(3)(1) are counts; **no per-message or per-row unread marker is drawn anywhere.** |
| **awaiting-reply** | **`B`, `C`, `D`** — three different renderings | `C`: the selected row's green curved reply/return arrow plus an inline duration fragment (label unreadable). `B`: the meta line's `lease: active` / `lease: closing` + `ack: ✓ delivered` — i.e. waiting is rendered as *the delivery's lease state*. `D`: a completion line ("Task queued ・ Will notify 3 members"). `A` renders it not at all. |
| **flagged** | **`A`, `C`** | `A`: an amber shield glyph plus an outlined amber pill `Flagged` on one reply. `C`: an amber shield containing `!` on two index rows. `B` and `D` draw no flag. |

### 3.5 Where the four options differ (structure, not skin)

| # | Difference | `A` | `B` | `C` | `D` |
|---|---|---|---|---|---|
| 1 | What Z2 indexes | sessions | agents | conversations (triaged) | — (Z2 absent) |
| 2 | Z1 status strip | absent | present | absent | partly, as Z4 stat cards |
| 3 | Z4 inspector | participants | permission matrix + audit trail | absent | capability map |
| 4 | Threading visual | rail + elbows, reply-count pill, indentation | none (messages are a flat, timestamped history) | none (a **count** per card, meaning unlabelled) | rail + elbows between nested levels |
| 5 | Delivery state made visible | — | lease + ack meta line | — | spinner / progress cards |
| 6 | Permission surface | role badges (ADMIN / MEMBER / VIEWER) + a padlock | a matrix with six permission columns | a role pill per card | role badges + a map legend |
| 7 | Mention popover | 4 **agent-group** rows with counts | none drawn | none drawn | 4 rows **typed** team / capability group / agent, with presence dots |
| 8 | Composer | input + `@` + `Shift + Enter` hint | placeholder + a target chip | **no composer drawn** | input with a live tag + a `+` attach |

**What is therefore locked as structure:** one shell; a room that is always Z3 with a header, a
transcript, a thread visual, a composer and an address popover; presence on every author; a per-message
*outcome* the bus can report (lease/ack for agents, a flag for the guard); an index column whose
subject is an option's choice over the same conversation objects; and one inspector drawer whose
subjects are participants, permissions, audit and topology. **What is NOT locked:** which option wins,
which inspector is default, whether Z1 exists, whether the index is sessions or agents, and the words on
any badge.

### 3.6 Colour roles

Brand roles (Bane, 2026-10-03): **charcoal `#0b0d12`** background, **mint `#4cc38a`** accent / success,
**lavender** and **amber** as the two secondary accents, and **robots as the mascot** (every agent is a
stylised robot head in `A`, `B`, `C` and `D`; `B` and `D` use a **human silhouette** for a human
principal — `B`'s matrix row named `admin`, `D`'s `Orion` and `Lumen` avatars). **No image uses a danger
/ red role** in the UI itself (red appears only in `A`'s macOS window-control dots). Roles are therefore:
charcoal background, a lifted charcoal panel surface, primary text near-white, secondary text muted
grey, mint for selection / presence / send / success, lavender and amber for agent and warning accents,
muted grey for the `VIEWER` role and for dashed / inactive lines. Hex values are **not** normative here;
the pair above is.

### 3.7 Contradictions between the images, and which side this document follows

1. **`A`'s role badge tint is inconsistent with the role name.** `MEMBER` is drawn purple for one agent
   and green for another. **Followed:** the *role word* is normative and a badge's tint is a per-agent
   accent — a colour that contradicts its own label could not be a normative encoding, and
   `specs/CHAT-PERMISSIONS.md` owns the role set.
2. **`D` contradicts itself on the `admin` colour.** The `admin` chip is drawn green-tinted in the thread
   and on the map node, while `D`'s own legend assigns **amber** to "Permission: admin". **Followed:
   the legend**, because a legend is an explicit statement of the encoding while a tint is decoration.
   Consequence: `D`'s amber nodes and dots mean *admin*, and its green ones are presence or an active
   link — not the same channel.
3. **`B` draws the permission matrix with a human row and seven robot rows, but no other image draws a
   principal at all.** **Followed:** the human row is evidence that a Principal is a **participant kind**
   (agreed with §2), not evidence of a second matrix schema.
4. **`D`'s capability halo implies fan-out ("3 agents • One tag. Many agents." with solid lines to four
   nodes), while the shipped capability route is a SELECTOR: one holder per delivery, round-robin over
   live holders, `404 NO_CAPABLE_AGENT` when there is none.** **Followed: the shipped contract.** The
   halo and the group count mean *"this tag has N live holders; one of them takes each delivery"* — the
   UI must never draw a capability tag as a broadcast, because the bus does not broadcast it.
5. **`A`/`C` draw amber shields with two different meanings** (`Flagged` on a message; `!` on an index
   row) and neither explains it. **Followed:** both are the guard's verdict surfaced on content
   (`guard.Meta.decision` ≠ `allow`, with `risk_level` / `reason`), which is the only shipped signal
   that is *about a message*; §6 records that an index-row flag may instead mean something about the
   session and is undetermined.
6. **`B`'s audit trail names events (`permission.grant`, `agent.degraded`, `lease.renew`,
   `session.complete`) that no route emits.** **Followed:** the *panel* is locked as structure; the
   event vocabulary is explicitly **NOT BUILT** (§4) and must not be treated as a target list to
   implement blindly.

---

## 3.8 Structure added by the v2 detail pass

Four blocks, added on Bane's 2026-10-03 detail pass. Each is **structure** (not skin); each has a mapping
row in §4; and none of them is drawn in any of the four images — where a block extends a drawn element,
that is stated. §3.1's citation rule is unchanged: no wording is taken from a mock.

### 3.8.1 Channels, direct messages and named groups (CR-CHAT-013)

- **A channel list** — the Z2 index's subject may be **channels** (project rooms), not only sessions or
  agents. A channel is the project's **top level**; the feature conversations live in its threads, which
  is the shape §1.4's test 2 requires.
- **A direct-message entry point** — a way to open or continue a **1:1 with one agent**. It is the same
  room object with a `kind` (`CHAT-SESSIONS.md` §1.2), so it is one list, one membership model and one
  transcript — not a second inbox.
- **Named-group rows and a group picker** — `A`'s drawn "Add participants" group rows (`Engineering` 4 …)
  and `D`'s typed `@Infra-Team` "Team · 6 members" row become **addressable** targets: a message sent to a
  named group is delivered to each **current** member of that curated roster, through the existing
  per-participant path (`CHAT-SESSIONS.md` §3.4 rule 7).
- **The curated-vs-capability rule in the UI** (the same rule §3.7 item 4 pins for `D`'s halo, restated
  because the picker is where it is easiest to get wrong): a **named group** row may show its roster and
  its count because both are data; a **capability** row must be drawn as "**one holder takes each
  delivery**", never as a broadcast. The two are distinguishable at a glance (a roster glyph vs a
  capability glyph) because they behave differently under the same word, "group".

### 3.8.2 Navigating depth without getting deep (CR-CHAT-017, second half)

Under the depth rule (D11, `CHAT-SESSIONS.md` §4.5) the default shape of a conversation is **flat**: a
reply stays in thread. Depth exists only where someone deliberately branched. Navigation therefore has two
jobs — show the branches that **do** exist, and never make the reader walk them:

- **Depth collapse** — below a configurable depth (**default 3**) a branch collapses into an **expandable
  summary card**. The default is a number with a meaning, not a magic constant: three is the deepest a
  reader is asked to follow before the interface offers to summarise instead.
- **A timeline rail** — the branch structure of the thread **and where the reader currently is**. This is
  the same rail §3.3 item 12 draws for reply elbows, given a whole-thread job.
- **Per-thread and per-sub-thread summaries** — always an **INDEX**: generated, visibly **marked as
  generated**, with the raw messages **one click underneath**, and **never** replacing the record
  (`CHAT-SESSIONS.md` §4.7 fixes the data property; this section fixes the surface).
- **Search that returns LOCATION** — a hit is a **path**, not a fragment:
  `namespace > channel > thread > sub-thread > message`, filterable by **agent**, **capability**,
  **has-attachment** and **unresolved**.

**SHIPPED (DF-CRIER-448):** every message offers Reply in thread (unchanged flat
attribution) and Branch from here (a deliberate new child thread). Both submit the exact
selected message as `parent_id`, including a reply. Branch sends additionally name `branch:true`;
the persisted child root uses the existing thread anchor rather than pretending a root is a reply.
The transcript read exposes that root's `anchor_message_id`, while `thread_depth` remains the
server's deliberate-branch depth, never `reply_depth`. Reply/branch return links and the branch
rail open the exact message, reveal its containing collapsed group if needed, scroll and focus its
stable `message-<id>` article. The rail marks the current message on navigation and transcript scroll.
The depth selector defaults to `DEFAULT_THREAD_COLLAPSE_DEPTH = 3` (the same zero-based threshold
as the API's `DefaultCollapseDepth`): branches at depth >= 3 become generated index cards.
Each count is computed from the messages actually withheld in the current filtered view; expanding
reveals that complete group, including its deeper branches, but not a sibling group. Expansion survives
polling, not a session switch; changing the threshold resets expansion. This is an informational
index over records, not an error or a summary of model-authored content. Explicit task creation keeps
its existing thread behavior; the composer names that restriction if task is selected for a branch.
No search surface or new summary-generation mechanism is introduced by this change.

### 3.8.3 Message kinds in the stream (CR-CHAT-018, D12)

The transcript renders **three kinds distinctly** (`CHAT-SESSIONS.md` §4.4 owns the wire and storage
side):

1. **PLAIN MESSAGE** — a message in a thread; no obligation.
2. **ADDRESSED MESSAGE** — a tag marks the **intended reader**. **Nothing executes**, and nothing fans
   out to a third party as a task. This is the default meaning of a tag, and it is drawn as emphasis on
   the addressed participant, never as a command.
3. **TASK / ACTION** — the explicit "do this" kind, **rendered differently** (a distinct card, not a
   bubble) because it is the **only** kind that may create work or carry a lifecycle.

**This is a safety property, not a styling choice.** A UI that renders an addressed message the way it
renders a task teaches the operator that a tag runs things. A tag must not be clickable into execution,
and the task affordance must be an explicit, separate action. §5.3 states it as an invariant.

### 3.8.4 The late-join context prompt (CR-CHAT-016, D10)

Adding an agent (or a human) to a thread **in flight** asks **how much context to share** — **none** /
**summary** (default) / **since <message-id>** / **full** — and the answer is **recorded on the thread as
a system event** naming the **mode** and the **boundary message id** (`CHAT-SESSIONS.md` §4.6 owns the
record). The surface must state the **boundary**, not just the word: "summary" alone cannot explain a
reply whose substance came from the summary's edge. A later change to the mode is permitted and is itself
recorded.

**SHIPPED (DF-CRIER-447):** the add-agent control asks only when the selected session contains
messages, with all four modes and summary selected. Since requires a boundary message ID and
cannot submit without it. Summary/full's inclusive last-included boundary and since's exclusive
lower boundary are returned from the recorded decision and shown in server-authored system-event
text, interleaved by seq. Empty-session joins record none with a nothing-to-share reason and no
invented boundary. Human joins use the same API; this browser control still accepts agent IDs only.
Later changes are recorded by the existing context PUT API, not a new browser editor.

---

## 4. The mapping table

Every UI element, the crier primitive or endpoint that serves it, and its status. `SHIPPED` = exists on
the bus today. `PARTIAL` = the primitive exists but not in the shape the UI needs. `NOT BUILT` = nothing
serves it; **the owed thing is named.** Route names below are the shipped surface (see
`docs/openapi.yaml`); no route in this table is invented.

| # | UI element (and where it is drawn) | Served by | Status |
|---|---|---|---|
| 1 | Z2 index of **agents** (`B`; the roster header "AGENTS (12)" + "+ New") | `GET /agents` (list), `GET /agents?capability=` (filter), `POST /agents` (register), `DELETE /agents/{id}` (unregister) | **SHIPPED** |
| 2 | Index row **capability chip** (`B`: routing / summarize / research / vision / memory / planning / data / execution / image / guardrails / tools / analysis) | the agent's `capabilities[]` on its registry row | **SHIPPED** |
| 3 | Agent **presence dot + status word** (`B` online / idle / degraded; `A`, `C`, `D` dots only) | derived status `online` \| `stale` \| `offline` from `last_seen` vs `CR_PRESENCE_STALE_AFTER_S` (CR-FEAT-024), read via `GET /agents`; the connection table via `GET /mesh/peers`; the window is published in `GET /status` field `presence_stale_after_s` | **RESOLVED (CR-CHAT-020)** — the bus's real enum is `online` \| `stale` \| `offline` and there is no honest `degraded` signal, so the word is **DROPPED** from the interface; the client renders the three shipped words, mapping the mock's `idle` to `stale`. No UI element may render a status the API cannot produce. |
| 4 | **Queue depth** figure + trend glyph (`B`) | `GET /status` field `queue_depth` + `GET /status` field `queue_depth_history` (CR-CHAT-020) | **SHIPPED (CR-CHAT-020)** — the trend/sparkline is a client projection of `queue_depth_history`: a timestamped, oldest-first ring of up to 60 samples of the SAME measurement `queue_depth` reports (process-lifetime; null until the first sample). |
| 5 | **Health** word + dot + chevron (`B` "System Healthy"; `A`'s status card "All systems operational") | `GET /health`, `GET /status` field `health` (CR-CHAT-020) | **SHIPPED (CR-CHAT-020)** — `GET /status` field `health` carries the graded word `ok` \| `degraded` \| `down`: `ok` while the serving registry backend answers its probe, `degraded` when it does not, `down` reserved for the process itself failing to answer (GET /health's word — the process cannot honestly say it about itself). A chevron that reveals detail remains a client composition over the status card's existing fields. |
| 6 | **"6/6 agents online"** aggregate (`A`) | `GET /status` field `agents_online` + `GET /status` field `agents_total` (CR-CHAT-020) | **SHIPPED (CR-CHAT-020)** — the aggregate is THE count: `agents_online` / `agents_total`, counted with the SAME presence derivation `GET /agents` reports every row's status with (one instant, one rule — no client-side recount that could compute the figure two ways). |
| 7 | Z1 **breadcrumb** namespace → … → session (`B`) | `GET /namespaces` for the realm half; `GET /sessions` + `GET /sessions/{id}/messages` for the session half | **SHIPPED** (CR-CHAT-019) — both halves resolve; the trail is a client composition over two documented reads. |
| 8 | **Session list row** (title, preview, relative time) (`A`) | `GET /sessions` | **SHIPPED** (CR-CHAT-019). A *view* over the same session objects; a row carries the title, the last message's `payload.text` preview and its time. |
| 9 | **Conversation card** + filter chips with counts (`C`) | `GET /sessions` with `state`, `kind`, `min_messages`, `min_participants` filters | **SHIPPED** (CR-CHAT-019) — a *view* over the same objects, narrowed; not a new object. |
| 10 | **Floating compose** action (`C`), compose icons (`A`), `+ New` (`B`) | `POST /sessions` (CR-CHAT-019); `POST /agents` exists for agents | **SHIPPED** for sessions/rooms. |
| 11 | **Session header** — title, id, start timestamp, audience count, view selector, overflow | the session object (`GET /sessions`, `GET /sessions/{id}/messages`) | **SHIPPED** (CR-CHAT-019). The object carries title, id, `created_at` (start), `audience_count`, `participant_count`, `state` and `visibility`; `session_id` is no longer a wire tag only. |
| 12 | **State badge** and close/reopen system events | `POST /sessions/{id}/state`; session `state` / `closed_at` and transcript `system_events` | **SHIPPED backend (DF-CRIER-446) and honest control (DF-CRIER-460)**; the page reads server state and `/status`, disabling lifecycle actions with a bound-principal reason when the ACL is enabled or unknown. `{state, sender}` remains usable only with the ACL off and signatures not required. Contract: CHAT-SESSIONS.md §4.5a. |
| 13 | **Privacy indicator** — the padlock beside `A`'s title | the session object's `visibility` field + the `403 VISIBILITY_FORBIDDEN` refusal on the reads | **SHIPPED** (CR-CHAT-019) — a `private` session is readable only by a member or by a principal holding a CR-CHAT-003 read grant. |
| 14 | **Transcript message** — author, body, timestamp | `GET /sessions/{id}/messages` (the ordered cross-agent transcript); `GET /agents/{id}/inbox` returns `InboxEntry` (id, agent_id, sender, payload, created_at, expires_at, priority, namespace, thread_id, guard); the deliver body carries `payload`, `sender`, `session_id`, `thread_id`, `request_id`, `kind` | **SHIPPED** (CR-CHAT-019) — an ordered, cross-agent transcript read exists (seq order, each with a resolvable parent), and the per-agent durable read is unchanged; the body is still `payload` (`payload.text`). |
| 15 | **Thread reply-count pill + rail/elbows + indentation** (`A`, `D`) | `thread_id` on `POST /agents/{id}/inbox` and now persisted on the stored record (`InboxEntry.thread_id` + the session backends); `GET /sessions/{id}/messages` serves `thread_id`, `parent_id`, `root_id`, `thread_depth`, `reply_depth`; guard policy key (`thread:<id>`) | **SHIPPED** (CR-CHAT-019) — a thread is reconstructable from storage ALONE (no `InboxEntry` field was stored before this row). |
| 16 | **Stacked-layers count** per card (`C`, values 3/1/7/2/4/9) | none — meaning unlabelled in the image | **NOT BUILT**. Unresolved by the images: whether it counts replies, files, versions or participants (§6). |
| 17 | **Message meta line** — lease word, ack tick (`B`) | `GET /agents/{id}/inbox` returns `lease_id`, `leased_count`, `queue_depth`; `POST /agents/{id}/inbox/ack` releases a lease | **SHIPPED** (the state is real; it is simply not attached to a session-scoped transcript yet). |
| 18 | **Per-message latency** (`B` "⚡ 86ms") | `InboxEntry.delivery_ms` on `GET /agents/{id}/inbox` (CR-CHAT-020) | **SHIPPED (CR-CHAT-020)** — the deliver path records its whole milliseconds per message; a timed delivery carries `delivery_ms` on the stored entry and the durable read. An untimed entry omits the field (not measured ≠ zero). |
| 19 | **SYSTEM EVENT** rows — "Lease renewed … (+5m)" (`B`) | leases are taken per retrieve with `lease` / `lease_seconds` and released by ack | **RESOLVED (CR-CHAT-020)** — no lease **renewal** operation exists and none is built (it would change lease semantics broadly for one UI element). The element is **DROPPED**: the UI vocabulary for lease state is **lease-taken / lease-expired / acked** only. |
| 20 | **SYSTEM EVENT** rows — expiry and unacknowledged loss | `GET /agents/{id}/inbox/dead-letters` (reason `ttl_expired_unacked`, 7-day retention) plus the `MESSAGE_EXPIRED` receipt written into the **sender's** inbox | **SHIPPED** — this is the real "something terminal happened to a message" surface, and it is a **dead letter, not a moderation flag**. |
| 21 | **Audit trail** panel (`B`: Time / Principal / Event / Details) | `GET /sessions/{id}/audit` (`internal/session/audit_http.go`); separate opt-in delivery log at `GET /delivery-log` / `GET /delivery-log/verify` | **SHIPPED read API (CR-CHAT-021)** over session events plus session-subject grant events when wired. JSONL retains the full membership log; SQL projects membership facts and retains lifecycle events (`audit.go`). The embedded page is not the full reference-image audit panel. |
| 22 | **Permission badge / role pill** | `GET /sessions/{id}/roles`; stored principal role/status from `GET /principals`; `internal/session/audit_http.go` / `internal/registry/permissions_read.go` | **SHIPPED read data**. Session membership roles and principal namespace roles are distinct records. The browser shows session-role rows in Session permissions and stored principal fields in Authority; this is not browser authentication or an editor. |
| 23 | **Permission matrix** (`B`) | `GET /sessions/{id}/permissions`; `internal/session/audit_http.go` and `BuildPermissionMatrix` in `audit.go` | **SHIPPED read-only session matrix (CR-CHAT-021)**: active participant roles plus session grants, `send` / `read` / `invite` / `admin`, allowed cells labelled `role` or `grant`. `chat.html` renders the rows. No per-session mint column, deployment-wide effective-authority evaluator or matrix editor is claimed (§7). |
| 24 | **Participants list** + count + "Add participants" search (`A`; `D`'s audience counts) | `GET`/`POST /sessions/{id}/participants` (CR-CHAT-019); `GET /agents` (the roster) | **SHIPPED** — membership of a conversation is a recorded event, returned with its active flag and count. |
| 25 | **Group rows** with counts | `GET /groups`, `GET /groups/{name}` for curated rosters; `GET /agents?capability=` for a different, dynamic capability pool | **SHIPPED roster API**, not the embedded group UI: console count/roster/picker remains **OPEN DF-CRIER-449**. Reference-image counts are placeholders, not live measurements. |
| 26 | **Mention popover** — agent-group rows with counts (`A`) | `GET /agents?capability=` (or `GET /agents` for the full roster) | **PARTIAL** — resolvable today only if a "group" is in fact a capability name. The parser that turns a typed token into a resolution is CR-CHAT-004. |
| 27 | **Mention popover** — typed Team / Capability Group / Agent rows | agent/capability reads and named-group roster API (`GET /groups`) exist | **PARTIAL** — backend groups are not absent; the embedded named-group picker/roster workflow remains **OPEN DF-CRIER-449**. |
| 28 | **Capability tag delivery** — the map's halo and "One tag. Many agents." (`D`), and any `@cap:y` send | `POST /capabilities/{capability}/inbox` — resolves to **ONE** holder, round-robin over live holders (pool = the live holders when any exist, else all holders), `404 NO_CAPABLE_AGENT` when nobody advertises it, same guard / lease / ack / TTL / federation path as a by-id delivery | **SHIPPED** — and it is a **selector, not a fan-out** (§3.7 item 4). The UI must present it that way. |
| 29 | **Capability map** — nodes, solid / dashed links, halo geometry, legend, callout (`D`) | none (the index is data; the topology is not computed) | **NOT BUILT**. Owed: a topology read (which holders exist, which links are live) — arguably a client-side projection of `GET /agents` + `GET /mesh/peers`, which is a decision for CR-CHAT-009/010. |
| 30 | **Guard flag** — `A`'s amber shield + `Flagged` pill, `C`'s amber shield `!` | `guard.Meta` on the entry: `decision` (allow / block / sanitize), `risk_level`, `reason`, `matched_patterns`, `policy`, `provider`, `model`, `errored`, `quarantined`, `sanitized`, `quarantined_payload` — surfaced on `GET /agents/{id}/inbox`, on the deliver response, and as the uniform notice on a block | **SHIPPED** — this is the one "badge on a message" that is real, and CR-CHAT-009 requires it to be **visible, not silent**. |
| 31 | **Composer** text input + send | `POST /sessions/{id}/messages` (CR-CHAT-019); the deliver call: `POST /agents/{id}/inbox` with `payload` (+ `sender`, `request_id`, `delivery_mode`, `timeout_ms`, `priority`, `ttl_seconds`, `idempotency_key`) | **SHIPPED** — composing into a session fans out one delivery per participant through the shipped inbox path. |
| 32 | **Target chip** (`B` "Agent Bus") and the popover's resolved handle | the URL path's agent id, or the capability route (row 28) | **SHIPPED** as two routes; the unified "what am I sending to" control is a client composition. |
| 33 | **Keyboard hints** (`A` `⌘ K`, "Shift + Enter for newline") | none — client-local | **NOT BUILT** by design; no server meaning. |
| 34 | **Attachment / embedded block** (`A` `txt` and `bash` blocks; `D`'s cards) | `payload` is opaque (`json.RawMessage` on the deliver body / `[]byte` on the entry); the existing convention reads `payload.text` | **SHIPPED** as an opaque payload — crier defines **no** message-content schema, so a code block is the sender's shape rendered by the client. An **attachment** is carried **BY REFERENCE** (an asset id): the bytes live in object storage and the **wire never carries them** (CR-CHAT-014, D9). The reference *is* the shipped opaque payload; the **upload / fetch surface is NOT BUILT**, and its design — per-fetch authorization, bucket/prefix policy, GC — is `CHAT-PERMISSIONS.md` / `CHAT-STORAGE.md`, **not this document**. |
| 35 | **Tag chips** `# planning` … (`A`) | none — `InboxEntry` has no tag or label field | **NOT BUILT**. Owed: a decision whether a chip is a **relay topic** (fan-out, no history), a session label, or an address form — CR-CHAT-004 / CR-CHAT-005. |
| 36 | **Message overflow `•••`** (`D`), copy glyphs (`A`), paperclip, emoji, split-send chevron | none — client-local affordances | **NOT BUILT** by design; any action behind them (edit, delete, quote, react) is out of scope (§6). |
| 37 | **Add-participant** affordance (`A`) | `POST /sessions/{id}/participants` | **SHIPPED** (CR-CHAT-019 membership). |
| 38 | **Namespace / realm wall** behind every element above | `GET /namespaces`; realm decided at registration, delivery, transfer, relay publish + subscribe; per-realm auth posture, rate limit, guard settings, retention | **SHIPPED** (CR-FEAT-029) — and every session-scoped surface owed above must be realm-scoped on arrival. |
| 39 | **Channel list** — the Z2 index of **channels** (project rooms) (§3.8.1) | `GET /sessions?kind=channel` (CR-CHAT-019) | **SHIPPED** as the `kind` filter on the session list (CR-CHAT-019); a `/channels` alias is not built and is not owed. |
| 40 | **Direct-message entry point** — a 1:1 with one agent (§3.8.1) | the session `kind: direct` on `POST`/`GET /sessions` (CR-CHAT-019) | **PARTIAL** — the object, the create and the list ship as one room model with a `kind`; what a DM implies for GRANTS is still CR-CHAT-003. |
| 41 | **Named-group row / picker**, and curated-set fan-out | `POST`/`GET /groups`, `GET /groups/{name}`, `PATCH /groups/{name}/members`; `internal/session/groups_http.go` and session fan-out | **SHIPPED API and roster fan-out (CR-CHAT-013 / CR-CHAT-022)**. Console create/list/count/roster controls remain **OPEN DF-CRIER-449**. A group is not an agent credential; capability delivery remains a selector, not this fan-out. |
| 42 | **Depth collapse** below a configurable depth (**default 3**) into an expandable summary card (§3.8.2) | `GET /sessions/{id}/messages` carries `thread_depth` and `reply_depth` (CR-CHAT-019) | **SHIPPED** (DF-CRIER-448) — the browser collapse rule defaults to depth 3 (`DEFAULT_THREAD_COLLAPSE_DEPTH`, the same zero-based threshold as the API's `DefaultCollapseDepth`); a branch at depth >= the threshold renders as an expandable card stating exactly the count it withholds in the current filtered view, and expanding reveals that complete group only. The card is an index over records, not a model-generated summary — row 44 remains owed. |
| 43 | **Timeline rail** — branch structure + current position (§3.8.2) | `GET /sessions/{id}/messages` serves `thread_id` / `parent_id` / `root_id` / `thread_depth` (CR-CHAT-019) | **SHIPPED** (DF-CRIER-448) — the branch rail renders the session's branch structure and marks the current message (`aria-current`) on anchor navigation and on transcript scroll; every link opens its exact `message-<id>`, revealing a containing collapsed group first. |
| 44 | **Per-thread / per-sub-thread summary card** — generated, marked generated, raw messages one click under (§3.8.2) | none | **NOT BUILT**. Owed: a summary surface + a generated marker + the raw-message link (CR-CHAT-017). Who generates it and where it is stored is undecided (§6). |
| 45 | **Search that returns LOCATION** — `namespace > channel > thread > sub-thread > message`, filterable by agent / capability / has-attachment / unresolved (§3.8.2) | none | **NOT BUILT**. Owed: a search read over the transcript tree (CR-CHAT-017; whether it is an index or a scan is CR-CHAT-020, §6). |
| 46 | **Attachment chip / asset reference** on a message (§3.8, CR-CHAT-014) | none — the payload is opaque today (row 34) | **NOT BUILT**. Owed: the upload/fetch surface. An asset id rides the message; the **bytes live in object storage** and the wire never carries them — design is `CHAT-PERMISSIONS.md` / `CHAT-STORAGE.md`, not this document. |
| 47 | **Late-join context-share prompt** — none / summary / since / full, recorded as a system event naming the mode **and the boundary message id** (§3.8.4) | `POST /sessions/{id}/participants`; transcript `GET /sessions/{id}/messages` | **SHIPPED** (DF-CRIER-447): add-agent prompt for nonempty sessions, all four modes, boundary validation, durable join/change system events; human API shares the contract, browser editor for later changes not provided. |
| 48 | **Message-kind rendering** — plain vs addressed vs **TASK/ACTION**, visibly distinct (§3.8.3) | none | **NOT BUILT**. Owed: the three kinds distinguishable **on the wire, in storage and in the transcript** (CR-CHAT-018; the record field is `CHAT-SESSIONS.md` §4.4, and it must **not** overload the shipped deliver-body `kind`, which is the envelope kind — `message`/`configure`/`configure_ack`). |
| 49 | **Compile / merge a context bundle** — gather N messages (across threads or sessions) into ONE payload, hand it to an agent by tagging it, and expand a part back to its source (§4.8 of `CHAT-SESSIONS.md`) | `POST /sessions/{id}/compile` (merge + fan-out), `GET /sessions/{id}/messages/{mid}/expand` (citation → original) | **SHIPPED** (CR-CHAT-028) — the merged message is a **NEW** message whose every part carries a `{source_session_id, source_message_id}` **provenance citation**; a source the compiler could not read is omitted **with a named gap** (never silently included, never silently dropped) and a merge with nothing readable is **refused**; the compiled message rides the shipped inbox fan-out (§5.1, no second delivery path). Distinct from a quote/reply, which is an id pointer — both are needed. |
| 50 | **Composer in-flight line** — per target: elapsed vs the delivery budget, the attempts made, and an early unreachable/timeout signal naming the target and the transport's own reason (DF-CRIER-414) | `GET /sessions/{id}/messages` → `dispatch_live` on the message being delivered (DF-CRIER-414; the same read as rows 14 and 31) | **SHIPPED** (DF-CRIER-414) — process-local and never persisted: present only while THIS process is running the pass, absent after a restart, and gone the moment the pass's outcomes are durable. It carries no outcome; the terminal outcome is still the recorded `outcomes`. |

---

## 5. The invariants (the two from v1, plus the v2 safety invariant)

### 5.1 The UI is a CLIENT of the same REST/WS — no parallel truth, no second delivery path

Binding on every row in this series and on every client built from it:

- **Every UI action is a documented public API call.** No private endpoint, no bespoke backend-for-
  frontend, no server-side session state that the public API cannot read back. CR-CHAT-009's acceptance
  is exactly this: a full round-trip against a **stock** crier with no private endpoints.
- **No second delivery path.** A message reaches an agent through the existing deliver path and nowhere
  else — the guard choke point, the webhook driver, the durable inbox write, lease / ack / TTL,
  dead-lettering and the federation fallback are the *same* code, not a re-implementation. A session's
  fan-out is N calls into that path (CHAT-SESSIONS.md §3), not a session queue.
- **The rendered state must be reconstructable from the API.** If the client shows something the API
  cannot explain — a count, a badge, a status word — that is a parallel truth and it is a defect.
- **Both realtime lanes are already the bus's own.** Realtime arrives over the existing WS surfaces
  (`GET /relay/subscribe/{topic}` for relay fan-out; the mesh socket for mesh traffic). A browser client
  does not get a new socket protocol; if the existing lanes cannot serve the browser, that is a finding
  and a board row, not a second protocol.

### 5.2 The existing text and message paths must not regress

- **Additive only.** No existing route's request or response shape changes; no existing field changes
  meaning; no new auth demand appears on an existing route. Every session/thread field added to a record
  is `omitempty` so a pre-existing client's body stays byte-identical (the precedent is `priority` and
  `namespace` on the inbox entry).
- **The bus stays agent-first.** A Principal speaks *as* a bound agent, so nothing that consumes crier
  today has to learn a new participant kind (CR-CHAT-003/007).
- **The existing text paths keep working.** The relay, the inbox, the webhook driver, the MCP bridge and
  the A2A binding are untouched by this series; regressing any of them is a worse outcome than not
  shipping the UI. CR-CHAT-009's acceptance restates it: *the existing pages/landing are untouched.*

### 5.3 A tag is addressing, never an action (CR-CHAT-018, D12) — the v2 safety invariant

- **A tag on a message says who the message is for. It never says "execute this".** Tagging an agent
  produces an **addressed** message; nothing runs, no work is created, and no third party receives a task
  or a fan-out. A **TASK/ACTION** is a separate, explicit kind and the **only** kind that may create work
  (`CHAT-SESSIONS.md` §4.4 — the three kinds and their obligations).
- **The three kinds are distinguishable on every surface** — wire, storage, transcript and render. A
  client that cannot tell an addressed message from a task has turned a tag into a remote command, which
  is the failure this invariant exists to prevent; a UI that draws them the same is a defect, not a style.
- **Additive to v1's two invariants, and binding on every later row in the series** exactly as they are.
  Nothing here relaxes §5.1 (one delivery path, a client of the public API) or §5.2 (no regression).

---

## 6. Open questions — what the images do NOT determine

1. **Which option ships.** Bane has not picked; the four are not reconciled on Z2's subject (`A`
   sessions / `B` agents / `C` conversations) or on whether Z1 exists. CR-CHAT-011 produces the legible
   companion mock of the chosen direction — the choice is an owner decision, not a spec gap.
2. **The stacked-layers count in `C`** (3, 1, 7, 2, 4, 9) — replies, files, versions or participants.
   Unlabelled; not resolved by the images.
3. **`A`'s tag chips** (`# planning` …) — filters, applied labels, or suggestions? And is a chip a relay
   topic (fan-out, no history) or a session label? CR-CHAT-004/005.
4. **`C`'s index-row amber shield** — the same guard verdict as `A`'s message flag, or something about
   the *session* (a restriction)? This document follows the message-flag reading (§3.7 item 5).
5. **`C`'s selected-row green reply arrow + inline duration** — its label is unreadable. The reading
   here is "awaiting reply", with a duration; whether the duration is an SLA, a lease, or an age is
   undetermined.
6. **`B`'s per-message latency** (⚡ 86ms) — does the interface owe a per-delivery timing field, or is
   the element dropped? Not decided here.
7. **`B`'s `mint` column** — minting what (keys? agents? credentials?) — undefined in the image and
   owned by CR-CHAT-003.
8. **`C`'s left-rail nav labels** are unreadable placeholder boxes; the icons suggest inbox / chat /
   group / box / document but no label is recorded and none is invented here.
9. **`D`'s "Thread view" selector** — what the alternative view is (flat? by agent?) is still not drawn,
   but the question it gestures at is now **decided**: under D11 the default shape is **flat** (a reply
   stays in thread) and depth exists only where someone deliberately branched (§3.8.2, `CHAT-SESSIONS.md`
   §4.5). What remains open is only which *presentation* the selector switches between — a flat list and
   the branch rail are views over the same tree, not two shapes of the data.
10. **The legible-token requirement** — §4 rows 26/27 require every tag a popover offers to be
    resolvable. `A`'s `@dev-agents` is not resolvable today unless it is a capability name; whether the
    grammar invents group addresses or maps them onto capabilities is CR-CHAT-004's decision.
11. **Empty state** — undrawn in all four (see §3.4). Owed as a design task, most cheaply in CR-CHAT-011.
12. **Where the client is served from** (static files from the existing binary vs a separately hosted
    client of the same public API) — D5 fixes *that it is a client*; the packaging is undecided.

Added with the v2 detail pass — open, and deliberately not answered here:

13. **The named-group object's shape** — storage, roster ownership, and who may edit it (CR-CHAT-022 /
    CR-CHAT-003). This document fixes only that a named group is **curated** and addressable, and that it
    is never conflated with a capability target.
14. **Summary generation** (CR-CHAT-017) — who generates a per-thread summary, where it is stored, and
    whether it is cached. What is **not** open: it is an index, marked generated, with the raw messages
    one click underneath, and it never replaces the record (`CHAT-SESSIONS.md` §4.7).
15. **Search** (CR-CHAT-017 / CR-CHAT-020) — an index over the transcript or a scan. What is not open is
    that a hit returns its **location**, as a path, with the four filters.
16. **The share-mode boundary when the boundary message is itself summarised away** (CR-CHAT-016) — if
    `since <message-id>` names a message that has since aged out, the recorded boundary must still be
    resolvable or must be reported as a hole; the record is the requirement here, the resolution is not
    decided.
17. **Attachments** (CR-CHAT-014) — the upload path, per-fetch authorization, bucket/prefix policy and GC
    are **not designed in this document**; they belong to `CHAT-PERMISSIONS.md` / `CHAT-STORAGE.md`. The
    only claim made here is the one the mapping table states: an attachment is a **reference**, and the
    wire never carries the bytes.

---

## 7. Status

**Status: implemented in part · verified 2026-10-21 · DF-CRIER-445.** The DRAFT v2 label
at the top describes the design revision, not an absent implementation. This section is the current
implementation inventory; it supersedes the older build-status annotations elsewhere in the design.
A registered API, a stored object, a rendered browser control and authenticated authority are separate
claims. None implies the others.

### Shipped surfaces

- **Principal, binding and grant management ships.** `cmd/server/main.go` registers
  `GET /principals`, `GET /bindings` and `GET /grants` to `HandleListPrincipals`,
  `HandleListBindings` and `HandleListGrants` in `internal/registry/permissions_read.go`.
  `POST /principals`, `POST /bindings` and `POST /grants` call `HandleMintPrincipal`,
  `HandleCreateBinding` and `HandleCreateGrant` in `internal/registry/principals_api.go`;
  `POST /grants/{grantid}/revoke` appends a revocation. These use the same
  `internal/permissions` store/fold the delivery ACL reads, not a second identity database.
  Principal records include stored role/status data; binding reads return the effective pair;
  grant reads exclude expired and revoked grants. This is management, **not browser sign-in**.
- **Registration is unconditional; authorization is not.** Permission management requires
  `CR_PERMISSIONS_ENABLED=true` and the distinct `CR_PERMISSIONS_ADMIN_TOKEN` Bearer credential.
  An unarmed or unauthorized handler returns `403 MANAGEMENT_FORBIDDEN`, not an absent route;
  shared middleware can first refuse a missing/invalid deployment credential with 401.
  `GET /trust/anchors` and `POST /trust/anchors` also ship (`cmd/server/main.go`,
  `internal/registry/trust_handler.go`), with their own management gate; trust storage does not require
  the delivery ACL switch. The middleware accepts the admin credential on management paths without
  treating a message credential as permission-minting authority.
- **The browser reads the authority records (DF-CRIER-384).** `GET /chat` serves
  `cmd/server/chat.html`. Its read-only Authority view calls `GET /principals`, `GET /bindings`,
  `GET /grants`, `GET /fed/peers` and `GET /trust/anchors`, plus `GET /status` for posture.
  Each inventory names its source route and labels the server-returned field columns; missing fields
  say not reported. Empty, populated, refused and failed reads are distinct states. A collapsed fold
  issues no management reads. No credential entry, principal minting, binding or grant editor is
  provided by this view, and a refused read is not presented as an empty inventory.
- **A session permission matrix and roles read already ship**, not just grant records:
  `GET /sessions/{id}/permissions`, `GET /sessions/{id}/roles` and `GET /sessions/{id}/audit`
  are registered by `session.RegisterAuditRoutes` from `cmd/server/main.go` and handled by
  `internal/session/audit_http.go`. The matrix derives active membership roles and overlays session
  grants; it exposes `send`, `read`, `invite`, `admin` and each allowed cell's `role`/`grant` source
  (`internal/session/audit.go`). `chat.html` renders the Session permissions rows read-only. This is
  **not** a deployment-wide effective-authority evaluator or the reference image's five-column
  send/read/invite/admin/mint editor; there is no per-session mint operation. The matrix is not proof
  that a browser can present a principal credential or satisfy an armed delivery/lifecycle ACL.
- **Sessions, transcripts and lifecycle ship.** `cmd/server/main.go` registers `GET`/`POST /sessions`,
  `GET`/`POST /sessions/{id}/messages` and `GET`/`POST /sessions/{id}/participants`.
  `internal/session` stores the session, membership, message kinds, `thread_id` and same-level reply
  `parent_id`; the browser lists channels/direct conversations and exposes plain sends and explicit
  tasks. `POST /sessions/{id}/state` closes/reopens with durable attributed system events
  (`internal/session/lifecycle.go`, DF-CRIER-446; `CHAT-SESSIONS.md` §4.5a). Its armed-ACL browser
  limitation is listed below, not hidden by this backend claim.
- **Named-group APIs ship; the browser roster surface does not.** `POST`/`GET /groups`,
  `GET /groups/{name}` and `PATCH /groups/{name}/members` are registered in `cmd/server/main.go`
  and served by `internal/session/groups_http.go`. They store curated rosters and fan out via the
  existing session delivery path, unlike a capability selector. DF-CRIER-449 remains open for the
  console create/list/count/roster experience; it must not be read as proof that the API is absent.
- **Thread reads ship; the page now uses the navigation ones.** The server registers thread messages,
  timeline, summary and session search reads (`internal/session/threading.go`), plus recorded
  participant context GET/PUT and compile/expand routes. Stored branch records and parent/anchor
  fields exist in `internal/session/threading.go` and the stores. The embedded page ships the
  deliberate-branch, return-to-exact-anchor and depth-collapse workflows (§3.8.2, DF-CRIER-448); the
  generated summary reads and location-returning session search remain API reads with no browser
  workflow. The late-join prompt and recorded system-event rendering ship (DF-CRIER-447, §3.8.4).

### Still missing or limited

- **Browser sign-in is unresolved (DF-CRIER-333).** The page has no login/cookie exchange and cannot
  attach a Bearer header on plain navigation. `CR_AUTH_TOKEN` protects the page and API; a trusted
  fronting arrangement remains an owner decision. The page cannot sign agent requests either:
  signature-enforced session sends are refused, not silently exempted (§1, DF-CRIER-379).
- **Close/Reopen cannot satisfy an armed ACL from this page (DF-CRIER-460).**
  `chat.html` sends `{state, sender}`; `internal/session/lifecycle.go` requires session admin authority
  resolved through a bound principal. The page now disables the relevant visible control, with an
  inline reason and `aria-describedby`, when the `management_surface` field from `GET /status` is `enabled-unarmed` or
  `admin-token-gated`. Those values both mean the delivery ACL is enabled; management access is
  not session-admin authority. Only `disabled` establishes that the ACL is off. Missing, future or
  unreadable posture stays unknown and disabled, including while a refresh is pending. The session
  read determines which transition is applicable; acting-ID and signature prerequisites still apply.
  The unarmed request body is unchanged. A racing HTTP 403 uses DF-CRIER-459's `refused` class and
  preserves the server's cause, not a red failure or an assumed state change. This is control honesty,
  not browser sign-in: DF-CRIER-333 remains unresolved.
- **The embedded transcript renders replies at one level, not as nesting (DF-CRIER-448).** Depth is
  visualised by the branch index cards and the branch rail; a reply never adds depth (D11). "Branch
  from here" (wire `branch:true`), return-to-exact-anchor navigation (`message-<id>` links from the
  transcript and the rail) and expandable depth collapse (selector 1–6, default 3) ship in §3.8.2.
  Still absent from the browser: re-parenting an existing message, a filterable transcript-search box
  that returns a location, and generated per-thread summary cards — those reads exist in
  `internal/session/threading.go` but are not browser workflows. An explicit task send cannot also
  branch; the composer names that restriction instead of silently dropping the intent.
- **Named-group rosters are not manageable in the console (DF-CRIER-449).** No group picker or
  create/list/read-membership control ships in `chat.html`; the shipped HTTP roster operations above
  and ident-only member rule do not turn a group name into an authenticated agent identity.
- **No accessibility audit is recorded (DF-CRIER-450).** Existing status roles and ARIA attributes are
  not a keyboard/focus, contrast, reduced-motion or announced-error audit; no accessibility compliance
  is claimed.
- **The design still exceeds the embedded page.** Generated summary cards, a timeline rail and
  location-returning transcript search are not complete browser workflows. The late-join prompt is
  shipped (DF-CRIER-447); later context changes still use the API rather than a browser editor. Attachment upload/fetch and byte handling are not provided by this interface;
  an opaque payload or asset reference is not an attachment service. Lease renewal remains deliberately
  absent (§4 row 19). The four reference images remain layout references with placeholder lettering,
  not screenshots of a fully implemented control panel.

### Executable scope of this inventory

`docs/claims.yaml` anchors the shipped authority statements and probes their registered paths using
`make docs-check`'s existing `kind: route` language (GET, non-404). A named 403 proves registration,
**not** a populated read, permission to manage records, browser access or POST semantics. The
`TestDocsClaimsInterfaceAuthorityFixture` fixture additionally boots the real in-process server with
test-owned stores and armed permissions, creates a principal/binding/grant, reads each back with
`count=1`, and checks that the message credential is refused. It runs under the same `make docs-check`
test-name selection. No claim here asserts that the open browser rows are complete.

---

## Dagger control

*Added by **CR-CHAT-033** (2026-10-04) as an addendum to this document's series. The dagger surface is
not a UI element, so it is recorded here rather than as a row of §4's mapping table. §5's invariants —
one delivery path, no parallel truth, a client of the shipped API — bind it unchanged, and nothing in
§1–§7 moves because of it.*

A crier surface (an MCP tool plus REST routes) by which a Hermes instance or an agent **creates and
controls a Dagger pipeline**: create a run, observe its status, cancel it, resume it from a checkpoint,
rewind to a node, and run a registered skill.

**The boundary rule (decision D18): crier does NOT embed a second executor.** Every verb is a request to
the EXISTING dagger surface, and crier holds only the run id, the state the executor last reported,
references to its evidence, and the agent that asked for it. A second scheduler would be a second truth.

- **The bridge is the whole vocabulary.** `internal/daggerctl.DaggerBridge` is create / status / cancel /
  resume / rewind / run-skill and nothing else; the only implementation this repository ships speaks
  HTTP/JSON to the endpoint named by `CR_DAGGER_URL`. A bridge that refuses a create leaves **no local
  record** — there is no fallback engine that could have produced one.
- **Why it belongs on the bus.** Control has to be addressable and auditable like any other work: the six
  routes exist only while `CR_DAGGER_URL` is set, and the run's outcome is delivered to the REQUESTING
  agent through the **shipped inbox path** — the same durable store write a `MESSAGE_EXPIRED` receipt
  uses (§5.1's one-delivery-path rule), exactly once, rather than a side channel.
- **States.** `running` / `succeeded` / `failed` / `cancelled` / `unknown`. Only the three terminal
  states are delivered; `unknown` is a word crier could not map, so it is recorded, not guessed at, and
  it never fires a completion.
- **The MCP halves.** `create_run`, `run_status`, `cancel_run`, `resume_run`, `rewind_run`, `run_skill` —
  they drive the run records the SERVER holds, so a run started inside a thread is visible in that
  thread.

**Not decided here** (the rows that own them): running a DAG **as a scheduling engine** — DAG nodes that
invoke crier, and crier events that trigger a DAG — is **CR-CHAT-034**; **remote or local execution
targets** for the same interface are **CR-CHAT-035**. This section claims neither.
