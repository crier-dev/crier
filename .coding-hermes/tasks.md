
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

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"Primary: cmd/server builds bin/crier, a Go HTTP/WebSocket server listening on :8767 by default. Secondary: cmd/crier-mcp builds bin/crier-mcp, a stdio MCP server and remote bridge.","promise":"Promise: this project claims a developer or operator can connect heterogeneous autonomous a

- [P1] TTL contract is silently ignored — A message created with ttl_seconds=1 was accepted but received an effective 86400-second expiry, so the OpenAPI contract misrepresents retention behavior without returning an error.
- [P1] Default MCP and HTTP processes do not share state as documented — HTTP agents were absent from the local in-memory MCP agent list; state became shared only in remote bridge mode. The integration guide instead claims the default MCP server shares the HTTP backend.
- [P1] Official operator recipes fail under their documented defaults — The relay publish example returned 401 until X-Agent-ID was added, and TESTERS.md starts with CR_REQUIRE_AGENT_SIG=true while its unsigned inbox GET and webhook PATCH omit required signatures; the uns
- [P2] MCP and guard documentation is stale or misleading — Live tools/list returned 13 tools rather than the documented 8, and examples/demo.sh warned that the guard was ON because the client lacked DEEPSEEK_API_KEY even though the running server log proved C
- [P2] Startup lacks collision-safe guidance — The documented :8767 startup failed because another Crier listener occupied the port; using verified port 18877 worked, but discovery and alternate-port verification required manual operator investiga

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"cmd/server (built as ./bin/crier; default port :8767)","promise":"Promise: this project claims a user can connect autonomous agents through pub/sub, peer-to-peer messaging, discoverable identities, and durable inboxes by using the Crier HTTP/WebSocket message bus.","readme_present":t

- [P1] Core messaging works, but the documented tester workflow fails under default authentication — The runnable demo completed registration, delivery, signed retrieval, acknowledgement, and empty-inbox verification; exact-topic WebSocket pub/sub also delivered an event. However, TESTERS.md creates 
- [P1] Inbox validation contradicts the published API contract — POST /agents/scout-bob/inbox with an empty JSON object returned 201 and queued a payload-less message, although OpenAPI marks payload as required and promises HTTP 400 for invalid bodies.
- [P1] Advertised wildcard subscriptions are unsupported — The OpenAPI topic description promises wildcard subscribers, but subscribing to dogfood.* returned HTTP 400 without an explanatory reason; only exact-topic pub/sub was verified working.
- [P1] Demo reports guard state from the wrong configuration source — examples/demo.sh warned that the guard was enabled and failing open because DEEPSEEK_API_KEY was unset, while the tested server log showed the effective configuration was CR_GUARD_ENABLED=false, makin
- [P2] Documented startup sequence performs a redundant rebuild — Running make build followed by make run compiled the server twice; despite this, health and docs returned 200 and first success was reached in 23.7 seconds.

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"cmd/server/main.go (builds bin/crier; optional MCP bridge: cmd/crier-mcp/main.go)","promise":"Promise: this project claims a user can connect autonomous agents through pub/sub, peer-to-peer WebSockets, discoverable identities, and durable inbox messaging by running the Crier HTTP/Web

- [P1] Documented tester workflow fails under default authentication settings — TESTERS.md registers agents without retaining private keys, but CR_REQUIRE_AGENT_SIG=true by default; its unsigned inbox GET returned 401, so a real user cannot complete the documented manual workflow
- [P1] Advertised interactive API documentation is not interactive — The live /docs endpoint returned a static index linking JSON and YAML specifications, with no controls for issuing the requests that TESTERS.md says users can fire from the browser.
- [P1] Demo reports an incorrect effective guard state — examples/demo.sh warned that the guard was enabled and failing open because DEEPSEEK_API_KEY was unset, while the running server logged CR_GUARD_ENABLED=false; this makes security-relevant output untrustworthy.
- [P2] Documented command sequence performs redundant builds — make build, direct go build, and make run all succeeded, but make run depends on make build, causing the server to be compiled three times when the requested commands are followed sequentially.
- [P2] Core message-bus workflow works when using the runnable demo — The isolated server reached 200 /health in 20.9 seconds, and the demo completed registration, delivery, signed retrieval, signed acknowledgement, and empty-inbox verification with HTTP 201, 201, 200, 204, and 200.

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"cmd/server/main.go (built as bin/crier); MCP bridge: cmd/crier-mcp/main.go (built as bin/crier-mcp)","promise":"Promise: this project claims a user can register and discover autonomous agents and exchange guarded messages through pub/sub, peer-to-peer mesh, webhooks, and durable inbo

