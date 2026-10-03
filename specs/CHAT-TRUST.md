# CHAT-TRUST.md — the signing trust model: vouching, verification, rotation, revocation

Status: **DRAFT v1** · 2026-10-03 · **CR-CHAT-024** + **CR-CHAT-025**
Source material: Bane, 2026-10-03, verbatim: *"chain of trust since we are already doing some auth for
messages we need to allow signing trusts."*; the board rows CR-CHAT-024 and CR-CHAT-025 (read in full);
and the shipped primitives at this worktree's HEAD — the per-agent ed25519 public key
(`internal/registry/types.go`), the signed agent-scoped routes (`internal/registry/agentsig.go`), the
hash-chained server-signed delivery log (`internal/detect/audit.go`), and `specs/DETECTION.md` (which says
of its own layer: *"This layer is wiring, not a new trust primitive."*).

This document is the normative design authority for ONE question: **why should a verifier believe a
signature it has never seen before?** It defines the trust anchor, vouching, verification, key rotation
and revocation semantics. Where it and the shipped code disagree, the **code wins** — file the drift as a
board row (`DF-CRIER-*`) rather than rewriting either side. Sections that describe behaviour which does
not exist yet are marked **NOT BUILT** and must not be added to `docs/claims.yaml` (claims execute against
a live server).

Cross-references: peer identity and per-peer policy are the sibling `specs/CHAT-FEDERATION.md`
(CR-CHAT-023 + CR-CHAT-024) — this document supplies the key trust that a peer link consumes (§8.1). The
local permission model is `specs/CHAT-PERMISSIONS.md` (CR-CHAT-003) — a verified signature is an
IDENTITY, never a grant; the delivery ACL still decides reach. The delivery log's format is
`specs/DETECTION.md` (CR-FEAT-030) — this document builds the trust MODEL over that log's primitives and
does not change the log. Cross-instance identity (one identity for a human, one key for an agent, no
per-instance re-registration) is CR-CHAT-026, the seam in §8.2.

**Glossary (normative; the exact terms this document uses).** A **Signer** is the holder of an ed25519
keypair. A **Subject** is the thing a key speaks for (`instance` | `agent` | `principal-binding`). A
**Key ID** (`key_id`) is the hex prefix of the sha256 of a public key — the shipped `detect.Signer`
convention, so a verifier can name WHICH key signed. A **Trust anchor** is a key a verifier already
believes, by decision, before any signature arrives (§3). A **Voucher** is a key that signs another
key's public key, making a **Delegation** (§4). A **Chain** is the path from a subject key's vouchers up
to an anchor. A **Verdict** is the verifier's decision about one signature, with a NAMED outcome (§5). A
**Trust class** is the temporal annotation a verdict carries for a revoked signer (§7): `revoked-after-
signing` is not the same verdict as `TRUST_KEY_REVOKED`.

---

## 1. Purpose & scope

### 1.1 The one question this document answers

> **A signature arrives, made by a key the verifier has never seen. What must be true for the verifier to
> believe it — and what does the verifier believe, exactly?**

Nothing else. This document is not an authentication mechanism (that is key possession, `agentsig.go`),
not an authorization mechanism (that is CHAT-PERMISSIONS.md), and not a delivery mechanism (that is
`WEBHOOK-DELIVERY.md` + the inbox). It is the missing layer between them: the reason a key that was never
pre-registered can still be believed, and the exact meaning of that belief.

### 1.2 What this document IS normative for

1. **The raw material** it builds on: per-agent ed25519 keys and the hash-chained, server-signed
   delivery log — stated for what they are and are NOT (§2).
2. **The trust anchor**: what a holder already believes, and nothing else (§3).
3. **Vouching / delegation**: how a key comes to be trusted through a chain, the chain's shape, and the
   stated limit (decision D16) (§4).
4. **Verification end to end**: every step that must hold for a foreign signature, and the NAMED outcome
   at each failure (§5).
5. **Key rotation** without breaking verifiable history (§6).
6. **Revocation** — what it MEANS for a record valid when signed, given that the hash chain makes history
   immutable (§7). This is the reason the document exists.
7. **The seams** to peer auth (CR-CHAT-024) and cross-instance identity (CR-CHAT-026) (§8).

### 1.3 What it is NOT normative for

- **A global PKI.** There is no certificate authority and none is proposed; every anchor is a LOCAL
  decision (§3), and there is no global revocation list (§7.4).
