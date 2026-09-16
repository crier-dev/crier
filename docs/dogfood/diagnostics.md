# Crier Diagnostics Trail — how it's built, errors hit, the right way

Explanatory record from the 2026-08-09 dogfood run: how Crier is built, the
behavioral findings (not raw logs), and the right way to use/extend it.

## How it's built

- **One binary, three services**: `cmd/server` mounts relay (pub/sub), mesh
  (P2P WS), and registry+inboxes on one HTTP server (gorilla/mux). Port from
  `CRIER_PORT` (default 8767). Logging via slog (`CR_LOG_LEVEL`, `CR_LOG_FORMAT`).
- **Pluggable store**: `internal/registry` has `MemoryStore` (process-lifetime)
  and `PostgresStore` (`CR_DATABASE_URL`, migrations auto-run on start).
  `internal/mcp` runs the same store logic over MCP stdio.
- **Auth**: Bearer token middleware (`CR_AUTH_TOKEN`) on everything except
  `/health`; per-agent ed25519 request signing (`CR_REQUIRE_AGENT_SIG=true` by
  default) on inbox endpoints AND registry DELETE.
- **Lease model**: `Retrieve` marks up to N messages with a fresh lease ID
  (30s default, `?lease=` overrides, `?max=` caps at 100). `Ack(lease_id,
  message_ids)` removes messages whose IDs are found under the lease.
  `PurgeExpired` runs on a 30s ticker in `main.go`: TTL-purges expired messages
  and releases expired leases back to unleased.
- **Mesh**: server-side `handleMessage` dispatches REQUEST/RESPONSE/ERROR only.
  Routes (`message_id` → requester) are recorded on forward and consumed by
  `forwardResponse` keyed on the response's `request_id`.

## Errors hit during the run (and the right way)

1. **Ack with `lease_id` only → 204 but no-op** (CR-GAP-014).
   Why: `MemoryStore.Ack` builds `idSet` from `message_ids`; empty set → the
   "found < len(messageIDs)" check is `0 < 0` = false → returns nil. Lease is
   never validated when `message_ids` is empty. The OpenAPI spec requires
   `message_ids`; the handler doesn't enforce it. Right way: reject empty
   `message_ids` with 400, and always ack with the IDs from the retrieve
   response. Symptom to watch for: a message "disappears" from retrieve (it's
   leased, not acked) then comes back after ~30–60s.

2. **Ack with an expired/unknown lease → 400/409 depending on shape**.
   An ack with `message_ids` under the wrong lease → 409 with a precise error
   ("message X is not leased under lease Y (current lease: Z)"). This is the
   good error path — it only fires when `message_ids` is present.

3. **Mesh frames silently dropped** (CR-GAP-016). A frame with a float
   `timestamp` (vs RFC3339 string) or `"target":"beta"` (vs
   `"target":{"agent_id":"beta"}`) fails `json.Unmarshal` inside
   `handleMessage`/`handleAgentRequest` and is dropped with no ERROR, no log.
   Right way: RFC3339 timestamps, PeerRef object shapes, `request_id` in the
   RESPONSE equal to the request's `message_id` (routing keys on the latter,
   looks up by the former).

4. **REGISTER_ACK never comes** (CR-GAP-016). The client-side `register()`
   sends REGISTER fire-and-forget; the server has no REGISTER handler and
   never sends REGISTER_ACK; KEEPALIVE frames are likewise unprocessed
   server-side, and `KeepaliveAck` (listed in `docs/specs.md`) does not exist
   in code. The mesh still works for request/response — the handshake claims
   are just overstated.

5. **`DELETE /agents/{id}` → 401 with valid Bearer** (CR-GAP-015). The
   signature gate covers registry DELETE, but only "inbox endpoints" are
   documented as requiring signatures. Right way: send X-Agent-* headers.

6. **CLI**: `./bin/crier --help` works (fixed in CR-GAP-005); `./bin/crier-mcp
   --help` still starts the server (CR-GAP-017).

7. **Cross-interface**: HTTP server and crier-mcp pointed at the same
   `CR_DATABASE_URL` share state (verified). Note the HTTP server and MCP
   server do NOT talk to each other — they are two front-ends on one store.

## The right way (cheat sheet)

- Always ack with `message_ids` + `lease_id`; verify with `/inbox/stats`.
- Sign `"METHOD\n/path\nunix-ts"` with the agent's ed25519 key; keep ts within
  ±30s (clock skew kills requests).
- For durability, set `CR_DATABASE_URL`; in-memory = process-lifetime.
- Mesh raw clients: newline-terminated JSON, RFC3339 timestamps, PeerRef
  shapes, request_id == message_id echo.
- Test with auth+signing ON — that's the documented default posture; the
  demo needs `CR_AUTH_TOKEN` exported to match.

## Historical context

