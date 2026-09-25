# Namespaces (realms) — per-realm policy for agents and messages

Status: **Implemented** · v1 · 2026-09-25 · **CR-FEAT-029**
(source: external hands-on review `DISPATCH · CRI-001` by **Carter**, delivered via Bane 2026-09-25;
tested live at `ca28523d` on 2026-09-24 on a scratch port with the guard disabled per `TESTERS.md` §1)

This document is the normative design authority for crier's namespace (realm) dimension. Where it
and the shipped code disagree, the **code wins** — file the drift as a board row (`DF-CRIER-*`)
rather than rewriting either side silently.

---

## 1. The gap this closes

The review's scaling section, verbatim:

> there are no namespaces or tenants (compare WAMP realms — one noisy neighbour, one shared fate)

and it was right, at the level of the code as much as the docs: the registry had no realm on a row,
the relay's topic space was one flat namespace, every inbox lived in one namespace, the publish rate
limit was **one counter per agent id**, and the message guard's concurrency cap and circuit breaker
were **process-wide**. A single flooding agent therefore consumed the publish budget of the agent id
it happened to share, and — worse — a flood anywhere could trip the shared guard circuit and stop
*every* delivery in the deployment being checked.

`docs/capacity-ceiling.md` already said so, in the measurement that closed CR-FEAT-033:

> live WebSockets per agent, there are no namespaces, and the 100/min/agent publish …

## 2. The ancestor: what WAMP realms actually do

The row says to read WAMP's realm semantics before designing, so this section is the reading, not a
vibe. Quotes are from the **WAMP Basic Profile** (wamp-proto.org, `wamp_bp_latest_ietf.html`),
§*Realms, Sessions and Transports* and §*Glossary*:

- *"A Realm is a WAMP routing and administrative domain, optionally protected by authentication and
  authorization."*
- *"WAMP messages are only routed within a Realm."* and *"Routing occurs only between WAMP Sessions
  that have joined the same Realm."*
- *"Realm — Isolated WAMP URI namespace serving as a routing and administrative domain, optionally
  protected by AA [authentication and authorization]."*
- A **session** attaches to a realm during the handshake (`HELLO [HELLO, Realm|uri, …]`), and a
  **principal** is defined *"within a Realm"*, running in the security context of a role *within that
  Realm*.

So a WAMP realm is three things at once: **(a)** an isolated URI (routing) namespace, **(b)** an
administrative/auth domain, and **(c)** per-session membership — one session, one realm.

### What crier steals

1. **Isolation is a property of routing, not a filter.** Two realms using the identical topic name
   are two disjoint sets of subscribers. crier implements this by *keying* the relay's subscription
   map by realm (`subscriptionKey`), so a publish in realm A cannot even see realm B's subscribers —
   there is no post-hoc "is this allowed?" step that a leak could skip.
