# Crier — Agent-to-Agent Message Bus

[![Go Version](https://img.shields.io/badge/Go-1.26.6%2B-00ADD8?logo=go)](https://go.dev)
[![CI](https://github.com/crier-dev/crier/actions/workflows/ci.yml/badge.svg)](https://github.com/crier-dev/crier/actions/workflows/ci.yml)
[![bunker-e2e](https://github.com/crier-dev/crier/actions/workflows/bunker-e2e.yml/badge.svg)](https://github.com/crier-dev/crier/actions/workflows/bunker-e2e.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**Communication backbone for the autonomous agent economy** — extracted and
generalized from Hivemind. Crier lets bots, scripts, and AI agents exchange
messages over topics, direct mesh RPC, durable inboxes, webhooks, and
bus-to-bus federation — self-hosted, in one Go binary.

> **Testing crier?** Start with [TESTERS.md](TESTERS.md) — a per-mode checklist,
> the known rough edges, and how to report. A **static** spec browser ships at
> **/docs** on any running server: one self-contained HTML page (no CDN, no
> Swagger-UI — it works offline) that links the machine-readable
> **/openapi.json** and **/openapi.yaml**. It is *not* an interactive request
> console — fire your requests with curl or any HTTP client, as TESTERS.md §1 does.

![The crier fleet — agents on their own platforms, passing glowing messages along luminous paths](docs/img/agents-wide.png)

<p align="center"><i>The fleet: every agent gets a mailbox, a voice, and a place in the mesh.
Meet the mascots → <a href="#who-its-for">builder · home · business · courier</a></i></p>

Crier provides four primitives for agent communication:

## Architecture

### 1. Relay (Pub/Sub)

Central message relay. Agents publish events to topics; subscribers receive them over WebSocket.

- HTTP + WebSocket transport
- Topic-based routing with subscriber counts
- Every subscription frame names the literal published topic:
  `{"topic":"<published topic>","event":<event>}` — the event is delivered
  exactly as published (never string-encoded), and the topic field is what lets
  a wildcard subscriber know which topic matched
- Publishing is fire-and-forget: `POST /relay/publish` returns 202 as soon as the event is accepted, and if the topic has **zero subscribers** the event is dropped by design — publishes to empty topics are not queued or retained. A topic only appears in `GET /relay/topics` after at least one subscriber connects (subscribe first, then publish; a publish to an empty topic does not create it)
- Rate limiting per agent (100 events/minute) — requires the `X-Agent-ID` header when enabled; `0` disables both
- Bearer token authentication

### 2. WebSocket Mesh (Peer-to-Peer)

Direct agent-to-agent communication layer with discovery, keepalive, and request/response correlation.

- Peer discovery via registry
- One-way REGISTER on connect (fire-and-forget; the server never sends REGISTER_ACK — see `docs/mesh-protocol.md`)
- 30-second keepalive loop
- Concurrent request/response with timeout tracking
- Clean shutdown with WebSocket close frames

### 3. Agent Registry

Every agent has a discoverable identity with capability cards.

- Register/unregister with ed25519 public key
- List and detail endpoints with registration status (see note below)
- Capability-based routing (future)

> **What `status` means — registration-liveness only.** An agent's `status` is
> set to `"online"` at registration and never changes until unregistration; it
> is NOT live-connection health. `last_seen` is set at registration and
> advanced by a successful `PATCH /agents/{id}` — that PATCH is the only
> activity the registry records, and the value it returns is the value now
> persisted. The registry has no heartbeat source today
> (mesh KEEPALIVE frames are sent but not processed server-side), so an agent
> that never PATCHes — including one whose process crashed — still reports
> `"online"` with a `last_seen` frozen at its registration or last PATCH.
> For live-connection truth, use the mesh: `GET /mesh/peers` lists agents with
> an active WebSocket connection (see [Try the Mesh](#try-the-mesh)).

### 4. Inboxes

Durable per-agent FIFO queues with lease-based delivery. Durability is backend-dependent: with `CR_DATABASE_URL` set (PostgreSQL backend) agents — including their `webhook` and `guard` configs — and undelivered messages survive server restarts; without it the in-memory backend is used (process-lifetime only).

- Lease prevents double-delivery: messages are leased for N seconds on retrieval
- ACK confirms delivery; un-ACKed messages return to queue after lease expiry
- TTL expiry auto-purges stale messages (default 24h; optional per-message `ttl_seconds`, `0` = never expires, reported on the wire as `"expires_at":null`)
- Every message is leased to exactly one retriever; a concurrent retriever receives only what the first did not lease (`queue_depth`/`leased_count` on the retrieve body disambiguate an empty `messages` array: `leased_count` > 0 means HELD, not lost — DF-CRIER-177)

### 5. Webhook delivery (bypasses the inbox)

An agent that registers a `webhook` config (`PATCH /agents/{id}` with
`{"webhook":{"url":…}}`) receives its messages at that endpoint instead of its
durable inbox (CR-FEAT-001, [`specs/WEBHOOK-DELIVERY.md`](specs/WEBHOOK-DELIVERY.md) §4).
The inbox is **not** written: `GET /agents/{id}/inbox` for a webhook-configured
agent is empty by design, so an empty retrieve is not evidence that a message
was never sent — the message may have gone to the endpoint (or failed there).
The same holds for leased messages on a non-webhook agent: an empty `messages`
array with `leased_count` > 0 means another retriever holds an unexpired lease
(the message returns after lease expiry or ack), not that it was never
delivered (DF-CRIER-177).

The deliver accept names the transport, so a sender never has to infer it from
the status code (DF-CRIER-157):

| Accept | `transport` | `delivery_mode` | Meaning |
|--------|-------------|-----------------|---------|
| `200` | `webhook` | — | blocking: the endpoint's reply is in `reply` |
| `202` | `webhook` | `async` \| `batch` | accepted for **webhook delivery, queued — not stored** |
| `201` | `inbox` | — | stored in the durable inbox (`expires_at` present: RFC 3339, or `null` when it never expires) |

A `202` is a promise about the queue, not about delivery: the endpoint can
still fail afterwards. A blocking delivery the endpoint permanently rejects
(a non-retryable 4xx, or a 2xx whose body the agent's reply schema cannot map)
answers `502` — retrying the identical message cannot succeed. Only a timeout
or an exhausted budget answers `504`, where a later attempt can still land.

**Recovery when an async/batch delivery dies.** After the queue exhausts its
bounded retries (`CR_WEBHOOK_MAX_RETRIES` failing attempts, or the endpoint's
own smaller `webhook.retries` budget, one per `CR_WEBHOOK_REDELIVER_S` tick —
5 failed attempts / 30s by default, i.e. roughly two to three minutes after the
accept), the item is dropped and
exactly one durable notification is written into the **sender's own inbox**. It
is a direct store write: it never routes back through webhook delivery, so it
cannot recurse — even when the sender itself is a webhook-configured agent.

```json
{"kind":"error","code":"WEBHOOK_FAILED","message_id":"<original>","target":"<recipient agent>","retries":6,"status_code":503,"error":"status 503"}
```

`status_code` is omitted and `error` carries the transport error string when the
failure was transport-level (no HTTP response). The notification is
best-effort and never retried: a missing `sender` on the rejected message, or an
unregistered sender, is logged instead. While an endpoint is degraded (circuit
opened after `CR_WEBHOOK_CIRCUIT_THRESHOLD` consecutive failures) queued
redeliveries pause instead of POSTing and resume when a probe
(`CR_WEBHOOK_PROBE_S`) succeeds, so a poisoned endpoint can take much longer
than the default cadence to reach exhaustion.

## Quick Start

### Prerequisites

- Go 1.26.6 or later
- OpenSSL 3.x or later with `xxd` on PATH — the quickstart signing helper uses `openssl pkeyutl -sign -rawin`, an OpenSSL 3+ flag. On older OpenSSL the helper fails loudly instead of signing (see below). That flag is also a ONE-SHOT operation: the payload must be a seekable file (the helper writes it and signs it with `-in`), because a piped or redirected payload makes `pkeyutl` fail with a zero-byte signature — the helper refuses that too, instead of sending it

### Build

```bash
make build
```

Or build directly:

```bash
go build -o bin/crier ./cmd/server
```

`make build` stamps the build identity (`version`, `commit`, `build_time`) into
the binary via `-ldflags`. A build with no ldflags at all — the bare `go build`
above — still reports the git commit, because `internal/buildinfo` falls back to
the VCS metadata the Go toolchain embeds. Ask a binary or a running server what
it is:

```bash
./bin/crier -version          # crier v1.2.3-1a2b3c4d   (TAGGED build: tag v1.2.3 at commit 1a2b3c4d)
curl -s localhost:8767/version # {"version":"1.2.3","commit":"1a2b3c4d",...}
```

The `v1.2.3-1a2b3c4d` line is what a TAGGED build prints, so it is not what a
fresh untagged clone shows. One source (`internal/buildinfo`) and one format
(`v<version>-<commit>[-dirty]`, the `v` glued on only for a real stamped
version) produce these, and no artifact of one checkout can report a different
identity — all 4 build paths that compile a crier binary (`make build`,
`make build-mcp`, `Dockerfile`, `Dockerfile.mcp`) stamp it, and a
`make docs-check` claim fails if one of them stops:

| Build | `-version` prints | Version segment comes from |
|-------|-------------------|----------------------------|
| `make build`, tagged commit | `crier v1.2.3-1a2b3c4d` | the tag, via `git describe --tags` |
| `make build`, untagged checkout | `crier v<describe>-<commit>`, e.g. `crier v9c74185-dirty-9c741850` | `git describe --always --dirty`: the short commit, `-dirty` when the tree had uncommitted changes |
| bare `go build -o bin/crier ./cmd/server` | `crier dev-<commit>`, e.g. `crier dev-9c741850-dirty` | nothing stamped, so the version segment is the `dev` sentinel — rendered bare, never `vdev` — and the commit comes from the Go toolchain's VCS metadata |
| `docker build .` / `make docker-build` | the same string `make build` prints for the same tree | the image build derives the same `git describe` values and stamps them; `--build-arg VERSION=… COMMIT=… BUILD_TIME=…` overrides |

(`<describe>` and `<commit>` are placeholders — the shas shown are one example
checkout, so yours will differ; the shape is the stable part.)

The MCP server carries no identity of its own either: `crier-mcp --version`
prints the full identity, and its `initialize` result answers
`serverInfo.version` with the version segment of that same identity (never a
literal version), so a client and the CLI can never disagree about which build
is running.

Override the version with `make build VERSION=1.2.3`; `make build` with no
override uses `git describe`.

### Run

```bash
# Default port :8767
make run
```

On a shared host the default port is often already held by a leftover server
from an earlier session. Check before starting:

```bash
ss -tlnp | grep :8767         # who holds the default port? (empty output = free)
```

If it is taken, start on a free port and confirm which build answered —
`GET /version` is one of the five unauthenticated paths:

```bash
./bin/crier -port 8768         # or: CRIER_PORT=8768 ./bin/crier
curl -s localhost:8768/version # {"version":"...","commit":"..."}
```

If the bind fails anyway, the server exits non-zero naming the port, the
holder-check command and the `-port`/`CRIER_PORT` alternative, plus the build
identity of the binary that failed to start.

### Stop / restart

```bash
make stop
```

`make run` starts the server with a pidfile (`.crier.pid` at the repo root —
gitignored; override with `PIDFILE=<path>` for `make run`/`make stop`, or set
`CR_PIDFILE`). The pidfile is written only after the port is actually bound —
a failed bind leaves no pidfile — and records the pid, the port and the
absolute binary path. `make stop` reads it, verifies the recorded pid is
still running that exact binary (via `/proc/<pid>/exe`), and sends one
SIGTERM, which triggers the server's graceful shutdown. With no pidfile
`make stop` is a clean no-op success, so it is safe in any state:

```bash
$ make stop
./bin/crier -stop -pidfile .crier.pid
crier: nothing to stop — no pidfile at .crier.pid
```

A stale pidfile (the server died without cleanup) is removed and reported;
a pidfile whose pid now runs a *different* binary is refused with both
paths printed and **nothing is signalled** — the stop command never
SIGKILLs and never matches by port or process name, so it cannot kill an
unrelated process.

Lost the launcher? A server started detached (`setsid make run …`, then the
launcher exits) keeps holding its port — that server is exactly what
`make stop` reaches, because the pidfile names the server process, not the
launcher:

```bash
$ setsid make run &        # launcher exits; server keeps running
$ make stop
./bin/crier -stop -pidfile .crier.pid
crier: stopping pid 4169877 (SIGTERM)
crier: pid 4169877 stopped
```

Older servers started without a pidfile (before this existed) are not
reachable by `make stop`. Find them by port and stop them manually —
SIGTERM (the default `kill`) and Ctrl-C both trigger the same graceful
shutdown:

```bash
ss -tlnp | grep :8767         # who holds the default port?
kill <pid>                    # SIGTERM — graceful shutdown
```

> **The LLM message guard is ON by default.** Every inbound delivery is
> classified by a guard LLM (default model `deepseek-v4-flash`, 10s
> per-message budget — `CR_GUARD_TIMEOUT_MS`) before it is webhook-POSTed
> or inbox-stored. For local dev without an API key, set
> `CR_GUARD_ENABLED=false`; to exercise the guard, set `DEEPSEEK_API_KEY`.
> Without a key the guard call fails (`reason: guard_error: all providers
> failed: no provider api key`) and the guard fails OPEN — the delivery
> proceeds, the deliver response carries `"guard":{…,"errored":true}` and the
> outbound webhook POST carries `X-Crier-Guard-Error: true`. Note the over-cap
> path is deterministic: it runs regardless of the guard LLM's health, so a
> keyless deployment still gets it. If a workload legitimately sends
> machine-generated bodies above the cap, raise
> `CR_GUARD_MAX_PAYLOAD_BYTES`, or silence the noisy class per policy
> (`"checks":{"masquerade":false}` — the class's prematch patterns are then
> suppressed); extra patterns appended via `CR_GUARD_PATTERNS_EXTRA` are
> high-confidence (able to block an over-cap payload) unless they declare
> `"confidence":"low"`, which opts them into the report-only treatment.
> See
> [Message guard (LLM)](#message-guard-llm).

### Try it

A minimal register → deliver → retrieve round-trip with the default signed configuration. If you started the server with `CR_AUTH_TOKEN` set (auth enabled), every request except the **five exempt paths** — `/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs` (the list is the switch in `internal/middleware/auth.go`) — needs the Bearer header shown below; if `CR_AUTH_TOKEN` is unset, auth is disabled and the header can be dropped. Measured on a running server: all five answer `200` with no token, and `GET /agents` answers `401`:

```bash
AUTH=(-H "Authorization: Bearer ${CR_AUTH_TOKEN:-}")

# 0. One-time setup: generate an ed25519 keypair for agent-1 (needs openssl 3.x + xxd)
openssl genpkey -algorithm ED25519 -out /tmp/crier-agent.key >/dev/null 2>&1
PUBKEY_HEX=$(openssl pkey -in /tmp/crier-agent.key -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)
# sig helper: hex(ed25519_sign("METHOD\nPATH\nTS", key)) — same wire format as examples/demo.sh.
# Requires OpenSSL >= 3 for pkeyutl -sign -rawin; on older OpenSSL it errors loudly
# instead of producing an empty (silently-401-rejected) signature. That flag is also a
# ONE-SHOT operation: the payload must be a seekable file passed with -in, so the helper
# refuses a zero-byte signature rather than sending it (a piped payload fails with
# "unable to determine file size for oneshot operation" and would 401 blaming the headers).
sig() { if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then echo "ERROR: this signing helper requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)" >&2; return 1; fi; printf '%s\n%s\n%s' "$1" "$2" "$3" > /tmp/crier-payload.txt; _sig=$(openssl pkeyutl -sign -rawin -inkey /tmp/crier-agent.key -in /tmp/crier-payload.txt 2>/dev/null | xxd -p -c 128); if [ -z "$_sig" ]; then echo "ERROR: signing produced an EMPTY signature. pkeyutl -sign -rawin is a one-shot operation and needs a SEEKABLE payload passed with -in <file> — a piped or redirected payload fails with 'unable to determine file size for oneshot operation' and yields zero bytes, which the server rejects 401 naming the empty X-Agent-Sig header." >&2; return 1; fi; printf '%s\n' "$_sig"; }

# 1. Register an agent (public_key = hex-encoded ed25519 public key; required
#    whenever signature enforcement is on — the default. Only a server run
#    with CR_REQUIRE_AGENT_SIG=false accepts registration without it.)
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"id\":\"agent-1\",\"public_key\":\"${PUBKEY_HEX}\",\"capabilities\":[\"demo\"]}"
# 201

# 2. Deliver a message to its inbox. `ttl_seconds` is optional: absent keeps the
#    24h default, 0 means the message never expires and is reported as
#    "expires_at":null (the key is present, the value is null — never the zero
#    time). `transport` in the body is
#    always present: "inbox" here, "webhook" when the target has a webhook
#    configured (then the inbox is bypassed — see §5 of the architecture notes).
curl -s -X POST localhost:8767/agents/agent-1/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world"},"ttl_seconds":3600}'
# 201 {"id":"...","transport":"inbox","expires_at":"..."}  (expires_at = created_at + ttl_seconds)

# 3. Publish to a relay topic (pub/sub). `X-Agent-ID` is the header the relay
#    requires — the per-agent rate limiter keys on it and a publish without it
#    is 401. The signature trio is NOT verified on this endpoint (measured at
#    HEAD: a stale `X-Agent-Ts` and a bogus `X-Agent-Sig` both still answer
#    202); it is enforced on the agent-scoped endpoints in steps 4-6, which is
#    why the shared `sig` helper is used here too. A publish to a topic with no
#    live subscriber still answers 202 and drops the event (see §1).
TS=$(date +%s)
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8767/relay/publish "${AUTH[@]}" \
  -H 'Content-Type: application/json' -H 'X-Agent-ID: agent-1' \
  -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig POST /relay/publish "$TS")" \
  -d '{"topic":"demo-topic","event":{"hello":"subscribers"}}'
# 202 — drop the trio and it is still 202; drop `X-Agent-ID` and it is 401.

# 4. Retrieve — agent-scoped endpoints require per-agent request signatures by
#    default (CR_REQUIRE_AGENT_SIG=true). This covers inbox retrieve/ack/stats
#    AND DELETE /agents/{id}. Headers:
#      X-Agent-ID  agent id
#      X-Agent-Ts  unix seconds (must be within ±30s of the server clock)
#      X-Agent-Sig hex ed25519 signature over "METHOD\nPATH\nTS"
#    Timestamps are generated fresh below — never hardcode them (stale timestamps
#    are rejected with 401).
TS=$(date +%s)
curl -s localhost:8767/agents/agent-1/inbox "${AUTH[@]}" \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig GET /agents/agent-1/inbox "$TS")"
# 200 {"messages":[{"id":"...","payload":"eyJoZWxsbyI6IndvcmxkIn0=","lease_id":"..."}],"lease_id":"...","queue_depth":1,"leased_count":1}
# queue_depth/leased_count disambiguate an empty batch: messages=[] with
# leased_count>0 = everything queued is HELD under a lease (DF-CRIER-177)
# Note: message payloads are base64-encoded on the wire ([],byte form)

# 5. Ack the message — message_ids is REQUIRED (an ack without it is rejected
#    with 400: it would otherwise be a silent no-op and the message would be
#    redelivered after lease expiry). Sign "POST\n/agents/agent-1/inbox/ack\n<ts>".
TS=$(date +%s)
curl -s -X POST localhost:8767/agents/agent-1/inbox/ack "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig POST /agents/agent-1/inbox/ack "$TS")" \
  -d '{"lease_id":"<lease_id from retrieve>","message_ids":["<id from retrieve>"]}'
# 204 — message permanently removed (never redelivered after lease expiry)

# Dev shortcut: disable signing for trusted single-user setups
CR_REQUIRE_AGENT_SIG=false make run
# With signing disabled, POST /agents no longer needs a public_key either —
# register with just an id:
#   curl -s -X POST localhost:8767/agents -d '{"id":"agent-1"}'        # 201
# A keyless agent registered this way is unusable on a server that enforces
# signing (its agent-scoped calls answer 401 "no registered public key"), so
# re-enabling CR_REQUIRE_AGENT_SIG later means re-registering with a key.
curl -s localhost:8767/agents/agent-1/inbox
# 200 — no signature headers required

# 6. Delete the agent — DELETE /agents/{id} requires the same per-agent
#    signature (not just inbox endpoints). Sign "DELETE\n/agents/agent-1\n<ts>".
TS=$(date +%s)
curl -s -X DELETE localhost:8767/agents/agent-1 "${AUTH[@]}" \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig DELETE /agents/agent-1 "$TS")"
# 204 — agent removed (401 without the signature headers)
```

> Prefer the runnable script: [`examples/demo.sh`](examples/demo.sh) performs the
> full register → deliver → signed retrieve → ack round-trip with an ephemeral
> ed25519 keypair (openssl 3.x). Start the server, then run `./examples/demo.sh`.

### Remote MCP mode (crier-mcp)

`crier-mcp` bridges MCP clients (Claude Code, Cursor, …) to a running Crier
server instead of an in-process store: set `CRIER_HTTP_URL` (plus
`CRIER_AGENT_ID`) and every registry/inbox tool call becomes a signed HTTP
request against that server.

Because the server enforces per-agent signatures by default
(`CR_REQUIRE_AGENT_SIG=true`), the bridge signs every request with an ed25519
key. It registers its own identity on startup, so there is no manual
registration step: start the server and run the bridge.

```bash
# Run the MCP bridge against the remote server (all four vars are read at startup)
export CRIER_HTTP_URL=http://localhost:8767
export CRIER_AGENT_ID=mcp-agent
# Recommended for any long-lived bridge: a stable key whose public half the
# server keeps across restarts. Optional — see "Signing keys" below.
openssl genpkey -algorithm ED25519 -out ~/.config/crier/mcp-agent.key
export CRIER_AGENT_PRIVATE_KEY_FILE=$HOME/.config/crier/mcp-agent.key
# Optional shared bearer token, only when the server runs with CR_AUTH_TOKEN:
# export CRIER_AUTH_TOKEN=...
# CRIER_AUTH_TOKEN is the bridge's own name; CR_AUTH_TOKEN is accepted as an
# alias for the same shared secret (setting only CR_AUTH_TOKEN works — the
# bridge logs which variable it used, and warns if both are set to different
# values, in which case CRIER_AUTH_TOKEN wins).
make build-mcp && ./bin/crier-mcp
```

The launcher's stdout carries only JSON-RPC frames — the build's own diagnostics go
to stderr — so a strict stdio client may launch that line as-is; `make mcp` does the
same in a single command.

**Automatic registration (no operator step).** On startup in remote mode
crier-mcp registers `CRIER_AGENT_ID` on the server with its public key and the
`mcp`/`bridge` capability tags, then logs the outcome. Registration is
idempotent — an existing identity is left untouched, so re-running is a no-op.
This matters because the bridge polls *its own* inbox for replies
(`get_messages`, `ask_agent`): if the server never heard of that agent id, the
reply leg fails with `agent not found` (a 404). A registration failure (server
unreachable, …) is logged at error level and does **not** abort startup, so a
harness already running is never killed by a bootstrap step.

**Signing keys.** Each request carries a fresh `X-Agent-Ts` / `X-Agent-Sig`
signing `METHOD\n<path>\n<unix-seconds>` — the query string is excluded.

- `CRIER_AGENT_PRIVATE_KEY_FILE` must contain a PKCS#8 PEM ed25519 private key
  (`openssl genpkey -algorithm ED25519`); anything else (unreadable file, wrong
  format, RSA/EC key) is an explicit startup error. Key material is never
  logged or echoed.
- **Unset, the bridge generates an ephemeral ed25519 key for that run** and
  signs with it — enough to work against a signed server on a fresh in-memory
  server, and enough for `CR_REQUIRE_AGENT_SIG=false` servers (which ignore the
  signature entirely).
- The ephemeral key is regenerated on every run, while a persistent server
  keeps the public key registered by the **first** run. Later runs therefore
  find the id already registered with a *different* key and log an error:
  every signed request would be rejected with 401. Remedies — set
  `CRIER_AGENT_PRIVATE_KEY_FILE` to the private key whose public half is
  registered for `CRIER_AGENT_ID`, or delete the stale agent
  (`curl -X DELETE localhost:8767/agents/mcp-agent`) and restart, or point the
  bridge at a fresh in-memory server. Set the variable for any long-lived
  bridge.

#### MCP tools

The MCP server exposes **13 tools** (measured live via a `tools/list` stdio
exchange). The argument names below are the properties of each tool's
`InputSchema` in `internal/mcp/server.go`; `*` marks a required argument.

| Tool | Arguments |
|------|-----------|
| `register_agent` | `id`*, `public_key` (hex ed25519, 64 chars; required whenever signature enforcement is on — the default. Only an MCP bridge on a server run with `CR_REQUIRE_AGENT_SIG=false` accepts registration without it), `capabilities` (string array, default `[]`) |
| `list_agents` | _none_ |
| `get_agent` | `id`* |
| `unregister_agent` | `id`* |
| `deliver_message` | `agent_id`*, `payload`* (JSON object) |
| `retrieve_inbox` | `agent_id`*, `max_messages` (int 1-100, default `10`), `lease_seconds` (int 1-3600, default `30`) |
| `ack_messages` | `agent_id`*, `lease_id`*, `message_ids`* (string array, min 1) |
| `inbox_stats` | `agent_id`* |
| `send_message` | `agent_id`*, `payload`*, `reply_to` (optional correlation id) |
| `get_messages` | `max` (int 1-100, default `10`) |
| `ask_agent` | `agent_id`*, `payload`*, `timeout_s` (default `30`, max `300`) |
| `mesh_peers` | _none_ |
| `mesh_request` | `target`*, `method`*, `path`*, `body` (opaque JSON), `timeout_ms` (default `15000`) |

Unknown arguments are rejected: a member of `arguments` that is not one of the
tool's declared properties is a normal tool error naming it — `invalid
arguments: unknown argument "max_messges"` — instead of being dropped and
silently replaced by a default. The contract covers the top-level `arguments`
object only: the value of an opaque `payload` (`deliver_message`,
`send_message`, `ask_agent`) or `body` (`mesh_request`) is passed through
untouched, so what lives inside it is the caller's business (DF-CRIER-190).

##### What each tool needs

`tools/list` is the only metadata an MCP client sees before it calls a tool, so
every tool whose handler needs environment says so in its own description, and
`crier-mcp` logs **one line at startup** naming the tools that cannot work in the
mode it started in (`mode`, `tools`, `available_tools`, `unavailable_tools`,
`missing_env`). "Needs" is the environment the handler checks before it does
anything; "bridge scope" is the server-side rule for a remote bridge — the
bridge presents its own identity (`CRIER_AGENT_ID`) on every agent-scoped
request, and a server running with signature enforcement on
(`CR_REQUIRE_AGENT_SIG=true`, the default) answers 403
`agent "<bridge>" may only access its own resources` for any other agent.

| Tool | Needs | Notes |
|------|-------|-------|
| `register_agent`, `list_agents`, `get_agent`, `deliver_message`, `send_message` | _nothing_ | work in every mode; delivery and reads of the registry are not restricted to the bridge's own agent |
| `retrieve_inbox`, `ack_messages`, `inbox_stats` | _nothing_ | bridge scope: on a remote bridge `agent_id` must be the bridge's own identity (403 otherwise) |
| `unregister_agent` | _nothing_ | bridge scope: same rule — `id` must be the bridge's own identity on a remote bridge |
| `get_messages` | `CRIER_AGENT_ID` | reads the bridge's own inbox and acks what it returns; fails immediately without it |
| `ask_agent` | `CRIER_AGENT_ID` | sends to another agent, but the reply is read from the bridge's own inbox |
| `mesh_peers` | `CRIER_HTTP_URL` | queries `GET /mesh/peers` on that server |
| `mesh_request` | `CRIER_MESH_URL`, `CRIER_AGENT_ID` | the bridge opens its own WebSocket connection only when both are set |

With no environment at all — the default in-process stdio mode — four tools
cannot work, and startup says exactly which:

```text
INFO MCP tool surface: some advertised tools cannot work in this mode — set the missing environment variables to enable them mode=in-process tools=13 available_tools=9 unavailable_tools="[get_messages ask_agent mesh_peers mesh_request]" missing_env="[CRIER_AGENT_ID CRIER_HTTP_URL CRIER_MESH_URL]"
```

### Try the Mesh

The mesh is the second primitive: direct agent-to-agent WebSocket connections
over `GET /mesh/connect/{agentID}`. Unlike the registry and the inboxes it needs
no signing setup — but it does have a frame contract, and a client that gets the
correlation wrong hangs instead of erroring.

**The zero-install path is this repo's own demo.** It builds this server and a Go
WebSocket client (gorilla/websocket, already in `go.mod`), starts its own relay on
a scratch port and runs the whole exchange below live — REGISTER, REQUEST,
RESPONSE, and the KEEPALIVE frames a client must ignore — asserting the result:

```bash
bash examples/ws-mesh-demo/run-demo.sh                          # scratch port 18961
DEMO_KEEPALIVE_WAIT=0 bash examples/ws-mesh-demo/run-demo.sh    # skip the ~30s keepalive wait
```

It exits 0 only when the RESPONSE's `request_id` equals the REQUEST's
`message_id`, its `status_code` is what the responder sent, and a KEEPALIVE frame
that arrived on the same socket was ignored instead of being taken for the reply.
Flags and the step-by-step transcript: `examples/ws-mesh-demo/README.md`.

**Manual alternative — any WebSocket client works.** Neither `websocat` nor
`wscat` ships with crier, so install one first (`cargo install websocat`, or Node
≥ 16 for `npx wscat -c <url>`). The agent ID in the path *is* the peer identity:
`ws://localhost:8767/mesh/connect/agent-1`. Two terminals, one frame each:

```bash
# Terminal A — connect as agent-1 and paste the REGISTER frame below. It is
# fire-and-forget: any RFC3339 timestamp works and the server never replies.
websocat ws://localhost:8767/mesh/connect/agent-1

# Terminal B — the peer is now visible (no token needed unless CR_AUTH_TOKEN is set):
curl -s localhost:8767/mesh/peers
# {"peers":[{"agent_id":"agent-1"}],"count":1}
```

A peer shows up as soon as the socket connects and stays listed while the
connection is open; close Terminal A and it disappears. Driving the *exchange* by
hand needs a loop that filters frames by `type`, so use
`bash examples/ws-mesh-demo/run-demo.sh` (or the Python worked example in
[`docs/mesh-protocol.md`](docs/mesh-protocol.md)) rather than pasting frames into
two `websocat` sessions.

#### The frames

Every frame is one JSON object in one WebSocket **text** frame with a trailing
newline (`json.Marshal` + `\n`, `internal/mesh/message.go:107`). These five are
the whole wire contract; every field name exists in `internal/mesh/message.go`
and only the marked values are yours to generate:

<!-- mesh-frames:start -->
```json
{"type":"REGISTER","version":1,"message_id":"f0e1d2c3b4a5968778695a4b","timestamp":"2026-09-18T09:15:00.123456789-05:00","agent_id":"agent-1","lease_id":"","lease_ttl_ms":3600000,"capabilities":{"version":"0.1.0","topics":[],"max_concurrent_sessions":10}}
{"type":"REQUEST","version":1,"message_id":"9d8f0a1b2c3d4e5f6a7b8c9d","timestamp":"2026-09-18T09:15:01.123456789-05:00","source":{"agent_id":"agent-1"},"target":{"agent_id":"agent-2"},"method":"GET","path":"/ping","body":{"hello":"world"},"trace_id":"f1e2d3c4b5a69788796a5b4c","timeout_ms":5000}
{"type":"KEEPALIVE","version":1,"message_id":"7c6b5a493827160514233241","timestamp":"2026-09-18T09:15:31.123456789-05:00","lease_id":"","agent_id":"agent-1"}
{"type":"RESPONSE","version":1,"message_id":"3f2a1b0c9d8e7f6a5b4c3d2e","timestamp":"2026-09-18T09:15:01.234567890-05:00","request_id":"9d8f0a1b2c3d4e5f6a7b8c9d","source":{"agent_id":"agent-2"},"status_code":200,"body":{"pong":true},"trace_id":"f1e2d3c4b5a69788796a5b4c"}
{"type":"ERROR","version":1,"message_id":"e57206b16d39e6e28a01e286","timestamp":"2026-09-18T09:15:01.345678901-05:00","request_id":"9d8f0a1b2c3d4e5f6a7b8c9d","error":{"code":"CONTROLLER_OFFLINE","message":"peer agent-2 not connected"},"trace_id":"f1e2d3c4b5a69788796a5b4c"}
```
<!-- mesh-frames:end -->

The ids above are literals so the correlation between the frames is visible; a
real client generates a fresh 24-hex `message_id` per frame (`openssl rand -hex
12`) and a `trace_id` for the REQUEST it can echo. What each frame's fields mean
and who fills them:

| Frame | Field | Filled by / meaning |
|-------|-------|---------------------|
| all | `type` | The message type: `REGISTER`, `REGISTER_ACK`, `KEEPALIVE`, `REQUEST`, `RESPONSE`, `ERROR`. Dispatch on this. |
| all | `version` | Protocol version, `1`. |
| all | `message_id` | Per-frame unique id (24 hex chars). Yours. **Not** the correlation field. |
| all | `timestamp` | RFC3339 with nanoseconds (Go `time.Time`). Yours; any parseable value works. |
| `REGISTER` | `agent_id` | The connecting agent — should match the id in the connect URL. |
| `REGISTER` | `lease_id` / `lease_ttl_ms` | Registry lease; empty / requested TTL on connect. |
| `REGISTER` | `capabilities` | `{version, topics[], max_concurrent_sessions}` — informational. |
| `KEEPALIVE` | `lease_id` / `agent_id` | Sender's lease (empty) and identity. No `request_id`. |
| `REQUEST` | `source` / `target` | `{"agent_id": "<id>"}` — you and the peer you address. `target.agent_id` is what the server routes on; a REQUEST without one is answered `INVALID_MESSAGE`. |
| `REQUEST` | `method` / `path` | Your application-level route (e.g. `GET` / `/ping`). The server does not interpret them. |
| `REQUEST` | `body` | Opaque JSON payload, omitted when empty. |
| `REQUEST` | `trace_id` | Yours; the responder echoes it in the reply. |
| `REQUEST` | `timeout_ms` | Yours; the server does not enforce it — your client does. |
| `RESPONSE` | `request_id` | **The `message_id` of the REQUEST being answered.** This is the correlation field. |
| `RESPONSE` | `source` | The answering peer. |
| `RESPONSE` | `status_code` | HTTP-style code from the responder. |
| `RESPONSE` | `body` | Whatever the responder wrote, relayed verbatim (see below). |
| `ERROR` | `request_id` | Same field, same rule: the failed REQUEST's `message_id`. |
| `ERROR` | `error` | `{code, message, retry_after_ms?}`; `CONTROLLER_OFFLINE` means the target peer is not connected, `INTERNAL` means the route table is full. |

#### Two rules a client must implement

**Correlate the reply on `request_id`, never on the frame's own `message_id` and
never on arrival order.** A responder MUST set `request_id` to the exact
`message_id` of the REQUEST it is answering (`internal/mesh/message.go:76`). The
server records `message_id → requester` when it forwards the REQUEST and looks
the reply up by `request_id` (`internal/mesh/peer.go` `forwardResponse`, `:402`);
a reply that does not echo it is dropped with no error and no log, and the caller
hangs until its own `timeout_ms`. `ERROR` frames correlate identically — they take
the same forwarding path, so the server's own `CONTROLLER_OFFLINE` answer also
carries your `message_id` in `request_id`. The reply's own `message_id` is a
fresh id of its own, as in the example above, so comparing *that* to your
REQUEST's id matches nothing: `request_id` is the only field that links a reply
to its request.

**`body` is relayed verbatim, and its JSON type is the responder's choice.** The
RESPONSE's `body` is held as raw bytes (`json.RawMessage`,
`internal/mesh/message.go:79`) and the frame is handed back unchanged, so an
object stays an object and a string stays a string — a responder that puts a JSON
*string* on the wire (Python's `json.dumps({...})` is the classic) sends a string,
and nothing on the server side rewrites it. `TestResponseBodyRelayedVerbatim` in
`internal/mesh` pins this over the relayed path.

**Read in a loop and dispatch on `type` — ignore `KEEPALIVE` frames while a reply
is outstanding.** The server sends a KEEPALIVE to every connected peer every
30 seconds (`KeepaliveInterval = 30 * time.Second`,
`internal/mesh/peer.go:42`, the value `cmd/server/main.go:149` runs with; the
accepted-connection loop is `internal/mesh/peer.go:230`), and each client sends
its own every 30 seconds on the same socket its reply arrives on. So the frame
after your REQUEST is not necessarily the answer: read frames one at a time,
dispatch on `type`, and ignore everything that is not the `RESPONSE` you are
waiting for (correlated as above) or an `ERROR`. A KEEPALIVE carries no
`request_id` at all, which is what makes the filter safe — but a client that
treats "the next frame" as the answer reads a KEEPALIVE as a RESPONSE and sees
`status_code: 0`. The demo prints exactly this: the KEEPALIVE frame the server
sent to the requester (`"agent_id":"crier"` — the server's own mesh identity) and
the RESPONSE it accepted instead.

For the deeper reference — every message type, the error codes, the silent-drop
rules, a verified Python round-trip — see
[`docs/mesh-protocol.md`](docs/mesh-protocol.md).

### Test

```bash
# Full test suite
make test

# Short (no integration tests)
make test-short

# Vet
make lint
```

### The demo harnesses never measure a server they did not start

The three runnable harnesses (`examples/federation-demo`,
`examples/hermes-gateway-demo`, `examples/ws-mesh-demo`) each start their own
crier server on a scratch port and then poll it, so a stale or foreign process
squatting that port would make a run report success for a binary it never built.
They are guarded against exactly that: `federation-demo` and
`hermes-gateway-demo` refuse to start while anything already listens on their
ports — naming the holder's pid, its command line and the `ss -tlnp | grep :<port>`
audit command — and, once a server answers `/health`, they assert that the process
holding the port is the pid they started; `ws-mesh-demo` refuses the same way and
additionally requires an empty peer list at startup. A started process that dies
before answering aborts the run with the tail of its log instead of being papered
over. Which build is answering can be confirmed at any time — `GET /version`
returns the running server's identity (version, commit, build time, dirty). The
three guards live in `scripts/lib/port-guard.sh`; `make port-guard-selftest`
exercises all of them on a port the selftest picks as free itself, and CI runs
that selftest on every push.

## Message guard (LLM)

Every inbound delivery is classified by an LLM message guard before it reaches the receiver (CR-FEAT-010..014). The guard sits at ONE choke point in `POST /agents/{id}/inbox` — after the deliver request is decoded, before BOTH downstream branches (webhook POST and inbox store) — so webhook (blocking/async/batch) and inbox deliveries get identical treatment. The verdict is computed exactly once per message; redelivery and batch flush never re-run the guard.

- **Prompt-injection screening** — the guard LLM inspects the payload for four attack classes: instruction injection, jailbreak, masquerade (obfuscated/hidden instructions), and structured-object attacks (payloads that could be interpreted as control data). A deterministic pattern pre-scan runs first over the RAW payload; its matches enrich the LLM prompt and are merged into the final verdict. JSON payloads are shown to the LLM as a schema-aware text projection, non-JSON as a text envelope (both bounded by `CR_GUARD_RENDER_MAX_BYTES`).
- **Structured verdicts** — the LLM answers with exactly one JSON object: `{decision, risk_level, reason, matched_patterns}`, where `decision` is `allow` | `block` | `sanitize` and `risk_level` is `low` | `medium` | `high`. A deterministic escalation table guarantees the LLM can never under-block below the policy's `block_risk` threshold (default `high`).
- **allow** — delivery proceeds as-is; the verdict still rides on the envelope / inbox entry and the outbound POST headers.
- **block** — uniform `403 GUARD_BLOCKED` with the full verdict; the message is never queued, never stored, never POSTed.
- **sanitize** — the guard LLM REWRITES the payload with a fixed neutralization prompt and the **rewritten payload is delivered in place of the original** (validated before delivery: valid JSON, pattern-clean, size-capped). The original rides in `crier.guard.quarantined_payload` (base64) for provenance — the original is never delivered on a sanitize verdict. If the rewrite is unavailable: fail-closed policies block, fail-open policies deliver a deterministic quarantine notice.
- **Fail-open by default** — an LLM error (provider down, timeout, missing API key) resolves to `allow` with `errored: true`; per-policy `fail_closed: true` flips this to the policy's error action (default `block`). The deterministic pre-scan is the exception and is never disabled by an LLM outage: if it matched a **high-confidence** pattern (explicit injection/jailbreak text, control keys, structural escapes), the fail-open outcome escalates to `block`/`high` with `reason: "guard_error: …; deterministic prematch block: <names>"` — the same evidence that blocks an over-cap payload without any LLM call. Its matches (empty, or low-confidence shape hits only) otherwise ride along in `matched_patterns` instead of being discarded.
- **Outcome on the wire** — the outbound webhook POST carries `X-Crier-Guard-*` headers: `X-Crier-Guard-Decision`, `X-Crier-Guard-Risk`, `X-Crier-Guard-Reason` (percent-encoded), `X-Crier-Guard-Patterns` (comma-joined), `X-Crier-Guard-Policy`, `X-Crier-Guard-Provider`, `X-Crier-Guard-Model`, and `X-Crier-Guard-Error: true` on the error path. A header whose verdict value is empty is omitted rather than sent blank (`Patterns`/`Policy`/`Provider`/`Model`), and `-Error` appears only when the guard errored. Blocked messages never POST. Inbox entries and deliver responses carry the same verdict as `crier.guard` metadata — on a deliver response the `guard` object is omitted only for a **clean** allow (no error, risk `low`, no `matched_patterns`); an allow that carries any risk marker is surfaced so it can't be mistaken for a clean pass.
- **Per-agent policy** — guard policy is configured at agent registration: `"guard":{"policies":[{"id":"default"}]}` (at least one policy required; invalid config → 400). A bare id resolves to the built-in named policy; inline policies (`{model, base_url, api_key_ref, fail_closed, action, providers, ...}`) are accepted, and `channel_match` globs (`session:*`, `thread:*`) scope a policy to specific channels. Agents without a guard config use the server-wide default (`CR_GUARD_DEFAULT_POLICY`, built-in `default` when unset).
- **Providers** — each policy declares a failover chain (`providers`, implicit `[deepseek]` when omitted). Presets: `deepseek` (default, model `deepseek-v4-flash`, thinking disabled — the preset hard-rejects `thinking_enabled`), `groq` (default `openai/gpt-oss-120b`), `nvidia` (default `google/gemma-4-31b-it`), or `custom` (requires `base_url` + `api_key_ref`). Model ids are the providers' own qualified ids, vendor prefix included — both free-limit lanes reject the bare id with 404 `model_not_found`. API keys are referenced as `env:VAR` and never stored inline (deepseek preset → `DEEPSEEK_API_KEY`). The router takes the first healthy provider: one retry (250ms backoff) on 429/5xx/network errors, a per-endpoint circuit breaker (`CR_GUARD_CIRCUIT_*`), a concurrency cap (`CR_GUARD_MAX_CONCURRENT`), and one per-message time budget across the whole chain (`CR_GUARD_TIMEOUT_MS`, default 10s). A degraded lane is visible in the server log: every provider the router skips (no key, open circuit, unknown preset, permanent rejection) or that exhausts its attempts — including a provider whose attempt the per-message budget cut off (`reason: per-message budget exhausted`) and one the chain never reached because the budget was already spent (`reason: per-message budget exhausted before attempt`) — emits one `guard router: provider skipped` / `guard router: provider failed` line naming provider, model and reason — plus the provider's HTTP status and error code on a failure — and a chain that lands on a fallback adds one `guard router: failover landed on a later provider` line (`from` → `to`). Keys and payloads are never logged.
- **Kanban output (opt-in)** — a policy can enable fire-and-forget kanban cards (`"kanban":{"enabled":true,"on":"block"|"all","assignee":...,"board_url":...}`): each scoped verdict posts a card (`[crier-guard] <agent> <decision>: <reason>`, full verdict metadata, sender, truncated payload excerpt) through the `hermes kanban create` CLI or an HTTP sink (`CR_GUARD_KANBAN_URL`). Writes are bounded (queue `CR_GUARD_KANBAN_QUEUE`, default 100; 10s per card) and never fail the delivery — full queue drops + counts, write failures log + count.
- Payloads above `CR_GUARD_MAX_PAYLOAD_BYTES` (default 65536) skip the LLM entirely — the deterministic pre-scan is the only verdict source. Only **high-confidence** prematch hits (explicit injection text, control keys, structural escapes) block; a hit from a low-confidence shape pattern (currently `b64_blob`, an 80+ char alphanumeric/base64-like run that ordinary machine-generated bodies trip) is reported as evidence but not blocked: delivery is allowed with `risk_level: medium` and `reason: payload_exceeds_guard_cap: low-confidence prematch only`. No hit at all → allow, risk medium, `reason: payload_exceeds_guard_cap`, `patterns: ["oversize"]`.

Full spec: [`specs/LLM-MESSAGE-GUARD.md`](specs/LLM-MESSAGE-GUARD.md) (CR-SPEC-002).

## Agent Ecosystem

The [`examples/agent-ecosystem/`](examples/agent-ecosystem/) directory is a runnable
reference stack that demonstrates Crier as the message bus between **popular agent systems**:
Pi Agent, OpenCode, Claude Code, Codex, Aider, Goose, Hermes, plus a plain webhook echo sink
and a full battery of tests. Every agent self-registers with Crier at boot and answers
through the bus (blocking webhook round-trips, async fire-and-forget, the LLM message guard
with real DeepSeek verdicts when `DEEPSEEK_API_KEY` is set).

```bash
cd examples/agent-ecosystem
docker compose up -d --build
docker compose run --rm --build battery
```

Setup, per-harness walkthroughs, battery guide, bunker deployment, CI ops and
troubleshooting: [`docs/AGENT-ECOSYSTEM.md`](docs/AGENT-ECOSYSTEM.md) (CR-FEAT-022). The
normative design authority is [`specs/AGENT-ECOSYSTEM.md`](specs/AGENT-ECOSYSTEM.md)
(CR-SPEC-003).

## Configuration

All configuration is via environment variables (defaults shown):

| Variable | Default | Description |
|----------|---------|-------------|
| `CRIER_PORT` | `8767` | Server listen port |
| `CR_PIDFILE` | _(unset — no pidfile)_ | Pidfile path. When set (or `-pidfile <path>` is passed, or `make run`'s default `.crier.pid` is used), the server writes `{pid, port, binary}` to this file **after** the port is bound and removes it on graceful shutdown; `-stop`/`make stop` read it to stop that exact process safely (ownership-checked against `/proc/<pid>/exe`; a mismatch is refused without signalling). See [Stop / restart](#stop--restart). |
| `CR_DATABASE_URL` | _(unset — in-memory backend)_ | PostgreSQL connection (optional). When set, the registry and inboxes use the durable PostgreSQL backend (migrations applied automatically on start). Precedence: `CR_DATABASE_URL` → `DATABASE_URL` → `CRIER_DATABASE_URL`. Example: `postgres://crier:crier@localhost:5437/crier?sslmode=disable`. See [Durable backend (PostgreSQL)](#durable-backend-postgresql) for the runnable compose path. |
| `CR_AUTH_TOKEN` | _(unset — auth disabled)_ | Bearer token for API authentication. When set, all requests **except the five exempt paths** (`/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs` — see `internal/middleware/auth.go`) require `Authorization: Bearer <token>`; unset = no auth (local dev). |
| `CR_REQUIRE_AGENT_SIG` | `true` | Enforce per-agent ed25519 request signing on agent-scoped endpoints (inbox retrieve/ack/stats, DELETE /agents/{id}, and PATCH /agents/{id}). Set `false` only for trusted single-user dev setups. |
| `CR_LOG_LEVEL` | `info` | Log level. One of `debug`, `info`, `warn`, `error`. |
| `CR_LOG_FORMAT` | `text` | Log format. One of `text`, `json`. |
| `CR_RATE_LIMIT_PER_MINUTE` | `100` | Per-agent publish rate limit (events/minute), keyed on the `X-Agent-ID` header. `0` disables rate limiting and the identity requirement. |
| `CR_WS_ALLOWED_ORIGINS` | _(unset — all origins allowed)_ | Comma-separated list of allowed WebSocket `Origin` headers (`scheme://host:port`). `*` allows all origins. |
| `CR_FED_LINKS` | _(unset — federation disabled)_ | Comma-separated base URLs of linked relays (relay-to-relay federation, CR-FEAT-006). When set, deliveries to agents unknown on this relay are forwarded to each linked relay in order (first non-404, non-retryable answer wins and is relayed back verbatim — a blocking webhook reply returns to the original sender), and `GET /fed/peers` lists the linked relays with their agents. A transient link outage (unreachable link, or a retryable 5xx/408/429) is held and retried instead of answering `404` (`202 Accepted {"status":"held",…}`) — but only when the request names a `sender`; a sender-less request is never held and answers `502 {"error":"FEDERATION_FAILED",…}` immediately, because the terminal notification is addressed to the sender and could never reach an inbox (DF-CRIER-129). A definitive all-links-404 still answers `404` immediately. Example: `http://localhost:18772`. |
| `CR_FED_NAME` | _(unset — `localhost:<port>`)_ | Optional display name for this relay in the `GET /fed/peers` listing. |
| `CR_FED_TOKEN` | _(unset — no link auth)_ | Shared secret for federation link authentication. When set, every request this relay sends to a linked relay (deliver forwards and `/agents` discovery) carries `Authorization: Bearer <token>`, so an auth-enabled destination relay accepts the forward instead of returning 401. The linked relay must share the value: set the source relay's `CR_FED_TOKEN` equal to the destination relay's `CR_AUTH_TOKEN`. The secret is never logged, echoed, or included in `GET /fed/peers` output. |
| `CR_FED_MAX_HOLD_S` | `300` | How long a federated delivery is held at this relay and retried when every link fails transiently, before the sender gets an explicit terminal outcome (DF-CRIER-7, `specs/WEBHOOK-DELIVERY.md` §8.1). Only the transient case is held, and only when the request names a `sender` — an unreportable delivery is answered synchronously with `502 FEDERATION_FAILED` instead of being held (DF-CRIER-129). |
| `CR_FED_QUEUE_FILE` | _(unset — memory queue)_ | Path of the durable hold-queue document. When set, held deliveries survive a source-relay restart (one atomically rewritten JSON document, `0600`, recovered from `<path>.bak` after a crash). Unset keeps the queue process-lifetime only — held deliveries are lost on restart, the same contract the in-memory registry backend documents for inboxes. |
| `CR_DATABASE_MAX_CONNS` | `4` | Maximum PostgreSQL pool connections. |
| `CR_DATABASE_MIN_CONNS` | `0` | Minimum PostgreSQL pool connections kept open (must be ≤ `CR_DATABASE_MAX_CONNS`). |
| `CR_DATABASE_MAX_CONN_LIFETIME` | `30m` | Maximum lifetime of a pooled connection (Go duration, e.g. `30m`, `1h`). |
| `CR_DATABASE_MAX_CONN_IDLE_TIME` | `5m` | Maximum idle time of a pooled connection (Go duration). |
| `CR_DATABASE_CONNECT_TIMEOUT` | `10s` | PostgreSQL connect timeout (Go duration). |
| `CR_GUARD_ENABLED` | `true` | LLM message-guard master switch. When on, every inbound delivery is classified before webhook POST / inbox store. |
| `CR_GUARD_TIMEOUT_MS` | `10000` | Per-message guard budget in milliseconds — covers the whole provider chain, retries included. |
| `CR_GUARD_MAX_CONCURRENT` | `8` | Maximum concurrent guard LLM calls. |
| `CR_GUARD_CIRCUIT_THRESHOLD` | `10` | Consecutive failures (per base URL + model) that open the provider circuit breaker. |
| `CR_GUARD_CIRCUIT_COOLDOWN_S` | `300` | How long a tripped circuit stays open (seconds); the first call after expiry is the probe. |
| `CR_GUARD_MAX_PAYLOAD_BYTES` | `65536` | Payloads larger than this skip the LLM entirely (risk medium). Only high-confidence prematch hits block; low-confidence shape hits (`b64_blob`) allow with `reason: payload_exceeds_guard_cap: low-confidence prematch only`. |
| `CR_GUARD_RENDER_MAX_BYTES` | `32768` | Byte cap on the payload projection fed to the LLM. |
| `CR_GUARD_DEEPSEEK_BASE_URL` | `https://api.deepseek.com/v1` | Base URL override for the deepseek provider preset. |
| `CR_GUARD_MODEL` | `deepseek-v4-flash` | Default model override for the deepseek provider preset. |
| `CR_GUARD_PATTERNS_EXTRA` | _(unset)_ | JSON array of extra prematch patterns (`[{"name","pattern","class"}]`) — appended, or replacing built-ins with the same name. Invalid JSON/regex fails fast at startup. |
| `CR_GUARD_DEFAULT_POLICY` | _(unset — built-in `default`)_ | JSON `Policy` used as the server-wide default when the target agent registers no guard config. Must parse + validate at startup (fail-fast). |
| `CR_GUARD_KANBAN_QUEUE` | `100` | Kanban worker queue capacity (fire-and-forget cards, opt-in per policy `kanban`). |
| `CR_GUARD_KANBAN_URL` | _(unset — Hermes kanban CLI)_ | HTTP kanban sink base URL (http/https, CR-FEAT-009). When set, guard cards are POSTed here as JSON (fire-and-forget); unset = cards go through the `hermes kanban create` CLI writer. |
| `CR_WEBHOOK_SECRET` | _(unset)_ | HMAC outbound signing. |
| `CR_WEBHOOK_TIMEOUT_S` | `30` | Outbound webhook timeout, seconds. |
| `CR_WEBHOOK_MAX_RETRIES` | `5` | Outbound retry count for queued async/batch webhook deliveries. When a delivery exhausts them, the sender gets exactly one durable `WEBHOOK_FAILED` in its own inbox — see [Webhook delivery](#5-webhook-delivery-bypasses-the-inbox). |
| `CR_WEBHOOK_REDELIVER_S` | `30` | Redelivery interval, seconds — one queued delivery attempt per tick. |
| `CR_WEBHOOK_PROBE_S` | `60` | Dead-target probe interval, seconds. |
| `CR_WEBHOOK_CIRCUIT_THRESHOLD` | `10` | Consecutive failures that open the circuit. |
| `CR_WEBHOOK_BATCH_MAX` | `10` | Batch flush size. |
| `CR_WEBHOOK_BATCH_FLUSH_S` | `5` | Batch flush interval, seconds. |
| `DEEPSEEK_API_KEY` | _(unset)_ | API key for the deepseek provider preset (referenced as `env:DEEPSEEK_API_KEY`). Without it, guard LLM calls fail and the guard fails open. |
| `CR_ENABLE_PPROF` | `false` | Opt-in: register `GET /debug/pprof/` (plus `cmdline`, `profile`, `symbol`, `trace`, `heap`, `goroutine`, `block`, `mutex`, `threadcreate`) for live Go profiling. Default off — unset means the path is not registered and answers `404`. Not auth-exempt: with `CR_AUTH_TOKEN` set it requires the Bearer header like any other authenticated route. See [Observability](#observability-metrics--profiling). |
| `CR_ENABLE_METRICS` | `false` | Opt-in: register `GET /metrics` serving the Prometheus text exposition format (v0.0.4) — deliveries, webhook outcomes, guard decisions, federation hold depth, relay events, WS subscribers, HTTP requests. Default off — unset means the path is not registered and answers `404`. Not auth-exempt: with `CR_AUTH_TOKEN` set it requires the Bearer header like any other authenticated route. See [Observability](#observability-metrics--profiling). |

### Durable backend (PostgreSQL)

The `postgres` service in `docker-compose.yml` (image `postgres:16-alpine`, credentials `crier`/`crier`, database `crier`) publishes the container's in-container port 5432 on host port **5437** by default:

```bash
docker compose up -d postgres
CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' ./bin/crier
```

Migrations apply automatically on startup; agents and undelivered messages then survive restarts. Both the host port and the project (and therefore the container name) are env-overridable — `CRIER_PG_HOST_PORT` picks the host port, `COMPOSE_PROJECT_NAME` scopes the container away from a name collision on a shared host.

When host 5437 is already taken (check with `ss -tlnp | grep :5437`), override it and use the same port in the URL:

```bash
CRIER_PG_HOST_PORT=5493 docker compose up -d postgres
CR_DATABASE_URL='postgres://crier:crier@localhost:5493/crier?sslmode=disable' ./bin/crier
```

When a stale container from an earlier project squats the expected name, rescope the compose project so its container is named after it instead:

```bash
COMPOSE_PROJECT_NAME=crier-lab docker compose up -d postgres
# container <project>-postgres-1; the URL still uses localhost:5437 (or your CRIER_PG_HOST_PORT override)
```

Stop and remove with `docker compose down`; add `-v` to drop the `pgdata` volume as well.

## API

The full API is documented in [`docs/openapi.yaml`](docs/openapi.yaml) — an OpenAPI 3.1 spec with **13 paths** and **17 operations** (a path carries one entry per HTTP method, so the two counts differ) across 7 operation groups. Every count in this README names its unit; measure them yourself:

```bash
grep -c '^  /' docs/openapi.yaml                                    # 13 paths
grep -cE '^    (get|post|put|patch|delete):' docs/openapi.yaml      # 17 operations
grep -oE 'HandleFunc\("[^"]+"' cmd/server/main.go | sort -u | wc -l # 16 router paths
```

The router registers **16 paths**: those 13 plus the three spec-hosting routes (`/openapi.json`, `/openapi.yaml`, `/docs`) that are not part of the API document.

| Group | Endpoints | Description |
|-------|-----------|-------------|
| **Health** | `GET /health` | Service health check |
| **Version** | `GET /version` | Build identity of the running server (version, commit, build time, dirty) — public like `/health` |
| **Relay** | `POST /relay/publish`, `GET /relay/subscribe/{topic}`, `GET /relay/topics` | Pub/sub |
| **Mesh** | `GET /mesh/connect/{agentID}`, `GET /mesh/peers` | P2P connections |
| **Federation** | `GET /fed/peers` | Relay-to-relay federation peer listing (CR-FEAT-006) |
| **Registry** | `POST /agents`, `GET /agents` (capability filter), `GET /agents/{id}`, `PATCH /agents/{id}`, `DELETE /agents/{id}` | Agent identity + self-configuration |
| **Inbox** | `POST /agents/{id}/inbox`, `GET /agents/{id}/inbox`, `POST /agents/{id}/inbox/ack`, `GET /agents/{id}/inbox/stats` | Message delivery |

## Observability (metrics & profiling)

Two opt-in live-inspection surfaces (`DF-CRIER-142`); both are **off by default** (set the env var to enable, unset = the path answers `404`):

- `GET /metrics` (`CR_ENABLE_METRICS=true`) — the Prometheus text exposition format (v0.0.4): `deliveries_total`, `webhook_deliveries_total{outcome}`, `guard_decisions_total{decision}`, `federation_held_current`, `relay_events_total`, `ws_subscribers` (relay topic subscribers + connected mesh peers, summed), and `http_requests_total{code}`.
- `GET /debug/pprof/` (`CR_ENABLE_PPROF=true`) — the standard Go profiling index plus the named profiles (`heap`, `goroutine`, `block`, `mutex`, `threadcreate`, `profile`, `symbol`, `trace`, `cmdline`).

**Neither path is auth-exempt**: they are served like any other authenticated route — with `CR_AUTH_TOKEN` set they require `Authorization: Bearer <token>`; with auth disabled they are open. The exempt-path list in `internal/middleware/auth.go` is unchanged. Exposure note: the pprof surface reveals runtime internals (stacks, heap) — enable it only on trusted networks.

## Documentation

| File | Description |
|------|-------------|
| [`docs/architecture.md`](docs/architecture.md) | Architecture overview and design decisions |
| [`docs/specs.md`](docs/specs.md) | Component specifications and acceptance criteria |
| [`docs/mesh-protocol.md`](docs/mesh-protocol.md) | Mesh wire protocol — framing, message types, correlation contract, worked example |
| [`docs/openapi.yaml`](docs/openapi.yaml) | OpenAPI 3.1 API specification |
| [`docs/integration-guide.md`](docs/integration-guide.md) | End-to-end integration guide — auth modes, signing, inbox lifecycle, mesh, Postgres |
| [`docs/AGENT-ECOSYSTEM.md`](docs/AGENT-ECOSYSTEM.md) | Agent-ecosystem reference stack — setup, per-harness walkthroughs, battery guide, bunker deployment, CI ops, troubleshooting (CR-FEAT-022) |
| [`specs/LLM-MESSAGE-GUARD.md`](specs/LLM-MESSAGE-GUARD.md) | Message guard spec (CR-SPEC-002) — verdict contract, policies, providers, kanban output |
| [`examples/demo.sh`](examples/demo.sh) | Runnable end-to-end demo (register → deliver → signed retrieve → ack) |

## Project Status

All core primitives are implemented and tested:

- **Relay** — Thread-safe in-memory pub/sub, 87.5% coverage, 7/7 GitReins PASS
- **Mesh** — P2P WebSocket connections ported from Hivemind, 8/8 GitReins PASS
- **Registry + Inboxes** — Net-new, 78.3% coverage, 8/8 GitReins PASS
- **Persistence** — PostgreSQL backend for registry + inboxes via `CR_DATABASE_URL`; verified live that agents (webhook + guard config included), and undelivered messages survive a server restart
- **Message guard** — LLM prompt-injection guard at the delivery choke point (CR-FEAT-010..014): structured verdicts, fail-open with per-policy fail-closed, X-Crier-Guard-* headers, provider failover, opt-in kanban cards
- **API** — 16 router paths registered in `cmd/server/main.go` (`HandleFunc`), documented as 13 paths / 17 operations in `docs/openapi.yaml`, wired with middleware and graceful shutdown
- **CI** — GitHub Actions, matrix build Go 1.26.6

Coverage numbers above are measured fresh per change (`go test -short -count=1 -cover ./internal/<pkg>`); the ≥70% gate lives in `make coverage-check`.

### Roadmap

- **CI-003b** ✅ — PostgreSQL persistence for registry and inboxes (implemented, `CR_DATABASE_URL`)
- **CI-007** ✅ — MCP server exposing registry and inbox tools (implemented, `cmd/crier-mcp`)
- **Capability-based routing** — route messages by agent capability cards

## License

MIT — see [LICENSE](LICENSE).
