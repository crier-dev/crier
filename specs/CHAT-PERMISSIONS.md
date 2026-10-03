# CHAT-PERMISSIONS.md — principals, agent classes, roles, grants and the delivery ACL

Status: **DRAFT v2** · 2026-10-03 · **CR-CHAT-003** + **CR-CHAT-007** + **CR-CHAT-012** + **CR-CHAT-013** + **CR-CHAT-014** + **CR-CHAT-018**
REVISION 2026-10-03 (v2): folded in Bane's detail pass as normative content — §2.5 (a remote participant is a
LOCAL shadow principal marked `remote`, **D14**; federation's only credential TODAY is the one shared
`CR_FED_TOKEN`), §6.9 (an asset fetch is checked PER FETCH against the caller's grant — an asset is never a
permission bypass, **D9**/CR-CHAT-014), §6.10 (a group is a grant SUBJECT and a membership edit is a
permission change, **D8**/CR-CHAT-013), §6.11 (a TASK is a different authority than a message,
**D12**/CR-CHAT-018), and the corrected depth rule cross-referenced from §6.10–§6.11 (a reply STAYS IN
THREAD, **D11**/CR-CHAT-017). The refusal table §6.7, the audit vocabulary §7.4 and the NOT BUILT list §8.1
are expanded; §1.3's out-of-scope list now says explicitly that asset AUTHORIZATION and the LOCAL half of
federation are IN scope. No v1 statement is contradicted.
Source material: the four approved UI options produced 2026-10-03 (read in full with a vision pass; the
drawn labels quoted below are transcribed from the images, and **nothing here is quoted that an image
does not carry**), `~/crier-interface/GOALS.md` (G3, G7, G13, G14; decisions D2, D3, D7, D8), and the
shipped code at the worktree HEAD.

This document is the normative design authority for WHO MAY DO WHAT in the crier comms interface. Where
it and the shipped code disagree, the **code wins** — file the drift as a board row (`DF-CRIER-*`)
rather than rewriting either side silently. Sections that describe behaviour that does not exist yet
are marked **NOT BUILT** and must not be claimed in `docs/claims.yaml` (claims execute against a live
server).

Cross-references: the tag grammar and its refusals are `specs/CHAT-ADDRESSING.md` (CR-CHAT-004); the
record shapes for principals, grants and audit lines are `specs/CHAT-STORAGE.md` (CR-CHAT-006); the
session/thread objects and the interface surface are the sibling's `specs/CHAT-INTERFACE.md` and
`specs/CHAT-SESSIONS.md`.

**Glossary (normative; the exact terms every CHAT spec uses).** A **Principal** is a HUMAN user, distinct
from an agent, and can speak AS a bound agent. An **Agent** is a registered crier agent, with a CLASS
(`personal` | `service`), an OWNER and capabilities. A **Session** is a durable conversation: participants
(principals + agents), membership, an ordered transcript. A **Thread** is a subtree inside a session, created ONLY by a deliberate branch (**D11**): a reply
stays in its thread and never descends on its own.
An **Address** is a tag: `@agent`, `@team:x`, `@cap:y`, `@ns/*`, `#session`. A **Grant** is an ACL entry
binding a principal to an agent / group / session / capability. A **Namespace** is the realm/tenant wall
(already shipped: auth posture, rate limits, retention).

---

## 1. Purpose & scope — the threat model this closes

### 1.1 The hole, stated plainly

**Today the bus is trust-by-reach: anyone who can reach the port can deliver to any agent.**

That is the shipped posture, not a slur. The deployed gate is:

- `CR_AUTH_TOKEN` — ONE shared Bearer secret for the whole deployment (`internal/middleware/auth.go`:
  a mismatch is `401 {"error":"invalid token"}`), and
- optionally `CR_REQUIRE_AGENT_SIG` — a per-agent ed25519 signature on the signed, agent-scoped routes.

Neither of those authenticates the **sender of a message as itself**. `POST /agents/{id}/inbox` accepts
any authenticated caller and an optional, self-declared `sender` field (`deliverRequest.Sender`, "the
originating agent id (webhook envelope metadata)") — it is recorded, never checked. So for delivery, the
gate is reachability: hold the deployment token, name any agent id, and the message lands.

Namespaces (CR-FEAT-029, `specs/NAMESPACES.md`) closed the *outer* wall: a message's realm is taken from
the **target's** row, never from the sender's claim, and a cross-realm delivery is `403 NAMESPACE_MISMATCH`.
They did **not** close the inner one, and they were never meant to: a realm is an administrative domain, so
every principal inside `acme` can still reach every agent inside `acme`.

### 1.2 The threats

| # | Threat | Closed by |
|---|---|---|
| T1 | **I can talk to YOUR agent.** A stranger delivers into my agent's inbox, and my agent works for them. | §6 delivery ACL, §3 agent classes (cross-owner **default-deny**) |
| T2 | **Namespace membership becomes blanket permission.** Being in `acme` is read as "may reach everything in `acme`" — the outer wall is mistaken for the inner one. | §5 roles, §6 grants (a role is a bundle over ACTIONS, not a reach badge) |
| T3 | **Impersonation with no audit.** A human speaks as an agent, and the transcript cannot say which human did it. | §2 principals + binding, §7.4 audit, §6.3 `as_agent` |
| T4 | **Reach widening by fan-out.** `@cap:x` (and `@team:x`) deliver without naming a member, so a reach check written against agent ids is bypassed by addressing a pool. | §6.4 the capability/team ACL is checked on the ADDRESS, before rotation, and a refusal consumes no rotation turn |
| T5 | **A viewer who can write.** A read grant accidentally admits a send, because the check is coarse. | §5 the four roles as action bundles; §6.5 a viewer's `send` is `deny` by construction |
| T6 | **A scope nobody can inspect.** "It owns a repo" is folklore; nobody can enumerate what an agent owns. | §4 the scope is an explicit, inspectable object |

### 1.3 Scope

**In scope:** principals and their lifecycle; the binding by which a principal speaks AS an agent; the
`personal`/`service` agent classes and their default reach; the OWNS/SCOPE relation; the four namespace
roles; the grant model and the grant matrix (action × subject); the DELIVERY ACL for `@agent`, `@team:`,
`@cap:` and `#session`; the refusals; audit lines.

**Out of scope (binding):** the session/thread data model and the interface layout (sibling specs); the
tag grammar itself (CR-CHAT-004); the storage/transport shape (CR-CHAT-006); asset STORAGE and the S3
integration (CR-CHAT-014, `specs/CHAT-STORAGE.md`) — but the AUTHORIZATION of an asset fetch is IN scope
(§6.9), because it is a permission question and must not become a bypass; federation and peer trust
(CR-CHAT-023/024/025, `specs/CHAT-FEDERATION.md` + `specs/CHAT-TRUST.md`) — but the LOCAL consequence of a
remote participant is IN scope (§2.5); the dots/grokbot bridge (CR-CHAT-008). **Namespaces stay the outer
wall and are reused, not replaced** — this spec adds an inner wall and never re-opens the outer one.

---

## 2. Principals — the human user

### 2.1 Definition

A **Principal** is a HUMAN user. It is a different kind of thing from an Agent, and the distinction is the
point: *"a human must not need to hold ed25519 keys to chat (hostile UX) and must not be anonymous (no
audit)"* (CR-CHAT-007). A principal never appears as a bus identity; it appears as the ANSWER to "which
human spoke as which agent".

