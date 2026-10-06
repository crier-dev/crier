# CHAT-SESSIONS.md — the session and threading model

Status: **DRAFT v2** · 2026-10-03 · Owner: Bane · Tickets: **CR-CHAT-002** (the session object, data model,
membership, ordered transcript, per-namespace retention) and **CR-CHAT-005** (threading: session vs
thread vs topic, reply semantics)
REVISION v2 · 2026-10-03 — folded Bane's detail pass into this document. **Added:** the session `kind` —
`channel` (a project-scoped top-level room) and `direct` (a 1:1) are the SAME object (§1.2, **CR-CHAT-013**);
the named-group fan-out rule (§2.1, §3.4 rule 7 — a curated roster fans out, a capability stays a selector,
**D8**); the three message **kinds** (§4.4 — plain / addressed / TASK-ACTION, a tag is addressing and the
only kind that may create work, **CR-CHAT-018**, **D12**); the **depth rule** (§4.5 — a reply STAYS IN
THREAD; a sub-thread exists only when deliberately branched, **CR-CHAT-017**, **D11**); the late-join
context-share record (§4.6, **CR-CHAT-016**, **D10**); the navigability data properties (§4.7 —
collapse is derivable, a summary is an index, search returns LOCATION, **CR-CHAT-017**); and the record
shapes for all of it (§5 — `message_kind`, `chat_threads`, `chat_context_shares`, `session.thread.branch`
and `session.member.context`). The two rows that are not this document's are named here only for
completeness: **CR-CHAT-014** (attachments — this document carries just the scope note that the asset
**reference** rides the opaque `payload` and the bytes do not, §5.2) and **CR-CHAT-015** (nested threads
and the request→thread flow — the acceptance test is stated in `CHAT-INTERFACE.md` §1.4, and the
data-side property it leans on is §4.3 here). **Corrected (D11):** v1 left thread depth **open** (§6 item 5 asked
"arbitrarily deep, or flat with `parent_id` used for quoting only?") and its reply record named `parent_id`
as *the immediate parent* (§4.2), which could be read as "a reply-to-a-reply is a deeper level". **That is
not the rule.** `parent_id` is reply **attribution**; a reply never changes `thread_id` and never deepens
the tree; a level is created **only** by a deliberate branch. §4.2 / §4.3 are corrected in place, §4.5 is
new, and §6 item 5 is marked ANSWERED — nothing was left silently contradictory.
Series: `specs/CHAT-INTERFACE.md` (the spec of record — objects, UI surface, the element→endpoint mapping),
`specs/CHAT-PERMISSIONS.md` (CR-CHAT-003 — principals, roles, grants), `specs/CHAT-ADDRESSING.md`
(CR-CHAT-004 — the tag grammar), `specs/CHAT-STORAGE.md` (CR-CHAT-006 — the dual backend, the bundle, the
reconciler), plus two rows authored concurrently by their own owners and **not on disk as of this line**:
`specs/CHAT-FEDERATION.md` (CR-CHAT-023 + CR-CHAT-024) and `specs/CHAT-TRUST.md` (CR-CHAT-024 +
CR-CHAT-025).
Visual source of truth for the surface: the four approved UI options of 2026-10-03 — `A` channels +
threads, `B` operator console, `C` triage inbox, `D` chat + capability map — read as layout references
whose lettering is placeholder (CR-CHAT-011). Cited below as `(A)`…`(D)`.
Precedent: `specs/NAMESPACES.md`, `specs/A2A-OPTION.md` (same rigor + format).
Decisions of record owned here: **D1** (a session is an aggregate over durable inbox deliveries — §3.4),
**D10** (late-join context — §4.6), **D11** (the depth rule, revised — §4.5), **D12** (message kinds —
§4.4), and the D6 reference (§5).

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
| `kind` | enum `channel` \| `direct` | yes | The session's **shape** (CR-CHAT-013). `channel` = a **project-scoped top-level room**: the place a project lives, whose **top level is the project conversation** and whose threads carry the detail. `direct` = a **1:1** with one agent. A channel and a DM are the **same object** with different defaults — one membership model, one transcript, one fan-out rule (§3.4) — not two primitives. A session record with no `kind` reads as `channel`, so a record written before this field existed is not a third shape. **NOT BUILT.** |
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

**A named group is not a session** (CR-CHAT-013, D8). A **named group** is a curated, editable **set of
agents** — an address target with a roster, and nothing else: no participants, no transcript, no state. A
message addressed to it is delivered to each **current** member as one fan-out delivery per member (§3.4
rule 7), which makes a group send the **same shape** as a session send, with the same per-target
idempotency and the same recorded audience. It is deliberately **not** the capability selector: a
capability target resolves to **one live holder**, a named group delivers to **every** member it currently
holds — the two are different address kinds (`@team:x` vs `@cap:y`) and must never be conflated. Who may
edit a roster, how a group is stored, and what a group grant means are CR-CHAT-003 / CR-CHAT-022; this
document fixes only the delivery shape.

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

