# CHAT-THREADING.md — session, thread, sub-thread, and what a "channel" is

**Status: DRAFT v1 · 2026-10-04 · CR-CHAT-005** (threading semantics) — normative for the
three-level model, reply semantics, and the persisted `thread_id` on the record. It writes no
new object: it fixes the VOCABULARY and the STORAGE of the objects `CHAT-SESSIONS.md` defines,
and it names what is still owed.

**Parents.** `specs/CHAT-SESSIONS.md` defines the session, its membership, its transcript, the
fan-out rule and the depth rule; this document MUST NOT contradict it. Where the two overlap,
`CHAT-SESSIONS.md` is the authority and this document cites the section. `specs/CHAT-INTERFACE.md`
owns the UI; `specs/CHAT-STORAGE.md` owns the log/view mechanism and the bundle.

**Scope.** CR-CHAT-005. It does **not** build the transcript API (CR-CHAT-019), the branch
operation and its route (CR-CHAT-017), message kinds (CR-CHAT-018), summaries, search
(CR-CHAT-020), federation (`CHAT-FEDERATION.md`), permissions (`CHAT-PERMISSIONS.md`) or the
address grammar (`CHAT-ADDRESSING.md`). §7 states the boundaries.

---

## 1. The three levels, and the one exact term for each

There are **three** levels. A UI element that needs a fourth is a **finding**, not a new
primitive (`CHAT-SESSIONS.md` §4.1). Every level below is mapped to a shipped crier primitive,
so nothing here is invented.

| Level | Exact term (use this word, nowhere else) | What it is | Durable? | Multiplicity |
|---|---|---|---|---|
| 1 | **session** | The **durable room**: participants, membership and an ordered transcript. `kind` is `channel` or `direct` (§2). | yes — with its transcript (`CHAT-SESSIONS.md` §3.5) | whole realm |
| 2 | **thread** | A **named reply subtree inside one session**: one thread root message plus its replies. Its key is `thread_id`, which **is** the root's `message_id`. | yes — with its session | N per session (a **tree** of threads) |
| 3 | **sub-thread** | A **thread created by a deliberate branch** off a message in another thread (`CHAT-SESSIONS.md` §4.5 rule 2). It is a thread like any other — it just names a `parent_thread_id` and an `anchor_message_id`. | yes — with its session | N per thread |

**The tree is the THREAD tree.** Session → thread → sub-thread. A reply never creates a level;
only a deliberate branch does (§4.5, decision **D11**). The parent thread is left byte-identical
apart from an anchor, and branching issues no fan-out by itself.

### 1.1 The vocabulary rule (no alias soup)

Three words are in play and two of them are traps. The mapping is fixed and exclusive:

| Word | Means, in this document and in the UI | Never means |
|---|---|---|
| **channel** | A **session** whose `kind` is `channel` — a **session-scoped** room (a project-scoped top-level room, `CHAT-SESSIONS.md` §1.2). A channel and a direct message are the **same object** with different defaults: one membership model, one transcript, one fan-out rule (`CHAT-SESSIONS.md` §3.4). | a relay topic; a fan-out; a group of agents |
| **direct** (`dm`) | A **session** whose `kind` is `direct` — a 1:1 with one agent. Same object as a channel. | a thread; an inbox |
| **topic** | The **relay pub/sub primitive** — `POST /relay/publish`, `GET /relay/subscribe/{topic}`, `GET /relay/topics`: fan-out with **NO history**, scoped to a namespace (`CHAT-SESSIONS.md` §4.1). A subscriber that is offline misses the event; that is the contract, not a bug. | a room; a level; anything the UI draws as a conversation |

**The rule, stated once:** the UI word *channel* maps to **session** — specifically a session of
kind `channel` — and **never** to the relay topic. The only place the word "channel" denotes the
relay primitive is `CHAT-SESSIONS.md` §4.1's own naming of it ("A named fan-out channel. A
'channel' in the broadcast sense is a topic"); that is a discussion of the **relay primitive**,
not of a room, and this document and the UI do not use it that way. Concretely: when a user
"sends to a channel", crier fans out **one `POST /agents/{id}/inbox` per session member** (§3.4)
and the message lands in an ordered **transcript** — it is **not** a relay publish, and no relay
subscriber receives it.

The four UI surfaces (`CHAT-INTERFACE.md` §3.8.1) are therefore views over exactly two objects —
**session** (channels and DMs) and **thread** — with **topic** as the only fan-out-without-history
primitive and **inbox** as the only delivery mechanism.

---

