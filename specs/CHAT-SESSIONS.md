# CHAT-SESSIONS.md — the session and threading model

Status: **DRAFT v1** · 2026-10-03 · Owner: Bane · Tickets: **CR-CHAT-002** (the session object, data model,
membership, ordered transcript, per-namespace retention) and **CR-CHAT-005** (threading: session vs
thread vs topic, reply semantics)
Series: `specs/CHAT-INTERFACE.md` (the spec of record — objects, UI surface, the element→endpoint mapping),
`specs/CHAT-PERMISSIONS.md` (CR-CHAT-003 — principals, roles, grants), `specs/CHAT-ADDRESSING.md`
(CR-CHAT-004 — the tag grammar), `specs/CHAT-STORAGE.md` (CR-CHAT-006 — the dual backend, the bundle, the
reconciler).
Visual source of truth for the surface: the four approved UI options of 2026-10-03 — `A` channels +
threads, `B` operator console, `C` triage inbox, `D` chat + capability map — read as layout references
whose lettering is placeholder (CR-CHAT-011). Cited below as `(A)`…`(D)`.
Precedent: `specs/NAMESPACES.md`, `specs/A2A-OPTION.md` (same rigor + format).
Decisions of record owned here: **D1** (a session is an aggregate over durable inbox deliveries — §3.4)
and the D6 reference (§5).

---

## 1. The Session object

### 1.1 Definition

A **Session** is a **durable conversation**: a set of participants (Principals and Agents), an explicit
membership, and an **ordered transcript** that is append-only. It is the room a human and a set of agents
share; it is **not** a bus primitive and **not** a new transport (§4, D1).

### 1.2 Fields

| Field | Type | Required | Meaning |
|---|---|---|---|
| `id` | string, opaque, URL-safe | yes | The session's identity. It is the **same value** the wire already carries as `session_id` — on the deliver body (`POST /agents/{id}/inbox`, CR-FEAT-004), on the webhook envelope (`crier.session_id`) and in the federation hold record — and the same value the guard resolves `session:<id>` policy keys against. One identity, no mapping table. |
| `namespace` | string | yes | The realm the session lives in. `""` is the default namespace (the shipped canonical spelling). A session is attached to exactly one realm, and no surface in this series may move it or bridge two realms (`specs/NAMESPACES.md`). |
| `title` | string | no | Display only. Drawn in every option (`A` "Build Plan", `D` "#A7F3 · Multi-Agent Task" as a subtitle). Never an addressing target. |
| `created_at` | timestamp | yes | When the session was created. |
| `created_by` | Principal | yes | Who created it. Recorded as an **event** (§2.3), so it is auditable; a session whose creator cannot be named is invalid. |
| `state` | enum `open` \| `closed` | yes | See §1.3. |
| `closed_at` | timestamp | no | Set exactly once, by the `close` event. |
| `retention_seconds` | integer | no | The session's message-lifetime default. Bounded by the realm's `retention_seconds`: a session may declare a **shorter** lifetime, never a longer one (§3.5). |
| `group` | string | no | A session-group / project handle, for the multi-team mapping (CR-CHAT-010: project/team ↔ namespace / session-group). **NOT BUILT** — the field is reserved here so adding a team is data, not schema surgery. |
| `visibility` | enum | no | Whether the session is discoverable and by whom. Drawn only as a **privacy indicator**: `A` draws a padlock glyph beside the title, with no label. **NOT BUILT**; its semantics belong to CR-CHAT-003 (a Grant) and are deliberately not fixed here. |

The transcript and membership are **not** fields on the object: they are ordered event sequences (§2.3,
§3.1) owned by the append log, because a mutable list field on a room record is exactly the hidden state
that makes "reconstructable from the transcript alone" (§4.3) unprovable.

### 1.3 Lifecycle

    created (open) ──▶ messages, threads, membership changes ──▶ closed

- **Creation** appends a `session.create` record. There is no side channel: a session that is not in the
  log does not exist.
- **Open** is the only state that accepts new records other than `close` / `reopen`.
- **Closed** means no new message, reply or membership change is admitted. **Closing does not delete.**
  The transcript is retained; a closed room is still readable by its members (the retention rule, §3.5,
  is the only thing that removes content).
- **There is no `deleted` state.** A room whose history can be removed is a room whose transcript cannot
  be the record — the property §3 and §4.3 are built on. If a room must be made unreachable, that is an
  access decision (a Grant), not a delete.