- **Authorization.** A verified signature proves a key signed bytes; a `may_deliver` check proves reach
  (CHAT-PERMISSIONS.md §3.2). The two are independent, and a valid signature never carries a grant.
- **Content safety.** Whether a signed message is malicious is the guard's question
  (`specs/LLM-MESSAGE-GUARD.md`), a different layer with a different verdict.
- **The delivery log format.** `specs/DETECTION.md` owns the record shape, the chain and the key file;
  this document only consumes `key_id`, `hash` and `signature` as raw material (§2.2).

---

## 2. The raw material that exists

### 2.1 Per-agent ed25519 keys

- Every registered agent has a `public_key` on its registry row (`Agent.PublicKey`, `HexKey`);
  registration accepts an optional key, and a keyless row is possible (it was registered while signature
  enforcement was off).
- The signed, agent-scoped routes verify possession of the corresponding private key
  (`internal/registry/agentsig.go`): the caller sends `X-Agent-ID`, `X-Agent-Ts` and `X-Agent-Sig`, and
  the server verifies an ed25519 signature over

  ```
  <METHOD>\n<path>\n<unix-seconds>
  ```

  within a ±30-second window, refusing a mismatched key, a malformed signature and a keyless agent with
  distinct named messages.

**Two honest limits of this primitive, stated because the trust model must not build on a fiction:**

1. **This signature authenticates a REQUEST, not a MESSAGE PAYLOAD.** It covers the method, the path and
   a timestamp — **not the body**. A "signed message" in the sense of "the payload's author is
   cryptographically bound to this payload" is **NOT BUILT**; §5.1 proposes the shape, and §9 items 3
   carries it as owed.
2. **A delivery's `sender` is recorded, not verified.** `deliverRequest.Sender` is a self-declared string
   (CHAT-PERMISSIONS.md §1.1). The key exists; the bus's delivery path does not yet check it against the
   key.

### 2.2 The hash-chained, server-signed delivery log

`specs/DETECTION.md` §3.3 defines it, and these are the properties this document uses as raw material:

- each record's `hash` is the sha256 of the record with `hash` and `signature` blanked;
- the signature is the SERVER's ed25519 signature over that hash;
- the record's `prev_hash` names its predecessor **whose value is inside the hash**, so the records form a
  chain: editing breaks the hash and the signature, deleting or reordering breaks the sequence and the
  chain from that point on, and appending a record signed by another key fails verification;
- the signing key is a FILE (`CR_DETECT_KEY`), created `0600`, never silently regenerated, and a log that
  does not verify is refused at startup;
- every record carries a `key_id`.

**The limits, stated as plainly as the spec itself states them** (`specs/DETECTION.md` §2, §8): the log is
**OPT-IN** (`CR_DETECT_ENABLED`, default `false` — with it off, no log file is written and the delivery
path is byte-identical), it is **agent-scoped** — it observes deliveries to agents, not the relay or the
mesh — and it is **one file on one host**, signed by one server key. It is not a tamper-proof trail
against root, and it does not read the mesh or the relay.

### 2.3 The statement this section exists to make

> **The primitives exist. The MODEL does not.**

A per-agent key exists and proves possession. A server-signed hash chain exists and proves what the server
observed. What does not exist is the answer to §1.1: no way for a verifier to decide whether a key it has
never seen should be believed, no chain from a foreign key to something held, no rotation that keeps
history verifiable, and no meaning for a revoked key's past records. `specs/DETECTION.md` itself names the
gap by disclaiming it — *"wiring, not a new trust primitive"* — and this document supplies the primitive
that was disclaimed.

---

## 3. The trust anchor

### 3.1 What a holder already believes

A verifier's trust anchor set is exactly two things, and there is no third:

1. **Its own keys** — the instance key (CHAT-FEDERATION.md §5.1) and the keys of agents it registered
   itself.
2. **Explicitly allowed peers** — the key(s) of another instance that this instance's operator accepted,
   by the link handshake (CHAT-FEDERATION.md §5.2) or by configuration.

```json
{
  "id": "anchor_01J9Z8Q2K7",
  "kind": "trust-anchor",
  "subject": {"type": "instance", "id": "peer_acme"},
  "key": {"alg": "ed25519", "key_id": "…", "public_key": "…"},
  "accepted_at": "2026-10-03T19:40:00Z",
  "accepted_by": "prin_01J9Z6V0Q7",
  "expires_at": null
}
```

