## Dogfood Findings (2026-09-09)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server), default port :8767 — env-driven config (CRIER_PORT, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG, CR_GUARD_ENABLED) with CLI flag overrides (-port, -db-url, -version); a secondary MCP server binary bin/crier-mcp (cmd/crier-

- [P2] Default port :8767 may already host a fleet instance — no scratch-port guidance — Live: :8767 occupied by pre-existing crier (pid 3466723); 'make run' would bind-fail. CRIER_PORT=18767 worked and my own PID owned the port; README documents CRIER_PORT (line 237) but never warns that
- [P2] 'Try the Mesh' assumes websocat/wscat, neither installed — Live: neither websocat nor wscat on PATH; README:160-165 examples require one. A stdlib-only raw-socket WS probe (handshake + frame parse) worked first try against /relay/subscribe/{topic} and /mesh/c
- [P2] crier-mcp framing undocumented; standard length-prefixed MCP stdio fails — Live: newline-delimited JSON-RPC works even without initialize (tools/list returned 13 tools); but the standard MCP stdio framing (4-byte big-endian length prefix) returns {"error":{"code":-32700,"mes
- [P2] Guard verdict location ambiguous — body crier.guard, not deliver-response headers — Live on guard-ON keyless instance: POST /agents/g1/inbox → 201 with verdict only in JSON body {"guard":{"decision":"allow","errored":true,"reason":"guard_error: all providers failed: no provider api k

## Dogfood Findings (2026-09-10)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server) listening on :8767, with a companion MCP server bin/crier-mcp; no client SDK required — plain curl/openssl/websocat against the REST API (16 endpoints: health, relay, mesh, federation, registry, inbox)","promise":"Crier claims to be

- [P1] MCP stdio only speaks newline-delimited JSON-RPC; standard length-prefixed framing hangs silently — Live on HEAD 3956cfb: initialize + tools/list via 4-byte BE length-prefixed framing (the MCP spec stdio transport, what real MCP clients use) → no response in 5s, process stays alive; same requests ne
- [P2] Relay publish docs gap: X-Agent-ID header and {topic,event} body shape only in openapi.yaml — Live: POST /relay/publish without X-Agent-ID → 401 'X-Agent-ID header required for rate-limited publish'; with {topic,payload} → 400 'event is required'; correct {topic,event}+header → 202 with WS fra
- [P2] Mesh REQUEST/RESPONSE wire format not in README — PeerRef source/target, status_code, request_id echo — Live two-agent round-trip works with the documented format (docs/mesh-protocol.md): REQUEST with source/target {"agent_id":...} forwarded verbatim, RESPONSE with request_id echoing the REQUEST's messa
- [P2] Mesh clients must filter KEEPALIVE frames by type — undocumented in README — Server sends KEEPALIVE to every connected peer every 30s (internal/mesh/peer.go keepaliveLoop, KeepaliveInterval 30s); a client reading the socket without type-filtering sees them interleaved with REQ
- [P2] GET /version returns 404 page not found — Live: curl localhost:8767/version → 404 page not found; endpoint is not documented anywhere (README/AGENTS.md document -version CLI flag only). Minor guess-friction, self-evident.

## Dogfood Findings (2026-09-10)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"HTTP/WebSocket server binary (bin/crier, from cmd/server) listening on :8767, with a companion MCP server (bin/crier-mcp, from cmd/crier-mcp) exposing registry + inbox tools; config is env-driven (CRIER_PORT, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG) with CLI flag overrides"}

- [P0] MCP stdio server only responds at stdin EOF — hangs real MCP clients — Verified: MCP tools/list only answers when stdin is closed. A genuine MCP client (claude, hermes bridge) keeps stdin open (PIPE pattern) and would hang indefinitely. This breaks the promised 'MCP server exposing registry + inbox tools' component for real use, even though the EOF-pipe test passes.
- [P1] Relay publish requires X-Agent-ID header that the README never mentions — Publish without X-Agent-ID returns 401, and the README relay section has no curl example and no mention of the header. A real user following the documented workflow hits an unexplained 401 on the first relay publish.
- [P1] Mesh REQUEST/RESPONSE wire format only in docs/mesh-protocol.md — README mesh example stops at REGISTER/peers; the actual request/response contract (PeerRef source/target objects, status_code, request_id echoing message_id) lives only in a separate doc. Users cannot complete a mesh exchange from the README alone.
- [P1] KEEPALIVE frames interleave on the mesh socket mid-conversation, undocumented — Clients must loop-recv and filter frames by type or KEEPALIVE frames corrupt the request/response flow. This behavior is not documented in the README, so a naive client implementation stalls or misparses.
- [P2] Port-conflict guidance missing and /version is not an HTTP route — Default :8767 was occupied by fleet instances; README never documents running a second instance on another port (override + bind-failure guidance). GET /version returns 404 — only the -version CLI flag works, and the route is undocumented.

## Dogfood Findings (2026-09-11)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server, default port :8767), plus a secondary MCP server binary bin/crier-mcp (cmd/crier-mcp) exposing registry+inbox tools; config is env-driven (CRIER_PORT, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG) with CLI flag overrides (-p

- [P2] Integration guide MCP tool count is stale — docs/integration-guide.md claims 8 tools; live tools/list on bin/crier-mcp returns 13 (ack_messages, ask_agent, deliver_message, get_agent, get_messages, inbox_stats, list_agents, mesh_peers, mesh_req
- [P2] Health endpoint not discoverable — Verified live: /health returns 200, /healthz returns 404; /health is only mentioned mid-paragraph in the README auth section, no endpoint table.
- [P2] Relay pub/sub not runnable from README alone — Subscribe URL /relay/subscribe/{topic} and the {topic, event} publish envelope only exist in docs/openapi.yaml; a guessed payload body gets 400 'event is required'. Verified live that the documented p
- [P2] README 401-on-stale-timestamp claim has a 404 edge case — Unregistered agents get 404 (existence checked before signature/timestamp); negative-path testers will trip on the mismatch. Documented happy-path auth held: unsigned retrieve 401, duplicate register 
- [P2] No version stamping — make build succeeds but ./bin/crier -version prints 'crier dev' — useless for bug reports.
