# CR-FEAT-008 — Hermes HTTP gateway cross-backend demo

Proves the CR-SPEC-001 vision end-to-end: **any agent, of any backend, reached
through a plain HTTP endpoint**. Two agents of *different* backends collaborate
through crier webhook delivery, and the replies come back to the sender
correlated, in the same session context.

```
 harness-agent (inbox-backed — the crier-mcp harness side)
      |
      |  POST /agents/gateway-agent/inbox   (blocking, session_id, request_id)
      v
 crier server (memory backend · CRIER_PORT=18788 · webhook driver enabled)
      |
      |  POST http://127.0.0.1:18789/webhook   (hermes-http-gateway schema)
      |  X-Crier-Event: message · X-Crier-Agent: gateway-agent
      |  X-Crier-Session: sess-… · X-Crier-Signature: hmac-sha256
      v
 adapter.py — fake Hermes gateway (this demo's "api_server")
      |   verifies the HMAC signature
      |   keeps a per-session context window keyed by session_id
      |   optionally forwards the full session history to a live backend
      v
 {"choices": [{"message": {"content": "<reply>"}}]}
      |
      v  (template ResponseMap choices.0.message.content extracts the reply)
 harness-agent receives 200 {"id", "reply", "session_id", "request_id"}
```

## What the demo proves (mapping to CR-SPEC-001)

| CR-SPEC-001 section | Demonstrated by |
|---|---|
| §2 registration `webhook` object | `gateway-agent` registered with `schema_template: hermes-http-gateway`, `delivery_mode: blocking` |
| §3 outbound envelope contract | Adapter log shows the POST with `X-Crier-Event` / `X-Crier-Agent` / `X-Crier-Session` headers and a **verified** `X-Crier-Signature` (HMAC-SHA256 over the raw body) |
| §3 response contract / blocking reply | `POST /agents/gateway-agent/inbox` with `delivery_mode: blocking` returns **200** `{id, reply, session_id, request_id}` |
| §4 blocking mode | Reply extracted via the template's `response_map`; sender sees the reply inline, correlated by `request_id` |
| §5 session mapping | `session_id` flows deliver-body → envelope → `X-Crier-Session` header (spec §3) and the template's body slot; **both** messages in the same session hit the same adapter session (turn 1 → turn 2 in one context window) — see *Known gap* below for the body-slot nuance |
| §6 `hermes-http-gateway` template | The template renders `{model, messages, stream, session_id, thread_id}` and maps the reply from `choices.0.message.content` — `internal/webhook/schema.go` |
| §7 PATCH /agents | Not exercised here (covered by CR-FEAT-007 tests) — this demo focuses on the delivery path |
| §9 webhook config surface | `CR_WEBHOOK_SECRET` (signing) exercised; `CR_WEBHOOK_TIMEOUT_S`/retries use defaults |

### Blocking-reply correlation (`request_id == message_id`)

The mesh RESPONSE contract says a reply's `request_id` is the original
REQUEST's `message_id`. The HTTP blocking path preserves that contract: the
demo sends each deliver with a sender-chosen `request_id` (`req-<ts>-1`,
`req-<ts>-2`) and the 200 response echoes it, together with the server-assigned
message id (`id`, 24-hex) and the `session_id`. The run-demo.sh script asserts
the echo, so the transcript shows each reply pinned to the request that
produced it.

### Session continuity

Message 1: `session_id=sess-demo-<ts>` → adapter records **turn 1**.
Message 2 (follow-up): *same* `session_id` → adapter records **turn 2** under
the same session and (when a live backend is configured) forwards the full
turn-1+turn-2 history in one context window — so a follow-up like *"add 11 to
your previous answer"* is answered with the previous answer in context. The
adapter's `GET /sessions` endpoint is queried by the script to assert
continuity programmatically, not just visually.

### thread_id mapping (v1 note)

The envelope field `crier.thread_id` exists and the template already maps it
(`"thread_id": "{{crier.thread_id}}"`), but the v1 HTTP deliver API
(`POST /agents/{id}/inbox`) does not yet carry a `thread_id` field — thread
context on this surface rides in the **payload** (`payload.thread_id`), which
is exactly what the demo sends. The moment any delivery surface populates the
envelope's `thread_id` (e.g. the mesh bridge), it flows through the template
unchanged. The adapter echoes both the `session_id` and the `thread_id` it
receives on the wire inside every reply.