- **`reopen`** is an event, not a field mutation: it appends `session.reopen` and the room returns to
  `open`. A record is never rewritten, so "was this open on date T" is answerable by replay.
- `B` draws a richer-looking lifecycle than this — a green outlined `ACTIVE` badge and two system-event
  rows ("Lease renewed for session …", "Session … marked complete"). Only the *existence* of a session
  state is taken from the image; the vocabulary `open` / `closed` is fixed here, and `ACTIVE` is rendered
  as "open". A richer state machine (paused, archived, per-participant state) is **NOT BUILT** and not
  fixed by the images.

### 1.4 Who may create a session

- Creation is an **explicit capability**, not an implication of presence. Being a member of a realm, or
  of an agent's owner, does not create the right to open a room. (The grant semantics — which role holds
  the capability, and what it is called — are `specs/CHAT-PERMISSIONS.md`, CR-CHAT-003; this document
  fixes only that it is explicit and checked.)
- **Creating does not imply inviting.** The permission surface `B` draws separates the columns `send`,
  `read`, `invite`, `admin`, `mint` — so the ability to open a room and the ability to add a member to
  it are two different capabilities, and a creator who holds only `admin` over an empty room has a room
  they cannot populate.
- `POST /sessions` (owed) records `created_by` from the authenticated principal; a request that cannot
  name one is refused, never defaulted to a service identity.

---

## 2. Membership

### 2.1 Who is in

A **member** is either a **Principal** (a human) or an **Agent**, each with a role. The role set is
`specs/CHAT-PERMISSIONS.md`; the names quoted from the images are `ADMIN`, `MEMBER`, `VIEWER` (`A`, `C`)
and `admin` / `invoke` / `read` (`D`) — and `B`'s matrix columns `send`, `read`, `invite`, `admin`,
`mint` are the *capability* axes, not four more roles. The two vocabularies are not reconciled here
because they are not this document's to reconcile; what this document fixes is: **every member has
exactly one role, and membership is a recorded event, not a derived property** of who happens to own
which agent.

Default reach, restated from the Agent definition: a **personal** agent's default reach is its owner
plus explicit grants; **cross-owner is default-deny**. A session does not weaken that — a personal agent
added to a room is reachable in that room by the room's members per its grants, and a delivery to it from
outside the room is still refused without one.

### 2.2 Who may join

**Explicitly, by invitation.** A principal or agent becomes a member when an `add` event names them.

The rejected alternative, recorded because it is the tempting one: **open self-join** (a session
declaring itself joinable by anyone in the realm). Rejected because the audience of every message — and
therefore the cost and the exposure of every fan-out (§3.4) — would become unbounded and unknowable at
send time, and because "who could read this" would stop being answerable from the transcript. If a room
genuinely needs broadcast semantics, that is the **relay topic** (§4.1), which has fan-out and no
history, and it is a different primitive by design.

### 2.3 How membership changes

Membership is an **append-only event sequence**, never a mutable set:

- `session.member.add` — carries `member_type` (principal | agent), `member_id`, `role`, `actor`, `ts`.
- `session.member.remove` — carries `member_type`, `member_id`, `actor`, `ts`, optional `reason`.
- A role change is a `remove` + `add` pair (or a distinct `role.change` event — an implementation choice,
  **NOT BUILT** either way). It is never an in-place edit.

Consequences, both load-bearing:

1. **Membership at time T is derivable** by replaying events up to T. This is what makes "who could read
   this message when it was sent" answerable — a question an audit trail (`B`'s fourth panel) is
   worthless without.
2. **A replay is idempotent** by `(session_id, seq)`; a duplicate `add` for the same member and role is a
   no-op, not a second row.

### 2.4 What happens to a removed member

Five rules, in order:

1. The removal is an **event** — visible in the room, with actor and time. A member never "just stops
   appearing".