The board (`.coding-hermes/board/tasks.jsonl`) shows the project's own
evolution: CR-GAP-001..013 were mostly docs/UX gaps found by earlier
hunter/stand-in cycles and fixed by the foreman (CLI flags CR-GAP-005,
.env.example vars CR-GAP-008, integration demo CR-GAP-009, auth headers in
quickstart CR-GAP-010, endpoint counts CR-GAP-011, CI matrix CR-GAP-012,
CONTRIBUTING Go version CR-GAP-013). This run adds CR-GAP-014..018. The
recurring E2E-001 (live battery every 5–10 ticks) and NEVER-DONE (11-point
audit) tasks remain open by design.

---

# 2026-09-08 addendum — webhook driver + federation diagnostics

## How the push side is built

- **Delivery choke point**: `POST /agents/{id}/inbox` resolves the target's
  webhook config (set at registration or via signed PATCH). Webhook present →
  the webhook driver handles it per `delivery_mode`; absent → durable inbox
  pull model, unchanged. The LLM guard runs BEFORE both branches, once per
  message; its verdict rides outbound in `X-Crier-Guard-*` headers and
  `crier.guard` (fail-open with `errored:true` when no API key — don't
  confuse that with a real classification).
- **Blocking mode** = synchronous: the deliver HTTP call waits for the
  receiver's 2xx, extracts the reply per schema `response_map`, and returns
  `{"id","reply","session_id"}` to the sender. Bounded by `timeout_ms`
  (request-level or agent default). Failure → 502 for a permanent reject, 504
  for timeout/budget exhaustion (corrected 2026-09-16: permanent rejects became
  502 in DF-CRIER-157; 504 is timeout/budget only — see
  specs/WEBHOOK-DELIVERY.md:95).
- **Async/batch** = queue + worker: 202 immediately, POST happens off-thread;
  batch coalesces to `{"messages":[...]}` on `max_messages` or flush interval.
- **HMAC**: `CR_WEBHOOK_SECRET` → `X-Crier-Signature =
  hex(hmac_sha256(secret, raw_body))` on every outbound POST. Verified exact
  against a Python receiver in this run.
- **Federation** (`CR_FED_LINKS`) is just the webhook driver aimed at another
  crier: deliver to an unknown agent → forward the envelope to each link;
  agent tables exchange on a TTL (60s observed) and appear in `/fed/peers`
  with `via`. Replies route back because `message_id` is hop-invariant.

## Failure-path errors hit in this run (and the right way today)

1. **Federating with an auth-enabled relay** (DF-CRIER-6): A forwards bare;
   B's Bearer middleware 401s it; the sender sees the 401 verbatim. No
   `CR_FED_*` secret env exists. Right way today: LAN-only links, or put your
   own proxy that injects auth between the relays.
2. **Link down** (DF-CRIER-7): B's agent stays in A's cached table after B
   dies, so A forwards → connection refused → instant 404 to the sender.
   `CR_FED_MAX_HOLD_S` is not consulted (set 60, held 0). Message lost.
3. **Async retries exhausted** (DF-CRIER-8): the queue drops the message with
   only a server-log line; the promised ERROR `WEBHOOK_FAILED` to the sender
   does not exist. Observed cadence 30s (not `CR_WEBHOOK_REDELIVER_S=5`) and
   attempt count from the server default (per-agent `retries` ignored,
   DF-CRIER-9).
4. **Wrong reply shape for the template** → 502 (corrected 2026-09-16: permanent
   rejects became 502 in DF-CRIER-157; 504 is timeout/budget only — see
   specs/WEBHOOK-DELIVERY.md:95) with a GOOD error message
   (`response map "choices.0.message.content": missing key "choices"`) —
   this is the model error path; extraction errors are loud, unlike the
   silent drops above.
5. **Registration validation order quirk** (not filed): a bad `public_key`
   with a webhook object errors the same as without; the confusing part was
   mine — the 400 body was swallowed by `-o /dev/null` and the next call's
   404 ("agent not found: \"wb-oai\"") read like a registry bug. Lesson:
   when piping curl to `/dev/null` you are testing blind; show the body.

## Right-way cheat sheet additions

- Webhook success path is production-solid: blocking/async/batch + HMAC +
  templates + live reconfig via signed PATCH. Failure paths are not: wrap
  async sends in your own correlation/timeout, and don't federate over
  untrusted links.
- When testing push delivery, run a logging receiver and assert on the LOG
  (envelope headers/body), not just on the deliver response — the deliver
  response hides what actually went on the wire in async/batch modes.
- `CR_FED_NAME` sets your display name for OTHERS' `/fed/peers`, but your
  own listing can still show a self-entry with a `localhost:<port>` name you
  never configured (DF-CRIER-12) — don't parse that endpoint blindly.

---

# 2026-09-14 addendum — federation failure taxonomy + how the hold path is built

## How the federation hold path works (read this before touching it)

- `internal/federation/federation.go` classifies every link answer: 2xx/3xx
  relays verbatim; 404 means "try the next link"; 5xx/408/429 and transport
  errors are `TransientError`; any other 4xx is a definitive rejection that
  surfaces to the sender as-is. `forwardPass` returns
  `ErrNotFoundOnAnyLink` only when EVERY link 404s (or none are configured).