## 2. What each level maps to, on the shipped bus

| Level | Shipped primitive it maps to | Identity carried on the wire | History |
|---|---|---|---|
| **session** (channel / direct) | **Nothing of its own.** A session is an **aggregate over inbox deliveries** — decision **D1** (`CHAT-SESSIONS.md` §3.4). The server today recognises `session_id` as a wire tag and a guard policy key and nothing else. | `session_id` on the deliver body (`POST /agents/{id}/inbox`, **CR-FEAT-004**), on the webhook envelope (`crier.session_id`, `specs/WEBHOOK-DELIVERY.md` §5) and in the federation hold record; guard key `session:<id>` | durable, per `CHAT-SESSIONS.md` §3.5 |
| **thread** / **sub-thread** | The same deliver path, with the thread tag; a **guard policy key** can already scope to it, so a policy can be scoped to a conversation before any session route exists. | `thread_id` on the deliver body (**CR-FEAT-004**); guard key `thread:<id>` (`specs/LLM-MESSAGE-GUARD.md` §4.2, `internal/guard/policy.go` `ResolvePolicy`) | durable with its session |
| **topic** | The relay (`POST /relay/publish`, `GET /relay/subscribe/{topic}`) | topic name, namespace-scoped | **none** |

**`thread_id` is one key per thread, and the key is a message id.** The thread root's
`thread_id` **is** the root's own `message_id`; a reply's `thread_id` is that same value, which
is what makes a subtree a single lookup and what makes a reply-count pill derivable from data
rather than maintained (`CHAT-SESSIONS.md` §4.2, §4.3).

---

## 3. Reply semantics

A **reply** is a message with a `parent_id`. Its fields, and nothing else, decide where it sits:

| Field | Meaning | Required on |
|---|---|---|
| `message_id` | This message's id, identical to the id the deliver path returns (one identity per message, `CHAT-SESSIONS.md` §3.4 rule 1). | every message |
| `thread_id` | The thread this record belongs to — the id of that thread's **root message**. A reply **never** carries a new `thread_id`: replying does not move a record out of its thread. | every message |
| `parent_id` | The **immediate** parent's message id — what this record replies **to**. It is reply **attribution, not depth**. | a reply only; **absent** on a thread root |
| `message_kind` | `plain` \| `addressed` \| `task` (`CHAT-SESSIONS.md` §4.4, D12) — a distinct record field, never the envelope `kind`. | every message (CR-CHAT-018) |
| `seq`, `ts` | `seq` is the ordering authority inside the session; `ts` is display and correlation (§3.1). A client never orders by `ts` alone. | every message |
| audience | The **resolved** delivery set, recorded on the record so the fan-out is auditable and a later reader does not re-derive it from membership that has since changed (§4.2). | every message |

**Root vs reply.** A message with **no** `parent_id` is a **thread root**, and its `thread_id`
**is its own `message_id`** — the writer forces this and the store refuses a root whose
`thread_id` differs (`CHAT-SESSIONS.md` §4.3; `internal/session/store.go` `Record.Validate`).
A reply's `thread_id` is the root's id.

**What a reply carries and what it does not:**
- It **re-notifies**, **once per (message id, target)** — enforced by the per-target idempotency
  key, so a retry or a replay cannot double-deliver (§4.2). A delivery to the replier is never
  issued.
- Its **default audience** is the parent's author plus every participant who has already spoken
  in that thread, minus the replying author (§4.2). An explicit address list overrides it, and
  the record states which rule produced the audience (`AudienceRule`).
- It **does not deepen the thread**. See §4.
- An **edit** is not a reply: it is a new record and it does not re-notify.

**Addressing a thread.** A thread (and a sub-thread) is a normal address target: `thread_id` on
the deliver body. `#session` addressing is `CHAT-ADDRESSING.md`'s; this document only fixes that
a target is a **thread id**, never a "message path".

---

## 4. Depth — D11, cited, and the one thing that is NOT depth

**The rule, verbatim from `CHAT-SESSIONS.md` §4.5 (`CR-CHAT-017`, decision D11):**

> **A reply STAYS IN THREAD.** Agents talking to each other inside a thread are messages at the
> **same level** — not new levels. Depth is **not** a function of who replied, of how many
> replied, or of a tag being used. A **sub-thread** is created **only when deliberately branched**
> (an explicit action or a shortcut the sender takes on purpose), and the **thread tree** is the
> only thing that deepens.

Consequences this document adopts without amendment:

