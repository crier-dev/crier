# CHAT-STORAGE.md — the dual backend: JSONL as the ordered append log, PostgreSQL as the query view

Status: **DRAFT v2** · 2026-10-03 · **CR-CHAT-006** + **CR-CHAT-014**
REVISION 2026-10-03 (v2): folded in Bane's detail pass — the `asset` kind carried BY REFERENCE on BOTH
backends (§3.10, **D9**/CR-CHAT-014), the bundle that carries references and re-imports WITHOUT the bytes
plus what a reader sees then (§6.4), asset retention/GC and the per-namespace bucket/prefix policy (§6.5),
the TASK message kind on the message record (§3.7, **D12**/CR-CHAT-018), the corrected DEPTH rule — a
`thread` record exists only for a deliberate branch (§3.6, **D11** correction/CR-CHAT-017), the
federated-delivery shape in the JSONL log (§2.5), and the rule that trust material (peer keys) never travels
in a session bundle (§6.2, CR-CHAT-024/025). No v1 statement is contradicted.
Source material: `~/crier-interface/GOALS.md` (non-negotiable 7 and 8; G11, G15, G19, G21; decisions D6,
D9, D11, D12, D14), `specs/ci-003b-postgresql-persistence.md` (the shipped persistence), and the three
fleet precedents named in the goal (boards = JSONL keep-LAST; DuckBrain = git-backed JSONL row-images; the
scheduler's JSONL deploy API).

This document is the normative design authority for how the comms interface STORES and TRANSPORTS its
records. Where it and the shipped code disagree, the **code wins** — file the drift as a board row
(`DF-CRIER-*`). Nothing here may be added to `docs/claims.yaml` until the code ships (claims execute
against a live server).

Cross-references: the objects stored here are defined in `specs/CHAT-PERMISSIONS.md` (principals —
principal, binding, grant, scope, audit) and the sibling's `specs/CHAT-SESSIONS.md` (session, thread,
message).

**Glossary (normative; the exact terms every CHAT spec uses).** A **Principal** is a HUMAN user, distinct
from an agent, and can speak AS a bound agent. An **Agent** is a registered crier agent, with a CLASS
(`personal` | `service`), an OWNER and capabilities. A **Session** is a durable conversation: participants
(principals + agents), membership, an ordered transcript. A **Thread** is a subtree inside a session, created ONLY by a deliberate branch (**D11**): a reply
stays in its thread and never descends on its own.
An **Address** is a tag: `@agent`, `@team:x`, `@cap:y`, `@ns/*`, `#session`. A **Grant** is an ACL entry
binding a principal to an agent / group / session / capability. A **Namespace** is the realm/tenant wall
(already shipped: auth posture, rate limits, retention).

---

## 1. The requirement

Bane's constraint, in the words of the goal (non-negotiable 7, verbatim):

> **Dual backend, kept in sync: PostgreSQL + JSONL files.** PostgreSQL is the queryable store; JSONL is the
> **transport/interchange** form (bundle a session, ship it, re-import). Postgres alone is "big and heavy"
> — JSONL must be a first-class peer, not an export. (Bane, 2026-10-03.)

and CR-CHAT-006's own framing:

> Bane's constraint: this has been built on Postgres, which is big and heavy — JSONL must be a first-class
> peer for easy transport.

The requirement is therefore TWO things at once, and both are normative:

1. **PostgreSQL stays the queryable store.** It is what the API serves from: indexed, searchable,
   transactional. Nothing in this document removes the shipped `agents` / `inbox_entries` /
   `dead_letters` schema or the CR-FEAT-034 durability that already landed.
2. **JSONL is a first-class peer, not an export.** A session (or a team, or a whole namespace) can be
   written to disk as JSONL, shipped anywhere, and re-imported into a fresh instance — safely, repeatedly,
   and without a distributed transaction.

**A first-class peer does not mean two writers.** §2 states the shape that makes both true.

---

## 2. The shape — log first, view second, and NO two-way live writes

### 2.1 The division of labour

| | JSONL | PostgreSQL |
|---|---|---|
| Role | the **ORDERED APPEND LOG** — and the transport form | the **QUERY VIEW** built from the log |
| Written by | the API, once per record-version, append-only | the PROJECTION of a log append |
| Read by | the reconciler, the bundle exporter, a git checkout, a human | the API (every read path), search, joins, counts |
| Ordered by | the file's line order, plus the `rev` field | `delivery_sequence` (shipped) / `rev` for projected tables |
| Git | tracked, one line per version, append-only, no rewrites | not tracked (the DB is not a document) |

The fleet already runs this shape three times, which is why this is a pattern rather than a proposal:

- **boards** — `.coding-hermes/board/tasks.jsonl` + `events.jsonl` are the canonical store; SQLite/DuckDB
  is a query view, and the doctrine is append-log-with-keep-LAST;
- **DuckBrain** — the storage of record is a git repo of JSONL row-images (hourly snapshots), with a
  queryable index built from it;
- **the scheduler** — config and templates ship as JSONL through a deploy API, idempotent per
  `(template, date)`.

### 2.2 There are NO two-way live writes

**Stated as a prohibition, because it is the trap this design exists to avoid:**

> **No component ever writes PostgreSQL and then tries to bring the JSONL log up to date — and no
> component ever writes the log and then treats PostgreSQL as the thing to fix. There is exactly ONE
> write direction for new state, and it is: record → JSONL line → projection → PostgreSQL.**

| Allowed | Forbidden |
|---|---|
| append a record-version to the JSONL log, then project it into PostgreSQL | write PostgreSQL, then back-fill the JSONL log from the row |
| re-project the log into PostgreSQL (from the reconciler or an import) | run two stores with live two-way writes / distributed transactions |
| rebuild the PostgreSQL view entirely from the log | treat PostgreSQL as a second source of truth that the log must be reconciled TO |