```json
{
  "id": "prin_01J9Z6V0Q7",
  "kind": "principal",
  "display_name": "Bane",
  "namespace": "",
  "role": "owner",
  "status": "active",
  "created_at": "2026-10-03T17:45:05Z",
  "last_login_at": "2026-10-03T18:02:11Z"
}
```

`kind` is part of the record, not decoration: an id is never guessable as "agent or human" by its shape.
Everything the rest of the system addresses is an **agent** id; a principal id is only ever a subject of a
grant (§6) or the author of an audit line (§7.4).

### 2.2 Lifecycle

| State | Reached by | What changes |
|---|---|---|
| `invited` | an `admin`/`owner` mints an invite (`invite` action, §5) | the principal exists, holds no grant, cannot log in |
| `active` | first successful login | grants apply; deliveries may be made |
| `suspended` | an `admin` suspends it | every grant is inert (not deleted), logins refused, no delivery |
| `revoked` | an `admin` revokes it | grants are tombstoned (§6.6), bindings closed, sessions keep their transcript |

A state change is an audit line (`principal.invited`, `principal.activated`, `principal.suspended`,
`principal.revoked`) and is **never** a silent edit: `suspended` and `revoked` are separate states because
"suspended" is reversible and "revoked" is a decision about a person, which the transcript has to be able
to state years later.

### 2.3 The binding — a principal speaks AS an agent

```json
{
  "id": "bind_01J9Z6W4M2",
  "kind": "binding",
  "principal": "prin_01J9Z6V0Q7",
  "agent": "atlas",
  "as_agent": true,
  "created_at": "2026-10-03T18:02:11Z",
  "created_by": "prin_01J9Z6V0Q7"
}
```

- A binding says *this human may speak as this agent*. It does not say *may read its inbox* — that is a
  separate action (`read`, §6) and it is deliberately not implied.