1. **Depth is the thread tree's position, never a `parent_id` hop-count.** A reader attaches
   records to their replies **without** inferring a level from reply hops; a reader that treats a
   three-deep `parent_id` chain as three levels has **misread the record** (`CHAT-SESSIONS.md`
   §4.3, §4.5).
2. **The depth read is a walk over `parent_thread_id`.** Implemented as `State.ThreadDepth`
   (`internal/session/session.go`): a root thread is level **0** and each deliberate branch adds
   one; a cycle reports `-1` rather than looping.
3. **A sub-thread is a thread.** It has its own `thread_id`, it is a normal address target, and it
   obeys §4.3's reconstruction property extended by the thread tree.
4. **Two branches from one message are siblings, not levels** (§4.5 rule 6).
5. **Depth is data, not layout.** Collapse (default 3), the rail and the indentation are client
   rules over the thread tree; never a stored shape and never a server-side truncation
   (`CHAT-SESSIONS.md` §4.7 rule 1, `CHAT-INTERFACE.md` §3.8.2).

### 4.1 `parent_id` is walked for exactly ONE thing: recovering a missing thread key

There is one operation that walks `parent_id`, and it is **not** a depth measure. A record that
reached the reader **without** a `thread_id` (§5.3) has its **thread key** derived: a root's key
is its own message id, and a reply's key is the key of the nearest ancestor in its `parent_id`
chain.

That is a **grouping key**, and grouping is precisely what `CHAT-SESSIONS.md` §4.3 says the
transcript alone must support. It is not a level: deriving the key never creates a sub-thread,
never changes `parent_thread_id`, and never affects `ThreadDepth`. A derived reply sits at level
**0** of its thread exactly like a reply that stated its key, because the level is the thread's.

---

## 5. The persisted `thread_id` — what shipped, what was owed, and how it migrates

### 5.1 What shipped, and the gap it left

`thread_id` shipped as a **wire tag**, not a stored field:

- on the deliver body, `POST /agents/{id}/inbox` — field `thread_id` (**CR-FEAT-004**,
  `internal/registry/handler.go` `deliverRequest`), and
- as a **guard policy key**, `thread:<id>`, so `ResolvePolicy` can already scope a policy to a
  conversation (`internal/guard/policy.go`, `specs/LLM-MESSAGE-GUARD.md` §4.2).

The **delivery** record did not carry it. `registry.InboxEntry` has **neither `session_id` nor
`thread_id`**: a message sitting in an agent's durable inbox states no room and no thread. That
is the gap `CHAT-INTERFACE.md` §4 row 15 and `CHAT-SESSIONS.md` §7 record, and it is why the
thread is not reconstructable **from the inbox**.

**The record of record is the transcript, not the inbox.** `CHAT-STORAGE.md` §2.1 fixes the
division of labour: JSONL is the **ordered append log and the transport form**, PostgreSQL is the
**query view built from it**, and §2.4 states plainly that an **inbox entry is not a
record-version** — it is the delivery queue. So the persistence owed by this row is on the
**session record**, in `internal/session`.

### 5.2 What `internal/session` stores (BUILT, this row)

`thread_id` is persisted on every message record, on both backends:

| Backend | Where | Written by | Read by |
|---|---|---|---|
| JSONL (log + transport form, `CHAT-SESSIONS.md` §5.1) | `Record.thread_id` on `session.message` and `session.thread.reply` (`internal/session/store.go`) | `Message.Record()` → `MarshalLine()`; `Record.Validate()` **requires** it on a message | `ParseRecord` (read contract) → `Replay` |
| PostgreSQL (query view, `CHAT-SESSIONS.md` §5.2) | `chat_transcript.thread_id TEXT NOT NULL`, and `chat_threads.thread_id` for the thread node (`internal/session/postgres_store.go` `SchemaStatements`) | `PostgresStore.Append` → `projectMessage` (INSERT … ON CONFLICT (session_id, seq) DO UPDATE) | `loadMessages` / `loadThreads` → `Load` |

The write contract is strict: a **reply** that states no `thread_id` is **refused**
(`ErrInvalidRecord`); the builder has no transcript, so deriving a reply's key at write time is
the caller's job (see §6, NOT BUILT).

A thread **root** materialises its `chat_threads` row from the message record alone — a root
thread has `parent_thread_id NULL` and `anchor_message_id NULL` (§4.3, §5.2), which is what makes
a subtree a single lookup with no side table.

### 5.3 The read contract, and backward compatibility