- `session.member.add` — carries `member_type` (principal | agent), `member_id`, `role`, `actor`, `ts`, and
  — for a join to a **thread in flight** — an optional `context_share` naming the share mode and the
  boundary message id (§4.6, D10).
- `session.member.remove` — carries `member_type`, `member_id`, `actor`, `ts`, optional `reason`.
- `session.member.context` — a **later change** to a member's `context_share` (§4.6 rule 3): carries
  `member_type`, `member_id`, `context_share`, `actor`, `ts`. It is a new event, never a rewrite of the
  `add` — "what did it know when it replied" stays answerable after the fact.
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
7. **A named group fans out; a capability does not** (CR-CHAT-013, D8). A message addressed to a **named
   group** issues one delivery **per distinct current member** through the same path — the roster is
   curated data, so the set is enumerable and recordable *before* the send, exactly as a session audience
   is, and each delivery carries the same per-target idempotency key (rule 2). A **capability** target
   resolves to **one** holder (rule 4). The two are different address kinds and are never conflated: a
   group's audience is knowable, a capability's holder is decided at delivery time and is recorded then.

**BUILT (CR-CHAT-019):** the one-call fan-out. `POST /sessions/{id}/messages` resolves the audience (the
active participants, or an explicit target list), writes the transcript record FIRST as the intent, issues
one delivery per participant through this same path (§3.2's ordering), and writes each outcome back onto
that record. `GET /sessions/{id}/messages` serves the resulting ordered cross-agent transcript. A client
may still compose a fan-out by hand with a shared `session_id` / `thread_id`; the route is the shipped
one-call form of it.

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
| **session** | The **durable room**: participants, membership, an ordered transcript. This document. A **channel** (a project-scoped top-level room) and a **direct message** (1:1 with one agent) are session `kind`s (§1.2), not new levels. | **Nothing of its own.** A session is an **aggregate** over inbox deliveries (§3.4, D1). Its `session_id` is already a wire tag (deliver body, webhook `crier.session_id`, federation hold) and a guard policy key. | **DURABLE**, per §3.5. | N principals + agents |
| **thread** | A **named reply subtree inside one session**: a thread root plus its replies, or a **sub-thread deliberately branched** off a message in another thread (§4.5). A reply never leaves its thread and never adds a level. | Carried as `thread_id` on the same deliver body (CR-FEAT-004) and as a guard policy key (`thread:<id>`) — so the guard can already scope a policy to a conversation or a thread. | durable with its session | N, inside one session (a **tree** of threads) |

**The rule the UI must obey:** there is no fourth concept. `A`'s channels + threads, `B`'s session
console, `C`'s conversation cards and `D`'s session hub are all views over *session* (Z3) with *topic* as
the only fan-out-without-history primitive, and *inbox* as the only delivery mechanism. A UI element that
needs a level not in this table is a finding, not a new primitive.

Two v2 additions do **not** add a level, and it is worth saying so explicitly because both sound like they
might: a **channel** and a **direct message** are session `kind`s (§1.2), so they are views over *session*;
a **named group** is an **address target over agents** (§2.1), not a room and not a level. The only tree in
this model is the **thread tree**, and it grows only by a deliberate branch (§4.5).

Note what is **PARTIAL** today: `thread_id` is a **wire tag and a policy key, not a stored field** —
`registry.InboxEntry` carries no `session_id` and no `thread_id`. A thread is therefore **not
reconstructable from stored messages**; it is reconstructable from the transcript, which is why the
transcript is the record (§4.3).

### 4.2 Reply semantics

**What a reply carries** (the wire + record fields, all required unless marked):

| Field | Meaning |
|---|---|
| `session_id` | The room. Same identity as the deliver body's `session_id`. |
| `thread_id` | The **thread this record belongs to** — the id of that thread's ROOT message. One key per thread. A reply **never** carries a new `thread_id`: replying does not move a record out of its thread (D11, §4.5). |
| `parent_id` | The **immediate** parent's message id — what this record replies to. Present on a reply, absent on a thread root. It is **reply attribution, not depth**: a reply-to-a-reply is at the SAME level, in the SAME thread (D11, §4.5). |
| `message_kind` | `plain` \| `addressed` \| `task` — the three kinds of §4.4. Required, and **not** the deliver body's existing `kind` (that field is the envelope kind: `message` \| `configure` \| `configure_ack`) — a different field, never overloaded (§4.4 rule 4). |
| `message_id` | This message's id, identical to the id the deliver path returns (one identity, §3.4 rule 1). |
| author | The authoring agent id, **plus** the Principal when a human spoke as it (CR-CHAT-007). |
| `ts`, `seq` | Ordering (§3.1). |
| audience | The resolved delivery set (below), recorded on the record so the fan-out is auditable **and** so a later reader does not have to re-derive it from a membership that has since changed. |

**Root vs reply.** A message with no `parent_id` is a **thread root**; its `thread_id` **is its own
message id**. A reply's `thread_id` is the root's id, which is what makes a subtree a single lookup and
what makes `A`'s "4 replies" pill and `D`'s connector rail derivable from data rather than maintained.
**A reply does not deepen the thread**: replying is level-preserving, and the depth rule (§4.5, D11) is why
the default shape of a conversation is flat. This sentence is a **correction** — v1 named `parent_id` as
"the immediate parent" and left depth open; the record fields are unchanged, but their reading is now
fixed: `parent_id` says *what was replied to*, never *how deep this is*.

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
- **A record's LEVEL is its thread, never its `parent_id` chain.** Depth is a thread's position in the
  **thread tree** (§4.5): the thread's `parent_thread_id` and `anchor_message_id` state where it hangs,
  and a reply's `parent_id` states only what it replied to. A reconstruction therefore attaches records
  to their replies **without inferring a level from reply hops** — the correction D11 makes (§4.2). A
  reader that treats a three-deep `parent_id` chain as three levels has misread the record.
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

### 4.4 Message kinds — a tag is addressing, not an action (CR-CHAT-018, D12)

Three kinds. They are **structurally distinguishable on the wire, in storage and in the transcript**, not
merely by reading the text:

| Kind | What it is | Obligation | May create work / carry a lifecycle |
|---|---|---|---|
| **plain** | A message in a thread, seen by the thread's participants. | none | no |
| **addressed** | A message carrying one or more `Address` tags (`specs/CHAT-ADDRESSING.md`). The tagged agent is the **intended reader**; everyone else still sees it per the thread's own audience rule (§4.2). | none — **nothing executes** | no |
| **task** | The explicit "do this" kind. | the addressee is asked to act | **yes — this kind only** |

Rules:

1. **A tag is addressing.** `@agent` inside a message says "this is for you". It does **not** mean "execute
   this", it does **not** fan out to other agents as a task, and it does **not** create a sub-thread
   (§4.5, D11). This is the **safety property**: conflating *addressed* with *task* is how a chat with
   agents becomes a system that silently does things — a tag would be a remote command and an addressed
   reply would fan out work to strangers.
2. **Only `task` may create work**, be claimed, run, complete or fail. Plain and addressed messages have
   no lifecycle. A `task` is also the only kind that may be addressed to a capability or a named group
   **for execution** (§2.1, §3.4).
3. **The kinds are data, not inference.** A record carries its kind; a reader must be able to tell an
   addressed message from a task **without guessing from its text**, and a client must not upgrade a kind
   on its own (no client-side "this looked like a task").
4. **The field is NOT the shipped envelope `kind`.** The deliver body's existing `kind` is the **envelope
   kind** — `message` (default) | `configure` | `configure_ack` (`docs/openapi.yaml`, the deliver request)
   — and it must **not** be overloaded: a `configure` directive and a task are different things. The
   message kind therefore rides its **own record field** (`message_kind`, §5), and how it maps onto the
   deliver body is a decision for CR-CHAT-018 / CR-CHAT-019 — never a redefinition of an existing field.
5. **Rendering follows the kind** (`CHAT-INTERFACE.md` §3.8.3): an addressed message is emphasis on the
   addressed participant; a task is a distinct card. Drawing them alike is a defect.

**NOT BUILT:** no wire field, no stored field and no record type carries the message kind today; the
three-way distinction exists only as this rule. The one identity discipline of §3.4 is unaffected — a
message has one id across the room whatever its kind.

### 4.5 The depth rule — depth is EARNED (CR-CHAT-017, decision D11)

> **A reply STAYS IN THREAD.** Agents talking to each other inside a thread are messages at the
> **same level** — not new levels. Depth is **not** a function of who replied, of how many replied, or of
> a tag being used. A **sub-thread** is created **only when deliberately branched** (an explicit action or
> a shortcut the sender takes on purpose), and the **thread tree** is the only thing that deepens.

**This corrects an earlier answer**, and the correction is stated rather than quietly applied. v1 left the
question **open** — §6 item 5 asked whether a thread is "arbitrarily deep, or flat with `parent_id` used
for quoting only" — and its reply record (§4.2) named `parent_id` as *the immediate parent*, which can be
read as *a reply-to-a-reply is a deeper level*. **That reading is wrong.** `parent_id` is reply
attribution; a level is a **thread**; a thread appears only when one is deliberately created. Under D11, §6
item 5 is **ANSWERED** (below).

Consequences, all binding:

1. **Addressing a non-member does NOT spawn.** A message that tags an agent who is not in the current
   thread's audience does **not** spray the thread to them and does **not** open a level. The tag is
   addressing (§4.4); the audience rule is §4.2, and a sender who tags someone outside the audience sees
   the **resolution** (who would be notified) before the send.
2. **A deliberate branch creates a sub-thread.** When the sender takes the explicit branch action — a
   command, a shortcut, a pick in the UI — a **sub-thread** is created: a **new thread** whose root is the
   triggering message, anchored to the message it branched from in the parent thread. The parent thread is
   left **byte-identical apart from an anchor** to the child: no message is moved, re-parented or copied,
   and **branching issues no fan-out by itself**.
3. **The parent's anchor is the whole trace.** The branch is recorded once, on the child thread (`§5.1`,
   `session.thread.branch`), carrying `parent_thread_id` and `anchor_message_id`. Nothing about the
   parent's messages changes, so a reader who never opens the child sees the parent exactly as it was.
