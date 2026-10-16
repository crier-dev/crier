# 2026-10-16 — Integration report: async webhook failure ladder re-proof (real-use run)

Target: crier at HEAD `839b4f2` (binary `v0.1.0-rc2-409-g839b4f2`, built fresh this run, `make build` 4.7s warm).

## The angle

The 2026-09-08 run found three webhook failure-path defects (DF-CRIER-8 silent drop on
async exhaustion, DF-CRIER-9 per-agent retries ignored, redeliver cadence ignored). All
were fixed with code + tests, but never re-dogfooded **live** — the fixes were only ever
seen by the unit tests that pinned them. This run drove the whole failure ladder against
a real server with a real always-500 HTTP sink.

## What a user does (and what happened)

Scratch environment: in-memory server on :18777 with
`CR_WEBHOOK_REDELIVER_S=3 CR_WEBHOOK_MAX_RETRIES=4`; sink on :18977 answering 500 to
every POST. Agents `df-sender` (sender) and `df-web` (webhook target, `delivery_mode:
async`, `retries: 2`), one shared keygen ed25519 key (sender needs no key to deliver,
but registration requires a public key).

| Step | Result |
|---|---|
| `crier keygen` | PASS — report names the file, prints the registration body, self-verifies sign+verify. One friction: `-out` flag exists but is absent from `--help`'s flag list; keygen help comes from the file's own doc-comment (logged below) |
| Register sender + webhook agent | 201 both; webhook config echoed by GET semantics of the 201 body |
| Deliver async WITH `sender` | 200 accept, `transport: webhook`, `delivery_mode: async` (resolved mode echoed) — 0.6ms median |
| Retries | EXACTLY per contract: attempts at 0s/3s/6s (3s cadence from `CR_WEBHOOK_REDELIVER_S`), then `delivery dropped (retries exhausted) ... retries=3 budget=2` — the per-endpoint `retries: 2` was HONORED (DF-CRIER-9 fix holds live) |
| Exhaustion notification (DF-CRIER-8) | Arrived in the SENDER's durable inbox at 6s: `{"kind":"error","code":"WEBHOOK_FAILED","message_id":<orig>,"target":"df-web","retries":3,"status_code":500}`. Retrieved via signed retrieve, `ack` 200. The silent drop is fixed when a sender is named. |
| Deliver async WITHOUT `sender` | Same retry ladder, same drop — then `webhook: failure notification skipped (no sender on exhausted item)`. Sender inbox empty at 150s. **The DF-CRIER-8 class of silent drop still exists for the documented-default caller** → DF-CRIER-320 (P1). |
| HMAC/signing | Not exercised this run (server-wide `CR_WEBHOOK_SECRET` was unset; covered by 09-08 run) |

## The finding in one paragraph

The spec (§4) says the exhaustion notification lands in the sender's inbox and that a
missing sender is "logged best-effort". That combination means: any caller following the
client docs (`sender` is optional — "the server does not require or verify it") gets the
exact silent drop DF-CRIER-8 was filed to kill. The fix direction is a durable outcome
for the no-sender case — the natural target is the TARGET agent's own inbox (the agent
whose endpoint is failing is the party that must learn about it) — plus a spec line that
states where the notification goes in every case.

## Performance (Step 2b)

`crier_client.deliver` async webhook accept, in-process, n=20 after 1 warmup:
median 0.6ms, mean 0.6ms, min 0.4ms, max 0.8ms. Cold boot to healthy <1s.
Guard error branch (no provider key) adds no measurable latency. Nothing a user
would notice → perf record row DF-CRIER-322, no profile taken.

## Install leg

`make build` 4.7s warm, binary stamps HEAD correctly. Bunker leg not run this tick —
explicit partial skip citing the 10-10 and 10-13 fresh-box passes (DF-CRIER-321).
Scratch state fully cleaned: server + sink killed, ports verified free, key file deleted.

## Friction count: 3

1. `public_key` required at registration is not stated in the deliver-facing docs a
   first-time reader reaches first (hit it on the first register).
2. `crier keygen -out` flag exists but `--help`'s flag block doesn't list it (the
   help text comes from the doc-comment prose).
3. The 401 on the sender-inbox poll loop gave no hint that retrieve needs the
   signature trio while register does not — the error text is fine once you see it,
   but the auth surface split (signed vs unsigned routes) is learnable only by trial.