### Known gap — `{{crier.session_id}}` body placeholder renders empty

Found by this demo's first live run (transcript section "envelope session
mapping evidence"). The `hermes-http-gateway` template body contains
`"session_id": "{{crier.session_id}}"`, but `expandTemplate`/`resolvePath` in
`internal/webhook/schema.go` only descend into `map[string]any` / `[]any`
values — and `TemplateContext.Crier` is the **struct**-typed `EnvelopeMeta`,
so the placeholder resolves to the default (empty string). The same applies to
`{{crier.thread_id}}`. Repro (1 file, no deps):

```go
cfg := &webhook.Config{URL: "http://x", SchemaTemplate: "hermes-http-gateway"}
env := &webhook.Envelope{Crier: webhook.EnvelopeMeta{SessionID: "sess-x"}, ...}
b, _ := webhook.ResolveTemplate(cfg).BuildBody(cfg, env)
// b contains "session_id":"" despite SessionID:"sess-x"
```

**Session continuity in this demo therefore rides the other spec §3 channel —
the `X-Crier-Session` header**, which `postBody` sets directly from the
envelope (not through the template) and which the adapter logs + verifies on
both turns. The blocking API response's `session_id` field (server-side echo)
is likewise unaffected. Suggested fix (foreman-owned, out of scope for this
demo's hard rules): marshal the `TemplateContext` to JSON and back into
`map[string]any` inside `buildContext` (or add a struct-walking case in
`resolvePath`), plus a unit test asserting `{{crier.session_id}}` renders. The
adapter is already forward-compatible: it prefers the body slot when
populated and falls back to the header.

## Files

- `adapter.py` — fake Hermes gateway: stdlib-only HTTP server (`/webhook`,
  `/healthz`, `/sessions`), HMAC verification, per-session context window,
  optional forward to a live backend, OpenAI-shape replies.
- `run-demo.sh` — builds the crier server, starts both processes, registers
  the two agents, drives the two-session round trips + an inbox round trip,
  asserts every invariant, and tees the full transcript.
- `TRANSCRIPT-<date>.md` — real captured output of the last run (generated by
  the script, never hand-written).

## How to run

```bash
cd examples/hermes-gateway-demo
./run-demo.sh
```

Prerequisites: `go`, `python3` (stdlib only), `curl`. The script builds the
crier binary itself (into a temp dir) and needs ports 18788 + 18789 free
(override with `CRIER_PORT` / `ADAPTER_PORT`). `CR_AUTH_TOKEN` must be unset;
the demo starts crier with `CR_REQUIRE_AGENT_SIG=false` and the memory backend
(`CR_DATABASE_URL` unset), per the demo's simplification — no per-agent
signatures, no auth token.

### Live LLM or canned?

- If `DEEPSEEK_API_KEY` is present in `~/.hermes/.env` (the exact lookup:
  `grep '^DEEPSEEK_API_KEY=' ~/.hermes/.env | cut -d= -f2-`), the adapter
  forwards each message to DeepSeek chat completions — exactly **2 calls** per
  run, one per message, logged in the transcript. The follow-up question is
  answered with the previous turn in context, which makes continuity visible
  in the answers themselves.
- If `DEMO_HERMES_GATEWAY_URL` is set, the adapter forwards to that live Hermes
  gateway instead (OpenAI-compatible `/chat/completions`).
- Otherwise the adapter replies canned but session-aware (turn counter +
  history length in the reply text), so the demo is fully offline and
  deterministic — correlation and continuity are the point, not the LLM.

Exit code 0 = demo passed; every `PASS:` line in the transcript corresponds to
an asserted invariant, and any failure aborts with the failing artifact paths.

## Acceptance checklist

- [x] request delivered to gateway-agent via webhook (blocking)
- [x] blocking reply correlated back to sender (`request_id` echoed, message
      `id` returned)
- [x] second message in same session hits the same adapter session (turn 2,
      one context window)
- [x] both agents exchange at least one successful round trip (webhook round
      trips + inbox deliver/retrieve/ack)
- [x] `go build ./...`, `go vet ./...`, `go test ./... -count=1` green
- [x] `timeout 300 gitreins guard` 4/4
