
## Dogfood Findings (2026-09-08)
Verdict: PROMISING-BUT-ROUGH
Promise: A user can start one Go server and drive an agent-to-agent bus: registry (ed25519 identities, signed self-configuration via PATCH), durable lease-based inboxes, PUSH delivery to any agent's own HTTP endpoint (webhook, blocking/async/batch, HMAC-signed, schema templates), and relay-to-relay federation (CR_FED_LINKS). This run focused on webhook delivery + federation — the two primitives no prior dogfood had exercised.
Install: SKIPPED-install-bunker — bunker host las-bunker-03 (100.69.3.13) unreachable: ssh connect timeout + ping 100% packet loss at run time; never silently passed.
Board rows: DF-CRIER-6..12 (see .coding-hermes/board/tasks.jsonl).

- [P1] DF-CRIER-6: Federation forwards deliveries WITHOUT auth credentials — link to an auth-enabled relay 401s instantly (spec §8 promised authenticated links; no CR_FED_* secret env exists at all).
- [P1] DF-CRIER-7: Fed link down = instant silent 404 drop — no CR_FED_MAX_HOLD_S hold, no durable queue at source, no ERROR frame; message lost (spec §8 promised all three).
- [P1] DF-CRIER-8: Async webhook delivery silently drops after retries exhausted — sender inbox empty, no ERROR WEBHOOK_FAILED, nothing observable except a server log line.
- [P2] DF-CRIER-9: Per-agent webhook `retries` setting ignored (agent said 3, server did 6; CR_WEBHOOK_MAX_RETRIES=5 floors it); CR_WEBHOOK_REDELIVER_S=5 also ignored (30s cadence).
- [P2] DF-CRIER-10: docs/integration-guide.md has zero webhook/federation content — wire contract only in DRAFT spec + openapi.yaml.
- [P3] DF-CRIER-11: Envelope sender shape drift — spec §3 object {"agent_id":...} vs wire plain string.
- [P3] DF-CRIER-12: GET /fed/peers lists the local relay as its own peer.
- VERIFIED-FIXED: CR-GAP-014 (ack without message_ids) now correctly rejected with 400; correct ack → 204 + queue_depth 0. README ack contract now matches live behavior.

## Dogfood Findings (2026-09-01)
Verdict: UNKNOWN-VALUE
Promise: {"entry_point":"?","promise":"(unparsed agent output) agent error: agent loop reached max_turns (25) with no final response","run_commands":[]}