- `as_agent: true` is the ONLY value this version defines. A binding with `as_agent: false` (an
  admin who may approve on an agent's behalf but not speak as it) is a **PROPOSED-DEFAULT** shape; it is
  not implemented and not claimed.
- **Creating a binding requires the `admin` action on the agent** (§6 grant matrix). A principal cannot
  self-bind: `POST`ing a binding for yourself without the action is `403 DELIVERY_FORBIDDEN` (the one
  authorization code, §6.7).
- An agent may have several bindings (two humans who both own it) and a principal may hold several
  bindings. **A personal agent may have exactly one OWNER** (§3.2) — bindings are speech rights, ownership
  is accountability.
- The bus never sees a principal: a delivery made under a binding is a delivery from the **agent**, and the
  principal rides alongside it as `principal_id` (§6.3). That is how "the bus still only ever sees agents"
  and "the audit names the human" are both true.

**NOT BUILT:** there is no principal store, no binding store, no login, no invite, and no route that
accepts a principal id. Every statement in §2 is the design this spec fixes; the shipped registry has no
such concept, and `docs/claims.yaml` must not claim one.

### 2.4 Login (normative shape, not an implementation)

Login is a session cookie or OIDC (D2's recommendation). The one normative requirement this spec places on
it: **the login outcome is a principal id, and every subsequent write carries `principal_id` + `as_agent`,
or it carries neither.** There is no third state — a request that says "anonymous" is refused on any
surface the ACL is armed for (§6.7), because an unattributable write is exactly what T3 is.

### 2.5 Remote principals — a LOCAL shadow, so grants stay local (D14, CR-CHAT-023/026)

A **remote participant** is a human or agent that lives on ANOTHER crier instance. It is not a new kind of
record and not a second grant model: it is represented LOCALLY as a **shadow principal** marked `remote`,
so every authorization decision in this document runs unchanged against a local principal id.

```json
{
  "id": "prin_fed_peer_01J9Z…",
  "kind": "principal",
  "display_name": "Ada @ peer",
  "namespace": "",
  "role": "member",
  "status": "active",
  "remote": true,
  "remote_instance": "peer",
  "remote_ref": "ada",
  "created_at": "2026-10-03T20:00:00Z",
  "last_login_at": null
}
```

| Field | Rule |
|---|---|
| `remote` | `true` for a shadow; absent (the `omitempty` discipline) for a local principal. A shadow is otherwise an ordinary principal row. |
| `remote_instance` / `remote_ref` | the peer instance and the participant's ref ON that peer. The LOCAL id is the shadow's own id; these two fields are how the audit names where it came from. |
| `role` | the shadow holds a LOCAL role and LOCAL grants like any principal. **A remote participant never carries its origin's grants across the boundary** — a grant is local or it does not exist. That is the whole point of D14. |

Three binding consequences:

1. **Grants stay local.** A delivery from or to a remote participant is authorized by the same
   `may_deliver` predicate (§3.2) against the same local shadow principal. There is no remote grant
   evaluation and no delegation of authorization across the link.
2. **Trust-by-reach, stated as it exists TODAY.** The only federation credential shipped is ONE shared
   secret — `CR_FED_TOKEN` (the source's `CR_FED_TOKEN` equals the destination's `CR_AUTH_TOKEN`) — so a
   linked peer is trusted to speak for ANY identity it names: peers cannot be told apart, given different
   access, or revoked individually. That residual is what `specs/CHAT-FEDERATION.md` and
   `specs/CHAT-TRUST.md` (peer keys, mutual auth, a chain to an anchor) exist to close; it is **not**
   closed here, and this spec must not be read as claiming otherwise.
3. **The audit names the origin.** An audit line for a remote actor carries the shadow principal id AND
   `remote_instance`/`remote_ref` (§7.4), so a transcript can state that a decision involved another
   instance.

**NOT BUILT:** no shadow-principal record, no `remote` field, no remote resolution and no peer-key trust.
Owed: `GET /fed/address?instance=<i>&agent=<a>` (the local shadow resolution, CHAT-ADDRESSING.md §1.5) and
the peer-key link (CR-CHAT-024). The shipped federation credential remains the single shared `CR_FED_TOKEN`;
`specs/CHAT-FEDERATION.md` (CR-CHAT-023/024) and `specs/CHAT-TRUST.md` (CR-CHAT-025) own the cross-instance
design, and this spec fixes only the LOCAL half.

---

## 3. Agent classes — personal vs service

### 3.1 The two classes

| | `personal` | `service` |
|---|---|---|
| **Owned by** | exactly ONE principal (`owner`, §3.2) | the namespace (no single human owner) |
| **Knows about** | its owner — a private context about me | nothing about a person; it knows its SCOPE (§4) |
| **Default reach** | its owner + principals explicitly granted (§6) | the principals/agents its declared scopes include |
| **Addressable by** | `@<agent-id>` | `@<agent-id>`, and collectively by `@cap:y` / `@ns/…` |
| **Reads like** | "my agent, I talk to it" | "the agent that owns the deploy box; anyone on the box's team may reach it" |

The class is a property of the agent ROW:

```json
{
  "id": "atlas",
  "class": "personal",
  "owner": "prin_01J9Z6V0Q7",
  "capabilities": ["planning", "architecture", "tools"],
  "namespace": "acme"
}
```

```json
{
  "id": "deploy-bot",
  "class": "service",
  "owner": null,
  "capabilities": ["deploy", "rollback"],
  "scopes": ["scope_ci_box", "scope_repo_crier"],
  "namespace": "acme"
}
```

- `class` is ABSENT on every pre-existing row, and absent means **unchanged behaviour** (§8, non-regression):
  the same `omitempty` discipline CR-FEAT-029 used for `namespace` and INT-A2A-001 for `a2a`, so an
  unclassed row serialises byte-identically to today. An unclassed row is *reachable by anyone who can
  reach the port* — the shipped posture — and that residual is stated in §8.1 rather than hidden.
- A `personal` row with no `owner`, or a `service` row with an `owner`, is refused at registration
  (`400`, naming the field). A class that cannot answer "who is accountable" is not a class.
- Class is **not** mutable by `PATCH`: changing a personal agent to service (or the reverse) re-writes who
  may reach it, so it is a re-registration, the same rule NAMESPACES.md §5 applies to moving a live agent
  between realms.

### 3.2 THE RULE THAT MATTERS

> **The permission model must let me talk to MY agents but NOT to YOURS. Cross-owner is DEFAULT-DENY.**
> Reaching another owner's agent takes an explicit grant — or that agent being declared `service` with a
> scope that includes me.

Executable form (this is the whole rule, and everything else in this document is machinery around it):

```
may_deliver(principal P, agent A):
  if A.class is absent            -> unchanged shipped posture (trust-by-reach; §8.1)
  if A.class == personal:
      allow  iff  P == A.owner
              or   a live grant (P, A, send) exists
      deny   otherwise                       # including every other owner's agent
  if A.class == service:
      allow  iff  some live scope S of A has S.reach ∋ P
              or   a live grant (P, A, send) exists
      deny   otherwise
```

Two consequences worth stating outright:

1. **Owning an agent does not grant reach to other people's agents.** Ownership is accountability for
   one agent; it is not a key to the namespace.
2. **A `service` agent's reach is not "everyone".** It is the union of its scope reach sets, which is an
   explicit, inspectable object (§4) — a service agent with no declared scope and no grant is reachable by
   `admin`/`owner` only.

### 3.3 What the four approved options actually DRAW about class (and what they do not)

Read with the vision pass and quoted verbatim; no label below is inferred:

- **Option A (channels + threads)** — the Participants panel draws six agents with a role chip and a
  capability line: `Atlas` `ADMIN` `Planning · Architecture · Tools`; `Nova` `MEMBER`
  `Research · Analysis · Docs`; `Sage` `MEMBER` `Tasks · Execution · Monitoring`; `Quill` `VIEWER`
  `Data · Costs · Reporting`; `Pixel` `VIEWER` `Design · UI · Prototyping`; `Beacon` `VIEWER`
  `Security · Compliance · Risk`. The workspace header draws `12 agents · Production` and the footer
  `6/6 agents online`. **No `personal`/`service` label and no owner name appears anywhere in A.**
- **Option B (operator console)** — the agent roster draws a per-row capability chip (`routing`,
  `summarize`, `research`, `vision`, `memory`, `planning`, `data`, `execution`, `image`, `guardrails`,
  `tools`, `analysis`) with a status word (`online` / `idle` / `degraded`). **No class label and no owner
  column.**
- **Option C (triage inbox)** — rows carry role chips only (`MEMBER`, `VIEWER`, `ADMIN`). **No class
  label.**
- **Option D (chat + capability map)** — the node labels are `Orion [admin]`, `Echo [invoke]`,
  `Nova [read]`, `Sage [read]`, `Atlas [invoke]`, `Rhea [read]`, `Kappa [invoke]`, `Lynx [read]`,
  `Matrix [invoke]`, with the map heading `AGENT / CAPABILITY MAP` and the stat cards
  `8 Agents` / `3 Capability Groups` / `12 Active Links`. **No `personal`/`service` label appears.**

So the class is **not drawn in any approved option** — it is a data-model concept this spec fixes, and
when it is surfaced the badge must be mint for `personal` and lavender for `service` (Appendix A), never a new
colour invented at the UI layer. Recording this honestly matters: a reader of the mock must not be told
that a class badge exists in the locked design when none of the four images carries one.

---

## 4. Resource ownership — the OWNS/SCOPE relation

### 4.1 What a scope IS

A **Scope** is an explicit, inspectable object naming a thing an agent is responsible for, and who may
reach that agent BECAUSE of it.

```json
{
  "id": "scope_repo_crier",
  "kind": "scope",
  "holder": "deploy-bot",
  "resource": {"kind": "repo", "ref": "github.com/coding-hermes/crier"},
  "reach": {"principals": ["prin_…", "prin_…"], "groups": ["team:infra"], "capabilities": []},
  "declared_by": "prin_01J9Z6V0Q7",
  "declared_at": "2026-10-03T17:45:05Z",
  "namespace": "acme"
}
```

| Field | Rule |
|---|---|
| `holder` | an agent id. The holder must be `class: service` — a **personal** agent may not hold a scope, because a scope is a reach grant and a personal agent's whole contract (§3.1) is "my owner and nobody else". |
| `resource.kind` | a closed set: `repo` \| `box` \| `service` \| `dataset` \| `namespace`. An unknown kind is `400`, naming the accepted set — never stored as opaque prose (an inspectable object with a free-text "what" is not inspectable). |
| `resource.ref` | the canonical locator for that kind (a repo URL, a hostname, a service name, a dataset id, a namespace name). Carried verbatim; crier never dereferences it (the same rule as an A2A `url` part). |
| `reach` | the principals, named groups and capabilities that may reach the holder. `reach` is the ONLY thing that makes a service agent reachable (§3.2). |
| `declared_by` / `declared_at` | the principal and instant of the declaration. Never `system`: a machine did not decide who may reach a box. |

### 4.2 What owning a resource ENTITLES

Owning a resource entitles the holder to exactly three things, and no more:

1. **Being the address for that resource's traffic.** `@deploy-bot` is how the repo's traffic is addressed;
   a capability the holder advertises (`@cap:deploy`) resolves to it like any other holder.
2. **A reach set.** The principals in `reach` may deliver to the holder without an individual grant each
   (this is the "or that agent being declared service with a scope that includes me" half of THE RULE).
3. **Being inspectable.** `GET /agents/{id}/scopes` (or the agent read) enumerates what it owns.

It does **not** entitle the holder to: reach other agents, read other agents' inboxes, act on the resource
through crier (crier carries messages; it does not perform the deploy), or widen its own `reach`.

### 4.3 Declaring, inspecting and changing a scope

| Operation | Who may | Refusal |
|---|---|---|
| declare | a principal with the `admin` action on the holder, in the holder's namespace | `403 DELIVERY_FORBIDDEN` |
| inspect | the holder's namespace (`read` role and above); any principal in `reach` sees the scope that includes them | `403` on a foreign namespace (`NAMESPACE_MISMATCH`, shipped) |
| change `reach` (add/remove a principal, group or capability) | as declare | as declare; the CHANGE is audited (`scope.reach.changed`) |
| delete | as declare; refused while the holder's deliveries reference it | `409`, naming the holder |

Every one of the four writes an audit line. A `reach` widening is a permission change and is treated as
one: it is the only way a `service` agent becomes newly reachable, and an unaudited reach change would be
indistinguishable from an attack.

**NOT BUILT:** there is no scope record, no route, no `GET /agents/{id}/scopes`, and no `scope.reach.changed`
audit event. The registry row has no `class`, no `owner` and no `scopes` member.

---

## 5. Roles inside a namespace

### 5.1 The four roles

| Role | Bundle of actions | Accountable for |
|---|---|---|
| `owner` | send, read, invite, admin, mint, invoke **+ namespace policy** | the namespace itself (its posture, its members) |
| `admin` | send, read, invite, admin, mint, invoke | day-to-day administration: grants, bindings, scopes |
| `member` | send, read, invite, invoke | ordinary participation |
| `viewer` | read | reading, and nothing else |

`owner` is the namespace's accountable principal, not a super-admin badge: only `owner` may change the
namespace's `auth` posture, `rate_limit_per_minute`, `guard_enabled`/`guard_policy` or `retention_seconds`
— the four per-namespace axes NAMESPACES.md §4 already defines. That is why the role exists at all even
though no approved option draws it (§5.3).

### 5.2 May/may-not, per role

| Action | owner | admin | member | viewer |
|---|---|---|---|---|
| `read` a session's transcript / an inbox | yes | yes | yes | **yes** |
| `send` a message into a session / deliver to an agent | yes | yes | yes | **no** |
| `invite` a participant to a session | yes | yes | yes | no |
| `admin` — manage grants, bindings, scopes, membership | yes | yes | **no** | no |
| `mint` — register a new agent into the namespace, or bind a new agent to a principal | yes | yes | **no** | no |
| `invoke` — address `@cap:y` / `@team:x` (a pool/roster, not a member) | yes | yes | yes | **no** |
| change the namespace's own policy (§5.1) | **yes** | no | no | no |
| read the audit trail of this namespace | yes | yes | own lines | own lines |

Two rules are load-bearing and are stated as such:

- **`read` never implies `send`.** A viewer is a first-class participant with exactly one action; the
  refusal for a viewer's send is the same `403 DELIVERY_FORBIDDEN` an unbound stranger gets (§6.7), so a
  UI can render one message for both.
- **`admin` is a role, not a reach badge.** An `admin` of namespace `acme` may administer `acme`; the
  role grants no reach to an agent in `beta`, and no reach at all to a `personal` agent they do not own
  (§3.2). This is T2 closed.

### 5.3 What the options DRAW about roles

- **A** draws three role chips: `ADMIN`, `MEMBER`, `VIEWER`. **No `OWNER` chip appears anywhere in A.**
  The vision pass is explicit that the chip colour follows the *agent's* colour, not the role: `Nova`'s
  `MEMBER` chip is purple because Nova is purple, `Sage`'s `MEMBER` chip is green because Sage is green.
- **B** draws no role chips at all. Its permission surface is the action **matrix** (§6.2), with an
  `admin` principal row.
- **C** draws three role chips — `MEMBER`, `VIEWER`, `ADMIN` — and the vision pass records the same
  finding as A with the sign flipped: *"the chip colour is NOT strictly tied to role — MEMBER appears in
  both teal and orange, and ADMIN appears in both teal and orange. VIEWER is consistently purple."*
- **D** draws three permission badges in the thread (`[admin]`, `[invoke]`, `[read]`) and a legend that
  states the colour mapping explicitly: `Permission: admin` (orange dot), `Permission: invoke` (purple
  dot), `Permission: read` (blue dot).

**No approved option draws an `OWNER` chip.** The `owner` role is required for the reason in §5.1; its
badge is a UI decision that no mock has made, and Appendix A fixes the token if it is ever drawn.

---

## 6. The grant model

### 6.1 What a grant binds

A **Grant** is an ACL entry binding a principal to a subject, with an action set.

```json
{
  "id": "grant_01J9Z8Q2K7",
  "kind": "grant",
  "principal": "prin_01J9Z6V0Q7",
  "subject": {"type": "agent", "ref": "atlas"},
  "actions": ["send", "read"],
  "granted_by": "prin_01J9Z6V0Q7",
  "granted_at": "2026-10-03T18:10:00Z",
  "expires_at": null,
  "note": "owner may always reach own agent; explicit so it is auditable"
}
```

| Field | Rule |
|---|---|
| `principal` | a principal id, never an agent id. An agent is not a grantee: an agent reaches things by being BOUND (§2.3) or by being owned (`personal`, §3.2), and a second reach path for machines would be a second truth. |
| `subject.type` | `agent` \| `group` (`team:x`) \| `capability` (`cap:y`) \| `session` (`#id`) \| `namespace` (a realm, for `read` only). |
| `actions` | a non-empty subset of the closed action set `send` `read` `invite` `admin` `mint` `invoke`. An unknown action is `400`, naming the accepted set. |
| `expires_at` | `null` = no expiry; a timestamp = the grant is inert after it, and the effective-permission computation treats it as absent (it is NOT deleted — an expired grant is evidence). |
| `granted_by` | must hold the action being granted (you cannot grant `admin` without `admin`). |

**A grant is ADDITIVE to a role and can never exceed it for the granting party** (you cannot grant what you
do not hold). It is the answer to "member, plus exactly one more thing": Option B DRAWS precisely this
case — `kairo` holds `send` `read` and `mint` while holding neither `admin` nor `invite`, i.e. a member
with the `mint` action granted on top of the role bundle.

### 6.2 The grant matrix (action × subject)

The action vocabulary is taken from what is **drawn**, not invented. Option B's permission matrix has
these exact column headers, transcribed verbatim:

```
Principal | send | read | invite | admin | mint
```

and the vision pass records the cells as `✓` (green) and `—` (gray dash), with **no `×` anywhere**:

| Principal (as drawn in B) | send | read | invite | admin | mint |
|---|---|---|---|---|---|
| `orion` | ✓ | ✓ | ✓ | — | — |
| `nimbus` | ✓ | ✓ | — | — | — |
| `atlas` | ✓ | ✓ | ✓ | — | — |
| `lumen` | ✓ | ✓ | — | — | — |
| `kairo` | ✓ | ✓ | — | — | ✓ |
| `echo` | ✓ | ✓ | — | — | — |
| `sable` | — | ✓ | — | — | — |
| `admin` | ✓ | ✓ | ✓ | ✓ | ✓ |

Option D draws `invoke` as a permission name (`[... invoke]`, legend `Permission: invoke`). It is therefore
part of the action set and, per §6.4, it is the action that authorizes addressing a **pool or roster**
rather than a member. **The reconciliation is a decision, stated with its alternative:**

> **DECISION (D3'):** `invoke` is a DISTINCT action from `send`.
> **Rejected alternative:** fold `invoke` into `send` (one action for "may cause work at an agent").
> **Why rejected:** `@cap:y` and `@team:x` are the reach-widening addresses (T4). Folding them into `send`
> means a namespace that wants to let a member talk to ONE named colleague cannot help but also let them
> broadcast to every holder of `cap:deploy`. The drawn D badge `invoke` is evidence that the design already
> treats calling a capability group as its own thing.

The resulting **action × subject** matrix, which is normative:

| action | `agent` | `team:x` | `cap:y` | `#session` | `namespace` |
|---|---|---|---|---|---|
| `send` | deliver into that agent's inbox | **no** — a team is addressed with `invoke` | **no** | append to that session | no |
| `read` | read that agent's inbox | no | no | read that session's transcript | read any session in the realm |
| `invite` | no | add a member to that group | no | add a participant to that session | no |
| `admin` | manage grants/bindings on that agent | edit that group's roster | no | manage that session (delete, retention, participants) | administer the realm |
| `mint` | register that agent id / bind it | no | no | no | register any agent into the realm |
| `invoke` | no | deliver to that named group | deliver to that capability pool | no | **no** — a realm is not a pool |

Reading the matrix, two cells are the whole security story: `send × agent` is T1's cell, and
`invoke × cap:y` is T4's cell.

### 6.3 The delivery ACL — who may deliver to `@agent`

```
POST /agents/{id}/inbox              →  require action `send`  on subject {agent, id}
POST /capabilities/{capability}/inbox→  require action `invoke` on subject {capability, capability}
@team:x delivery                     →  require action `invoke` on subject {group, x}
#session append                      →  require action `send`  on subject {session, id}
```

The **effective sender** is computed once, before the check:

```
effective_sender(request):
  if request carries (principal_id, as_agent):      # a human through the UI/API (§2.3)
      require a live binding (principal_id -> as_agent); else 403 DELIVERY_FORBIDDEN
      return the principal
  if request carries X-Agent-ID (an agent's own signed call):
      return the agent's owner for a `personal` agent, else "the namespace" for a `service` agent
  if request arrives over the federation link (CR_FED_TOKEN today):
      return the LOCAL shadow principal for the named remote participant (§2.5)
      # a remote actor NEVER resolves to a local agent's own reach, and carries no remote grants
  else:
      return anonymous
```

`anonymous` is allowed on an **unclassed** target (the shipped posture, §8.1) and is **refused** on a
`personal` or `service` target when the ACL is armed (`403 DELIVERY_FORBIDDEN`, `"principal":"anonymous"`).

### 6.4 The default-deny rule, and the two delivery surfaces it must cover

> **DEFAULT-DENY, stated as the rule and not as a slogan:** if no live grant, no ownership rule (§3.2) and
> no scope reach set (§4.1) yields `allow`, the delivery is refused. There is no fall-through, no
> "unknown target" allowance, and no surface exempt from the check.

Two properties are binding, because each closes a hole the naive version leaves open:

1. **`@cap:y` and `@team:x` are checked on the ADDRESS, before anything rotates.** The shipped capability
   path resolves the holder *before* the guard and the store (handler comment: "Selection happens here,
   before the deliver path proper"), and the shipped rotation advances "once per dispatched delivery".
   The ACL step therefore runs **after** the idempotency-key resolution and **before** holder selection,
   so that (a) a refused `@cap:y` delivery consumes **no rotation turn** — otherwise a forbidden sender
   could still perturb which worker takes the next legitimate job — and (b) a refusal has no store side
   effect. A rotation turn consumed by a refusal is a denial-of-fairness bug, not a cosmetic one.
2. **A control with no ACL entry is denied by default in the AUTHORIZATION sense only.** i.e. the absence
   of a grant denies; the absence of an ACL *deployment* (§8.1) leaves legacy behaviour unchanged. These
   are different statements and the code must not conflate them.

### 6.5 A viewer who may read but not send

The worked refusal, in one place, because it is the acceptance criterion for CR-CHAT-003
("a principal without a delivery grant is refused with a NAMED error when addressing an agent"):

```
principal  prin_viewer  role=viewer  namespace=acme
grant      (prin_viewer, {session, #build-plan}, [read])
binding    (prin_viewer -> quill, as_agent=true)          # may speak AS quill …
grant      (prin_viewer, {agent, quill}, [read])          # … but only READ it

GET  /sessions/build-plan                       -> 200  (read)
POST /sessions/build-plan/messages              -> 403 DELIVERY_FORBIDDEN  (no `send`)
POST /agents/quill/inbox   {payload: …}         -> 403 DELIVERY_FORBIDDEN  (no `send` on quill)
POST /capabilities/research/inbox              -> 403 DELIVERY_FORBIDDEN  (no `invoke`)
```

Note the third line: owning a binding to `quill` is not `send`. A binding is a speech right that is
exercised through an authorized session or delivery, not a bypass of the action check. Without that rule,
the weakest role in the system would be the strongest attack: bind to any agent, then speak as it.

### 6.6 Revocation semantics

- A grant is **tombstoned, never edited away**: the record stays, `revoked_at`/`revoked_by` are set, and
  the effective-permission computation treats it as absent. Deleting the record would erase the audit trail
  of who held what when (T3).
- Revocation takes effect on the **next** delivery, not on a cached set: the effective-permission
  computation is per request (the guard resolves its credential per check for the same reason —
  NAMESPACES.md §4.1).
- A suspended principal's grants are inert but visible; a revoked principal's grants are tombstoned.

### 6.7 The refusal — a NAMED error, never a silent drop

| Situation | Status + code |
|---|---|
| no grant / not the owner / not in any scope reach set | `403 DELIVERY_FORBIDDEN` |
| a bound principal delivering as an agent it is not bound to (or no binding at all) | `403 DELIVERY_FORBIDDEN`, `"reason":"NO_BINDING"` |
| an anonymous sender on a classed target | `403 DELIVERY_FORBIDDEN`, `"principal":"anonymous"` |
| target in another namespace | `403 NAMESPACE_MISMATCH` (**shipped** — never re-spelled) |
| `@cap:y` with no holder at all | `404 NO_CAPABLE_AGENT` (**shipped** — resolution precedes nothing; a nonexistent pool is not an authorization question) |
| malformed address | `400 INVALID_ADDRESS` (CHAT-ADDRESSING.md §4) |
| address resolves to nothing | `404 UNKNOWN_ADDRESS` (CHAT-ADDRESSING.md §4) |
| an asset fetch whose caller lacks the read grant on the message that owns it (§6.9) | `403 ASSET_FORBIDDEN` (**NOT BUILT**) |
| creating a TASK without the task authority (§6.11) | `403 DELIVERY_FORBIDDEN`, `"reason":"NO_TASK_AUTHORITY"` (**NOT BUILT**) |
| a remote actor whose local shadow holds no grant (§2.5) | `403 DELIVERY_FORBIDDEN`, `"principal":"remote"` (**NOT BUILT**) |

The body is machine-readable, and names the decision inputs so a UI can explain itself without parsing prose:

```json
{"error":"DELIVERY_FORBIDDEN",
 "reason":"NO_GRANT",
 "principal":"prin_viewer",
 "as_agent":"quill",
 "target":{"type":"agent","ref":"atlas"},
 "action":"send",
 "detail":"no live grant for principal prin_viewer on agent atlas (class personal, owner prin_01J9Z6V0Q7)"}
```

**`403 DELIVERY_FORBIDDEN` does not exist in the shipped server.** The shipped authorization refusals are
`403 NAMESPACE_MISMATCH`, `403 GUARD_BLOCKED`, `403 AGENT_QUARANTINED` and the auth middleware's `401`s.
This code is introduced by this spec and is **NOT BUILT**; until it ships, an unauthorized delivery is
accepted exactly as it is today.

### 6.8 Where the check sits in the one delivery path

`internal/registry/handler.go`'s `deliver()` is the ONE deliver implementation behind both
`POST /agents/{id}/inbox` and `POST /capabilities/{capability}/inbox` — sharing it "is the point of
CR-FEAT-026 — a capability-routed delivery is not a parallel delivery path that could drift from the
by-id one on the guard, the lease, the ack or the expiry receipt". The ACL is therefore a **step in that
one function**, never a second path:

| Order | Step (shipped, in `deliver()`) | This spec |
|---|---|---|
| 1 | raw request-body cap — `413 REQUEST_BODY_TOO_LARGE`, *before JSON decoding or any delivery-side work* | unchanged |
| 2 | decode + validate (payload required, ttl, priority, capability required) — *"a 400 answers the REQUEST, never the target"* | unchanged |
| 3 | sender idempotency key — a replay is answered *"from its receipt without consulting the registry at all"*, in-flight is `409`, and any non-accept response releases the key (`defer attempt.Abandon()`) | unchanged; a refusal therefore also releases the key, so a corrected retry under the same key is delivered rather than answered with a replay of the refusal (§8.2 Q1) |
| **4** | **— NEW — address-level ACL, for a POOL or ROSTER address (`@cap:y`, `@team:x`)** | **this spec, NOT BUILT** |
| 5 | capability holder selection + rotation (CR-FEAT-026) | unchanged, and it MUST run after step 4 so that a refusal consumes **no rotation turn** (§6.4 rule 1) |
| 6 | target agent fetch + namespace resolution — `403 NAMESPACE_MISMATCH` | unchanged |
| **7** | **— NEW — target-level ACL, for an AGENT address** (it needs the row's `class` and `owner`) | **this spec, NOT BUILT** |
| 8 | target containment — `403 AGENT_QUARANTINED` | unchanged |
| 9 | guard choke point — `403 GUARD_BLOCKED` | unchanged |
| 10 | webhook driver / durable inbox write / federation fallback | unchanged |
| 11 | audit line | §7.4 (NOT BUILT) |

**Why the ACL is TWO insertion points and not one.** An address-level check is a statement about the POOL
(may this sender invoke `cap:deploy`?) and can be made before any holder exists — and it must be, because
step 5 is where a rotation turn is spent. A target-level check is a statement about a specific agent and
cannot be made before step 6 has read the row that carries its `class` and `owner`. A single insertion
point would therefore be either unauthorizable (too early: no row, no class) or too late (a forbidden
`@cap:y` sender would already have moved the rotation cursor, which is the denial-of-fairness bug §6.4
rule 1 forbids). Both points run the same `may_deliver` predicate (§3.2) against the same effective sender
(§6.3) — two positions, one rule.

Step 9's position is deliberate: **authorization before content inspection.** A refused sender must not be
able to reach the guard (an LLM lane), because otherwise a forbidden sender can spend the deployment's
guard budget — the same noisy-neighbour reasoning NAMESPACES.md §4.2 uses for `guard_enabled: false`.

### 6.9 An asset is a permission check, not a URL (D9, CR-CHAT-014)

An asset (a file, an image, "stuff") is carried BY REFERENCE: the record holds an asset id, never the bytes
(CHAT-STORAGE.md §3.10). That makes the fetch a NEW access point, and the rule is absolute:

> **An asset must NEVER become a permission bypass.** The bytes are reachable only through a check made
> against the caller's LIVE grant at the moment of the fetch — never at upload, and never cached.

The check, in full:

- **Per fetch, not at upload.** A fetch resolves the caller's effective sender (§6.3) and requires the
  action the asset's owning message requires: the `read` action on the message's session, or the `read`
  action on the agent whose inbox carried it — the same action that let the caller SEE the message.
  Uploading an asset grants nothing to anyone.
- **A reference is not a capability.** The object locator (bucket/key, CHAT-STORAGE.md §3.10) is never
  returned to a client; a delivery returns a crier asset reference. If a deployment hands out a pre-signed
  URL, it is short-lived and issued only AFTER the check has passed — the URL is a consequence of the
  check, never a substitute for it.
- **Restricting the message restricts the bytes.** If the message is later restricted — the read grant
  revoked, the attachment removed, the message retention-reaped — the NEXT fetch is refused, because the
  check is derived from the LIVE grant and not from a stored "was allowed". **Honest limit, stated:** a byte
  already handed over cannot be un-fetched; per-fetch checking bounds the window, and short-lived URLs
  bound it further. That is exactly why the check is per fetch (D9) rather than an inherited permission.
- **The refusal is NAMED:** `403 ASSET_FORBIDDEN` (**NOT BUILT**), carrying the asset ref and a reason
  (`NO_GRANT` / `MESSAGE_RESTRICTED` / `ASSET_GONE`), so a UI can distinguish "you may not" from "no longer
  available" (CHAT-STORAGE.md §6.4 gives the fetch-side states).

**NOT BUILT:** no asset object, no per-fetch check, no `403 ASSET_FORBIDDEN`. Owed by CR-CHAT-014:
`POST /assets` (upload), `GET /assets/{id}` (the checked reference), `GET /assets/{id}/content` (the bytes
or a short-lived redirect). The storage shape is `specs/CHAT-STORAGE.md` §3.10/§6.4/§6.5.

### 6.10 A group is a grant SUBJECT, and membership is DATA — never authority (D8, CR-CHAT-013)

The grant matrix §6.2 already binds a Grant to a `group` subject: `invoke` delivers to the named group,
`admin` edits its roster. This section states the two rules that keep that from silently widening anyone's
rights:

1. **A group is a subject, not a role.** Holding `invoke` on `group:x` means "may address the group"; it
   confers NO action on any MEMBER, and NO `read` on any history — a group has no history of its own, it is
   a routing set, and the messages live in sessions (CHAT-STORAGE.md §3.5).
2. **A group is curated and editable, and the edit is a PERMISSION-RELEVANT change.** Whoever holds `admin`
   on `group:x` may add or remove members. That edit:
   - does **not** confer any new action on a member ADDED — an added agent is reachable by the group's
     existing `invoke` right and holds exactly the role and grants it already had;
   - does **not** confer any new action on the EDITOR — `admin` on a group is a right over the roster, not
     over the members;
   - is **AUDITED** as a permission change (`group.membership.changed`, §7.4, **NOT BUILT**), because a
     change that alters who a group's messages reach is indistinguishable from an attack if it is silent;
   - is resolved **per request** (the effective-permission computation is per delivery, §6.6), so routing
     always follows the CURRENT roster and a stale cached set can never be used to reach a removed member.

**The named group vs the capability target:** a named group is a curated roster (membership is DATA); a
capability target is a dynamic pool resolved by the shipped selector (`POST /capabilities/{capability}/inbox`
→ ONE live holder, round-robin) and is NOT a fan-out. The two are different things and neither may silently
become the other (CHAT-ADDRESSING.md §1.4).

**Cross-reference — depth (D11 corrected, CR-CHAT-017):** nothing here changes message nesting. A delivery
to a group or capability reaches each member through the existing per-agent path, and the message in a
thread STAYS IN THREAD — depth is never a function of how many recipients a tag reached (CHAT-ADDRESSING.md
§2.6).

**NOT BUILT:** no group object, no roster, no membership route, no `group.membership.changed` event. Owed by
CR-CHAT-022: `POST /groups`, `GET /groups`, `GET /groups/{id}`, `PATCH /groups/{id}/members`.

### 6.11 A TASK is a different authority than a message (D12, CR-CHAT-018)

The message-kind split (plain / addressed / task) is a SAFETY property: conflating an addressed message
with an action is how a tag becomes a remote command (CHAT-ADDRESSING.md §2.5). Authorization follows the
kind:

| Kind | The authority required | May address | May create work? |
|---|---|---|---|
| `plain` | `send` on the session (§6.3) | the session | no |
| `addressed` | `send` on the session / `send` on the agent (§6.3) | an agent, or a group/capability (NOTIFY only) | **no** — a tag never executes |
| `task` | the **task authority**: `invoke` on the TARGET, plus `send` on the session it sits in | an agent, or a group/capability **for EXECUTION** | **yes** — the only kind that may |

Rules, stated as the authority model:

- **Addressing a group/capability is not the same right as TASKING it.** `@team:x`/`@cap:y` with the
  `invoke` action may be used to ADDRESS (notify) or to EXECUTE, and the two uses are distinguishable on
  the wire by the message KIND — an addressed message to a capability notifies the addressed set and
  executes nothing; a task to a capability is dispatched for execution.
- **Creating a TASK requires the task authority** — `invoke` on the target (an agent, or the
  group/capability), plus `send` on the session the task is raised in. The matrix's `invoke` cell is
  therefore the cell that matters for execution: it is what distinguishes "may call this pool/roster" from
  "may talk in this room".
- **A TASK is a record with a lifecycle, not a message with a flag.** It has state
  (open/claimed/running/done/failed), and every transition is an audited, append-only record-version
  (CHAT-STORAGE.md §3.7). A `task.created` audit line names the creator and the target (§7.4).
- **Who may create one:** a principal holding `invoke` on the target and `send` on the session (a `member`
  with that grant, or above). A `viewer` and a grantless stranger are refused `403 DELIVERY_FORBIDDEN`
  `"reason":"NO_TASK_AUTHORITY"` (**NOT BUILT**), the same code as every other authorization refusal (§6.7).
- **A tag that is NOT a task is never enough.** No amount of addressing — an agent, a group, a capability,
  a wildcard — creates work. Execution is a separate, explicit kind.

**NOT BUILT:** no task kind, no task authority, no task route and no `task.created` event. Owed:
`POST /tasks`, `POST /tasks/{id}/claim`, `POST /tasks/{id}/complete`, and the `kind:"task"` value on the
session append/write path.

---

## 7. Worked examples

### 7.1 Mine vs yours — I may reach my agent, refused to yours

```
principal  prin_bane   (namespace acme)
agent      atlas       class=personal  owner=prin_bane   capabilities=[planning,architecture,tools]
agent      quill       class=personal  owner=prin_other  capabilities=[data,costs,reporting]
grant      (prin_bane, {agent, atlas}, [send,read])

POST /agents/atlas/inbox   as prin_bane     -> 201 accepted        (owner, and granted)
POST /agents/quill/inbox   as prin_bane     -> 403 DELIVERY_FORBIDDEN reason=NO_GRANT
                                               detail: "… class personal, owner prin_other"
POST /capabilities/data/inbox as prin_bane  -> 403 DELIVERY_FORBIDDEN action=invoke
```

The middle line is THE RULE, executed. The third line is worth noticing: `prin_bane` is refused the
capability pool even though a holder exists, because `invoke` is its own action (§6.2) and no grant
confers it.

### 7.2 A service agent that owns a repo — who may reach it

```
agent   deploy-bot  class=service  owner=null  capabilities=[deploy,rollback]
scope   scope_repo_crier  resource={kind:repo, ref:github.com/coding-hermes/crier}
        reach={principals:[prin_bane, prin_ana, prin_leo], groups:[team:infra], capabilities:[]}
        declared_by=prin_bane
grant   (prin_ana, {agent, deploy-bot}, [send])          # explicit, on top of the scope

POST /agents/deploy-bot/inbox as prin_leo   -> 201  (in scope_repo_crier's reach)
POST /agents/deploy-bot/inbox as prin_sam   -> 403 DELIVERY_FORBIDDEN reason=NO_GRANT
POST /agents/deploy-bot/inbox as <team:infra member> -> 201 (the group is in reach)
GET  /agents/deploy-bot/scopes              -> 200  {scopes:[scope_repo_crier]}   # NOT BUILT
```

And the honest negative: `deploy-bot` is reachable **because a scope says so**, not because it is a
"service agent" — a service agent with no scope and no grant is reachable by `owner`/`admin` only (§3.2).

### 7.3 A viewer who may read but not send

§6.5, verbatim. This is the acceptance criterion for CR-CHAT-003 and the row's proof obligation:
*"the refusal is tested, not asserted"* — the test must measure the refusal live (a `403` on the write and
a `200` on the read), not assert a table.

### 7.4 The audit line each outcome writes

The event vocabulary is anchored on what Option B **draws** in its `AUDIT TRAIL` table — columns
`Time | Principal | Event | Details`, with these exact events transcribed: `session.complete`,
`message.send`, `lease.renew`, `session.start`, `permission.grant`, `agent.degraded`, `queue.depth`,
`heartbeat`. This spec adds the events the permission model needs, marked **NOT BUILT**:

| Event | Written when | `Details` |
|---|---|---|
| `permission.grant` (drawn) | a grant is created | the grant id |
| `permission.revoke` (**NOT BUILT**) | a grant is tombstoned | the grant id |
| `delivery.denied` (**NOT BUILT**) | `403 DELIVERY_FORBIDDEN` | the action + target + reason |
| `delivery.allowed` (**NOT BUILT**) | a classed target accepts a delivery | the action + target |
| `binding.create` (**NOT BUILT**) | a principal binds to an agent | the binding id |
| `scope.declared` / `scope.reach.changed` (**NOT BUILT**) | a scope is declared / widened | the scope id |
| `principal.login` (**NOT BUILT**) | a principal authenticates | the session id |
| `group.membership.changed` (**NOT BUILT**) | a named group's roster is edited (§6.10, CR-CHAT-022) | the group id + the adds/removes |
| `task.created` (**NOT BUILT**) | a TASK is created (§6.11, CR-CHAT-018) | the task id + the target |
| `asset.fetched` / `asset.fetch.denied` (**NOT BUILT**) | an asset fetch succeeds / is refused (§6.9, CR-CHAT-014) | the asset ref + the owning message |
| `delivery.remote` (**NOT BUILT**) | a delivery to/from a remote shadow principal (§2.5) | the shadow principal + `remote_instance`/`remote_ref` |

`delivery.allowed` is deliberately included: an audit that records only denials cannot answer "who sent
this", which is T3's question.

---

## 8. What is NOT built, and the open questions

### 8.1 NOT BUILT

1. **Principals.** No principal store, no record, no lifecycle, no login, no invite, no route.
2. **Bindings.** No binding store and no `as_agent` on any write path. The shipped `deliverRequest.Sender`
   is a self-declared string that is recorded and never checked — that is the impersonation hole T3.
3. **Agent classes.** The registry row has no `class` and no `owner`. Every agent is effectively unclassed,
   and an unclassed row keeps today's trust-by-reach posture — **stated here as the residual risk, not
   glossed**: until classes ship, §3.2's rule is not in force anywhere.
4. **Scopes.** No scope record, no `GET /agents/{id}/scopes`, no `scope.*` audit event. Nothing in crier
   can answer "what does this agent own".
5. **Roles.** No role field, no role store, no enforcement. The four roles are a design object here.
6. **Grants.** No grant store, no evaluation, no tombstone. The grant matrix §6.2 is a contract, not code.
7. **The delivery ACL.** `403 DELIVERY_FORBIDDEN` does not exist. `deliver()` has no authorization step
   between namespace resolution and the guard choke point; the nine-step table in §6.8 marks step 5 as new.
8. **Revocation.** No revocation, no expiry evaluation, no per-request effective-permission computation.
9. **Audit.** No audit trail. The eight drawn event names in §7.4 exist only as pixels in Option B; the
   seven events this spec adds exist only in this document.
10. **Class/owner/role badges in the UI.** None of the four approved options draws a class label, an owner
    name or an `OWNER` chip (§3.3, §5.3). No UI work may present one until the data model ships.
11. **Named groups as grant subjects.** No group object, no roster, no membership route and no
    `group.membership.changed` audit event. Owed by **CR-CHAT-022**: `POST /groups`, `GET /groups`,
    `GET /groups/{id}`, `PATCH /groups/{id}/members` (**NOT BUILT**). Until they ship, `subject.type: group`
    is a contract with no object behind it.
12. **Asset authorization.** There is no asset object, no per-fetch check and no `403 ASSET_FORBIDDEN`.
    Owed by **CR-CHAT-014**: `POST /assets` (upload), `GET /assets/{id}` (the checked reference),
    `GET /assets/{id}/content` (the bytes; a short-lived redirect is acceptable only after the check
    passes) (**NOT BUILT**).
13. **The TASK authority.** No task kind, no task authority, no `POST /tasks`, no
    `403 … NO_TASK_AUTHORITY`. The message-kind split (D12) is a contract with no construct behind it.
14. **Remote shadow principals.** No `remote` field, no shadow record, no remote resolution and no `403`
    for a grantless remote actor. Owed by **CR-CHAT-023/024** (§2.5): `GET /fed/address?instance=<i>&agent=<a>`
    (the local shadow resolution, CHAT-ADDRESSING.md §1.5) and the peer-key link; the shipped federation
    credential remains the single shared `CR_FED_TOKEN`.

### 8.2 Open questions

1. **Does a replayed idempotent delivery re-check the ACL?** §6.8 step 3 places the idempotency replay
   BEFORE authorization, on the ground that a replay is not a new delivery and a since-revoked grant must
   not re-litigate a decision already made. The alternative — authorize first, so a revoked principal's
   replay is refused — is defensible. **PROPOSED-DEFAULT:** replay first (as shipped order implies).
2. **May an agent reach another agent directly?** §6.3's effective-sender rule maps an agent's own signed
   call to its owner's reach. The alternative is to give the agent a principal-shaped identity of its own.
   **PROPOSED-DEFAULT:** map to the owner (one accountable human per `personal` agent), and to "the
   namespace" for `service`.
3. **`team:x` fan-out vs rotation.** Named teams do not exist. A curated roster whose delivery rotates
   would silently drop a message on all but one member; a roster whose delivery fans out costs N writes.
   **PROPOSED-DEFAULT:** `@team:x` **fans out** (one delivery per distinct member, each ACL-checked, one
   idempotency scope for the whole fan-out) while `@cap:y` **rotates** (one holder, shipped). The
   distinction is D8's: a named group is a curated set of DISTINCT agents; a capability is a pool of
   interchangeable ones. Owned by CR-CHAT-004/CR-CHAT-013.
4. **Is `invoke` one action or two** (`invoke:team` / `invoke:cap`)? **PROPOSED-DEFAULT:** one action, split
   later only if a deployment asks for it (the closed action set is the contract, and splitting it is
   additive).
5. **Who owns a `service` agent when nobody does?** §3.2 says no single human owner. But accountability
   needs a name: **PROPOSED-DEFAULT:** the namespace's `owner` role holder is the accountable principal of
   record for a `service` agent, without gaining reach to it.
6. **Do grants survive a namespace rename?** Namespaces are immutable in the shipped model (a move is
   unregister + re-register, NAMESPACES.md §5), so a grant naming a namespace subject is invalidated by a
   move. **PROPOSED-DEFAULT:** a move tombstones namespace-scoped grants and requires re-granting; the
   alternative (silently carrying them) would re-open T2.
7. **Where is `mint` enforced?** `POST /agents` today is a deployment-level route with no notion of who is
   registering. **PROPOSED-DEFAULT:** `mint` is checked on the registration route for `personal` agents and
   on the namespace for `service` ones, and an unarmed deployment keeps the shipped behaviour.
8. **Is an asset fetch re-checked when the message it belongs to is later restricted?** §6.9 says YES — the
   check is per fetch, so restricting the message takes effect on the NEXT fetch, not retroactively on bytes
   already handed over. **PROPOSED-DEFAULT:** per-fetch (D9), with the honest limit that a byte already
   fetched cannot be un-fetched; short-lived signed URLs bound the window, and the storage side states the
   bundle behaviour (CHAT-STORAGE.md §6.4).

---

## Appendix A — the colour roles (reconciliation of what is drawn)

The four options do not agree, and the disagreement is a real design finding rather than a rendering
accident. Recorded so the UI does not ship a colour rule that contradicts the API:

| Source | Drawn statement | What it means |
|---|---|---|
| A | role chips `ADMIN`/`MEMBER`/`VIEWER`; chip colour follows the AGENT (`Nova`'s `MEMBER` is purple, Sage's `MEMBER` is green) | colour = agent identity, NOT role |
| C | role chips `MEMBER`/`VIEWER`/`ADMIN`; "MEMBER appears in both teal and orange, and ADMIN appears in both teal and orange. VIEWER is consistently purple" | the same finding, with the sign flipped |
| B | no role colour; permission is carried by the action MATRIX (`send`/`read`/`invite`/`admin`/`mint`) | colour carries status (`online` green, `idle` gray, `degraded` amber) and lease state (`active` teal, `closing` amber) |
| D | legend, explicit: `Permission: admin` orange · `Permission: invoke` purple · `Permission: read` blue | colour = ACTION/role |

**Normative rule (this spec's decision):**

1. **Colour encodes ACTION (a role's permission), never agent identity, on any permission surface.**
   D's legend is the only explicit statement of a mapping in the four options, so it wins the argument;
   A's and C's per-chip agent colouring is reclassified as an identity accent on the message row (an
   avatar/handle tint), which is allowed and is not a permission indicator. A UI must never tint a
   permission badge with the agent's identity colour.
2. The badge palette is the brand's, not D's raw hexes: `admin` → amber (the drawn "warning/attention"
   accent, and the only role that can widen reach); `invoke` → lavender (the drawn "agent identity"
   accent, and the action that addresses a pool); `read` → mint-dim (the brand's positive accent at low
   emphasis); `owner` (undrawn, §5.3) → amber at full emphasis plus a key glyph; `viewer` → mint-dim, the
   same as `read`, because a viewer's whole bundle IS `read`.
3. `Flagged` (drawn in A as an amber shield + amber text) and the amber shield-with-`!` (drawn in C on two
   rows) are **content/status** markers — the guard's verdict and the agent's degraded state — and must not
   be reused as a permission colour. Two meanings on one colour is how a permission badge gets read as a
   warning and ignored.

---

## 9. Status line

`DRAFT v2 · 2026-10-03 · CR-CHAT-003 + CR-CHAT-007 + CR-CHAT-012 + CR-CHAT-013 + CR-CHAT-014 + CR-CHAT-018`

REVISION 2026-10-03 (v2): §2.5 (remote shadow principals — D14), §6.9 (asset fetch is a permission check —
D9), §6.10 (groups as grant subjects — D8), §6.11 (the task authority — D12); §6.3's effective-sender rule
extended for a remote actor; §6.7's refusal table, §7.4's audit vocabulary and §8.1's NOT BUILT list each
extended, and every new affordance names its owed endpoint. §7.4's intro count was also corrected (it said
"four" over a table of seven).

Statements in this document that describe behaviour which does not exist are marked **NOT BUILT** in place
(§2.3, §2.5, §3.3, §4.3, §5.3, §6.7, §6.9, §6.10, §6.11, §7.4, §8.1). Nothing here may be added to
`docs/claims.yaml` until the corresponding code ships, because claims execute against a live server.
