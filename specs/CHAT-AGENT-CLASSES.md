# CHAT-AGENT-CLASSES.md — agent classes & resource ownership (spec of record)

Status: **DRAFT v1** · 2026-10-06 · **CR-CHAT-012**

Source material: the CR-CHAT-012 board row (Bane, 2026-10-03), quoted in full in §1.1; the shipped
permission model at HEAD — `internal/permissions/` (the `permissions` package and its JSONL/PostgreSQL
stores), `internal/registry/permissions.go` (the two delivery-ACL insertion points),
`cmd/server/permissions.go` (the boot wiring), and the registry row `internal/registry/types.go`; and the
sibling specs of the comms-interface series as they stand on disk.

Cross-references: the object definitions and the series' decision ledger are
`specs/CHAT-INTERFACE.md` (CR-CHAT-001) — it defines *Agent* as a Principal-owned, classed thing in its §2,
and this document is the authority for the class taxonomy that definition leans on; the grant model, the
four namespace roles, the effective-sender rule and the DELIVERY ACL are `specs/CHAT-PERMISSIONS.md`
(CR-CHAT-003 + CR-CHAT-007); the session / membership / transcript objects are `specs/CHAT-SESSIONS.md`
(CR-CHAT-002 + CR-CHAT-005) and `specs/CHAT-THREADING.md` (CR-CHAT-005); the tag grammar is
`specs/CHAT-ADDRESSING.md` (CR-CHAT-004); the record shapes and the dual backend are
`specs/CHAT-STORAGE.md` (CR-CHAT-006); the realm wall is `specs/NAMESPACES.md` (CR-FEAT-029).

This document is the **normative design authority for the agent taxonomy and the ownership model**: the two
agent CLASSES, the OWNER relation, the OWNS/SCOPE relation over resources, and the default-reachability
matrix per class. Where it and the shipped code disagree, the **code wins** — file the drift as a board row
(`DF-CRIER-*`) rather than rewriting either side silently. Sections that describe behaviour which does not
exist yet are marked **NOT BUILT** and must not be claimed in `docs/claims.yaml` (claims execute against a
live server).

**Overlap with `specs/CHAT-PERMISSIONS.md`, stated so it cannot be misread as two truths.**
`CHAT-PERMISSIONS.md` §3 ("Agent classes — personal vs service") and §4 ("Resource ownership — the
OWNS/SCOPE relation") already restate these rules from inside the permission model, and they landed first
(commit `ff80dbd`, merged `4927e54`). This document is the **extraction**: it fixes the taxonomy as its own
subject, adds the Registry-row contract, the full Scope object, the decisions ledger and the acceptance
tests the taxonomy is judged by, and it does **not** restate the grant/role/delivery-ACL design — for a
GRANT's shape, a ROLE bundle or the DELIVERY ACL, `CHAT-PERMISSIONS.md` is the authority. The two documents
state the same rules for the class taxonomy; if they ever disagree, the **shipped code wins** and the drift
is a `DF-CRIER-*` row.

---

## Glossary (normative; the terms this document uses)

An **Agent** is a registered crier agent with a CLASS, a reach posture and capabilities. A **Principal** is
a HUMAN user, distinct from an agent; it is the only thing that can OWN an agent and the only thing a
GRANT binds (`CHAT-PERMISSIONS.md` §6.1 — a grant's `principal` field is never an agent id). An **Owner** is
the single principal accountable for a `personal` agent. A **Scope** is an explicit, inspectable object by
which a `service` agent declares a resource it is responsible for and the set of principals/groups/
capabilities that may reach it BECAUSE of that resource. **Reach** is the union of a scope's
`reach` sets, plus ownership, plus live grants — nothing else. Cross-owner is **DEFAULT-DENY**.

---

## 1. Purpose & scope

### 1.1 The requirement, in Bane's own words (CR-CHAT-012)

> Two classes of agent, with different permissions by construction.
>
> **PERSONAL agent** — owned by one human; it knows about its owner (a private context about me); default
> reach = its owner plus explicitly granted principals. This is my agent, I talk to it.
>
> **SERVICE / SHARED agent** — scoped; may OWN systems, code or other things (an agent that owns a repo, a
> box, a service). It is reachable according to its scope, not its owner.
>
> **THE RULE THAT MATTERS:** the permission model must let me talk to MY agents but NOT to YOURS —
> cross-owner is default-deny, and reaching another owner's agent requires an explicit grant (or its
> declaration as a service agent whose scope includes me).
>
> ACCEPTANCE: the class + ownership model is normative; the default-reachability matrix is stated per class
> and tested (my agent: reachable by me, refused to a stranger; your agent: refused to me without a grant; a
> service agent with a scope: reachable per scope, with the scope itself an explicit, inspectable object).

### 1.2 The hole this closes

**Today the bus is trust-by-reach: anyone who can reach the port can deliver to any agent.** A registered
agent has no class, no owner and no scope, so the bus cannot answer "is this MY agent or YOURS". Two threats
follow, and they are the ones this taxonomy closes:

| # | Threat | Closed by |
|---|---|---|
| T1 | **I can talk to YOUR agent.** A stranger delivers into my agent's inbox, and my agent works for them. | §2 (classes) + §3 (owner) + §5 (default-deny matrix) |
| T6 | **A scope nobody can inspect.** "It owns a repo" is folklore; nobody can enumerate what an agent owns or who may reach it because of it. | §4 (the Scope is an explicit object) |

The leak is a **residual, stated not hidden**: a row with NO class keeps today's posture exactly (§2.6,
§8.1) — the taxonomy is additive and arms nothing until a deployment writes a class.

