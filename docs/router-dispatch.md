# Router dispatch: model/provider hints, inbox delivery, correlated replies (CR-FEAT-036)

Crier is the transport between a scheduler that dispatches a lane and the
consumer that runs it. This page documents the ONE contract those two sides
share when the consumer is a **task-router in a bunker** — a router that cannot
be reached inbound and therefore POLLS its own Crier inbox — and the contract a
consumer that does not use a router at all shares with the same endpoint.

Crier transports `model` and `provider`; it never chooses them. It delivers to
the intended inbox under the deployment's delivery ACL, hands the message back
exactly as it was delivered, and carries the Crier correlation a result answers.

```
scheduler ──POST /agents/{id}/inbox──▶ Crier ──durable inbox──▶ poll/lease
    (dispatch: payload + optional model/provider)                    │
                                                                     ▼
                                                       bunker task-router
                                            (interprets hints, selects a pair,
                                             invokes the Hermes Gateway)
    ▲                                                                │
    └──── poll/lease ◀── Crier ◀──POST /agents/{id}/inbox─── (result + reply_to)
```

## Scope

In scope for Crier, and implemented here: the three optional envelope members
below, their preservation on every read surface, inbox delivery, and the
authorization that decides whether a given sender may deliver to a given inbox.

Explicitly NOT in scope, and deliberately absent from this component:

- prompt analysis and model/provider SELECTION (the task-router's work);
- invoking the Hermes Gateway (the task-router's work);
- scheduler target/pool/usage registration (the scheduler's work);
- any scheduler task identifier in the envelope. Correlation uses Crier's own
  message id — the `id` a delivery returns and a poll hands back.

## 1. The three optional members

They ride the deliver request
(`POST /agents/{id}/inbox`, or `POST /capabilities/{capability}/inbox`)
and appear on inbox reads of the stored message.

| member | forms a caller may send | what Crier does |
| --- | --- | --- |
| `model` | explicit string, blank string, JSON `null`, or absent | transports it; never fills in a value |
| `provider` | explicit string, blank string, JSON `null`, or absent | transports it; never fills in a value |
| `reply_to` | a Crier message id or correlation id, or absent | stores it with the message; never routes or authorizes on it |

An explicit value survives delivery **byte for byte**: Crier does not trim it,
does not case-fold it, and does not validate it against any catalogue of models
or providers. The bus does not know which model names exist, and a bus that
rejected one would be a selector in disguise.

Absent is the **canonical default**: a dispatch that names no model has no
`model` member on the stored message, which is exactly what every
pre-CR-FEAT-036 delivery looked like (`omitempty`, so nothing new appears for a
sender that does not use these members). A JSON `null` means the same thing — a
member with no value is no value — so it resolves to that one canonical default
instead of retaining a separate null form. A blank string (empty or entirely
Unicode whitespace) is preserved exactly as sent and is still DEFAULT for
selection purposes. Thus omission is the preferred canonical spelling, not a
promise that every default input is rewritten to omission: consumers must
recognize both omitted members and present blanks. This is identical in the
HTTP poll, PostgreSQL JSONB storage, and RemoteStore forwarding paths.

A member that is neither a string nor `null` (`"model": 5`, `"model": {}`) is
refused with `400 {"error":"invalid json"}` before anything is dispatched or
stored: a selection the bus cannot carry must not look like one it did.

## 2. The four selection cases (the consumer contract)

The consumer — a task-router, or the caller itself — reads the pair off the
message and decides. All four combinations are ordinary message states:

| `model` | `provider` | the consumer that receives it |
| --- | --- | --- |
| explicit | explicit | uses that pair |
| default | explicit | selects a model within that provider |
| explicit | default | selects a compatible/best provider for that model |
| default | default | selects the pair from the prompt |

Crier's whole contribution to that table is that every cell arrives exactly as
the scheduler wrote it, so the case is decidable from the message alone. There
is no Crier default model, no Crier default provider, and no Crier-side
selection step anywhere on the delivery path.

## 3. Mode A — explicit hints, no task-router in the path

A caller that already knows which pair it wants sets both members and reads them
off its own consumer's poll. Nothing in Crier requires a router to be involved:
the members are additive, optional and inert. This is the mode a non-router
consumer uses, and it is the reason an explicit value is never rewritten.

## 4. Mode B — router-delegated selection

When one or both members are default, the choice is still open on the message.
A task-router in a bunker polls its own inbox (see §5), reads the two members,
applies the table in §2 against the dispatch prompt, and runs the lane. Crier
does not analyze the prompt or choose a pair — the router's result comes
back as a normal correlated delivery (§6).

## 5. Delivery, poll, lease, acknowledgement

The bunker router polls because it accepts no inbound connection. That means
everything Crier owes it must be visible on a retrieve:

<!-- doccheck -->
```bash
curl -s -X POST $BASE/agents/lane-dispatch-probe -H 'Content-Type: application/json' -d '{"id":"lane-dispatch-probe","public_key":"abababababababababababababababababababababababababababababababab"}'   # -> 201
curl -s -X POST $BASE/agents/router-dispatch-probe -H 'Content-Type: application/json' -d '{"id":"router-dispatch-probe","public_key":"babababababababababababababababababababababababababababababababa"}'   # -> 201
curl -s -X POST $BASE/agents/router-dispatch-probe/inbox -H 'Content-Type: application/json' -d '{"payload":{"lane":"crier-worker"},"model":"gpt-5.3","provider":"zai-glm-default"}'   # -> 201
curl -s -X POST $BASE/agents/router-dispatch-probe/inbox -H 'Content-Type: application/json' -d '{"payload":{"lane":"crier-worker"},"model":null}'   # -> 201
curl -s $BASE/agents/router-dispatch-probe/inbox   # -> 401
curl -s -X POST $BASE/agents/lane-dispatch-probe/inbox -H 'Content-Type: application/json' -d '{"payload":{"result":"done"},"reply_to":"placeholder-dispatch-id"}'   # -> 201
curl -s $BASE/agents/lane-dispatch-probe/inbox   # -> 401
```

The steps above are replayed by `make docs-check` against a booted server, so
this recipe cannot drift from the shipped handlers. Read them as: the two
registrations are the two identities; the third and fourth deliveries are the
two hint arms (both explicit, and a `null` model with no provider — neither of
which Crier fills in); the sixth is the correlated result the scheduler
receives.

Both reads are the same call — `GET /agents/{id}/inbox`, which leases what it
returns and answers with a `lease_id` plus `queue_depth` / `leased_count` — and
both are answered **401 unsigned**: an inbox read is agent-owned, so it is
readable only by that agent's own ed25519-signed request (`X-Agent-ID`,
`X-Agent-Ts`, `X-Agent-Sig`), which is why the replayed steps above carry the
refusal rather than a body. The signed spelling, and the reason the default
configuration insists on it, are in `TESTERS.md` §2; the router signs as itself
for exactly the same reason it polls at all — nobody else may read the dispatch
it was sent.

The `reply_to` value in the sixth step is a placeholder: in a real round trip it
is the `id` the poll returned for the dispatch, and the scheduler matches the
answer on exactly that value (§6). One more step a live consumer takes is not in
the replayed recipe because its input is dynamic: work that is finished is
acknowledged with `POST /agents/{id}/inbox/ack` under the lease that claimed it
— `204`, and the message is gone. Acking a message your lease no longer covers
is `409`; acking one that is already gone is `404`.

An unacknowledged dispatch is redelivered when its lease expires, exactly as
before this feature (the lease/redelivery contract is untouched: a live lease is
never double-delivered, an expired one releases the message, and a stale lease
cannot acknowledge it). The redelivered copy still carries the scheduler's
hints.

## 6. The correlated reply

`reply_to` is provenance, not authority. A router that finishes a dispatch
answers with a normal delivery whose `reply_to` is the dispatch's Crier message
id, so the scheduler can join the result to the dispatch from its own inbox
alone — no scheduler task id is ever added to the envelope, and the value is
echoed on the stored message rather than being echoed once in a response the
poller never sees. It does not grant reach, it does not address anything, and a
delivery is admitted or refused without consulting it (§7).

## 7. Authorization: least privilege, and why a payload cannot grant it

Delivery to an inbox is decided by the verified sender identity plus the
configured delivery ACL — never by anything inside the payload. The two sends
this architecture needs are exactly two grants:

- the scheduler's identity holds `send` on the ROUTER's inbox (the dispatch);
- the router's identity holds `send` on the SCHEDULER's inbox (the result).

With the delivery ACL enabled and the scheduler, router and unrelated targets classed as `service`,
the two principals active, each bound to its own service agent, and ONLY those
two agent-scoped `send` grants, all other pairs are denied with the named
`403 DELIVERY_FORBIDDEN` refusal
(`reason: NO_GRANT`, `NO_BINDING` or `ANONYMOUS`) and stores nothing:

- the router sending to its own inbox (no self-grant);
- either identity reaching an unrelated agent;
- an agent identity alone, or an anonymous body, on a classed target;
- a delivery whose body or payload claims `reply_to`, `sender` or `as_agent`
  naming the authorized party. The authorization decision is the same with and
  without those claims — a forged correlation changes nothing, which is the
  difference between provenance and authority.

Concretely, a trusted delivery caller supplies `principal_id` plus `as_agent`.
The permission store must contain that active principal, its live binding to
the named agent, and its live `send` grant on the recipient. Setting `sender`,
`as_agent`, or `reply_to` without those records grants nothing; revoking the
stored grant refuses the next delivery of the same body. A service agent's
`X-Agent-ID` alone does not map to a principal or confer send authority.

Trust boundary: the existing delivery API accepts principal assertions from
the deployment's trusted callers; it does not authenticate a human login or
cryptographically bind `principal_id` to a delivery credential. The shared
Bearer gate, when configured, authenticates the deployment caller, not a
particular principal. Do not expose this assertion API to untrusted callers
or describe it as per-service cryptographic authentication. Inbox poll/ack
signatures separately prove the owning agent's key. This feature does not
change either established identity mechanism.

The grant contract itself (principals, bindings, grants, agent classes and how a
delivery ACL is armed) is documented in `specs/CHAT-PERMISSIONS.md` and
`docs/integration-guide.md`; this page only states the two patterns a router
deployment needs.

## 8. What this preserves for existing consumers

- A delivery that names none of the three members is unchanged: no new member
  appears on the request, the stored message, or any read of it.
- `POST /agents/{id}/inbox`, the capability-routed spelling, webhook push,
  long-poll reads, leases, acknowledgements, TTL/expiry, dead letters and
  federation fallback all behave exactly as before.
- A webhook-push delivery does not carry these members: they travel on the
  INBOX envelope, which is the envelope a polling consumer reads. A
  push-configured target is a different transport with its own envelope, and a
  router polls.
- MCP `retrieve_inbox` preserves hints and `reply_to` with the lease; use it
  with `ack_messages` when work must finish before acknowledgement. The
  convenience `get_messages` poll also exposes these members (including its
  buffered path), but retains its existing automatic acknowledgement behavior
  and is not a work-completion lease API. Existing payload-level
  `crier_reply_to` correlation is unchanged.
- ACL-disabled deployments and unclassed legacy targets keep their existing
  trust-by-reach behavior. The least-privilege pattern above requires explicit
  permission records; registering a bus agent alone does not configure them.

## 9. How this is verified

- `internal/registry/crfeat036_test.go` — the four selection cases across all
  supported spellings of the default arm, over the real HTTP surface (deliver →
  poll/lease → reply → ack), the authorization matrix above, the forged
  `reply_to` arms, and the lease-expiry/redelivery contract with hints present.
- `internal/registry/postgres_store_unit_test.go` — the stored representation
  (the hints travel as one JSONB body; SQL NULL when nothing was sent).
- `internal/registry/dispatch_audit_test.go` — the complete sender/recipient
  permission matrix, missing/wrong bindings and revoked grants, signed
  RemoteStore poll/ack with a correlated result and only completed work acked,
  plus read-only snapshot mutation regressions.
- `internal/registry/dispatch_postgres_audit_test.go` — a disposable local
  PostgreSQL instance (Unix socket only, no existing database): real 12→13
  migration with a legacy row, the serialized HTTP hint matrix, RemoteStore
  delivery, Peek/PeekInbox, and a long-poll read. Explicitly skips when local
  PostgreSQL binaries are unavailable or initdb cannot run as root.
- `internal/mcp/dispatch_hints_test.go` — serialized HTTP deliveries read
  through MCP `retrieve_inbox` and both `get_messages` paths.
- `cmd/server/dispatch_hints_test.go` — the served OpenAPI schema accepts null
  for both request hints; the generated YAML remains identical to its source.
- `docs/claims.yaml` — the anchors of this page, and the recipe in §5, are
  executed by `make docs-check` against a booted in-process server.

## Signed service-agent authorization (opt-in follow-up)

For machine-to-machine scheduler → router and router → scheduler traffic, use
`CR_SERVICE_DELIVERY_ALLOWLIST` and registered sender keys as described in
[service-delivery-auth.md](service-delivery-auth.md). This is separate from the
human-principal ACL above: `principal_id`/`as_agent` do not authorize a service
delivery, and `reply_to` is never authority. Existing deployments with no
configured service recipient keep the documented legacy behavior. The gate
does not alter the hint/default/correlation contract on this page.
