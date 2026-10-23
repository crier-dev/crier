# Dogfood: named groups + late-join context + lifecycle — real-use run 2026-10-23

Tick: crier-dogfood-2026-10-23-05-27-03 · HEAD 59d503b (v0.2.0-78-g59d503b-dirty) · scratch server `:18790`, sqlite registry + sqlite sessions, signature enforcement ON, guard off.

## Promise tested

The deltas since the last dogfooded commit (2026-10-20c, edfec90): the named-group
roster API (daecd07, DF-CRIER-449), late-join context-share records (3784174,
DF-CRIER-447), context-boundary validation on all write paths (DF-CRIER-472),
and the session close/reopen lifecycle with actor events (c6f3e07, DF-CRIER-446).
Promise under test (README §3-§6, specs/CHAT-SESSIONS.md §§2.3/4.5a/4.6):
*"A user can create a session, address a curated agent group, add members with an
explicit context-share decision, and close/reopen the session — every decision
recorded, every skip named."*

## How it was used (real consumer path)

1. `crier keygen -out alice.key -id alice -json` ×4 (atlas, worker-a, worker-b,
   latecomer) — no openssl, no xxd needed, `-json` gives the registration body
   verbatim. Friction 0.
2. `POST /agents` ×4 — 201 each (avg ~4ms).
3. `POST /sessions` — first attempt 400: `created_by: unknown field "agent_ids"
   (accepted: agent, principal, as_agent, system)`. The error named the accepted
   shape; second attempt `{"agent":"atlas"}` → 201, kind `channel`.
4. Three messages posted into the empty-audience session (201 each, server-side
   30-82ms; zero deliveries — see below).
5. Participants added: atlas (role owner), worker-a, worker-b (default roles),
   latecomer with `context_share:"full"`.
6. Context ladder on a second session: `since` with a real boundary (2ff986eb…),
   `since` missing boundary → 400 INVALID_CONTEXT_SHARE, unknown and cross-session
   boundaries → 400 CONTEXT_BOUNDARY_NOT_FOUND (detail names both the id and the
   session), empty-session join records `none` + reason, plain default join on a
   nonempty session records `summary` with the inclusive upper edge.
7. Materialized view: `GET /sessions/{id}/participants/agent/{id}/context` —
   share (mode + boundary), set_by, set_at, given_message_ids in seq order,
   generated_summary for summary mode (an index that names every message —
   §4.6 rule 2 honoured), explanation prose. `decided:true` on all.
8. Lifecycle: close 200 (closed_at microsecond UTC), double-close 409
   SESSION_ALREADY_CLOSED, reopen 200, reopen-open 409 SESSION_ALREADY_OPEN,
   garbage state 400 INVALID_SESSION_STATE, unsigned 401 with the DF-CRIER-333
   browser note. All in system_events with actor + seq + reason (`session.close`
   seq 6 / `session.reopen` seq 7 on the same transcript).
9. Groups: `POST /groups` (201, roster `[]`), duplicate 409 GROUP_EXISTS,
   `PATCH /groups/ops/members` `{add:[atlas, worker-a, ghost-agent], actor:atlas}`
   → 200 with the edited roster — placeholder `ghost-agent` accepted (roster is
   curated data, registration is the one prerequisite).
10. Group fan-out through a session send: body tag `@team:ops` in the payload text
    resolved to `{"kind":"group","id":"ops"}` and fanned out — receipts named BOTH
    outcomes per README §6's contract: ghost-agent refused (AGENT_NOT_FOUND with
    the id named) + worker-a accepted. Unknown group `@team:nosuchgroup` →
    recorded skip + server WARN naming the group (`group has no known roster`).
11. Inbox lease/ack: signed retrieve → lease_id + leased_count →
    `POST /agents/{id}/inbox/ack` `{"lease_id":…, "message_ids":[…]}` → 204.
    (The ack body needs `message_ids` — singular attempt 400s with an error that
    names the required key: self-teaching, no finding.)