### 1.3 Scope

**In scope (binding):** the two agent classes and their defining properties; the OWNER relation for
`personal` agents; the OWNS/SCOPE relation for `service` agents over resources; the Scope object and its
closed resource-kind set; the default-reachability matrix per class and the `may_deliver` predicate; the
refusal and the acceptance tests for cross-owner denial.

**Out of scope (binding):** the principal lifecycle, login, bindings and the grant model — `CHAT-PERMISSIONS.md`
(CR-CHAT-003/007); the four namespace roles and their bundles — `CHAT-PERMISSIONS.md` §5; the tag grammar
that ADDRESSES an agent — `CHAT-ADDRESSING.md` (CR-CHAT-004); the session/thread objects — `CHAT-SESSIONS.md`
/ `CHAT-THREADING.md`; the record shapes and the dual backend — `CHAT-STORAGE.md`; the realm wall — the
shipped `NAMESPACES.md` (reused, never re-opened). **Namespaces stay the outer wall; this document adds the
inner one and never bridges two realms.**

### 1.4 Authority on conflict

1. **Shipped code wins** over this document and over `CHAT-PERMISSIONS.md`.
2. For **grants, roles, the effective sender and the DELIVERY ACL shape**, `CHAT-PERMISSIONS.md` is the
   authority; this document fixes only the taxonomy those rules read.
3. For **the class fields on the registry row, the Scope object, the resource-kind set and the
   default-reachability matrix**, this document is the authority.

---

## 2. The two agent classes

### 2.1 THE RULE

> **The permission model must let me talk to MY agents but NOT to YOURS. Cross-owner is DEFAULT-DENY.**
> Reaching another owner's agent takes an explicit grant — or that agent being declared `service` with a
> scope whose `reach` includes me.

Everything else in this document is machinery around that sentence.

### 2.2 `personal` — owned by one human

| Property | Rule |
|---|---|
| Owned by | exactly ONE principal (the `owner`, §3) |
| Knows about | its owner — a private context about me |
| Default reach | its OWNER, plus principals explicitly granted (`CHAT-PERMISSIONS.md` §6) |
| May hold a scope? | **NO** (§4.1) — a scope is a reach grant, and a personal agent's whole contract is "my owner and nobody else" |
| Addressable by | `@<agent-id>` |
| Reads like | "my agent, I talk to it" |

### 2.3 `service` — scoped, owns systems

| Property | Rule |
|---|---|
| Owned by | **no single human owner** (`owner` absent). Accountability is a namespace question, §3.4 |
| Knows about | nothing about a person; it knows its SCOPE (§4) |
| Default reach | the principals/groups/capabilities its declared scope `reach` sets include — and nothing else |
| May hold a scope? | **YES** — a scope is the only thing that makes a service agent reachable (besides a grant) |
| Addressable by | `@<agent-id>`, and collectively by `@cap:y` / `@ns/…` |
| Reads like | "the agent that owns the deploy box; anyone the box's scope lists may reach it" |

Two consequences, stated outright (§5):

1. **Owning an agent does not grant reach to other people's agents.** Ownership is accountability for one
   agent; it is not a key to the namespace.
2. **A `service` agent's reach is not "everyone".** It is the union of its scope `reach` sets — an explicit,
   inspectable object — plus live grants. A service agent with no declared scope and no grant is reachable by
   the namespace's `admin`/`owner` role-holders only, and by nobody else.

### 2.4 The class is a property of the agent ROW

