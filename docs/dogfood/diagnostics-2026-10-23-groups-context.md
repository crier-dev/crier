# Diagnostics — 2026-10-23 groups/context/lifecycle real-use run

## Environment resolution

- Target repo: /home/kara/crier (fresh workdir, NOT the stale prompt path).
- Board: .coding-hermes/board/tasks.jsonl (751→756 rows after filing).
- Tick id: crier-df... grep-clean across events.jsonl/tasks.jsonl/dogfood-log.md
  BEFORE starting (0/0/0 hits) — no sibling or prior-turn collision.
- Binary built fresh from HEAD 59d503b (`make build`, ~seconds warm).
- Scratch server: sqlite registry + sqlite sessions (CR_REGISTRY_SQLITE_PATH /
  CR_SESSION_SQLITE_PATH set explicitly), guard off, sig enforcement ON,
  port 18790 (8767 held by the live fleet crier; TESTERS.md's port-conflict note
  covered this). Launched as a terminal(background=true) process — shell `&`
  is refused by the harness approval gate; real pid captured via pgrep, not
  the harness's wrapper pid.
- Signing helper: openssl pkeyutl -rawin staged in a scratch sign.sh (README's
  helper writes /tmp payloads; scratch-local copy avoids /tmp crosstalk).

## Errors hit, in order, with the way through

1. `created_by: unknown field "agent_ids"` — session create. Way through: the
   strict-decode error names the accepted keys; `{"agent":"atlas"}` works.
   (Friction, not filed: error message is the coach.)
2. targets[] shape hunt — `to`→400, string[]→400, `{tag}`→400, `{kind,id}`→201.
   Filed DF-CRIER-477 (docs) and, after reading source, DF-CRIER-475 (the silent
   kind:"team" drop, fanout.go default branch, session.go:149-153).
3. ack singular `message_id` → 400 `message_ids is required` — self-teaching.
4. curl -m 8 with a slow first send → HTTP:000 (timeout at 8s? actually the
   780ms outlier was INSIDE the window; the 000 was a harness-environment
   transient — retried via a staged script and it never reproduced).
5. bunker destroy refused (live processes under the agent uid) — the destroy
   gate working as designed; `-stop` + retry destroyed cleanly. A PREVIOUS
   agent's orphaned crier would have hit the same gate: verify before retrying.

## Right way (for the next user of these surfaces)

- Session send to a group: use the TEXT tag `@team:<name>` or
  `{"kind":"group","id":"<name>"}` in targets[]. NEVER `kind:"team"` (silent
  no-op, DF-CRIER-475).
- Group membership edit body is `{add:[…], remove:[…], actor:…}` (remove first,
  then add).
- The context boundary id for `since` is any message id of THIS session; missing
  → INVALID_CONTEXT_SHARE, unknown/cross-session → CONTEXT_BOUNDARY_NOT_FOUND.
- The member context view (GET participants/{type}/{id}/context) is the surface
  that answers "what did it know when it replied" — it carries the generated
  summary AND the explanation, so an integrator never needs to reverse-engineer
  the join from system_events.
- Boot with BOTH CR_REGISTRY_SQLITE_PATH and CR_SESSION_SQLITE_PATH for the
  durable two-backend posture; `/status` reads both back.
- Signing on a fresh box needs openssl >= 3 (pkeyutl -rawin); crier keygen
  avoids openssl entirely for KEYS, and the clients/ libraries sign for you.

## Evidence paths

- Raw curl captures: /var/tmp/crier-df-1023/*.json + server.log (pre-teardown;
  scratch dir wiped after the run)
- Board rows: DF-CRIER-475..479 (commit 517daa2)
- This report's sibling: docs/dogfood/2026-10-23-groups-context-lifecycle-real-use.md