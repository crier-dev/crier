## Dogfood Findings (2026-09-09)
Verdict: SHIPPABLE
Promise: {"entry_point":"HTTP/WebSocket server binary bin/crier (cmd/server), default port :8767 — env-driven config (CRIER_PORT, CR_DATABASE_URL, CR_AUTH_TOKEN, CR_REQUIRE_AGENT_SIG, CR_GUARD_ENABLED) with CLI flag overrides (-port, -db-url, -version); a secondary MCP server binary bin/crier-mcp (cmd/crier-

- [P2] Default port :8767 may already host a fleet instance — no scratch-port guidance — Live: :8767 occupied by pre-existing crier (pid 3466723); 'make run' would bind-fail. CRIER_PORT=18767 worked and my own PID owned the port; README documents CRIER_PORT (line 237) but never warns that
- [P2] 'Try the Mesh' assumes websocat/wscat, neither installed — Live: neither websocat nor wscat on PATH; README:160-165 examples require one. A stdlib-only raw-socket WS probe (handshake + frame parse) worked first try against /relay/subscribe/{topic} and /mesh/c
- [P2] crier-mcp framing undocumented; standard length-prefixed MCP stdio fails — Live: newline-delimited JSON-RPC works even without initialize (tools/list returned 13 tools); but the standard MCP stdio framing (4-byte big-endian length prefix) returns {"error":{"code":-32700,"mes
- [P2] Guard verdict location ambiguous — body crier.guard, not deliver-response headers — Live on guard-ON keyless instance: POST /agents/g1/inbox → 201 with verdict only in JSON body {"guard":{"decision":"allow","errored":true,"reason":"guard_error: all providers failed: no provider api k