The class belongs on the registry row, not in a side table. The contract this document fixes:

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
  "capabilities": ["deploy", "rollback"],
  "namespace": "acme"
}
```

- **Fields:** `class` (`personal` | `service`) and `owner` (a principal id) are OPTIONAL additions to the
  registry row `internal/registry/types.go`'s `Agent`. Both are `omitempty`, the same discipline CR-FEAT-029
  applied to `namespace` and INT-A2A-001 to `a2a`, so an unclassed row serialises **byte-identically** to
  today.
- **Coherence is validated at registration** (and refused `400`, naming the field):
  - `personal` ⇒ `owner` MUST be a non-empty principal id;
  - `service` ⇒ `owner` MUST be absent;
  - unclassed (`class` absent) ⇒ `owner` absent.
  A class that cannot answer "who is accountable" is not a class.
- **`PATCH /agents/{id}` MUST NOT change `class` or `owner`.** Changing a personal agent to service (or the
  reverse) re-writes who may reach it, so it is a re-registration — the same rule `NAMESPACES.md` §5 applies
  to moving a live agent between realms. A `PATCH` that carries a different `class` is `400`.
- **The read surfaces:** `GET /agents` and `GET /agents/{id}` return `class`/`owner` when set. A
  `GET /agents?class=` filter is **NOT BUILT**.

### 2.5 Where the class data lives TODAY (interim, stated honestly)

The registry row has **no** `class` and **no** `owner`. Until §2.4 ships, the ACL reads the class from a
`permission.agent` **record** in the permission store (`internal/permissions` — `RecordAgent` /
`AgentInfo{ID, Class, Owner, Namespace, Capabilities, Scopes}`), folded keep-LAST per id like every other
record. That is deliberate: it lets the delivery ACL (§5) be armed and evaluated before the registry row
grows, and the ACL reads **one shape** (`permissions.AgentInfo`) either way — when the registry row grows
`class`, the lookup source moves and the ACL does not change.

**This is an interim, not the contract.** §2.4 is the contract and is **NOT BUILT** (§8.1 item 1).

### 2.6 An unclassed row keeps today's posture (the residual)

`class` is ABSENT on every pre-existing row, and absent means **unchanged behaviour**: an unclassed agent is
reachable by anyone who can reach the port — the shipped trust-by-reach posture. The ACL has no class to
evaluate and allows (reason `UNCLASSED`). This residual is stated here and in §8.1 rather than glossed:
**until classes ship, THE RULE is not in force anywhere.**

---

## 3. The OWNER relation (personal agents)

### 3.1 Definition

The OWNER relation binds **exactly one principal** to a `personal` agent:

```
owner(agent A) -> principal P     iff   A.class == "personal" AND A.owner == P.id
```

- `owner` is a **principal id**, never an agent id. An agent is not an owner: an agent reaches things by
  being BOUND (`CHAT-PERMISSIONS.md` §2.3) or by being owned; a second reach path for machines would be a
  second truth.
- **A personal agent has exactly one owner.** Bindings are *speech rights* and a personal agent may have
  several (`CHAT-PERMISSIONS.md` §2.3); ownership is *accountability* and there is exactly one.
- A `personal` row with no owner is refused at registration (`400`, naming the field).

### 3.2 What the owner gets

Owning a personal agent entitles the owner to exactly one reach fact and no more:

1. **Default access to their agent.** `may_deliver(owner, personal_agent)` is `allow` (`send`), by ownership
   — reason `OWNER` in the shipped predicate. This is the "my agent, I talk to it" half of THE RULE.

It does **not** entitle the owner to: reach another owner's `personal` agent (T1), read another agent's
inbox, administer the namespace, or reach a `service` agent in another realm. Ownership of one agent is not
a key to the namespace.

### 3.3 The owner can grant access to other principals

A `personal` agent is **not a closed world**: the owner may grant a principal access to it.

- The grant is an ordinary `CHAT-PERMISSIONS.md` §6.1 entry — `{principal, subject:{agent, A}, actions:[...]}` —
  created by a principal holding the `admin` action on A (the owner has it by definition).
- The granted principal then reaches A by **GRANT**, exactly like any other granted subject; there is no
  separate "friend of my agent" mechanism.
- A grant is **revocable and tombstoned** (`CHAT-PERMISSIONS.md` §6.6), so "un-share my agent" is an
  auditable write, not an edit of history.

So the `personal` reach column is: **owner OR a live grant** — and a cross-owner delivery with neither is
refused (§5).

### 3.4 Who is accountable for a `service` agent

A `service` agent has no single human owner (§2.3). Accountability still needs a name, and the resolution is
a **decision** (AC2, §6): the namespace's `owner`-role holder is the **accountable principal of record** for
a `service` agent, **without gaining reach to it**. That is stated as accountability, not authority: the
role-holder can administer (declare/inspect/change scopes, `CHAT-PERMISSIONS.md` §4.3) by virtue of the
`admin` action, and that administration is audited, but the reach decision belongs to the scope, not to the
role.

---

## 4. The OWNS/SCOPE relation (service agents)

### 4.1 What a Scope IS

A **Scope** is an explicit, inspectable object naming a thing an agent is responsible for, and who may reach
that agent BECAUSE of it. It is **never implicit**: there is no "it owns a repo" folklore, no reach inferred
from a capability string, and no scope that cannot be enumerated by a read.

```json
{
  "id": "scope_repo_crier",
  "kind": "scope",
  "holder": "deploy-bot",
  "resource": {"kind": "repo", "ref": "github.com/coding-hermes/crier"},
  "reach": {"principals": ["prin_bane", "prin_ana"], "groups": ["team:infra"], "capabilities": []},
  "declared_by": "prin_01J9Z6V0Q7",
  "declared_at": "2026-10-03T17:45:05Z",
  "namespace": "acme"
}
```

| Field | Rule |
|---|---|
| `id` | the scope's own id; stable, referenced by audit lines |
| `kind` | `scope` — the record kind, so an id is never guessable as another object |
| `holder` | an agent id. The holder MUST be `class: service` — a `personal` agent may not hold a scope (§2.2, AC9) |
| `resource.kind` | a **closed set**: `repo` \| `box` \| `service` \| `dataset` \| `namespace`. An unknown kind is `400`, naming the accepted set — never stored as opaque prose (an inspectable object with a free-text "what" is not inspectable) |
| `resource.ref` | the canonical locator for that kind (a repo URL, a hostname, a service name, a dataset id, a namespace name). Carried verbatim; **crier never dereferences it** — the same rule an A2A `url` part follows |
| `reach` | the principals, named groups and capabilities that may reach the holder. `reach` is the ONLY thing that makes a service agent reachable (§2.3) |
| `declared_by` / `declared_at` | the principal and instant of the declaration. **Never `system`**: a machine did not decide who may reach a box |
| `namespace` | the realm the holder lives in; a scope never bridges two realms |

**Scope is OWNS + REACH.** `resource` is the OWNS half (what the agent is responsible for); `reach` is the
authority half (who may reach it because of that). The two are one object on purpose: a resource without a
reach set is folklore, and a reach set without a resource is a grant wearing a scope's clothes.

### 4.2 The resource kinds (the OWNS half)

| `resource.kind` | `ref` is | Example |
|---|---|---|
| `repo` | a repository locator (URL or canonical path) | `github.com/coding-hermes/crier` |
| `box` | a hostname or host id | `dedi-2` |
| `service` | a service name | `deploy-bot` |
| `dataset` | a dataset id | `ds_01J9…` |
| `namespace` | a namespace (realm) name | `acme` |

The set is **closed and extensible only by a spec change**: an unknown kind is refused, never stored as a
free-form string, so the OWNS relation stays a queryable relation rather than prose.

### 4.3 What owning a resource ENTITLES (the REACH half)

Owning a resource entitles the holder to exactly three things, and no more:

1. **Being the address for that resource's traffic.** `@deploy-bot` is how the repo's traffic is addressed;
   a capability the holder advertises (`@cap:deploy`) resolves to it like any other holder.
2. **A reach set.** The principals in `reach.principals` may deliver to the holder without an individual
   grant each — this is the "or its declaration as a service agent whose scope includes me" half of THE
   RULE. `reach.groups` / `reach.capabilities` are the set-based forms (§4.4).
3. **Being inspectable.** The scopes of an agent are enumerable by a read (§4.5).

It does **NOT** entitle the holder to: reach other agents, read other agents' inboxes, act on the resource
through crier (crier carries messages; it does not perform the deploy), widen its own `reach` (that is a
declaration the holder's `admin` must make, §4.5), or confer any action on a principal merely because that
principal is listed in `reach` (AC10).

### 4.4 `reach` — the sets, and what resolves TODAY

`reach` carries three sets:

| Set | Means | Resolution TODAY |
|---|---|---|
| `principals` | a principal-directory id may reach the holder | **SHIPPED** — the ACL matches a principal id directly |
| `groups` | members of the named curated group (`team:x`) may reach the holder | **NOT BUILT** — no group object/roster exists; owed by CR-CHAT-013/CR-CHAT-022 |
| `capabilities` | holders of the named capability may reach the holder | **NOT BUILT** as a reach set — capability membership is a registry index (`GET /agents?capability=`), not a resolved set the ACL reads today |

The shipped ACL resolves **only** `reach.principals`; the two set-valued forms are a contract with no object
behind them yet. That is stated in the code as well as here, so it cannot be silently assumed.

### 4.5 Declaring, inspecting and changing a scope

| Operation | Who may | Refusal | Status |
|---|---|---|---|
| declare | a principal with the `admin` action on the holder, in the holder's namespace | `403 DELIVERY_FORBIDDEN` | **NOT BUILT** |
| inspect | the holder's namespace (`read` role and above); any principal in `reach` sees the scope that includes them | `403` on a foreign namespace (`NAMESPACE_MISMATCH`, shipped) | **NOT BUILT** |
| change `reach` (add/remove a principal, group or capability) | as declare | as declare; the CHANGE is audited (`scope.reach.changed`) | **NOT BUILT** |
| delete | as declare; refused while the holder's deliveries reference it | `409`, naming the holder | **NOT BUILT** |

Every one of the four writes an audit line (§4.6). A `reach` widening is a **permission change** and is
treated as one: it is the only way a `service` agent becomes newly reachable, and an unaudited reach change
would be indistinguishable from an attack.

**NOT BUILT:** there is no scope record on the wire, no route, no `GET /agents/{id}/scopes`, no
`scope.declared` / `scope.reach.changed` audit event, and no scope-declaration route. The shipped
`permissions.Scope` carries only the reach-set half (`{id, reach}`).

### 4.6 The audit vocabulary a scope owes

| Event | Written when | `Details` |
|---|---|---|
| `scope.declared` | a scope is declared | the scope id, the holder, the resource kind/ref |
| `scope.reach.changed` | a scope's `reach` is widened or narrowed | the scope id + the adds/removes |
| `scope.deleted` | a scope is deleted | the scope id, the holder |

All three are **NOT BUILT**; they are owed with §4.5's routes. An unwritten reach widening is a silent
permission change, which is exactly what T6 leaves open today.

---

## 5. The default-reachability matrix

### 5.1 The matrix (normative)

Rows are the claimant; columns are the target class. "legacy" = the unclassed posture (§2.6).

| Claimant | `personal` target | `service` target | unclassed target |
|---|---|---|---|
| **its owner** | **ALLOW** (`OWNER`) | n/a — a service agent has no owner | legacy |
| a principal with a **live grant** on the target | ALLOW (`GRANT`) | ALLOW (`GRANT`) | legacy |
| a principal in a **scope `reach`**: `principals` | n/a — a personal agent holds no scope | **ALLOW** (`SCOPE`) | legacy |
| a principal in a **scope `reach`**: `groups` / `capabilities` | n/a | **ALLOW** once resolved (§4.4 — NOT BUILT) | legacy |
| a `admin`/`owner` role-holder **in the same realm** | **DENY** (T2 closed) | ALLOW **only** when the agent has no scope and no grant (`ADMIN`) | legacy |
| a **different owner** / any stranger | **DENY** (`NO_GRANT`) | **DENY** (`NO_GRANT`) | legacy |
| an **anonymous** sender | **DENY** (`ANONYMOUS`) | **DENY** (`ANONYMOUS`) | ALLOW (`UNCLASSED`) |
| a **remote** shadow principal | as any principal (grants stay local) | as any principal | legacy |
| a **calling agent** (`X-Agent-ID`) | resolves to the caller's OWNER; refused if it has none | refused (`NO_GRANT`) — no owner to act as | legacy |

Three cells are the whole security story: **different owner × personal** is T1's cell (`DENY`), **stranger ×
service** is T6's cell (`DENY` unless a scope says so), and **admin-in-realm × personal** is T2's cell
(`DENY` — a namespace role is not a reach badge).

### 5.2 The rule, executable

This is the whole rule; everything else in this document is machinery around it, and it is the predicate the
shipped checker implements (`internal/permissions/checker.go`):

```
may_deliver(principal P, agent A, action "send"):
  if A.class is absent            -> ALLOW  (UNCLASSED: unchanged shipped posture; §2.6, §8.1)
  if A.class == personal:
      allow  iff  P == A.owner                    # the owner, §3.2
              or   a live grant (P, A, send)      # granted by the owner, §3.3
      deny   otherwise                            # including every other owner's agent
  if A.class == service:
      allow  iff  some live scope S of A has S.reach ∋ P     # §4.1, §4.4
              or   a live grant (P, A, send)
              or   (no scope and no grant) and P holds admin/owner in A's realm
      deny   otherwise
