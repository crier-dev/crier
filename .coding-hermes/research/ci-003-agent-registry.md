# CI-003: Agent Registry + Persistent Inboxes — Research

## Context

Crier is an agent-to-agent message bus. Two primitives are built:
1. **CI-001** — Relay: pub/sub with WebSocket topic routing (`internal/relay/`, 249 lines, 13 tests)
2. **CI-002** — Mesh: peer-to-peer agent connections (`internal/mesh/`, 614 lines, 8/8 GitReins PASS)

CI-003 adds the third primitive: **Agent registry + persistent inboxes**.

## Spec (from docs/specs.md § CI-003)

### Design
- Registry is an in-memory store mapping agentID → Agent record
- Agent: `{ID, PublicKey, Capabilities, Status, RegisteredAt, LastSeen}`
- Inbox is a per-agent FIFO queue with lease-based delivery
- Messages expire after TTL (default 24h)
- Lease prevents double-delivery: retrieve leases messages for N seconds, ACK confirms, un-ACKed messages return to queue after lease expires

### Endpoints
| Method | Path | Purpose |
|--------|------|---------|
| POST | /agents | Register agent |
| GET | /agents | List all agents |
| GET | /agents/{id} | Agent detail + health |
| DELETE | /agents/{id} | Unregister agent |
| POST | /agents/{id}/inbox | Deliver message to agent |
| GET | /agents/{id}/inbox | Retrieve leased messages |
| POST | /agents/{id}/inbox/ack | Acknowledge delivery |
| GET | /agents/{id}/inbox/stats | Queue depth, leased count, age |

### Acceptance criteria
1. Register agent with ed25519 public key → 201
2. Duplicate agent ID → 409
3. Retrieve leased messages → only un-ACKed messages returned
4. ACK → messages permanently removed
5. Un-ACKed messages return to queue after lease TTL expires
6. TTL-expired messages auto-purged
7. Agent unregister cleans up inbox
8. Concurrent delivery: two retrievers get disjoint message sets

## Existing patterns to follow

- **Go idioms:** sync.RWMutex for thread safety, constructor-based initialization, interface-based testing
- **Error handling:** fmt.Errorf with %w wrapping
- **Package structure:** One package per component (`internal/relay/`, `internal/mesh/`)
- **Handler wiring:** gorilla/mux in `cmd/server/main.go`
- **Tests:** table-driven, `go test -short -count=1 ./...`
- **No external dependencies:** gorilla/mux + gorilla/websocket only (already in go.mod)

## Hilo impact

No existing dependents on `internal/` — the registry is net-new. `cmd/server/main.go` already uses gorilla/mux, so adding registry routes is additive.

## DuckBrain memory

No prior findings for Crier. This is the first CI-003 attempt.

## GitReins config

`.gitreins/config.yaml` is set up for Go: go_build, go_lint, go_tests, Tier 2 with 100 iter / 10m / 1M input / 384k output.
