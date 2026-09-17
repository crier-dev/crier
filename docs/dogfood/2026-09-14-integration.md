# 2026-09-14 — Durability + Federation deep run

Focus this run: the two promises prior dogfooding left stale — PostgreSQL
durability (last verified 2026-08-09) and the federation outage contract
(`CR_FED_TOKEN` / `CR_FED_MAX_HOLD_S` / `CR_FED_QUEUE_FILE` — last exercised
pre-fix on 2026-09-08). Plus the mandatory ephemeral-bunker install leg that
no prior run had done. All work at HEAD `57034d8`. Scratch: `/tmp/dogfood-crier`.

## Topology used

```
relay-a (:18871, Postgres db `crier`)  --CR_FED_LINKS-->  relay-b (:18872)
  CR_FED_TOKEN=relayb-secret                CR_AUTH_TOKEN=relayb-secret
  CR_FED_MAX_HOLD_S=25..90                  backend: in-memory then Postgres db `crier_b`
  CR_FED_QUEUE_FILE=/tmp/dogfood-crier/a/holdq.json
agents: carol/frank/workshop on A, eve/dave on B; ed25519 keys in /tmp/dogfood-crier/{a,b}
```

## Verified working at HEAD (real use)

1. **Federated delivery with link auth (DF-CRIER-6 fix).** `POST :18871/agents/dave/inbox`
   → forwarded to B → `201` in 10ms. First attempt with mismatched token
   correctly returned `{"error":"invalid token"}` 401 to the sender — my config
   bug (`CR_FED_TOKEN` must equal the DESTINATION's `CR_AUTH_TOKEN`, one shared
   secret, not two separate values). The README documents this correctly; read
   it twice like I didn't.
2. **Signed retrieve across the hop.** eve's message delivered via A was
   retrieved at B with `X-Agent-ID/X-Agent-Ts/X-Agent-Sig` (payload
   `{"order":"pickup-parts"}` decoded from base64).
3. **Hold-and-retry (DF-CRIER-7 fix).** B down → deliver via A →
   `202 {"status":"held","id":"...","target":"eve","max_hold_s":25}` (5ms, no
   blocking) → `holdq.json` gained one item with `deadline = now+25s`,
   `next_attempt_at = now+2s`, and the connection-refused error string.
4. **Crash recovery.** Kill A while the item is held → restart A → the item is
   reloaded from the queue file and flushed on the next sweep. Proven
   end-to-end in the final scenario: A held, A killed mid-hold, BOTH relays
   restarted (B's registry restored from Postgres db `crier_b`), eve still
   registered (HTTP 200), sweep flushed → eve's inbox returned
   `{"recovery": "final-proof"}`. At-least-once semantics: exactly one copy.
5. **Terminal failure reports.** Budget expiry with sender=`workshop` →
   exactly one `FEDERATION_FAILED` entry in workshop's signed inbox
   (`{kind:error, code, message_id, target, sender, attempts:4, error}`).
   All-links-404 (B restarted, in-memory registry wiped, eve re-registered
   seconds too late) → correct distinct report `agent not found on any linked
   relay`. Both outcomes are durable inbox entries, not lost drops — the
   DF-CRIER-8 style silent drop does not exist on the fed path at HEAD.
6. **PostgreSQL durability re-proven at HEAD.** carol/frank + an undelivered
   message both survived a relay restart (migrations auto-apply; DB was a
   throwaway `postgres:16-alpine` container, port 5438, no real data touched).
7. **Bunker fresh-machine install (first ever install leg for crier).**
   agent 3d7624ca on las-bunker-03: public clone of the documented
   `https://github.com/crier-dev/crier.git` URL at 57034d8 → toolchain
   download + `make build` = **58s** → smoke: health 200, register 201,
   deliver 201, retrieve returned the payload base64 intact. Destroyed after.

## The one real contract trap found

`FEDERATION_FAILED` reports route to the **sender's inbox** — and the sender
is taken from the deliver body's `sender` field. My first probe used
`"source":"workshop"` (a guessed field): the message held fine, the budget
expired, the hold manager ran `fail()` — and the sink refused to deliver
(`no sender on message`, one log line, outcome gone). `openapi.yaml` documents
the held path but never says a terminal report REQUIRES `sender` on the
original deliver. Filed as **DF-CRIER-129** (doc/contract gap) and
**DF-CRIER-130** (no handler-level E2E test covers body→HoldMeta→report,
which is why the trap survived).

> **Fixed (DF-CRIER-129, 2026-09-17).** The paragraph above is the measurement
> as taken on 2026-09-14 and is kept as history. The contract it found is gone:
> a deliver request that names no `sender` is no longer held at the source — the
> source relay answers `502 {"error":"FEDERATION_FAILED","message_id":…,
> "target":…,"attempts":…,"detail":"…no sender…"}` synchronously and enqueues
> nothing, so the outcome can no longer be lost (pinned by
> `TestHandleDeliverFederationSenderlessTransientIsNeverHeld` and
> `TestForwardOrHoldSenderlessTransientIsNotHeld`).

## Right way (operator recipe that worked)

```bash
# destination relay (B) — token = its own CR_AUTH_TOKEN
CR_AUTH_TOKEN=relayb-secret ./bin/crier -port 18872
# source relay (A) — CR_FED_TOKEN MUST equal B's CR_AUTH_TOKEN
CR_DATABASE_URL=postgres://... CR_FED_NAME=relay-a \
CR_FED_LINKS=http://localhost:18872 CR_FED_TOKEN=relayb-secret \
CR_FED_MAX_HOLD_S=60 CR_FED_QUEUE_FILE=/tmp/holdq.json \
  ./bin/crier -port 18871
# deliver: include "sender" if you ever want failure reports back
curl -X POST localhost:18871/agents/eve/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{...},"sender":"workshop"}'
```

Cleanup: both scratch relays killed, `crier-df-pg` container removed, bunker
agent 3d7624ca destroyed. No repo data touched; no visibility/permission
changes made anywhere.
