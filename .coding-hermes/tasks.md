
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