| Field | Rule |
|---|---|
| `subject` | what the key speaks for: an instance, or an agent. A key never speaks for "everyone". |
| `key` | the accepted public key and its `key_id`. |
| `accepted_at` / `accepted_by` | the instant and the LOCAL principal or operator action that decided it. Never `system`: a machine did not decide to trust an instance. |
| `expires_at` | `null` = no expiry; a timestamp = the anchor is inert after it, and the verification of §5 treats it as absent (it is NOT deleted — an expired anchor is evidence, the same rule CHAT-PERMISSIONS.md §6.1 applies to an expired grant). |

### 3.2 The rule that makes the model decidable

> **Trust starts at an anchor and nowhere else.** A key is believed only if it IS an anchor or a chain of
> vouchers leads to one (§4). There is no implicit belief from reachability, from a shared secret, from
> "it arrived over the federation link", or from a subject's own claim.

That is the whole difference from the shipped posture. Today one shared secret is believed by everyone,
which means **no one is authenticated**: holding the token proves the token, not the holder
(CHAT-FEDERATION.md §2.2). An anchor is a per-key, per-subject, revocable decision about a NAMED thing.

**NOT BUILT:** there is no anchor store, no anchor record, no route that reads or writes one, and no
`accepted_by` anywhere. A peer's key today enters nothing — a peer is not even identified.

---

## 4. Vouching / delegation

### 4.1 The delegation record

A key comes to be trusted through a **voucher**: another key signs the subject's public key, with a scope.
Normative shape (NOT BUILT):

```json
{
  "id": "vouch_01J9Z9A4T1",
  "kind": "delegation",
  "issuer":  {"type": "instance", "id": "peer_acme", "key_id": "…"},
  "subject": {"type": "agent", "id": "atlas-on-b", "key_id": "…", "public_key": "…"},
  "scope": {
    "namespaces": ["acme-mirror"],
    "directions": ["inbound"],
    "purposes": ["message-signing"],
    "max_depth": 0
  },
  "not_before": "2026-10-03T19:40:00Z",
  "expires_at": "2027-04-03T19:40:00Z",
  "signature": "…"
}
```

The issuer signs the canonical record (every field except `signature`, deterministically encoded — the
same rule `detect.Entry.canonical()` uses), so a delegation cannot be edited into a wider scope.

### 4.2 The chain's SHAPE

- **A chain is a path**, not a set: `subject key → voucher → … → anchor`. Verification walks it in that
  direction (§5).
- **It is bounded**: `scope.max_depth` on a delegation limits how many further delegations its subject
  may issue. A delegation with `max_depth: 0` vouches for a signing key and **cannot** vouch onward.
- **It is scoped**: a delegation names the namespaces, directions and purposes it covers. A key believed
  for `message-signing` in `acme-mirror` is not believed for anything else, and a verification step
  checks the scope of every hop (§5.3).
- **It is temporal**: `not_before` / `expires_at` bound it, and a hop outside its window is refused —
  never clamped.

### 4.3 The stated limit (decision D16)

**DECISION (D16): direct peers only by default; transitive trust only through an explicit delegation.**

> A foreign key is believed when it was vouched for by an anchor this holder ALREADY holds. A key
> vouched for by a key that is itself vouched for is **not** trusted by default: the second hop requires
> a delegation whose scope explicitly carries the transitive purpose (and a `max_depth` that permits the
> hop beyond it).

**The alternative it beat:** trust transitivity by default — "a key vouched for by a trusted key is
trusted, and so on". Rejected, and the reason is measured in the model's own attack surface: default
transitivity makes one compromised peer a bridge into every instance it vouches for, and the holder
cannot enumerate what it is exposed to (its anchor set no longer bounds its trust set). Direct-peers-only
keeps the anchor set the whole truth: **you trust exactly what you accepted, plus what THOSE keys
explicitly, narrowly, and revocably delegate.** The cost is named: a two-hop case needs an explicit
delegation, and D16 makes that a decision an operator makes, not a default a design takes.

### 4.4 Where a delegation is verified from

The holder's anchor set, the delegation records and the revocation records are **local state**: a holder
verifies against what IT holds, and a peer cannot inject an anchor. A peer may publish its delegated key
set; the holder imports the parts it is willing to hold, through an explicit acceptance, and nothing is
trusted merely by being published.