- [P0] Dogfood run died at harness level — no product output at all — Both the promise and real-use records carry the same error: 'agent error: agent loop reached max_turns (25) with no final response'. The evaluating agent never produced a final answer, so the promise 
- [P1] Usability metrics are vacuous, not good — friction_count=0 and time_to_first_success_s=null are uninformative: no frictions were recorded only because the run never progressed to real use. Zero friction here means 'nothing was tested', not 's
- [P1] No evidence for or against trustworthiness — promises_held=[] and promises_broken=[] are both empty — nothing was completed, held, or broken. A re-run is required with a narrower task scope and a turn cap that guarantees a final response (or a c

## Dogfood Findings (2026-09-07)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server, default port :8767) — plus bin/crier-mcp (cmd/crier-mcp) MCP server; no specs/_index.md exists (specs/ holds AGENT-ECOSYSTEM.md, LLM-MESSAGE-GUARD.md, WEBHOOK-DELIVERY.md, ci-003b-postgresql-persistence.md)","promise":"Crier claims 

- [P2] MCP register_agent echoes zero-value status/registered_at/last_seen — Verified live: MCP tools/call register_agent returns {"status":"","registered_at":"0001-01-01T00:00:00Z","last_seen":"0001-01-01T00:00:00Z"} while GET /agents/mcp-judge-2 shows the real server-side va
- [P2] Relay publish shape undocumented in README — 401 without X-Agent-ID, body key is `event` not `payload` — Verified: POST /relay/publish without X-Agent-ID → 401; with {topic,event} → 202; with {topic,payload} → 400 {"error":"event is required"}. README relay section has no curl example; shape only in docs
- [P2] README mesh section only shows REGISTER; REQUEST/RESPONSE frame shape lives only in docs/mesh-protocol.md — Verified: two-peer REQUEST→RESPONSE round-trip works with documented shape (source/target as {"agent_id":...} PeerRefs, status_code, request_id echoing message_id) — but a wrong envelope times out sil
- [P2] MCP remote mode 401s on inbox tools unless server runs CR_REQUIRE_AGENT_SIG=false (undocumented) — Verified: crier-mcp with CRIER_HTTP_URL+CRIER_AGENT_ID against a default-signing server → inbox_stats returns 401 'missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)'; constraint ap
- [P2] demo.sh 'DEEPSEEK_API_KEY set — deliveries are LLM-guarded' note is misleading with a placeholder key — Verified: server started with DEEPSEEK_API_KEY=sk-placeholder-invalid → delivery still 201 allow with errored=true and provider 401 surfaced in reason ('Your api key: ****alid is invalid'); the guard 

## Dogfood Findings (2026-09-09)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server, default port :8767) with env-driven config (CRIER_PORT, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG, CR_GUARD_ENABLED); secondary entry: MCP server bin/crier-mcp (cmd/crier-mcp) exposing registry + inbox tools.","promise":"
- [P2] CRIER_PORT env var works on current HEAD — report friction stale — config/config.go:142-148 reads CRIER_PORT; live run of CRIER_PORT=18767 ./bin/crier bound :18767 with /health 200. The report's top friction ('env ignored, only -port works') is not reproducible on HE
- [P2] Relay publish examples omit X-Agent-ID; documented command 401s — Live: publish without header → 401 'X-Agent-ID header required for rate-limited publish', with header → 202. README has no publish curl example (prose only, line 18); docs/integration-guide.md:262 exa
- [P2] MCP remote-mode env vars and signing caveat undocumented — CRIER_HTTP_URL/CRIER_AGENT_ID exist only in cmd/crier-mcp/main.go; README has a single MCP mention (CI-007). Live: remote inbox_stats → 401 'missing agent signature headers' under default CR_REQUIRE_A
- [P2] Stale-timestamp 401 now distinct from missing-headers 401 — report friction stale — Live: stale ts → 401 'request timestamp outside allowed window (±30s)'; no headers → 401 'missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)'. The misleading-error friction is fixed
- [P2] Docker battery lacks teardown note; websocat/wscat not in prerequisites — README prerequisites list only Go/OpenSSL/xxd, yet the mesh section requires websocat/wscat (Python websockets alternative exists in integration-guide.md:257). No 'docker compose down' teardown in REA

## Dogfood Findings (2026-09-09)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server), default port :8767; secondary entry point is the MCP server bin/crier-mcp (cmd/crier-mcp)","promise":"Crier is an agent-to-agent message bus (Go) that claims a user can build an autonomous agent economy where agents register with e

- [P1] MCP stdio registry is process-local — register via MCP invisible to HTTP server, undocumented — Verified live on HEAD 11bac21: MCP stdio register_agent (13 tools via tools/list) returned 201 but the agent never appeared in GET /agents on the HTTP server (only demo-1788983260 listed); remote mode
- [P2] Relay has no worked example — publish 401s without X-Agent-ID and README omits the header — Verified: POST /relay/publish without X-Agent-ID → 401 'X-Agent-ID header required for rate-limited publish'; with the header → 202 and WS frame delivered as plain JSON. README relay section is prose 
- [P2] MCP remote-mode env vars undocumented in README — CRIER_HTTP_URL/CRIER_AGENT_ID/CRIER_AUTH_TOKEN exist only in a PRD HTML per report; grep of README confirms zero mentions. Remote mode verified working (register_agent 201, agent visible on HTTP serve
- [P2] Port-bind failure silent in background starts; quickstart hardcodes :8767 — Verified: ./bin/crier -port 8767 with the port held logs 'server failed error="listen tcp :8767: bind: address already in use"' and exits 1 — but a backgrounded start shows nothing, and README curl ex
- [P2] No MCP tools/call JSON-RPC example in README — Verified: initialize/tools/list/tools/call all work over stdio (13 tools), but README has no worked MCP example — a new user must hand-write JSON-RPC. Report's friction 5 confirmed.

## Dogfood Findings (2026-09-09)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server) listening on :8767, with an optional MCP server binary bin/crier-mcp (cmd/crier-mcp) exposing registry + inbox tools; no specs/_index.md exists (specs/ holds AGENT-ECOSYSTEM.md, LLM-MESSAGE-GUARD.md, WEBHOOK-DELIVERY.md, ci-003b-postgresql-persistence.md)"}

- [P2] README quickstart curls hardcode localhost:8767 — 8 occurrences of localhost:8767 in README quickstart (lines 107-169); demo.sh is port-agnostic via CRIER_URL (verified: CRIER_URL=http://localhost:58772 CR_AUTH_TOKEN=judge-token ./examples/demo.sh → 201/201/200/204 in 0.124s) but the README curls require manual port edits for a scratch instance.
- [P2] specs/ has 4 docs and no _index.md — ls specs/ = AGENT-ECOSYSTEM.md, LLM-MESSAGE-GUARD.md, WEBHOOK-DELIVERY.md, ci-003b-postgresql-persistence.md; no index or normative-spec guidance for new users.
- [P2] MCP remote mode undocumented in README; stdio registry invisible to HTTP server — README has only a CI-007 mention. Verified live: stdio register_agent (mcp-local-1) → HTTP GET /agents/mcp-local-1 404 (in-memory only); remote mode with CRIER_HTTP_URL/CRIER_AGENT_ID/CRIER_AUTH_TOKEN works (mcp-remote-1 → HTTP 200) but --help documents CR_AUTH_TOKEN while cmd/crier-mcp/main.go:79 reads CRIER_AUTH_TOKEN — documented remote mode 401s as written.
- [P2] Relay publish needs X-Agent-ID but README has no worked publish example — Live: POST /relay/publish without X-Agent-ID → 401 'X-Agent-ID header required for rate-limited publish'; with it → 202 and WS frame delivered as JSON. README relay section (line 282) is prose + endpoint table only; the report's requested worked relay WS example is valid.
- [P2] Guard-on-by-default friction overstated — keyless failure is fast, not 10s — Live on guard-ON instance (no DEEPSEEK_API_KEY): deliveries complete in 0-2ms with guard metadata decision=allow, errored=true, reason='guard_error: all providers failed: no provider api key' — no 10s burn (provider failure short-circuits). README lines 82-89 already prominently document CR_GUARD_ENABLED=false for keyless dev; the 10s-latency claim is stale.

## Dogfood Findings (2026-09-09)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server, default port :8767) with REST endpoints (/agents, /agents/{id}/inbox, /relay/publish, /mesh/connect/{agentID}, /health) plus a companion MCP server binary bin/crier-mcp (cmd/crier-mcp); config is env-driven (CRIER_PORT, CR_DATABASE_

- [P1] Guard-on-by-default is a first-run trap for keyless users — Following the quickstart verbatim without CR_GUARD_ENABLED=false burns up to 10s per delivery on a failing guard LLM call (X-Crier-Guard-Error) before any message lands; the flag is only discoverable 
- [P2] Relay subscribe path missing from README quickstart — Guessed /relay/subscribe?topic=X from README prose and got 404; the real path is /relay/subscribe/{topic} (path segment), documented only in docs/integration-guide.md and the API table — no relay exam
- [P2] X-Agent-ID publish requirement buried in config table — First /relay/publish returned 401 because X-Agent-ID is required when rate limiting is enabled (default 100/min); this is documented only in the config table, not the relay section, so a first-time pu
- [P2] MCP stdio framing undocumented for raw clients — crier-mcp is line-delimited JSON-RPC (bufio.Scanner), not 4-byte length-prefixed framing; a hand-rolled probe hung 60s until the source was read. Raw MCP clients need this documented.

## Dogfood Findings (2026-09-09)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server), default port :8767; optional MCP server bin/crier-mcp (cmd/crier-mcp)","promise":"This project claims a user can build and operate an agent-to-agent communication backbone for the autonomous agent economy: agents register with ed25

- [P1] MCP remote mode broken as documented — help/README say CR_AUTH_TOKEN, code reads CRIER_AUTH_TOKEN — cmd/crier-mcp/main.go:79 reads os.Getenv("CRIER_AUTH_TOKEN") but --help and README document CR_AUTH_TOKEN. Live on HEAD 96fcacf: with documented CR_AUTH_TOKEN=judge-token, remote register_agent → 401 
- [P1] Stdin-piped openssl pkeyutl -sign -rawin yields empty signature and a misleading 401 — Live: file-based helper (README/demo.sh) → 129-char hex sig, signed retrieve 200; piping the payload via stdin → 0 bytes, server 401 'missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-S
- [P2] Relay publish body schema undocumented in README — {topic,payload} 400s, {topic,event} works — Live: POST /relay/publish with {"topic":"t1","payload":{...}} → 400 'event is required'; with {"topic":"t1","event":{...}} → 202. Correct schema exists only in docs/openapi.yaml:46; README relay secti
- [P2] Default port :8767 occupied by pre-existing crier instance; no port-conflict guidance — Live: :8767 held by a 2-day-old crier (pid 3466723; four other crier instances on 28767/38767/48767). CRIER_PORT=18767 override bound cleanly and /health 200 — env override works, but README quickstar
- [P2] README prerequisite Go 1.26.6+ vs go1.26.5 — minor version drift, no failure — README:3/60/311 say Go 1.26.6 or later; go1.26.5 built bin/crier and ran the full demo round-trip without issue. Cosmetic prerequisite overstatement, not a defect.
