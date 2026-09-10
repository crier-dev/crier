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