**NOT BUILT:** no delegation record, no voucher walk, no scope evaluation, no `max_depth`, and no D16
enforcement.

---

## 5. Verification, end to end

### 5.1 The worked example — a signature from a foreign instance

A message arrives at instance A over the federated link of `peer_acme`, alleging to come from agent
`atlas-on-b`, signed with a key A has never seen. The signed payload (proposed, §9 item 3) is:

```
crier-trust-v1\n<peer_id>\n<agent_id>\n<message_id>\n<sha256(payload)>\n<ts>
```

Every step that must hold, in order, and what happens at each failure:

| # | Step | Failure | NAMED outcome |
|---|---|---|---|
| 0 | **Envelope parses.** The signature, the key id, the signer's claimed subject and the timestamp are present and well-formed. | a field is missing or unparseable | `400 TRUST_MALFORMED` |
| 1 | **The signer key is resolved.** The `key_id` is looked up in the local known-key set: an anchor, or a delegated key the holder holds. | the `key_id` names nothing held | walk the chain (§5.2) |
| 2 | **The chain reaches an anchor.** A delegation path from the signer key to a held anchor exists, within depth. | no path exists | `403 TRUST_NO_PATH_TO_ANCHOR` |
| 3 | **Every hop is in scope.** Each delegation's namespaces / directions / purposes cover this use; `max_depth` permits each hop. | a hop is out of scope | `403 TRUST_DELEGATION_OUT_OF_SCOPE` |
| 4 | **Every hop is in its window.** `not_before` ≤ now ≤ `expires_at` for each delegation. | a hop is outside its window | `403 TRUST_DELEGATION_EXPIRED` |
| 5 | **No key in the path is revoked** for this use at this instant (§7). | a key is revoked | `403 TRUST_KEY_REVOKED` |
| 6 | **The signature verifies** over the canonical payload (§5.1) with the signer's public key. | it does not verify | `401 TRUST_SIGNATURE_INVALID` |
| 7 | **The timestamp is fresh** and inside the replay window. | it is stale or in the future beyond the window | `401 TRUST_REPLAY_WINDOW` |
| 8 | **The verifier decides**, and records the verdict with the trust class of §7.3. | — | verdict recorded |

Two properties of the order are decisions, stated because they are easy to get backwards:

- **The chain is walked BEFORE the signature is verified** (steps 2–5 before 6). Verifying a signature
  first would spend CPU on keys the holder has no reason to consider, and — worse — a verification error
  would be indistinguishable from an untrusted key unless the trust decision is separate. The two are
  separate verdicts with separate codes.
- **Revocation is checked per hop** (step 5), not only on the leaf: a revoked voucher cannot reprieve
  itself by having signed its subject before revocation, and a revoked subject is refused whatever its
  voucher's state.

### 5.2 What verification does NOT prove

- **It does not prove the signer is honest.** It proves a key signed bytes. Content safety is the guard's
  layer (LLM-MESSAGE-GUARD.md) and identity is CHAT-PERMISSIONS.md's.
- **It does not prove the payload is the one delivered**, unless the payload digest is inside the signed
  bytes (§5.1); the shipped request signature does not cover the body (§2.1 limit 1), so until item 3 of
  §9 ships, a "verified signature" is a verified REQUEST, not a verified MESSAGE.
- **It does not prove the key was uncompromised at signing time.** A revoked-after-signing record is
  verified and suspect at once (§7.5) — the model states this rather than resolving it away, because it
  cannot.

### 5.3 Whose scope governs

A delegation's scope is an upper bound on the use it authorizes; the VERIFIER's local policy is a second
bound. A delegated key believed for `acme-mirror / message-signing / inbound` is still refused if the
local peer policy (CHAT-FEDERATION.md §6) does not admit `acme-mirror` inbound — two independent checks,
the same conjunction §6.2 of CHAT-FEDERATION.md applies to reach.

**NOT BUILT:** there is no chain walk, no anchor set, no signed message payload, and none of the eight
`TRUST_*` outcomes. Today a key is only ever verified against the row of the agent it addresses
(`agentsig.go`), and only for a request signature.

---

## 6. Key ROTATION without breaking verifiable history

### 6.1 The rotation certificate

Rotation must not break the verifiability of anything already signed. The mechanism is an **append**, not
a rewrite — the hash chain's whole value is that history cannot be rewritten (DETECTION.md §3.3):

