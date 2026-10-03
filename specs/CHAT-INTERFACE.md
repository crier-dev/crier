# CHAT-INTERFACE.md — the crier comms interface (spec of record)

Status: **DRAFT v1** · 2026-10-03 · Owner: Bane · Tickets: **CR-CHAT-001** (this document — the spec of
record for the series) and **CR-CHAT-009** (the web-client MVP this document scopes)
Source material: the four approved UI options generated 2026-10-03 —
`A` channels + threads, `B` operator console, `C` triage inbox, `D` chat + capability map
(`~/.hermes/cache/images/openai_codex_gpt-image-2-medium_20261003_1243{19,30,31,32}_*.png`).
Per **CR-CHAT-011** those images are **feel / layout references only — their lettering is placeholder**.
An element below is cited by the label the image actually DRAWS where a label is drawn; where an image
renders its text as unreadable placeholder boxes (option `C` does this for most strings), this document
says so instead of inventing a label.
Companions in this series (different owners, not written here): `specs/CHAT-SESSIONS.md` (the
session + threading half of this document), `specs/CHAT-PERMISSIONS.md` (CR-CHAT-003),
`specs/CHAT-ADDRESSING.md` (CR-CHAT-004), `specs/CHAT-STORAGE.md` (CR-CHAT-006).
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
4. **The two invariants** (§5) that no later row in this series may break.
5. **The decisions of record** for the decisions that belong to this document (§1.3).

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
| **D1** | The session is an **aggregate over durable inbox deliveries**, not a new bus: a message to a session fans out one `POST /agents/{id}/inbox` per participant, reusing guard / webhook / durable write / lease / ack / TTL / dead-letter / federation fallback. | `CHAT-SESSIONS.md` §3 (owner) | *A session-scoped queue and its own delivery path.* Rejected: it would be a second delivery path with its own truth — the exact thing §5.1 forbids — and would lose offline semantics, TTL expiry receipts and the federation fallback that the inbox already has. |
| **D2** | The human is a **`Principal`**, distinct from an Agent, and speaks **AS a bound Agent**. | `CHAT-PERMISSIONS.md` (CR-CHAT-003/007) — not this document | *A human holding an ed25519 key.* Rejected: hostile UX (the bus's own contract says keys are the agent's), and no per-human audit. |
| **D3** | A **`Grant`** binds a principal to an agent / group / session / capability, and delivery is refused without one (a named refusal, not a silent drop). | `CHAT-PERMISSIONS.md` (CR-CHAT-003) — not this document | *Trust-by-reach (today's posture: anyone who can reach the port can deliver to anyone).* Rejected: fine for a fleet, hostile for multi-team humans. |
| **D4** | Addressing is a tag grammar (`@agent`, `@team:x`, `@cap:y`, `@ns/*`, `#session`) resolved server-side; the capability tag resolves to **one holder**, never a fan-out. | `CHAT-ADDRESSING.md` (CR-CHAT-004) — not this document | *A capability tag that fans out to every holder.* Rejected: it changes delivered semantics, lease/ack consequences and sender expectations; `POST /capabilities/{capability}/inbox` is a selector, and §4 below pins the UI to that. |
| **D5** | **Where it ships:** the chat surface ships as a **client of the stock server** — a browser client (option: served by crier itself, under the existing server binary/landing surface) that calls only documented public REST/WS. | this document, §5.1 + §7 | *A separate service with its own backend (a bespoke BFF).* Rejected: a second truth, and the MVP's own acceptance (CR-CHAT-009) requires a full round-trip against a stock crier with no private endpoints. This document does not decide *static files from the existing binary* vs *a separately hosted client of the same API* — both satisfy §5.1; §6 carries it. |
| **D6** | **Dual backend:** JSONL is the **ordered append log and the transport form**; PostgreSQL is the **query view built from it**. No two-way live writes. | `CHAT-STORAGE.md` (CR-CHAT-006) — referenced by `CHAT-SESSIONS.md` §5 | *Two independent stores with live two-way writes.* Rejected: distributed-transaction territory; a conflict loses either way, and a mismatch would be silent drift instead of a finding. |

---

## 2. The objects

Seven terms, each one paragraph. `Status` names what exists on the bus today.

**Principal** — a HUMAN user, distinct from an agent. A Principal can **speak AS a bound agent**, so the
bus only ever sees agents while permissions and audit stay per-human. *Web / storage status:* **NOT
BUILT.** Today the bus has one deployment-level credential (`CR_AUTH_TOKEN`, Bearer) and per-namespace
posture (`shared` | `token`, with `X-Crier-Namespace-Token`); the registry stores agents only. Owed:
CR-CHAT-003 (principal, roles, grants) and CR-CHAT-007 (identity, login, audit).

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

**Grant** — an **ACL entry** binding a principal to an agent / group / session / capability. *Status:*
**NOT BUILT** (CR-CHAT-003). Everything the four images draw as a *permission* indicator (role badges, a
permission matrix, a permission chip) is therefore rendered from a source that does not exist yet.

**Namespace** — the **realm / tenant wall**, already shipped: auth posture (`shared` | `token` with
`token_ref`), rate limits (`rate_limit_per_minute`), guard settings (`guard_enabled`, `guard_policy`) and
retention (`retention_seconds`) per realm, plus migration 008's NULL-means-default storage. *Status:*
**SHIPPED** (`specs/NAMESPACES.md`, CR-FEAT-029). It is the outer wall in this series: a session lives in
exactly one realm, and no surface here may bridge two.

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

## 4. The mapping table

Every UI element, the crier primitive or endpoint that serves it, and its status. `SHIPPED` = exists on
the bus today. `PARTIAL` = the primitive exists but not in the shape the UI needs. `NOT BUILT` = nothing
serves it; **the owed thing is named.** Route names below are the shipped surface (see
`docs/openapi.yaml`); no route in this table is invented.

| # | UI element (and where it is drawn) | Served by | Status |
|---|---|---|---|
| 1 | Z2 index of **agents** (`B`; the roster header "AGENTS (12)" + "+ New") | `GET /agents` (list), `GET /agents?capability=` (filter), `POST /agents` (register), `DELETE /agents/{id}` (unregister) | **SHIPPED** |
| 2 | Index row **capability chip** (`B`: routing / summarize / research / vision / memory / planning / data / execution / image / guardrails / tools / analysis) | the agent's `capabilities[]` on its registry row | **SHIPPED** |
| 3 | Agent **presence dot + status word** (`B` online / idle / degraded; `A`, `C`, `D` dots only) | derived status `online` \| `stale` \| `offline` from `last_seen` vs `CR_PRESENCE_STALE_AFTER_S` (CR-FEAT-024), read via `GET /agents`; the connection table via `GET /mesh/peers`; the window is published in `GET /status.presence_stale_after_s` | **PARTIAL** — the real enum has three words, and `idle` / `degraded` are not two of them. `degraded` has **no server signal at all** (NOT BUILT; owe either a signal or dropping the word). |
| 4 | **Queue depth** figure + trend glyph (`B`) | `GET /status.queue_depth` | **PARTIAL** — the number is SHIPPED; the trend/sparkline is **NOT BUILT** (no time series; `/metrics` counters exist, `/status` has no history). |
| 5 | **Health** word + dot + chevron (`B` "System Healthy"; `A`'s status card "All systems operational") | `GET /health`, `GET /status` (guard_enabled, registry_backend, federation_enabled, …) | **PARTIAL** — a boolean-ish up/down is SHIPPED; a graded severity, and a chevron that reveals detail, is **NOT BUILT**. |
| 6 | **"6/6 agents online"** aggregate (`A`) | none — needs `GET /agents` + the presence derivation, counted client-side | **NOT BUILT** as a surface. Owed: an explicit aggregate (or a documented client rule) so the number cannot be computed two ways. |
| 7 | Z1 **breadcrumb** namespace → … → session (`B`) | `GET /namespaces` for the realm half | **PARTIAL** — namespaces SHIPPED; the session half is NOT BUILT (row 12). |
| 8 | **Session list row** (title, preview, relative time) (`A`) | none | **NOT BUILT**. Owed: `GET /sessions` (CR-CHAT-002). |
| 9 | **Conversation card** + filter chips with counts (`C`) | none | **NOT BUILT**. Owed: `GET /sessions` with state/count filters — a *view* over the same objects, not a new object. |
| 10 | **Floating compose** action (`C`), compose icons (`A`), `+ New` (`B`) | `POST /agents` exists for agents | **NOT BUILT** for sessions/rooms. Owed: `POST /sessions` (CR-CHAT-002). |
| 11 | **Session header** — title, id, start timestamp, audience count, view selector, overflow | none | **NOT BUILT**. Owed: the session object (CR-CHAT-002). Today `session_id` is a wire tag only (deliver body; webhook `crier.session_id`; federation hold). |
| 12 | **State badge** `ACTIVE` (`B`), `Live` (`D`), the "Session … marked complete" event (`B`) | none | **NOT BUILT**. Owed: a session lifecycle (CR-CHAT-002). |
| 13 | **Privacy indicator** — the padlock beside `A`'s title | none | **NOT BUILT**. Owed: a session visibility field (CR-CHAT-002) and the permission that guards it (CR-CHAT-003). |
| 14 | **Transcript message** — author, body, timestamp | `GET /agents/{id}/inbox` returns `InboxEntry` (id, agent_id, sender, payload, created_at, expires_at, priority, namespace, guard); the deliver body carries `payload`, `sender`, `session_id`, `thread_id`, `request_id`, `kind` | **PARTIAL** — a delivered message is durable and readable **per agent**, and its body is `payload` (the existing convention renders `payload.text`; WEBHOOK-DELIVERY's schema templates). There is **no ordered, cross-agent transcript read** — NOT BUILT (owed: `GET /sessions/{id}/messages`, CR-CHAT-002). |
| 15 | **Thread reply-count pill + rail/elbows + indentation** (`A`, `D`) | `thread_id` on `POST /agents/{id}/inbox` (CR-FEAT-004) and as a guard policy key (`thread:<id>`) | **PARTIAL** — the wire field SHIPPED; the stored field is **NOT BUILT** (`InboxEntry` has no `thread_id`), so a thread is not reconstructable from stored messages. Owed: thread_id on the record + a transcript read (CR-CHAT-005). |
| 16 | **Stacked-layers count** per card (`C`, values 3/1/7/2/4/9) | none — meaning unlabelled in the image | **NOT BUILT**. Unresolved by the images: whether it counts replies, files, versions or participants (§6). |
| 17 | **Message meta line** — lease word, ack tick (`B`) | `GET /agents/{id}/inbox` returns `lease_id`, `leased_count`, `queue_depth`; `POST /agents/{id}/inbox/ack` releases a lease | **SHIPPED** (the state is real; it is simply not attached to a session-scoped transcript yet). |
| 18 | **Per-message latency** (`B` "⚡ 86ms") | none per message; `/metrics` counts `deliveries_total` and `guard_decisions_total` in aggregate | **NOT BUILT**. Owed: a per-delivery timing field on the entry, or the element is dropped (§6). |
| 19 | **SYSTEM EVENT** rows — "Lease renewed … (+5m)" (`B`) | leases are taken per retrieve with `lease` / `lease_seconds` and released by ack | **NOT BUILT** — there is no lease *renewal* operation. Owed: either a renewal, or the UI states lease-taken / lease-expired only. |
| 20 | **SYSTEM EVENT** rows — expiry and unacknowledged loss | `GET /agents/{id}/inbox/dead-letters` (reason `ttl_expired_unacked`, 7-day retention) plus the `MESSAGE_EXPIRED` receipt written into the **sender's** inbox | **SHIPPED** — this is the real "something terminal happened to a message" surface, and it is a **dead letter, not a moderation flag**. |
| 21 | **Audit trail** panel (`B`: Time / Principal / Event / Details) | `GET /delivery-log` + `GET /delivery-log/verify` — the OPT-IN, hash-chained ed25519 delivery log (`CR_DETECT_ENABLED`, default false), scoped to agents and delivery verdicts | **PARTIAL / NOT BUILT** — a delivery log exists but is opt-in, agent-scoped and about deliveries, not about sessions and permissions. A session-and-principal audit (`session.start`, `permission.grant`, `agent.degraded`…) is **NOT BUILT** (owed: CR-CHAT-007). |
| 22 | **Permission badge / role pill** (`A` ADMIN / MEMBER / VIEWER; `C` role pill; `D` `admin` / `invoke` / `read`) | none. Today: one deployment Bearer (`CR_AUTH_TOKEN`), per-namespace posture (`shared` \| `token` + `X-Crier-Namespace-Token`), per-agent ed25519 signatures (`CR_REQUIRE_AGENT_SIG`) | **NOT BUILT** — none of the shipped auth surfaces is a *per-principal role*. Owed: CR-CHAT-003. |
| 23 | **Permission matrix** (`B`) — columns send / read / invite / admin / mint, tick / dash cells | none | **NOT BUILT**. Owed: the grant model + a read surface (CR-CHAT-003). The column names are the image's; their semantics are the permission spec's. |
| 24 | **Participants list** + count + "Add participants" search (`A`; `D`'s audience counts) | `GET /agents` (the roster) | **PARTIAL** — a roster exists; *membership of a conversation* is **NOT BUILT** (owed: `GET`/`POST /sessions/{id}/participants`, CR-CHAT-002). |
| 25 | **Group rows** with counts (`A` Engineering 4 / Operations 3 / Research 2 / Design 2 / Security 2; `D` "Capability Group • 3 agents") | the capability index: `GET /agents?capability=` (exact match on advertised capabilities) | **PARTIAL** — capability *membership* is real; a **group** object with a name and a count is **NOT BUILT** (owed: the group/team concept, CR-CHAT-003 / CR-CHAT-010). `A`'s counts are placeholder. |
| 26 | **Mention popover** — agent-group rows with counts (`A`) | `GET /agents?capability=` (or `GET /agents` for the full roster) | **PARTIAL** — resolvable today only if a "group" is in fact a capability name. The parser that turns a typed token into a resolution is CR-CHAT-004. |
| 27 | **Mention popover** — typed rows: Team / Capability Group / Agent, with presence dots (`D`) | agent rows SHIPPED (`GET /agents`); capability rows SHIPPED via the capability index; **Team** rows | **PARTIAL** — a Team is NOT BUILT (row 25). |
| 28 | **Capability tag delivery** — the map's halo and "One tag. Many agents." (`D`), and any `@cap:y` send | `POST /capabilities/{capability}/inbox` — resolves to **ONE** holder, round-robin over live holders (pool = the live holders when any exist, else all holders), `404 NO_CAPABLE_AGENT` when nobody advertises it, same guard / lease / ack / TTL / federation path as a by-id delivery | **SHIPPED** — and it is a **selector, not a fan-out** (§3.7 item 4). The UI must present it that way. |
| 29 | **Capability map** — nodes, solid / dashed links, halo geometry, legend, callout (`D`) | none (the index is data; the topology is not computed) | **NOT BUILT**. Owed: a topology read (which holders exist, which links are live) — arguably a client-side projection of `GET /agents` + `GET /mesh/peers`, which is a decision for CR-CHAT-009/010. |
| 30 | **Guard flag** — `A`'s amber shield + `Flagged` pill, `C`'s amber shield `!` | `guard.Meta` on the entry: `decision` (allow / block / sanitize), `risk_level`, `reason`, `matched_patterns`, `policy`, `provider`, `model`, `errored`, `quarantined`, `sanitized`, `quarantined_payload` — surfaced on `GET /agents/{id}/inbox`, on the deliver response, and as the uniform notice on a block | **SHIPPED** — this is the one "badge on a message" that is real, and CR-CHAT-009 requires it to be **visible, not silent**. |
| 31 | **Composer** text input + send | the deliver call: `POST /agents/{id}/inbox` with `payload` (+ `sender`, `request_id`, `delivery_mode`, `timeout_ms`, `priority`, `ttl_seconds`, `idempotency_key`) | **SHIPPED** for a single named target. Composing **into a session** (fan-out to N participants) is **NOT BUILT** (owed: CR-CHAT-002; CHAT-SESSIONS.md §3). |
| 32 | **Target chip** (`B` "Agent Bus") and the popover's resolved handle | the URL path's agent id, or the capability route (row 28) | **SHIPPED** as two routes; the unified "what am I sending to" control is a client composition. |
| 33 | **Keyboard hints** (`A` `⌘ K`, "Shift + Enter for newline") | none — client-local | **NOT BUILT** by design; no server meaning. |
| 34 | **Attachment / embedded block** (`A` `txt` and `bash` blocks; `D`'s cards) | `payload` is opaque (`json.RawMessage` on the deliver body / `[]byte` on the entry); the existing convention reads `payload.text` | **SHIPPED** as an opaque payload — crier defines **no** message-content schema, so a code block is the sender's shape rendered by the client. Attachment *upload/storage* of files is **NOT BUILT**. |
| 35 | **Tag chips** `# planning` … (`A`) | none — `InboxEntry` has no tag or label field | **NOT BUILT**. Owed: a decision whether a chip is a **relay topic** (fan-out, no history), a session label, or an address form — CR-CHAT-004 / CR-CHAT-005. |
| 36 | **Message overflow `•••`** (`D`), copy glyphs (`A`), paperclip, emoji, split-send chevron | none — client-local affordances | **NOT BUILT** by design; any action behind them (edit, delete, quote, react) is out of scope (§6). |
| 37 | **Add-participant** affordance (`A`) | none | **NOT BUILT** (owed: CR-CHAT-002 membership). |
| 38 | **Namespace / realm wall** behind every element above | `GET /namespaces`; realm decided at registration, delivery, transfer, relay publish + subscribe; per-realm auth posture, rate limit, guard settings, retention | **SHIPPED** (CR-FEAT-029) — and every session-scoped surface owed above must be realm-scoped on arrival. |

---

## 5. The two invariants

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
9. **`D`'s "Thread view" selector** — what the alternative view is (flat? by agent?) is not drawn.
10. **The legible-token requirement** — §4 rows 26/27 require every tag a popover offers to be
    resolvable. `A`'s `@dev-agents` is not resolvable today unless it is a capability name; whether the
    grammar invents group addresses or maps them onto capabilities is CR-CHAT-004's decision.
11. **Empty state** — undrawn in all four (see §3.4). Owed as a design task, most cheaply in CR-CHAT-011.
12. **Where the client is served from** (static files from the existing binary vs a separately hosted
    client of the same public API) — D5 fixes *that it is a client*; the packaging is undecided.

---

## 7. Status

**Status: DRAFT v1 · 2026-10-03 · CR-CHAT-001 (+ CR-CHAT-009 for the MVP).**

This document is a **specification of an interface that does not exist yet**. Concretely, **not built**:

- **No session object and no session routes** — no `GET /sessions`, no `POST /sessions`, no transcript
  read, no membership. `session_id` exists as a wire tag on the deliver body and the webhook envelope, as
  a guard policy key, and in the federation hold record — nothing more.
- **No stored `thread_id`** — it is a wire tag and a guard policy key; `InboxEntry` does not carry it, so
  no thread is reconstructable from stored messages.
- **No Principal, no Grant, no role, no permission matrix** — permissions and identity are CR-CHAT-003 and
  CR-CHAT-007; the shipped auth surfaces (deployment Bearer, namespace posture, per-agent ed25519) are not
  per-principal roles.
- **No address grammar or parser** — CR-CHAT-004. The agent and capability halves resolve today; the
  `@team:` / `@ns/*` / `#session` halves do not.
- **No group/team objects** — capability *membership* is real; a named group with a count is not.
- **No audit trail scoped to sessions and principals** — the opt-in delivery log is agent- and
  delivery-scoped and off by default.
- **No queue-depth history / trend, no graded health severity, no fleet presence aggregate, no per-message
  latency, no lease renewal, no session lifecycle state, no session visibility/privacy field, no file
  attachments, no tag/label field on a message.**
- **No UI at all** — the four images are layout references with placeholder lettering (CR-CHAT-011); no
  HTML client exists in this repository.

What **is** shipped and reusable, and must not be reinvented: the relay (publish / subscribe / topics),
the durable inbox with lease / ack / TTL and dead letters, capability delivery
(`POST /capabilities/{capability}/inbox`, selector semantics), the agent registry with derived presence,
per-namespace policy (auth posture, rate limits, guard settings, retention), the guard verdict metadata,
and `GET /status` / `GET /health`.

Not claimed, and not to be claimed in `docs/claims.yaml`, until they exist: session reads and writes,
membership, transcripts, threading, principals, grants, roles, the address grammar, and the web client.
