# ws-mesh-demo — WebSocket subscribe + mesh demo (CR-GAP-050)

A runnable, **zero-external-install** demo of the crier relay's two live
WebSocket surfaces, proven end-to-end against a real server:

1. **Relay pub/sub fan-out** — `POST /relay/publish` → subscriber receives the
   event over `ws://…/relay/subscribe/<topic>`.
2. **Mesh peer registration** — two peers connect via
   `ws://…/mesh/connect/<agentID>` and appear in `GET /mesh/peers` with
   `"count":2`.

## Requirements

- Go toolchain only (the repo's `go.mod` requires go 1.26.6; `GOTOOLCHAIN=auto`
  resolves it from the module cache).
- `curl` (for the publish + health/peers probes).
- `github.com/gorilla/websocket` v1.5.3 — **already in `go.mod`/`go.sum`**.
  No `go get`, no pip/npm/apt, no new dependencies. Nothing outside `examples/`
  is touched.

## Quick start

```bash
cd examples/ws-mesh-demo
bash run-demo.sh            # or: DEMO_PORT=19999 bash run-demo.sh
```

The script builds the server and demo client into a `mktemp` dir (never the
repo), starts the relay auth-disabled on port 18767 (override with `DEMO_PORT`),
spawns a subscriber and two mesh peers, publishes an event, and fails loudly
unless **both** checks pass:

- subscriber receives `hello from run-demo` over WS; and
- `GET /mesh/peers` returns `count:2` with `demo-agent-a` + `demo-agent-b`.

Live output is teed into `TRANSCRIPT-<date>.md` in this directory. The script
is idempotent — run it as many times as you like (cleanup kills all processes).

## Client usage

```
ws-mesh-demo [-url BASE] subscribe -topic TOPIC [-once]
ws-mesh-demo [-url BASE] peer -agent AGENT_ID
```

| Subcommand | Flag    | Meaning                                                        |
|------------|---------|----------------------------------------------------------------|
| subscribe  | `-topic`| Topic to subscribe to (required).                               |
| subscribe  | `-once` | Exit 0 after the first received event (used by run-demo.sh).    |
| peer       | `-agent`| Agent ID to register as on the mesh (required).                 |
| (both)     | `-url`  | Server base URL, default `http://127.0.0.1:18767`.              |

`subscribe` prints `SUBSCRIBED <topic>` after the WS upgrade and one
`EVENT <payload>` line per received event (payload is the raw event JSON the
relay fans out). `peer` prints `PEER CONNECTED <agentID>` and then holds the
socket open (discard-read loop keeps gorilla's default ping handler
auto-ponging, so the relay's keepalive never drops the peer — a closed WS
removes the peer from `/mesh/peers`, see `internal/mesh/peer.go` `OnClose`).

## Endpoints exercised

| Endpoint | Method | Purpose | Source |
|----------|--------|---------|--------|
| `/relay/publish` | POST | Publish `{"topic","event"}`; 202 on success | `cmd/server/main.go:105` |
| `/relay/subscribe/{topic}` | WS | Fan out raw event JSON as text frames | `cmd/server/main.go:106` |
| `/mesh/connect/{agentID}` | WS | Register a peer on upgrade | `cmd/server/main.go:112` |
| `/mesh/peers` | GET | `{"peers":[{"agent_id":...}],"count":N}` | `cmd/server/main.go:113` |

Wire details: `docs/mesh-protocol.md`, OpenAPI: `cmd/server/openapi.yaml`.

## Notes

- The demo runs the relay **auth-disabled** (`CR_AUTH_TOKEN` empty → pass-through
  per `internal/middleware/auth.go`); `CR_REQUIRE_AGENT_SIG` is forced false
  (signing gates the registry, not relay/mesh). The script unsets any ambient
  `CR_AUTH_TOKEN` first.
- Publishing requires `X-Agent-ID` when rate limiting is on (default 100/min)
  — the demo sends `X-Agent-ID: demo-publisher`.
- Build artifacts are written to a `mktemp` dir; the repo stays clean apart
  from this directory's files.
