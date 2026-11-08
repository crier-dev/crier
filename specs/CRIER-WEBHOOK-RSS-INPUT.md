# CRIER-WEBHOOK-RSS-INPUT — Inbound input adapters: webhook ingest + RSS/Atom poll

Status: DRAFT v1 · 2026-11-08 · Owner ruling 2026-11-08 (cr: "the owner wants WEBHOOK INPUT — ingest of external events as messages/tasks") · Rows: **INT-012** (Part A, webhook) · **INT-013** (Part B, RSS)

Series: `specs/WEBHOOK-DELIVERY.md` (OUTBOUND push — this spec is its inbound twin and changes
nothing there), `specs/CRIER-INGRESS.md` (the trusted-caller ingress this spec sits beside),
`specs/CRIER-HARNESS.md` (where a queued input goes next), `specs/LLM-MESSAGE-GUARD.md`
(the guard choke point every delivered message already passes).

## 1. Purpose

Crier today is delivery-out only. The only inbound surfaces — `POST /agents/{id}/inbox` and
`POST /ingress/messages` — authenticate a **trusted crier caller** that composes the message
itself. External systems (CI, GitHub, monitors, scrapers) cannot push events in: they would
have to hold a registry bearer token and speak crier's message schema. This spec adds two
**input adapters**, both default-off, both terminating in the EXISTING delivery path:

- **Part A — webhook ingest**: a source registers with crier, gets an HMAC secret, and POSTs
  signed event payloads to a per-source URL; crier verifies, maps payload → message, and
  delivers through the inbox path.
- **Part B — RSS/Atom poll input**: crier polls configured feeds on an interval, deduplicates
  items by stable id, and delivers each new item as a message from a per-feed identity.

**Premise verification (measured at HEAD `95fd40b0`, 2026-11-08).**

| Claim | Measured fact |
|---|---|
| Webhook OUTBOUND is implemented-but-not-run | `internal/webhook/` ships driver.go, queue.go, schema.go, batch/backoff/budget code; `webhook.Config` (`webhook.go:56-66`) carries `URL/AuthType/AuthValueRef/SchemaTemplate/CustomSchema/DeliveryMode/Batch/Retries/TimeoutMs`; registration strictness at `internal/registry/handler.go:517` (`strictWebhookMember`); spec `specs/WEBHOOK-DELIVERY.md` DRAFT v1 |
| HMAC exists only OUTBOUND | `config/config.go:593` `Secret string // CR_WEBHOOK_SECRET — HMAC outbound signing`; `crypto/hmac` appears only in `internal/webhook` tests (signature the SERVER sends). No inbound verification anywhere |
| Inbound webhook ingest is ABSENT | no `/ingress/webhooks` route (only `POST /ingress/messages`, `cmd/server/ingressroutes.go`); no per-source secret store; `internal/ingress/handler.go` routes three trusted-caller modes only |
| RSS/Atom input is ABSENT | `grep -rni 'rss\|atom'` over tracked sources matches ONLY memory-RSS probes (`scripts/load-soak.py`, `scripts/e2e-battery.sh`) and self-link tests — no feed parsing, no poller, no ETag state |
| Reuse surfaces exist | `internal/ratelimit` sliding-window (relay + global inbox-ingest budget, CR-FEAT-035, `internal/ratelimit/ratelimit.go:1-23`); `internal/ingress.InboxMessage` + `InboxWriter` narrow interface (`internal/ingress/handler.go:46-60`); guard choke point sits in the delivery path behind both |

## 2. Gates (both parts)

Both adapters follow the shipped opt-in contract (`DETECTION.md` CR-FEAT-030 pattern):

- **R1** `CR_INGEST_WEBHOOK_ENABLED` — tolerant bool, **default false**. False ⇒ the two
  webhook routes are never registered (curl 404s), no source table is read, the process is
  byte-identical to today apart from config parse.
- **R2** `CR_INGEST_RSS_ENABLED` — tolerant bool, **default false**. False ⇒ no poller
  goroutine, no feed config read. Additionally **empty feed list ⇒ poller never starts** even
  when enabled (an enabled poller with zero feeds is refused at startup: exit 1 naming
  `CR_INGEST_RSS_CONFIG_FILE empty` — fail-closed, not silently idle).
- **R3** Startup validation is fail-closed: enabled but required config missing/invalid ⇒
  exit 1 with the named env var in the message (DETECTION/DUCKBRAIN-ARCHIVE §4 pattern).

## 3. Part A — webhook ingest (INT-012)

### 3.1 Flow