- the OUTGOING key signs the INCOMING key:

  ```
  crier-trust-rotate-v1\n<old_key_id>\n<new_key_id>\n<not_before>\n<reason>
  ```

- the certificate is durable and is itself a record; the incoming key is not trusted until the outgoing
  key has signed it (`403 TRUST_ROTATION_UNSIGNED` otherwise);
- an **overlap window** is declared (`not_before` of the new key to the `expires_at` of the old), during
  which a signature by EITHER key verifies — the window in which the rotation announcement propagates.

### 6.2 Why it keeps history verifiable

- A record's own verdict depends on the key that was in force when it was signed, and the log's per-record
  `key_id` (DETECTION.md §3.4) is exactly what names that key. A reader verifies each era under its own
  key; the rotation certificate is the link that says the two eras are the same subject.
- **Re-signing old records is not rotation** — it is forgery; the holder of the new key cannot attest to
  what the old key signed, and a chain that tolerates it is no chain. Rejected (§7.2, option E).

### 6.3 Refusals

| Situation | Outcome |
|---|---|
| a rotation certificate not signed by the outgoing key | `403 TRUST_ROTATION_UNSIGNED` |
| a rotation naming a key that is revoked | `403 TRUST_KEY_REVOKED` |
| a signature accepted outside the declared overlap window | `401 TRUST_REPLAY_WINDOW` / `403 TRUST_KEY_UNKNOWN` |

**NOT BUILT:** no rotation certificate, no overlap window, no rotation route or record. The shipped
detect key is explicitly NOT rotated (`specs/DETECTION.md` §3.3: an unreadable key file is a startup
error, and a per-boot key would make every restart an unverifiable log).

---

## 7. REVOCATION — the hard one

### 7.1 The question, stated exactly

A record was valid when it was signed: the key was trusted, the signature verified, the record is in a
chain. Later the key is revoked. **What does the revocation MEAN for that record?** The hash chain makes
history immutable, so revocation cannot rewrite it — the only honest answers are about what a READER does
with the record, not what happens to it.

### 7.2 The options, each with its cost

| Option | What it does | Cost | Verdict |
|---|---|---|---|
| **A — Rewrite / erase** | delete the affected records, or re-sign them under a new key | it forges history, and a rewrite is exactly what the chain exists to DETECT (DETECTION.md §3.3: editing breaks the hash and the signature). A system that rewrites its own log has no log | **REJECTED** |
| **B — Retroactive invalidation** | declare every record by that key invalid NOW | it makes a record that WAS verified claim to be unverified — a lie about the past — and destroys the ability to investigate what the key signed while trusted (the compromise window is exactly what a reviewer needs) | **REJECTED** |
| **C — Cut-forward, time-anchored trust** | revocation sets `revoked_at`; a signature that verified BEFORE that instant remains a verified signature, annotated `revoked-after-signing`; a signature arriving AT or AFTER it is refused | nothing is hidden and nothing is rewritten; the cost is that a reader must attend to the trust class, not a boolean | **RECOMMENDED** |
| **D — Key-only vs namespace revocation** | revoke the KEY, not everything it ever touched; a delegated subject's revocation does not revoke its voucher, and the voucher's scope is re-evaluated | a compromise of the voucher is a separate decision (it gets its own revocation) | **PART OF C** |
| **E — Re-sign** | the new key re-signs the old records | forgery: the new key cannot attest to bytes it did not sign; it is option A with a nicer name | **REJECTED** |

### 7.3 The recommendation, stated as the model

**Trust is a property with a TIME, not a boolean.** A verdict has three parts: the signature's validity,
the key's trust state, and the instant the signature was made. Concretely:

- a signature made **before** `revoked_at` → the signature VERIFIES and the verdict is
  `trust_class: "revoked-after-signing"`, NAMED `TRUST_REVOKED_AFTER_SIGNING` (verified, and suspect);
- a signature made **at or after** `revoked_at` → refused with `403 TRUST_KEY_REVOKED`;
- a revoked key can never sign a ROTATION (§6) or a new DELEGATION (§4): revocation ends the key's
  forward authority, not its history.

### 7.4 Revocation is per-verifier, and tombstoned