- [P1] Documented secure workflow cannot complete — TESTERS.md discards Bob's generated Ed25519 private key, then attempts protected inbox retrieval and webhook PATCH without signatures; both reproducibly return 401. The working signed demo proves the 
- [P1] MCP launch command contaminates the stdio protocol — The documented compound command `make build-mcp && ./bin/crier-mcp` emits Go build output before JSON-RPC. A strict MCP client therefore does not receive a clean stdio stream, although separately laun
- [P1] MCP storage mode and state visibility are unclear — Bare crier-mcp silently uses an undocumented process-local store; startup output does not distinguish local storage from HTTP-bridge mode. Users cannot reliably know whether MCP and server clients sha
- [P1] Published authentication contract contradicts runtime defaults — OpenAPI states that every non-health endpoint requires bearer authentication, but the shipped default has auth disabled: unauthenticated registration and delivery succeeded. Per-agent signature enforc
- [P2] Documentation and response metadata contain misleading details — /docs is a static specification index rather than the advertised interactive console; /health returns JSON text as `text/plain` despite OpenAPI declaring `application/json`; the demo incorrectly warns

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"key_features":["Agent-to-agent message bus","Relay pub/sub over HTTP and WebSocket","Peer-to-peer WebSocket mesh","Discoverable agent registry","Durable lease-based inboxes","PostgreSQL persistence","LLM prompt-injection guard","Relay-to-relay federation","Remote MCP bridge"],"quick_start":["Requi

- [P1] Default manual walkthrough fails authentication — TESTERS.md omits required per-agent signatures for inbox reads and webhook PATCH requests; both returned HTTP 401. Its random public-key-shaped registrations retain no matching private keys, so users 
- [P1] Concurrent-consumer guidance misstates live behavior — A live race over ten queued messages produced a 0/10 split with no duplication, contradicting TESTERS.md's claim that both retrievers receive every message and concealing a starvation risk.
- [P1] Demo reports guard configuration unreliably — examples/demo.sh warned that the guard was enabled and failing open while the server log proved CR_GUARD_ENABLED=false; the script infers server state from its own environment rather than the running 
- [P2] Documentation and HTTP contract contain observable mismatches — /docs is a static OpenAPI JSON/YAML index rather than the promised interactive console, and GET /health returned JSON as text/plain despite OpenAPI declaring application/json.
- [P2] Quick start performs a redundant rebuild — Following make build with make run rebuilds the same server binary, adding avoidable friction despite the first healthy response arriving in 0.898 seconds with a warm Go cache.

## Dogfood Findings (2026-09-13)
Verdict: UNKNOWN-VALUE
Promise: {"readme_present":true,"run_commands":["make build","CR_GUARD_ENABLED=false ./bin/crier -port 8767"]}

- [P0] No successful real-use workflow was demonstrated — Real-use evidence reports works=false, time_to_first_success_s=null, and no completed user outcome.
- [P1] Promised commands were not verified — The evidence does not show whether `make build` or `CR_GUARD_ENABLED=false ./bin/crier -port 8767` completed successfully, so the promised workflow cannot be confirmed.
- [P1] Usability evidence is incomplete — friction_count=0 conflicts with the absence of a first success; zero recorded friction does not establish low-friction usability when the run never produced a usable result.
- [P1] Captured output is not actionable — The only note is `(unparsed) What would you like me to do?`, which neither demonstrates Crier functionality nor identifies what a real user can accomplish.
- [P2] Trustworthiness cannot be established — promises_held and promises_broken are both empty despite works=false, leaving no command output, behavior trace, or explicit promise assessment to audit.

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"project_summary":"Crier is a Go-based agent-to-agent message bus providing HTTP/WebSocket pub/sub relay, peer-to-peer mesh networking, a discoverable agent registry, durable lease-based inboxes, relay federation, an MCP bridge, and an optional LLM message guard.","readme_present":true,"runme_comma

- [P1] Default tester walkthrough fails authentication — TESTERS.md omits mandatory agent-signature headers: its inbox retrieval and webhook PATCH commands return HTTP 401, preventing the documented default workflow from completing as written.
- [P1] Advertised matrix harness is not locally runnable — scripts/bunker-matrix.sh requires undisclosed remote deployment arguments and failed at scp with "Connection closed" before exercising all advertised message-moving modes.
- [P2] Interactive API documentation claim is inaccurate — /docs only links to OpenAPI JSON and YAML and provides no browser interface for executing requests, despite being described as interactive.
- [P2] Keyless quickstart produces noisy guard metadata — Without DEEPSEEK_API_KEY, delivery succeeds fail-open as documented, but an ordinary first message is marked medium-risk with guard_error metadata.
- [P2] Core messaging paths provide verified real value — A healthy server was reached in 38 seconds; signed inbox delivery and acknowledgement, WebSocket relay fan-out, two-bus federation, all Go tests, go vet, and the 70% coverage gate succeeded, with meas
\n## Dogfood Findings (2026-09-13)\nVerdict: PROMISING-BUT-ROUGH\nPromise: {"entry_point":"Primary: bin/crier HTTP/WebSocket server on :8767; secondary: bin/crier-mcp stdio MCP bridge to a running Crier server.","promise":"Crier claims autonomous agents can discover one another and exchange messages through live relay pub/sub, direct WebSocket mesh requests, durable lease-\n\n- [P1] The full promised workflow was not completed — Relay pub/sub, signed inbox delivery, MCP messaging, and Compose sink delivery worked, but direct WebSocket mesh requests, webhook delivery, federation, and PostgreSQL-backed durability were not succe\n- [P1] Default user walkthroughs fail authentication — TESTERS.md discards the private key needed for signed agent operations and then issues unsigned inbox and webhook requests; both returned HTTP 401, blocking the documented workflows without users inde\n- [P1] Published API contracts contradict live behavior — A dogfood.* wildcard subscription returned HTTP 400 despite wildcard support being advertised; /health returned text/plain despite OpenAPI declaring application/json, and global bearer security contra\n- [P1] Compose quickstart unexpectedly reruns and resets the stack — docker compose up -d --build already ran the 10-pass/0-fail/1-skip battery, while the documented follow-up command rebuilt and recreated every dependency, changed the Crier container ID, and erased in\n- [P1] MCP is functional but exposes untrustworthy metadata and stale guidance — A raw remote MCP session exposed 13 tools and successfully created, queried, delivered, consumed, and auto-ACKed data, but register_agent returned blank status and year-1 timestamps while immediate re

## Dogfood Findings (2026-09-13)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"Primary: cmd/server, built as ./bin/crier and serving HTTP/WebSocket APIs on :8767 by default; secondary: cmd/crier-mcp, built as ./bin/crier-mcp and exposed over stdio or as a bridge to a running Crier server.","promise":"Crier is a Go-based agent-to-agent message bus that provides 

- [P1] Documented Compose workflow duplicates tests and destroys live state — compose up automatically ran the battery, then the documented compose run command ran it again, recreated the Crier container, changed its container ID, and erased the in-memory registry.
- [P1] Documented MCP launch is unsafe for strict stdio clients — make build-mcp && ./bin/crier-mcp printed the Go build command to stdout before JSON-RPC traffic; a manually constructed client was required to complete initialize, tools/list, register_agent, and read-back across 13 tools.
- [P1] Ecosystem request contract is not usable from the guide alone — An intuitive payload.task request returned HTTP 200 with ECHO: no text; the required payload.text, sender, session_id, delivery_mode, and timeout_ms fields had to be recovered from battery.sh before a successful blocking sink round-trip.
- [P2] Public API and identity claims drift from observed behavior — The claimed interactive /docs page was only a static OpenAPI link index, /health returned JSON as text/plain, and the MCP binary reported dev via -version but 0.1.0 during initialize.
- [P2] Core workflow succeeds quickly and demonstrates real value — The server built and reached HTTP 200 health in 1.001 seconds; signed register/deliver/retrieve/ACK completed, unsigned retrieval returned 401, MCP state changes read back immediately, and both Compose batteries reported 10 pass, 0 fail, 1 skip.
