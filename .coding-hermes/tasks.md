
## Dogfood Findings (2026-09-12)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"Go HTTP/WebSocket relay server (bin/crier, built from ./cmd/server, listens on :8767) as the primary entry point; secondary entry point is the MCP server bin/crier-mcp from ./cmd/crier-mcp. No library or cron surface — config is env-driven with -port/-db-url/-version CLI flag overlro

- [P1] crier-mcp remote mode cannot use the inbox tools under the DEFAULT signing config — Live against my own :18961 (CR_REQUIRE_AGENT_SIG default true), remote mode CRIER_HTTP_URL+CRIER_AGENT_ID: retrieve_inbox, inbox_stats and ack_messages all return isError=True, 401 'missing agent sign
- [P1] The auth variable the binary's own --help documents does not work in remote mode — ./bin/crier-mcp --help prints 'CR_AUTH_TOKEN  bearer token required on all requests', but cmd/crier-mcp/main.go:79 passes os.Getenv("CRIER_AUTH_TOKEN") to NewRemoteStore. Verified on auth-on :18962 (C
- [P1] MCP list_agents reports an auth failure as a successful empty registry — Same session, auth-on :18962 with the documented CR_AUTH_TOKEN (or a wrong CRIER_AUTH_TOKEN): get_agent -> isError=True with the 401 text ('missing Authorization header' / 'invalid token'), while list
- [P2] Wildcard subscribers are advertised in two shipped docs but rejected at subscribe time — docs/architecture.md:12 'Topic-based routing with wildcards' and docs/openapi.yaml:50 'Topic name (dot-separated, supports wildcard subscribers)' vs live: websocket GET /relay/subscribe/use31.* and us
- [P2] MCP tool inventory, relay frame/202 contract and the lease re-read are all undocumented and drifting — tools/list returns 13 tools while docs/integration-guide.md:316 still claims '8 tools'. Relay publish returns 202 with an empty body (Content-Length 0 — no id, no accepted-vs-dropped signal) and subsc
\n## Dogfood Findings (2026-09-12)\nVerdict: PROMISING-BUT-ROUGH\nPromise: {\"entry_point\":\"Two binaries: bin/crier (HTTP/WebSocket relay+mesh+registry+inbox server from cmd/server, default :8767, env config CRIER_PORT/CR_DATABASE_URL/CR_AUTH_TOKEN/CR_REQUIRE_AGENT_SIG) and bin/crier-mcp (MCP server exposing registry+inbox tools from cmd/crier-mcp)\",\"promise\":\"Promise: this\n\n- [P1] crier-mcp remote auth env var contradicts its own --help — Re-verified live at HEAD e55ce89: cmd/crier-mcp/main.go:79 reads CRIER_AUTH_TOKEN for RemoteStore, but --help (main.go:138) documents CR_AUTH_TOKEN. A user pointing the MCP server at an auth-enabled r\n- [P2] Wildcard subscribers promised but rejected live — docs/openapi.yaml:50 'supports wildcard subscribers' and docs/architecture.md:12 'Topic-based routing with wildcards', but live WS subscribe to demo.* returns 400 {\"error\":\"invalid topic\"} (relay.go:1\n- [P2] MCP tool count stale and no MCP onboarding — integration-guide.md:316 claims '8 tools'; live tools/list over stdio JSON-RPC returns 13. README has exactly 1 crier-mcp mention and no initialize/tools/call quickstart — usage had to be discovered v\n- [P2] Relay publish requires X-Agent-ID, quickstart never shows it — POST /relay/publish without X-Agent-ID returns 401 'X-Agent-ID header required for rate-limited publish' (verified live); README quickstart only covers register/deliver/retrieve/ack.\n- [P2] MCP-registered agents invisible via HTTP on default in-memory backend — Verified live: after MCP stdio register path, HTTP GET /agents on the running server lists only the HTTP-registered demo agent. Cross-entrypoint state sharing requires CR_DATABASE_URL (Postgres); docs

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"HTTP/WebSocket relay server binary bin/crier (built from cmd/server) — CLI flags -port/-db-url/-version with env-driven config (CRIER_PORT :8767, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG) — plus a second binary bin/crier-mcp (built from cmd/crier-mcp) exposing the registr