4. **A sub-thread is a thread.** It has its own `thread_id` (its root's message id), it is a normal
   address target (`thread_id` on the deliver body, §4.1), and it obeys every rule in this document
   including §4.3's reconstruction property, extended by the thread tree.
5. **Depth is data, not layout.** The UI may collapse, summarise and rail the tree
   (`CHAT-INTERFACE.md` §3.8.2; §4.7 here), but the tree it draws is the thread tree the records state —
   **never** a reply count, a participation count or any inferred shape.
6. **Two branches from one message are siblings, not levels.** Branching twice from the same message
   creates two parallel sub-threads; neither is deeper than the other, and each carries its own anchor.

**NOT BUILT:** no branch operation, no `parent_thread_id`, no anchor field and no sub-thread record exists
today — the rule has nothing behind it yet.

### 4.6 Late join — the context-share record (CR-CHAT-016, decision D10)

Adding an agent (or a human) to a thread **in flight** asks **how much context to share**. Four modes:

| Mode | What the joiner is given |
|---|---|
| `none` | nothing from before the join — the joiner starts fresh |
| `summary` | a generated digest — **the default** (D10) |
| `since <message-id>` | the messages after a named **boundary** message |
| `full` | the whole thread history |

Rules:

1. **The answer is recorded on the thread as a system event**, naming the **mode** and the **boundary
   message id** — as part of the member-add event, or as the `session.member.context` event that follows it
   (§2.3). Without that record, an agent given everything is indistinguishable from one given nothing, and
   *why it answered as it did* is unanswerable.
2. **`summary` is a generated index**, not a replacement for the record: it is marked generated, and the
   raw messages remain reachable underneath it (§4.7 rule 2). A joiner given `summary` can always reach the
   raw thread.
3. **A later change is permitted and is itself recorded.** Changing the mode appends a
   `session.member.context` event — a new record, never a rewrite of the add. The transcript therefore
   shows the share history, and "what did it know when it replied" stays answerable at any later point.
4. **A spawned sub-thread inherits the question.** A sub-thread's first member-add records its own share
   mode and boundary, defaulting to `summary` like any other join (§4.5).
5. **A share mode never widens the audience.** What a joiner is *given* is a context decision recorded on
   the room; **who receives deliveries** stays the audience rule of §4.2 and the fan-out of §3.4. A share
   mode cannot make a non-participant a recipient, and it cannot reach into an inbox: the delivery mechanics
   are unchanged.
6. **`none` is a first-class answer, not a failure.** It is recorded like any other, so a joiner that was
   deliberately given nothing is distinguishable from one whose context was never decided.

**NOT BUILT:** nothing records a share mode or a boundary message id today; there is no join-time prompt
and no `session.member.context` event. Owed: CR-CHAT-016 (the surface and the record) with the shape in §5.

### 4.7 Navigability — what the record must support (CR-CHAT-017, second half)

The visual surface is `CHAT-INTERFACE.md` §3.8.2. The properties **these records must have**, so the UI is
never asked to invent structure the transcript does not carry:

1. **Collapse is derivable.** A reader can compute a message's **level** (its thread's depth in the thread
   tree) and a thread's **branch factor** from the records alone: `thread_id`, `parent_thread_id` and the
   anchor (§4.5) give the tree, and `seq` gives the order. A collapse threshold (**default 3**) is a
   **client rule over that data** — never a stored shape and never a server-side truncation.
