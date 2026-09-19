# LLM Mesh — two LLMs, one Crier, one puzzle

Proof that Crier's messaging actually carries a multi-LLM collaboration.
Two LLM agents hold **partial clues** to a logic puzzle; neither can solve it
alone; they must exchange information and converge. The controller verifies
their final answers against ground truth.

## The two lanes

Crier has two messaging surfaces, and this directory demonstrates both:

| Lane | Directory | What talks to Crier | Who reads the transport |
|---|---|---|---|
| **Raw protocol** | `mesh/` | Python client (`crier_mesh.py`) | The agent itself — WebSocket frames, correlation ids, keepalives |
| **Bridge (MCP)** | `.` (this dir) | `crier-mcp` — the harness-facing bridge | **Nobody but the bridge.** The harnesses (LLM agents) call MCP tools only: `send_message`, `get_messages`, `ask_agent`, `mesh_peers`, `mesh_request` |

The second lane is the answer to "the messaging exists but it requires the
harnesses to read the data directly": with the bridge, a harness knows
nothing about Crier's wire — no sockets, no signing, no leases, no
correlation ids. Those live in `crier-mcp`, which is part of the Crier
codebase and linked against the same registry package.

## Run

```bash
# Bridge lane (harnesses use MCP tools only) — the main demo
./run-demo.sh

# Raw protocol lane (agents speak the mesh directly)
./mesh/run-demo.sh
```

Both need `DEEPSEEK_API_KEY` in the environment (model: `deepseek-v4-flash`,
override with `--model`). Each run starts its OWN Crier server on a scratch
port that it CHOOSES: it walks a bounded candidate list (18777..18781 on the
bridge lane, 18778..18782 on the raw lane), names the holder of every candidate
it skips, and uses the first free one — so a long-lived unrelated listener on
the first candidate, or a squatter that took it between two runs of the same
script, rotates the run on to the next candidate instead of making it skip
(QA-CRIER-10). The selected port is then passed to the server, the controller,
the harnesses and the raw mesh clients alike, and once the server answers
`/health` the run asserts that the process holding the port is the one it
started (QA-CRIER-9).

- `CRIER_PORT` — use THIS port. It is checked, and the run aborts naming the
  holder when it is occupied; an explicitly requested port is never silently
  rotated, because a run on a port you did not name would misreport what was
  measured.
- `CRIER_PORT_CANDIDATES` — how many candidates the default rotation may try
  (default 5). When they are all occupied the run fails, listing every
  attempted port and its holder, instead of skipping.

Each run then prints the full conversation and exits 0 only if both agents
solved the puzzle.

## The puzzle

Four plots (A, B, C, D) hold tomato, lettuce, carrot, pepper — one each.

- **agent-a** knows: Plot C is lettuce · Plot A is not pepper
- **agent-b** knows: The tomato is in plot A · Plot B is not tomato ·
  Tomato and carrot are in plots A and D, in some order

Solution: A=tomato, B=pepper, C=lettuce, D=carrot. Each agent alone is stuck;
together they have it.

## What's in here

- `mcp_client.py` — minimal MCP stdio client (the generic harness↔bridge interface)
- `harness.py` — LLM agent: drives the puzzle with four tools
  (`get_messages`, `ask_peer`, `answer_peer`, `submit_final`), all bridged
- `controller.py` — third harness-side peer: collects finals, verifies,
  sends verdicts, and exercises the live mesh lane (peers + PING)
- `task-garden.json` — the puzzle (clues + ground truth)
- `mesh/` — the raw-protocol lane (same puzzle, direct wire)

## Bridge tools added to crier-mcp

- `send_message(agent_id, payload, reply_to?)` — durable inbox send; the
  bridge merges correlation fields into the payload
- `get_messages(max?)` — own inbox; the bridge owns lease + ack
- `ask_agent(agent_id, payload, timeout_s)` — blocking request/reply over
  the inbox (deliver + correlated poll)
- `mesh_peers()` — who's on the live mesh
- `mesh_request(target, method, path, body, timeout_ms)` — live
  REQUEST/RESPONSE round-trip through the bridge's own WebSocket connection

Config for the bridge: `CRIER_HTTP_URL` (remote-server mode), `CRIER_AGENT_ID`
(bridge identity), `CRIER_MESH_URL` (enables the mesh tools), `CRIER_AUTH_TOKEN`
(optional bearer).

## Protocol notes the demo surfaces

- **Two-way blocking RPC deadlocks conversational agents.** Both agents calling
  `ask_agent` first each waited 60s for a reply the other never sent. Harnesses
  need interruptible waits (poll + answer incoming while waiting); the raw
  lane avoids it structurally (responder on the connection thread).
- **Silent drops make client bugs invisible.** The Python client's timestamps
  (`-0500`, missing colon) weren't RFC3339 → Go's `time.Time` unmarshal failed
  → the server silently dropped every frame. Client-side validation is
  mandatory; a server-side log-on-parse-failure would be a good protocol
  improvement.
- The mesh has no automatic reconnect: a bridge holds one connection for its
  lifetime and retries only at startup.
- Incoming mesh REQUESTs to a bridge are auto-answered `bridge_alive` — the
  mesh is the liveness lane, the durable inbox is the content lane for LLM
  traffic. That split is a design decision this demo makes explicit.
- RESPONSE `body` is **responder-controlled and relayed verbatim**: the server keeps
  the frame's raw bytes (`json.RawMessage`) and never re-encodes the value, so an
  object body comes back as an object. This demo sees a JSON *string* only because
  its own client stringifies the payload (`mesh/crier_mesh.py`:
  `"body": json.dumps(body)`). The `mesh_request` bridge decodes the body into a map,
  so a string body reaches an MCP client as `"body": null`, not as a string
  (DOGFOOD-MESH-2 — see the RESPONSE section of docs/mesh-protocol.md).
- With `CR_REQUIRE_AGENT_SIG=false`, `crier-mcp`'s remote mode needs only the
  agent id header; with signing enabled, the bridge would hold the agent key
  and sign on the harness's behalf (not yet implemented).
