# ws-mesh-demo — WebSocket subscribe + mesh demo (CR-GAP-050, DF-CRIER-3)

A runnable, **zero-external-install** demo of the crier relay's two live
WebSocket surfaces, proven end-to-end against one real server it starts itself:

1. **Agent registration** — both demo peers are HTTP-registered first
   (`POST /agents` → `201`), so they exist in the agent registry as well as on
   the mesh.
2. **Relay pub/sub fan-out** — `POST /relay/publish` → subscriber receives the
   event over `ws://…/relay/subscribe/<topic>`, framed as
   `{"topic":"<published topic>","event":<event>}`.
3. **Mesh peer registration** — the two registered peers connect via
   `ws://…/mesh/connect/<agentID>` and appear in `GET /mesh/peers` with
   `"count":2`.
4. **Mesh REQUEST → RESPONSE round-trip** (DF-CRIER-3) — `demo-agent-a` sends a
   REQUEST to `demo-agent-b`, which answers with a RESPONSE whose `request_id`
   is the REQUEST's `message_id`. The requester loop-receives and switches on
   `type`, so the server's KEEPALIVE frames arriving on the same socket are
   ignored instead of being mistaken for the reply.

Everything is asserted: the script exits 0 only when all of it holds.

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
DEMO_KEEPALIVE_WAIT=0 bash run-demo.sh  # skip the ~30s live KEEPALIVE wait
bash run-demo.sh -h                     # usage + the registration precondition
```

The script builds the server and demo client into a `mktemp` dir (never the
repo), then runs **10 steps**: build → start relay → HTTP-register both peers →
spawn subscriber → spawn both mesh peers (B in responder mode) → assert
`/mesh/peers` → publish → assert the subscriber received it → run the mesh
exchange and assert its correlation → cross-check the responder's log. It fails
loudly unless **all** of these hold:

- `POST /agents` returns `201` for `demo-agent-a` and `demo-agent-b`, and both
  ids are listed by `GET /agents`;
- `GET /mesh/peers` returns `count:2` with `demo-agent-a` + `demo-agent-b`;
- the subscriber receives `hello from run-demo` over WS;
- the RESPONSE's `request_id` **equals** the REQUEST's `message_id` (and is
  distinct from the RESPONSE's own `message_id`, so the run can tell the two
  fields apart), with `status_code` 200 and the responder's body object relayed
  verbatim;
- the responder's own log shows it received that same `message_id` and echoed it
  as `request_id`;
- a KEEPALIVE frame that arrived on the requester's socket was classified as
  ignored, never accepted as the reply.

### Registration precondition

`run-demo.sh` registers both peers (`POST /agents`, step 3/10) **before** either
one opens `ws://…/mesh/connect/<agentID>` (step 5/10) — a mesh peer in a real
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

## The mesh exchange (DF-CRIER-3)

The demo's frames are built by `internal/mesh`'s own structs and `mesh.Marshal`
(`Register`, `Request`, `Response`, `Keepalive`), so a tester sees the bytes the
server really exchanges — not a hand-written imitation of them.

```text
REGISTER SENT message_id=<id> agent_id=demo-agent-a          # one-way, no reply
ROUNDTRIP CONNECTED demo-agent-a -> demo-agent-b (ws://…)
REQUEST SENT message_id=<id> target=demo-agent-b method=GET path=/ping trace_id=<id>
RESPONSE RECEIVED request_id=<the REQUEST's message_id> response_message_id=<its own id> status_code=200 body={"pong":true,"from":"demo-agent-b"}
ROUNDTRIP OK request_id=<the REQUEST's message_id> status_code=200 response_message_id=<its own id> keepalives_ignored=1
```

Two rules make the exchange work, and both are asserted by the run:

- **Correlation is on `request_id`, not on arrival order.** The requester accepts
  a frame only when its `request_id` is the `message_id` of the REQUEST it sent
  (`classifyInbound` in `main.go`). The server routes the reply by exactly that
  value (`internal/mesh/peer.go` `forwardResponse`), so a RESPONSE — or an ERROR
  — that does not echo it is dropped and the requester times out. That is also
  why the script asserts the reply's own `message_id` is *different* from the
  correlation id: otherwise the assertion could not tell the two fields apart.
- **`KEEPALIVE` frames interleave on the same socket and must be ignored.** The
  server sends one to every accepted peer every
  `mesh.DefaultMeshConfig().KeepaliveInterval` = **30s**
  (`internal/mesh/peer.go:39-48`; `cmd/server/main.go:149` uses those defaults),
  and a client sends its own on the same cadence. So a client awaiting a
  RESPONSE must read in a loop and switch on `type` — never assume the next
  frame is the answer, and never correlate on the frame's own `message_id`.