2. **A summary is an INDEX, never a record.** A summary — per thread or per sub-thread — is **generated**,
   is **marked as generated**, and **never replaces** the raw messages: the messages it summarises stay in
   the transcript and the summary points at them. A summary that exists without its raw messages under it
   is a defect, because the transcript is the record (§4.3).
3. **Search returns LOCATION.** A hit must be answerable as a **path**:
   `namespace > channel > thread > sub-thread > message`. The transcript tree supplies every element of
   that path (the session's `namespace`, its `kind`, the thread, its parent thread, the message), and the
   search is filterable by **agent**, **capability**, **has-attachment** and **unresolved**.
4. **"Unresolved" is derivable, not a trusted flag.** A thread holding a `task`-kind message with no
   terminal outcome, or a message whose delivery outcomes are incomplete (§3.2), is **unresolved** — a
   query over the records, not a separate bit a client is trusted to maintain.
5. **Attachment presence is derivable per message.** Whether a message carries an attachment is a property
   of the message's (opaque) payload — the asset **reference** of CR-CHAT-014 — so the `has-attachment`
   filter needs the reference on the message, not a content index.

**NOT BUILT:** no depth/ancestry read, no summary surface, no generated marker and no search read exist.
The route work is CR-CHAT-017 / CR-CHAT-019 / CR-CHAT-020.

### 4.8 Compile — gathering N messages into ONE cited bundle (CR-CHAT-028)

**A COMPILE selects N messages**, from one thread, from several threads or from what the humans said,
**MERGES them into ONE payload**, and **hands that payload to an agent by tagging it**. It answers a
different question from a quote or a reply, and the two are not the same affordance:

| Affordance | What it answers | What it carries |
|---|---|---|
| **reply / quote** | "in answer to **X**" | an id **pointer** (`parent_id`), plus whatever text the sender wrote |
| **compile** | "here is everything relevant, gathered" | the source **content inline**, plus a **provenance citation per part** |

Both are needed. A reply attributes a remark to the message it answers; a compile assembles a working set
for a reader who was not there.

**The design rule that makes a merged message safe — all of it binding:**

1. **A merged message must not become an untraceable blob.** Every part carries a
   **provenance citation** — the `source_session_id` and `source_message_id` it came from — and the part's
   content travels **with** that citation. There is no shape in which a part has content and no source.
2. **The merged message is a NEW message.** It has its own `message_id`, it opens its own thread (a
   citation is **not** `parent_id`: a compile is not a reply, and §4.2's reply attribution is unchanged),
   and it is recorded by the ordinary message record. The **sources are not mutated**: nothing is moved,
   re-parented, copied over or deleted, so a source's own thread and content are exactly what they were.
3. **The sources stay resolvable to anyone who may read them.** A recipient can take a part's citation and
   resolve it back to the original — its content **and its location** (session, message id, author,
   timestamp, `seq`, thread). The resolution runs as the **reader**, not as the compiler: a part whose
   source the reader may not read resolves to a **named gap**, never to content.
4. **A source the compiler could not read is OMITTED WITH A NAMED GAP — or the merge is refused.** A
   source that is in another realm, does not exist, is not in the session that was named, or lies in a
   session the compiler is neither a member of nor granted a read on (§4 row 13 of `CHAT-INTERFACE.md`)
   is emitted as a part with `cited: false` and a machine-readable **reason**, carrying its citation.
   A source is **never silently included** (content must not leak) and **never silently dropped** (the
   reader must be able to see both that it was named and why it is missing). A merge in which **every**
   source is unreadable is **refused** outright, rather than shipped as an empty shell.
5. **Tagging an agent is ADDRESSING, never an action** (§4.4, D12). A compiled message is `plain` or
   `addressed` and can never be `task`; a tag on a compile is never an instruction to execute. This is the
   v2 safety invariant applied to the one affordance most likely to be mistaken for "go do this".
6. **No second delivery path.** The compiled message is fanned out through the shipped delivery path
   (§3.4) exactly as any other message is — one `POST /agents/{id}/inbox` per participant, with the same
   guard choke point, durable inbox write, lease / ack / TTL, dead-lettering and federation fallback.
7. **A compile writes nothing to its sources' sessions.** The only record it appends is its own message in
   the session it was compiled into.

**Where it lives.** `POST /sessions/{id}/compile` performs the merge and the fan-out, and
`GET /sessions/{id}/messages/{mid}/expand` resolves a compiled message's citations back to their
originals. Both are ADDITIVE to CR-CHAT-019's session surface and are clients of the same primitives
(`docs/openapi.yaml` is the wire contract; `internal/session` is the implementation).

**Built:** CR-CHAT-028 ships both routes, the new-message property, per-part citations, the reader-scoped
expand and the omit-with-named-gap / refuse behaviour above.
**Owed:** the per-principal **grant** check that would let a source be readable by a grant where membership
does not apply is `CHAT-PERMISSIONS.md` (CR-CHAT-003). Until it ships, a private source resolves by
**membership** alone, and every non-member outcome is the named gap of rule 4 — the structural branch is
in place, so arming the ACL adds a decision to an existing refusal, not a new code path.

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
{"v":1,"type":"session.create","session_id":"…","seq":1,"ts":"…","namespace":"…","kind":"channel","title":"…","created_by":{…},"retention_seconds":86400}
{"v":1,"type":"session.member.add","session_id":"…","seq":2,"ts":"…","member_type":"agent","member_id":"atlas","role":"member","actor":{…},
 "context_share":{"mode":"summary","boundary_message_id":"…"}}
{"v":1,"type":"session.member.context","session_id":"…","seq":3,"ts":"…","member_type":"agent","member_id":"atlas","context_share":{"mode":"full"},"actor":{…}}
{"v":1,"type":"session.member.remove","session_id":"…","seq":4,"ts":"…","member_type":"agent","member_id":"atlas","actor":{…},"reason":"…"}
{"v":1,"type":"session.message","session_id":"…","seq":5,"ts":"…","message_id":"…","thread_id":"…","message_kind":"addressed","author":{…},"payload":…,
 "audience":{"rule":"session"|"reply-default"|"explicit","targets":[{"kind":"agent","id":"nimbus"},…]},
 "outcomes":[{"target":"nimbus","outcome":"delivered","inbox_entry_id":"…"}],"idempotency_key":"…"}
{"v":1,"type":"session.thread.branch","session_id":"…","seq":6,"ts":"…","thread_id":"…","parent_thread_id":"…","anchor_message_id":"…","root_message_id":"…","actor":{…},"reason":"…"}
{"v":1,"type":"session.thread.reply","session_id":"…","seq":7,"ts":"…","message_id":"…","thread_id":"…","parent_id":"…","message_kind":"plain","author":{…},"payload":…,
 "audience":{…},"outcomes":[…],"idempotency_key":"…"}
{"v":1,"type":"session.close","session_id":"…","seq":8,"ts":"…","actor":{…},"reason":"…"}
{"v":1,"type":"session.reopen","session_id":"…","seq":9,"ts":"…","actor":{…}}
```

Fields shared by every record: `v`, `type`, `session_id`, `seq`, `ts`. `session.message` and
`session.thread.reply` carry `message_id`, `thread_id`, **`message_kind`** (`plain` \| `addressed` \|
`task` — §4.4), `author`, `payload`, `audience`, `outcomes`, `idempotency_key`; `thread.reply`
additionally carries `parent_id`. The **thread tree** is recorded by `session.thread.branch` — the new
thread's `thread_id` plus `parent_thread_id` and `anchor_message_id` (§4.5) — and the **late-join
context** by the optional `context_share` on `session.member.add` and by `session.member.context` for a
later change (§4.6). A `session.member.add` for a **channel** join carries no `context_share` unless the
room is in flight; a `session.message` or `session.thread.reply` record is a **thread root** when its
`thread_id` equals its own `message_id`, and the only record that creates a new `thread_id` is
`session.thread.branch`. Replay: order by `(session_id, seq)`, **keep-LAST** per `(session_id, seq)`
(identical duplicates are no-ops), idempotent by `message_id` for message records. A `session.message`
whose `outcomes` are incomplete is a message with unresolved deliveries — a repairable state (§3.2), not a
corrupt line.

### 5.2 PostgreSQL — the query view

Built from the log; never written in parallel with it (D6). Table and column names are fixed here so the
view and the client cannot invent two schemas:

- `chat_sessions(id PK, namespace, kind, title, created_by, created_at, state, closed_at,
  retention_seconds, group_id, visibility)` — one row per session, the projection of
  `create`/`close`/`reopen`. `kind` is `channel` \| `direct` (§1.2), so a channel list and a DM list are
  two queries over one table — not two tables.
- `chat_session_members(session_id FK, member_type, member_id, role, added_at, removed_at, added_by,
  removed_by)` — the projection of the membership events; a membership **at time T** is a query over
  `added_at <= T AND (removed_at IS NULL OR removed_at > T)`.
- `chat_transcript(session_id, seq, message_id, thread_id, parent_id, message_kind, author_type, author_id,
  principal_id, payload, guard jsonb, audience jsonb, created_at, PRIMARY KEY (session_id, seq))` — the
  projection of message records. `(session_id, seq)` is the ordering key; `thread_id` + `parent_id` are
  what §4.3 requires (`parent_id` is **attribution**; a message's LEVEL is its thread, read from
  `chat_threads` below); `message_kind` is `plain` \| `addressed` \| `task` (§4.4), so the
  addressed-vs-task distinction is **queryable**, not only parsed from text; `guard` carries the shipped
  `guard.Meta` so a flagged message is queryable, not just rendered.
- `chat_deliveries(session_id, message_id, target_agent_id, inbox_entry_id, outcome, updated_at,
  PRIMARY KEY (session_id, message_id, target_agent_id))` — the per-target fan-out outcome (§3.2). It is
  a **projection**, not a queue: the authoritative delivery is the agent's inbox entry, and this row is
  how the room reads its outcome back.
- `chat_threads(thread_id PK, session_id, parent_thread_id, root_message_id, anchor_message_id, created_by,
  created_at)` — the projection of `session.thread.branch` (§4.5): the **thread tree**, one row per thread.
  A root thread has `parent_thread_id` NULL and `anchor_message_id` NULL; a sub-thread names both. The
  tree is what depth (§4.7 rule 1) is a query over — never `parent_id` hop-counting.
- `chat_context_shares(session_id, member_type, member_id, mode, boundary_message_id, set_by, set_at)` —
  the projection of the member's latest `context_share` (§4.6, D10). A **latest-state** row for the
  question "what does this member hold", with the **events** (`session.member.add` /
  `session.member.context`) remaining the record of how it changed.

Two v2 notes on scope, so neither is smuggled in through the shapes above: the **named group** is **not**
a session table (its object and roster are CR-CHAT-022, its grants CR-CHAT-003 — this document only fixes
that its send fans out like a session send, §3.4 rule 7), and an **attachment** is a **reference inside
the (opaque) `payload`**, not a table here — its storage, authorization and lifecycle are
`specs/CHAT-STORAGE.md` and `specs/CHAT-PERMISSIONS.md` (CR-CHAT-014, D9).

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
4. **Are DMs a session type? — ANSWERED (v2).** Yes: a DM is the **same object** with `kind: direct`
   (§1.2), a 1:1 with one agent, not a third primitive — one membership model, one transcript, one
   fan-out rule. The *permission* consequence (what a DM implies for grants, and whether a DM defaults to
   different visibility) remains CR-CHAT-003's, and is not fixed here.
5. **Thread depth / reply-to-reply. — ANSWERED, and the earlier reading was WRONG (v2, D11).** v1 asked
   whether a thread is arbitrarily deep, or flat with `parent_id` for quoting only. The rule is §4.5: **a
   reply STAYS IN THREAD** — agents talking to each other are messages at the **same level** — and a
   sub-thread is created **only when deliberately branched**. `parent_id` is attribution, not depth. What
   the images draw as nested connectors (`D`) is therefore the **thread tree**, which is flat by default.
6. **Can a thread span sessions?** A strict no is assumed (a thread is inside one session). If a
   cross-room reply is ever wanted, it is a new primitive and needs a decision, not a field reuse.
7. **Edit / delete / react.** Out of scope here; if wanted, they are **new records** (never mutations),
   and their re-notification semantics must be stated. `D` draws a per-message overflow affordance whose
   menu is not drawn.
8. **Session groups.** `group` (CR-CHAT-010) is reserved but its semantics — is a group a namespace, a
   label, or a room-of-rooms — are undecided; a room-of-rooms must not become a second membership model.
   **This is not the named group of §2.1** (CR-CHAT-013): that is an **address target over agents**, with
   no session grouping semantics at all. The two must not be merged in storage or in naming.
9. **Presence of a *human* participant.** The registry's derived presence is mesh/registry evidence for
   **agents**; a Principal's online state has no server signal and would need one (or the UI must not
   draw a dot for a human). `A` draws a presence dot on every avatar, including principals.
10. **`state` vocabulary.** `open` / `closed` is this document's minimum. `B` implies more (`ACTIVE`, a
    "marked complete" event, a renewal event). Do paused / archived states exist?
11. **Who may close a session**, and can a member be removed from a closed room? §1.3 forbids new records
    after close, so the answer must be "reopen first" or "close is not absolute".

Added with the v2 detail pass — open, and deliberately not answered here:

12. **The named-group object** (CR-CHAT-022 / CR-CHAT-003) — its storage, its roster, who may edit it and
    what a group grant means. This document fixes only the **delivery shape** (§2.1, §3.4 rule 7).
13. **Summary generation** (CR-CHAT-017) — who generates a per-thread summary, where it is stored and
    whether it is cached. Not open: it is an **index** — generated, marked generated, raw messages one
    click under it, never replacing the record (§4.7 rule 2).
14. **Search** (CR-CHAT-017 / CR-CHAT-020) — an index over the transcript or a scan. Not open: a hit
    returns its **location** as a path, with the four filters (§4.7 rule 3).
15. **The message kind on the wire** (CR-CHAT-018 / CR-CHAT-019) — the record field is fixed here
    (`message_kind`, §5); how it maps onto the deliver call, and whether a `task` reuses the shipped
    lease/ack lifecycle or carries a lifecycle of its own, is the row's decision. What is fixed: the
    shipped envelope `kind` is **not** the message kind and is not redefined (§4.4 rule 4).
16. **A boundary that has aged out** (CR-CHAT-016) — when `since <message-id>` names a message removed by
    retention, the recorded share is a **hole** that must be reported as one; how the reader distinguishes
    it from a never-shared context is undecided (it is the same distinction §4.3's broken-thread finding
    needs).
17. **The attachment reference's shape** (CR-CHAT-014 / CR-CHAT-006) — the reference rides the opaque
    `payload`; its exact form, and whether the transcript duplicates it as a column for the
    `has-attachment` filter (§4.7 rule 5), is `CHAT-STORAGE.md`'s decision alongside
    `CHAT-PERMISSIONS.md` for fetch authorization (D9).

---

## 7. Status

**Status: DRAFT v2 · 2026-10-03 · CR-CHAT-002 + CR-CHAT-005.**

**Not built** — this document specifies objects and rules that no route implements:

- **The session object and its routes ship (CR-CHAT-019).** `POST /sessions`, `GET /sessions`,
  `GET /sessions/{id}/messages`, `POST /sessions/{id}/messages` and the membership routes
  (`GET`/`POST /sessions/{id}/participants`) exist. `session_id` remains a **wire tag** on the deliver body
  (`POST /agents/{id}/inbox`, CR-FEAT-004), on the webhook envelope (`crier.session_id`) and in the
  federation hold record, and a **guard policy key** (`session:<id>`) — and is now also the key of a stored
  session object. Still owed: the close/reopen ROUTE (§1.3's state ships; the route is CR-CHAT-002's).
- **`thread_id` IS stored (CR-CHAT-019).** `registry.InboxEntry` carries it and the deliver path records
  it, so a thread is reconstructable from stored messages alone. `session_id` is still NOT stored on
  `InboxEntry` (the session store carries the transcript record, which names the session).
- **The fan-out ships (CR-CHAT-019).** `POST /sessions/{id}/messages` is the one-call "send to the
  session": it enumerates the membership, issues one `POST`-equivalent delivery per participant through
  the same path, and records the transcript record with each target's outcome.
- **No append log and no session tables.** The JSONL record types and the four Postgres tables in §5 are
  target shapes; nothing writes them (CR-CHAT-006 owns the mechanism).
- **No membership, no roles, no grants** (CR-CHAT-003) and **no address parser** for `@team:` / `@ns/*` /
  `#session` (CR-CHAT-004).
- **No lease renewal** — the lease is taken per retrieve and released by ack; the renewal event `B` draws
  has no operation behind it.
- **No session state surface, no visibility/privacy field, no session group, no edits or reactions.**
- **Nothing added in v2 is built either.** No session `kind` — no channel and no DM (§1.2, CR-CHAT-013);
  no named-group object, roster or group fan-out (§2.1, §3.4 rule 7 — `@team:x` is grammar, not a group);
  no `message_kind` on any wire field, stored field or record, and the shipped envelope `kind`
  (`message` | `configure` | `configure_ack`) is a **different field** that this document forbids
  overloading (§4.4); no branch operation, no `session.thread.branch` record, no `parent_thread_id` and no
  anchor, so the depth rule (D11, §4.5) is a rule with nothing behind it; no context-share record
  (`session.member.context`) and no join-time prompt (§4.6, D10); no depth/ancestry read, no generated
  summary, no generated marker and no search that returns a location (§4.7); no asset reference beyond the
  shipped opaque `payload`, and no attachment upload/fetch (CR-CHAT-014 — designed by `CHAT-PERMISSIONS.md`
  / `CHAT-STORAGE.md`, not here).

**Shipped and reused, not to be reinvented:** the durable inbox with lease / ack / TTL / dead letters
(`GET /agents/{id}/inbox`, `/ack`, `/stats`, `/transfer`, `/dead-letters`), idempotent delivery
(`idempotency_key`), delivery modes and `timeout_ms`, priority, per-namespace retention and policy
(`GET /namespaces`, CR-FEAT-029), capability delivery as a **selector** (CR-FEAT-026), the relay topic
primitive, the guard verdict metadata, and the derived agent presence (CR-FEAT-024).

Not claimed, and not to be claimed in `docs/claims.yaml`, until they exist: sessions, membership,
transcripts, threads, the fan-out, session retention, session state, and the session storage shapes — nor
any v2 addition: session `kind` (channel / direct), the named-group fan-out, the message kinds, sub-thread
branching, the depth rule, the late-join context record, the navigability properties, or the attachment
reference.