- A revocation record is `{key_id, revoked_at, revoked_by, reason}`, and it is **tombstoned, never edited
  away** — the same discipline CHAT-PERMISSIONS.md §6.6 applies to a revoked grant, and for the same
  reason: erasing it would erase who trusted what, when.
- There is **no global CRL**, because there is no global authority (§3.1). A peer MAY publish its key
  state; a holder applies what it chooses to hold, as local policy. A holder's revocation is final for
  that holder and binds no one else.
- Revocation takes effect on the **next** verification, not on a cached verdict: the anchor and
  revocation sets are resolved per check, exactly as the namespace credential is (NAMESPACES.md §4.1).

### 7.5 What a revoked-but-chained record therefore MEANS to a reader

> **It is evidence of what happened, verified under the key that was in force, and it is not a claim
> that the key was uncompromised.**
>
> The record proves the bytes were signed by key K, and that K was trusted by this holder at the time.
> It does NOT prove K was uncompromised then — revocation is usually a response to compromise, so the
> interval from the key's acceptance (or `not_before`) to `revoked_at` is a window of **probable
> compromise**. The record's trust class says so: `revoked-after-signing` — genuine, historical, and
> suspect from the earliest moment compromise cannot be excluded.
>
> A reader therefore may rely on it for HISTORY (who signed what, and when) and must not rely on it for
> AUTHORITY (that the signer was entitled to speak NOW, or that the content is uncompromised). The
> revocation does not make the record disappear, and it does not make it a lie; it changes what the
> record is allowed to support.

The corollary for the chain: the chain stays verifiable forever because revocation is recorded, not
applied to history. A verifier that follows the chain sees each record's own `key_id`, verifies each era
under its own key, and reads the revocation records as annotations on the timeline — which is precisely
why the chain, not a mutable key directory, is the right raw material.

**NOT BUILT:** no revocation record, no `revoked_at`, no trust class, no `TRUST_REVOKED_AFTER_SIGNING`.
Nothing in the tree today can revoke a signing key; the detect key cannot even be rotated.

---

## 8. The seams

### 8.1 To peer auth (CR-CHAT-024)

- The **instance key** (CHAT-FEDERATION.md §5.1) is what a peer link authenticates with, and the accepted
  peer key IS a trust anchor (§3.1 item 2): a link is instance-scale vouching, and it is how a foreign
  instance's key enters the anchor set. There is no second, parallel "peer trust" mechanism.
- A **peer's delegated agent keys** (an agent on B speaking to A without being re-registered) are believed
  by the chain of §4, rooted at B's accepted instance key. A peer may publish its key set; A imports the
  parts it is willing to hold. Publishing is not believing (§4.4).
- The failure vocabularies compose: a key with no path to an anchor is `TRUST_NO_PATH_TO_ANCHOR` (§5.1);
  a peer whose link is not accepted is `FED_PEER_UNTRUSTED` (CHAT-FEDERATION.md §5.4). The first is about
  a KEY, the second about a LINK; a request can fail either, and the codes never stand in for each other.

### 8.2 To cross-instance identity (CR-CHAT-026)

- **One identity for a human**: the human authenticates where they already do (OIDC-shaped), and each
  instance holds a LOCAL shadow principal so the grant model stays local and uniform
  (CHAT-FEDERATION.md §9, decision D14). The trust model does not create a second human identity; it
  routes the already-authenticated human into the local model as a `remote`-marked principal.
- **One key for an agent**: the agent's ed25519 keypair IS its identity, and CR-CHAT-026's requirement
  (*"an agent must NOT need a separate credential per instance it speaks to"*) is satisfied by a peer
  ACCEPTING that key through the chain of §4 rather than re-registering the agent. The key never leaves
  the agent; the peer verifies it locally.
- The two halves meet at the shadow: the shadow principal is the local projection of a remote human, and
  the shadow agent (where an agent is projected) carries the accepted key material that §5 verifies.

### 8.3 To the delivery log

- The log's per-record `key_id` is the raw material a future trust annotation attaches to: the log says
  what the SERVER observed, the trust model says what a signature MEANS.
- Because the log is **opt-in** (`CR_DETECT_ENABLED` default false) and server-local, the trust model
  MUST NOT presuppose it: a verifier's decision comes from the anchor set and the signed payload, not from
  the log. The log is evidence ABOUT a decision, never the decision.

**NOT BUILT:** no anchor store, no delegation store, no rotation, no revocation, and no seam wiring at
all. The D14 shadow principal itself is owed to CHAT-PERMISSIONS.md (its §8.1 item 1).

