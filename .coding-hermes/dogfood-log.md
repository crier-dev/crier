# Dogfood Log

| Date | Verdict | Promise | Top findings | Time-to-first-success |
|------|---------|---------|--------------|----------------------|
| 2026-08-09 | 🟡 PROMISING-BUT-ROUGH | Agent-to-agent message bus: relay pub/sub over WS, agent registry, durable lease-based inboxes, P2P mesh, MCP server, optional Postgres durability | 1) Ack without message_ids silently no-ops (204) — README/demo ack is a false confirmation, message redelivered later (CR-GAP-014, P1). 2) Mesh works but protocol is undocumented with overstated handshake claims (CR-GAP-016, P2). 3) MCP server + Postgres durability + relay + registry all verified working end-to-end. | ~2 min (build 0.56s + demo.sh 0.118s) |

## 2026-08-09 — Crier (cron dogfood run)

**Promise statement:** "A developer/agent can, by starting one Go server and hitting 14 HTTP endpoints (or via an MCP server), build an agent-to-agent messaging layer: register agents with ed25519 identities, deliver durable lease-based inbox messages, pub/sub over WebSocket relay, and P2P mesh connections — with Bearer auth and per-agent request signing, and optional PostgreSQL durability."

**What was actually done (real use, not tests):**
- Built (0.56s), started with auth + signing enabled (documented default posture), ran `examples/demo.sh` (0.118s round-trip).
- Verified CR-GAP-005 fix live: `--help`/`--version` work.
- Full HTTP workflow: register → deliver → signed retrieve → ack → verify; registry CRUD; stats; error paths (wrong method in signed payload → 401; stale ts → 401 ±30s; wrong key → 401; wrong auth token → 401; unknown agent → 404).
- Lease semantics: concurrent retrieves get disjoint sets; un-acked leased messages are redelivered after lease expiry + 30s purge tick.
- Relay: WS subscribe → publish (202) → event received; topics listing.
- Mesh: two-agent REQUEST/RESPONSE round-trip over WS (works once wire format is right).
- MCP: real MCP client (stdio) → initialize/list_tools/call 8 tools; register/deliver/retrieve/ack round-trip; cross-interface consistency (agent registered via HTTP visible via MCP, shared Postgres).
- Durability: scratch Postgres container; agent + undelivered message survived full server restart.

**Friction count (user-facing): 8**
1. Documented ack is a no-op (lease_id only) — silent false success (CR-GAP-014)
2. DELETE /agents/{id} sig requirement undocumented (CR-GAP-015)
3. Mesh wire format undocumented; REGISTER_ACK never sent; KeepaliveAck in docs but not code; request_id==message_id contract implicit; malformed frames silently dropped (CR-GAP-016)
4. crier-mcp ignores --help/--version (CR-GAP-017)
5. No AGENTS.md (CR-GAP-018)
6. Python client friction: timestamp must be RFC3339 string, PeerRef object shapes — no docs, had to read source (folded into CR-GAP-016)
7. README quickstart `AUTH=(-H "Authorization: Bearer ${CR_AUTH_TOKEN:-}")` with unset token sends an empty Bearer header — worked, but sloppy
8. `server.log` / `/tmp` leftovers in repo root (gitignored? `server` binary committed? — repo hygiene note)

**Verdict: 🟡 PROMISING-BUT-ROUGH** — value is real (relay/registry/inbox/MCP/durability all work), but the shipped demo teaches a false ack contract and mesh is rough around the edges.

**Tasks added:** CR-GAP-014 (P1), CR-GAP-015 (P2), CR-GAP-016 (P2), CR-GAP-017 (P2), CR-GAP-018 (P3).

**Foreman:** not paused (cooldown 7200s < 14400s) — no wake needed; board has fresh work.
2026-09-01 | UNKNOWN-VALUE | n/a t2fs | friction 0 | 3 findings

2026-09-07 | SHIPPABLE | 21s t2fs | friction 6 | 5 findings