```

`live` is `CHAT-PERMISSIONS.md` §6.6: a revoked or expired grant is treated as absent, evaluated **per
request**, never cached.

### 5.3 Worked examples

**T-1 — My agent: reachable by me, refused to a stranger.**

```
principal  prin_bane   owns  agent atlas  class=personal
agent      atlas       class=personal  owner=prin_bane

POST /agents/atlas/inbox   as prin_bane      -> 201 accepted        (OWNER)
POST /agents/atlas/inbox   as prin_stranger  -> 403 DELIVERY_FORBIDDEN reason=NO_GRANT
                                                detail: "… class personal, owner prin_bane"
```

**T-2 — Your agent: refused to me without a grant.**

```
agent  quill  class=personal  owner=prin_other

POST /agents/quill/inbox as prin_bane            -> 403 DELIVERY_FORBIDDEN reason=NO_GRANT
                                                    detail: "… class personal, owner prin_other"
# after the owner grants prin_bane send on quill:
POST /agents/quill/inbox as prin_bane            -> 201 accepted    (GRANT)
```

The middle line is THE RULE, executed: I may reach MY agent, and I am refused YOURS.

**T-3 — Service agent with scope: reachable per scope.**

```
agent  deploy-bot  class=service  owner=null
scope  scope_repo_crier  resource={kind:repo, ref:github.com/coding-hermes/crier}
       reach={principals:[prin_leo], groups:[team:infra], capabilities:[]}
       declared_by=prin_bane