---

## 9. What is NOT built, with the owed thing named

1. **The trust anchor store and record.** No anchor, no `accepted_by`, no route. Owed: §3.
2. **Delegation / voucher records and the chain walk.** No record, no scope evaluation, no `max_depth`,
   no D16 enforcement. Owed: §4.
3. **A signed MESSAGE payload.** The shipped signature covers `<METHOD>\n<path>\n<ts>` — NOT the body
   (agentsig.go; §2.1 limit 1). Until a payload binding exists, "a signed message" is not a claim this
   system can make. Owed: §5.1.
4. **The eight `TRUST_*` verdicts.** `TRUST_MALFORMED`, `TRUST_NO_PATH_TO_ANCHOR`,
   `TRUST_DELEGATION_OUT_OF_SCOPE`, `TRUST_DELEGATION_EXPIRED`, `TRUST_KEY_REVOKED`,
   `TRUST_SIGNATURE_INVALID`, `TRUST_REPLAY_WINDOW`, `TRUST_REVOKED_AFTER_SIGNING`. None exists; there is
   no `/trust/*` route and no trust verdict in any response. Owed: §5, §7.3.
5. **Key rotation** — no certificate, no overlap window, no route. Owed: §6.
6. **Revocation** — no revocation record, no `revoked_at`, no tombstone, no trust class. Owed: §7.
7. **Revocation publication / import.** No mechanism for a peer to publish its key state or for a holder
   to apply it. Owed: §7.4.
8. **Trust annotations on the delivery log.** The log records `key_id`; nothing annotates its records with
   a trust class. Owed: §8.3.
9. **The peer seam.** Anchors are populated by the link handshake, which is itself unbuilt
   (CHAT-FEDERATION.md §5.2, §10 items 1–3). Owed: §8.1.
10. **The D14 shadow principal and the CR-CHAT-026 projection.** Owed to CHAT-PERMISSIONS.md and
    CHAT-FEDERATION.md §9. Owed: §8.2.

---

## 10. Open questions

1. **Is the message-digest binding inside the signed payload, or a signature over the whole envelope?**
   **PROPOSED-DEFAULT:** sign a canonical transcript that includes `sha256(payload)` (§5.1) — the body is
   bound, and the payload stays opaque to the bus. Signing the raw body would require canonicalization the
   bus does not have.
2. **Should keys be per-agent, per-instance, or both be accepted at the same surface?** **PROPOSED-DEFAULT:**
   both are accepted, and the `subject.type` on the anchor/delegation says which — an instance key speaks
   for its instance, an agent key for its agent. Collapsing them is rejected in CHAT-FEDERATION.md §5.5.
3. **How does an anchor expire, and is re-acceptance automatic on a rotation?** **PROPOSED-DEFAULT:** an
   anchor expires only when the operator set `expires_at`, and a rotation certificate (§6) carries the
   trust forward WITHIN the accepted anchor's continuity — so a peer's rotation does not silently drop the
   link. A rotation by a key that is not the accepted one is a new acceptance, not a rotation.
4. **Where do revocation records live — per instance, or exchanged between peers?** **PROPOSED-DEFAULT:**
   per instance (local, §7.4), with a publication format available to peers as an OPT-IN import. A shared
   revocation store would be a global authority, which §3.1 rejects.
5. **Does a revoked voucher invalidate history signed by its subjects?** **PROPOSED-DEFAULT:** no — the
   subject's own records keep their own verdicts; the voucher's revocation stops FUTURE delegations and
   requires the subject to be re-vouched. Retroactively cascading the voucher's revocation would be option
   B at one remove, and §7.2 rejects B.
6. **Is the trust class recorded on the record, or computed at read time?** **PROPOSED-DEFAULT:** computed
   at read time from the revocation record and the signature timestamp. Recording it would make the trust
   verdict mutable state on an immutable record — the one thing §7 refuses.

---

## 11. Status line

`DRAFT v1 · 2026-10-03 · CR-CHAT-024 + CR-CHAT-025`

Statements in this document that describe behaviour which does not exist are marked **NOT BUILT** in place
(§3.2, §4.4, §5.3, §6.3, §7.5, §8.3) and enumerated in §9. Nothing here may be added to `docs/claims.yaml`
until the corresponding code ships, because claims execute against a live server.
