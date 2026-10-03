# CHAT-ADDRESSING.md — the tag grammar, its resolution and its refusals

Status: **DRAFT v1** · 2026-10-03 · **CR-CHAT-004**
Source material: the four approved UI options produced 2026-10-03 (read in full with a vision pass; every
label quoted below is transcribed from an image, and nothing is quoted that an image does not carry),
`~/crier-interface/GOALS.md` (G4, G14; decisions D4, D8), and the shipped code at the worktree HEAD.

This document is the normative design authority for how a human or an agent ADDRESSES something in the
crier comms interface. Where it and the shipped code disagree, the **code wins** — file the drift as a
board row (`DF-CRIER-*`).

Cross-references: authorization for every address is `specs/CHAT-PERMISSIONS.md` §6 (the action × subject
matrix and `403 DELIVERY_FORBIDDEN`); the record shapes are `specs/CHAT-STORAGE.md`; sessions and threads
are the sibling's `specs/CHAT-SESSIONS.md`.

**Glossary (normative; the exact terms every CHAT spec uses).** A **Principal** is a HUMAN user, distinct
from an agent, and can speak AS a bound agent. An **Agent** is a registered crier agent, with a CLASS
(`personal` | `service`), an OWNER and capabilities. A **Session** is a durable conversation: participants
(principals + agents), membership, an ordered transcript. A **Thread** is a reply subtree inside a session.
An **Address** is a tag: `@agent`, `@team:x`, `@cap:y`, `@ns/*`, `#session`. A **Grant** is an ACL entry
binding a principal to an agent / group / session / capability. A **Namespace** is the realm/tenant wall
(already shipped: auth posture, rate limits, retention).

**The one rule this document exists to enforce:** *an unknown or unauthorised address is a NAMED error,
never a silent drop.* A tag that cannot be resolved to exactly one deliverable thing must fail loudly,
because a tag that resolves to the wrong thing delivers a message to the wrong agent — and in this system
those agents act.

---

## 1. The grammar

### 1.1 EBNF

```ebnf
(* ---------- a single address ---------- *)
address        = agent_ref | team_ref | cap_ref | ns_ref | session_ref ;
agent_ref      = "@" , ident ;
team_ref       = "@team:" , group_ident ;
cap_ref        = "@cap:" , capability_ident ;
ns_ref         = "@ns/" , ( "*" | ns_name , ( "/" , ( "*" | ident ) ) ) ;
session_ref    = "#" , session_ident ;

(* ---------- an address LIST (a "to" line) ---------- *)
address_list   = address , { ( "," | ";" | whitespace ) , address } ;
                                  (* at most maxAddressesPerWrite = 64 *)

(* ---------- an ADDRESSED WRITE: a room plus recipients ---------- *)
addressed_write= session_ref , [ address_list ]          (* append into a session     *)
                | address_list                            (* deliver to recipients only *)
                ;

(* ---------- tokens ---------- *)
ident          = ident_start , { ident_char } ;
ident_start    = ALPHA | DIGIT | "_" ;
ident_char     = ALPHA | DIGIT | "_" | "-" | "." ;
group_ident    = ident ;            (* same charset as ident *)
capability_ident = ident ;
session_ident  = ident ;
ns_name        = ident ;            (* and the server enforces ^[a-z0-9][a-z0-9_-]{0,63}$ *)

ALPHA          = "A".."Z" | "a".."z" ;
DIGIT          = "0".."9" ;
```

| Bound | Value | Why |
|---|---|---|
| `maxAddressesPerWrite` | **64** | Bounds one write's fan-out. A list longer than this is `400 INVALID_ADDRESS` naming the limit and the count received — never truncated, because a silently truncated list delivers to the WRONG subset. |
| `maxAddressLength` | **256** characters (including the sigil) | Matches the idempotency key's own 128-character discipline in spirit: an address is an identifier, not a payload. |
| `maxResolvedTargets` | **256** | An `@ns/…/*` wildcard may resolve to more entities than a literal list; beyond this the expansion is refused (`400 INVALID_ADDRESS`, `"reason":"TOO_MANY_TARGETS"`) rather than truncated. |

