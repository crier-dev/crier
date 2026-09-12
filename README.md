# Crier — Agent-to-Agent Message Bus

[![Go Version](https://img.shields.io/badge/Go-1.26.6%2B-00ADD8?logo=go)](https://go.dev)

Communication backbone for the autonomous agent economy. Extracted and generalized from Hivemind.

Crier provides four primitives for agent communication:

## Architecture

### 1. Relay (Pub/Sub)

Central message relay. Agents publish events to topics; subscribers receive them over WebSocket.

- HTTP + WebSocket transport
- Topic-based routing with subscriber counts
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
> is NOT live-connection health. The registry has no heartbeat source today
> (mesh KEEPALIVE frames are sent but not processed server-side), so an agent
> whose process crashed still reports `"online"` with a frozen `last_seen`.
> For live-connection truth, use the mesh: `GET /mesh/peers` lists agents with
> an active WebSocket connection (see [Try the Mesh](#try-the-mesh)).

### 4. Inboxes

Durable per-agent FIFO queues with lease-based delivery. Durability is backend-dependent: with `CR_DATABASE_URL` set (PostgreSQL backend) agents and undelivered messages survive server restarts; without it the in-memory backend is used (process-lifetime only).

- Lease prevents double-delivery: messages are leased for N seconds on retrieval
- ACK confirms delivery; un-ACKed messages return to queue after lease expiry
- TTL expiry auto-purges stale messages (default 24h)
- Concurrent retrievers get disjoint message sets

## Quick Start

### Prerequisites

- Go 1.26.6 or later
- OpenSSL 3.x or later with `xxd` on PATH — the quickstart signing helper uses `openssl pkeyutl -sign -rawin`, an OpenSSL 3+ flag. On older OpenSSL the helper fails loudly instead of signing (see below)

### Build

```bash
make build
```

Or build directly:

```bash
go build -o bin/crier ./cmd/server
```

### Run

```bash
# Default port :8767
make run
```

> **The LLM message guard is ON by default.** Every inbound delivery is
> classified by a guard LLM (default model `deepseek-v4-flash`, 10s
> per-message budget — `CR_GUARD_TIMEOUT_MS`) before it is webhook-POSTed
> or inbox-stored. For local dev without an API key, set
> `CR_GUARD_ENABLED=false`; to exercise the guard, set `DEEPSEEK_API_KEY`.
> Without a key the guard call fails and the guard fails OPEN — the
> delivery proceeds, marked `X-Crier-Guard-Error: true`. See
> [Message guard (LLM)](#message-guard-llm).

### Try it

A minimal register → deliver → retrieve round-trip with the default signed configuration. If you started the server with `CR_AUTH_TOKEN` set (auth enabled), every request except `/health` needs the Bearer header shown below; if `CR_AUTH_TOKEN` is unset, auth is disabled and the header can be dropped:

```bash
AUTH=(-H "Authorization: Bearer ${CR_AUTH_TOKEN:-}")

# 0. One-time setup: generate an ed25519 keypair for agent-1 (needs openssl 3.x + xxd)
openssl genpkey -algorithm ED25519 -out /tmp/crier-agent.key >/dev/null 2>&1
PUBKEY_HEX=$(openssl pkey -in /tmp/crier-agent.key -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)
# sig helper: hex(ed25519_sign("METHOD\nPATH\nTS", key)) — same wire format as examples/demo.sh.
# Requires OpenSSL >= 3 for pkeyutl -sign -rawin; on older OpenSSL it errors loudly
# instead of producing an empty (silently-401-rejected) signature.
sig() { if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then echo "ERROR: this signing helper requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)" >&2; return 1; fi; printf '%s\n%s\n%s' "$1" "$2" "$3" > /tmp/crier-payload.txt; openssl pkeyutl -sign -rawin -inkey /tmp/crier-agent.key -in /tmp/crier-payload.txt 2>/dev/null | xxd -p -c 128; }

# 1. Register an agent (public_key = hex-encoded ed25519 public key)
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"id\":\"agent-1\",\"public_key\":\"${PUBKEY_HEX}\",\"capabilities\":[\"demo\"]}"
# 201

# 2. Deliver a message to its inbox
curl -s -X POST localhost:8767/agents/agent-1/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world"}}'
# 201 {"id":"..."}

# 3. Retrieve — agent-scoped endpoints require per-agent request signatures by
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
# 200 {"messages":[{"id":"...","payload":"eyJoZWxsbyI6IndvcmxkIn0=","lease_id":"..."}],"lease_id":"..."}
# Note: message payloads are base64-encoded on the wire ([],byte form)

# 4. Ack the message — message_ids is REQUIRED (an ack without it is rejected
#    with 400: it would otherwise be a silent no-op and the message would be
#    redelivered after lease expiry). Sign "POST\n/agents/agent-1/inbox/ack\n<ts>".
TS=$(date +%s)
curl -s -X POST localhost:8767/agents/agent-1/inbox/ack "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H 'X-Agent-ID: agent-1' -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: $(sig POST /agents/agent-1/inbox/ack "$TS")" \
  -d '{"lease_id":"<lease_id from retrieve>","message_ids":["<id from retrieve>"]}'
# 204 — message permanently removed (never redelivered after lease expiry)

# Dev shortcut: disable signing for trusted single-user setups
CR_REQUIRE_AGENT_SIG=false make run
curl -s localhost:8767/agents/agent-1/inbox
# 200 — no signature headers required

# 5. Delete the agent — DELETE /agents/{id} requires the same per-agent
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
(`CR_REQUIRE_AGENT_SIG=true`), remote mode needs the agent's **ed25519
private key** — the same PKCS#8 PEM file the quickstart above generates with
`openssl genpkey -algorithm ED25519`. Generate (or reuse) a key, register its
public half for your agent id, then point crier-mcp at the file:

```bash
# 0. One-time: generate a key and register its public half for the bridge agent
openssl genpkey -algorithm ED25519 -out ~/.config/crier/mcp-agent.key
PUBKEY_HEX=$(openssl pkey -in ~/.config/crier/mcp-agent.key -pubout -outform DER | tail -c 32 | xxd -p -c 64)
curl -s -X POST localhost:8767/agents -H 'Content-Type: application/json' \
  -d "{\"id\":\"mcp-agent\",\"public_key\":\"${PUBKEY_HEX}\",\"capabilities\":[\"mcp\"]}"

# 1. Run the MCP bridge against the remote server (all four vars are read at startup)
export CRIER_HTTP_URL=http://localhost:8767
export CRIER_AGENT_ID=mcp-agent
export CRIER_AGENT_PRIVATE_KEY_FILE=$HOME/.config/crier/mcp-agent.key
# Optional shared bearer token, only when the server runs with CR_AUTH_TOKEN:
# export CRIER_AUTH_TOKEN=...
make build-mcp && ./bin/crier-mcp
```

- `CRIER_AGENT_PRIVATE_KEY_FILE` must contain a PKCS#8 PEM ed25519 private
  key; anything else (unreadable file, wrong format, RSA/EC key) is an
  explicit startup error. Key material is never logged or echoed.
- Each request carries a fresh `X-Agent-Ts` / `X-Agent-Sig` signing
  `METHOD\n<path>\n<unix-seconds>` — the query string is excluded.
- The key is **optional only when the server disables per-agent signatures**
  (`CR_REQUIRE_AGENT_SIG=false`): with the variable unset, crier-mcp sends
  the plain `X-Agent-ID` identity, which signed servers reject with 401.

### Try the Mesh

The mesh is the second primitive: direct agent-to-agent WebSocket connections.
Unlike the registry/inboxes it needs no signing setup — just a WebSocket client
([websocat](https://github.com/vi/websocat), or `npx wscat -c <url>`):

```bash
# Terminal A — connect as agent-1, then send one REGISTER frame (fire-and-forget:
# any RFC3339 timestamp works, the server never replies)
websocat ws://localhost:8767/mesh/connect/agent-1
{"type":"REGISTER","version":1,"message_id":"0123456789abcdef01234567","timestamp":"2026-08-10T18:00:00.123456789-05:00","agent_id":"agent-1","lease_id":"","lease_ttl_ms":3600000,"capabilities":{"version":"0.1.0","topics":[],"max_concurrent_sessions":10}}

# Terminal B — the peer is now visible:
curl -s localhost:8767/mesh/peers
# {"peers":[{"agent_id":"agent-1"}],"count":1}
```

A peer shows up as soon as the socket connects and stays listed while the
connection is open (keepalive frames are exchanged every 30s); close Terminal A
and it disappears. If you started the server with `CR_AUTH_TOKEN` set, add the
Bearer header to the `curl` as in the section above. For the full wire protocol —
REQUEST/RESPONSE correlation, error frames, a verified two-agent round-trip —
see [`docs/mesh-protocol.md`](docs/mesh-protocol.md).

### Test

```bash
# Full test suite
make test

# Short (no integration tests)
make test-short

# Vet
make lint
```

## Message guard (LLM)

Every inbound delivery is classified by an LLM message guard before it reaches the receiver (CR-FEAT-010..014). The guard sits at ONE choke point in `POST /agents/{id}/inbox` — after the deliver request is decoded, before BOTH downstream branches (webhook POST and inbox store) — so webhook (blocking/async/batch) and inbox deliveries get identical treatment. The verdict is computed exactly once per message; redelivery and batch flush never re-run the guard.

- **Prompt-injection screening** — the guard LLM inspects the payload for four attack classes: instruction injection, jailbreak, masquerade (obfuscated/hidden instructions), and structured-object attacks (payloads that could be interpreted as control data). A deterministic pattern pre-scan runs first over the RAW payload; its matches enrich the LLM prompt and are merged into the final verdict. JSON payloads are shown to the LLM as a schema-aware text projection, non-JSON as a text envelope (both bounded by `CR_GUARD_RENDER_MAX_BYTES`).
- **Structured verdicts** — the LLM answers with exactly one JSON object: `{decision, risk_level, reason, matched_patterns}`, where `decision` is `allow` | `block` | `sanitize` and `risk_level` is `low` | `medium` | `high`. A deterministic escalation table guarantees the LLM can never under-block below the policy's `block_risk` threshold (default `high`).
- **allow** — delivery proceeds as-is; the verdict still rides on the envelope / inbox entry and the outbound POST headers.
- **block** — uniform `403 GUARD_BLOCKED` with the full verdict; the message is never queued, never stored, never POSTed.
- **sanitize** — the guard LLM REWRITES the payload with a fixed neutralization prompt and the **rewritten payload is delivered in place of the original** (validated before delivery: valid JSON, pattern-clean, size-capped). The original rides in `crier.guard.quarantined_payload` (base64) for provenance — the original is never delivered on a sanitize verdict. If the rewrite is unavailable: fail-closed policies block, fail-open policies deliver a deterministic quarantine notice.
- **Fail-open by default** — an LLM error (provider down, timeout, missing API key) resolves to `allow` with `errored: true`; per-policy `fail_closed: true` flips this to the policy's error action (default `block`).
- **Outcome on the wire** — the outbound webhook POST carries `X-Crier-Guard-*` headers: `X-Crier-Guard-Decision`, `X-Crier-Guard-Risk`, `X-Crier-Guard-Reason` (percent-encoded), `X-Crier-Guard-Patterns` (comma-joined), `X-Crier-Guard-Policy`, `X-Crier-Guard-Provider`, `X-Crier-Guard-Model`, and `X-Crier-Guard-Error: true` on the error path. Blocked messages never POST. Inbox entries and deliver responses carry the same verdict as `crier.guard` metadata.
- **Per-agent policy** — guard policy is configured at agent registration: `"guard":{"policies":[{"id":"default"}]}` (at least one policy required; invalid config → 400). A bare id resolves to the built-in named policy; inline policies (`{model, base_url, api_key_ref, fail_closed, action, providers, ...}`) are accepted, and `channel_match` globs (`session:*`, `thread:*`) scope a policy to specific channels. Agents without a guard config use the server-wide default (`CR_GUARD_DEFAULT_POLICY`, built-in `default` when unset).
- **Providers** — each policy declares a failover chain (`providers`, implicit `[deepseek]` when omitted). Presets: `deepseek` (default, model `deepseek-v4-flash`, thinking disabled — the preset hard-rejects `thinking_enabled`), `groq` (default `gpt-oss-120b`), `nvidia` (default `gemma-4-31b`), or `custom` (requires `base_url` + `api_key_ref`). API keys are referenced as `env:VAR` and never stored inline (deepseek preset → `DEEPSEEK_API_KEY`). The router takes the first healthy provider: one retry (250ms backoff) on 429/5xx/network errors, a per-endpoint circuit breaker (`CR_GUARD_CIRCUIT_*`), a concurrency cap (`CR_GUARD_MAX_CONCURRENT`), and one per-message time budget across the whole chain (`CR_GUARD_TIMEOUT_MS`, default 10s).
- **Kanban output (opt-in)** — a policy can enable fire-and-forget kanban cards (`"kanban":{"enabled":true,"on":"block"|"all","assignee":...,"board_url":...}`): each scoped verdict posts a card (`[crier-guard] <agent> <decision>: <reason>`, full verdict metadata, sender, truncated payload excerpt) through the `hermes kanban create` CLI or an HTTP sink (`CR_GUARD_KANBAN_URL`). Writes are bounded (queue `CR_GUARD_KANBAN_QUEUE`, default 100; 10s per card) and never fail the delivery — full queue drops + counts, write failures log + count.
- Payloads above `CR_GUARD_MAX_PAYLOAD_BYTES` (default 65536) skip the LLM entirely: a prematch hit blocks, otherwise the delivery is allowed (risk medium).

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
| `CR_DATABASE_URL` | _(unset — in-memory backend)_ | PostgreSQL connection (optional). When set, the registry and inboxes use the durable PostgreSQL backend (migrations applied automatically on start). Precedence: `CR_DATABASE_URL` → `DATABASE_URL` → `CRIER_DATABASE_URL`. Example: `postgres://crier:crier@localhost:5432/crier?sslmode=disable` |
| `CR_AUTH_TOKEN` | _(unset — auth disabled)_ | Bearer token for API authentication. When set, all requests except `/health` require `Authorization: Bearer <token>`; unset = no auth (local dev). |
| `CR_REQUIRE_AGENT_SIG` | `true` | Enforce per-agent ed25519 request signing on agent-scoped endpoints (inbox retrieve/ack/stats, DELETE /agents/{id}, and PATCH /agents/{id}). Set `false` only for trusted single-user dev setups. |
| `CR_LOG_LEVEL` | `info` | Log level. One of `debug`, `info`, `warn`, `error`. |
| `CR_LOG_FORMAT` | `text` | Log format. One of `text`, `json`. |
| `CR_RATE_LIMIT_PER_MINUTE` | `100` | Per-agent publish rate limit (events/minute), keyed on the `X-Agent-ID` header. `0` disables rate limiting and the identity requirement. |
| `CR_WS_ALLOWED_ORIGINS` | _(unset — all origins allowed)_ | Comma-separated list of allowed WebSocket `Origin` headers (`scheme://host:port`). `*` allows all origins. |
| `CR_FED_LINKS` | _(unset — federation disabled)_ | Comma-separated base URLs of linked relays (relay-to-relay federation, CR-FEAT-006). When set, deliveries to agents unknown on this relay are forwarded to each linked relay in order (first non-404, non-retryable answer wins and is relayed back verbatim — a blocking webhook reply returns to the original sender), and `GET /fed/peers` lists the linked relays with their agents. A transient link outage (unreachable link, or a retryable 5xx/408/429) is held and retried instead of answering `404` (`202 Accepted {"status":"held",…}`); a definitive all-links-404 still answers `404` immediately. Example: `http://localhost:18772`. |
| `CR_FED_NAME` | _(unset — `localhost:<port>`)_ | Optional display name for this relay in the `GET /fed/peers` listing. |
| `CR_FED_TOKEN` | _(unset — no link auth)_ | Shared secret for federation link authentication. When set, every request this relay sends to a linked relay (deliver forwards and `/agents` discovery) carries `Authorization: Bearer <token>`, so an auth-enabled destination relay accepts the forward instead of returning 401. The linked relay must share the value: set the source relay's `CR_FED_TOKEN` equal to the destination relay's `CR_AUTH_TOKEN`. The secret is never logged, echoed, or included in `GET /fed/peers` output. |
| `CR_FED_MAX_HOLD_S` | `300` | How long a federated delivery is held at this relay and retried when every link fails transiently, before the sender gets an explicit terminal outcome (DF-CRIER-7, `specs/WEBHOOK-DELIVERY.md` §8.1). Only the transient case is held. |
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
| `CR_GUARD_MAX_PAYLOAD_BYTES` | `65536` | Payloads larger than this skip the LLM entirely (prematch hit → block, else allow, risk medium). |
| `CR_GUARD_RENDER_MAX_BYTES` | `32768` | Byte cap on the payload projection fed to the LLM. |
| `CR_GUARD_DEEPSEEK_BASE_URL` | `https://api.deepseek.com/v1` | Base URL override for the deepseek provider preset. |
| `CR_GUARD_MODEL` | `deepseek-v4-flash` | Default model override for the deepseek provider preset. |
| `CR_GUARD_PATTERNS_EXTRA` | _(unset)_ | JSON array of extra prematch patterns (`[{"name","pattern","class"}]`) — appended, or replacing built-ins with the same name. Invalid JSON/regex fails fast at startup. |
| `CR_GUARD_DEFAULT_POLICY` | _(unset — built-in `default`)_ | JSON `Policy` used as the server-wide default when the target agent registers no guard config. Must parse + validate at startup (fail-fast). |
| `CR_GUARD_KANBAN_QUEUE` | `100` | Kanban worker queue capacity (fire-and-forget cards, opt-in per policy `kanban`). |
| `CR_GUARD_KANBAN_URL` | _(unset — Hermes kanban CLI)_ | HTTP kanban sink base URL (http/https, CR-FEAT-009). When set, guard cards are POSTed here as JSON (fire-and-forget); unset = cards go through the `hermes kanban create` CLI writer. |
| `CR_WEBHOOK_SECRET` | _(unset)_ | HMAC outbound signing. |
| `CR_WEBHOOK_TIMEOUT_S` | `30` | Outbound webhook timeout, seconds. |
| `CR_WEBHOOK_MAX_RETRIES` | `5` | Outbound retry count. |
| `CR_WEBHOOK_REDELIVER_S` | `30` | Redelivery interval, seconds. |
| `CR_WEBHOOK_PROBE_S` | `60` | Dead-target probe interval, seconds. |
| `CR_WEBHOOK_CIRCUIT_THRESHOLD` | `10` | Consecutive failures that open the circuit. |
| `CR_WEBHOOK_BATCH_MAX` | `10` | Batch flush size. |
| `CR_WEBHOOK_BATCH_FLUSH_S` | `5` | Batch flush interval, seconds. |
| `DEEPSEEK_API_KEY` | _(unset)_ | API key for the deepseek provider preset (referenced as `env:DEEPSEEK_API_KEY`). Without it, guard LLM calls fail and the guard fails open. |

## API

The full API is documented in [`docs/openapi.yaml`](docs/openapi.yaml) — an OpenAPI 3.1 spec covering 16 endpoints across 6 operation groups:

| Group | Endpoints | Description |
|-------|-----------|-------------|
| **Health** | `GET /health` | Service health check |
| **Relay** | `POST /relay/publish`, `GET /relay/subscribe/{topic}`, `GET /relay/topics` | Pub/sub |
| **Mesh** | `GET /mesh/connect/{agentID}`, `GET /mesh/peers` | P2P connections |
| **Federation** | `GET /fed/peers` | Relay-to-relay federation peer listing (CR-FEAT-006) |
| **Registry** | `POST /agents`, `GET /agents` (capability filter), `GET /agents/{id}`, `PATCH /agents/{id}`, `DELETE /agents/{id}` | Agent identity + self-configuration |
| **Inbox** | `POST /agents/{id}/inbox`, `GET /agents/{id}/inbox`, `POST /agents/{id}/inbox/ack`, `GET /agents/{id}/inbox/stats` | Message delivery |

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
- **Persistence** — PostgreSQL backend for registry + inboxes via `CR_DATABASE_URL`; verified live that agents and undelivered messages survive a server restart
- **Message guard** — LLM prompt-injection guard at the delivery choke point (CR-FEAT-010..014): structured verdicts, fail-open with per-policy fail-closed, X-Crier-Guard-* headers, provider failover, opt-in kanban cards
- **API** — 16 HTTP endpoints wired with middleware, graceful shutdown
- **CI** — GitHub Actions, matrix build Go 1.26.6

Coverage numbers above are measured fresh per change (`go test -short -count=1 -cover ./internal/<pkg>`); the ≥70% gate lives in `make coverage-check`.

### Roadmap

- **CI-003b** ✅ — PostgreSQL persistence for registry and inboxes (implemented, `CR_DATABASE_URL`)
- **CI-007** ✅ — MCP server exposing registry and inbox tools (implemented, `cmd/crier-mcp`)
- **Capability-based routing** — route messages by agent capability cards

## License

MIT — see [LICENSE](LICENSE).
