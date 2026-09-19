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
 crier server (memory backend · port chosen by the guard, first candidate 18788 · webhook driver enabled)
      |
      |  POST http://127.0.0.1:<adapter-port>/webhook   (hermes-http-gateway schema)
      |  X-Crier-Event: message · X-Crier-Agent: harness-agent (the SENDER)
      |  X-Crier-Target: gateway-agent (the agent this delivery is FOR)
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
| §3 outbound envelope contract | Adapter log shows the POST with `X-Crier-Event` / `X-Crier-Agent` (the sender) / `X-Crier-Target` (the agent the delivery is for) / `X-Crier-Session` headers and a **verified** `X-Crier-Signature` (HMAC-SHA256 over the raw body) |
| §3 response contract / blocking reply | `POST /agents/gateway-agent/inbox` with `delivery_mode: blocking` returns **200** `{id, reply, session_id, request_id}` |
| §4 blocking mode | Reply extracted via the template's `response_map`; sender sees the reply inline, correlated by `request_id` |
| §5 session mapping | `session_id` flows deliver-body → envelope → `X-Crier-Session` header (spec §3) and the template's body slot; **both** messages in the same session hit the same adapter session (turn 1 → turn 2 in one context window) — see *Session-id body slot* below for the measured body slot |
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
(`"thread_id": "{{crier.thread_id}}"`), and the v1 HTTP deliver API
(`POST /agents/{id}/inbox`) does carry a top-level `thread_id` field:
`deliverRequest.ThreadID` is declared at `internal/registry/handler.go:47-51`
(`json:"thread_id,omitempty"`) and passed straight into the webhook envelope at
`internal/registry/handler.go:584` (`webhook.EnvelopeMeta.ThreadID`,
`internal/webhook/webhook.go:128`), which the `{{crier.thread_id}}` body slot
renders — `docs/openapi.yaml:486-488` documents the field on the deliver body
(both landed in `ae71cbc`, CR-FEAT-011). This demo nonetheless sends its
`thread_id` inside the payload (`payload.thread_id` — `run-demo.sh:170,213`),
so the demo's own body slot reads empty; that is the demo's request shape, not
an API or template limitation. The moment any delivery surface populates the
envelope's `thread_id` (e.g. the mesh bridge), it flows through the template
unchanged. The adapter echoes both the `session_id` and the `thread_id` it
receives on the wire inside every reply.

### Session-id body slot (fixed by CR-GAP-037)

Found by this demo's first live run (transcript section "envelope session
mapping evidence"). The `hermes-http-gateway` template body contains
`"session_id": "{{crier.session_id}}"`, but `expandTemplate`/`resolvePath` in
`internal/webhook/schema.go` only descended into `map[string]any` / `[]any`
values — and `TemplateContext.Crier` is the **struct**-typed `EnvelopeMeta`, so
the placeholder resolved to the default (empty string). CR-GAP-037 (commit
342614b) fixed exactly that: `buildContext` now JSON round-trips the context
(`internal/webhook/schema.go:176-186`) so struct-typed values become plain map
nodes keyed by their json tags, with a unit test asserting the placeholder
renders (`internal/webhook/schema_test.go:64+`). Repro (1 file, no deps):

```go
cfg := &webhook.Config{URL: "http://x", SchemaTemplate: "hermes-http-gateway"}
env := &webhook.Envelope{Crier: webhook.EnvelopeMeta{SessionID: "sess-x"}, ...}
b, _ := webhook.ResolveTemplate(cfg).BuildBody(cfg, env)
// b contains "session_id":"sess-x"
```

**Session continuity in this demo rides both spec §3 channels** — the template's
body slot, now populated, and the `X-Crier-Session` header, which `postBody` sets
directly from the envelope (not through the template) and which the adapter logs +
verifies on both turns. The blocking API response's `session_id` field
(server-side echo) is likewise unaffected. The body slot's `thread_id` is still
empty, but that is not a template or API limitation: since `ae71cbc`
(CR-FEAT-011) the deliver API does carry a top-level `thread_id`
(`internal/registry/handler.go:47-51,584`, `docs/openapi.yaml:486-488`), and
this demo sends its `thread_id` inside the payload instead
(`run-demo.sh:170,213`) — see *thread_id mapping (v1 note)* above. The
adapter is already forward-compatible: it prefers the body slot when
populated and falls back to the header.

## Files

- `adapter.py` — fake Hermes gateway: stdlib-only HTTP server (`/webhook`,
  `/healthz`, `/sessions`), HMAC verification, per-session context window,
  optional forward to a live backend, OpenAI-shape replies.
- `run-demo.sh` — builds the crier server, starts both processes, registers
  the two agents, drives the two-session round trips + an inbox round trip,
  asserts every invariant, and tees the full transcript — which it now writes
  **outside the repo** (see *Transcript* below).
- `TRANSCRIPT-<date>.md` — the committed historical captures, real output of the
  run **on that date** (generated by the script, never hand-written). These files
  are records: current runs write to `${TMPDIR:-/tmp}` and no longer touch them
  (DF-CRIER-207), so the ones here stay exactly as captured. A transcript is a dated
  record, so one that predates a fix still shows the pre-fix behaviour:
  `TRANSCRIPT-2026-08-20.md` was captured before CR-GAP-037, so its
  `body session_id slot   : ''` line and its `See README "Known gap"` pointer
  are historical — that pointer names the section now titled *Session-id body
  slot (fixed by CR-GAP-037)* (above), which documents the fix and its repro.
  `TRANSCRIPT-2026-09-16.md` is the most recent committed capture and shows the
  fixed behaviour: `PASS: template body carried the session_id (body slot populated)`.

## How to run

```bash
cd examples/hermes-gateway-demo
./run-demo.sh
```

Prerequisites: `go`, `python3` (stdlib only), `curl`. The script builds the
crier binary itself (into a temp dir) and **chooses** both ports before building
anything: it walks a bounded candidate block — `CRIER_PORT_BASE` (default 18788)
and `ADAPTER_PORT_BASE` (default 18793), 5 candidates each (`CRIER_PORT_CANDIDATES`
/ `ADAPTER_PORT_CANDIDATES`) — names the holder of every candidate it skips, and
uses the first free one (QA-CRIER-10). A port you name with `CRIER_PORT` /
`ADAPTER_PORT` is checked and never rotated: an occupied one aborts the run
naming its holder. Every candidate occupied is a named failure listing each
attempted port and its holder. `CR_AUTH_TOKEN` must be unset; the demo starts
crier with `CR_REQUIRE_AGENT_SIG=false` and the memory backend
(`CR_DATABASE_URL` unset), per the demo's simplification — no per-agent
signatures, no auth token. `DEMO_TRANSCRIPT=<path>` overrides the capture path
(the selftest uses it so a test run cannot dirty `git status`); by default the
transcript is written outside the repo — see *Transcript* below.

## Transcript

Live output is teed into a transcript **outside the repo**:

```
${TMPDIR:-/tmp}/hermes-gateway-demo-TRANSCRIPT-<date>.XXXXXX.md    # mktemp-derived
```

Its path is printed at the end of the run (`DEMO PASSED — transcript: <path>`)
and in the run header, and it can be redirected with
`DEMO_TRANSCRIPT=/path/to/file ./run-demo.sh`. Concurrent runs get separate
files, a re-run never overwrites the previous capture, and no run writes into the
working tree — the committed `TRANSCRIPT-<date>.md` files are left in place as
historical records (DF-CRIER-207).

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
