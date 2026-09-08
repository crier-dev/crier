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
  (request-level or agent default). Failure → 504 with last status.
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
4. **Wrong reply shape for the template** → 504 with a GOOD error message
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