2. They receive **no new fan-out deliveries** from that point forward. Delivery is resolved against the
   membership as of the sending record's `seq`, so a removal that races a send resolves deterministically
   (the send's audience is the membership at its own `seq`; there is no "mostly removed" state).
3. **Their existing deliveries are untouched.** An inbox is per-agent and durable; removing a member from
   a room must not reach into another agent's inbox and purge it — that would be a cross-agent write and
   a second truth. Their unread messages stay theirs, with their leases, acks, TTLs and dead-lettering
   unchanged.
4. **The transcript is not rewritten.** Their past records remain, attributed. A room's history does not
   change because a participant left; hiding it is a *visibility* decision (CR-CHAT-003), not a delete.
5. **Re-adding is a new event**, never a rollback. The transcript therefore shows the join/leave/join
   sequence, which is the honest record.

---

## 3. The transcript

### 3.1 Ordering

- Every record carries a **monotonic `seq` per session** and a `ts`. `seq` is the ordering authority;
  `ts` is display and correlation.
- The **JSONL append log is the ordering authority** (D6, `CHAT-STORAGE.md`); the Postgres view is built
  from it. Two records claiming the same `(session_id, seq)` with different content is a **conflict and a
  finding**, never a silent last-write-wins. Replay of a duplicate `(session_id, seq)` with identical
  content is a no-op.
- A client must not order by `created_at` alone: clocks disagree across senders, and an ordering that a
  reader can get wrong is not an ordering. The `seq` is what makes `A`'s threaded reply cluster and `D`'s
  connector rail renderable at all.

### 3.2 Durability

A message is **durable for the room** when its transcript record is in the log, and **durable for a
participant** when the corresponding delivery is in that participant's inbox. The two are related by
**D1's ordering rule**:

> **The transcript record is written first, as the intent, carrying the message id and the resolved
> audience; the fan-out follows, and each target's outcome is written back onto that same record.**

The rejected alternative — fan-out first, log afterwards — was rejected because a crash between the two
leaves a message that exists in one agent's inbox and not in the room: two truths, and the exact failure
§5.1 of `CHAT-INTERFACE.md` forbids. With the log first, a crash leaves a message that the room knows
about and whose delivery outcomes are incomplete — a **visible, repairable state** rather than a silent
divergence.

Per-target outcome, recorded on the transcript record: `delivered` (the inbox accepted it),
`leased` (a holder has it in flight), `acked`, `expired` (dead-lettered unacknowledged — the shipped
reason is `ttl_expired_unacked`), `refused` (a named refusal — no grant, unknown agent, `NO_CAPABLE_AGENT`
for a capability target, a namespace mismatch). A `refused` outcome is **shown**, never swallowed:
`B` renders exactly this surface as its per-message meta line (`lease: active` / `lease: closing` /
`ack: ✓ delivered`, from `README`-visible deliver state), which is why the UI already has somewhere to
put it.

### 3.3 The transcript is a view, not a second store of messages

The transcript's records are **the room's own ordered log**; the *messages* they describe are delivered
through the existing path and live, for each agent, in that agent's durable inbox. There is no
session-scoped message store, no session-scoped queue, and no session-only copy of a payload. The
transcript record is a **record of the message** (identity, author, parent, audience, outcome), not a
parallel mailbox.

### 3.4 The fan-out rule (the load-bearing rule of this document)

> **A message addressed to a session is delivered by issuing ONE delivery per participant through the
> EXISTING path** — `POST /agents/{id}/inbox` with `payload`, `session_id`, `thread_id` (and
> `sender` / `request_id` / `priority` / `ttl_seconds` as the sender sets them) — **reusing** the guard
> choke point, the webhook driver, the durable inbox write, lease / ack / TTL, dead-lettering and the
> federation fallback. There is **no second delivery path**.

Rules that make it safe:

1. **One message id across the room.** The transcript record's message id **is** the message id the
   deliver path returns. There is no session-level id distinct from the delivery id — one identity, so
   the room and the inbox can never disagree about which message they are talking about.
2. **Per-target idempotency.** Each fan-out delivery carries a deterministic idempotency key derived from
   `(session_id, thread_id, message_id, target_agent_id)`. The shipped deliver path already deduplicates
   on `idempotency_key` within a window and answers a retry with the **first** delivery's accept (same
   message id, nothing stored twice) — so a retried fan-out, or a replayed log, cannot double-deliver and
   cannot double-notify a member.
3. **Agents keep offline semantics.** A participant that is offline, leased-elsewhere, or slow does not
   block the room: its delivery waits in its durable inbox under the normal TTL, and if it never takes
   it, the shipped ownership path produces the `MESSAGE_EXPIRED` receipt in the **sender's** inbox and a
   dead letter on the target's dead-letter path. The room shows the outcome (rule §3.2); nobody has to
   poll for it.