POST /agents/deploy-bot/inbox as prin_leo   -> 201  (SCOPE)
POST /agents/deploy-bot/inbox as prin_sam   -> 403 DELIVERY_FORBIDDEN reason=NO_GRANT
GET  /agents/deploy-bot/scopes              -> 200  {scopes:[scope_repo_crier]}   # NOT BUILT
```

And the honest negative: `deploy-bot` is reachable **because a scope says so**, not because it is a
"service agent" — a service agent with **no scope and no grant** is reachable by `admin`/`owner` only (§2.3).

### 5.4 The refusal

Cross-owner denial reuses the ONE authorization code `CHAT-PERMISSIONS.md` §6.7 fixes — this document
introduces no new code:

| Situation | Status + code |
|---|---|
| no live grant / not the owner / not in any scope `reach` | `403 DELIVERY_FORBIDDEN` |
| a bound principal delivering as an agent it is not bound to | `403 DELIVERY_FORBIDDEN`, `"reason":"NO_BINDING"` |
| an anonymous sender on a classed target | `403 DELIVERY_FORBIDDEN`, `"principal":"anonymous"` |
| target in another namespace | `403 NAMESPACE_MISMATCH` (**shipped** — never re-spelled) |

The body is machine-readable and names the decision inputs so a UI can explain itself without parsing prose:

```json
{"error":"DELIVERY_FORBIDDEN",
 "reason":"NO_GRANT",
 "principal":"prin_bane",
 "as_agent":"atlas",
 "target":{"type":"agent","ref":"quill"},
 "action":"send",
 "detail":"no live grant for principal prin_bane on agent quill (class personal, owner prin_other)"}
