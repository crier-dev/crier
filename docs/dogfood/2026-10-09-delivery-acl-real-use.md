# Dogfood integration report — the delivery ACL in real use (2026-10-09)

Surface under test: CR-CHAT-007 human identity phase 1 (principals, bindings,
agent classes, grants, the delivery ACL) at HEAD `8c316e3b`. This is the
fresh product change since the 10-01 dogfood run, so this run took the
untouched surface, not the CLI/relay pass previous runs already swept.

## The promise

specs/CHAT-PERMISSIONS.md §1.1: "the permission model must let me talk to MY
agents but NOT to YOURS" — cross-owner is default-deny, an anonymous sender is
refused on a classed target, a principal speaks AS an agent through a live
binding, grants are evaluated per request and revocation is a tombstone.

## How to arm it (the operator path, as measured)

```bash
CR_PERMISSIONS_ENABLED=true \
CR_PERMISSIONS_DIR=/var/lib/crier/permissions \
CR_PERMISSIONS_ADMIN_TOKEN=<admin-secret> \
./bin/crier -port 8767
```

Two of those three settings cannot be combined with `CR_AUTH_TOKEN` today
(DF-CRIER-297): with both the message token and the admin token set, no
request reaches the management surface — the auth middleware demands the
message token, the handlers demand the admin token, and one Bearer cannot be
both. Until that is fixed the reachable operator posture is the tokenless dev
mode plus `CR_PERMISSIONS_*`. The log line `delivery acl armed` tells you the
checker is wired.

## The scenario that worked end-to-end

Two owners (A, B), one viewer, four agents. `atlas`/`quill` classed personal
owned by A, `bolt` personal owned by B, `chatter` classed service.

1. Mint principals: `POST /principals` (201, `prin_…` ids).
2. Bind speech rights: `POST /bindings` {principal, agent} (201, `bind_…`).
3. Class agents: `POST /agents/{id}/class` {class: personal|service, owner}
   (201) — this is what ARMS the ACL per agent; unclassed agents keep the
   legacy posture (verified: anonymous delivery to `chatter` pre-classing was
   accepted, refused after classing).
4. Deliveries checked exactly per spec §6.7:
   - anonymous → classed target: `403 {"error":"DELIVERY_FORBIDDEN",
     "reason":"ANONYMOUS", "principal":"anonymous", …}` — machine-readable,
     a UI can branch on it.
   - owner A (as atlas) → own atlas: 201, message stored with
     `principal_id` recorded (T3 provenance read back on retrieve).
   - owner A (as atlas) → B's bolt: `403 reason=NO_GRANT` naming principal,
     as_agent, target and target owner in `detail`.
   - A as bolt (no binding): `403 reason=NO_BINDING`.
   - viewer bound to atlas sending as atlas: `403 reason=NO_GRANT` (a
     binding is a speech right, not a send — §6.5 held).
   - owner → same-realm service agent: allowed (§3.2 admin/owner reach).
5. Grant lifecycle: `POST /grants` {principal, subject:{type:agent,ref},
   actions:[send]} → delivery to B's bolt flips 403→201 →
   `POST /grants/{id}/revoke` → next delivery 403 again (per-request
   evaluation, no cache staleness). An expired grant (expires_at in the past)
   is inert without any revoke.
6. Capability pools (T4): `POST /capabilities/{cap}/inbox` refuses an
   anonymous sender with `reason=ANONYMOUS` at the ADDRESS (action `invoke`),
   and with a holder registered the delivery is ACL-checked against the
   RESOLVED agent — A delivering into a pool whose holder is B's personal
   agent is refused `NO_GRANT` on that agent. Fan-out cannot bypass the
   per-agent check.
7. Restart durability: the JSONL store (`permissions.jsonl`, one line per
   record, folded keep-last) survived a server restart — class records,
   bindings and principals all still enforced afterwards. Registry and
   inboxes did not (in-memory backend, documented demo-only).

## Errors hit, and what they taught

- `{"error":"payload is required"}` — the deliver body member is `payload`,
  not `body`; openapi names it, the error does not (minor, self-explanatory).
- `public_key must be 64 hex characters (ed25519)` — registration needs a
  real key; the demo's openssl one-liner is the right template.
- `409 Agent ID already registered` — keys are immutable; a mistyped key
  strands the agent (DF-CRIER-302).
- `500 store failure` on a bad role value — the one validation that answers
  as a server fault instead of a 400 (DF-CRIER-301).
- `missing agent signature headers` — retrieves require ed25519 request
  signatures; copy `examples/demo.sh`'s transcript form
  (`"<METHOD>\n<path>\n<unix-seconds>"`).

## Verdict inputs (2026-10-09)

- Works: yes — the §6.7 refusal table is implemented as specified, including
  the two NOT-BUILT rows' absence (no ASSET_FORBIDDEN / NO_TASK_AUTHORITY
  anywhere, as the spec says).
- Usable: the matrix took ~25 minutes cold with no source reading for the
  happy path; friction was all documentation (297/298/299).
- Perf: ACL-checked delivery 8.5 ms ± 0.5 warm (curl incl.; server p50 from
  its own access log 0.2–0.5 ms), refusal 8.1 ms — the check costs nothing a
  user can feel. Boot-to-healthy with the ACL store: 80–108 ms (n=5).
