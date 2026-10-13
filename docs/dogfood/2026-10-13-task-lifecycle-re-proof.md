# Dogfood 2026-10-13 — closed-fix re-proof + task lifecycle as a fresh integrator

**Target:** crier at HEAD `7c487452` (v0.1.0-rc2-383-g7c48745). Lane: `crier-dogfood`.
**Angle:** the 10-10 dogfood (DF-CRIER-303..308) drove the CR-CHAT wave at `8c316e3`; the foreman
waves at ticks 454/456 then CLOSED DF-CRIER-297 (dual-token deadlock), 299 (openapi class route),
300 (zero created_at), 301 (input validation), 304 (task lifecycle unreadable), 306 (keygen
signing) and shipped two new surfaces (409 key-immutability `768cda6`, task GET routes) — none
re-dogfooded through real use. This run re-proves the closed rows on fresh evidence and takes the
repaired task lifecycle end to end as a user would.

## Environment

Scratch server, dev build of HEAD: `/tmp/df-crier`, port 18821,
`CR_SESSION_BACKEND=sqlite CR_SQLITE_PATH=/tmp/df-crier/df.db CR_AUTH_TOKEN=msg-token-1
CR_PERMISSIONS_ENABLED=true CR_PERMISSIONS_ADMIN_TOKEN=admin-token-2
CR_PERMISSIONS_DIR=/tmp/df-crier/perms` (in-memory registry + sqlite sessions, the documented
demo posture; registry agents do not survive restart by design).

## Re-proofs (all PASS, recorded as board events 1410–1414)

| Row | What was re-proven live |
| --- | --- |
| DF-CRIER-297 (P0, closed) | Armed ACL with two distinct tokens: mint via admin token 201; message token on the same route 403 `MANAGEMENT_FORBIDDEN` naming the admin gate. Equal-token boot (`CR_AUTH_TOKEN == CR_PERMISSIONS_ADMIN_TOKEN`, ACL armed) refuses the boot with an error that names DF-CRIER-297 and both variables, in <5s. The documented production posture is reachable. |
| DF-CRIER-300 (closed) | `POST /principals` 201 body `created_at=2026-10-13T00:58:59.160914965Z`; binding body `created_at=2026-10-13T01:01:00.357925918Z`. Zero timestamps gone. |
| DF-CRIER-301 (closed) | `role:"superadmin"` → 400 `INVALID_ROLE` naming owner/admin/member/viewer; binding to unknown principal → 400 `UNKNOWN_PRINCIPAL`; agent id in the principal field → 400 `INVALID_PRINCIPAL` with the §2.1/§6.1 explanation. All before any append. |
| DF-CRIER-306 (closed) | `crier keygen` key signs via `openssl pkeyutl -sign -rawin -in FILE` (rc=0, 64-byte sig) and verifies (`Signature Verified Successfully`). |
| DF-CRIER-299 (closed) | `POST /agents/{id}/class` exercised live (2 mints 201). |

## The task lifecycle, end to end (DF-CRIER-304 fix verified + the next wall)

