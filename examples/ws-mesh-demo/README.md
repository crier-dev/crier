# ws-mesh-demo — WebSocket subscribe + mesh demo (CR-GAP-050)

A runnable, **zero-external-install** demo of the crier relay's two live
WebSocket surfaces, proven end-to-end against one real server it starts itself:

1. **Agent registration** — both demo peers are HTTP-registered first
   (`POST /agents` → `201`), so they exist in the agent registry as well as on
   the mesh.
2. **Relay pub/sub fan-out** — `POST /relay/publish` → subscriber receives the
   event over `ws://…/relay/subscribe/<topic>`.
3. **Mesh peer registration** — the two registered peers connect via
   `ws://…/mesh/connect/<agentID>` and appear in `GET /mesh/peers` with
   `"count":2`.

## Requirements

- Go toolchain only (the repo's `go.mod` requires go 1.26.6; `GOTOOLCHAIN=auto`
  resolves it from the module cache).
- `curl` (health/register/peers/publish probes) and `sha256sum` (coreutils —
  derives each agent's 64-hex registry `public_key` from its id).
- `github.com/gorilla/websocket` v1.5.3 — **already in `go.mod`/`go.sum`**.
  No `go get`, no pip/npm/apt, no new dependencies. Nothing outside `examples/`
  is touched, and the demo writes **nothing** into the repo.

## Quick start

```bash
cd examples/ws-mesh-demo
bash run-demo.sh                        # scratch port 18961
DEMO_PORT=19999 bash run-demo.sh        # or any free port
bash run-demo.sh -h                     # usage + the registration precondition
```

The script builds the server and demo client into a `mktemp` dir (never the
repo), then runs **8 steps**: build → start relay → HTTP-register both peers →
spawn subscriber → spawn both mesh peers → assert `/mesh/peers` → publish →
assert the subscriber received it. It fails loudly unless **all** of these hold:

- `POST /agents` returns `201` for `demo-agent-a` and `demo-agent-b`, and both
  ids are listed by `GET /agents`;
- `GET /mesh/peers` returns `count:2` with `demo-agent-a` + `demo-agent-b`;
- the subscriber receives `hello from run-demo` over WS.

### Registration precondition

`run-demo.sh` registers both peers (`POST /agents`, step 3/8) **before** either
one opens `ws://…/mesh/connect/<agentID>` (step 5/8) — a mesh peer in a real
deployment is a known agent, and the demo models that ordering.

Wire contract, verified live against the server the script starts:

| Call | Body / result |
|------|---------------|
| `POST /agents` | `{"id":"<agent>","capabilities":["demo","mesh"],"public_key":"<64 hex>"}` → `201` + the agent object |
| `POST /agents` (duplicate id) | → `409 {"error":"agent already registered: …"}` |
| `POST /agents` (missing/short `public_key`) | → `400` — the key must be exactly 64 hex chars |
| `GET /agents` | `{"agents":[…]}` — lists registered ids |

`GET /mesh/peers` itself reports **live WebSocket connections**, not registry
rows: an id that never called `POST /agents` still shows up there and is removed
again on close (`internal/mesh/handler.go`, `internal/mesh/peer.go` `OnClose`).
Registration is a fidelity step here, not a requirement of the mesh view.

## Two guards that keep a run honest

The demo never measures a server it did not start:

1. **Port pre-check** — if anything already listens on `$DEMO_PORT` the script
   aborts with `FAIL: something is already listening on :<port>`. (Before this
   guard, a `DEMO_PORT` run whose clients fell back to the compiled-in default
   port measured a *docker-published* crier that happened to own that port, and
   failed as `FAIL: /mesh/peers count != 2` with a foreign peer list.)
2. **Empty-peer assertion** — right after startup the script requires
   `GET /mesh/peers` to report `count:0`, and fails if its own relay process
   exits during the readiness loop. A foreign server answering on the same port
   would already list peers and is caught here instead of being measured.

Every demo client is spawned with `-url "$BASE"`, so `DEMO_PORT` moves the
server **and** the clients together.

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
| (both)     | `-url`  | Server base URL, default `http://127.0.0.1:18961`.              |

`subscribe` prints `SUBSCRIBED <topic>` after the WS upgrade and one
`EVENT <payload>` line per received event (payload is the raw event JSON the
relay fans out). `peer` prints `PEER CONNECTED <agentID>` and then holds the
socket open (discard-read loop keeps gorilla's default ping handler
auto-ponging, so the relay's keepalive never drops the peer — a closed WS
removes the peer from `/mesh/peers`, see `internal/mesh/peer.go` `OnClose`).

## Transcript

Live output is teed into a transcript **outside the repo**:

```
${TMPDIR:-/tmp}/ws-mesh-demo-TRANSCRIPT-<date>.XXXXXX.md     # mktemp-derived
```

Its path is printed as the last line of the run, and it can be redirected with
`DEMO_TRANSCRIPT=/path/to/file bash run-demo.sh`. Concurrent runs get separate
files; new runs never dirty `git status`. The historical, tracked
`TRANSCRIPT-2026-08-31.md` is left in place as-is.

## Endpoints exercised

| Endpoint | Method | Purpose | Source |
|----------|--------|---------|--------|
| `/agents` | POST | Register an agent: `{"id","capabilities","public_key"}` → 201 | `cmd/server/main.go:306` |
| `/agents` | GET | List registered agents | `cmd/server/main.go:307` |
| `/relay/publish` | POST | Publish `{"topic","event"}`; 202 on success | `cmd/server/main.go:105` |
| `/relay/subscribe/{topic}` | WS | Fan out raw event JSON as text frames | `cmd/server/main.go:106` |
| `/mesh/connect/{agentID}` | WS | Register a peer on upgrade | `cmd/server/main.go:112` |
| `/mesh/peers` | GET | `{"peers":[{"agent_id":...}],"count":N}` | `cmd/server/main.go:113` |

Wire details: `docs/mesh-protocol.md`, OpenAPI: `cmd/server/openapi.yaml`.

## Notes

- The demo runs the relay **auth-disabled** (`CR_AUTH_TOKEN` empty → pass-through
  per `internal/middleware/auth.go`); `CR_REQUIRE_AGENT_SIG` is forced false, so
  `POST /agents` needs no per-agent signature. The script unsets any ambient
  `CR_AUTH_TOKEN` first.
- Publishing requires `X-Agent-ID` when rate limiting is on (default 100/min)
  — the demo sends `X-Agent-ID: demo-publisher`.
- The default port is the scratch `18961`, deliberately clear of the fleet's
  long-lived listeners (`:8767`, `:18767`); `run-demo.sh` pre-checks and the
  client default matches the script's default.
- Build artifacts and per-step client logs live in a `mktemp` dir; the repo stays
  clean apart from this directory's own tracked files.

## Regression tests

`run_demo_test.go` pins the DF-CRIER-152 failure modes as source invariants:
every client invocation carries `-url "$BASE"`, the transcript is written
outside the repo, the port is pre-checked before the relay starts, the peers are
registered before their mesh connections, and the `-h`/README text states the
registration precondition.

```bash
go test ./examples/ws-mesh-demo/ -count=1
```