4. **Capability targets are NOT fanned out.** If a member is addressed by capability — the `@cap:y` tag,
   `D`'s halo, "One tag. Many agents." — the delivery resolves to **one** holder, round-robin over live
   holders, through `POST /capabilities/{capability}/inbox`. The room records which holder took it. A
   capability tag is a **selector, not a broadcast**, and the UI must not draw it as one
   (`CHAT-INTERFACE.md` §3.7 item 4).
5. **Principals have no inbox.** An inbox is per-**agent** (`InboxEntry.AgentID`). A Principal receives a
   fan-out through a **bound agent** (CR-CHAT-007: a principal may speak as, and is reached via, a bound
   agent), and the transcript records both the bound agent and the human who spoke as it. The binding is
   **NOT BUILT** — today a fan-out can only target agents.
6. **Cost is visible, not hidden.** N participants means N deliveries; the room's audience is knowable
   before the send (that is why §2.2 refuses open self-join). A send to a large room is a large write, and
   the interface must be able to say so.

**NOT BUILT:** the one-call fan-out. A client can compose it today by issuing one
`POST /agents/{id}/inbox` per participant with a shared `session_id` / `thread_id` — but there is no
server route, no membership list to enumerate, and no record returned. Owed: `POST /sessions/{id}/messages`
(and its thread variant) plus the transcript record, per CR-CHAT-002.

### 3.5 Retention, per namespace

- The realm's `retention_seconds` (the namespace policy axis, SHIPPED — `specs/NAMESPACES.md`, CR-FEAT-029)
  is the **default message lifetime for deliveries into that realm**: when a delivery carries no explicit
  `ttl_seconds`, the realm's retention is applied. An explicit `ttl_seconds` on the delivery overrides it.
- A session **stays inside its realm's retention**. It may declare a **shorter** `retention_seconds` for
  its messages; it may not declare a longer one. A room cannot outlive the policy of the realm it lives
  in, because that would make the realm's retention a suggestion.
- **Two clocks, stated honestly.** The *delivery* clock is the message TTL — it decides how long an
  unretrieved message waits in an inbox and when it becomes a dead letter. The *room* clock is the
  session's retention — it decides how long the transcript record is kept. They are not the same number
  and are not intended to be: a delivery that expired unacknowledged still leaves a transcript record
  stating `expired`, because "the message was sent and never taken" is exactly the fact a room must not
  lose. Whether the transcript should ever be retained **longer** than the delivery TTL for a given realm
  is an owner decision (§6).

---

## 4. Threading — session vs thread vs topic

### 4.1 The three levels, mapped to existing primitives

| Level | What it is | Existing crier primitive | History | Multiplicity |
|---|---|---|---|---|
| **topic** | A named fan-out channel. A "channel" in the broadcast sense is a topic. | Relay pub/sub: `POST /relay/publish`, `GET /relay/subscribe/{topic}` (WebSocket), `GET /relay/topics`. Subscriber-side wildcards `*` (one segment) and `>` (one or more trailing segments); scoped to a namespace, so two realms using the identical literal topic are disjoint sets; no cross-realm bridging. | **NONE** — a subscriber that is offline misses the event. That is the primitive's contract, not a bug. | every current subscriber |
| **inbox** | A durable, directed message slot for ONE agent. | `POST /agents/{id}/inbox` (deliver) · `GET /agents/{id}/inbox` (retrieve, with `lease` / `lease_seconds` / `limit` / `wait`) · `POST /agents/{id}/inbox/ack` · `GET /agents/{id}/inbox/stats` · `POST /agents/{id}/inbox/transfer` · `GET /agents/{id}/inbox/dead-letters`. Capability addressing: `POST /capabilities/{capability}/inbox` (resolves to one holder). | **DURABLE** — the message waits through the agent being offline, with TTL, lease/ack, dead-lettering, federation fallback. | exactly one agent id |
| **session** | The **durable room**: participants, membership, an ordered transcript. This document. | **Nothing of its own.** A session is an **aggregate** over inbox deliveries (§3.4, D1). Its `session_id` is already a wire tag (deliver body, webhook `crier.session_id`, federation hold) and a guard policy key. | **DURABLE**, per §3.5. | N principals + agents |
| **thread** | A **reply subtree inside one session**. | Carried as `thread_id` on the same deliver body (CR-FEAT-004) and as a guard policy key (`thread:<id>`) — so the guard can already scope a policy to a conversation or a thread. | durable with its session | N, inside one session |