1. `POST /sessions` (after two shape walls, see findings) → 201.
2. `POST /sessions/{id}/tasks` `{"payload":{"text":...},"sender":"alice","targets":[{"kind":"agent","id":"bob"}]}`
   → first **403 TASK_FORBIDDEN NO_TASK_AUTHORITY** ("calling agent alice has no owner principal
   to act as") — even though the ACL is armed and alice is the session creator. Resolved by
   minting an owner principal, binding alice, and classing alice `personal` with that owner
   (3 calls, none documented together anywhere). After that: 201 with `task_id` AND `state` at
   the TOP level (the DF-CRIER-304 fix — verified).
3. `GET /sessions/{id}/tasks/{task_id}` → 200 `{task_id, state:"open", owner:null, versions:1}`.
4. claim by alice (the sender) → 201, state claimed, owner=alice, new record-version seq 3 in the
   same thread. claim by the ADDRESSEE bob → **403 NO_TASK_AUTHORITY** until bob is also classed
   `personal` with an owner. After classing: a second claim → 409 `INVALID_TASK_TRANSITION`
   ("only an open task can be claimed") — the ladder is honest.
5. complete by bob (NOT the claimer) → **201 done** (accepted); complete again → 409 ("only a
   claimed or running task"). GET-after: `state=done, owner=alice, versions=3`. Transcript shows
   the three task record-versions (open/claimed/done) in one thread.
6. Unarmed control (port 18822, no ACL): create 201, addressee claim 201, `owner=bob` —
   trust-by-reach as shipped. Coherent.

## New findings → board rows

- **DF-CRIER-314 (P2)** — the lifecycle works, but the actor the task is ADDRESSED to cannot
  claim it without the class+owner chain, and no README/integration-guide sentence teaches the
  task lifecycle at all (the only authority prose is specs §2.5/§6.11, without the prerequisite
  spelled out). Plain messages from the same sender need nothing — an undocumented asymmetry.
  Complete-not-by-claimer is accepted (spec-consistent, undocumented). Fix direction: lifecycle
  docs with a working recipe + a design call on resolving the addressee.
- **DF-CRIER-315 (P3)** — management surface is write-only: GET on /principals, /bindings,
  /agents/{id}/class all 405. The only audit view is the JSONL on disk; a management client
  cannot see what it armed.
- **DF-CRIER-316 (P3)** — `CR_DATABASE_URL=sqlite:///...` boots into a postgres migration parse
  error quoting the sqlite value (honest but hint-less; the fix is a one-line env hint), plus
  opposite unknown-field strictness: sessions reject `name` (400) while /principals silently
  accept `id`.
- **DF-CRIER-318 (P2)** — INSTALL leg REAL on bunker-mvp agent `dfcrier1013` (las-01..04 all
  offline): private repo → `git archive` tarball substitute (named as such; no permission or
  credential changes), `make build` rc=0 in **98s** on stock go1.22.2 (toolchain auto-download —
  exactly the fresh-box path INSTALL.md Path B claims), smoke `-version` + health 200 + register
  201, agent destroyed + key removed. **Residue observation:** bunker-mvp orphan counts GREW
  between post-destroy rechecks (3→7 users, 5→7 homes, 6→10 keys across ~4 min with 0 registered
  agents added) — not the documented ~60s reconciler lag; flagged for the bunker owner.
- **DF-CRIER-317 (P3)** — perf record: warm health 8.5ms±7.8 (curl-fork dominated), cold
  boot-to-healthy 97/114/231ms n=3 sqlite (69ms in-memory control; the 3-4s first readings were
  loadavg-34 contamination, cited honestly), task ops all <100ms, guard <5s. **Nothing
  user-noticeable — no profiling pass, no optimization row; this row IS the record.**

## Friction census (what a fresh user hits)

1. `CR_DATABASE_URL` guess fails with a postgres error (5 min). → 316
2. Session create body: `name` rejected, `title` is the field (openapi-only knowledge).
3. Task authority: 3 undocumented calls before the addressee/creator can act. → 314
4. No read routes on the management surface to check your own arming. → 315

## Verdict

**SHIPPABLE (behaviour)** — every closed fix from the 10-09/10-10 waves holds under real use;
the task lifecycle is now genuinely usable end to end with honest 409s and a readable state
surface. The value is real (the ACL + task lifecycle together are the multi-owner agent
workflow the specs promise); the usability gap is concentrated in ONE place: the task
authority's class+owner prerequisite is invisible in the docs a fresh user reads.

## Restart durability

Task state (open→claimed→done, owner, versions) and the ACL records survived a server restart
via the sqlite backend + permissions JSONL. Registry agents did not (in-memory, documented
demo posture). Nothing corrupted.