`-keepalive-wait` (default 35s inside `run-demo.sh`, set with
`DEMO_KEEPALIVE_WAIT`) makes the requester hold its socket past that 30s tick and
exit 0 only after a real KEEPALIVE frame arrived and was ignored, so the
transcript carries the frame rather than a description of the rule.
`DEMO_KEEPALIVE_WAIT=0` keeps the run short and prints that the live observation
was skipped; the same filter is then exercised by
`TestClassifyInboundFiltersKeepaliveAndForeignReplies` (deterministic frame
classification) and `TestRoundtripIgnoresKeepaliveFramesMidAwait` (a real
in-process mesh whose KEEPALIVE interval is far below the responder's answer
delay, so KEEPALIVE frames land while the RESPONSE is pending — the client's exit
status is the assertion).

Deeper protocol reference (all message types, error codes, silent-drop rules):
[`docs/mesh-protocol.md`](../../docs/mesh-protocol.md).

## Client usage

```
ws-mesh-demo [-url BASE] subscribe -topic TOPIC [-once]
ws-mesh-demo [-url BASE] peer -agent AGENT_ID [-respond]
ws-mesh-demo [-url BASE] roundtrip -agent AGENT -target AGENT
```

| Subcommand | Flag    | Meaning                                                        |
|------------|---------|----------------------------------------------------------------|
| subscribe  | `-topic`| Topic to subscribe to (required).                               |
| subscribe  | `-once` | Exit 0 after the first received event (used by run-demo.sh).    |
| peer       | `-agent`| Agent ID to register as on the mesh (required).                 |
| peer       | `-respond` | Answer inbound REQUEST frames with a RESPONSE (`request_id` = the REQUEST's `message_id`). |
| peer       | `-status` / `-body` | `status_code` and raw JSON body used for that answer (`200`, and an omitted body, by default). |
| peer       | `-respond-delay` | Wait this long before answering — models a slow peer, and lets a caller see KEEPALIVE frames arrive mid-await. |
| roundtrip  | `-agent` / `-target` | Connect as `-agent`, send one REQUEST to `-target` (both required). |
| roundtrip  | `-method` / `-path` / `-body` | The REQUEST's application payload (`GET /ping`, no body, by default). |
| roundtrip  | `-expect-status` | `status_code` the RESPONSE must carry (default 200). |
| roundtrip  | `-timeout` | How long to wait for the RESPONSE (default 15s). |
| roundtrip  | `-keepalive-wait` | After the RESPONSE, hold the socket up to this long for at least one live KEEPALIVE frame (0 = no wait). |
| (all)      | `-url`  | Server base URL, default `http://127.0.0.1:18961`.              |

`subscribe` prints `SUBSCRIBED <topic>` after the WS upgrade and one
`EVENT <payload>` line per received event (payload is the raw event JSON the
relay fans out). `peer` prints `PEER CONNECTED <agentID>`, sends the one-way
`REGISTER` frame, then holds the socket open (every frame is read, so gorilla's
default ping handler keeps auto-ponging and the relay's keepalive never drops the
peer — a closed WS removes the peer from `/mesh/peers`, see
`internal/mesh/peer.go` `OnClose`). `roundtrip` exits 0 only on a correlated
RESPONSE carrying the expected status code, and prints
`ROUNDTRIP OK request_id=… status_code=… response_message_id=… keepalives_ignored=…`
so a shell driver can assert the whole contract from one line.

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
| `/relay/subscribe/{topic}` | WS | Fan out `{"topic":…,"event":…}` frames | `cmd/server/main.go:106` |
| `/mesh/connect/{agentID}` | WS | Register a peer on upgrade; relay REQUEST/RESPONSE between peers | `cmd/server/main.go:112` |
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
- The request leg reconnects as `demo-agent-a` after closing that peer's holding
  socket: the server keys connections by agent ID (`Mesh.AcceptPeer`), so a
  second socket under one id replaces the first — and a late `OnClose` from the
  old socket would delete the new entry and drop the RESPONSE. The script waits
  for `demo-agent-a` to leave `/mesh/peers` before the REQUEST goes out.
- Build artifacts and per-step client logs live in a `mktemp` dir; the repo stays
  clean apart from this directory's own tracked files.

## Regression tests

`run_demo_test.go` pins the DF-CRIER-152 failure modes as source invariants
(every client invocation carries `-url "$BASE"`, the transcript is written
outside the repo, the port is pre-checked before the relay starts, the peers are
registered before their mesh connections, and the `-h`/README text states the
registration precondition) and the DF-CRIER-3 exchange:

- `TestRunDemoDrivesTheExchangeLeg` — the shell driver really runs the roundtrip
  and really asserts the correlation (`request_id` equals the REQUEST's
  `message_id`, and differs from the reply's own `message_id`), the status code,
  the verbatim object body, the responder-side echo, and a classified KEEPALIVE.
- `TestClassifyInboundFiltersKeepaliveAndForeignReplies` — the filter itself:
  KEEPALIVE (and another request's RESPONSE/ERROR) is ignored, only the
  correlated reply is accepted.
- `TestRoundtripIgnoresKeepaliveFramesMidAwait` — a real in-process mesh
  (`mesh.HandleConnect` on a real listener) with a 120ms keepalive interval and a
  900ms responder delay, so KEEPALIVE frames arrive while the RESPONSE is
  pending; the requester's exit status is the assertion.

```bash
go test ./examples/ws-mesh-demo/ -count=1
```

