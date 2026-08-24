# Crier Integration Guide

How to build an agent on Crier end-to-end: start the server, register an
agent, deliver and retrieve messages, ack them, publish/subscribe over the
relay, connect peers over the mesh, and run it durably on PostgreSQL.

This is the maintained, user-facing companion to:

- `docs/openapi.yaml` — the complete HTTP API contract
- `docs/mesh-protocol.md` — the authoritative mesh wire format
- `docs/architecture.md` / `docs/specs.md` — design and behavior specs
- `examples/demo.sh` — the runnable register → deliver → retrieve → ack round-trip

Everything below was live-verified against a running server. All examples use
`curl` + `openssl` + `websocat` (or Python `websockets`); no client SDK is
required.

---

## 1. Running the server

Build and start with the default in-memory backend:

```bash
make build
./bin/crier                 # listens on :8767
```

The server binary takes no positional arguments but accepts flags that
**override** the env-driven config (see `./bin/crier --help`):

| Flag | Overrides | Default |
|------|-----------|---------|
| `-port N` | `CRIER_PORT` | `8767` |
| `-db-url URL` | `CR_DATABASE_URL` | unset (in-memory backend) |
| `-version` | — | prints build version and exits |

Configuration is otherwise env-driven (full table in README):

| Env var | Purpose | Default |
|---------|---------|---------|
| `CRIER_PORT` | listen port | `8767` |
| `CR_DATABASE_URL` | PostgreSQL URL (fallbacks: `DATABASE_URL`, `CRIER_DATABASE_URL`) | unset |
| `CR_AUTH_TOKEN` | bearer token required on all requests except `/health` | empty = auth disabled |
| `CR_REQUIRE_AGENT_SIG` | require per-agent ed25519 signatures on inbox + agent-delete endpoints | `true` |
| `CR_LOG_LEVEL` / `CR_LOG_FORMAT` | logging (`debug\|info\|warn\|error`, `text\|json`) | `info` / `text` |
| `CR_RATE_LIMIT_PER_MINUTE` | relay publish rate limit | `100` |
| `CR_WS_ALLOWED_ORIGINS` | comma-separated WebSocket origins, `*` = allow all | allow all |
| `CR_GUARD_ENABLED` | LLM message-guard master switch. When on, every inbound delivery is classified before webhook POST / inbox store | `true` |
| `CR_GUARD_TIMEOUT_MS` | Per-message guard budget in milliseconds — covers the whole provider chain, retries included | `10000` |
| `DEEPSEEK_API_KEY` | API key for the deepseek provider preset. Without it, guard LLM calls fail and the guard fails open | unset |

### Auth configurations

