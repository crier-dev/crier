# CHAT-FEDERATION.md — external namespaces: peer instances, remote participants, and per-peer policy

Status: **DRAFT v1** · 2026-10-03 · **CR-CHAT-023** + **CR-CHAT-024**
Source material: Bane, 2026-10-03, verbatim: *"external namespaces think about external servers of slack
allowing different slacks to intermix I don't nor does my agent always want to have to login to more
panels so federation between careers [criers] with an auth processes."*; the board rows CR-CHAT-023 and
CR-CHAT-024 (read in full); and the shipped code at this worktree's HEAD — the route registration and
`fedClient` wiring in `cmd/server/main.go`, `internal/federation/` (`federation.go`, `hold.go`,
`holdfile.go`), `config/config.go` (`FederationConfig`), `internal/middleware/auth.go`,
`internal/registry/handler.go` (the deliver path's federation fallback) and
`internal/registry/federation_failure.go`.

This document is the normative design authority for how ONE crier instance relates to ANOTHER: what a
peer is, how a link is authenticated and authorized, what may cross the boundary, and how a member of a
foreign instance becomes a participant in a local session. Where it and the shipped code disagree, the
**code wins** — file the drift as a board row (`DF-CRIER-*`) rather than rewriting either side. Sections
that describe behaviour that does not exist yet are marked **NOT BUILT** and must not be added to
`docs/claims.yaml` (claims execute against a live server).

Cross-references: **the delivery path itself is not restated here** — links, forwarding, the hold/retry
contract and the `FEDERATION_FAILED` receipt are `specs/WEBHOOK-DELIVERY.md` §8–§8.1 (CR-FEAT-006) and
the shipped code, and this document adds nothing to them. The local permission model this must mesh with
is `specs/CHAT-PERMISSIONS.md` (CR-CHAT-003 + CR-CHAT-007 + CR-CHAT-012) — principals, roles, grants and
the delivery ACL, **reused, never replaced**. Sessions and membership are `specs/CHAT-SESSIONS.md`
(CR-CHAT-002 + CR-CHAT-005); the local realm wall is `specs/NAMESPACES.md` (CR-FEAT-029); the delivery
log and its verdicts are `specs/DETECTION.md` (CR-FEAT-030); the trust-chain internals (vouching,
rotation, revocation semantics) are the sibling `specs/CHAT-TRUST.md` (CR-CHAT-024 + CR-CHAT-025). Assets
(S3) are CR-CHAT-014 — a **pointer only** here, §8.2. Audit surfaces are CR-CHAT-021, §8.3.

**Glossary (normative; the exact terms this document uses).** An **Instance** is one deployment: one
crier server, one store, one registry, one set of namespaces. A **Peer** is another instance, known to
this one by an identity (§4) — not merely a URL. A **Link** is the configured, directed relationship
between this instance and a peer (§4.2). An **External namespace** is a scope that belongs to a peer and
is admitted, by local policy, to participate in this instance's sessions (§3). A **Remote participant** is
a principal or agent that lives on a peer and appears as a participant of a local session (§3, §9). A
**Shadow principal** is the LOCAL record minted for a remote participant so that grants, permission
checks and audit stay local (§9, decision D14). A **Peer policy** is the per-peer rule about which
namespaces, agents and directions are permitted (§6). The **Degraded posture** is the shipped shared-secret
link auth, kept as the default (§2, §5.3).

---

## 1. Purpose & scope

### 1.1 What this document IS normative for

1. **The peer model** — what a peer IS, how a link is established, and the direction(s) a link carries
   (§4).
2. **External namespaces** — the Slack-Connect shape: a member of another instance participating in a
   LOCAL session as a participant, and the per-peer policy that governs what may cross (§3, §6).
3. **Authentication of a peer** — mutual, at link setup, by KEY rather than by a shared password, with
   the shipped `CR_FED_TOKEN` path kept as the degraded default posture (§5).
4. **Authorization per peer** — which namespaces and which agents a peer may reach, in which direction,
   and the NAMED refusal when a policy declines (§6).
5. **Revocation of ONE peer** without disturbing the others, and what happens to in-flight held
   deliveries (§7).
6. **What crosses the boundary and what does not** — the message body versus a reference, the asset rule
   (pointer), and the audit naming of a remote actor (§8).
7. **The remote-participant model** as a LOCAL shadow principal marked `remote` (decision D14, §9), so
   grants and permission checks stay local and uniform.

### 1.2 What this document is NOT normative for

- **The delivery path.** Links, forwarding to a peer, the hop marker, the hold/retry budget, the durable
  hold queue and the terminal `FEDERATION_FAILED` receipt are `specs/WEBHOOK-DELIVERY.md` §8–§8.1. The
  transport is not the problem this document solves and it is not changed here.
- **The local permission model.** Principals, roles, grants, the action set and the delivery ACL are
  `specs/CHAT-PERMISSIONS.md`. A remote participant is evaluated by that model, unchanged (§9); this
  document introduces no second permission engine.
- **The trust chain.** Vouching, key rotation and the meaning of a revoked-but-chained record are
  `specs/CHAT-TRUST.md`. This document consumes a peer's key as a trust anchor; it does not define how a
  key comes to be trusted (§5.5, §8 of CHAT-TRUST.md).
- **Assets.** S3-backed files and asset storage are CR-CHAT-014. This document states only whether asset
  BYTES cross the boundary (§8.2).
- **The realm model.** A local namespace is `specs/NAMESPACES.md`. An external namespace is a DIFFERENT
  kind of scope and is never implemented by reusing a local realm (§3.3).

### 1.3 The standing constraint (extend, never replace)

The same binding constraint the A2A option carries, restated here because federation is the surface most
likely to be tempted out of it:

> **The new path is ADDITIVE and DEFAULT-OFF. The shipped `CR_FED_TOKEN` path stays as the degraded
> default posture, and an instance must be able to run with the token OFF.** No existing route's
> behaviour, status code, body shape or auth requirement may change because keyed peers exist.

Concretely: with no peer records configured, an instance federates exactly as it does today — the same
`CR_FED_LINKS`, the same optional `CR_FED_TOKEN`, the same `GET /fed/peers`, the same hold/retry and the
same `FEDERATION_FAILED` receipt.

---

## 2. The shipped baseline — and the measured problem

### 2.1 What federation does today (grounded, per the code)

| Surface | Shipped behaviour | Where |
|---|---|---|
| Link configuration | `CR_FED_LINKS` — comma-separated base URLs; an invalid entry is skipped | `config/config.go`, `federation.NewClient` |
| Local display name | `CR_FED_NAME`, else `localhost:<port>` | `cmd/server/main.go` (`federationName`) |
| Discovery | `GET /fed/peers` — the local relay first, then each linked relay's agents fetched live from its `GET /agents`; only `id` and `capabilities` cross | `federation.HandlePeers` |
| Forwarding | A delivery to an agent unknown locally is POSTed to each link as `<link>/agents/{id}/inbox`, carrying the exact original deliver JSON plus `X-Crier-Fed-Hop: 1`; the first non-404, non-retryable answer is relayed back verbatim | `internal/registry/handler.go` deliver path |
| Loop prevention | `X-Crier-Fed-Hop` — a request that already arrived over a link never forwards again | `federation.HopHeader` |
| Hold/retry | A transient failure (unreachable link, 5xx/408/429) is held and retried with 2s-doubling backoff (cap 60s) inside `CR_FED_MAX_HOLD_S` (default 300); durable when `CR_FED_QUEUE_FILE` is set | `internal/federation/hold.go`, `holdfile.go` |
| Terminal outcome | One durable `FEDERATION_FAILED` receipt in the **sender's** own inbox; a synchronous `502 FEDERATION_FAILED` when no queue is available, the queue refuses, or the body names no `sender` | `internal/registry/federation_failure.go` |
| Detection | Verdicts `federation_held` / `federation_failed`, transport `federation` | `internal/detect`, `internal/registry/detection.go` |
| Client-side store | `registry.RemoteStore` — a `Store` implementation that talks to a running crier over HTTP (the bridge/crier-mcp store). It is a proxy for ONE agent's own credentials, **not** a peer identity | `internal/registry/remote.go` |

### 2.2 The measured problem — ONE shared secret, and what that costs

Federation auth today is a single optional secret. `FederationConfig.Token`'s own doc comment states the
contract exactly: *"When set, every request this relay sends to a linked relay carries
`Authorization: Bearer ***`, so a remote relay protecting itself with `CR_AUTH_TOKEN` accepts the
forward. The linked relay must share the value: **source `CR_FED_TOKEN` == destination `CR_AUTH_TOKEN`**."*
The destination validates it with the deployment's ordinary Bearer middleware — one secret for the whole
deployment (`internal/middleware/auth.go`, constant-time compare against `CR_AUTH_TOKEN`).

That is not "weak peer auth"; it is **no peer identity at all**, and the four consequences are the
measured problem this document exists to close:

1. **Peers are indistinguishable.** No peer id travels on a forward — the hop header is a boolean
   marker, and the Bearer value is the same for every peer. The destination cannot tell peer A from peer
   B, or from any other holder of the token. A forward can be attributed to *the federation* and to no
   member of it.
2. **No per-peer rights.** There is no object to hang a policy on, so every peer reaches exactly what
   every other peer reaches (whatever the local ACL allows, and for an unclassed agent that is
   trust-by-reach — CHAT-PERMISSIONS.md §1.1).
3. **No individual revocation.** Revoking a leaked credential means rotating `CR_AUTH_TOKEN`, which is
   the deployment's own gate for every route — so the only revocation available is a coordinated
   deployment-wide rotation that disturbs every peer and every local client at once.
4. **One leak is total.** The secret is symmetric and shared: any peer that legitimately holds it can
   act as any other peer, and a leak is a leak of the deployment gate itself. There is no forward
   secrecy and no way to scope the damage after the fact.

**The shipped token path is therefore the degraded default posture (§1.3): kept, not removed.** It stays
correct for a fleet of instances that already share one operator, and it is exactly what a keyed peer
replaces for a boundary that is not one trust domain.

### 2.3 What the baseline does NOT have

Stated as the gap this document designs into, not as a slur on the shipped feature:

- no **peer identity** — a link is a URL plus a shared secret (§4);
- no **mutual authentication** — only the source proves anything, and only to the deployment gate (§5);
- no **per-peer policy** — no namespace, agent or direction scoping (§6);
- no **revocation** of one peer (§7);
- no **remote participant** — a message can be forwarded to an agent on a peer, but nothing on the peer
  can appear as a participant of a LOCAL session (§3, §9);
- no **boundary rule** about what crosses — today the whole payload crosses (§8).

---

## 3. External namespaces — the Slack-Connect shape

### 3.1 The definition

An **external namespace** is a scope that belongs to a peer instance and is admitted, by LOCAL policy, to
participate in this instance's sessions. It is not a local realm:

- a local realm (`specs/NAMESPACES.md`) is an administrative domain of THIS deployment: its agents are
  this instance's rows, and a delivery into it is a local delivery;
- an external namespace is a peer's scope: its members are not local rows, and nothing is delivered
  "into" it by local addressing.

The consequence, and the reason the distinction is normative: **a local `@ns/*` address never resolves
into an external namespace, and an external namespace name never resolves to a local realm.** Admitting a
peer's scope must not silently make that peer's members addressable as if they were local; that is the
reach-widening the whole boundary exists to prevent.

### 3.2 A member of another instance as a remote participant

The shape Bane is pointing at is Slack Connect: two organisations share a room, and **each side's members
are real participants with their own permissions**. Translated:

1. A session on instance A has local participants (principals + agents, CHAT-SESSIONS.md).
2. By local policy (§6), instance A admits a peer P and some of the scopes P contains.
3. A principal or agent on P then appears in a session on A as a **remote participant**.
4. That participant reaches exactly what A's policy and A's own ACL allow it to reach (§6.2, §9) — its
   permissions are decided on A, never claimed by P.

### 3.3 The boundary rule — resolve locally, never believe the claim

This is the same discipline `specs/NAMESPACES.md` §5 applies to a realm: *a claim that disagrees is
checked, never believed*. Here it is stated as the rule:

> **Every fact about a remote participant is resolved on the LOCAL instance.** Which peer it belongs to,
> which external namespace it comes from, what it may reach, and whether it exists at all are all decided
> from local state (the peer record, the peer policy, the shadow principal). A value the peer asserts in a
> payload — an agent id, a namespace name, a role, a sender — is DATA, never authority.

Three normative consequences:

- A peer cannot widen its own reach by asserting a role or a namespace in a message (the remote
  equivalent of the impersonation hole CHAT-PERMISSIONS.md §1.1 names).
- A peer that names a local agent in a forward reaches it only if the peer policy admits that agent AND
  the local ACL admits the shadow principal (§6.2) — two checks, one decision.
- The remote instance's own permission model is never consulted for a decision on this instance; it is
  consulted only by its own operators, on its own side of the boundary.

### 3.4 What the shipped path already gives this (and what it does not)

The transport, the hold, the terminal receipt and the discovery *do* exist (§2.1) — the boundary can
reuse them wholesale, and §3.2's delivery of a message into a peer's member's inbox IS the shipped
forward. What does not exist: the peer identity to attach policy to, the per-peer policy itself, and any
notion of a remote participant in a local session. Everything below is that machinery, all of it
**NOT BUILT**.

---

## 4. The peer model

### 4.1 A peer is an instance identity

A **Peer** is another INSTANCE, known by an identity, not by a URL. A URL is an address that can be
repointed; an identity is what a policy and a revocation attach to. The normative record shape:

```json
{
  "id": "peer_acme",
  "kind": "peer",
  "name": "Acme",
  "url": "https://crier.acme.example",
  "key": {"alg": "ed25519", "key_id": "…", "public_key": "…"},
  "direction": "both",
  "state": "active",
  "linked_at": "2026-10-03T19:40:00Z",
  "revoked_at": null,
  "revoked_by": null
}
```

| Field | Rule |
|---|---|
| `id` | the local name for the peer — the handle a policy, an audit line and a hold-queue decision name. Local and stable; never derived from a remote claim. |
| `kind` | `peer` is part of the record, not decoration: an id is never guessable as "peer or principal or agent" by its shape (the same rule CHAT-PERMISSIONS.md §2.1 applies to `kind`). |
| `url` | the peer's base URL, used by the shipped forward. **Addressable, not authoritative** — changing it does not change the identity. |
| `key` | the peer's instance key (§5) as this instance has ACCEPTED it. A peer may carry several accepted keys across a rotation (CHAT-TRUST.md §6); this document only fixes that the accepted set is what authentication is checked against. |
| `direction` | the directions this link carries (§4.3). |
| `state` | `proposed` \| `active` \| `revoked` (§4.4). |
| `linked_at` / `revoked_at` / `revoked_by` | the instants and the actor of the decision. `revoked_by` is a local principal or an operator action, never `system`: a machine did not decide to distrust an instance. |

### 4.2 The link

A **Link** is the directed relationship between this instance and a peer. A link exists in two halves,
one on each side, and **a link is `active` only when both halves exist**: A knowing B's key while B does
not know A's is a `proposed` link on A, and A's forwards are refused until B accepts (§5.2). That is what
makes the setup MUTUAL rather than a one-sided configuration.

Establishment (normative shape, NOT BUILT):

1. An operator declares the peer — `id`, `url`, expected key (or "accept the key presented at
   handshake", which is an explicit decision, not a default).
2. The handshake of §5.2 runs, in both directions.
3. Each side records the peer's accepted key and marks its half `active`.
4. Only then does a forward carry the peer's signature.

### 4.3 Direction

Direction is per link, and it is a closed set:

| Value | Meaning |
|---|---|
| `outbound` | this instance may forward deliveries TO the peer (today's `CR_FED_LINKS` behaviour) |
| `inbound` | the peer may forward deliveries INTO this instance |
| `both` | both of the above |

The shipped configuration is **outbound-only by construction**: `CR_FED_LINKS` says where this instance
forwards; nothing tracks who may forward INTO it. The shipped token does not change that either — the
token admits a forward, it does not name a peer or a direction, so an instance holding the deployment
token can forward anywhere the deployment token is accepted. Per-direction enforcement is part of the
peer policy (§6) and is **NOT BUILT**.

### 4.4 Link lifecycle

| State | Reached by | What it means |
|---|---|---|
| `proposed` | this side declared the peer, or a handshake is half-done | no forward is accepted; a forward is refused with `403 FED_PEER_UNTRUSTED` |
| `active` | both halves accepted the other's key | forwards are authenticated and checked against the peer policy |
| `revoked` | an operator revokes this peer (§7) | every credential of this peer is refused; the other peers are untouched |

A state change is an audit line (`peer.proposed`, `peer.linked`, `peer.revoked`) and is never a silent
edit — the same rule CHAT-PERMISSIONS.md §2.2 applies to a principal's lifecycle, for the same reason.

**NOT BUILT:** no peer record, no peer store, no link route, and no `/fed/*` route beyond the shipped
`GET /fed/peers`. There is no `CR_FED_PEERS`-style configuration document, no `direction` and no `state`
anywhere in the tree.

---

## 5. Authentication — mutual, by key

### 5.1 The instance key

Each instance holds ONE **instance key**: an ed25519 keypair that identifies the INSTANCE, presented at
link setup and on every forward to a keyed peer. Its properties mirror the shipped detect signing key
(`specs/DETECTION.md` §3.3) because that key already got these right:

- it is a FILE, created `0600` (`CR_FED_INSTANCE_KEY`, default alongside the deployment's config);
- it is **never regenerated silently** — an unreadable, non-hex or wrong-length key file is a startup
  error, because a per-boot key would make every handshake and every accepted peer unverifiable;
- it has a `key_id` (the hex prefix of the sha256 of the public key, the same convention `detect.Signer`
  uses) so a verifier can name which key signed, across a rotation.

**The instance key is not an agent key** (§5.5). One is the identity of a deployment; the other is the
identity of an agent row inside it.

### 5.2 Mutual auth at link setup (the handshake)

Normative shape (NOT BUILT): a handshake in which **each side proves possession of its instance key** and
both store the other's public key. Neither side presents a password; there is nothing symmetric to leak.

1. A sends its peer id, URL, `key_id`, a fresh nonce and a timestamp.
2. B signs `(A.nonce ‖ B.nonce ‖ A.id ‖ B.id ‖ ts)` with B's instance key and returns B's `key_id` and
   signature.
3. A verifies B's signature against the key B presented, then countersigns the same transcript with A's
   instance key.
4. B verifies A's signature. Both halves are now `active`.

Properties this fixes, by reference to the shipped signing shape (`internal/registry/agentsig.go`): the
signature covers a **method, a path and a timestamp**, plus the two nonces and the two peer ids here, so a
captured handshake cannot be replayed against another peer or after the window; and the window is bounded
the way the shipped agent-signature window is bounded (±30s), refused with a named outcome, never
processed with a warning.

### 5.3 The degraded default posture — `CR_FED_TOKEN`, kept

The shipped token path stays, with these rules (all **NOT BUILT** except the first):

1. **Unchanged when no peer records exist.** An instance with `CR_FED_LINKS` and `CR_FED_TOKEN` and no
   keyed peers federates exactly as it does today. This is the non-regression contract of §1.3.
2. **A keyed peer does not present the token.** When a peer has an accepted key, its forwards are
   authenticated by key; the token is not consulted for that peer.
3. **A token-only peer is still admitted while the operator allows it.** The instance may run with
   `CR_FED_REQUIRE_TOKEN` (proposed name) unset/true — today's posture, where the deployment bearer is
   required — or `false`, where a keyed peer needs no token. The token is NEVER an alternative to a
   key for a peer that HAS one: the two do not stack and cannot be substituted.
4. **The instance can run with the token OFF.** With `CR_FED_TOKEN` unset and only keyed peers
   configured, the federation path is key-only. This is the acceptance criterion *"the instance must be
   able to run with the token off"* (CR-CHAT-024) and is a configuration, not a code change to the
   shipped token path.
5. **A token-authenticated forward is marked degraded.** `GET /fed/peers` gains an additive, `omitempty`
   per-peer `auth` member (`"key"` | `"token"`) so an operator can see which links are still on the old
   posture. Absent on a deployment with no peer records keeps today's `/fed/peers` body byte-identical.

### 5.4 The refusals (a NAMED outcome, never a silent drop)

| Situation | Status + code |
|---|---|
| no peer credential presented at all (no key, no accepted token) | `401 FED_PEER_UNAUTHENTICATED` |
| a key is presented but its signature does not verify | `401 FED_PEER_SIGNATURE_INVALID` |
| a key verifies but is not in the peer's accepted key set (or has no path to an anchor — CHAT-TRUST.md §5) | `403 FED_PEER_UNTRUSTED` |
| a keyless peer while the deployment requires keys (`CR_FED_REQUIRE_TOKEN=false` + no accepted token) | `401 FED_PEER_KEY_REQUIRED` |
| the peer is known but revoked (§7) | `403 FED_PEER_REVOKED` |
| the shipped token path, wrong bearer | `401 {"error":"invalid token"}` (**shipped** — never re-spelled) |

### 5.5 The instance key is not the agent key (the seam)

- The **instance key** identifies a deployment and is what a PEER accepts at link setup (§5.1). It
  authenticates the LINK, not a message.
- An **agent key** (`Agent.PublicKey`) identifies an agent and is what the signed agent-scoped routes
  verify (`internal/registry/agentsig.go`) and what a peer must learn to accept through the trust chain
  (`specs/CHAT-TRUST.md` §8, CR-CHAT-026).

Keeping them separate is a decision, and the alternative is named: use the instance key as the agent key
for every agent in the deployment. Rejected — it collapses one identity into many-to-one, makes a
per-agent revocation meaningless (revoking one agent would be revoking the instance), and destroys the
one-key-per-agent property CR-CHAT-026 depends on.

**NOT BUILT:** no instance key, no `CR_FED_INSTANCE_KEY`, no handshake route, no peer-signature header,
no `CR_FED_REQUIRE_TOKEN`, and no peer auth of any kind. Today the only federation credential is the
shared bearer of §2.2.

---

## 6. Authorization — per-peer policy

### 6.1 The policy record

A **peer policy** answers, for one peer: which namespaces it may reach, which agents, in which direction,
and whether it may participate in sessions. Normative shape (NOT BUILT):

```json
{
  "peer": "peer_acme",
  "direction": {"inbound": true, "outbound": true},
  "namespaces": {"allow": ["acme-mirror"]},
  "agents": {"allow": ["atlas", "quill"], "deny": ["deploy-bot"]},
  "capabilities": {"allow": []},
  "sessions": {"may_open": true, "may_join": ["#build-plan"]}
}
```

| Field | Rule |
|---|---|
| `peer` | the local peer id (§4.1). One policy per peer. |
| `direction` | per-direction admission; it must agree with the link's direction (§4.3) — a policy cannot enable a direction the link does not carry. |
| `namespaces.allow` | the external namespaces admitted from this peer. An empty list means none: **the default is deny** (no grant-style fall-through, the same rule CHAT-PERMISSIONS.md §6.4 states for delivery). |
| `agents.allow` / `agents.deny` | local agent ids this peer may reach. `deny` wins over `allow` (an explicit exclusion is never overridden by a wildcard). |
| `capabilities.allow` | capability pools this peer may invoke; empty means none. Kept distinct from `agents` for the reason CHAT-PERMISSIONS.md §6.2 keeps `invoke` distinct from `send`. |
| `sessions` | whether the peer may open a session here, and which local sessions it may join. |

### 6.2 The wall rule — a policy may NARROW, never WIDEN

> **A peer policy is a WALL, not a grant.** Reaching a local agent from a peer requires the peer policy
> to admit it AND the local ACL to admit the shadow principal. The two are a conjunction; neither alone
> is sufficient.

This is deliberate and it is the answer to the obvious temptation — to let a permissive peer policy
constitute authorization. It cannot, for two reasons:

1. **The local model still owns reach.** A `personal` agent owned by someone else is unreachable by a
   peer even with `agents.allow: ["*"]`, because CHAT-PERMISSIONS.md §3.2's rule (`allow iff P ==
   A.owner or a live grant`) decides for the shadow principal exactly as it decides for a local one. A
   peer policy cannot manufacture a grant.
2. **Two independent mistakes must coincide for a leak.** A misconfigured peer policy is not by itself an
   unauthorized delivery; a misconfigured local grant is not by itself reachable from the boundary.

### 6.3 Where the check sits

On the federated INGRESS — a request arriving with the hop marker from an authenticated peer — the order
is normative:

| Order | Step | Owner |
|---|---|---|
| 1 | peer identification + authentication (key or accepted token) | this document §5 |
| 2 | peer policy: external namespace admitted, local agent admitted, direction permitted | this document §6 |
| 3 | remote-participant resolution: resolve (or mint) the LOCAL shadow principal | this document §9 |
| 4 | local realm resolution — the target's stored row, never a claim (`403 NAMESPACE_MISMATCH`) | shipped (CR-FEAT-029) |
| 5 | the local delivery ACL — `may_deliver` on the shadow principal | `specs/CHAT-PERMISSIONS.md` §6.8, steps 4/7 |
| 6 | quarantine, guard choke point, delivery, audit | shipped |

Three properties are binding, and each mirrors a rule the local ACL already states:

- **Authorization happens before content inspection.** A refused crossing must not reach the guard (an
  LLM lane) and spend its budget — CHAT-PERMISSIONS.md §6.8's rule, applied at the boundary.
- **A refusal has no side effect.** No inbox entry is written, no lease is taken, no rotation turn is
  consumed (the `@cap:*` fairness rule, CHAT-PERMISSIONS.md §6.4 rule 1).
- **A refusal is not a 404.** A policy decline names the reason; it never masquerades as "the agent does
  not exist", because the two are different facts with different operator responses.

### 6.4 The refusals — the NAMED decline

| Situation | Status + code |
|---|---|
| the peer may not reach this local namespace at all | `403 FED_NAMESPACE_NOT_PERMITTED` |
| the peer may not reach THIS agent | `403 FED_AGENT_NOT_PERMITTED` |
| the direction is not admitted on this link | `403 FED_DIRECTION_NOT_PERMITTED` |
| the peer is not admitted to this session | `403 FED_SESSION_NOT_PERMITTED` |
| the peer policy allows it, the LOCAL ACL does not | `403 DELIVERY_FORBIDDEN` (**CHAT-PERMISSIONS.md §6.7 — the same code, never a new one**, so a UI renders one string for "you may not") |
| the peer is revoked (§7) | `403 FED_PEER_REVOKED` |

The body is machine-readable and names the decision inputs, so an operator can explain a refusal without
parsing prose:

```json
{"error":"FED_AGENT_NOT_PERMITTED",
 "peer":"peer_acme",
 "direction":"inbound",
 "target":{"type":"agent","ref":"deploy-bot"},
 "detail":"peer peer_acme is not permitted to reach agent deploy-bot (agents.deny)"}
```

### 6.5 Worked example — peer A granted less than peer B

```
peer  peer_acme   state=active  direction=both
policy peer_acme  namespaces.allow=[acme-mirror]  agents.allow=[atlas]  agents.deny=[deploy-bot]
                 sessions={may_open:true, may_join:[#build-plan]}

peer  peer_beta   state=active  direction=inbound
policy peer_beta  namespaces.allow=[beta-lab]  agents.allow=[atlas, quill]

POST /agents/atlas/inbox       from peer_acme   -> 201 (allowed; shadow principal also passes local ACL)
POST /agents/deploy-bot/inbox  from peer_acme   -> 403 FED_AGENT_NOT_PERMITTED (deny)
POST /agents/quill/inbox       from peer_acme   -> 403 FED_AGENT_NOT_PERMITTED (not in allow)
POST /agents/quill/inbox       from peer_beta   -> 201 (allowed for B, refused for A — the point)
POST /agents/atlas/inbox       from <no peer key> -> 401 FED_PEER_UNAUTHENTICATED
```

The fourth line is the acceptance criterion *"peer A can be granted less than peer B"*: the difference is
a policy record, not a different secret.

**NOT BUILT:** there is no peer policy record, no store, no evaluation, and none of the six `FED_*`
refusals exists. An inbound forward today is admitted by the deployment bearer alone (or by no auth at
all when `CR_AUTH_TOKEN` is unset) and lands wherever the local ACL leaves it.

---

## 7. Revocation — one peer, not all

### 7.1 Revoking ONE peer

Revocation is a per-peer decision: the peer record's `state` becomes `revoked` with `revoked_at` and
`revoked_by`, and its accepted keys are removed from the accepted set. The record is **tombstoned, never
edited away** — the same discipline CHAT-PERMISSIONS.md §6.6 applies to a grant, for the same reason:
deleting it would erase who was trusted when.

- **B is untouched.** B's key, B's policy and B's held mail are unaffected. This is the property the
  shared token cannot express (§2.2 consequence 3).
- **It takes effect on the NEXT crossing**, not on a cached decision: the credential is resolved per
  request, exactly as the namespace credential is (§4.1 of NAMESPACES.md resolves its own credential per
  check).
- **Revocation is per-verifier.** A revocation is THIS instance's decision about what IT trusts. A peer
  is not obliged to honour it, and this instance does not claim a global revocation authority; a peer
  that wants to be believed can publish its own key state and a holder may apply it as local policy
  (`specs/CHAT-TRUST.md` §7).

### 7.2 In-flight held deliveries

Two things can be "in flight", and they get opposite treatment. The asymmetry is stated, not hidden:

**(a) OUTBOUND holds at this instance.** These are deliveries this instance accepted (`202 held`) for an
agent somewhere on the peer, queued in the shipped hold queue (§2.1). When the target link is revoked:

- the held item is **failed immediately** with the terminal `FEDERATION_FAILED` receipt the sender would
  have received at the budget's end, carrying a `detail` that names the revocation and a `status` of
  `403 FED_PEER_REVOKED`;
- the item is **removed** from the queue: it is never retried against a link the operator turned off;
- it is **never silently dropped** — the sender is told, once, exactly as it is told on a budget expiry.

**(b) INBOUND requests already accepted.** A delivery a peer sent that this instance already accepted is
durable in a local inbox; it is a fact, not a credential. Revocation does **not** recall it. The operator
who must remove it removes the entry (the shipped inbox controls) or contains the shadow principal
(CHAT-PERMISSIONS.md's suspend/revoke lifecycle); the transcript keeps what crossed. Stating this is the
honest version of "revocation stops a peer" — it stops the peer's NEXT message, never the one already
delivered.

### 7.3 The outcome list

| Situation | Outcome |
|---|---|
| a revoked peer's new forward | `403 FED_PEER_REVOKED` |
| a held outbound delivery whose link was revoked | immediate terminal `FEDERATION_FAILED`, `detail` naming the revocation |
| a delivery already in a local inbox | delivered; not recalled |
| an audit requirement | `peer.revoked` line with `revoked_by` |

**NOT BUILT:** no revocation, no peer state, no hold-queue interaction with a peer decision. The hold
queue is keyed by target agent id (`federation.HoldItem.AgentID`) and knows nothing of peers, links or
credentials.

---

## 8. What crosses the boundary — and what does not

### 8.1 The message body versus a reference

Today the whole payload crosses: the forward re-POSTs the EXACT deliver JSON the source received
(`internal/registry/handler.go`; `HoldItem.Body` is the same bytes on a retry). That is the correct
default for a message bus, and it becomes a policy question the moment the boundary is not one trust
domain.

A crossing policy (proposed, **NOT BUILT**):

| `crossing` | What the peer receives | When to use |
|---|---|---|
| `body` (default) | the message body, as today | a peer in the same trust posture; parity with the shipped path |
| `reference` | a **reference** to the message — its id, and a locator the peer must resolve on its own with its own credential | a payload that must not leave the instance even for an admitted peer |

`reference` is honest only if the resolver it implies exists; it does not today, so the knob is stated as
design and not as a choice an operator can make.

### 8.2 Assets — pointer only (CR-CHAT-014)

Stated plainly so nobody has to infer it: **asset bytes cross the boundary only when they are inside the
delivery payload.** A message that REFERENCES an S3 asset (CR-CHAT-014) crosses as a reference, and this
instance MUST NOT hand the peer a credential with which to dereference it, and MUST NOT fetch the asset
across the boundary on the peer's behalf. The peer resolves the reference with its own credential and its
own policy, or it does not; either way the asset's authorization stays on the instance that owns it.

Asset storage itself is CR-CHAT-014 and is not designed here. This section fixes only the boundary rule.

### 8.3 Audit — a remote participant is named REMOTE

A remote actor never appears in local audit or in the local transcript as a local principal, and never as
unattributed. The subject is spelled as a **remote-qualified identity**:

```
remote:<peer id>:<external subject id>
```

- an audit line for a crossing names the peer, the direction, the local shadow principal, the target and
  the outcome (`delivery.allowed` / `delivery.denied` / the `FED_*` refusal);
- a transcript that shows a message from a remote participant shows the remote marker, so a reader can
  tell a colleague on another instance from a local one — the same discipline CHAT-PERMISSIONS.md §7.4
  applies to naming the human behind an agent;
- the audit vocabulary and read surfaces are CR-CHAT-021; this document fixes only the naming rule.

### 8.4 Identity, idempotency, loops

- **Identity**: the forwarded request carries the peer's authentication and the remote actor's
  remote-qualified identity; it never carries a local principal id as its author.
- **Idempotency**: the sender's idempotency key travels with the forwarded request (shipped), so a peer's
  retry is answered by the DESTINATION's idempotency receipt rather than delivering twice — the same
  protection the local path has.
- **Loops**: `X-Crier-Fed-Hop` stays the mechanism (shipped): a request that already arrived over a link
  is never forwarded again, so two mutually-linked instances cannot ping-pong a message. The hop header
  does not authenticate anything; authentication is §5.

**NOT BUILT:** no `crossing` policy, no reference resolver, no remote-qualified audit subject, and no
remote-aware idempotency scoping beyond the shipped per-target key.

---

## 9. The remote-participant model — a LOCAL shadow principal (decision D14)

**DECISION (D14): a remote participant is a LOCAL shadow principal marked `remote`.**

```json
{
  "id": "prin_remote_01J9Z6V0Q7",
  "kind": "principal",
  "display_name": "ana@acme",
  "namespace": "",
  "role": "member",
  "status": "active",
  "remote": {
    "peer": "peer_acme",
    "instance": "https://crier.acme.example",
    "external_id": "prin_01J9…",
    "key_id": "…"
  },
  "created_at": "2026-10-03T19:45:05Z",
  "last_login_at": null
}
```

- The `remote` marker is **part of the record, not decoration** — exactly as `kind` is
  (CHAT-PERMISSIONS.md §2.1): a reader can always tell a local principal from a projection of a remote
  one, and a policy or an audit line can key on it.
- The shadow is minted by the LOCAL instance at the boundary (or provisioned by an operator on first
  sight); it **holds no key**, cannot log in locally, and exists to be a **grant subject** and an **audit
  subject**.
- A remote AGENT (the CR-CHAT-026 case) is projected the same way: a local shadow agent row marked
  `remote`, carrying the accepted key material of the agent so its own signature can be verified locally
  through the trust chain (CHAT-TRUST.md §8).

**Why local and uniform (the reason for the decision).** Grants, roles, the action set, the delivery ACL
and every refusal stay LOCAL and UNIFORM: the same `may_deliver` predicate (CHAT-PERMISSIONS.md §3.2)
evaluates a shadow principal exactly as it evaluates a local one, with no second permission engine and no
"remote rules" branch. A remote participant is a principal like any other.

**The alternative it beat:** trust the remote instance's role/membership claim for its own members.
Rejected — it is a second truth (the remote instance would widen its members' reach here by fiat), and it
would make the local ACL non-uniform precisely where an adversary is most likely to be. D14 is the reason
§3.3's boundary rule can be stated as a one-liner: everything about a remote participant is resolved
locally, because everything about it locally EXISTS.

**NOT BUILT:** there is no principal store at all (CHAT-PERMISSIONS.md §8.1 item 1), so no shadow
principal, no `remote` marker, no minting and no session participation for a remote actor.

---

## 10. What is NOT built, with the owed thing named

Everything in this document except §2's baseline table, §3.3's rule (stated, not enforced) and the
shipped token path is unbuilt. Item by item:

1. **Peer identity.** No peer record, no peer store, no instance key, no link route. Owed: §4, §5.1.
2. **The mutual key handshake.** Owed: §5.2.
3. **Key-based auth on the federated ingress**, and the `CR_FED_REQUIRE_TOKEN` /
   `CR_FED_INSTANCE_KEY` configuration. Owed: §5.3–§5.5.
4. **Per-peer policy** (namespaces, agents, capabilities, sessions, direction) and its evaluation.
   Owed: §6.
5. **Per-peer revocation**, and the hold-queue behavior on revocation. Owed: §7.
6. **Remote participant shadow principals** (D14). Blocked on CHAT-PERMISSIONS's principal store, which
   is itself unbuilt (its §8.1 item 1). Owed: §9.
7. **External namespaces as admitted scopes**, distinct from local realms. Owed: §3.
8. **The crossing policy** (`body` / `reference`) and the asset boundary rule's enforcement. Owed: §8.1,
   §8.2.
9. **Remote-qualified audit naming.** Owed: CR-CHAT-021 + §8.3.
10. **Session participation for a remote participant** (membership events, join/open). Blocked on
    CHAT-SESSIONS's session object (CR-CHAT-002). Owed: §3.2, §9.
11. **The six `FED_*` refusals.** None exists. The shipped federated refusals are the middleware's
    `401 {"error":"invalid token"}`, the all-links `404 agent not found`, and the terminal
    `502 FEDERATION_FAILED` — none of which can name a peer or a policy. Owed: §5.4, §6.4.
12. **Peer configuration from a document** (`CR_FED_PEERS` / a file). Only `CR_FED_LINKS`, `CR_FED_NAME`,
    `CR_FED_TOKEN`, `CR_FED_MAX_HOLD_S` and `CR_FED_QUEUE_FILE` exist. Owed: §4.2.

---

## 11. Open questions

1. **Is an external namespace a NEW scope kind, or an alias for a local realm?** **PROPOSED-DEFAULT:** a
   distinct kind owned by the peer record. Reusing a local realm would make a peer's members addressable
   by local `@ns/*` delivery, which is the exact widening §3.1 forbids.
2. **One instance key, or one key per link?** **PROPOSED-DEFAULT:** one instance key presented on every
   link; per-link keys are an additive later option, and rotation (CHAT-TRUST.md §6) is per instance.
3. **Does the peer policy live on the source, the destination, or both?** **PROPOSED-DEFAULT:** both —
   the source's policy chooses which link a delivery may use; the destination's policy decides what it
   accepts (§6.2's conjunction). A single-sided policy cannot protect the side that did not set it.
4. **Can a peer reach a `personal` agent?** **PROPOSED-DEFAULT:** only through an explicit grant to the
   shadow principal; the class rule outranks the peer policy (§6.2). A `*` in `agents.allow` never
   means "every agent including someone else's personal one".
5. **What happens to a session when its peer is revoked mid-session?** **PROPOSED-DEFAULT:** the shadow
   principals are SUSPENDED (grants inert, not tombstoned, CHAT-PERMISSIONS.md §2.2) and the transcript
   keeps what crossed; nothing is recalled (§7.2b). A revocation is about the future, and pretending
   otherwise would be a lie about the past.
6. **Where does the instance key live — a file, or the deployment's secret store?** **PROPOSED-DEFAULT:**
   a `0600` file, never silently regenerated, mirroring `CR_DETECT_KEY` (`specs/DETECTION.md` §3.3); the
   secret store convention is `env:VAR` refs for outbound credentials, which is the wrong shape for a
   signing key.
7. **Does the token posture mark a link degraded in `GET /fed/peers`?** **PROPOSED-DEFAULT:** yes, via
   an additive `auth` member (`"key"` | `"token"`), `omitempty` so a deployment with no peer records
   serves today's body byte-identically (§5.3 item 5).

---

## 12. Status line

`DRAFT v1 · 2026-10-03 · CR-CHAT-023 + CR-CHAT-024`

Statements in this document that describe behaviour which does not exist are marked **NOT BUILT** in
place (§4.4, §5.5, §6.5, §7.3, §8.4, §9) and enumerated in §10. Nothing here may be added to
`docs/claims.yaml` until the corresponding code ships, because claims execute against a live server.