**The rule the UI must obey:** there is no fourth concept. `A`'s channels + threads, `B`'s session
console, `C`'s conversation cards and `D`'s session hub are all views over *session* (Z3) with *topic* as
the only fan-out-without-history primitive, and *inbox* as the only delivery mechanism. A UI element that
needs a level not in this table is a finding, not a new primitive.

Note what is **PARTIAL** today: `thread_id` is a **wire tag and a policy key, not a stored field** —
`registry.InboxEntry` carries no `session_id` and no `thread_id`. A thread is therefore **not
reconstructable from stored messages**; it is reconstructable from the transcript, which is why the
transcript is the record (§4.3).

### 4.2 Reply semantics

**What a reply carries** (the wire + record fields, all required unless marked):

| Field | Meaning |
|---|---|
| `session_id` | The room. Same identity as the deliver body's `session_id`. |
| `thread_id` | The **root** message id of the subtree — not the immediate parent. One key per subtree. |
| `parent_id` | The **immediate** parent's message id. Present on a reply, absent on a thread root. |
| `message_id` | This message's id, identical to the id the deliver path returns (one identity, §3.4 rule 1). |
| author | The authoring agent id, **plus** the Principal when a human spoke as it (CR-CHAT-007). |
| `ts`, `seq` | Ordering (§3.1). |
| audience | The resolved delivery set (below), recorded on the record so the fan-out is auditable **and** so a later reader does not have to re-derive it from a membership that has since changed. |

**Root vs reply.** A message with no `parent_id` is a **thread root**; its `thread_id` **is its own
message id**. A reply's `thread_id` is the root's id, which is what makes a subtree a single lookup and
what makes `A`'s "4 replies" pill and `D`'s connector rail derivable from data rather than maintained.

**The default audience**, when the sender names no address:

> the parent message's author **plus** every participant who has already spoken in that thread, **minus**
> the replying author.

Two rejected alternatives, because both are tempting and both are wrong here:

- *Reply to the whole session.* Rejected: a reply in a 40-member room would notify 40 participants for a
  remark aimed at one; threads exist precisely to stop that, and the fan-out cost is paid per reply.
- *Reply to the parent author only.* Rejected: it silently drops the other participants who joined that
  thread, so a thread stops being a conversation and becomes a pile of private pairs — and the reader
  sees messages in the room that the addressed set no longer matches.

An explicit `Address` list (`@agent`, `@cap:y`, …) **overrides** the default — the sender can always name
the audience, and the record states which rule produced the audience it delivered to.

**Does a reply re-notify?** **Yes — once per (message id, target).** "Once" is not a promise, it is
enforced: the fan-out's per-target idempotency key (§3.4 rule 2) means a retry, a replay or a client bug
cannot deliver the same message to the same participant twice. Two corollaries:

- A delivery to the **replier** is never issued (a reply does not notify its own author).
- An **edit** is not a reply and does not re-notify. Edits are out of scope (§6); if they arrive, they are
  a new record, not a re-notification of the old one.

### 4.3 A thread MUST be reconstructable from the transcript ALONE

This is CR-CHAT-005's acceptance property, stated as a rule:

> Given only a session's transcript records, in `seq` order, a reader can reconstruct every thread
> exactly: group records by `thread_id`, order each group by `seq`, and attach each record to its
> `parent_id`. No side table, no client-side state, no relay subscription, and no membership lookup may
> be required.

Consequences:

- `parent_id` and `thread_id` are on **every** message record — a root's `parent_id` is absent, and its
  `thread_id` equals its own `message_id`. There is no "obvious from context" case.
- A record whose `thread_id` is not the id of any root in the same transcript, or whose `parent_id` names
  a message that is not in the transcript, is a **broken thread**: it is **reported as a finding**, never
  silently re-rooted or dropped. (A record can legitimately reference a parent that was removed by
  retention — that is a retention hole, and the reader must be able to tell the two apart, which is why
  the finding names which case it is.)
- Ordering within a thread is `seq`, **not** arrival time at any one reader, so two readers reconstruct
  the same tree.
- The audience is recorded on each record (§4.2), so a reconstruction does not have to replay membership
  to know who was addressed — and conversely, membership replay (§2.3) and thread reconstruction are
  independent, so neither depends on the other's correctness.

---

## 5. Storage shape