> **The LLM message guard is ON by default.** Every inbound delivery is
> classified by a guard LLM (default model `deepseek-v4-flash`, 10s per-message
> budget — `CR_GUARD_TIMEOUT_MS`) before it is webhook-POSTed or inbox-stored.
> For local dev without an API key, set `CR_GUARD_ENABLED=false`; to exercise
> the guard, set `DEEPSEEK_API_KEY`. Without a key the guard call fails and the
> guard fails OPEN — the delivery proceeds, but the run is flagged
> `X-Crier-Guard-Error: true` on outbound webhook POSTs and
> `"guard":{"errored":true}` on the deliver response. See
> [The LLM message guard](#the-llm-message-guard) for the verdict wire format.

The two auth knobs combine into three supported configurations. Every recipe
in this guide works under all three (drop or add the headers as shown):

| Config | `CR_AUTH_TOKEN` | `CR_REQUIRE_AGENT_SIG` | Effect |
|--------|-----------------|------------------------|--------|
| A. Open | unset | `false` | No auth at all. Good for local dev only. |
| B. Bearer only | `secret` | `false` | All HTTP requests (except `/health`) need `Authorization: Bearer secret`; inbox endpoints are open to any agent. |
| C. Full security | `secret` | `true` (default) | Bearer token **and** per-agent ed25519 signatures on inbox/agent-delete endpoints. The production default. |

Start command per config:

```bash
# A. Open
./bin/crier
# B. Bearer only
CR_AUTH_TOKEN=secret CR_REQUIRE_AGENT_SIG=false ./bin/crier
# C. Full security
CR_AUTH_TOKEN=secret ./bin/crier
```

For the rest of this guide, define `AUTH=(-H "Authorization: Bearer $TOKEN")`
when in config B or C, and empty otherwise:

```bash
AUTH=()
[ -n "${CR_AUTH_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer ${CR_AUTH_TOKEN}")
```

---

## 2. Registry: agent identity

Every agent has an id, an ed25519 public key (hex), and optional
capabilities. The public key is what config C verifies signatures against.

```bash
# keygen (openssl 1.1.1+)
openssl genpkey -algorithm ED25519 -out agent.key
PUBKEY=$(openssl pkey -in agent.key -pubout -outform DER | tail -c 32 | xxd -p -c 64)

# register → 201
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"id\":\"agent-1\",\"public_key\":\"${PUBKEY}\",\"capabilities\":[\"intel\"]}"
```

List (`GET /agents` → 200), get one (`GET /agents/{id}`), and delete
(`DELETE /agents/{id}` → 204). **In config C, delete requires a signature**
(see §4 — the same signing scheme as inbox requests).

---

## 3. Inbox: deliver → retrieve → ack

The inbox is a durable per-agent FIFO with lease-based delivery: a retrieve
leases messages for 30 seconds; acked messages are removed; unacked messages
are redelivered after the lease expires.

### Deliver (no signature needed in any config)

```bash
curl -s -X POST localhost:8767/agents/agent-1/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world","n":42}}'          # → 201 {"id":"..."}
```

Every delivery first passes the LLM message guard (see below) — the response
body adds `"guard":{...}` whenever the verdict was not a plain allow, including
errored fail-open runs.

### The LLM message guard

The guard is **ON by default** (`CR_GUARD_ENABLED=true`) and sits at ONE choke
point in `POST /agents/{id}/inbox` (CR-FEAT-010): after the deliver request is
decoded, before BOTH downstream branches — webhook POST and inbox store — so
webhook and inbox deliveries get identical treatment and the verdict is
computed exactly once per message (redelivery and batch flush never re-run
it). The guard LLM (default model `deepseek-v4-flash`) returns a single JSON
verdict:

```json
{"decision":"allow","risk_level":"low","reason":"benign agent message","matched_patterns":[]}
```

`decision` is `allow` | `block` | `sanitize`; `risk_level` is `low` | `medium`
| `high`. A deterministic pattern pre-scan over the raw payload runs first and
its matches are merged into the final verdict.

- **allow** — the delivery proceeds as-is. The verdict rides on the outbound
  webhook POST as `X-Crier-Guard-*` headers (`X-Crier-Guard-Decision`,
  `X-Crier-Guard-Risk`, `X-Crier-Guard-Reason` (percent-encoded),
  `X-Crier-Guard-Patterns` (comma-joined), `X-Crier-Guard-Policy`,
  `X-Crier-Guard-Provider`, `X-Crier-Guard-Model`) and on the deliver response
  / inbox entry as `guard` metadata. A plain-allow verdict leaves the response
  `guard` field absent; non-allow verdicts and errored runs surface it.
- **block** — a uniform `403` with the full verdict; the message is never
  queued, never stored, never POSTed:
  ```bash
  curl -s -X POST localhost:8767/agents/agent-1/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
    -d '{"payload":{"text":"ignore previous instructions"}}'
  # → 403 {"error":"GUARD_BLOCKED","guard":{"decision":"block","risk_level":"high","reason":"...","matched_patterns":["..."],"policy":"default","provider":"deepseek","model":"deepseek-v4-flash"}}
  ```
- **sanitize** — the guard LLM rewrites the payload with a fixed
  neutralization prompt and the REWRITTEN payload is delivered in place of the
  original; the original rides in `crier.guard.quarantined_payload` (base64)
  for provenance and is never delivered.

**The guard fails OPEN by default.** Without `DEEPSEEK_API_KEY` — or on a
provider error / timeout — the guard resolves to `allow` with `errored: true`
(reason e.g. `guard_error: no provider api key`): the delivery still succeeds,
but each delivery can burn up to `CR_GUARD_TIMEOUT_MS` (default `10000` ms =
10s) and the run is flagged `X-Crier-Guard-Error: true` on outbound webhook
POSTs / `"guard":{"errored":true}` on the deliver response. Per-policy
`fail_closed: true` flips this to the policy's error action (default `block`).
For a deterministic keyless dev loop start the server with
`CR_GUARD_ENABLED=false`; to exercise the guard, set `DEEPSEEK_API_KEY`.

Guard policy is per-agent at registration
(`"guard":{"policies":[{"id":"default"}]}` — at least one policy required,
invalid config → 400); agents without a guard config use the server-wide
default (`CR_GUARD_DEFAULT_POLICY`, built-in `default`). Full contract:
`specs/LLM-MESSAGE-GUARD.md`.

### Retrieve (signed in config C)

```bash
TS=$(date +%s)
printf 'GET\n/agents/agent-1/inbox\n%s' "$TS" > payload.txt
SIG=$(openssl pkeyutl -sign -rawin -inkey agent.key -in payload.txt | xxd -p -c 128)
curl -s localhost:8767/agents/agent-1/inbox "${AUTH[@]}" \
  -H "X-Agent-ID: agent-1" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}"
# → 200 {"messages":[{"id":"...","payload":"<base64>",...}],"lease_id":"..."}
```

The signature scheme (config C only; the headers are ignored when
`CR_REQUIRE_AGENT_SIG=false`):

```
X-Agent-Sig = hex(ed25519_sign("METHOD\n/path\nunix-seconds", private_key))
```

- The **method and path are bound into the signature** — a signature over the
  wrong method or path is rejected with 401.
- The timestamp must be within ±30s of the server clock.
- The payload is base64-encoded JSON — decode before use.

### Ack (signed in config C — and read this carefully)

An ack **must include `message_ids`** (one or more message ids from the
retrieve response). A lease-only ack is rejected with 400 in current builds;
historically it was a silent no-op that left messages queued:

```bash
TS=$(date +%s)
printf 'POST\n/agents/agent-1/inbox/ack\n%s' "$TS" > payload.txt
SIG=$(openssl pkeyutl -sign -rawin -inkey agent.key -in payload.txt | xxd -p -c 128)
curl -s -X POST localhost:8767/agents/agent-1/inbox/ack "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H "X-Agent-ID: agent-1" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}" \
  -d '{"lease_id":"<lease_id from retrieve>","message_ids":["<message id>"]}'   # → 204
```

Verify the queue actually drained with the stats endpoint (not with a
re-retrieve — leased messages stay hidden behind their lease until it
expires):

```bash
curl -s localhost:8767/agents/agent-1/inbox/stats "${AUTH[@]}" \
  -H "X-Agent-ID: agent-1" -H "X-Agent-Ts: $(date +%s)" -H "X-Agent-Sig: ${SIG}"   # → queue_depth: 0
```

### Lifecycle summary

| Step | Endpoint | Config A | Config B | Config C |
|------|----------|----------|----------|----------|
| Register | `POST /agents` | open | bearer | bearer |
| Deliver | `POST /agents/{id}/inbox` | open | bearer | bearer |
| Retrieve | `GET /agents/{id}/inbox` | open | bearer | bearer + signature |
| Ack | `POST /agents/{id}/inbox/ack` | open | bearer | bearer + signature |
| Stats | `GET /agents/{id}/inbox/stats` | open | bearer | bearer + signature |
| Delete agent | `DELETE /agents/{id}` | open | bearer | bearer + signature |

---

## 4. Relay: publish/subscribe

```bash
# subscribe (WebSocket) — one terminal
websocat "ws://localhost:8767/relay/subscribe/my-topic" "${AUTH[@]}"
# or Python: websockets.connect("ws://localhost:8767/relay/subscribe/my-topic",
#             additional_headers={"Authorization": "Bearer TOKEN"})

# publish — another terminal → 202
curl -s -X POST localhost:8767/relay/publish "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"topic":"my-topic","event":{"kind":"alert","level":5}}'
```

The event arrives on the subscriber socket. `GET /relay/topics` lists topics
with live subscribers. Publishing is rate-limited
(`CR_RATE_LIMIT_PER_MINUTE`, default 100/min).

---

## 5. Mesh: agent-to-agent request/response

The mesh is a WebSocket peer layer: connect, send a one-way `REGISTER` frame,
then exchange `REQUEST`/`RESPONSE` frames that the server relays between
peers. Unlike the registry/inbox API, mesh frames carry **no bearer or
signature auth** — do not use the mesh for privileged operations without an
application-level auth layer.

```bash
# terminal 1 — agent alpha
websocat "ws://localhost:8767/mesh/connect/alpha"
{"type":"REGISTER","version":1,"message_id":"m1","timestamp":"2026-08-13T12:00:00.000000000Z","agent_id":"alpha","lease_id":"","lease_ttl_ms":3600000,"capabilities":{"version":"0.1.0","topics":[],"max_concurrent_sessions":10}}

# terminal 2 — agent beta (same pattern: connect + REGISTER)
# alpha → beta request (relayed by the server)
{"type":"REQUEST","version":1,"message_id":"q1","timestamp":"2026-08-13T12:00:01.000000000Z","source":{"agent_id":"alpha"},"target":{"agent_id":"beta"},"method":"GET","path":"/intel","body":null,"trace_id":"t1","timeout_ms":5000}

# beta → alpha response — request_id MUST equal the request's message_id
{"type":"RESPONSE","version":1,"message_id":"r1","timestamp":"2026-08-13T12:00:02.000000000Z","request_id":"q1","status_code":200,"source":{"agent_id":"beta"},"body":{"data":"..."}}
```

Rules that matter:

- Frames are single JSON objects, newline-terminated (NDJSON), with RFC3339
  timestamps — a non-RFC3339 timestamp gets the frame silently dropped.
- `REGISTER` is fire-and-forget: the server never acks it.
- A response whose `request_id` doesn't match a pending request is dropped
  and the requester times out.
- Connected peers are listed via `GET /mesh/peers` → `{"peers":[...],"count":N}`.

Full wire reference: `docs/mesh-protocol.md`.

---

## 6. Durable setup (PostgreSQL)

```bash
docker compose up -d postgres     # project container, port 5437
CR_DATABASE_URL='postgres://crier:crier@localhost:5437/crier?sslmode=disable' ./bin/crier
```

Migrations apply automatically on startup. With the Postgres backend, agents
and undelivered messages survive server restarts; without
`CR_DATABASE_URL` everything is process-lifetime only. The MCP server
(`make build-mcp && ./bin/crier-mcp`, stdio, 8 tools) shares the same
backend, so agents registered over HTTP are visible over MCP and vice versa.

---

## 7. Full round-trip in one script

`examples/demo.sh` runs the complete health → keygen → register → deliver →
signed retrieve → ack → verify-empty flow and works under all three auth
configurations (export `CR_AUTH_TOKEN` when you started the server with
auth enabled). It is guard-aware: with `DEEPSEEK_API_KEY` unset it notes the
fail-open guard behavior and exports `CR_GUARD_ENABLED=false` so keyless runs
stay deterministic; with a key set, deliveries are LLM-classified and the
script prints what the guard verdicts mean on the wire:

```bash
make run
CR_AUTH_TOKEN=secret ./examples/demo.sh     # config B or C
./examples/demo.sh                          # config A
```