### 1.2 The five forms, and what each means

| Form | Draws as | Meaning | Ships? |
|---|---|---|---|
| `@atlas` | `@Kappa`, `@Echo` (D) with the type label `Agent · …` | ONE registered agent id | registry ships; the tag does not |
| `@team:infra` | `@Infra-Team` (D), type label `Team · 6 members · Ops & Platform`; `@dev-agents` (A), `Engineering agents` `4` | a NAMED, CURATED group (D8's "named group", curated and editable) | **NOT BUILT** |
| `@cap:solver` | `@Data-Capabilities` (D), type label `Capability Group · 3 agents · Data & Analytics`; `@ops-agents` (A) | a dynamic CAPABILITY pool — the shipped capability index | **the endpoint ships**; the tag does not |
| `@ns/*`, `@ns/acme/*`, `@ns/acme/atlas` | the breadcrumb `root / prod / agents / message-bus / sessions / 3f9a7c2e` (B) is the only drawn namespace-shaped string | every addressable thing in a namespace / in one named namespace / one agent in one namespace | **NOT BUILT** |
| `#build-plan` | `SESSION #A7F3` / `Session #A7F3 · Live topology` (D) | the SESSION id (a room), not a recipient | **NOT BUILT** |

`@ns/*` is the only wildcard form. It expands inside the realm wall and **never widens it**: `@ns/*`
expands to the sender's own namespace, and `@ns/acme/*` is a cross-realm expansion that a sender outside
`acme` is refused for with the shipped `403 NAMESPACE_MISMATCH`. Each expansion is authorized
individually (CHAT-PERMISSIONS.md §6.4 rule 1) — a wildcard is N authorization checks, never one.

### 1.3 Combinations

- **A session plus recipients** — `#build-plan @atlas @cap:research`: `#build-plan` is the ROOM the message
  is appended to; the addresses are the RECIPIENTS (who is notified and whose inboxes receive a copy).
  `#session` alone means "the session's current participant set"; `@address` alone means "these
  recipients, no room" (the shipped inbox/webhook delivery path).
- **A list of mixed kinds** — `@atlas, @team:infra, @cap:deploy`: legal, and each element is resolved and
  authorized on its own. The write's outcome names the resolved set (§2.4), so a client never has to
  re-derive what happened from prose.
- **A bare `#` with no token, or `@` with no token** — `400 INVALID_ADDRESS`. There is no "current
  session" or "everyone" default: an address that names nothing is not an address.
- **Two `#` in one write** — `400 INVALID_ADDRESS`. One write has at most one room; a message in two rooms
  is two messages, and pretending otherwise would make the transcript's ownership ambiguous.

---

## 2. Resolution, precedence, and what happens when a tag matches several things

### 2.1 The precedence rule (reserved prefixes first, then bare tokens)

A tag is classified by its **reserved prefix**, and only then by lookup:

1. `@team:` → the named-group registry. `@cap:` → the capability index. `@ns/` → the namespace expansion.
   `#` → the session registry. **A reserved prefix is never reinterpretable**: `@cap:atlas` addresses the
   capability named `atlas`, and even if an agent called `atlas` exists it is not the target. This is what
   makes a typo a `404` instead of a delivery to the wrong kind of thing.
2. A bare `@token` (no reserved prefix) resolves in this fixed order, first match wins:

   ```
   @token  →  (1) exact agent id (the registry's `agents.id`, which is one flat primary key — the
                  namespace wall is applied AFTER resolution, as §4's `403 NAMESPACE_MISMATCH`)
              (2) named group whose name == token            (when groups ship)
              (3) capability whose name == token             (the shipped index)
              (4) otherwise: 404 UNKNOWN_ADDRESS
   ```

   The drawn options require this: **A** draws `@dev-agents` with the sub-label `Engineering agents` and
   the count `4`, and **D** draws `@Infra-Team` labelled `Team · 6 members · Ops & Platform`, i.e. both
   mocks write GROUPS with no reserved prefix, and D additionally draws a bare `@Kappa` labelled
   `Agent · Data Processing` next to a bare `@Data-Capabilities` labelled `Capability Group · 3 agents`.
   A grammar that rejected bare group tags would refuse three of the four entity rows the approved options
   actually draw.

3. **Ambiguity is a refusal, not a guess.** If a bare token matches more than one KIND (an agent named
   `research` AND a capability named `research`), the write is refused:

   ```
   409 {"error":"AMBIGUOUS_ADDRESS",
        "address":"@research",
        "matches":[{"type":"agent","ref":"research"},
                   {"type":"capability","ref":"research"}],
        "detail":"address matches more than one kind; use the prefixed form (@research or @cap:research)"}
   ```

   **DECISION (D4'):** ambiguity is refused rather than resolved by kind order.
   **Rejected alternative:** resolve by the order in §2.1.2 and report the choice.
   **Why rejected:** a silent choice between an *agent* and a *pool* is a delivery to a different set of
   recipients with different blast radius, and the response body of a message send is not where a human
   looks for a surprise. The prefixed forms exist to disambiguate; the refusal teaches them.

### 2.2 The canonical (wire) form is the PREFIXED form

The popover is where a human picks a TYPE — D draws exactly that, with a type column
(`Agent · Data Processing`, `Team · 6 members · Ops & Platform`, `Capability Group · 3 agents · Data & Analytics`).
The picker therefore commits the prefixed spelling on the wire (`@cap:data-capabilities`), and the bare
form is a convenience the parser accepts. A client SHOULD emit the prefixed form; the server MUST accept
both and MUST report what it resolved:

```json
{"resolved":[{"tag":"@cap:data-capabilities","type":"capability","ref":"data-capabilities"},
             {"tag":"@atlas","type":"agent","ref":"atlas"}]}
```

### 2.3 Precedence between a session and a recipient

`#session` and `@recipient` are different axes and are never in competition (§1.3): the session says WHERE
the message lives, the addresses say WHO is notified. A write that carries only `#session` notifies the
session's participants; a write that carries only addresses does not touch a session at all.

### 2.4 One resolution, one report

Every write answers with the resolution (§2.2) plus the outcome, in ONE body. The shipped capability
accept already does exactly this and is the precedent:

```json
{"id":"msg_…","transport":"inbox","capability":"solver","target":"worker-b"}
```

— *"The accept names what it chose … because a routed sender has to know which worker took the work."*
A multi-address write extends that principle to an array; it never leaves the caller to guess.

---

## 3. Capability fan-out — the shipped mechanism, quoted exactly

**This spec exposes the shipped capability addressing. It does not reinvent it, and it does not add a
second resolution path.** Verbatim from `README.md` (CR-FEAT-026) and the handler:

> `POST /capabilities/{capability}/inbox` takes the same body as `POST /agents/{id}/inbox` and picks the
> holder:
>
> - **Selection rule, in full.** Candidates are every `online`-or-not registered agent whose `capabilities`
>   include the name, matched EXACTLY like the discovery filter. LIVE holders rank first — liveness is the
>   registry's own derived status (`online` inside `CR_PRESENCE_STALE_AFTER_S`, §3), and while at least one
>   holder is live the pool is exactly the live holders, so a crashed worker absorbs no work. The choice
>   rotates **round-robin** over that pool, one holder per delivery, agent-id ascending, with one cursor per
>   capability advanced once per dispatched delivery. The accept names what it chose —
>   `{"id":"…","transport":"inbox","capability":"solver","target":"worker-b"}` — because the sender has to
>   know which worker took the work. A by-id delivery's body carries neither field: delivering by id is
>   unchanged.
> - **Zero holders is a NAMED error, never a silent drop**: `404 {"error":"NO_CAPABLE_AGENT",
>   "capability":"solver","detail":"…"}` and nothing is dispatched or stored. It is deliberately not the
>   plain `agent not found` a by-id delivery to an unknown id gets. A capability whose holders are
>   registered but NOT live is not this error either: the pool falls back to all holders, because the inbox
>   is durable and the message can wait for a worker that comes back …
> - **Retries do not fan out.** A capability-routed delivery that carries an `idempotency_key` is scoped to
>   the CAPABILITY rather than to the holder (the holder is not known when the key is resolved): a repeated
>   key is answered with the FIRST attempt's accept — same `id`, same `target` — instead of dispatching the
>   same job to a second worker, and it is answered even if the pool has since emptied. A concurrent
>   duplicate in flight is `409`.
> - **Stated limits.** Selection is LOCAL to this relay: a capability held only on a linked relay is not
>   selected, because federation forwards by agent id (CR-FEAT-006) — the refusal says so rather than
>   letting "nobody" mean "nobody anywhere". The cursor lives in the serving process and is not persisted
>   (a restart or a second process on the same store rotates on its own): this is fairness across holders,
>   not exactly-once dispatch. And there is no all-holders fan-out — one holder per delivery is the shipped
>   semantic.

Four consequences for the tag grammar, all of them inherited rather than decided:

1. **`@cap:y` resolves to ONE holder, not to the pool.** D's drawn callout card states the opposite intent —
   `One capability.` / `Multiple agents.` / `Greater reach.` — and the shipped answer is that the reach is
   over TIME (the rotation picks a different holder on the next delivery), not over a single write. The UI
   must not imply a broadcast: D's own drawn sub-line `Task queued · Will notify 3 members` is the sentence
   that must become `Task queued · 1 of 3 holders` unless a team-style fan-out is the address (see §6.3,
   question 3).
2. **`@cap:y` with zero holders is `404 NO_CAPABLE_AGENT`** — the shipped spelling of "this address resolves
   to nothing" for a capability, and it stays that spelling.
3. **The idempotency key of a `@cap:y` write is scoped to the capability.** Any multi-address or wildcard
   write inherits that principle: the key is scoped to the whole ADDRESS SET, so a retry does not re-fan-out.
4. **The rotation cursor is process-local and unpersisted.** An authorization refusal must not advance it
   (CHAT-PERMISSIONS.md §6.4 rule 1) — otherwise a forbidden sender can perturb which worker takes the next
   legitimate job, which is a denial-of-fairness bug.

**NOT BUILT:** the parser, the tag vocabulary, the expansion of `@ns/*`, the resolution report, and the
`@team:` form. The ROUTE (`POST /capabilities/{capability}/inbox`) and its semantics ship today.

---

## 4. Refusals — every one of them named

An address that is malformed, unknown, ambiguous or unauthorised produces a machine-readable refusal with
a stable code. There is no case in which a tag is dropped, ignored, logged and forgotten, or quietly
reinterpreted.

| Condition | Status + code | Body (the part that matters) |
|---|---|---|
| malformed tag: empty token, bad character, `@team:` with no name, `@ns/`, two `#`, more than `maxAddressesPerWrite`, an address longer than `maxAddressLength`, an expansion beyond `maxResolvedTargets` | `400 INVALID_ADDRESS` | `{"error":"INVALID_ADDRESS","address":"@team:","reason":"EMPTY_GROUP_NAME","detail":"accepted forms: @agent, @team:x, @cap:y, @ns/<name>/*, #session"}` |
| a namespace name that is not declared by this server | `400 UNKNOWN_NAMESPACE` (**shipped**, NAMESPACES.md §5) | `{"error":"UNKNOWN_NAMESPACE"}` — never mapped back to the default realm |
| a namespace expansion into a realm the sender is not in | `403 NAMESPACE_MISMATCH` (**shipped**) | `{"error":"NAMESPACE_MISMATCH"}` |
| well-formed, resolves to no agent / no group / no session | `404 UNKNOWN_ADDRESS` | `{"error":"UNKNOWN_ADDRESS","address":"@ghost","detail":"no agent, group or capability named 'ghost'"}` |
| `@cap:y` (or a bare token that resolves to a capability) with no holder | `404 NO_CAPABLE_AGENT` (**shipped** — quoted exactly in §3) | `{"error":"NO_CAPABLE_AGENT","capability":"solver","detail":"…"}` |
| a bare token that matches more than one kind | `409 AMBIGUOUS_ADDRESS` | §2.1 |
| resolves, but the sender lacks the action (`send` for an agent/session, `invoke` for a group/capability) | `403 DELIVERY_FORBIDDEN` (CHAT-PERMISSIONS.md §6.7) | `{"error":"DELIVERY_FORBIDDEN","reason":"NO_GRANT","action":"invoke","target":{"type":"capability","ref":"solver"}}` |

**The order of the checks** is part of the contract, because it decides which refusal a caller sees when a
tag is both unknown and unauthorised:

```
1. parse          → 400 INVALID_ADDRESS
2. namespace      → 400 UNKNOWN_NAMESPACE | 403 NAMESPACE_MISMATCH
3. resolve        → 404 UNKNOWN_ADDRESS | 404 NO_CAPABLE_AGENT | 409 AMBIGUOUS_ADDRESS
4. authorize      → 403 DELIVERY_FORBIDDEN
5. dispatch       → the shipped delivery path (guard, inbox/webhook, federation)
```

**A known information leak, stated rather than hidden:** step 3 before step 4 means an unauthorised caller
learns whether an address EXISTS (a `404` vs a `403`). The alternative — collapsing both into one refusal —
would hide the difference between a typo and a permission problem from the person typing it, which is worse
for the only user who can fix either. **PROPOSED-DEFAULT:** keep resolve-then-authorize (the shipped shape
for `404 agent not found` before the signature checks); see §6.4.

**NOT BUILT:** every code in this table except `400 UNKNOWN_NAMESPACE`, `403 NAMESPACE_MISMATCH` and
`404 NO_CAPABLE_AGENT`. `400 INVALID_ADDRESS`, `404 UNKNOWN_ADDRESS`, `409 AMBIGUOUS_ADDRESS` and
`403 DELIVERY_FORBIDDEN` do not exist in the shipped server, and there is no parser that could raise them.

---

## 5. Concrete examples — what a UI autocomplete shows

### 5.1 The popover, as the approved options draw it

**Option D** draws the mention popover with a type-and-count line per row — this is the exact shape the
autocomplete must produce, and it is the reason the grammar's three entity kinds each need a type label:

```
@Infra-Team          Team · 6 members · Ops & Platform        (green dot, team icon)
@Data-Capabilities   Capability Group · 3 agents · Data & Analytics   (purple dot, hexagon icon)
@Kappa               Agent · Data Processing                  (green dot, robot icon)
@Echo                Agent · Summarization                    (gray/white dot, robot icon)
```

**Option A** draws the same affordance with the count in a right-aligned column, under the popover title
`Agents`:

```
@dev-agents        Engineering agents      4
@ops-agents        Operations agents       3
@research-agents   Research agents         2
@design-agents     Design agents           2
```

and the `Add participants` panel draws the bare, un-@-prefixed group rows with the same counts
(`Engineering` `4`, `Operations` `3`, `Research` `2`, `Design` `2`, `Security` `2`).

**The group-with-count form is therefore `@<name>` + `<type label>` + `<count>`**, and both counts are
COMPUTED from a roster, never stored on the tag: A's `@dev-agents` count `4` and its `Engineering` row `4`
are the same number, and D's `Capability Group · 3 agents` names exactly the three agents it draws in the
cluster (`Kappa [invoke]`, `Lynx [read]`, `Matrix [invoke]`). A count on a tag is a live census; a stale
count is a lie about who will receive the message.

### 5.2 What the wire carries for those rows

| Popover row (drawn) | Wire tag (canonical) | Server answer when delivered |
|---|---|---|
| `@Kappa` | `@kappa` | one delivery to the agent `kappa` |
| `@Infra-Team` (`Team · 6 members · Ops & Platform`) | `@team:infra-team` | the team's fan-out (PROPOSED — §6.3, question 3) |
| `@Data-Capabilities` (`Capability Group · 3 agents`) | `@cap:data-capabilities` | `{"transport":"inbox","capability":"data-capabilities","target":"<one of the 3>"}` |
| `@dev-agents` (`Engineering agents` `4`) | `@team:dev-agents` | the team's fan-out (PROPOSED) |
| `@ns/*` | `@ns/*` | the sender's own namespace expansion, each element authorized |

### 5.3 The `#` chip collision — a real contradiction between two approved options, resolved

**A** draws a topic chip row: `# planning`, `# infra`, `# costs`, `# security`, `# release` and a `+`.
**D** draws the session identity as `SESSION #A7F3`, `Session #A7F3 · Live topology`, `Session #A7F3`
in the map hub, and the audit/system lines reference the same short id.

So the `#` sigil is drawn with **two different meanings in two approved options**, and the brief's grammar
assigns `#` to sessions (`#session`).

**DECISION (D4''):** `#` is reserved for SESSION addressing in an ADDRESS POSITION.
**Rejected alternative:** let `#token` mean whichever of the two resolves (the dual-meaning sigil).
**Why rejected:** it makes a typo ambiguous between "the session I meant" and "a topic tag", and the two
outcomes differ in whether a message gets delivered. Instead:

- In an **address position** (the composer's mention picker, a `to[]` field), `#token` is a session
  reference: it must resolve to a session id, or it is `404 UNKNOWN_ADDRESS`. It is never reinterpreted as
  a topic.
- On the **topic row of a session** (what A draws), `#planning` is session METADATA — the session's
  `topics[]` — and it is not parsed as a tag by any delivery path. It never delivers anything.
- The two surfaces are therefore distinct by construction: a topic chip is not an address, and an address
  is not a topic. A session's short id (`#A7F3` in D) and its full id
  (`3f9a7c2e-6b1d-4e2a-9c7e-1d2f8a4be9c1` in B) must both resolve to the same session; the short form is
  a display affordance and the wire carries the full id.

---

## 6. What is NOT built, and the open questions

### 6.1 NOT BUILT

1. **The parser.** No tag parsing exists anywhere in crier. `POST /agents/{id}/inbox` takes an id in the
   PATH and a capability in the PATH of `POST /capabilities/{capability}/inbox`; neither accepts a tag.
2. **`@team:x`.** There is no named-group object, no roster, no membership, no fan-out. `deny` is what the
   server does with the string today (it would be an agent id, §2.1.2 step 1, and almost certainly a
   `404 agent not found`).
3. **`@cap:y` as a tag.** The capability ROUTE ships; the tag does not. A caller must POST to the
   capability route today.
4. **`@ns/…`.** No expansion, no `@ns/*`, no cross-realm addressing. Namespaces ship as a policy dimension;
   addressing INTO one by tag does not.
5. **`#session`.** No session object exists (CR-CHAT-002). `session_id` ships as an optional opaque
   delivery field (CR-FEAT-004) consumed by the guard's `session:`/`thread:` policy keys — it is not a
   resolvable address and nothing validates it.
6. **The resolution report** (`"resolved":[…]`) and the multi-address write.
7. **The action check on an address.** `invoke` and `403 DELIVERY_FORBIDDEN` do not exist
   (CHAT-PERMISSIONS.md §6.7, §8.1).
8. **Four of the seven refusal codes** (§4, last paragraph).
9. **The autocomplete itself** — it is UI (CR-CHAT-009), and no client exists (`cmd/` is `server` +
   `crier-mcp`).

### 6.2 An agent id is addressable only if it fits the token charset

The registry validates an agent id as **non-empty only** (`if req.ID == ""` → `400 {"error":"id is
required"}`); there is no charset rule. The grammar's `ident` is restricted (`[A-Za-z0-9_][A-Za-z0-9_.-]*`).
**An agent id containing a character outside that charset — a space, a `@`, a `/`, a non-ASCII glyph — is
NOT addressable by tag.** Stated here rather than discovered later, because it is a real (if unlikely)
limitation: the fix is to constrain agent ids at registration, which this spec does **not** do (it would
refuse ids that other lanes may already use) and files as an open question (§6.3).

### 6.3 Open questions

1. **Should registration enforce the token charset?** **PROPOSED-DEFAULT:** no change to `POST /agents`
   now (a tightened charset could refuse an existing fleet's ids); an unaddressable id is refused by the
   parser with `400 INVALID_ADDRESS` and the detail says the id is not addressable — the operator renames.
   A future row may tighten registration with a deprecation window.
2. **Is `#` case-sensitive?** Session ids drawn in D are `#A7F3` (upper) and in B a lowercase UUID.
   **PROPOSED-DEFAULT:** session ids are matched case-sensitively (they are ids, and D's short form is
   derived from one); the DISPLAY may upper-case a short id. A case-insensitive match would make two ids
   collide that the store holds separately.
3. **Team fan-out or team rotation** (§3.1; CHAT-PERMISSIONS.md §8.2, question 3). **PROPOSED-DEFAULT:** `@team:x`
   fans out (one delivery per distinct member, each authorized, one idempotency scope for the whole
   fan-out) because a curated roster is a set of DISTINCT agents; `@cap:y` rotates because a pool is a set
   of interchangeable ones. Owned by CR-CHAT-013.
4. **What does a wildcard write's idempotency key scope by?** **PROPOSED-DEFAULT:** the expanded address
   set's canonical sorted form, so a replay of the same wildcard write answers the first attempt's whole
   report; the alternative (per-target keys) would leave a partial fan-out on a retry.
5. **May `@ns/*` be addressed by a principal outside the namespace at all?** §1.2 says no (the shipped
   `NAMESPACE_MISMATCH`). A grant naming a namespace subject (`read` only, CHAT-PERMISSIONS.md §6.1) may
   later authorize a read-side expansion; a WRITE-side expansion into a foreign realm is refused.
   **PROPOSED-DEFAULT:** no write-side crossing, ever — a realm is a wall, not a filter.
6. **Does the resolve-then-authorize order leak existence?** §4, last paragraph. **PROPOSED-DEFAULT:**
   keep it, and state it in the API docs so it is a known property rather than a surprise.
7. **Group-name and agent-id collision policy.** A team named `atlas` beside an agent named `atlas` makes
   the bare form ambiguous (§2.1.3) but the prefixed form unambiguous. **PROPOSED-DEFAULT:** no namespace
   reservation between kinds; the prefixed form is the answer, and the picker commits it visibly.

---

## 7. Status line

`DRAFT v1 · 2026-10-03 · CR-CHAT-004`

Statements describing behaviour that does not exist are marked **NOT BUILT** in place (§3, close; §4,
close; §6.1). The parser, the tag vocabulary, the group object, the namespace expansion, the session
address and the address authorization do not exist. What DOES exist is the capability delivery ROUTE —
`POST /capabilities/{capability}/inbox` — and §3 quotes it verbatim rather than describing it, because the
whole point of this spec is to expose a mechanism that already ships instead of inventing a second one.
Nothing here may be added to `docs/claims.yaml` until the parser ships.