```
external system                    crier                              delivery path
--------------                     ------                             -------------
1. POST /ingress/sources    ──────► 201 {source_id, secret}   (secret shown ONCE)
2. sign payload: HMAC-SHA256(secret, timestamp + "." + body)
   POST /ingress/webhooks/{source_id} ──► verify HMAC (constant-time)
                                        ├─ fail ► 401, log source_id only
                                        ├─ replay ► 409
                                        ├─ dedup (source_id, event_key) seen ► 200 {duplicate:true}
                                        ├─ map payload → InboxMessage
                                        └─ deliver via InboxWriter ─────► guard → inbox → webhook/harness out
```

### 3.2 Source registration (like agents)

```
POST /ingress/sources          {"id": "github-ci", "mapping": "passthrough", "tags": ["webhook","src:github-ci"]}
→ 201 {"source_id": "github-ci", "secret": "<64 hex>", "created_at": "..."}
DELETE /ingress/sources/{id}   → 204 (sources with undelivered queue entries are refused: 409)
GET  /ingress/sources          → [{"source_id","mapping","tags","created_at"}]  — NEVER the secret
```

- **R4** Secret: 32 bytes from `crypto/rand`, hex-encoded, returned exactly once in the 201
  body, never stored in logs, never retrievable again (same once-only discipline as the
  delivery-log key material in DETECTION.md). Stored raw in the registry store (HMAC
  verification needs the raw secret; the store is already crier's trust domain for webhook
  `AuthValueRef`-class material).
- **R5** `id` grammar: `^[a-z0-9][a-z0-9_-]{0,63}$` (tag-grammar discipline of CRIER-TAGS §2).
  Duplicate id ⇒ 409. Unknown id on ingest ⇒ 404.
- **R6** `mapping` v1 is the single enum value `"passthrough"` (§3.5). Unknown mapping ⇒ 400.

### 3.3 Ingest endpoint

```
POST /ingress/webhooks/{source_id}
Content-Type: application/json
X-Crier-Timestamp: 1762627200            (unix seconds)
X-Crier-Signature: sha256=<hex HMAC-SHA256(secret, "<timestamp>.<raw-body>)")
<raw body: the external system's event JSON, ≤ CR_INGEST_MAX_BODY_BYTES>
```

- **R7** HMAC: recompute over `timestamp + "." + rawBody` (timestamp is BOUND into the MAC —
  a stripped/edited timestamp cannot be re-signed), compare with `hmac.Equal` only.
- **R8** Replay window: `abs(now - timestamp) ≤ CR_INGEST_REPLAY_WINDOW_SECS` (default 300).
  Outside ⇒ 409 with `{"error":"timestamp outside replay window"}`. Inside but a signature
  match on a `(source_id, signature)` already ledgered ⇒ 200 `{"duplicate": true, "message_id": …}`
  (idempotent redelivery of the same event, NOT an error).
- **R9** Event identity for dedup: caller-supplied `Idempotency-Key` header if present, else
  `sha256(body)`. Ledger: the `boardctl`/CI sources need this to SURVIVE RESTART (unlike the
  process-scoped ingress ledger, `internal/ingress/idempotency.go`), so the webhook-source
  dedup table is PERSISTENT in the registry store: table `ingest_dedup(source_id, event_key,
  first_seen_at, message_id)`, index on `(source_id, event_key)`, pruned at 7 days.
- **R10** Payload→message mapping (`passthrough`): the parsed body becomes
  `InboxMessage{Author: source identity ("webhook/"+source_id, service class), Kind: text,
  Text: "webhook event from <source_id>", Payload: rawBody, Tags: source.tags,
  IdempotencyKey: R9 key}` — delivered through the SAME `InboxWriter` call
  `POST /ingress/messages mode=inbox` uses (`internal/ingress/handler.go`); guard,
  backpressure and delivery run unchanged downstream. Success ⇒ 202 `{"message_id": …}`.
- **R11** Rate limits: per-source sliding-window via `internal/ratelimit`
  (`CR_INGEST_SOURCE_RATE_PER_MINUTE`, default 60; exceed ⇒ 429 naming the source) — the
  global inbox-ingest budget (CR-FEAT-035) still sheds AFTER admission, unchanged.

### 3.4 Error catalog (Part A)