## What held up

Every §4.6 contract leg behaved exactly as the spec paragraph says, including the
two error codes, the empty-session special case, and the materialized view's
`generated_summary`/explanation pair. The lifecycle ladder matches §4.5a status
for status. The group fan-out matches README §6 exactly (named skip for the
unregistered member, fresh roster at send time — a second send after a roster edit
went to the new set). The named groups ARE wired on the shipped group store
(`{"kind":"group"}` target); mention addressing resolves. Durability posture
(sqlite registry + sqlite sessions) read correct from `/status` fields.

## What fell over (and the row)

- **`{"kind":"team","id":"ops"}` in targets[] delivers to NOBODY, silently** —
  201, audience recorded, empty dispatch, only a DEBUG log (default level hides
  it). The documented @team: form has no targets[] equivalent; the obvious mapping
  is a trap. DF-CRIER-475 (P1). Only `kind:"group"` works.
- **Discovery of the targets[] shape cost four 400s** (to[] / string array /
  {tag} / {kind,id}) before the working form; no example anywhere shows it.
  DF-CRIER-477 (P2).
- TESTERS.md install block is two releases stale (no-assets claim vs live v0.2.0
  8-asset release incl. install.sh + SHA256SUMS — verified live). DF-CRIER-476 (P2).
- specs/CHAT-SESSIONS.md §7 claims `message_kind`/session kind are NOT BUILT
  while the same section's close/reopen paragraph ships and the wire carries both
  fields. DF-CRIER-478 (P2).
- Empty-audience first send: 201 with no body evidence nothing was delivered.
  DF-CRIER-479 (P3).
- Observation, NOT filed: re-adding an already-active member appends another
  member.add event (no 409/dedupe) — matches the append-only event-sequence model
  (§2.3 consequence 2 covers replay, §2.4 rule 5 covers re-add), but a caller that
  retries on a network timeout doubles the join event in the transcript.

## Performance (Step 2b)

- Signed session send (client: openssl+xxd forks ~21ms + curl): median 213.5ms
  wall (n=5); server-side log duration 185-412ms.
- Single explicit-target send: server 135-170ms (n=3).
- Zero-participant send (early, before members existed): 30-82ms server-side.
- GET transcript 1.9-2.0ms, GET context view 1.9-2.6ms server-side (n=3 each);
  register 4-18ms.
- One 780ms outlier on a reused-signature send — single WAL-flush event,
  subsequent identical calls 2ms; not user-noticeable.
- **Verdict: no PERF row.** The 135-410ms band is the per-member durable
  inbox+transcript write path under fan-out; a human chatting would not feel it.
  Reads are ~2ms. Nothing here crosses a user-noticeable threshold.

## Install leg (fresh box)

REAL on bunker-mvp agent `crier-df-1023` (spawned 2026-10-23T05:41Z, ttl 2h,
destroyed + verified absent after): https clone of github.com/crier-dev/crier ✓
(~60s incl network), `make build` 87.7s on stock Go (deps auto-downloaded),
`./bin/crier -version` → v0.2.0-78-g59d503b-59d503be ✓ (clean-tree build carries
the real identity), boot → /health `{"status":"ok"}` ✓, keygen+register+SIGNED
retrieve (openssl pkeyutl -rawin) 200 on the fresh box ✓. Agent destroyed,
processes stopped first (the bunker destroy gate refused a destroy that would
orphan the running server — the gate WORKING as designed), no residue.

## Verdict

**SHIPPABLE (behaviour)** — every leg of the late-join/lifecycle/group contract
held under real use at HEAD; the one P1 is a silent-wire trap in the targets[]
mapping (documented form vs implemented kinds), not a break in any shipped leg.
Time-to-first-success ~4 min from binary to first 201 (scratch env); friction
count 3 (targets[] shape ×3 attempts, in-memory-vs-durable config choice at boot,
created_by object/string asymmetry between /sessions and /groups — the last is
noted here, not filed).