The read contract is one-directional: **the writer never produces a thread-less message, and the
reader accepts one.** `Record.Validate()` (write) requires `thread_id` on a message;
`Record.validateReadable()` (read, used by `ParseRecord` and `Replay`) does not. A record written
by the **pre-persisted generation** — the CR-FEAT-004 wire-tag era, or a bundle/dump made by it —
therefore **reads** rather than being refused.

The reader completes it in ONE deterministic pass, `State.resolveThreads()`
(`internal/session/session.go`), which every backend shares, so the JSONL log and the PostgreSQL
view derive the **same** key from the same facts and stay one value:

1. **Backfill.** A message with no `thread_id` gets one: a root's is its own message id; a
   reply's is the nearest ancestor's key along its `parent_id` chain (§4.1).
2. **Root rows.** Every root message materialises its `chat_threads` row, including a root whose
   key was just derived.
3. **Findings.** Every record whose key names no root, whose `parent_id` names no message, or
   whose thread's root was retained away is reported as a **broken thread** — `CHAT-SESSIONS.md`
   §4.3: *"it is reported as a finding, never silently re-rooted or dropped"*, and the finding
   names which case it is.

The derivation is **reported, never silent**: each derived key and each broken thread is a
`ThreadFinding` on the `State` (`Kind` = `derived-thread-id` | `broken-thread`). A transcript that
states every key — the shape this package writes — has an empty finding list.

### 5.4 The migration story

No rewrite, no downtime, no destructive migration. Both backends converge the same way:

- **JSONL (append log).** The log is **append-only and is never rewritten** — the log is the log
  of record (`CHAT-SESSIONS.md` §5.1). A legacy line stays exactly as it is and the reader derives
  its key. The derived key reaches the log on the **next record for that message**: the §3.2
  write-back a fan-out already performs (outcomes written back at the **same** `seq`, resolved by
  keep-LAST, §5.1), or a re-export → import cycle through the transport bundle
  (`CHAT-STORAGE.md` §4, §6). After that record lands, the transcript states the key on its own
  and the findings for it are empty. **No file is edited and no line is deleted.**
- **PostgreSQL (query view).** The column exists and is `NOT NULL`; a row written by the legacy
  generation carries the **empty string** `''` (not `NULL`). It is completed on the next
  re-projection of that record — `Append` → `projectMessage`, `ON CONFLICT (session_id, seq) DO
  UPDATE` — and until then `Load` derives the key in memory, exactly as the log path does. **No
  migration statement is owed in `internal/session`:** the §5.2 schema already carries the column.