| Condition | Status | Body note |
|---|---|---|
| Unknown source id | 404 | source id echoed |
| Missing/ill-formed signature or timestamp | 400 | never echo the secret or signature |
| Bad signature | 401 | logged as `source_id + failure` — body logged NEVER (R12) |
| Timestamp outside window | 409 | R8 |
| Duplicate event key | 200 | R8 |
| Body > `CR_INGEST_MAX_BODY_BYTES` (default 65536) | 413 | refused before HMAC |
| Per-source rate exceeded | 429 | R11 |
| Downstream delivery refused (guard/budget) | 202 | queued as dead letter per existing path; ingest POST itself succeeded — the acceptance is receipt-of-event, not delivery |

- **R12** A signature failure logs source id, timestamp, and reason — never the body, never
  the presented signature (log-injection + oracle discipline of LLM-MESSAGE-GUARD).

### 3.5 Edge cases (Part A)

- **R13** Oversize body refused BEFORE HMAC (no hash work on 10 MB junk); request body read
  bounded by `http.MaxBytesReader`.
- **R14** Timestamp header non-numeric ⇒ 400 (not 409).
- **R15** Source deleted mid-flight: in-flight verify uses the row read at request start;
  delete-after-verify deliveries complete (source row is snapshot-read, not locked).
- **R16** Clock skew: server uses its own `time.Now()`; sources must NTP-sync (stated limit).

### 3.6 Wiring (Part A)

- Env: `CR_INGEST_WEBHOOK_ENABLED` (R1), `CR_INGEST_REPLAY_WINDOW_SECS` (int, default 300,
  R8), `CR_INGEST_MAX_BODY_BYTES` (int, default 65536, R13), `CR_INGEST_SOURCE_RATE_PER_MINUTE`
  (int, default 60, R11).
- Routes registered in `cmd/server` only when the gate is on; new package `internal/ingest`
  owns handler + dedup store access (imports `internal/registry` for `InboxWriter`-equivalent,
  `internal/ratelimit`).

## 4. Part B — RSS/Atom poll input (INT-013)

### 4.1 Flow

```
poller tick (per feed, interval + jitter)
  → conditional GET (If-None-Match / If-Modified-Since)
  ├─ 304 → update last_fetch, done (no delivery)
  ├─ 200 → parse RSS2.0/Atom → for each item: item_id = link ∥ guid ∥ sha256(title+pubDate)
  │         ├─ id in feed's seen-set → skip
  │         └─ new → deliver message via inbox path (IdempotencyKey "rss:<feed-id>:<item_id>")
  └─ error → consecutive_failures++, backoff, never crash, never block the poller loop
```

### 4.2 Feed configuration (per-feed opt-in)

`CR_INGEST_RSS_CONFIG_FILE` — JSON array; absent/empty file ⇒ R2's fail-closed refusal:

```json
[{"id": "crier-releases", "url": "https://github.com/crier-dev/crier/releases.atom",
  "interval_secs": 300, "deliver_to": "board-manager", "tags": ["rss", "feed:crier-releases"]}]
```

- **R17** `id` grammar = R5. `url` must be https (refuse http at startup, named feed).
  `interval_secs` default 300, minimum 60 (smaller ⇒ startup refusal naming the feed);
  ±10% jitter per tick. `deliver_to` = an existing agent id; absent at startup ⇒ refusal
  naming the feed. Unknown keys ⇒ startup refusal (strict config).
- **R18** Per-feed agent identity: at startup the poller ensures an agent row
  `id = "feed-" + feed.id` exists (`POST /agents` self-registration semantics — public key
  generated and stored by the server, service class); every item is delivered with that
  author so permissions, tags and transcripts attribute the feed, not "anonymous".

### 4.3 Conditional GET + state

- **R19** Persisted per feed (`feed_state` table: `feed_id, etag, last_modified, last_fetch,
  consecutive_failures, next_poll`): send `If-None-Match: <etag>` / `If-Modified-Since:
  <last_modified>`; 304 ⇒ advance schedule only. State survives restart (SQLite/Postgres —
  same backend selection as the registry store).
- **R20** Feed state has a writer from day one: `last_fetch` is written on EVERY completed
  request (200, 304, or error) — a healthy-but-quiet feed shows a fresh `last_fetch`, never
  an aging one.

### 4.4 Parsing + dedup

- **R21** `encoding/xml` stdlib; RSS 2.0 `<item>` and Atom `<entry>`; item text =
  `title` + optional `link` + summary (HTML/CDATA stripped to plain text, whitespace
  collapsed); per-item cap `CR_INGEST_RSS_MAX_ITEM_BYTES` (int, default 4096) — longer
  summaries truncated with `…`, title never truncated beyond the cap.