- [P1] Default guard hard-blocks payloads over 64KB with a false-positive pattern, keyless or not — Live at HEAD 709f2af: POST /agents/j1/inbox with a 70000-byte alphanumeric payload → 403 {"error":"GUARD_BLOCKED","guard":{"decision":"block","risk_level":"high","reason":"payload_exceeds_guard_cap: p
- [P1] Lease is invisible and the resulting ack error is misleading — Live: retrieve on an inbox with nothing claimable → 200 {"messages":[],"lease_id":"bab8b87c07e880a5429e45f4b10f9aa2"} (handler mints the lease id before scanning); acking that lease → 409 'message is 
- [P1] Default-backend unacked redelivery takes lease + up to one purge tick — measured 60.0s for a documented 30s lease — Live measurement: deliver → retrieve (lease) → poll every 4s → message returned at 60.01s, exactly one purge tick (cmd/server/main.go:311 ticker 30s → PurgeExpired) after the 30s lease. Root cause is 
- [P2] No work distribution between concurrent retrievers despite the 'disjoint message sets' wording — Live: 10 queued messages, two concurrent retrieves → A got 10 (lease ac8a6bbc…), B got 0 (lease 3f209be1…), stats afterwards queue_depth 11 / leased_count 11. The README bullet is literally true ('dis
- [P2] Doc-vs-code mismatches around deliver status codes, X-Crier-Agent, and optional payload — Live: blocking-webhook deliver → 200 {"id":…,"reply":{"ok":true}} (async → 202, plain inbox → 201) while the README quickstart shows 201; X-Crier-Agent carried 'j2' (the sender, from webhook.go:274) a

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"Go HTTP/WebSocket server binary bin/crier (cmd/server, default port :8767) exposing REST + WS endpoints for relay/mesh/registry/inbox; secondary entry point bin/crier-mcp (cmd/crier-mcp), an MCP server exposing registry+inbox tools; not a library or cron job","promise":"Promise: this

- [P1] Docs promise wildcard relay routing; server rejects wildcards with reason-free 400 — architecture.md:12 'topic-based routing with wildcards' and openapi.yaml:50 'supports wildcard subscribers', but internal/relay/relay.go:140-167 validates topics as [letter/digit/_/-] only. Live probe
- [P1] ttl_seconds documented in OpenAPI but parsed nowhere; expiry hard-coded to 24h — openapi.yaml:447 documents ttl_seconds (0=never); grep across internal/server and internal/registry finds no parse of the field; memory_store.go:106-107 sets ExpiresAt=CreatedAt+24h unconditionally. R
- [P1] First-publish onboarding wall: README lacks publish shape, X-Agent-ID requirement, and WS auth recipe — Live-verified: POST /relay/publish with guessed {topic,payload} -> 400 bare 'event is required'; correct shape without X-Agent-ID -> 401 'X-Agent-ID header required for rate-limited publish' even with
- [P2] MCP surface doc drift: guide says 8 tools (live: 13), shared-backend claim false in default in-memory mode — Live tools/list over stdio returned 13 tools (register_agent..mesh_request) vs integration guide's 8; MCP-local agent registered in default mode is invisible over HTTP (own process-local store) despit
- [P2] Silent failure modes read as broken: no WS welcome frame, malformed mesh frames dropped despite INVALID_MESSAGE spec — Live: successful relay subscribe -> 101 then zero frames (silence on connect); reporter confirmed wrong-shape mesh frames vanish silently though docs/mesh-protocol.md defines an INVALID_MESSAGE error 

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"Primary: bin/crier HTTP/WebSocket server from cmd/server on port 8767. Secondary: bin/crier-mcp stdio MCP server/HTTP bridge from cmd/crier-mcp.","promise":"Promise: this project claims a user can connect autonomous AI agents to discover peers and exchange guarded messages through pu

- [P1] Documented default workflows fail with 401 responses — TESTERS.md starts with signature enforcement enabled but later uses unsigned inbox retrieval, and the integration guide omits X-Agent-ID from relay publishing under the default rate limit; both docume
- [P1] Keyless make run degrades guarded delivery — The exact make run path enables the guard without an API key; benign messages are delivered with a medium-risk guard_error until the user discovers and sets CR_GUARD_ENABLED=false, weakening confidenc
- [P2] MCP onboarding and inventory documentation are stale — Live initialize and tools/list succeeded and exposed 13 tools, but the integration guide advertises 8 and provides no copy-paste initialize, notifications/initialized, tools/list, and tools/call trans
- [P2] Core multi-transport workflow completes with real value — Within 25 seconds, the server served health on port 8767; the signed demo completed register, deliver, retrieve, acknowledge, and empty-inbox verification; WebSocket relay received a published event a

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"Primary: bin/crier HTTP/WebSocket server from cmd/server on port 8767. Secondary: bin/crier-mcp stdio MCP server/HTTP bridge from cmd/crier-mcp.","promise":"Promise: this project claims a user can connect autonomous AI agents to discover peers and exchange guarded messages through pu

- [P1] Documented default workflows fail with 401 responses — TESTERS.md starts with signature enforcement enabled but later uses unsigned inbox retrieval, and the integration guide omits X-Agent-ID from relay publishing under the default rate limit; both docume
- [P1] Keyless make run degrades guarded delivery — The exact make run path enables the guard without an API key; benign messages are delivered with a medium-risk guard_error until the user discovers and sets CR_GUARD_ENABLED=false, weakening confidenc
- [P2] MCP onboarding and inventory documentation are stale — Live initialize and tools/list succeeded and exposed 13 tools, but the integration guide advertises 8 and provides no copy-paste initialize, notifications/initialized, tools/list, and tools/call trans
- [P2] Core multi-transport workflow completes with real value — Within 25 seconds, the server served health on port 8767; the signed demo completed register, deliver, retrieve, acknowledge, and empty-inbox verification; WebSocket relay received a published event a"}


## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"Primary: bin/crier HTTP/WebSocket server from cmd/server on port 8767. Secondary: bin/crier-mcp stdio MCP server/HTTP bridge from cmd/crier-mcp.","promise":"Promise: this project claims a user can connect autonomous AI agents to discover peers and exchange guarded messages through pu

- [P1] Documented default workflows fail with 401 responses — TESTERS.md starts with signature enforcement enabled but later uses unsigned inbox retrieval, and the integration guide omits X-Agent-ID from relay publishing under the default rate limit; both docume
- [P1] Keyless make run degrades guarded delivery — The exact make run path enables the guard without an API key; benign messages are delivered with a medium-risk guard_error until the user discovers and sets CR_GUARD_ENABLED=false, weakening confidenc
- [P2] MCP onboarding and inventory documentation are stale — Live initialize and tools/list succeeded and exposed 13 tools, but the integration guide advertises 8 and provides no copy-paste initialize, notifications/initialized, tools/list, and tools/call trans
- [P2] Core multi-transport workflow completes with real value — Within 25 seconds, the server served health on port 8767; the signed demo completed register, deliver, retrieve, acknowledge, and empty-inbox verification; WebSocket relay received a published event a"}

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"cmd/server (bin/crier HTTP/WebSocket server); optional cmd/crier-mcp (bin/crier-mcp MCP bridge)","promise":"Promise: this project claims a user can exchange messages between autonomous agents by using Crier’s HTTP/WebSocket relay, peer mesh, agent registry, durable inboxes, and optio

- [P1] Documented HTTP workflows fail under their stated defaults — TESTERS.md starts with CR_REQUIRE_AGENT_SIG=true but its unsigned inbox GET returned 401; the integration guide's relay publish also returned 401 because it omitted the required X-Agent-ID.
- [P1] MCP bridge works but lacks a usable client path — The bridge built, initialized, exposed tools, and retrieved an HTTP-delivered message, but exercising initialize, tools/list, and tools/call required a custom JSON-RPC harness and live schema discover
- [P2] Documented MCP inventory is stale — docs/integration-guide.md and skills/crier-usage/SKILL.md advertise 8 MCP tools, while live tools/list returned 13.
- [P2] Demo reports guard configuration inaccurately — examples/demo.sh warned that the guard was enabled and failing open even though the server startup log confirmed CR_GUARD_ENABLED=false.
- [P2] Core relay workflow delivers real value — Both binaries built; /health returned 200; the signed inbox lifecycle completed with 201/201/200/204; WebSocket relay publish returned 202 and delivered the expected frame; first success took about 31