**Named residual (NOT BUILT, owed by CR-CHAT-019).** The delivery-side field is still owed:
`registry.InboxEntry` carries no `session_id` and no `thread_id`, so a message in an agent's
durable **inbox** cannot yet be grouped into its thread. Wiring the deliver body's `thread_id`
(CR-FEAT-004) onto the stored inbox entry — and the routes that read a session transcript — is
the API-upgrade row **CR-CHAT-019** ("the core object surfaces: sessions, the cross-agent
transcript, membership, and a PERSISTED `thread_id`"). The **transcript** record is the record of
record (`CHAT-STORAGE.md` §2.1, §2.4); the inbox entry is the delivery queue.

### 5.5 The reconstruction read (BUILT, this row)

`State.ReplyChain(messageID)` returns a message's recorded reply chain — thread root first, then
each message replied to in turn, ending with the message itself — **from the stored State alone**:
no side table, no membership lookup, no client state. It reports whether the chain is **complete**
inside the transcript; an incomplete chain is a `parent_id` that names a message that is not here
(a retention hole), and the part that IS recorded is still returned. It is the §4.3
reconstruction for a single leaf, and the primitive every later read surface (CR-CHAT-019) is
built on.

---

## 6. Built vs NOT BUILT, per affordance

| Affordance | State | Spec anchor | Where it lives / what is owed |
|---|---|---|---|
| Session as the durable room, `kind` = `channel` \| `direct` | **BUILT** (data model) | `CHAT-SESSIONS.md` §1.2, §4.1 | `internal/session` `Session`, `Kind`; no session **route** yet |
| Thread = root message + reply tree, keyed by `thread_id` | **BUILT** (data model) | `CHAT-SESSIONS.md` §4.2, §4.3 | `internal/session` `Message.ThreadID`, `Thread`, `ThreadMessages` |
| `thread_id` persisted on the message record, both stores | **BUILT** (this row) | `CHAT-SESSIONS.md` §5.1, §5.2 | §5.2 — JSONL `Record.thread_id`; PostgreSQL `chat_transcript.thread_id` |
| Read-compatible legacy record (no `thread_id`) + derivation + findings | **BUILT** (this row) | `CHAT-SESSIONS.md` §4.3 | §5.3 — `validateReadable`, `State.resolveThreads`, `ThreadFinding` |
| Reply chain reconstructable from storage alone | **BUILT** (this row) | `CHAT-SESSIONS.md` §4.3 | §5.5 — `State.ReplyChain` |
| Depth = thread-tree walk, never a `parent_id` hop-count (D11) | **BUILT** (data model) | `CHAT-SESSIONS.md` §4.5 (D11), §4.7 rule 1 | `State.ThreadDepth` over `parent_thread_id` |
| Deliberate branch creates a sub-thread (`parent_thread_id`, anchor) | **PARTIAL** — record + view only | `CHAT-SESSIONS.md` §4.5 rule 2 | `Thread` + `Thread.BranchRecord` exist; **no branch operation and no route** |
| Thread tree read (`parent_thread_id`, `anchor_message_id`) on the view | **BUILT** (data model) | `CHAT-SESSIONS.md` §5.2 | `chat_threads` + `State.Thread`, `State.ThreadDepth` |
| `thread_id` on the **delivery** record (`registry.InboxEntry`) | **NOT BUILT** | `CHAT-INTERFACE.md` §4 row 15; `CHAT-SESSIONS.md` §7 | owed: CR-CHAT-019 — the field on `inbox_entries` + the deliver-path write |
| Cross-agent transcript read (`GET /sessions/{id}/messages`) | **NOT BUILT** | `CHAT-SESSIONS.md` §3.3, §7 | owed: CR-CHAT-019 |
| Session routes (`POST`/`GET /sessions`, membership) | **NOT BUILT** | `CHAT-SESSIONS.md` §7 | owed: CR-CHAT-002 (route) / CR-CHAT-019 |
| Branch operation / wire field / route (sub-thread spawn) | **NOT BUILT** | `CHAT-SESSIONS.md` §4.5, §7; `CHAT-INTERFACE.md` §3.8.2 | owed: CR-CHAT-017 |
| `message_kind` on the wire and in the deliver body | **NOT BUILT** | `CHAT-SESSIONS.md` §4.4 | owed: CR-CHAT-018 |
| Late-join context-share record + prompt | **NOT BUILT** | `CHAT-SESSIONS.md` §4.6 (D10) | owed: CR-CHAT-016 |
| Navigability reads (depth/ancestry route, summaries, search-by-location) | **NOT BUILT** | `CHAT-SESSIONS.md` §4.7 | owed: CR-CHAT-017 / CR-CHAT-019 / CR-CHAT-020 |
| Writer-side derivation of a reply's `thread_id` | **NOT BUILT (deliberate)** | this document §5.2 | a reply must state its thread; the store refuses a thread-less write. A wrapper that derives it from the parent is a read-then-write the API layer may add (CR-CHAT-019) — the data model does not read a transcript to write a record |

---

## 7. Non-goals (binding)

1. **No fourth level.** A UI element needing one is a finding against this document and
   `CHAT-SESSIONS.md` §4.1.
2. **No second delivery path and no second identity.** The levels are view over the shipped
   deliver path and the shipped registry; nothing here introduces a mailbox, a transport or a
   second message id (`CHAT-SESSIONS.md` §4.1, `CHAT-INTERFACE.md` §5.1).
3. **`topic` is not a room.** Nothing in this document maps a session or a thread onto the relay;
   a "channel" is a session (channel or direct), and a relay topic has **no history** by contract.
4. **`thread_id` is never re-minted.** A reply carries the thread it is in; the only record that
   creates a new `thread_id` is a deliberate branch (`CHAT-SESSIONS.md` §5.1).
5. **`parent_id` is never a level.** The only walk of `parent_id` permitted is the recovery of a
   **missing** grouping key (§4.1), and it changes no depth.
6. **This document adds no route.** Routes are CR-CHAT-002 / CR-CHAT-019 (sessions and the
   transcript), CR-CHAT-017 (branch), CR-CHAT-020 (the read surfaces the UI draws but the bus
   does not expose).

---

**Not built, stated plainly:** no session route, no transcript route, no branch operation, no
`thread_id` on the stored delivery record, no `message_kind` wire field, no late-join record, no
summary or search surface. What this row settles is the **model and the vocabulary** (§1–§4), the
**persistence of `thread_id` on the transcript record with a backward-compatible read and a
defined migration** (§5), and the ledger of what remains (§6, §7).