One paragraph, then the shapes. **The mechanism is not this document's**: the dual backend, JSONL as the
ordered append log and the transport form, PostgreSQL as the query view, the export/import bundle, the
keep-LAST replay rule, the reconciler and the rule that a mismatch is a **finding and not silent drift**
are `specs/CHAT-STORAGE.md` (CR-CHAT-006, decision **D6**). What this document fixes is the **record
shape on both sides**, so the two views are built from the same facts and a round-trip is content-identical
(CR-CHAT-002's acceptance).

### 5.1 JSONL — the ordered append log and the transport form

One **record per line**, `v` (record version) first, so a reader can dispatch on a version it knows:

```
{"v":1,"type":"session.create","session_id":"…","seq":1,"ts":"…","namespace":"…","title":"…","created_by":{…},"retention_seconds":86400}
{"v":1,"type":"session.member.add","session_id":"…","seq":2,"ts":"…","member_type":"agent","member_id":"atlas","role":"member","actor":{…}}
{"v":1,"type":"session.member.remove","session_id":"…","seq":3,"ts":"…","member_type":"agent","member_id":"atlas","actor":{…},"reason":"…"}
{"v":1,"type":"session.message","session_id":"…","seq":4,"ts":"…","message_id":"…","thread_id":"…","author":{…},"payload":…,
 "audience":{"rule":"session"|"reply-default"|"explicit","targets":[{"kind":"agent","id":"nimbus"},…]},
 "outcomes":[{"target":"nimbus","outcome":"delivered","inbox_entry_id":"…"}],"idempotency_key":"…"}
{"v":1,"type":"session.thread.reply","session_id":"…","seq":5,"ts":"…","message_id":"…","thread_id":"…","parent_id":"…","author":{…},"payload":…,
 "audience":{…},"outcomes":[…],"idempotency_key":"…"}
{"v":1,"type":"session.close","session_id":"…","seq":6,"ts":"…","actor":{…},"reason":"…"}
{"v":1,"type":"session.reopen","session_id":"…","seq":7,"ts":"…","actor":{…}}
```

Fields shared by every record: `v`, `type`, `session_id`, `seq`, `ts`. `session.message` and
`session.thread.reply` carry `message_id`, `thread_id`, `author`, `payload`, `audience`, `outcomes`,
`idempotency_key`; `thread.reply` additionally carries `parent_id`. Replay: order by `(session_id, seq)`,
**keep-LAST** per `(session_id, seq)` (identical duplicates are no-ops), idempotent by `message_id` for
message records. A `session.message` whose `outcomes` are incomplete is a message with unresolved
deliveries — a repairable state (§3.2), not a corrupt line.

### 5.2 PostgreSQL — the query view

Built from the log; never written in parallel with it (D6). Table and column names are fixed here so the
view and the client cannot invent two schemas:

- `chat_sessions(id PK, namespace, title, created_by, created_at, state, closed_at, retention_seconds,
  group_id, visibility)` — one row per session, the projection of `create`/`close`/`reopen`.
- `chat_session_members(session_id FK, member_type, member_id, role, added_at, removed_at, added_by,
  removed_by)` — the projection of the membership events; a membership **at time T** is a query over
  `added_at <= T AND (removed_at IS NULL OR removed_at > T)`.
- `chat_transcript(session_id, seq, message_id, thread_id, parent_id, author_type, author_id,
  principal_id, payload, guard jsonb, audience jsonb, created_at, PRIMARY KEY (session_id, seq))` — the
  projection of message records. `(session_id, seq)` is the ordering key; `thread_id` + `parent_id` are
  what §4.3 requires; `guard` carries the shipped `guard.Meta` so a flagged message is queryable, not
  just rendered.
- `chat_deliveries(session_id, message_id, target_agent_id, inbox_entry_id, outcome, updated_at,
  PRIMARY KEY (session_id, message_id, target_agent_id))` — the per-target fan-out outcome (§3.2). It is
  a **projection**, not a queue: the authoritative delivery is the agent's inbox entry, and this row is
  how the room reads its outcome back.

Two properties the shapes exist to guarantee: a session round-trips through Postgres **and** through a
JSONL bundle with identical content, and a divergence between the two is **detected** rather than
accepted (`CHAT-STORAGE.md`).

---

## 6. Open questions

1. **`seq` allocation authority.** Is `seq` allocated by the writer of the log (the room's single
   appender), or by each delivering node and reconciled? Single-appender is simpler and this document
   assumes it; a multi-node fan-out needs the answer before the fan-out ships. Owed to
   `CHAT-STORAGE.md`.
2. **Do history holes need a marker?** When retention removes delivery-level records but keeps transcript
   records (or vice versa), should the transcript carry an explicit "hole" record so a reader can tell a
   retention gap from a broken thread? §4.3 requires the reader to tell them apart, which is easiest with
   a marker.
3. **Transcript vs delivery retention.** Should the transcript be retainable **longer** than the message
   TTL for a realm (so a room keeps its history after inboxes have expired)? Today the realm has one
   `retention_seconds`; a second axis is an owner decision.
4. **Are DMs a session type?** A two-party conversation is expressible as a session with two members, but
   whether it is the *same* object with a `direct` flag, or a distinct type with different visibility
   defaults, is undecided — and CR-CHAT-003 owns what a DM implies for grants.
5. **Thread depth / reply-to-reply.** The images draw nested connectors (`D`) and a single reply cluster
   (`A`). Is a thread arbitrarily deep, or flat with `parent_id` used for quoting only? §4.3 works either
   way, but the UI rules differ.
6. **Can a thread span sessions?** A strict no is assumed (a thread is inside one session). If a
   cross-room reply is ever wanted, it is a new primitive and needs a decision, not a field reuse.
7. **Edit / delete / react.** Out of scope here; if wanted, they are **new records** (never mutations),
   and their re-notification semantics must be stated. `D` draws a per-message overflow affordance whose
   menu is not drawn.
8. **Session groups.** `group` (CR-CHAT-010) is reserved but its semantics — is a group a namespace, a
   label, or a room-of-rooms — are undecided; a room-of-rooms must not become a second membership model.
9. **Presence of a *human* participant.** The registry's derived presence is mesh/registry evidence for
   **agents**; a Principal's online state has no server signal and would need one (or the UI must not
   draw a dot for a human). `A` draws a presence dot on every avatar, including principals.
