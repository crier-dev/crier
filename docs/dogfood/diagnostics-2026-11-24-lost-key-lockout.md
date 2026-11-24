# Diagnostic trail — the lost-key tombstone (DF-CRIER-515)

Tick `crier-dogfood-2026-11-24-19-47-57`, tree `b48e1aad`. This is the
"why it is built this way, what it costs, what would fix it" record behind the
P1 row. No code was changed — findings are the foreman's work.

## How it works

Agent identities in crier are **key-immutable by design**: the first registration
of an id binds its ed25519 public key forever. The registry handler
(`internal/registry/handler.go`, code `CodePublicKeyMismatch`) refuses a second
registration that presents a different key with 409
`public_key_mismatch`, and its message prescribes the remedy: *"delete the
agent first (DELETE /agents/{id}) or register under a new id."* The design is
sound as a security posture — a lost key must not let an attacker silently
rebind an identity — and `internal/registry/reregistration_test.go` enforces
the refusal.

## What actually happened (live reproduction)

1. `docker compose up -d postgres` on this host resurrected the named volume
   `crier_pgdata`, last written 2026-09-30 by an earlier run (dogfood fires do
   this too — ids `alice`/`bob` are the conventional test ids). Volume
   persistence is the durability feature working as designed; the *surprise*
   is that "from nothing" is not from nothing on a shared host.
2. Fresh `crier keygen` for `alice` (new key) → `POST /agents` → **409
   `public_key_mismatch`**. The transcript `round_trip.py` exits here (rc=1).
3. Following the error's own remedy: `DELETE /agents/alice` signed with the
   **new** key → **401 `signature verification failed`** — DELETE requires the
   signature of the *registered* key, which no longer exists anywhere (scratch
   is pruned on a timer).
4. Operator surface: none. The management writes
   (`POST /principals|bindings|grants|agents/{id}/class`) are permissions-only
   (README delivery-ACL table); no env, no CLI, no documented procedure deletes
   a registry row. The only path left is raw database surgery on the durable
   backend — undocumented, and exactly what a non-maintainer cannot do.

Net: the error message sends the user to a remedy that **cannot succeed in the
only situation the error occurs** (different key presented). The identity is a
permanent tombstone, and `GET /agents` serves it as `stale` forever.

## Cost

Any durable crier deployment where (a) test ids collide (`alice`, `agent-1`),
(b) keys live in pruned temp dirs, or (c) operators lose key material. The
documented happy path — clients/README's own example uses `alice` — fails
on first contact for anyone reusing a shared backend. This fire reproduced it
with 2026-09-30 residue; the next newcomer on this host will too.

## Candidate fixes (evaluated, none executed)

- **Operator-scoped delete** (admin token like `CR_PERMISSIONS_ADMIN_TOKEN`)
  for registry rows — smallest change, keeps key immutability for normal
  actors, gives the operator an escaped hatch. Best fit for the row.
- **Key rotation flow**: a registration signed with the OLD key may replace
  its identity's key (proof you *have* the old key is proof of ownership —
  distinct from the attacker case, which presents a new key unsigned).
- **Stale-identity sweep**: TTL on expired/never-seen registry rows —
  touches live semantics, riskiest of the three.

## The right way for users TODAY (until fixed)

Register under a NEW id when the old key is lost — `crier keygen -out k -id
<new-id>` and move on. Do not try to fight the tombstone client-side.

## Live captures

Raw HTTP bodies for the 409/401 arm and the stale `GET /agents` listing are in
`.coding-hermes/evidence/crier-dogfood-2026-11-24-19-47-57/` (register-409.out,
delete-with-new-key.out, agents-stale.json).