The reason is not dogma: two live writers need distributed-transaction machinery and lose on conflict
(GOALS.md's own words: *"that needs distributed-transaction machinery and loses on conflict"*). One
direction has no conflict to lose.

### 2.3 The write path, in order

```
request → validate → append ONE line to the JSONL log (fsync)
                    → project that line into the PostgreSQL view
                    → answer 2xx
```

Two properties are decisions, and each has a rejected alternative named:

1. **The log line is appended BEFORE the projection, and a failed projection does not roll the record
   back.** The request answers `500` naming the projection failure; the record is already durable in the
   log and the next reconcile re-projects it. **Rejected alternative:** project first, append on success —
   rejected because a crash between the two leaves a queryable record with no log line, which is the exact
   class the reconciler treats as the most serious finding (§5.3.4): the view would hold something the log
   cannot justify, and the log is the log of record.
2. **Reads are served from the view, so a stuck projection is invisible until it is healed.** Accepted and
   stated: the API answers from the projection, the reconciler reports the lag as a finding, and a
   projection backlog is a first-class alert rather than a silent divergence.

### 2.4 Envelope vs body, and what is NOT a record

Every log line is a **record-version**: one immutable line carrying the whole state of one object at one
revision.

```json
{"v":1,"kind":"message","id":"msg_01J9Z7K4","rev":7,"ts":"2026-10-03T18:22:07.412Z","hash":"sha256:…",
 "author":{"principal":"prin_01J9Z6V0Q7","as_agent":"atlas"},
 "namespace":"","body":{}}
```

(`body` is `{}` here only to keep the envelope example parseable; the real shapes of every `body` are §3.)

| Field | Rule |
|---|---|
| `v` | the envelope FORMAT version (integer, starts at 1). A reader that sees a `v` it does not know **refuses the bundle loudly** rather than skipping lines. |
| `kind` | one of `principal` `agent` `binding` `scope` `session` `thread` `message` `grant` `audit` `asset`. Closed set: an unknown kind makes the import fail, naming it. (The envelope's `kind` names the RECORD class; a message BODY's own `kind` names the message KIND — `plain` \| `addressed` \| `task`, §3.7. Do not conflate the two.) |
| `id` | the object's id. `(kind, id)` is the object's identity for keep-LAST (§4). |
| `rev` | the record-version of that object, monotonic per `(kind, id)`, starting at 1. A `rev` that is not a positive integer is refused. |
| `ts` | when this version was created (RFC 3339 / UTC). Never the wall clock of the importer. |
| `hash` | `sha256:` of the canonical `body` (JSON with sorted keys, no insignificant whitespace). Optional on a fresh append; REQUIRED where a bundle travels, because it is what makes "identical bytes" checkable without a deep compare. |
| `author` | who produced the version: `{"principal":…, "as_agent":…}` (a human through a binding) or `{"agent":"<id>"}` (an agent's own signed write) or `{"system":"reconciler"\|"sweep"\|"expiry"}`. A machine did not decide a permission; an author of `system` on a `grant` record is refused (`403 DELIVERY_FORBIDDEN` with `"reason":"SYSTEM_AUTHOR_ON_PERMISSION_RECORD"` — **NOT BUILT**). |
| `namespace` | the realm wall, `omitempty`, spelled `""` for the default realm — the same discipline `namespace.Canonical` uses on the shipped wire (`Canonical("") == Canonical("default") == ""`). |

**What is NOT a record-version:** the shipped `inbox_entries` row. An inbox entry is the DELIVERY QUEUE —
lease, ack, TTL, priority, `delivery_sequence` — i.e. operational state about a message's journey, not a
revision of a domain object. The log carries the `message` record (what was said, by whom, in which
session/thread); the queue carries the delivery. Conflating them would put a lease timestamp in the
transport form and make a bundle's content depend on who has read it.

### 2.5 A federated delivery in the JSONL log (the CHAT-FEDERATION.md seam)

A delivery that crosses to or from a linked peer is logged like any other record — but its AUTHOR states the
remote origin, because the transcript has to be able to say WHERE a message came from:

```json
{"v":1,"kind":"message","id":"msg_01J9ZC0R","rev":1,"ts":"2026-10-03T20:04:11Z","hash":"sha256:…",
 "author":{"principal":"prin_fed_peer_01J9Z…","remote_instance":"peer","remote_ref":"ada"},
 "namespace":"","body":{"session":"3f9a7c2e-…","thread":null,"kind":"addressed",
         "addresses":[{"tag":"@peer/atlas","type":"agent","ref":"peer/atlas"}]}}
```

Rules:

- **The `author` names the LOCAL shadow principal plus `remote_instance` / `remote_ref`**
  (CHAT-PERMISSIONS.md §2.5, D14). The local half is an ordinary principal id, so every existing read path
  works unchanged; the remote pair is how the log states the origin.
- **A remote participant is never written as a bare foreign id.** The log records the local shadow; the
  peer instance and ref ride alongside it. A bundle therefore round-trips without needing to reach the peer.
- **Trust material does not travel here.** Peer KEYS, a peer's published key set and any federation secret
  are NOT record bodies and NOT bundle members (§6.2): a session bundle is a conversation, not a trust
  store. The trust design is `specs/CHAT-TRUST.md` (CR-CHAT-025).
- **The delivery leg itself is not this log's business.** Hold/retry and the `FEDERATION_FAILED` receipt are
  owned by WEBHOOK-DELIVERY.md and `specs/CHAT-FEDERATION.md`; the log records only that the message exists
  and who authored it.

**NOT BUILT:** no federated authorship, no shadow participation, no peer-key store. Owed: the federated
delivery path and `GET /fed/address?instance=<i>&agent=<a>` (CR-CHAT-023/026), and the peer-key store
(CR-CHAT-024/025).

---

## 3. The record shapes

One line per record-version, per object. Every example below is a complete, parseable log line. The
`agent` and `message` kinds project onto schema that **already ships**; the rest are **NOT BUILT**.

### 3.1 `principal`

```json
{"v":1,"kind":"principal","id":"prin_01J9Z6V0Q7","rev":1,"ts":"2026-10-03T17:45:05Z",
 "author":{"system":"bootstrap"},"namespace":"acme",
 "body":{"display_name":"Bane","role":"owner","status":"active","created_at":"2026-10-03T17:45:05Z",
         "last_login_at":null,"email_ref":"env:PRINCIPAL_BANE_EMAIL"}}
```

Projection: `principals(id PK, rev, display_name, role, status, created_at, last_login_at, namespace,
body jsonb)`. Note `email_ref` is an `env:` REFERENCE, never an address value — the same rule the guard's
`api_key_ref` and the namespace's `token_ref` already follow ("never an inline secret", NAMESPACES.md §4.1).

### 3.2 `agent`

```json
{"v":1,"kind":"agent","id":"atlas","rev":3,"ts":"2026-10-03T18:02:11Z",
 "author":{"principal":"prin_01J9Z6V0Q7","as_agent":"atlas"},"namespace":"acme",
 "body":{"public_key":"9f2c…","capabilities":["planning","architecture","tools"],
         "class":"personal","owner":"prin_01J9Z6V0Q7","status":"online",
         "registered_at":"2026-10-03T17:50:00Z","last_seen":"2026-10-03T18:22:03Z",
         "webhook":null,"guard":null,"a2a":null}}
```

Projection: the SHIPPED `agents` table — `id TEXT PRIMARY KEY`, `public_key BYTEA NOT NULL` with
`CHECK (octet_length(public_key) = 32)`, `capabilities JSONB NOT NULL DEFAULT '[]'`,
`status TEXT NOT NULL DEFAULT 'online'` with `CHECK (status IN ('online','offline'))`,
`registered_at TIMESTAMPTZ NOT NULL`, `last_seen TIMESTAMPTZ NOT NULL` (migration 001), plus
`webhook JSONB NULL` and `guard JSONB NULL` (003), `a2a JSONB NULL` (005) and `namespace TEXT NULL` (008)
— gains the two columns this feature needs (`class TEXT NULL CHECK (class IN ('personal','service'))`,
`owner TEXT NULL REFERENCES principals(id)`), additively, the same shape migration `008_add_namespaces`
used: NULL means "not classed", a pre-existing row reads back unchanged, and nothing is backfilled.

### 3.3 `binding`

```json
{"v":1,"kind":"binding","id":"bind_01J9Z6W4M2","rev":1,"ts":"2026-10-03T18:02:11Z",
 "author":{"principal":"prin_01J9Z6V0Q7"},"namespace":"acme",
 "body":{"principal":"prin_01J9Z6V0Q7","agent":"atlas","as_agent":true,
         "created_at":"2026-10-03T18:02:11Z","created_by":"prin_01J9Z6V0Q7"}}
```

Projection: `bindings(id PK, rev, principal, agent, as_agent boolean, created_at, revoked_at NULL)` with a
UNIQUE partial index on `(principal, agent) WHERE revoked_at IS NULL`.

### 3.4 `scope`

```json
{"v":1,"kind":"scope","id":"scope_repo_crier","rev":2,"ts":"2026-10-03T18:14:00Z",
 "author":{"principal":"prin_01J9Z6V0Q7"},"namespace":"acme",
 "body":{"holder":"deploy-bot","resource":{"kind":"repo","ref":"github.com/coding-hermes/crier"},
         "reach":{"principals":["prin_…"],"groups":["team:infra"],"capabilities":[]},
         "declared_by":"prin_01J9Z6V0Q7","declared_at":"2026-10-03T17:45:05Z"}}
```

Projection: `scopes(id PK, rev, holder, resource_kind, resource_ref, reach jsonb, declared_by,
declared_at)` with a CHECK on `resource_kind IN ('repo','box','service','dataset','namespace')` and a
foreign key to `agents(id) ON DELETE CASCADE` (a scope dies with its holder, exactly as an inbox does).

### 3.5 `session`

```json
{"v":1,"kind":"session","id":"3f9a7c2e-6b1d-4e2a-9c7e-1d2f8a4be9c1","rev":4,
 "ts":"2026-10-03T18:29:19Z","author":{"principal":"prin_01J9Z6V0Q7","as_agent":"atlas"},
 "namespace":"acme",
 "body":{"title":"Build Plan","short_id":"A7F3","status":"active","topics":["planning","infra"],
         "created_at":"2026-10-03T18:28:03Z","created_by":"prin_01J9Z6V0Q7",
         "participants":[{"type":"agent","ref":"atlas"},{"type":"principal","ref":"prin_01J9Z6V0Q7"},
                         {"type":"agent","ref":"nova"}],
         "retention_seconds":86400,"closed_at":null}}
```

`short_id` is the display affordance Option D draws (`SESSION #A7F3`) and the sibling's CHAT-SESSIONS.md
owns the collision policy; the WIRE and the log always carry the full `id` (the same rule
CHAT-ADDRESSING.md §5.3 fixes for the tag). `retention_seconds` inherits the namespace's value when
unset, exactly as the shipped per-namespace axis does.

Projection: `sessions(id PK, rev, title, short_id UNIQUE, status, topics jsonb, created_at, created_by,
retention_seconds, namespace)` plus `session_participants(session_id, participant_type, participant_ref)`
with a UNIQUE `(session_id, participant_type, participant_ref)`.

### 3.6 `thread`

```json
{"v":1,"kind":"thread","id":"thr_01J9Z8Q2K7","rev":1,"ts":"2026-10-03T18:31:02Z",
 "author":{"agent":"nova"},"namespace":"acme",
 "body":{"session":"3f9a7c2e-6b1d-4e2a-9c7e-1d2f8a4be9c1","parent_message":"msg_01J9Z7K4",
         "root_message":"msg_01J9Z7K4","depth":1,"created_at":"2026-10-03T18:31:02Z","closed_at":null}}
```

`depth` is derived and stored (a reply subtree's depth must be answerable without walking the tree on every
read), and `root_message` is what makes a thread reconstructable from the transcript alone.

Projection: `threads(id PK, rev, session REFERENCES sessions(id) ON DELETE CASCADE, parent_message,
root_message, depth INT CHECK (depth >= 1), created_at, closed_at)`.

**A `thread` record exists ONLY for a DELIBERATELY BRANCHED subtree (D11, corrected — CR-CHAT-017).** A
reply STAYS IN THREAD: it is a `message` whose `thread` points at the thread it already belongs to, at the
SAME level as its parent. Depth is NEVER a function of who replied or how many replied, and addressing a
non-member (or a group, or a capability) does not create a thread. A new `thread` record — and a `depth`
increment — is written only when an explicit branch action creates one (`POST /messages/{id}/branch`, NOT
BUILT); `depth` here therefore counts DELIBERATE branches from the session root, not reply nesting. An
earlier draft of the sub-thread rule auto-spawned on a tag; that is wrong and is not implemented by this
shape.

### 3.7 `message`

```json
{"v":1,"kind":"message","id":"msg_01J9Z7K4","rev":1,"ts":"2026-10-03T18:28:03Z",
 "author":{"principal":"prin_01J9Z6V0Q7","as_agent":"atlas"},"namespace":"acme",
 "body":{"session":"3f9a7c2e-6b1d-4e2a-9c7e-1d2f8a4be9c1","thread":null,
         "addresses":[{"tag":"@cap:data-capabilities","type":"capability","ref":"data-capabilities"}],
         "payload":{"parts":[{"type":"text","text":"Kick off the data sweep for the new environment."}]},
         "attachments":[{"ref":"asset_01J9ZB3K","kind":"asset"}],
         "kind":"addressed","task":null,"idempotency_key":"…",
         "delivered":[{"agent":"kappa","message_id":"msg_…"}],
         "guard":{"decision":"allow","policy":"builtin-default"},
         "created_at":"2026-10-03T18:28:03Z","edited_at":null}}
```

Five rules, each closing a hole a naive transcript leaves open:

- **`author` names the human AND the agent** — this is T3's fix (CHAT-PERMISSIONS.md §2.3): the bus sees
  the agent, the record names the human.
- **`addresses` records the RESOLVED tags, not the typed ones** (CHAT-ADDRESSING.md §2.2). A transcript
  must answer "who was this sent to" years later, when a group has been renamed — so the resolved
  `{tag, type, ref}` is stored, and the tag is not re-resolved on read.
- **`delivered` names the inbox entries the message fanned out into** (§2.4): the session-level record is
  the message; its deliveries are the queue rows. A message with no `delivered` entries is a message that
  was recorded and not dispatched, which is a state worth being able to see.
- **`kind` is the message KIND, a closed set `plain` | `addressed` | `task` (D12).** A `plain` message has
  no `addresses`; an `addressed` message has at least one (a tag marks the intended reader and **nothing
  executes**); a `task` is the explicit "do this" kind and is the ONLY kind that may create work or be
  dispatched to a capability/group for EXECUTION. A `task` carries
  `task:{"id":"task_…","state":"open|claimed|running|done|failed","owner":null}`, and every state
  transition is a NEW record-version (`rev` increments) — never an in-place rewrite. Conflating `addressed`
  and `task` is how a tag becomes a remote command; the two are structurally distinguishable here, on the
  wire and in the transcript.
- **`attachments` carries asset REFERENCES, never bytes (D9, CR-CHAT-014; §3.10).** An attachment is
  `{"ref":"asset_…","kind":"asset"}`. The record never carries a blob, a base64 body, or a URL that bypasses
  the fetch check. An asset is authorized PER FETCH (CHAT-PERMISSIONS.md §6.9).

Projection: `messages(id PK, rev, session REFERENCES sessions(id) ON DELETE CASCADE, thread, author
jsonb, addresses jsonb, payload jsonb, guard jsonb, created_at, edited_at)` with a CHECK that at least one
of `session`/`addresses` is present (an unaddressed, sessionless message is not a message). Edited messages
are a NEW record-version of the same `id` (`edited_at` set, `rev` incremented) — never an in-place rewrite,
because the log is append-only and an edit that rewrites history is unauditable.

### 3.8 `grant`

```json
{"v":1,"kind":"grant","id":"grant_01J9Z8Q2K7","rev":2,"ts":"2026-10-03T18:35:44Z",
 "author":{"principal":"prin_01J9Z6V0Q7"},"namespace":"acme",
 "body":{"principal":"prin_01J9Z6V0Q7","subject":{"type":"agent","ref":"atlas"},
         "actions":["send","read"],"granted_by":"prin_01J9Z6V0Q7",
         "granted_at":"2026-10-03T18:10:00Z","expires_at":null,
         "revoked_at":null,"revoked_by":null,"note":"…"}}
```

Projection: `grants(id PK, rev, principal, subject_type, subject_ref, actions jsonb, granted_by,
granted_at, expires_at, revoked_at, revoked_by)` with a CHECK on `subject_type IN
('agent','group','capability','session','namespace')` and an index on `(principal, subject_type,
subject_ref) WHERE revoked_at IS NULL` — the exact lookup the effective-permission computation performs per
delivery. A revocation is a new version with `revoked_at` set (tombstone, CHAT-PERMISSIONS.md §6.6); the
line is never removed.

### 3.9 `audit`

```json
{"v":1,"kind":"audit","id":"aud_01J9Z9A1","rev":1,"ts":"2026-10-03T18:35:44Z",
 "author":{"principal":"prin_01J9Z6V0Q7"},"namespace":"acme",
 "body":{"event":"permission.grant","principal":"prin_01J9Z6V0Q7","as_agent":null,
         "details":{"grant":"grant_01J9Z8Q2K7"},"session":null}}
```

The event vocabulary is anchored on what Option B DRAWS in its `AUDIT TRAIL` table (columns
`Time | Principal | Event | Details`): `session.start`, `message.send`, `lease.renew`, `session.complete`,
`permission.grant`, `agent.degraded`, `queue.depth`, `heartbeat`. CHAT-PERMISSIONS.md §7.4 adds seven more
(`permission.revoke`, `delivery.allowed`, `delivery.denied`, `binding.create`, `scope.declared`,
`scope.reach.changed`, `principal.login` **NOT BUILT**), and this spec adds one: `storage.reconciled`
(§5.4). An `audit` record is **append-only with `rev` always 1**: an audit line is never corrected, only
superseded by a new line, because a mutable audit trail is not an audit trail.

Projection: `audit(id PK, rev, event, principal, as_agent, details jsonb, session, ts)` with an index on
`(namespace, ts)` for the trail view B draws, and on `(principal, ts)` for "what did this human do".

### 3.10 `asset`

An asset (a file, an image, "stuff") is METADATA plus a reference to bytes that live in object storage (S3
or any S3-compatible store — MinIO, R2, Ceph). **The record carries the REFERENCE, never the bytes** (G15,
D9). This is also what keeps a file from becoming a 32 MB inbox payload (the missing body cap was
DF-CRIER-292, since fixed).

```json
{"v":1,"kind":"asset","id":"asset_01J9ZB3K","rev":1,"ts":"2026-10-03T18:27:55Z",
 "author":{"principal":"prin_01J9Z6V0Q7","as_agent":"atlas"},"namespace":"acme",
 "body":{"store":"s3","bucket":"crier-acme","key":"assets/2026/10/01J9ZB3K/canary.png",
         "sha256":"…","bytes":20481,"media_type":"image/png","filename":"canary.png",
         "attached_to":{"kind":"message","ref":"msg_01J9Z7K4"},
         "created_at":"2026-10-03T18:27:55Z","created_by":"prin_01J9Z6V0Q7",
         "retention_seconds":86400,"deleted_at":null}}
```

| Field | Rule |
|---|---|
| `store` | the object-store driver: `s3` \| `minio` \| `r2` \| `ceph` (all S3-compatible). A closed set; an unknown driver refuses the import. |
| `bucket` / `key` | the OBJECT LOCATOR. The bucket and prefix are chosen by the per-namespace policy (§6.5) from the SERVER side, never taken verbatim from a request. **Nothing here is a fetchable URL** — a delivery returns a crier asset reference, and the bytes are reached only through the checked fetch (CHAT-PERMISSIONS.md §6.9). |
| `sha256` / `bytes` / `media_type` | the object's integrity and size, recorded AT UPLOAD. A reader can verify what it fetched; a size that contradicts `bytes` is a finding. |
| `attached_to` | the message (or session) the asset belongs to. `null` for an asset uploaded but not yet attached — an orphan, and §6.5's GC is what reaps it. |
| `retention_seconds` | the asset's lifetime, defaulting to the NAMESPACE's `retention_seconds` (NAMESPACES.md §4.4) and never longer than it. |
| `deleted_at` | a tombstone (§6.5). The record is never removed; a fetch of a tombstoned asset is a NAMED refusal (`410 ASSET_GONE`), not a `404`. |

**Both backends.** JSONL: one `asset` record-version per line, exactly as above, with the same idempotent
key `(kind, id, rev)` and keep-LAST rules (§4). PostgreSQL: `assets(id PK, rev, store, bucket, key,
sha256, bytes, media_type, filename, attached_kind, attached_ref, namespace, created_at, created_by,
retention_seconds, deleted_at)` with a UNIQUE `(store, bucket, key)` and an index on
`(attached_kind, attached_ref)`. **The bytes are in NEITHER backend** — they live in the object store.

---

## 4. Idempotent keys and keep-LAST

### 4.1 The idempotent key

> **A record-version's identity is `(kind, id, rev)`.**

Replay rules, in full:

| Replay | Result |
|---|---|
| `(kind, id, rev)` not seen before | append; project |
| `(kind, id, rev)` seen, `hash` identical (or absent and the canonical body bytes are identical) | **no-op** — nothing appended, nothing projected, the import reports it as `noop` |
| `(kind, id, rev)` seen, body DIFFERS | **`409 RECORD_REV_CONFLICT`**, naming the kind, the id, the rev and both hashes. Never overwritten. |
| a `rev` LOWER than the highest seen for `(kind, id)` | **keep-LAST**: it is accepted into the log (the log is append-only and history is history) but it does NOT win the projection; the import reports it as `stale` |
| a `rev` EQUAL to the highest seen, body identical | idempotent no-op |
| a `rev` GREATER than the highest seen | the new winner: appended and projected |

### 4.2 keep-LAST

> **For each `(kind, id)`, the record-version with the highest `rev` is the state of the object.**
> Ties are impossible by construction (§4.1 confines them to the identical-bytes no-op).

keep-LAST is enforced ONCE, in the projection, and it is the same rule the fleet's boards use. In SQL it
is a single upsert, and stating it as SQL is how a worker implements it without inventing a rule:

```sql
INSERT INTO messages (id, rev, …) VALUES ($1, $2, …)
ON CONFLICT (id) DO UPDATE SET rev = EXCLUDED.rev, …
WHERE EXCLUDED.rev > messages.rev;      -- the keep-LAST guard: a stale version never wins
```

**A stale version is not an error.** A bundle may legitimately contain an older version than the target
instance holds (someone shipped a session back from a branch); the correct outcome is "the log gains a
line, the view keeps its newer state, and the import says so". The alternative — refusing the stale line —
would make a bundle un-importable for the accidental reason that the receiving instance is ahead.

### 4.3 Why this makes re-import safe and repeatable

- Re-importing the SAME bundle twice is a no-op the second time (every `(kind, id, rev)` is a
  byte-identical replay).
- Importing a bundle into an instance that is AHEAD keeps the newer state and records the stale lines.
- Importing two bundles that both contain the same object resolves by `rev`, deterministically, with no
  clock dependence — the reason `rev` exists rather than relying on `ts`. (Two clocks disagree; two
  integers do not.)
- The `hash` makes "identical bytes" checkable without a deep compare, so a conflict is detected by
  comparing 64 hex characters rather than by re-serialising both sides.

---

## 5. The reconciler

### 5.1 What it compares

`storage-reconcile` (the fleet's board-reconciler shape) takes a JSONL log directory and a PostgreSQL DSN
and compares **per `(kind, id)`**, never per file:

```
for each (kind, id) present in the log:
    log_rev   = the highest rev for that key
    log_hash  = that version's hash
    view      = the projected row for (kind, id)
    if view is absent                        -> FINDING: missing_in_view
    elif view.rev < log_rev                  -> FINDING: stale_in_view
    elif project(view) != canonical(log line) -> FINDING: divergent
for each (kind, id) present in the view and absent from the log:
                                             -> FINDING: missing_in_log
```

### 5.2 The report

```json
{"ok":false,"checked":214,"log_files":9,
 "findings":{"missing_in_view":[{"kind":"message","id":"msg_…","log_rev":1}],
             "stale_in_view":[{"kind":"agent","id":"atlas","log_rev":3,"view_rev":2}],
             "divergent":[{"kind":"grant","id":"grant_…","log_hash":"sha256:aa…","view_hash":"sha256:bb…"}],
             "missing_in_log":[{"kind":"session","id":"3f9a7c2e-…","view_rev":4}]},
 "by_kind":{"message":{"checked":118,"findings":1},"agent":{"checked":3,"findings":1}},
 "checked_at":"2026-10-03T19:02:11Z","log_root":"…","dsn":"<redacted>"}
```

### 5.3 A MISMATCH IS A FINDING, NOT DRIFT

This is the whole point of having a reconciler rather than a sync job:

1. **The reconciler NEVER silently heals.** Its default mode is READ-ONLY: it reports, and **exits
   non-zero** when there is any finding, so it can be a CI step and a cron alert.
2. **Each finding is filed as a board row** (`DF-CRIER-*` for a code defect, `CR-STORAGE-*` for a data
   one), naming the kind, the id and both sides. A finding that is not filed is a finding that will be
   rediscovered.
3. **`missing_in_log` is the most serious class**, and is reported first: the view holds an object the log
   cannot justify, which means something wrote PostgreSQL without going through §2.3. That is either a
   bypassed write path (a code defect) or a manual edit (an operational one), and both must be named.
4. **`--repair` exists, is explicit, and is not the default.** It re-projects the log into the view for the
   keys whose LOG is ahead (`missing_in_view`, `stale_in_view`, `divergent`), writes ONE `audit` record
   (`storage.reconciled`) naming every key it changed, and **never deletes a view row**: a
   `missing_in_log` row is reported and left for a human, because deleting it would destroy the only
   evidence that the bypass happened.
5. **A divergent key is never auto-resolved by preference.** If the log's body and the view's body differ
   at the SAME `rev`, one of the two was produced by a path that did not honour §4 — that is a code
   investigation, not a merge. `--repair` re-projects the log (the log is the log of record) and the
   finding is filed regardless.

### 5.4 What the reconciler is NOT

- **It is not a sync daemon.** It runs on demand and on a schedule; it holds no locks, takes no lease, and
  has no "in sync now" state to be stale. Its only output is the report and the exit code.
- **It is not a second writer.** Its one write path is `--repair`'s re-projection (log → view), which is the
  same direction as §2.3 and therefore not a two-way write.
- **It is not the docs-claims gate.** `make docs-check` executes prose claims against a live server; this
  compares the log and the view. Different question, different tool.

**NOT BUILT:** there is no JSONL log, no projection, no reconciler and no `storage.reconciled` audit event.
The shipped server writes PostgreSQL directly (`internal/registry/postgres_store.go`) through the `Store`
interface and has no log layer at all.

---

## 6. The transport bundle

### 6.1 What is on disk

```
<bundle>/
  manifest.json          ← the bundle's identity, counts, per-file hashes, asset references
  principals.jsonl
  agents.jsonl
  bindings.jsonl
  scopes.jsonl
  sessions.jsonl
  threads.jsonl
  messages.jsonl
  grants.jsonl
  audit.jsonl
```

One file per `kind`, **the same lines that are in the log** — a bundle is a filtered copy of the log, not a
re-encoding of it. That is what makes "import" a replay (§4) rather than a translation, and it is why a
bundle round-trips.

```json
{
  "bundle_version": 1,
  "id": "bnd_01J9ZA7M",
  "created_at": "2026-10-03T19:10:00Z",
  "exported_by": {"principal": "prin_01J9Z6V0Q7", "as_agent": "atlas"},
  "source": {"build": "<the serving build's identity, internal/buildinfo>", "namespace": "acme"},
  "scope": {"kind": "session", "ref": "3f9a7c2e-6b1d-4e2a-9c7e-1d2f8a4be9c1"},
  "counts": {"principal": 2, "agent": 3, "binding": 2, "scope": 1,
             "session": 1, "thread": 4, "message": 118, "grant": 6, "audit": 37},
  "files": {"sessions.jsonl": {"lines": 1, "sha256": "…"},
            "messages.jsonl": {"lines": 118, "sha256": "…"},
            "agents.jsonl": {"lines": 3, "sha256": "…"}},
  "assets": [{"ref": "s3://…/canary.png", "sha256": "…", "media_type": "image/png", "bytes": 20481}]
}
```

Three rules about the manifest:

- **`counts` is checked at import.** A file whose line count disagrees with the manifest is a
  `400 BUNDLE_COUNT_MISMATCH` naming the file, the claimed count and the observed one — a bundle that lost
  lines in transit is refused rather than half-imported.
- **`files[].sha256` is checked per file**, and no file is read past its manifest entry: an extra file in
  the directory is refused (`400 BUNDLE_UNKNOWN_FILE`), because an extra file is an unreviewed input.
- **`assets[]` carries REFERENCES, never bytes** (G15: "S3-backed file/asset storage; the wire carries a
  reference, never the bytes"). And per D9, **an asset is authorized PER FETCH, not at upload** — so the
  manifest's asset list is not a permission, and importing a bundle never grants anyone access to an asset
  it names.

### 6.2 Secrets never travel

- An agent's `public_key` travels (it is public).
- A `token_ref`, `api_key_ref`, `auth_value_ref` or `email_ref` travels as the **`env:` REFERENCE**, never a
  resolved value — the shipped convention (NAMESPACES.md §4.1: "*never an inline secret*"; A2A-OPTION.md
  §5.6.2: "*crier references a secret by name and resolves it at send time*").
- An import that finds a resolved secret in a record body is refused with `400 BUNDLE_SECRET_INLINE`,
  naming the kind, the id and the field. A transport format that can carry a secret is a leak vector, and
  the refusal is the cheapest place to stop it.
- **Trust material never travels in a session bundle.** Peer keys, a peer's published key set, a federation
  secret (`CR_FED_TOKEN`) and any private key are NOT record bodies and NOT bundle members: a bundle is a
  conversation, not a trust store (CR-CHAT-024/025, `specs/CHAT-TRUST.md`). An import that finds one is
  refused `400 BUNDLE_TRUST_MATERIAL`, naming the file and the member — the sibling of
  `BUNDLE_SECRET_INLINE`, with the same reasoning.

### 6.3 The import path

Two entry points, ONE code path:

| Entry point | Shape |
|---|---|
| CLI | `crier-bundlectl import <dir> [--dry-run] [--namespace <n>] [--dsn <dsn>]` |
| HTTP | `POST /bundles/import` (multipart or a server-side path), same options as query parameters |

The shared contract:

1. **Dry-run first, always available.** `--dry-run` performs the whole comparison and prints the plan —
   `{"create":[…],"update":[…],"noop":[…],"stale":[…],"conflict":[…]}` — and writes NOTHING. A dry run that
   reports a conflict exits non-zero so it can gate a pipeline. (The fleet's rule: *dry-run before real*.)
2. **Conflict = refusal at the plan stage.** Any `(kind, id, rev)` that conflicts (§4.1) is reported in
   `conflict` and, without `--force`, the WHOLE import is refused (`409 RECORD_REV_CONFLICT` naming every
   conflicting key). A partial import that half-applies a bundle is worse than a refused one.
3. **A namespace wall applies to the transport too.** A bundle whose `scope`/`namespace` is not the target
   instance's is refused (`403 NAMESPACE_MISMATCH`, the shipped spelling) unless `--namespace` names the
   same realm; an import never widens a realm, and never maps an unknown realm back to the default
   (NAMESPACES.md §5's rule, applied to bundles).
4. **An import is an audit event.** Every applied import writes ONE `audit` record
   (`{"event":"bundle.import","details":{"bundle":"bnd_…","create":N,"update":N,"stale":N}}`), so the
   transcript of a realm can answer "when did this session arrive, and from where".
5. **`v` is refused, not skipped.** A line whose format `v` the importer does not know refuses the import
   (`400 BUNDLE_FORMAT_UNSUPPORTED`, naming the version) rather than being ignored — an unknown line is
   unknown state, and unknown state must not be silently dropped from a log of record.

**NOT BUILT:** no bundle exporter, no `crier-bundlectl`, no `POST /bundles/import`, no `GET
/sessions/{id}/bundle`, no manifest, no hash check, no asset references, no `bundle.import` audit event.

### 6.4 The bundle carries REFERENCES, not bytes (D9, CR-CHAT-014)

A bundle is transport for the LOG, and the log carries asset references — so a bundle that names an asset
re-imports WITHOUT the bytes. That is the intended property, not a limitation, and its consequences are
stated:

- **A re-import needs no object store.** The `asset` records and every `attachments` reference land in the
  receiving instance's view; the bytes stay where the exporting instance put them (its bucket/prefix, §6.5).
  The import reports the asset RECORDS it created; it does not report the assets as "complete".
- **A reader sees a NAMED state, never a silent placeholder.** When a client renders a message whose
  attachment is not fetchable (the reference points at a bucket this instance cannot reach, the object was
  GC'd, or the asset is tombstoned), the state is **`asset_unavailable`**: the transcript shows the
  attachment's NAME, size and `sha256` — all of which travel in the record — with an explicit "bytes not
  available here" affordance. A broken image with no explanation, or a fabricated empty file, is a lie about
  the record.
- **A failed fetch is a NAMED refusal**, not a `500`: `404 ASSET_NOT_FOUND` (no such record) /
  `410 ASSET_GONE` (tombstoned) / `502 ASSET_BACKEND_UNAVAILABLE` (the store is unreachable). The three are
  different questions and the caller can act on each. (Owed routes: `GET /assets/{id}` and
  `GET /assets/{id}/content` — NOT BUILT.)
- **A bundle never grants access.** The manifest's `assets[]` list (§6.1) is an INDEX for the importer, not
  a permission: importing a bundle does not let anyone fetch an asset their local grants do not already
  allow (CHAT-PERMISSIONS.md §6.9 — the check is per fetch).
- **An optional blob carrier, explicitly chosen.** A bundle MAY carry an `assets.blobs/` directory for a
  self-contained offline hand-off, but that is OPT-IN and the default is references-only; a bundle that
  carries blobs says so in the manifest (`assets[].included: true`) so a reference-only reader is never
  surprised.

### 6.5 Asset retention, GC, and the per-namespace bucket/prefix policy

- **Per-namespace bucket/prefix policy.** The bucket and key prefix an instance writes an asset to are
  chosen by the NAMESPACE, not the caller: `assets.bucket` and `assets.prefix` are per-namespace policy
  axes (the shape of the four in NAMESPACES.md §4 — declared once, inherited when unset). Unset means a
  single deployment-level bucket with a `namespace/<canonical>/` prefix, so an unconfigured deployment
  behaves as before. A namespace can never write into another namespace's prefix: the key is composed by
  the SERVER from the namespace's policy, and a caller-supplied key outside it is `400 INVALID_ASSET_KEY`.
- **Retention follows the namespace's `retention_seconds`.** An asset's default lifetime is the namespace's
  retention (§3.10), and an explicit `retention_seconds` may only SHORTEN it, never outlive it — the
  bounded-by-realm rule sessions already use (CHAT-SESSIONS.md §3.5). An asset whose message has been
  retention-reaped is tombstoned, not silently deleted (§3.10 `deleted_at`).
- **GC is a reconciliation, not a deletion sweep.** The reaper deletes OBJECT BYTES only for an asset whose
  `deleted_at` tombstone was already written and audited, and it writes ONE `storage.asset.gc` audit record
  naming every object it removed (a `kind:"audit"` line, §3.9). An asset whose RECORD still exists but whose
  bytes are missing is a FINDING (`asset_bytes_missing`, the §5.3 class), never a repair: the log is the log
  of record and the object store is a peer that can diverge.
- **Orphans are reaped by reference count.** An asset with `attached_to: null` past a grace period is
  tombstoned by the same audited operation; the grace period exists because upload and attach are two
  requests and a transient gap between them is normal.

**NOT BUILT:** no asset record, no `assets` table, no bucket/prefix policy axis, no reaper, no
`storage.asset.gc` audit event and no fetch refusal codes. Owed by **CR-CHAT-014**: `POST /assets` (upload),
`GET /assets/{id}` / `GET /assets/{id}/content` (fetch, per-fetch grant check), and the per-namespace
`assets.bucket` / `assets.prefix` config keys.

---

## 7. What is NOT built, and the open questions

### 7.1 NOT BUILT

1. **The JSONL log.** No `kind` file, no envelope, no `rev`, no `hash`, no `author`. The shipped server
   writes PostgreSQL directly.
2. **The projection.** No code projects a log line into a table; no keep-LAST upsert exists.
3. **The reconciler.** No comparison, no report, no `--repair`, no `storage.reconciled` audit event, and no
   `DF-CRIER-*`/`CR-STORAGE-*` finding pipeline for it.
4. **The bundle.** No manifest, exporter, importer, dry-run, hash check, or count check.
5. **Every new table.** `principals`, `bindings`, `scopes`, `sessions`, `threads`, `messages`, `grants`,
   `audit` — none exists. Migrations 001–008 ship `agents`, `inbox_entries` and `dead_letters` only.
6. **The two additive columns** `agents.class` / `agents.owner`.
7. **`GET /sessions/{id}/bundle`** and any storage-shaped route.
8. **Asset storage.** No S3 integration, no `asset` record, no `assets` table, no per-fetch authorization,
   no bucket/prefix policy and no GC (§3.10, §6.4, §6.5). Owed by CR-CHAT-014: `POST /assets` (upload),
   `GET /assets/{id}` / `GET /assets/{id}/content` (fetch, per-fetch grant check), and the per-namespace
   `assets.bucket` / `assets.prefix` config keys.
9. **A first-class peer in the CONFIG sense.** There is no `CR_STORAGE_LOG_ROOT` (or equivalent) and no
   `Store` implementation backed by the log. The `Store` interface (`internal/registry/store.go`) is
   implemented by `MemoryStore` and `PostgresStore`; a log-backed implementation does not exist, and the
   log-first write path of §2.3 is therefore not merely unimplemented — the current architecture has no
   seam for it. That is the largest piece of work this spec implies, and naming it is the point.
10. **The TASK message kind and its lifecycle.** §3.7's `kind:"task"` and its state transitions are a
    contract; no route creates or advances a task. Owed: `POST /tasks`, `POST /tasks/{id}/claim` /
    `POST /tasks/{id}/complete` (CHAT-PERMISSIONS.md §6.11).
11. **Federated authorship in the log.** §2.5's shadow-principal `author` block is a contract; nothing
    writes a remote origin. Owed by **CR-CHAT-023/026**: the federated delivery path plus
    `GET /fed/address?instance=<i>&agent=<a>` (the local shadow resolution, CHAT-ADDRESSING.md §1.5).
12. **Trust-material exclusion.** §6.2's `400 BUNDLE_TRUST_MATERIAL` refusal does not exist, and no peer-key
    store exists (CR-CHAT-024/025).

### 7.2 Open questions

1. **Where does the log live — one file per kind, one file per namespace, or one file per session?**
   A single `messages.jsonl` is simplest to append and worst to ship; per-session files are best to ship and
   require a fan-in on append. **PROPOSED-DEFAULT:** one append-only file per `(namespace, kind)` in the
   live log, and per-session files inside a BUNDLE (a bundle is the transport form, and it is free to
   re-shard what the live log keeps coarse). Owned by CR-CHAT-006.
2. **Is the log per-process or per-deployment?** Two relays writing one log would interleave lines;
   append-only survives that (each line is complete) but `rev` allocation would need to be a single
   writer's decision. **PROPOSED-DEFAULT:** the log is written by the process that owns the projection
   (one writer per log root), and a second process is refused at boot — the same "one process, one store"
   posture the shipped relay already assumes for its capability cursor ("the cursor lives in the serving
   process and is not persisted").
3. **Does a `message` edit create a new `rev` or a new `id`?** §3.7 says a new `rev` on the same `id`
   (append-only, never a rewrite). The alternative — a new id with a `supersedes` pointer — makes "the
   conversation as it stood at time T" reconstructable. **PROPOSED-DEFAULT:** new `rev`, because a
   transcript that can state what was said at a time must be a function of the log's order, which the
   version lines already give.
4. **Does the reconciler run against a git checkout or a live log root?** A checkout is reproducible and
   lags; a live root is current and mutating during the comparison. **PROPOSED-DEFAULT:** against a
   checkout (the fleet's board reconciler reads the git-tracked board), and the report names the commit it
   compared at, so a finding is reproducible.
5. **What is the retention story for the log?** PostgreSQL has per-namespace `retention_seconds`; the log
   has no reaper, and a log of record should not silently lose lines. **PROPOSED-DEFAULT:** the log is never
   trimmed by retention — retention governs DELIVERY TTL and the transcript's visibility, while the log is
   compacted only by an explicit, audited `storage.compact` operation that writes a compaction record.
   Unresolved and stated as such.
6. **Does the `audit` kind belong in the same log as domain objects?** It is the fastest-growing kind and
   the least likely to be shipped. **PROPOSED-DEFAULT:** same log, same envelope, separate FILE per bundle
   (which the §6.1 layout already does) so a bundle can omit audit while the live log keeps it.
7. **Is the JSONL the WIRE format for the UI too?** GOALS.md's D6 recommendation says *"it keeps the
   protocol honest: the JSONL *is* the wire format we already speak"*. The shipped wire is JSON over HTTP;
   JSONL is one JSON object per line. **PROPOSED-DEFAULT:** the bundle is JSONL; the API stays
   JSON-over-HTTP (no NDJSON streaming endpoint is added by this spec).
8. **Where does an asset's bytes-vs-reference join live for a bundle that carries blobs?** §6.4 makes blobs
   opt-in, but their manifest entry, hash and placement are targets. **PROPOSED-DEFAULT:** `assets.blobs/`
   mirrors the content-addressed key path, the manifest lists every blob with its `sha256`, and an importer
   recomputes the hash before accepting a blob — a blob whose hash disagrees is refused rather than stored.
9. **Does GC delete bytes the moment a message is retention-reaped, or after a grace period?** The object
   store and the record store expire on independent clocks. **PROPOSED-DEFAULT:** the tombstone is written
   at reaping (audited) and the bytes are deleted by the GC pass on its own schedule, so the two never
   disagree silently; a fetch in the interregnum is `410 ASSET_GONE`, not `404` (§3.10).

---

## 8. Status line

`DRAFT v2 · 2026-10-03 · CR-CHAT-006 + CR-CHAT-014`

REVISION 2026-10-03 (v2): §2.5 (a federated delivery in the log), §3.6 (a `thread` exists only for a
deliberate branch — the corrected D11 rule), §3.7 (the message `kind` and `attachments`), §3.10 (the `asset`
record on both backends), §6.2 (trust material never travels), §6.4 (references, not bytes) and §6.5
(retention, GC, bucket/prefix policy); §7.1's NOT BUILT list extended, each new affordance naming its owed
endpoint.

Statements describing behaviour that does not exist are marked **NOT BUILT** in place (§2.4 `inbox_entries`,
§2.5, §3.6, §3.7, §3.10, §5.4, §6.3, §6.4, §6.5, §7.1). The JSONL log, the projection, the reconciler, the
bundle, asset storage and every new table are unbuilt; the PostgreSQL persistence of `agents` /
`inbox_entries` / `dead_letters` (CR-FEAT-034, `specs/ci-003b-postgresql-persistence.md`) is shipped and is
extended, never replaced. Nothing here may be added to `docs/claims.yaml` until the log layer exists,
because claims execute against a live server.