- **R22** Item identity (first present wins): `<link>` → `<guid>` → `sha256(title + pubDate)`;
  seen-ids kept per feed as a bounded set (most recent 1000, in-memory loaded from
  `feed_state.seen_ids` JSON, persisted per tick). Delivery ALSO carries
  `IdempotencyKey = "rss:<feed-id>:<item_id>"` so the delivery path's own idempotency is the
  second net if the seen-set loses a race (two pollers never run for one feed: single
  goroutine per process, stated limit).

### 4.5 Failure backoff

- **R23** Per-feed `consecutive_failures`: next interval multiplies ×2 each failure, capped at
  32× base; any 2xx (including 304) resets to 1×. A feed never leaves the rotation; a feed
  with ≥10 consecutive failures logs one `feed degraded` line per failure (rate-limited) and
  is otherwise silent. DNS/timeout/TLS errors are failures; a 200 with unparsable body is a
  failure (named `parse_error` in the log line).

### 4.6 Error catalog (Part B)

| Condition | Behaviour |
|---|---|
| 304 | R19 — schedule advances, no delivery |
| 200 unparsable | R23 `parse_error`, backoff |
| 200 missing both link and guid | item_id falls to hash (R22); empty title+date ⇒ item SKIPPED with one logged line |
| deliver_to agent unregistered at delivery time | existing delivery-path refusal → dead letter (not a poller crash) |
| Feed deleted from config | state row kept (idempotent re-add); feed with no row is simply not polled |

### 4.7 Wiring (Part B)

- Env: `CR_INGEST_RSS_ENABLED` (R2), `CR_INGEST_RSS_CONFIG_FILE` (path, R17),
  `CR_INGEST_RSS_MAX_ITEM_BYTES` (R21). Poller goroutines started in `cmd/server` main wiring
  only when R2 passes and the feed list is non-empty; each feed = one goroutine with its own
  ticker; graceful shutdown stops all tickers and persists state.

## 5. Testing

| Test | Expected |
|---|---|
| register source → 201, secret 64 hex; GET /ingress/sources omits secret | R4 |
| POST signed body → 202 + message delivered (inbox read back shows payload + tags + author `webhook/github-ci`) | R10 |
| tampered body / bad key / stripped timestamp → 401; stale timestamp → 409; same body twice → 200 duplicate, ONE inbox message | R7, R8 |
| body 65537 bytes → 413, HMAC never computed (assert via counter) | R13 |
| source rate 61/min → 429 on the 61st | R11 |
| gate off → both routes 404; server route census byte-identical to main | R1 |
| RSS fixture server: first GET 200 two items → 2 messages; second GET (same state) 304 → 0; new item guid → exactly 1 message | R19, R22 |
| restart between polls with persisted etag/seen-ids → no redelivery | R19, R22 |
| fixture returns 500 ×3 then 200 → intervals 1×,2×,4×, then reset 1× | R23 |
| feed config https/http, bad interval, unknown deliver_to → startup exit 1 naming the feed | R17, R3 |

## 6. Non-goals

- No change to `specs/WEBHOOK-DELIVERY.md` outbound behavior, envelope, or `CR_WEBHOOK_SECRET`.
- No PubSubHubbub/WebSub push for feeds; no feed autodiscovery; no OPML import.
- No payload-mapping DSL beyond `passthrough` (a GitHub-Events-style template mapper is a
  follow-up row only if the owner asks).
- No per-source ACL into sessions: v1 delivers to agent inboxes only (session delivery goes
  through `CRIER-INGRESS.md` surfaces by an already-authenticated caller).

## 7. Open questions for review quorum

1. **Q-A1** Single `X-Crier-Signature: sha256=…` header (this spec) vs. ALSO accepting
   GitHub's `X-Hub-Signature-256` alias for drop-in CI compat. Alias costs one string
   comparison; refusing keeps the surface closed. Default: refuse.
2. **Q-A2** Secret rotation command shape: `POST /ingress/sources/{id}/rotate` (new secret,
  old valid for a grace window) vs. delete+recreate. Default: rotate with a 24h grace window,
  deferred unless a source actually needs zero-downtime rotation.
3. **Q-B1** May `deliver_to` be a session/template (ingress `new_session`) in v1 instead of
   inbox-only? Default: inbox-only (R-non-goal §6 row 4) — reopen if a quorum seat shows a
   concrete feed→channel use.
4. **Q-B2** Replay window 300s: too tight for batched/queued senders (CI retries after network
   partition can exceed 5 min)? The persistent dedup ledger (R9) already absorbs re-fires, so
   the window can stay tight. Default: 300s.