```

---

## 6. Decisions (this document's ledger)

This document keeps its own decisions ledger, id-prefixed **AC** so it cannot collide with the series'
**D1–D16** ledger owned by `CHAT-INTERFACE.md` and its companions. Each decision names the alternative it
beat.

| # | Decision | Rejected alternative, and why |
|---|---|---|
| **AC1** | **Two classes, `personal` \| `service`, and no third.** The class is a row property (§2.4). | *A boolean `shared` flag.* Rejected: a boolean cannot express "owned by one human with a private context" vs "scoped, owns systems", and it has no place to hang the owner. *A free-form `kind` label.* Rejected: a taxonomy the ACL cannot switch on is prose. |
| **AC2** | **`owner` is a PRINCIPAL id, exactly one, REQUIRED for `personal` and REFUSED for `service`.** Accountability for a `service` agent is the namespace `owner`-role holder, without reach (§3.4). | *An agent may own an agent.* Rejected: an agent is not a grantee (`CHAT-PERMISSIONS.md` §6.1); a second reach path for machines is a second truth. *A personal agent with many owners.* Rejected: bindings are speech rights and may be many; accountability has one name. *`owner` optional on `personal`.* Rejected: a class that cannot answer "who is accountable" is not a class. |
| **AC3** | **A Scope is an explicit, inspectable object with a CLOSED resource-kind set** (`repo`/`box`/`service`/`dataset`/`namespace`), never free text (§4.1). | *"It owns a repo" as prose on the agent row.* Rejected: not queryable, not inspectable, and T6's exact hole. *Infer ownership from the capabilities an agent advertises.* Rejected: `@cap:deploy` says what an agent CAN do, not what it is RESPONSIBLE for, and capability membership is dynamic (a pool rotates) while ownership is a declaration. |
| **AC4** | **Cross-owner is DEFAULT-DENY.** No live grant, no ownership rule and no scope `reach` ⇒ refuse, with `403 DELIVERY_FORBIDDEN` (§5). | *Trust-by-reach (today's posture).* Rejected: fine for a single-operator fleet, hostile the moment two humans share a bus. *Namespace membership as blanket permission.* Rejected: a realm is an administrative domain, not a reach badge (T2). |
| **AC5** | **A `service` agent's reach is the union of its scope `reach` sets (+ grants); it is NEVER "everyone", and never mediated by an owner** (§2.3). | *Reach every principal in the namespace.* Rejected: it makes declaring a `service` class equivalent to publishing an inbox. *Reach via the owning human.* Rejected: a service agent HAS no owning human (§2.3), and an owner-mediated reach would re-import T1 through the back door. |
| **AC6** | **The class is IMMUTABLE by `PATCH`** — changing it is a re-registration (§2.4). | *A mutable `class`.* Rejected: flipping personal→service silently re-writes who may reach the agent — a permission change disguised as a field edit. The precedent is `NAMESPACES.md` §5's realm move. |
| **AC7** | **An ABSENT class means UNCHANGED shipped behaviour** (the ACL has nothing to evaluate and allows), and it is stated as a residual, not glossed (§2.6, §8.1). | *Default every unclassed row to `service`.* Rejected: it would silently change reach for every pre-existing agent on upgrade. *Refuse an unclassed registration.* Rejected: it breaks every existing client and contradicts the additive-only invariant (`CHAT-INTERFACE.md` §5.2). |
| **AC8** | **A scope `reach` is the only thing that makes a `service` agent newly reachable, and every `reach` change is AUDITED** (§4.5). | *Reach inferred from capability advertisement.* Rejected: advertising a capability would then widen reach as a side effect of a routine registration. *Silent reach edits.* Rejected: an unaudited reach change is indistinguishable from an attack. |
| **AC9** | **A `personal` agent may NOT hold a scope** (§2.2, §4.1). | *Let a personal agent own a repo with a reach set.* Rejected: it contradicts the personal contract ("my owner and nobody else") and would give one agent two contradictory reach stories. A personal agent that must serve a resource is re-registered `service`. |
| **AC10** | **OWNS confers REACH only — never a grant of actions on the resource or on other agents** (§4.3). | *Treat ownership as an implicit admin/grant on the resource.* Rejected: crier carries messages; it does not act on a repo or a box, so "ownership" must not imply an action it cannot perform. Reach is the only entitlement, and it is enumerable. |
| **AC11** | **A scope's `reach.groups` / `reach.capabilities` are declared but resolve only when a group/capability object exists; today only `reach.principals` resolves, and that is stated in the spec and the code** (§4.4). | *Block scopes until groups exist.* Rejected: it withholds the principal-directory half that works today. *Silently ignore the unresolvable sets.* Rejected: a declared-but-inert reach entry is a security-relevant surprise and is named instead. |

---

## 7. Acceptance criteria & test cases

The taxonomy is judged by executable tests, not by a table. Each case below names the claimant, the target,
the expected status, and the shipped evidence that already exercises it where the ACL is built.

### 7.1 Acceptance criteria (normative)

| # | Criterion | Expected result | Shipped evidence (ACL armed) |
|---|---|---|---|
| **T-1** | **My agent** — the owner delivers to a `personal` agent it owns | `201` accepted, reason `OWNER` | `internal/permissions/checker_test.go TestPersonalOwnerAllowed`; `internal/registry/permissions_test.go TestOwnerMayReachOwnAgent` |
| **T-1b** | **My agent, refused to a stranger** — a principal with no grant delivers to someone else's `personal` agent | `403 DELIVERY_FORBIDDEN`, `reason=NO_GRANT`, detail naming the class and owner | `internal/permissions/checker_test.go TestPersonalStrangerRefusedNoGrant`; `internal/registry/permissions_test.go TestDeliverRefusedWithoutGrantNamedError` |
| **T-2** | **Your agent** — I am refused YOUR agent without a grant, and admitted once the owner grants me `send` | `403 NO_GRANT` → `201` after the grant | `internal/permissions/checker_test.go TestPersonalGrantedSenderAllowed`; `TestRevokedGrantStopsWorking` |
| **T-3** | **Service agent with scope** — a scope-reach principal is admitted, a non-reach principal is refused, and the scope is an explicit inspectable object | `201` (`SCOPE`) / `403 NO_GRANT` / `GET /agents/{id}/scopes -> 200` | `internal/permissions/checker_test.go TestServiceScopeReachAllowed`; the inspectable read is **NOT BUILT** (§8.1 item 3) |
| **T-4** | **A service agent with no scope and no grant** is reachable by a same-realm `admin`/`owner` only, and by nobody else | `201` for the role-holder, `403` for a stranger | `internal/permissions/checker_test.go TestServiceNoScopeIsAdminOnlyInRealm` |
| **T-5** | **A namespace `admin` cannot reach another owner's `personal` agent** (T2 closed) | `403 NO_GRANT` | `internal/permissions/checker_test.go TestAdminCannotReachAnotherOwnersPersonalAgent` |
| **T-6** | **An unclassed target keeps the legacy posture** (the residual) | `201` (reason `UNCLASSED`) | `internal/permissions/checker_test.go TestUnclassedTargetIsLegacyPosture`; `internal/registry/permissions_test.go TestUnclassedTargetKeepsLegacyPosture` |
| **T-7** | **The ACL is additive** — with no checker wired, the delivery path is byte-identical to pre-CR-CHAT-003 | accepted, no behavior change | `internal/registry/permissions_test.go TestNoCheckerIsByteIdenticalLegacy`; `internal/permissions/checker_test.go TestUnarmedCheckerAllowsEverything` |
| **T-8** | **Class/owner coherence** — a `personal` row without an owner, or a `service` row with one, is refused | validation error naming the field | `internal/permissions/permissions_test.go TestAgentClassOwnerCoherence` |
| **T-9** | **The refusal body shape** is the §5.4 machine-readable body | exact JSON shape | `internal/permissions/permissions_test.go TestRefusalBodyShape` |

**Owed tests (NOT BUILT — §8.1):** T-3's inspectable scope read; T-3's `reach.groups` / `reach.capabilities`
resolution; the registry-row `class`/`owner` round-trip on both stores (registration, `GET /agents`, `PATCH`
refusal); the `scope.declared` / `scope.reach.changed` / `scope.deleted` audit lines.

### 7.2 What "the class model is normative" means for a worker

A worker implementing any row that touches classes MUST, in the same change:

1. keep `class`/`owner` `omitempty` so an unclassed row stays byte-identical (§2.4);
2. validate the coherence rule at registration, naming the field on refusal (§2.4);
3. refuse a `PATCH` that changes the class (§2.4/AC6);
4. make the ACL read `permissions.AgentInfo` (`internal/permissions/types.go`) — the single shape — so the
   lookup source can move from the interim `permission.agent` record to the registry row **without** an ACL
   change (§2.5);
5. not add a second reach path: ownership and scope are the only class-owned reach inputs; everything else
   is a grant (`CHAT-PERMISSIONS.md`).

---

## 8. NOT BUILT, and the open questions

### 8.1 NOT BUILT (the ledger)

1. **The registry-row `class`/`owner` (§2.4).** `internal/registry/types.go`'s `Agent` has neither field;
   registration accepts neither; `GET /agents` returns neither; `PATCH` cannot be refused for a class
   change because there is no class. Until this ships, the class lives in a `permission.agent` record in the
   permission store (§2.5) — an interim the ACL reads through one shape.
2. **`GET /agents?class=` filter.** No filter; the read surface is the row's own field once (1) ships.
3. **The full Scope object (§4.1) and its routes (§4.5).** The shipped `permissions.Scope` carries only
   `{id, reach}`. Missing: `resource {kind, ref}`, `declared_by`/`declared_at`, `namespace`; the closed
   kind validation; the declare / inspect / change / delete routes; `GET /agents/{id}/scopes`.
4. **The scope `reach` set resolution for `groups` and `capabilities` (§4.4).** Only `reach.principals`
   resolves today. `groups` is owed by CR-CHAT-013/CR-CHAT-022; `capabilities` needs a resolved set the ACL
   can read.
5. **The scope audit events (§4.6).** No `scope.declared`, `scope.reach.changed` or `scope.deleted` line is
   written anywhere.
6. **The PRINCIPAL side (§3.3).** No principal store, no binding store, no login and no grant route exists
   on the HTTP surface; the ACL reads a JSONL/PostgreSQL store an operator seeds. `CHAT-PERMISSIONS.md`
   §8.1 items 1–9 own the principal/grant ledger; this document claims none of it.
7. **The `owner`-role account of record for a `service` agent (§3.4)** is a proposal (AC2), not a stored
   field.

### 8.2 Open questions

1. **Where does the class live on the wire for a read?** §2.4 fixes `GET /agents`/`GET /agents/{id}` returning
   `class`/`owner`. Whether an unclassed row omits them (the `omitempty` discipline, §2.4) or returns them as
   `null` is resolved as **omit** — consistent with `namespace`/`a2a` — and is not re-opened here.
   **PROPOSED-DEFAULT:** omit.
2. **Does a widened `reach` take effect on the next delivery only?** The effective-permission computation is
   per request (`CHAT-PERMISSIONS.md` §6.6), so a `reach` change is visible on the next delivery, never
   cached. **PROPOSED-DEFAULT:** per-request, matching grants.
3. **May a `service` agent hold a scope whose resource is in another realm?** §4.1 says a scope never bridges
   two realms; a resource located in another realm is a **declaration about a foreign thing**, and the
   holder's realm is what the ACL compares. **PROPOSED-DEFAULT:** the scope is realm-scoped to its holder;
   a foreign `resource.ref` is carried verbatim and confers no cross-realm reach.
4. **Is `namespace` an OWNS kind or a realm?** §4.2 lists `namespace` as a resource kind an agent may own
   (e.g. a namespace-administration agent), which is distinct from the realm wall. **PROPOSED-DEFAULT:** keep
   it as a resource kind; the realm wall stays `specs/NAMESPACES.md`.
5. **Who may delete a scope when its holder is unregistered?** §4.5 refuses a delete "while the holder's
   deliveries reference it". The unregister case is not decided. **PROPOSED-DEFAULT:** unregistering a
   `service` holder refuses while a scope names it, and the refusal names the scope.

---

## 9. Status line

`DRAFT v1 · 2026-10-06 · CR-CHAT-012`

This document is the spec of record for the **agent taxonomy and ownership model**: the two classes
(`personal` | `service`), the OWNER relation, the OWNS/SCOPE relation over resources, and the
default-reachability matrix with cross-owner default-deny.

**Built and reusable today:** the `internal/permissions` package (classes, owner, the reach-set half of a
scope, the `may_deliver` predicate), the two delivery-ACL insertion points in `internal/registry/permissions.go`,
the boot wiring (`cmd/server/permissions.go`, `CR_PERMISSIONS_ENABLED` / `CR_PERMISSIONS_DIR`, default OFF),
and the class/owner coherence validation — all covered by the tests §7.1 names.

**Not built, and not to be claimed in `docs/claims.yaml` until they exist:** the `class`/`owner` fields on
the registry row and their registration/read/`PATCH`-refusal contract; the full Scope object, its closed
resource-kind set and its declare/inspect/change/delete routes; `GET /agents/{id}/scopes`; the
`scope.declared` / `scope.reach.changed` / `scope.deleted` audit lines; and the `reach.groups` /
`reach.capabilities` resolution (§8.1).

Where this document and `specs/CHAT-PERMISSIONS.md` overlap (§3/§4 there), they state the same rules for the
taxonomy; the shipped code wins on any disagreement and the drift is filed as a `DF-CRIER-*` row.