- `ForwardOrHold` = one synchronous pass; on a transient error it hands the
  envelope to the `HoldManager` (`hold.go`) and the caller answers
  `202 {"status":"held",...}`. No hold manager attached → explicit `502
  FEDERATION_FAILED` synchronously, never a silent 404.
- `hold.go` runs a retry loop (`RetryEvery` with doubling backoff capped at
  `MaxRetryInterval`, plus a wake channel so enqueues trigger an immediate
  sweep). Per item: before `deadline` → re-forward; past `deadline` →
  `fail(..., "hold budget exhausted")`. A definitive non-2xx on retry →
  `fail(..., "definitive rejection")`; `ErrNotFoundOnAnyLink` →
  `fail(..., "agent not found on any linked relay")`.
- `fail()` removes the item from the queue FIRST (exactly-once: a re-sweep can
  never double-report), logs, then calls the notifier:
  `registry.FederationFailureSink` marshals a `{kind:error, code:
  FEDERATION_FAILED, ...}` payload and `Store.Deliver`s it into the SENDER's
  inbox — the same durable inbox path as any message, so it survives restarts
  under Postgres. An empty sender ⇒ refusal + one log line (see trap below).
- `CR_FED_QUEUE_FILE` (`holdfile.go`) makes the queue an atomically rewritten
  JSON document (`0600`, `.bak` recovery). `loadHoldFile` drops structurally
  broken items with a warning instead of refusing to start.

## The trap the tests missed (DF-CRIER-129/130)

The report can only be delivered if the ORIGINAL deliver body carried
`"sender":"..."`. Unit tests cover queue mechanics and sink behavior in
isolation, but nothing drives handler → ForwardOrHold → expiry → sink over
real HTTP — so the sender-field requirement was invisible until a live run
sent a body without it and the terminal outcome vanished into a log line.
Rule for reviewers: any change to `HoldMeta` population or `HoldItem`
serialization needs a handler-level test that asserts the report LANDS.

## Installability (bunker leg, 2026-09-14)

Fresh bare-Debian agent: git + docker-compose preinstalled, NO Go. Documented
path works end-to-end as a non-root user IF you install Go to a HOME prefix —
the official tarball targets `/usr/local` (root-only) and a naive `tar -C ~`
puts GOROOT inside GOPATH (loud warning, still builds). 58s clone→toolchain→
`make build`→smoke at 57034d8. Docs polish filed as DF-CRIER-131.

## The guard's verdict pipeline (how it actually flows, 2026-09-14)

`HandleDeliver` decodes the body and assembles the envelope, then calls
`guard.Filter.Check` ONCE — webhook modes and inbox storage branch only
after the verdict, so per-message cost is exactly one LLM call. Inside
`Check`: deterministic pattern pre-scan (raw bytes, `patterns.go`) → policy
resolution (agent policies matched against `session:`/`thread:` globs,
else `CR_GUARD_DEFAULT_POLICY`, else built-in default) → payload projection
(`render.go`; JSON gets a schema-aware `path=type value` walk so attack
KEY names are visible to the LLM) → router (`router.go`: providers in
policy order, one 250ms-retry each, per-`base_url+model` circuit breaker,
8-wide semaphore, whole chain under one 10s budget) → strict verdict
validation (§3.1; anything off-schema = guard error, NOT a downgrade).
The LLM's `(decision, risk_level)` then passes the deterministic escalation
table (`block_risk` cap) — the model can never under-block below the
policy threshold. Two things worth knowing that only a live run shows:

- **The verdict LLM is a classifier, not a router of gray cases — treat its
  strictness as a tuning surface.** Across 8 live verdicts (funded
  deepseek-v4-flash, temperature 0) it proposed block 6, allow 1, sanitize
  0; mixed benign+injection content was blocked wholesale and a benign
  `{"prompt": …}` payload was quarantined. Both behaviors trace to the
  system prompt (§3.3), which never says "mixed content ⇒ sanitize" and
  never says "a control-shaped key alone is not an attack." Since the
  prompt is explicitly immutable outside a deliberate version bump, these
  are prompt-version changes, not code patches — filed as DF-CRIER-147/148.
  The sanitize machinery itself is sound: the error-path action:sanitize
  run delivered the exact §3.5 quarantine fallback with byte-perfect
  `quarantined_payload` recovery, so once the verdict side is retuned the
  rewrite path has working rails to land on.
- **Failover is observable only in the verdict metadata.** Provider skip,
  retry, and circuit events produce no log lines of their own; the single
  audit line's `provider=`/`model=` fields are the only record of who
  answered (DF-CRIER-149). When wiring a new lane, probe it with one
  delivery and read that field — don't assume the first provider served it.

Join the guard line to its delivery line via `request_id` (DF-CRIER-141's
correlation middleware now threads through the guard audit call). Sample
block line: `guard msg=<id> target=inbox-1 ... decision=block risk=high
request_id=<rid> provider=deepseek model=deepseek-v4-flash patterns="…"
ms=1644 payload_bytes=156` + a WARN `event=guard_blocked` twin.
