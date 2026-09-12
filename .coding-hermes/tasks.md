
## Dogfood Findings (2026-09-12)
Verdict: PROMISING-BUT-ROUGH
Promise: {"entry_point":"Go HTTP/WebSocket relay server (bin/crier, built from ./cmd/server, listens on :8767) as the primary entry point; secondary entry point is the MCP server bin/crier-mcp from ./cmd/crier-mcp. No library or cron surface — config is env-driven with -port/-db-url/-version CLI flag overlro

- [P1] crier-mcp remote mode cannot use the inbox tools under the DEFAULT signing config — Live against my own :18961 (CR_REQUIRE_AGENT_SIG default true), remote mode CRIER_HTTP_URL+CRIER_AGENT_ID: retrieve_inbox, inbox_stats and ack_messages all return isError=True, 401 'missing agent sign
- [P1] The auth variable the binary's own --help documents does not work in remote mode — ./bin/crier-mcp --help prints 'CR_AUTH_TOKEN  bearer token required on all requests', but cmd/crier-mcp/main.go:79 passes os.Getenv("CRIER_AUTH_TOKEN") to NewRemoteStore. Verified on auth-on :18962 (C
- [P1] MCP list_agents reports an auth failure as a successful empty registry — Same session, auth-on :18962 with the documented CR_AUTH_TOKEN (or a wrong CRIER_AUTH_TOKEN): get_agent -> isError=True with the 401 text ('missing Authorization header' / 'invalid token'), while list
- [P2] Wildcard subscribers are advertised in two shipped docs but rejected at subscribe time — docs/architecture.md:12 'Topic-based routing with wildcards' and docs/openapi.yaml:50 'Topic name (dot-separated, supports wildcard subscribers)' vs live: websocket GET /relay/subscribe/use31.* and us
- [P2] MCP tool inventory, relay frame/202 contract and the lease re-read are all undocumented and drifting — tools/list returns 13 tools while docs/integration-guide.md:316 still claims '8 tools'. Relay publish returns 202 with an empty body (Content-Length 0 — no id, no accepted-vs-dropped signal) and subsc