10. **`state` vocabulary.** `open` / `closed` is this document's minimum. `B` implies more (`ACTIVE`, a
    "marked complete" event, a renewal event). Do paused / archived states exist?
11. **Who may close a session**, and can a member be removed from a closed room? §1.3 forbids new records
    after close, so the answer must be "reopen first" or "close is not absolute".

---

## 7. Status

**Status: DRAFT v1 · 2026-10-03 · CR-CHAT-002 + CR-CHAT-005.**

**Not built** — this document specifies objects and rules that no route implements:

- **No session object, no session route.** No `POST /sessions`, no `GET /sessions`, no
  `GET /sessions/{id}/messages`, no `POST /sessions/{id}/messages`, no membership routes. `session_id`
  exists today as a **wire tag** on the deliver body (`POST /agents/{id}/inbox`, CR-FEAT-004), on the
  webhook envelope (`crier.session_id`) and in the federation hold record, and as a **guard policy key**
  (`session:<id>`) — and nowhere else.
- **No stored `thread_id`.** It is a wire tag and a guard policy key; `registry.InboxEntry` carries
  neither `session_id` nor `thread_id`, so no thread and no transcript are reconstructable from stored
  messages today.
- **No fan-out.** There is no one-call "send to the session": a client must issue one
  `POST /agents/{id}/inbox` per participant, and there is no membership list to enumerate and no
  transcript record returned.
- **No append log and no session tables.** The JSONL record types and the four Postgres tables in §5 are
  target shapes; nothing writes them (CR-CHAT-006 owns the mechanism).
- **No membership, no roles, no grants** (CR-CHAT-003) and **no address parser** for `@team:` / `@ns/*` /
  `#session` (CR-CHAT-004).
- **No lease renewal** — the lease is taken per retrieve and released by ack; the renewal event `B` draws
  has no operation behind it.
- **No session state surface, no visibility/privacy field, no session group, no edits or reactions.**

**Shipped and reused, not to be reinvented:** the durable inbox with lease / ack / TTL / dead letters
(`GET /agents/{id}/inbox`, `/ack`, `/stats`, `/transfer`, `/dead-letters`), idempotent delivery
(`idempotency_key`), delivery modes and `timeout_ms`, priority, per-namespace retention and policy
(`GET /namespaces`, CR-FEAT-029), capability delivery as a **selector** (CR-FEAT-026), the relay topic
primitive, the guard verdict metadata, and the derived agent presence (CR-FEAT-024).

Not claimed, and not to be claimed in `docs/claims.yaml`, until they exist: sessions, membership,
transcripts, threads, the fan-out, session retention, session state, and the session storage shapes.
