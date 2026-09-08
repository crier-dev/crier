# 2026-09-08 — Integration report: webhook delivery + federation (real-use run)

This run exercised the two primitives no prior dogfood had touched: **push
delivery to an agent's own HTTP endpoint** (the webhook driver) and
**relay-to-relay federation** (`CR_FED_LINKS`). Everything below was done by
USING a live server, not reading tests. Verdict context: 🟡
PROMISING-BUT-ROUGH — the core push machinery is genuinely good; federation
and failure-path signaling are where the gaps are.

## Setup that worked (reproduce it)

```bash
make build
# Relay A: auth ON (documented posture) + outbound HMAC signing + fast timers
CR_AUTH_TOKEN=<bearer-token> CR_WEBHOOK_SECRET=<hmac-secret> \
CR_FED_LINKS=http://127.0.0.1:8898 CR_FED_NAME=relay-a CR_FED_MAX_HOLD_S=60 \
CR_WEBHOOK_REDELIVER_S=5 ./bin/crier -port 8899
# Relay B (for federation leg)
./bin/crier -port 8898
```

A controllable receiver (log every POST, return mode-selectable responses) is
the key tool: it turns "did the envelope arrive right" into greppable
evidence. Write one before you start — `examples/federation-demo/echo_webhook.py`
is a starting point.

## Verified-good surface (what a new user can rely on)

| Flow | Evidence from this run |
|---|---|
| Register agent with webhook (blocking, generic-custom) | 201, webhook config echoed by `GET /agents/{id}` |
| Blocking deliver → push → reply extraction | deliver returns `{"id", "reply", "session_id"}` in 16ms; receiver got envelope with `X-Crier-Event/Agent/Session/Signature/Retry` headers, `crier.{version,message_id,session_id,thread_id,delivery_mode,sender,kind,guard}` body |
| HMAC verification (receiver side) | `X-Crier-Signature` == hex(hmac_sha256(CR_WEBHOOK_SECRET, raw_body)) — verified in Python, exact match |
| Session/thread context | `X-Crier-Session` header + `crier.session_id` + echoed back in the deliver response |
| `PATCH /agents/{id}` self-configuration | Signed with the agent's ed25519 key (`"PATCH\n/agents/{id}\n<ts>"`), swapped webhook URL live; next deliver hit the new endpoint |
| openai-compatible template | `payload.text` → `messages:[{role:user,content:...}]` + default `model`; reply extracted via `$.choices[0].message.content`; a wrong reply body → clean 504 with the exact missing-key path |
| Async mode | 202 immediately, guard verdict in the response body; receiver POSTed seconds later |
| Batch mode | 5 batch-mode deliveries → exactly ONE POST (`X-Crier-Event: batch`, `{"messages":[...5 envelopes]}`), worst-case guard verdict in headers, per-message guard metadata inside each envelope |
| Cross-relay delivery | deliver on A to an agent registered on B → lands in B's inbox with lease + guard metadata intact; agent tables exchanged via `GET /fed/peers` |
| Cross-relay blocking webhook round-trip | A→B→B's-webhook→reply→A→sender, same `message_id` end-to-end, 1.7ms |
| Guard fail-open | no `DEEPSEEK_API_KEY` → delivery proceeds, `X-Crier-Guard-Error: true`, reason "no provider api key" — visible on the wire AND in `crier.guard` |

Time-to-first-success: ~2 minutes from `make build` to a full signed blocking
round-trip (register → deliver → reply), following README + openapi.yaml.

## Where it broke (the receiver-side view)

1. **Federation has no auth (DF-CRIER-6).** A forwards the delivery POST to
   the link bare — no Bearer, no shared secret header. Against an
   auth-enabled relay B, the sender sees `401 {"error":"missing Authorization
   header"}` in under 1ms. Spec §8 promised "outbound authenticated link
   (shared secret header v1)"; no `CR_FED_*` secret env exists. Workaround
   today: only federate with token-less relays (LAN-only).
2. **Link down = silent instant drop (DF-CRIER-7).** Kill B, deliver to its
   agent via A → `404 {"error":"agent not found"}` in 0.5ms. No
   `CR_FED_MAX_HOLD_S` hold, no durable queue at source, no ERROR after.
   Message lost. (The cached agent table still lists B's agents after B dies
   — that's why A even tries.)
3. **Async retry exhaustion = silent drop (DF-CRIER-8).** Always-500
   endpoint: attempts at 30s cadence (`X-Crier-Retry` 2,3,4,5 observed), then
   `webhook: delivery dropped (retries exhausted)` in the server log and…
   nothing. Sender's inbox empty forever. Spec §3/§4 promised an ERROR
   `WEBHOOK_FAILED` to the sender.
4. **Per-agent `retries` ignored (DF-CRIER-9).** Agent registered with
   `retries:3`; server did 6 attempts (default 5 → ×2 initial+retries?).
   `CR_WEBHOOK_REDELIVER_S=5` was also ignored (cadence stayed 30s).
5. **Envelope sender shape drift (DF-CRIER-11).** Wire sends
   `"sender":"agent-x"`; spec §3 draws `{"sender":{"agent_id":...}}`.
6. **`GET /fed/peers` lists self (DF-CRIER-12)** — and shows the local relay
   under a `localhost:8899` name it never configured.

## The right way, given the above

- Treat webhook push as **production-ready for the success path** (blocking,
  async, batch, templates, HMAC, self-config via PATCH).
- Treat federation as **single-hop LAN plumbing**: no auth, no failure
  durability. Don't build multi-hop or WAN topologies on it yet.
- For fire-and-forget sends where loss matters: don't rely on async webhooks
  yet — deliver to a plain inbox and pull, or wrap sends with your own
  correlation + timeout and re-deliver on silence.
- Verify guard behavior with a deliberately invalid `DEEPSEEK_API_KEY`
  before trusting `allow` verdicts: fail-open means "errored allow", visible
  as `X-Crier-Guard-Error: true` / `errored:true` in `crier.guard`.

## Fixed since the last run (verified live here)

- **CR-GAP-014 closed:** `POST .../inbox/ack` with `lease_id` only → **400**
  now (was a silent 204 no-op in the 2026-08-09 run). Correct ack with
  `message_ids` → 204, `queue_depth:0`. The README's ack contract now
  matches reality.
