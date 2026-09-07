
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