2. **Membership is recorded, not inferred.** An agent's realm is a registration-time property stored
   with its row (WAMP's session-realm attachment), and a message's realm is taken from the **target's
   row**, never from the sender's claim.
3. **A realm is an auth boundary.** crier's per-namespace **auth posture** adds a realm-scoped
   credential where the realm is known (registration into it, delivery into it, publishing and
   subscribing in it) — WAMP's "optionally protected by AA", implemented as *stricter-only*: a realm
   can demand an additional credential and can never remove the deployment's own gate.
4. **Per-realm policy.** Administrative domain → per-realm rate limits, guard settings and retention.

### What crier deliberately does NOT steal

- **Not a second router.** WAMP realms have their own router lifecycle and auth methods; crier's
  realms are a policy dimension on one process with one store, one guard, one relay.
- **No realm-to-realm bridging.** WAMP has no cross-realm routing either — and crier adds none. There
  is no API that delivers into another realm, and none is planned here.
- **No per-realm URI prefix.** crier's topics stay literal (`orders.created`), because the realm is
  carried in the routing key rather than baked into the topic name. This keeps a realm split from
  rewriting every existing topic.

## 3. The realm model

| Term | Meaning | Wire / storage spelling |
|---|---|---|
| **default namespace** | The single implicit realm every deployment had before this feature. Always exists, never declared, and carries no policy unless an operator declares one. | `""` — **empty string** |
| **declared namespace** | A realm an operator declared in `CR_NAMESPACES` / `CR_NAMESPACES_FILE`. | its own name |
| **canonical name** | The one spelling used on every wire and storage surface. | `namespace.Canonical` |

**The default realm is spelled `""`, everywhere.** `namespace.Canonical("") == ""` and
`namespace.Canonical("default") == ""`. This is the single seam that makes the third acceptance
criterion (below) true: an agent row, an inbox entry, a webhook envelope, a relay frame and a
`/relay/topics` entry from an unconfigured deployment serialise **byte-identically** to what they
serialised before this feature existed (every new member is `omitempty` and empty). The
operator-facing name `default` is accepted on input (registering into `"default"` is the same as
registering into `""`) and is reported as `"default"` by `GET /namespaces`, so no client has to
hard-code the empty spelling.

Names are `^[a-z0-9][a-z0-9_-]{0,63}$`. `default` is reserved for the default realm and may be
declared once (to carry policy for it) but never duplicated. **A name the server does not serve is
always refused, never mapped back to the default realm** — see §5.

## 4. Per-namespace policy

| Axis | Field | Unset means | Enforced where |
|---|---|---|---|
| auth posture | `auth` (`shared` \| `token`), `token_ref` | `shared` — the deployment's own gate (Bearer token + per-agent ed25519 signature) is the whole gate | registration into the realm, delivery into it, relay publish/subscribe in it |
| rate limit | `rate_limit_per_minute` | `CR_RATE_LIMIT_PER_MINUTE` (the deployment cap) | relay publish, counted per **(realm, agent)** |
| guard settings | `guard_enabled`, `guard_policy` | the deployment's guard posture (`CR_GUARD_ENABLED`, `CR_GUARD_DEFAULT_POLICY`) | the delivery choke point |
| retention | `retention_seconds` | `registry.DefaultMessageTTL` (24h) | the delivery's default message lifetime |

Every axis is **inherited when unset**: a namespace that declares nothing is behaviourally identical
to a namespace that does not exist. There is no axis whose zero value silently means something
stronger than "inherit" (a declared `retention_seconds: 0` is *refused*, not read as "never expires
for every message in this realm").

### 4.1 Auth posture

- **`shared`** (default, and the zero value): the deployment's `CR_AUTH_TOKEN` bearer and, where
  `CR_REQUIRE_AGENT_SIG` is on, the per-agent ed25519 signature are the whole gate. An unconfigured
  deployment is exactly this.
- **`token`**: an **additional** realm-scoped credential is required on the surfaces where the realm
  is known. It is supplied in `X-Crier-Namespace-Token` and resolved from `token_ref`, which must be
  an `env:VAR` reference (the guard's `api_key_ref` convention — **never an inline secret**). The
  comparison is constant-time, and the credential is resolved **per check**, so an environment
  rotation takes effect without a rebuild.
- A realm that declares `token` but resolves to an empty secret is **refused at boot**: a gate that
  admits everyone while looking closed is worse than no gate.

### 4.2 Guard settings and the noisy-neighbour problem

The guard's budget is process-wide: one `CR_GUARD_MAX_CONCURRENT`, one circuit breaker, one LLM
lane. That is precisely the shared fate the review measured. Two per-realm controls exist and only
two:

- **`guard_enabled: false`** — the guard choke point is **not called at all** for deliveries into
  that realm. This is the isolation valve: a realm that floods (or that is trusted by construction)
  cannot consume the guard's concurrency budget or trip the shared circuit for every other realm.
- **`guard_policy`** — the realm's **default policy** for deliveries in it, i.e. the §4.2-step-3
  server default of the guard spec (`specs/LLM-MESSAGE-GUARD.md`). An agent's own `guard.policies`
  still outrank it (the guard's own specificity order, unchanged).

Neither control can make a realm *less* checked than it already was unless the operator says so
explicitly: with both unset, the delivery path reaches the guard exactly as before.

### 4.3 Rate limits

The cap comes from the realm's `rate_limit_per_minute` when declared, else from
`CR_RATE_LIMIT_PER_MINUTE`. The counter key is `(canonical realm, agent id)`.

That key is the whole non-interference claim at the relay: two realms' publishers cannot consume
each other's budget, and **the same agent id has one budget per realm**. `0` is a declared policy
value meaning "this realm is not rate limited" — realm-local, exactly as `0` means "rate limiting
off" deployment-wide.

### 4.4 Retention

A realm's `retention_seconds` is the **default** message lifetime for deliveries into it. An explicit
`ttl_seconds` on the request always wins, including an explicit `0` (= never expires). The realm's
value is recorded as the entry's `TTLSeconds` before the expiry is resolved, so the store, the
retrieve body and the dead-letter record all agree about the lifetime that actually applied.

## 5. Where a realm is decided (and why that is the crossing defence)

| Lane | The realm comes from | A claim that disagrees |
|---|---|---|
| `POST /agents` | the request's `namespace`, validated against the declared set + that realm's auth posture | `400 UNKNOWN_NAMESPACE` / `401 NAMESPACE_UNAUTHORIZED` — **never** silently the default realm |
| `POST /agents/{id}/inbox` | the **target agent's stored row** | `403 NAMESPACE_MISMATCH`, and nothing is stored |
| `POST /agents/{id}/inbox/transfer` | both agents' rows (a move must stay inside one realm) | `403 NAMESPACE_MISMATCH` |
| `POST /relay/publish` | the `X-Crier-Namespace` **header** (read before the body, so the per-realm rate limit is exact) | `400` if the header and a body member disagree, or the name is undeclared; `401` if the realm's credential is missing/wrong |
| `GET /relay/subscribe/{topic}` | the `X-Crier-Namespace` header or the `namespace` query parameter (a browser WebSocket cannot set headers) | `400` when the two disagree, `400` for an undeclared name, `401` for a missing realm credential — **before** the upgrade |
| `PATCH /agents/{id}` | not a PATCH field | `400` — moving a live agent has no defined meaning for the messages already queued in its inbox |

Two properties follow, and they are the acceptance:

1. **A message cannot cross namespaces implicitly.** Its realm is the target's, and the only two
   ways a realm appears in a request (a body member or a header) are *checked*, never believed.
2. **A realm is never chosen by accident.** An undeclared name is an error on every lane — a typo
   must not put an agent into the wrong realm, which (with per-realm guard settings) could silently
   mean the unguarded one.

## 6. Configuration surface

```bash
# The whole namespace document, inline …
CR_NAMESPACES='{"namespaces":[ … ]}'
# … or as a file. Set at most ONE of the two (config.Load refuses both).
CR_NAMESPACES_FILE=/etc/crier/namespaces.json
```

```json
{
  "namespaces": [
    {
      "name": "acme",
      "auth": "token",
      "token_ref": "env:CR_NS_ACME_TOKEN",
      "rate_limit_per_minute": 240,
      "guard_enabled": true,
      "guard_policy": {"id": "acme-default", "thresholds": {"block_risk": "high"}},
      "retention_seconds": 86400
    },
    {"name": "batch", "guard_enabled": false, "rate_limit_per_minute": 0}
  ]
}
```

Rules, all **fail-closed and loud** at boot:

- unknown fields are refused (`DisallowUnknownFields`) — a misspelled key is never a silently ignored
  setting;
- duplicate names, an invalid name, a negative rate limit, a `retention_seconds < 1`, a non-`env:`
  `token_ref`, a `token_ref` on a `shared` realm, an unknown `auth` value, a `guard_policy` the guard
  package refuses, a trailing second JSON document, or a `CR_NAMESPACES_FILE` that cannot be read:
  each fails the boot with a message naming the namespace and the field.

Unset (or an empty document) is the shipped default: **one implicit realm, nothing declared**, and
the boot log says so (`namespaces: none declared — the implicit default namespace serves every
agent`).

## 7. Wire contract

- **`namespace` on an agent row** (`Agent.Namespace`, `omitempty`) — present only for a declared
  realm; absent for the default realm.
- **`namespace` on a stored message** (`InboxEntry.Namespace`, `omitempty`) — recorded with the
  message, so a retrieved message states its realm even after the agent's row is gone.
- **`namespace` in the webhook envelope** (`EnvelopeMeta.Namespace`, `omitempty`) — the push
  transport states the realm too.
- **`namespace` on a dead letter** (`DeadLetter.Namespace`, `omitempty`) — the destination outlives
  the agent row, so the realm travels with the record.
- **`namespace` on `/relay/topics` entries** (`TopicInfo.Namespace`, `omitempty`) — the same literal
  topic name appearing once per realm is the isolation made visible.
- **`GET /namespaces`** — the policy set the server is *actually* enforcing, with a live per-realm
  agent census. Reports the posture and the `env:` **reference**, never a resolved secret. Not
  auth-exempt (like `/status`, it reports posture).
- **Headers**: `X-Crier-Namespace` (the realm a relay request acts in), `X-Crier-Namespace-Token`
  (the realm credential a `token`-posture realm requires).

## 8. Storage

Migration `008_add_namespaces` adds a nullable `namespace` column to `agents`, `inbox_entries` and
`dead_letters`, each with a `CHECK` that a present value is non-blank. **`NULL` is the default
realm** and is `COALESCE`d to `""` on every read, so:

- every pre-existing row reads back as the default realm and behaves exactly as it did;
- nothing is backfilled (no fabricated realm name);
- no index is added — the only per-realm read is `GET /namespaces`, which counts the rows it already
  lists, and the delivery path resolves a realm from the target row it already fetches by primary
  key.

The in-memory backend needs no schema change; `MemoryStore.Update` preserves the stored realm
exactly as it preserves `RegisteredAt`, so no update path can move an agent between realms.

## 9. Non-regression contract (the third acceptance criterion)

Single-namespace behaviour is byte-identical to the pre-CR-FEAT-029 server, and that is
**measured**, not asserted:

| Surface | The property | Proof |
|---|---|---|
| agent row JSON | no `namespace` member for the default realm | `TestDefaultNamespaceRegistrationIsByteIdentical` (handler), `TestCRFEAT029SingleNamespaceIsByteIdentical` (live server), member-for-member comparison against a server with no namespaces at all |
| inbox entry JSON | no `namespace` member | `TestCRFEAT029RetentionIsPerNamespaceEndToEnd` reads the live retrieve body |
| relay frame bytes | unchanged `{"topic":…,"event":…}` | `TestSingleNamespaceRelayIsUnchanged` (byte comparison) |
| `/relay/topics` | no `namespace` member | same test, on the marshalled body |
| rate limiting | the deployment cap, per agent id | `CheckRateLimit(agent)` is `CheckRateLimitIn("", agent)`; `TestRelayNamespaceCapOfZero…` covers the realm-local `0` |
| guard | the choke point is reached exactly as before | `TestNamespaceGuardSettingsDoNotInterfere` counts LLM calls (0 for a realm that disabled it, >0 for one that did not) |
| retention | 24h when nothing is declared | `TestNamespaceRetentionIsTheDefaultLifetime` / `…RetentionIsPerNamespaceEndToEnd` |
| the whole existing suite | green, unmodified in behaviour | `go test -short ./...` |

## 10. Acceptance evidence

The row's three criteria map onto named tests:

1. **Two namespaces with different rate limits and guard policies demonstrably do not interfere.**
   `TestCRFEAT029TwoNamespacesDoNotShareFate` (live server: the tight realm 429s on its 4th publish
   while the quiet realm keeps publishing, and the flooding agent's own id is not limited there);
   `TestRelayPerNamespaceRateLimitsDoNotInterfere` and `TestRelayNamespaceCapOfZeroDisablesLimiting`
   (relay); `TestNamespaceGuardSettingsDoNotInterfere` (the `guard_enabled: false` realm never
   reaches the guard, counted on the mock LLM, while the other realm still gets its verdict).
2. **A message cannot cross namespaces implicitly.**
   `TestCRFEAT029MessageCannotCrossNamespaces` (live: 403 `NAMESPACE_MISMATCH`, inbox depth stays 0);
   `TestDeliveryIsPinnedToTheTargetNamespace`, `TestTransferCannotCrossNamespaces`,
   `TestRegisterIntoUndeclaredNamespaceIsRefused`, `TestUnwiredHandlerRefusesAnUndeclaredNamespace`,
   `TestRelayNamespacesAreDisjoint`, `TestRelayWildcardIsNamespaceScoped`,
   `TestRelayDefaultNamespaceIsItsOwnRealm`, `TestRelayUnknownNamespaceIsRefused`.
3. **Single-namespace behaviour is byte-identical to today.** §9's table, plus
   `TestNamespaceNamespaceOfAgentCanonicalises` / the `namespace` unit tests pinning
   `Canonical("") == Canonical("default") == ""`.

## 11. Non-goals and honest limits

- **The mesh lane is not realm-scoped.** `/mesh/connect/{agentID}` and the peer frames are a
  per-relay agent transport; realms partition the registry, inbox and relay lanes (the three the
  review named). A future row can extend the mesh, and this spec must be updated rather than
  silently outgrown.
- **No realm-to-realm bridging, no per-realm router.** See §2.
- **A browser WebSocket cannot send the realm token.** `?namespace=` covers the realm *name* (a
  browser `WebSocket` cannot set headers), but the realm *credential* is header-only, so a
  `token`-posture realm is not subscribable from a browser page without a proxy. Stated here rather
  than discovered later.
- **Moving a live agent between realms is unregister + re-register.** A PATCH refuses the member: the
  messages already queued in its inbox were delivered under the old realm's retention, guard settings
  and auth posture, and there is no defined way to re-interpret them.
- **Retention is a delivery default, not a reaper policy.** It bounds a *new* message's lifetime; it
  does not retroactively shorten messages already stored.
- **`GET /namespaces` counts agents from the live registry**, so on a remote-proxy backend (a relay
  that does not own the registry) the census is whatever that store reports — not a global truth.

## 12. Provenance

Filed from the external hands-on review **`DISPATCH · CRI-001`** by **Carter** (delivered via Bane
2026-09-25; tested live at `ca28523d` on 2026-09-24). The review's claim — *"there are no namespaces
or tenants (compare WAMP realms — one noisy neighbour, one shared fate)"* — was re-measured at the
filing HEAD before this row was worked, and the realm semantics this design steals from were read
from the WAMP Basic Profile (§2 quotes) rather than paraphrased from memory.
