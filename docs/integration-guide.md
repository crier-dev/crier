# Crier Integration Guide

How to build an agent on Crier end-to-end: start the server, register an
agent, deliver and retrieve messages, ack them, publish/subscribe over the
relay, connect peers over the mesh, and run it durably on PostgreSQL.

This is the maintained, user-facing companion to:

- `docs/openapi.yaml` — the complete HTTP API contract
- `docs/mesh-protocol.md` — the authoritative mesh wire format
- `docs/architecture.md` / `docs/specs.md` — design and behavior specs
- `examples/demo.sh` — the runnable register → deliver → retrieve → ack round-trip
- `examples/ws-mesh-demo/` — the runnable **zero-install** relay pub/sub + mesh
  demo: `run-demo.sh` plus a Go WebSocket client, nothing to install
- `examples/muster-bridge/run-demo.sh` — the runnable **Muster onboarding** path
  (§10): spec discovery, register → deliver → retrieve → ack with every status
  asserted, and the measured bearer-only vs signature-required matrix

Everything below was live-verified against a running server. Every leg in this
guide has a repo-shipped, runnable path — `examples/demo.sh`
for register/deliver/retrieve/ack and `examples/ws-mesh-demo/run-demo.sh` for
relay subscribe/publish and the mesh (§4, §5) — and those need only the Go
toolchain, `curl` and `coreutils`: **no external WebSocket client is required for
any part of this guide.** The `curl` + `openssl` + `websocat` transcript below is
the *manual* alternative for reading a single step or pasting one frame by hand
(`websocat`, or Python `websockets`, must then be installed yourself; no client SDK is
required). The signed-config snippets require **OpenSSL >= 3**: the signing
helper uses `openssl pkeyutl -sign -rawin` (an OpenSSL 3+ flag) and fails
loudly on older versions instead of producing an empty signature that the
server silently rejects with 401.

### Muster-generated clients

`servers[0]` is the local-development default. When generating a Muster command
client for a deployed relay, override it explicitly with that relay's real base
URL:

```bash
openapi-cli generate docs/openapi.yaml --base-url https://relay.example
```

The generated commands use `https://relay.example` rather than the spec's
`http://localhost:8767` default.

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
| `-version` | — | prints the build identity (version, commit, dirty marker) and exits |

Configuration is otherwise env-driven (full table in README):

| Env var | Purpose | Default |
|---------|---------|---------|
| `CRIER_PORT` | listen port | `8767` |
| `CR_DATABASE_URL` | PostgreSQL URL (fallbacks: `DATABASE_URL`, `CRIER_DATABASE_URL`) | unset |
| `CR_AUTH_TOKEN` | bearer token required on all requests **except the five exempt paths** (`/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs`) | empty = auth disabled |
| `CR_REQUIRE_AGENT_SIG` | require per-agent ed25519 signatures on agent-scoped endpoints: inbox retrieve/ack/stats, `DELETE /agents/{id}`, and `PATCH /agents/{id}` | `true` |
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
| B. Bearer only | `secret` | `false` | All HTTP requests except the five exempt paths (`/health`, `/version`, `/openapi.json`, `/openapi.yaml`, `/docs`) require `Authorization: Bearer ***`; inbox endpoints are open to any agent. |
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

# register → 201 (a duplicate id answers 409 {"error":"agent already
# registered: \"agent-1\""} — idempotency-refused, the existing agent is
# untouched; skip registration or delete the agent first)
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

An agent that has registered a webhook is **pushed** to instead of stored: the
message is POSTed to the agent's endpoint and the inbox stays empty — see
§8 *Push delivery (webhooks)* for the registration object and the three
delivery modes.

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
# Signing helper — requires OpenSSL >= 3 for pkeyutl -sign -rawin; on older
# OpenSSL it errors loudly instead of producing an empty (silently-401-rejected)
# signature. `-rawin` is also one-shot: it needs a SEEKABLE payload, so a piped
# payload yields a zero-byte signature — the helper now refuses that loudly too,
# instead of letting the server 401 with a message blaming the headers.
sig() { if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then echo "ERROR: this signing helper requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)" >&2; return 1; fi; _sig=$(openssl pkeyutl -sign -rawin -inkey agent.key -in payload.txt 2>/dev/null | xxd -p -c 128); if [ -z "$_sig" ]; then echo "ERROR: signing produced an EMPTY signature. pkeyutl -sign -rawin is a one-shot operation and needs a SEEKABLE payload passed with -in <file> — a piped or redirected payload fails with 'unable to determine file size for oneshot operation' and yields zero bytes, which the server rejects 401 naming the empty X-Agent-Sig header." >&2; return 1; fi; printf '%s\n' "$_sig"; }
SIG=$(sig)
curl -s localhost:8767/agents/agent-1/inbox "${AUTH[@]}" \
  -H "X-Agent-ID: agent-1" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}"
# → 200 {"messages":[{"id":"...","payload":"<base64>",...}],"lease_id":"...","queue_depth":1,"leased_count":1}
```

Both query spellings are accepted (the documented and the historical):

| Parameter | Alias | Default | Notes |
|---|---|---|---|
| `limit` | `max` | `10` | max messages claimed per retrieve; `> 100` → `400` |
| `lease_seconds` | `lease` | `30` | lease duration in seconds |
| `wait_seconds` | — | `0` | long-poll budget, `0..120`; outside the range or non-integer → `400` (CR-FEAT-023) |

When both spellings of the same parameter appear in one request, the
historical one (`max` / `lease`) wins and the alias fills in only what it
left absent (DF-CRIER-177).

**Stop polling: `?wait_seconds=` (CR-FEAT-023).** The durable lane used to be
strictly poll-only, which made it the one lane that could not wake a sleeping
agent. With `wait_seconds=N` the read parks and answers the moment a message is
claimable:

```bash
# the same signed retrieve, parked for up to 60s — returns as soon as a
# delivery lands (the signature covers the PATH, so the query string is not
# part of it)
curl -s "localhost:8767/agents/agent-1/inbox?wait_seconds=60" "${AUTH[@]}" \
  -H "X-Agent-ID: agent-1" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}"
# → 200 {"messages":[{...}],"lease_id":"...","queue_depth":1,"leased_count":1}
```

If the budget expires with nothing claimable the answer is the **same empty
body** a poll-only read gives (`messages: []`, `lease_id: ""`, plus the live
counters above), so a client treats both identically and simply re-polls — a
long-poll is a drop-in for a poll loop, not a new contract:

```bash
curl -s "localhost:8767/agents/agent-1/inbox?wait_seconds=5" "${AUTH[@]}" \
  -H "X-Agent-ID: agent-1" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}"
# → 200 {"messages":[],"lease_id":"","queue_depth":0,"leased_count":0}   (after ~5s)
```

What the budget does and does not change:

- **`0` or absent is exactly today's read** — immediate, no timer, no
  subscription. The budget is `0..120`; a non-integer, a negative value or
  anything above `120` is a `400` that names the parameter, because a
  documented parameter is honored or rejected, never silently ignored
  (DF-CRIER-180).
- **Lease, ack and TTL are untouched.** A long-poll is a sequence of ordinary
  retrieves: whatever it claims is leased by the same rules, and the counters
  are taken after the claim that produced the response.
- **Errors are never waited out.** An unregistered agent still answers `404`
  immediately, and a rejected parameter is answered before anything parks.
- **A delivery through this relay's deliver endpoint wakes a parked read
  immediately.** Inbox writes that bypass it — the federation hold queue, the
  webhook-failure notices, a second relay process sharing the store — surface
  within about a second.

An agent that cannot hold an HTTP request open at all has the other half: a
new-message ping on its mesh socket. Connect with
`?inbox_notify=1` on `GET /mesh/connect/{agentID}` and the server writes one
`INBOX_NOTIFY` frame there per delivery into that agent's inbox (message id and
sender; never the payload). It is opt-in per connection and best effort by
design — the durable read above stays the contract. Wire format:
[`docs/mesh-protocol.md`](mesh-protocol.md) §INBOX_NOTIFY.

An empty or fully-leased inbox is a **successful read with nothing to claim** —
not an error, and not a lease:

```bash
# second retrieve while everything is still leased (or a fresh agent)
# → 200 {"messages":[],"lease_id":"","queue_depth":3,"leased_count":3}
```

The `queue_depth` / `leased_count` counters on every retrieve response carry
the same numbers `GET /agents/{id}/inbox/stats` reports, taken after the
retrieve itself (DF-CRIER-177). They exist so an empty `messages` array is
never ambiguous:

- `queue_depth == 0 && leased_count == 0` — the inbox is genuinely empty
  (nothing was ever delivered, or everything was acked / expired).
- `leased_count > 0` — the messages are HELD under unexpired leases by other
  retrievers. They are not lost: they return after lease expiry (or are
  removed by their holder's ack). Before DF-CRIER-177 this state was
  byte-identical on the wire to an empty inbox.

The rules that follow from that (DF-CRIER-32):

- `messages` is always a JSON array — `[]` when nothing was claimable, never
  `null`.
- `lease_id` is a **non-empty string only when at least one message was
  leased**. An empty `lease_id` means there is nothing to ack: no lease was
  minted, so do not store it, retry it, or send it to the ack endpoint.
- Ack only IDs that came back from the **same** retrieve call as their
  `lease_id`. A lease does not make messages ackable across responses, and a
  lease covers exactly the batch it was minted for.

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
SIG=$(sig)  # helper from §3 Retrieve — fails loudly on OpenSSL < 3
curl -s -X POST localhost:8767/agents/agent-1/inbox/ack "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H "X-Agent-ID: agent-1" -H "X-Agent-Ts: ${TS}" -H "X-Agent-Sig: ${SIG}" \
  -d '{"lease_id":"<lease_id from retrieve>","message_ids":["<message id>"]}'   # → 204
```

Ack failures are two distinct classes — read the status, not just "error"
(DF-CRIER-32):

| Status | Meaning | What to do |
|---|---|---|
| `204` | Every ID was removed under that lease | done |
| `400` | Malformed body, or `lease_id` / `message_ids` missing or empty (a lease-only ack is a silent no-op — CR-GAP-014) | fix the request |
| `404` | Agent not found, **or** a requested `message_id` does not exist in the inbox (never delivered, already acked, or expired and purged). The body names the offending IDs. | drop the unknown IDs; do not retry them under another lease |
| `409` | Every requested ID exists, but at least one is leased under a **different** (or no) lease — the body reports the lease currently held | re-retrieve to get a fresh lease for those IDs |

A missing ID is always `404` and never reported as a lease conflict, so a
`409` means the messages are there and the lease you sent is stale — not that
your IDs are wrong. Conversely, if every ID you sent was returned by the
retrieve that minted `lease_id`, a `409` means the lease has since been
released (expired + purged, or acked by another caller).

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

**Zero-install path — run the shipped driver.** No WebSocket client to install:
`examples/ws-mesh-demo` builds this server and its own Go WebSocket client
(gorilla/websocket, from `go.mod`), starts a relay on a scratch port it proves is
free, subscribes on an exact topic and asserts the fan-out:

```bash
DEMO_KEEPALIVE_WAIT=0 bash examples/ws-mesh-demo/run-demo.sh   # port chosen by the guard (first candidate 18961)
DEMO_PORT=18977 DEMO_KEEPALIVE_WAIT=0 bash examples/ws-mesh-demo/run-demo.sh   # or a port YOU name (checked, never rotated)
```

Its relay steps prove, live, against one server it started itself:

- `POST /relay/publish` **without** `X-Agent-ID` → **401** `{"error":"X-Agent-ID
  header required for rate-limited publish"}` (the per-agent limiter keys on the
  header; `CR_RATE_LIMIT_PER_MINUTE=0` is the only way to drop the requirement),
- the same publish **with** `X-Agent-ID` → **202**,
- a publish to a *different* topic (`demo-other`) reaches no subscriber — the
  subscriber runs with `-once`, so a fan-out to every subscriber would hand it
  that event first and the run's payload assertion would fail,
- the subscriber receives exactly **one** frame,
  `{"topic":"demo","event":{...}}`, carrying the literal published topic.

The client is also usable against a server you already have running:

```bash
go run ./examples/ws-mesh-demo subscribe -topic my-topic -url http://localhost:8767
go run ./examples/ws-mesh-demo subscribe -topic my-topic -url http://localhost:8767 -once
```

(`subscribe` prints `SUBSCRIBED <topic>` after the upgrade, then one `EVENT
<payload>` line per received frame; `-once` exits 0 after the first one. Full
flag list: `examples/ws-mesh-demo/README.md`.)

**Manual alternative — an external WebSocket client.** Everything below can also
be done by hand with `websocat` (or Python `websockets`), which you install
yourself:

```bash
# subscribe (WebSocket) — one terminal
websocat "ws://localhost:8767/relay/subscribe/my-topic" "${AUTH[@]}"
# or Python: websockets.connect("ws://localhost:8767/relay/subscribe/my-topic",
#             additional_headers={"Authorization": "Bearer TOKEN"})

# publish — another terminal → 202
curl -s -X POST localhost:8767/relay/publish "${AUTH[@]}" -H 'Content-Type: application/json' \
  -H 'X-Agent-ID: agent-1' \
  -d '{"topic":"my-topic","event":{"kind":"alert","level":5}}'
```

The subscriber receives one text frame per published event, carrying the
literal published topic next to the event itself:

```json
{"topic":"my-topic","event":{"kind":"alert","level":5}}
```

- `event` is the event exactly as published — an object, a string, an array, a
  number or `null` — not a string-encoded copy of it.
- `topic` is always the **literal published topic**, never the subscription
  pattern. Subscribing with a wildcard (`alerts.*` matches one segment,
  `alerts.>` matches one or more trailing segments) therefore still tells you
  which topic matched: a publish to `alerts.fire` arrives as
  `{"topic":"alerts.fire","event":{...}}`.
- This is a deliberate breaking frame change (DOGFOOD-RELAY-1): frames used to
  be the bare event (`{"kind":"alert","level":5}`) with no topic. The publish
  call itself is unchanged — `POST /relay/publish` still answers `202` with an
  empty body.

`GET /relay/topics` lists topics with live subscribers. Publishing is
rate-limited **per agent** (`CR_RATE_LIMIT_PER_MINUTE`, default 100/min), so
the limiter keys on the `X-Agent-ID` header and a publish has to carry it:
while rate limiting is on, the call above without that header is rejected with
`401` and `{"error":"X-Agent-ID header required for rate-limited publish"}`
before the body is read. `CR_RATE_LIMIT_PER_MINUTE=0` turns the limiter off and
the header requirement off with it.

The header requirement is pinned both ways by `make docs-check` against its own
default-config server (rate limiting on), so the example above and this
requirement cannot drift apart again:

<!-- doccheck -->
```bash
curl -s -X POST $BASE/relay/publish -H 'Content-Type: application/json' -H 'X-Agent-ID: agent-1' -d '{"topic":"my-topic","event":{"kind":"alert","level":5}}'   # -> 202
curl -s -X POST $BASE/relay/publish -H 'Content-Type: application/json' -d '{"topic":"my-topic","event":{"kind":"alert","level":5}}'   # -> 401
```

---

## 5. Mesh: agent-to-agent request/response

The mesh is a WebSocket peer layer: connect, send a one-way `REGISTER` frame,
then exchange `REQUEST`/`RESPONSE` frames that the server relays between
peers. Unlike the registry/inbox API, mesh frames carry **no bearer auth**, and
on the default configuration **no signature auth either** — do not use the mesh
for privileged operations without an application-level auth layer. For a shared
or networked deployment, start the server with `CR_REQUIRE_MESH_AUTH=true`: the
connect is then an ed25519 challenge the peer signs with the key the registry
holds for it, an unauthenticated connection is refused `AUTH_FAILED` and never
listed by `GET /mesh/peers`, and a peer can only request as itself
([`docs/mesh-protocol.md`](mesh-protocol.md) §Authentication).

**Zero-install path — run the shipped driver.** `examples/ws-mesh-demo/run-demo.sh`
needs no WebSocket client: it starts its own relay on a scratch port it proves is
free (and proves the listening pid is its own before measuring it), connects two
peers over `ws://…/mesh/connect/<agentID>`, shows both in `GET /mesh/peers` while
their sockets are open, and runs the exchange below with the correlation and
KEEPALIVE rules asserted:

```bash
DEMO_KEEPALIVE_WAIT=0 bash examples/ws-mesh-demo/run-demo.sh   # port chosen by the guard (first candidate 18961)
```

The client also works against a server you already have running — the peer is
visible in `/mesh/peers` for as long as its socket is open:

```bash
go run ./examples/ws-mesh-demo peer -agent alpha -url http://localhost:8767
curl -s localhost:8767/mesh/peers     # {"peers":[{"agent_id":"alpha"}],"count":1}
```

**Manual alternative — an external WebSocket client.** The transcript below is a
`websocat` session you drive by hand (install `websocat` or Python `websockets`
first):

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

Host port and compose project are env-overridable (`CRIER_PG_HOST_PORT`, `COMPOSE_PROJECT_NAME` — see README § Durable backend).

Migrations apply automatically on startup. With the Postgres backend, agents
and undelivered messages survive server restarts; without
`CR_DATABASE_URL` everything is process-lifetime only. The MCP server
(`make build-mcp && ./bin/crier-mcp`, stdio, 13 tools) shares the same
backend, so agents registered over HTTP are visible over MCP and vice versa.

### 6.1 Remote MCP mode (crier-mcp against a running server)

The MCP server can also bridge to a **running Crier server over HTTP**
instead of opening its own database: set `CRIER_HTTP_URL` and `CRIER_AGENT_ID`
and registry/inbox tool calls become HTTP requests carrying that agent
identity. With the server in the secure default configuration
(`CR_REQUIRE_AGENT_SIG=true`), those requests must be per-agent signed, so
crier-mcp loads an ed25519 private key at startup:

```bash
# One-time: generate (or reuse) a PKCS#8 PEM ed25519 key and register its
# public half for the bridge's agent id — same key format as §3.
openssl genpkey -algorithm ED25519 -out ~/.config/crier/mcp-agent.key
PUBKEY_HEX=$(openssl pkey -in ~/.config/crier/mcp-agent.key -pubout -outform DER | tail -c 32 | xxd -p -c 64)
curl -s -X POST localhost:8767/agents -H 'Content-Type: application/json' \
  -d "{\"id\":\"mcp-agent\",\"public_key\":\"${PUBKEY_HEX}\",\"capabilities\":[\"mcp\"]}"

# Run the bridge: URL, agent id, and the private key file.
export CRIER_HTTP_URL=http://localhost:8767
export CRIER_AGENT_ID=mcp-agent
export CRIER_AGENT_PRIVATE_KEY_FILE=$HOME/.config/crier/mcp-agent.key
# Optional: shared bearer token, only when the server runs with CR_AUTH_TOKEN.
# export CRIER_AUTH_TOKEN=...
make build-mcp && ./bin/crier-mcp
```

The launcher's stdout carries only JSON-RPC frames — the build's own diagnostics go
to stderr — so a strict stdio client may launch that line as-is; `make mcp` does the
same in a single command.

- `CRIER_AGENT_PRIVATE_KEY_FILE` must hold a **PKCS#8 PEM ed25519** private
  key (`openssl genpkey -algorithm ED25519`). Unreadable, malformed,
  non-PKCS#8, or non-ed25519 keys fail at startup with an explicit error;
  key material is never logged or echoed.
- Every request then carries fresh `X-Agent-Ts` / `X-Agent-Sig` headers
  signing `METHOD\n<path>\n<unix-seconds>` (query string excluded) — the
  same scheme as §3, generated automatically.
- `CRIER_AUTH_TOKEN` is orthogonal: it authenticates the bridge to a server
  that requires the shared bearer token; the per-agent key authenticates the
  agent. Both can be set at once.
- **No key is needed only when the server disables per-agent signatures**
  (`CR_REQUIRE_AGENT_SIG=false`): unset `CRIER_AGENT_PRIVATE_KEY_FILE` and
  crier-mcp falls back to the plain `X-Agent-ID` identity. Against a signed
  server that combination fails with 401 on agent-owned routes.

The full variable reference is in `./bin/crier-mcp --help`.

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

---

## 8. Push delivery (webhooks)

An agent can register its own HTTP endpoint and have deliveries **pushed** to
it instead of being stored in its durable inbox. The endpoint is part of the
agent registration: pass a `webhook` object on `POST /agents` (at creation) or
on `PATCH /agents/{id}` (register/update it later); on PATCH an **absent or
explicit `null`** `webhook` removes it. Every delivery answer names where the
message actually went: `"transport":"webhook"` when it was pushed,
`"transport":"inbox"` when it was stored.

### 8.1 The registration object

```bash
# at creation (delivery_mode is optional — async is the default)
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"id":"agent-1","capabilities":["relay"],
       "webhook":{"url":"http://127.0.0.1:9000/hook","delivery_mode":"blocking"}}'
# → 201 {"id":"agent-1","public_key":"","capabilities":["relay"],"status":"online",
#        "registered_at":"…","last_seen":"…",
#        "webhook":{"url":"http://127.0.0.1:9000/hook","delivery_mode":"blocking"}}

# or later on an existing agent → 200 with the updated agent
curl -s -X PATCH localhost:8767/agents/agent-1 "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"webhook":{"url":"http://127.0.0.1:9000/hook","delivery_mode":"async"}}'

# remove it again: absent or null both drop the webhook → 200, no "webhook" key
curl -s -X PATCH localhost:8767/agents/agent-1 "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"webhook":null}'
```

| key | type | accepted values / range | notes |
|-----|------|-------------------------|-------|
| `url` | string | **required**, `http://` or `https://` | anything else → 400 `webhook.url must be http(s)://` |
| `auth_type` | string | `none` (default), `bearer` | `bearer` requires `auth_value_ref`; any other value → 400 |
| `auth_value_ref` | string | `env:VAR` | resolved from the server's environment at delivery time and sent as `Authorization: Bearer <value>`; the secret itself is never part of the registration, and any ref that is not `env:…` sends no Authorization header |
| `schema_template` | string | `generic-custom` (default), `openai-compatible`, `hermes-http-gateway` | picks how the outbound request is shaped and where the reply is read from; an unrecognised name falls back to `generic-custom` |
| `custom_schema` | object | `{"request_shape":{"method","headers","body"},"response_map"}` | wins over `schema_template` when present; `body` is a JSON template with `{{payload.x}}` / `{{crier.…}}` placeholders; `response_map` is `raw` (the whole response body) or a dot path such as `choices.0.message.content` |
| `delivery_mode` | string | `blocking`, `async` (default), `batch` | the agent's default mode; a delivery request can override it per message (§8.3) |
| `batch` | object | `{"max_messages","flush_interval_s"}` | batch mode only; a value > 0 wins over the server defaults (`CR_WEBHOOK_BATCH_MAX` = 10, `CR_WEBHOOK_BATCH_FLUSH_S` = 5s) |
| `retries` | integer | 0..10 | range-checked at registration; the redelivery budget for this endpoint. Absent or 0 uses the server budget `CR_WEBHOOK_MAX_RETRIES` (default 5); 1..10 is the budget for this endpoint; a value above the server setting is capped by it |
| `timeout_ms` | integer | 0..120000 | range-checked at registration; the blocking budget actually applied comes from the delivery request's own `timeout_ms` (default 30000, ceiling 120000) |

Registration is strict about its own vocabulary: an unknown or misnamed key
**inside the `webhook` object** — nested objects (`batch`, `custom_schema`,
`request_shape`) included — is a 400 that names the offending key and the keys
that are accepted. `"mode"` is not a key; the field is `delivery_mode`:

```bash
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"id":"oops","webhook":{"url":"http://127.0.0.1:9000/hook","mode":"blocking"}}'
# → 400 {"error":"webhook: unknown field \"mode\" (accepted: url, auth_type, auth_value_ref,
#          schema_template, custom_schema, delivery_mode, batch, retries, timeout_ms)"}

curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"id":"oops","webhook":{"url":"http://127.0.0.1:9000/hook","batch":{"max_msgs":5}}}'
# → 400 {"error":"webhook.batch: unknown field \"max_msgs\" (accepted: max_messages, flush_interval_s)"}
```

A refused `POST /agents` registers nothing, and a refused PATCH leaves the
agent's existing webhook untouched. This hold applies to the `webhook` object
only — the rest of the registration body is not held to it.

### 8.2 The three modes and their wire contracts

| mode | answer to the sender | what the endpoint receives | durable inbox |
|------|----------------------|----------------------------|----------------|
| `blocking` | 200 + the endpoint's inline `reply` | one envelope POST; the delivery waits for the answer | bypassed |
| `async` | 202 `{"id":…,"transport":"webhook","delivery_mode":"async"}` | one envelope POST from the background queue | bypassed |
| `batch` | 202 `{"id":…,"transport":"webhook","delivery_mode":"batch"}` | ONE coalesced POST of `{"messages":[envelope, …]}` when `max_messages` or `flush_interval_s` is reached | bypassed |

**`blocking`** — the sender gets the endpoint's reply inline (200):

```bash
curl -s -X POST localhost:8767/agents/blocking-agent/inbox "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"world"},"request_id":"r-1"}'
# → 200 {"id":"1fb5798c3e0fbf1c21d4a399","transport":"webhook",
#        "reply":{"echo":"sink-reply-1","choices":[{"message":{"content":"sink-content-1"}}]},
#        "request_id":"r-1"}
```

The endpoint received one POST of the Crier envelope (`generic-custom`
passthrough, `X-Crier-Event: message`):

```json
{"crier":{"version":1,"message_id":"1fb5798c3e0fbf1c21d4a399","request_id":"r-1",
          "delivery_mode":"blocking","target":"blocking-agent","kind":"message"},
 "payload":{"hello":"world"}}
```

The POST names both identities: `"target"` (and `X-Crier-Target` on the request)
is the agent the delivery is FOR — here `blocking-agent`, the agent in the
deliver URL — while `"sender"` (and `X-Crier-Agent`) is the agent it came from,
omitted from both when the delivery names no sender. One endpoint can serve
several agents, so the identity never has to be inferred from the URL.

`reply` is the endpoint's response body extracted per the schema:
`raw` hands back the whole body (above), `choices.0.message.content` hands back
just that string — the same endpoint with
`"schema_template":"hermes-http-gateway"` answers
`"reply":"sink-content-9"` and the endpoint receives the template-rendered body
`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hello-from-df150"}],"stream":false,…}`.

**`async`** — accepted immediately (202), pushed from the background queue:

```bash
curl -s -X POST localhost:8767/agents/async-agent/inbox "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"async"},"request_id":"r-2"}'
# → 202 {"id":"449b05958da0c2a5ef75d30d","transport":"webhook","delivery_mode":"async"}

# the message was PUSHED, not queued — the inbox stays empty:
curl -s "localhost:8767/agents/async-agent/inbox" "${AUTH[@]}"
# → 200 {"messages":[],"lease_id":"","queue_depth":0,"leased_count":0}
```

**`batch`** — accepted immediately (202), coalesced into one flush:

```bash
curl -s -X POST localhost:8767/agents/batch-agent/inbox "${AUTH[@]}" \
  -H 'Content-Type: application/json' -d '{"payload":{"n":1},"request_id":"r-3"}'
# → 202 {"id":"364186823e5afc7a39a3475b","transport":"webhook","delivery_mode":"batch"}
curl -s -X POST localhost:8767/agents/batch-agent/inbox "${AUTH[@]}" \
  -H 'Content-Type: application/json' -d '{"payload":{"n":2},"request_id":"r-4"}'
# → 202 {"id":"71fa491f0fcf4b7f0731ba81","transport":"webhook","delivery_mode":"batch"}
```

— with `"batch":{"max_messages":2,"flush_interval_s":1}` the endpoint then
receives ONE POST with `X-Crier-Event: batch`:

```json
{"messages":[
  {"crier":{"version":1,"message_id":"364186823e5afc7a39a3475b","request_id":"r-3","delivery_mode":"batch","kind":"message"},"payload":{"n":1}},
  {"crier":{"version":1,"message_id":"71fa491f0fcf4b7f0731ba81","request_id":"r-4","delivery_mode":"batch","kind":"message"},"payload":{"n":2}}]}
```

The batch wrapper is always that raw `{"messages":[…]}` array of envelopes —
schema templates shape the individual messages, not the batch. A single
message flushes on the timer alone (`"batch":{"flush_interval_s":1}` sends
`{"messages":[ … ]}` with one element after ~1s).

### 8.3 Per-message override

A delivery request may carry its own `delivery_mode`, which beats the agent's
registered default. It is validated against the same three values — anything
else is a 400 and nothing is dispatched:

```bash
# the agent's default is async, this delivery asks for blocking → 200 + inline reply
curl -s -X POST localhost:8767/agents/async-agent/inbox "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"override"},"delivery_mode":"blocking","request_id":"req-ovr-block"}'
# → 200 {"id":"0d8b9798cd6722c4bfc5d3ba","transport":"webhook","reply":{…},"request_id":"req-ovr-block"}

# the agent's default is blocking, this delivery asks for async → 202, RESOLVED mode echoed
curl -s -X POST localhost:8767/agents/blocking-agent/inbox "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"agent-default-blocking"},"delivery_mode":"async"}'
# → 202 {"id":"b2d9715db5c153e6034f98cd","transport":"webhook","delivery_mode":"async"}

# a value outside the set is refused before any dispatch or store
curl -s -X POST localhost:8767/agents/async-agent/inbox "${AUTH[@]}" \
  -H 'Content-Type: application/json' -d '{"payload":{"hello":"typo"},"delivery_mode":"sequential"}'
# → 400 {"error":"delivery_mode must be blocking|async|batch"}
```

The 202 accept body echoes the **resolved** mode (`async` or `batch`) whatever
the agent default was; a `blocking` delivery answers 200 with `transport` and
the inline `reply` instead, so it carries no `delivery_mode` field.

### 8.4 Retries, redelivery and federation

Retry/backoff, the offline queue for unreachable endpoints, the per-endpoint
circuit breaker, and relay-to-relay (federated) delivery are specified in
`specs/WEBHOOK-DELIVERY.md` and `docs/specs.md` — this section covers the
registration object and the wire contracts only.

---

## 9. External non-Go consumers: the durable inbox pull path

Everything in §2 and §3 is plain HTTP plus `openssl`: no Crier SDK, no Go
toolchain and no WebSocket client are involved. That makes the durable inbox the
one path an external harness — a Python, TypeScript or DB-native agent such as
Consensus — can adopt without writing any Go, and this section is that path end
to end. Every leg below was measured live against a scratch-port server in
config C, and the same round-trip is re-run on every commit by
`scripts/e2e-battery.sh` (the *foreign-agent inbox isolation* cell).

### 9.1 Pull from the durable inbox — do not subscribe for delivery

**An external, long-lived consumer should use the durable inbox: `POST
/agents/{id}/inbox` in, `GET /agents/{id}/inbox` out, `POST
/agents/{id}/inbox/ack` when it is done. It is pull-based, the server holds the
message until the consumer acks it, and it therefore survives consumer
downtime. The relay (`POST /relay/publish` / `GET /relay/subscribe/{topic}`,
§4) is for live push to a consumer that is already connected — it is a fan-out,
not a mailbox.**

Two reasons, both measured live on a scratch server (transcripts in §9.3):

- **The relay DROPS an event that is published while nobody is subscribed, and
  never replays it.** A publish with zero subscribers answers `202` and the
  event is gone: the subscriber that connected one second later waited 5s on
  that very topic and received no frame at all. An external consumer has no way
  to learn what it missed — a restart, a redeploy or even a reconnect window
  between subscribe and publish is a hole in its history. The inbox has no such
  window: a delivery is stored until it is acked, so downtime costs latency,
  never messages.
- **The inbox has no subscribe-before-publish race.** Relay push only reaches a
  consumer whose subscription is already established, so the producer and the
  consumer must be ordered correctly for every message. The inbox requires
  nothing of the consumer at delivery time: it can start, crash and restart in
  any order relative to the producer, and an unacked message reappears after its
  lease expires (30s by default) — the *same* message id, not a copy.

Prefer the relay when the consumer is already connected and best-effort live
push is what you want (a dashboard, a co-resident process, §4).

One more reason not to route an external producer at the webhook path (an agent
registered with a `webhook`, §8): the `openai-compatible` schema template reads
only `payload.text`, so any other payload shape fails the delivery loudly
(DF-CRIER-279: the request-body render errors, naming the missing path
`payload.text`, and nothing is POSTed — before that fix the same shape sent an
empty content body and was still reported as a successful delivery). A producer
following this section's payloads would not be delivered at all. Inbox pull has
no shaper between producer and consumer.

### 9.2 The recipe (config C — the production default)

The three auth configurations are the ones defined in §1. Which tier each step
needs — this is the whole authorization surface of the pull path:

| Step | Endpoint | Config A (open) | Config B (bearer) | Config C (bearer + signature) |
|------|----------|-----------------|-------------------|-------------------------------|
| Register the consumer | `POST /agents` | open | bearer | bearer — **no signature** |
| Deliver into the inbox | `POST /agents/{id}/inbox` | open | bearer | bearer — **no signature** |
| Retrieve (leases the batch) | `GET /agents/{id}/inbox` | open | bearer | bearer **+ signature** |
| Ack the lease | `POST /agents/{id}/inbox/ack` | open | bearer | bearer **+ signature** |
| Drain check (optional) | `GET /agents/{id}/inbox/stats` | open | bearer | bearer **+ signature** |

Only the consumer's OWN steps are signed, and only in config C: **register and
deliver need no signature in any configuration**, which is what makes the inbox
a mailbox — any registered producer may deliver into it. The signed steps are
exactly the agent-owned ones (`retrieve`, `ack`, `stats`, plus `DELETE
/agents/{id}` and `PATCH /agents/{id}`), and the caller in `X-Agent-ID` must be
the agent in the path: another agent's id is refused with `403`, not `401` (§9.3).

```bash
# One identity, one keypair — OpenSSL >= 3, the same key format as §2 and §3.
PORT=8767
AGENT=consensus
AUTH=(-H "Authorization: Bearer $CR_AUTH_TOKEN")   # drop this in config A
openssl genpkey -algorithm ED25519 -out agent.key
PUBKEY=$(openssl pkey -in agent.key -pubout -outform DER | tail -c 32 | xxd -p -c 64)

# 1. register the consumer with its public key                              # → 201
curl -s -X POST localhost:$PORT/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$AGENT\",\"public_key\":\"$PUBKEY\",\"capabilities\":[\"inbox\"]}"
# → 201 {"id":"consensus","public_key":"33acb5…","capabilities":["inbox"],"status":"online",
#        "registered_at":"…","last_seen":"…"}

# 2. deliver a message INTO its inbox — no signature, any registered producer  # → 201
curl -s -X POST localhost:$PORT/agents/$AGENT/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"payload":{"round_trip":"cr-consensus-1","text":"hello from an external non-Go harness"}}'
# → 201 {"id":"b619a256231e4fa5aeb00d82","transport":"inbox",
#        "expires_at":"2026-09-26T04:06:23.868749589Z"}
# (the payload is stored VERBATIM, wrapped in nothing — see §9.3 for the
#  round-trip proof: the retrieved payload base64-decodes to those exact bytes)

# 3. signed retrieve — leases the batch for 30s by default                     # → 200
TS=$(date +%s)
printf 'GET\n/agents/consensus/inbox\n%s' "$TS" > payload.txt
SIG=$(sig)   # helper from §3 Retrieve — fails loudly on OpenSSL < 3
curl -s localhost:$PORT/agents/$AGENT/inbox "${AUTH[@]}" \
  -H "X-Agent-ID: $AGENT" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG"
# → 200 {"messages":[{"id":"b619a256231e4fa5aeb00d82","agent_id":"consensus",
#         "payload":"eyJyb3VuZF90cmlwIjoiY3ItY29uc2Vuc3VzLTEiLCJ0ZXh0IjoiaGVsbG8g…",
#         "created_at":"2026-09-25T04:06:23.868749589Z","leased_at":"2026-09-24T23:06:23.902707318-05:00",
#         "lease_id":"cd981baea0533ab459186c8026e8be2e","acked":false,
#         "expires_at":"2026-09-26T04:06:23.868749589Z"}],
#        "lease_id":"cd981baea0533ab459186c8026e8be2e","queue_depth":1,"leased_count":1}

# 4. signed ack — only ids that came back with THAT lease_id                   # → 204
TS=$(date +%s)
printf 'POST\n/agents/consensus/inbox/ack\n%s' "$TS" > payload.txt
SIG=$(sig)   # helper from §3 Retrieve
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:$PORT/agents/$AGENT/inbox/ack "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -H "X-Agent-ID: $AGENT" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG" \
  -d '{"lease_id":"cd981baea0533ab459186c8026e8be2e","message_ids":["b619a256231e4fa5aeb00d82"]}'
# → 204
```

The signing helper is §3's `sig()`, verbatim — define it once before step 3:

```bash
sig() { if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then echo "ERROR: this signing helper requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)" >&2; return 1; fi; _sig=$(openssl pkeyutl -sign -rawin -inkey agent.key -in payload.txt 2>/dev/null | xxd -p -c 128); if [ -z "$_sig" ]; then echo "ERROR: signing produced an EMPTY signature. pkeyutl -sign -rawin is a one-shot operation and needs a SEEKABLE payload passed with -in <file> — a piped or redirected payload fails with 'unable to determine file size for oneshot operation' and yields zero bytes, which the server rejects 401 naming the empty X-Agent-Sig header." >&2; return 1; fi; printf '%s\n' "$_sig"; }
```

The payload the signature covers is `METHOD\n<path>\n<unix-seconds>` for the
agent in the path — the same scheme as §3, and the signature is bound to both
the method and the path, so a signature minted for the retrieve cannot be
replayed on the ack. The ack rules (§3) apply unchanged: a lease-only ack is
`400`, an unknown message id is `404`, and a stale lease is `409`.

An external consumer's loop is therefore: `GET /agents/{id}/inbox` → process
each message's base64-decoded `payload` → `POST /agents/{id}/inbox/ack` with
the `lease_id` and the ids from the same response. Anything the consumer does
not ack (a crash mid-batch, a process that dies) is redelivered when the lease
expires — no message loss, no subscribe-before-publish ordering requirement.

### 9.3 What was measured (scratch server, config C)

**The relay drop.** A publish to a topic with ZERO subscribers answers `202` and
the event is discarded — a later subscriber never sees it:

```bash
# publish with nobody subscribed → 202 (and GET /relay/topics → 200 {"topics":[]})
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:$PORT/relay/publish "${AUTH[@]}" \
  -H 'Content-Type: application/json' -H 'X-Agent-ID: consensus' \
  -d '{"topic":"consensus.live.drop","event":{"n":1}}'
# → 202

# a subscriber connects LATE on that topic and waits 5 seconds:
#   no frame within 5s -> the pre-subscription publish was DROPPED (never replayed)
# ... and the next publish, made while it IS connected, arrives immediately:
#   {"topic": "consensus.live.drop", "event": {"n": 2}}
```

**The inbox redelivery.** An unacked message comes back after its lease
expires — the same message id, under a fresh lease:

```bash
# deliver, then retrieve with a 3s lease, then do NOT ack:
#   retrieve #1 → 200  messages[0].id=9d72ab31…  lease_id=04e205c5…
#   retrieve #2 immediately → 200 {"messages":[],"lease_id":"","queue_depth":1,"leased_count":1}
#     (leased_count:1 is the tell documented in §3 — the message is HELD, not lost)
#   sleep 5   # the 3s lease expires
#   retrieve #3 → 200  messages[0].id=9d72ab31…  lease_id=f6b01976…   ← SAME id, redelivered

# and the cross-agent negative, same server: B (its own keypair, registered)
# signs a retrieve of A's inbox with its OWN headers → 403
#   {"error":"agent \"consensus-b\" may only access its own resources (target \"consensus\")"}
# while B's own inbox answers 200 {"messages":[],"lease_id":"","queue_depth":0,"leased_count":0}
```

**The payload round-trip.** `payload` is base64 of the delivered bytes; this
was compared byte-for-byte against the delivered string, and it matched:

```bash
# delivered:      {"round_trip":"cr-consensus-1","text":"hello from an external non-Go harness"}
# base64-decoded: {"round_trip":"cr-consensus-1","text":"hello from an external non-Go harness"}
# ROUND-TRIP: INTACT
```

---

## 10. Using crier from Muster

Muster is an external platform that generates an HTTP client from an OpenAPI
document. Crier ships that document, so a Muster-driven integration is built from
the spec rather than from an SDK — no Go toolchain, no WebSocket client and no
vendored library are involved. This section is that onboarding path end to end:
discover the spec, register an agent, deliver into its inbox, retrieve and ack.
§10.3 is the half a generated client cannot do for itself: the per-agent ed25519
signature.

Everything below is real and every status shown was measured live against a relay
built and started from this checkout in config C (the production default). The
same run is reproducible with the shipped runner in §10.4, which asserts each of
those statuses and fails when one of them changes.

### 10.1 Discover the spec: the served copy and the repo source of truth

Two copies of the document exist and they are byte-identical (asserted by
`TestOpenAPIDocsSpec` and by CI):

| Copy | Where | How to fetch |
|------|-------|--------------|
| served | `GET /openapi.json` and `GET /openapi.yaml` on any running relay | both are among the five auth-exempt paths (§1), so a spec fetch needs no token |
| repo | `docs/openapi.yaml` | the source of truth in a checkout; `cmd/server/openapi.yaml` is the generated copy (`make generate`) the server embeds and serves |

Point the generator at whichever copy fits: `GET /openapi.json` for a JSON
toolchain and `GET /openapi.yaml` for a YAML one, or `docs/openapi.yaml` for a
pinned checkout. Measured with no `Authorization` header at all:

```bash
curl -s localhost:8767/openapi.json | head -c 60        # → {"openapi":"3.1.0","info"…
curl -s -o /dev/null -w '%{http_code}\n' localhost:8767/openapi.yaml   # → 200
```

`servers[0]` in the document is the local-development default
`http://localhost:8767`; override it with the relay's real base URL (see the
"Muster-generated clients" note at the top of this guide).

### 10.2 The round trip: register, deliver, retrieve, ack

The consumer is one identity with one keypair — the same key format as §2. A
config C relay (the production default) requires the public half at registration
and refuses a registration without it, while the writes themselves need no
signature at all:

```bash
AGENT=muster-bridge
AUTH=(-H "Authorization: Bearer ${CR_AUTH_TOKEN:-}")     # drop this in config A

# register with the PUBLIC half of the agent's key                            # → 201
openssl genpkey -algorithm ED25519 -out agent.key
PUBKEY=$(openssl pkey -in agent.key -pubout -outform DER | tail -c 32 | xxd -p -c 64)
curl -s -X POST localhost:8767/agents "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$AGENT\",\"public_key\":\"$PUBKEY\",\"capabilities\":[\"inbox\"]}"
# → 201 {"id":"muster-bridge","public_key":"3622469f…","capabilities":["inbox"],"status":"online",…}
# (no public_key → 400 {"error":"public_key is required"}: with enforcement on,
#  the key IS the identity — a client that omits the field is not registered)

# deliver — bearer only, NO signature, in every configuration                  # → 201
curl -s -X POST localhost:8767/agents/$AGENT/inbox "${AUTH[@]}" -H 'Content-Type: application/json' \
  -d '{"payload":{"source":"muster","text":"hello from a Muster-driven integration"}}'
# → 201 {"id":"816583122b3815b05790b1c4","transport":"inbox","expires_at":"2026-09-26T06:02:26.719729018Z"}

# retrieve — SIGNED in config C: the unsigned read is refused here              # → 401
curl -s localhost:8767/agents/$AGENT/inbox "${AUTH[@]}"
# → 401 {"error":"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"}
```

That 401 is the boundary a generated client runs into (§10.3). The signed legs
use §3's `sig()` helper verbatim — OpenSSL ≥ 3, the same
`hex(ed25519("METHOD\n<path>\n<unix-seconds>"))` the guide documents there — and
the ack rules of §3 apply unchanged:

```bash
# retrieve, signed: leases the batch for 30s by default                        # → 200
TS=$(date +%s)
printf 'GET\n/agents/%s/inbox\n%s' "$AGENT" "$TS" > payload.txt
SIG=$(sig)   # helper from §3 Retrieve — fails loudly on OpenSSL < 3
curl -s localhost:8767/agents/$AGENT/inbox "${AUTH[@]}" \
  -H "X-Agent-ID: $AGENT" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG"
# → 200 {"messages":[…],"lease_id":"b1e9bbaef27a01f0f17c8d6fd45160f7","queue_depth":2,"leased_count":2}

# ack with THAT lease_id and the ids from that response                        # → 204
TS=$(date +%s)
printf 'POST\n/agents/%s/inbox/ack\n%s' "$AGENT" "$TS" > payload.txt
SIG=$(sig)
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8767/agents/$AGENT/inbox/ack "${AUTH[@]}" \
  -H 'Content-Type: application/json' \
  -H "X-Agent-ID: $AGENT" -H "X-Agent-Ts: $TS" -H "X-Agent-Sig: $SIG" \
  -d '{"lease_id":"b1e9bbaef27a01f0f17c8d6fd45160f7","message_ids":["3810068b0517f0394413be49","816583122b3815b05790b1c4"]}'
# → 204
```

### 10.3 The auth reality: what a bearer-only generated client can and cannot drive

**crier declares `bearerAuth` globally and `agentSignature` per operation.** The
document-level `security` is `- bearerAuth: []`, so a generated client applies the
bearer token to every operation; `components.securitySchemes` adds
`agentSignature`, an apiKey-style header (`X-Agent-Sig`) whose value is the §3
signature over the method, the raw path and a unix timestamp, signed with the
agent's **private** key. A client generated from the spec can send that header —
it cannot compute it: it holds no private key, and the server binds the value to
the method, the path, the timestamp and the `X-Agent-ID`.

Five operations declare `agentSignature`. Everything else is drivable by a
generated (bearer-only) client, and these five are not. Measured on a config C
relay, all with `Authorization: Bearer <token>` and no signature headers:

| Endpoint | Spec security | Unsigned (bearer only) | With the signature |
|----------|---------------|------------------------|--------------------|
| `GET /health`, `GET /version`, `GET /openapi.json`, `GET /openapi.yaml`, `GET /docs` | `security: []` | `200` — no token needed at all | — |
| `GET /status` | bearerAuth | `200` (`401` without the token) | — |
| `POST /agents` | bearerAuth | `201` (`400` without `public_key`) | — |
| `GET /agents` | bearerAuth | `200` (`401` without the token) | — |
| `GET /agents/{id}` | bearerAuth | `200` | — |
| `POST /agents/{id}/inbox` (deliver) | bearerAuth | `201` — **no signature in any configuration** | — |
| `GET /relay/topics` | bearerAuth | `200` | — |
| `POST /relay/publish` | bearerAuth | `202` | — |
| `GET /mesh/peers` | bearerAuth | `200` | — |
| `GET /fed/peers` | bearerAuth | `200` | — |
| `GET /agents/{id}/inbox` (retrieve) | bearerAuth + agentSignature | `401` | `200` |
| `GET /agents/{id}/inbox/stats` | bearerAuth + agentSignature | `401` | `200` |
| `POST /agents/{id}/inbox/ack` | bearerAuth + agentSignature | `401` | `204` |
| `PATCH /agents/{id}` | bearerAuth + agentSignature | `401` | `200` |
| `DELETE /agents/{id}` | bearerAuth + agentSignature | `401` | `204` |

The refusal is the same for every one of the five:
`{"error":"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"}`.
A complete but wrong trio is refused too — a timestamp outside ±30s, a signature
minted for another path, or a signature presented as another agent are all
refused (`401`, except the last: a *valid* signature whose `X-Agent-ID` is not the
`{id}` in the path answers `403`, `may only access its own resources`).

**The spec and the live server agree on this table**, including
`GET /agents/{id}/inbox/stats`: the document marks that operation with
`agentSignature` **and** the running relay refuses an unsigned call with `401`
(and answers `200` once the signature is supplied). An earlier internal note
believed the stats read was signature-free — it is not, and the row above is the
measurement that settles it.

Two consequences worth stating plainly:

- **Signature-required endpoints are not reachable from a generated client.**
  Drive them from your own application code with §3's helper (or from a
  hand-written client), and keep the generated client on the operations in the
  `bearerAuth`-only rows.
- **Config B is the configuration a generated client drives end to end**
  (`CR_REQUIRE_AGENT_SIG=false`): the same five operations then answer `200`,
  `200`, `404` (a bogus message id — that is the ack's own validation, not the
  auth gate), `200` and `204` unsigned, and a bogus signature trio is ignored
  rather than rejected. Register `201` → deliver `201` → retrieve `200` → ack
  `204` is the whole path, with nothing to sign.

### 10.4 The runnable example

`examples/muster-bridge/run-demo.sh` walks §10.1–§10.3 against a relay it builds
and starts itself on a scratch port it proves free **and proves it owns**
(CR-GAP-069: the `EXIT` trap kills the pid it started, and the port's holder pid
is asserted to be that pid). The transcript goes to a temp file outside the repo.
The run pasted below is that script: its scratch port was `18801` (the port
guard's first candidate, chosen because it was free), while §10.1–§10.2 show the
guide's documented port `8767` — same binary, same configuration, same statuses.

```bash
bash examples/muster-bridge/run-demo.sh                                   # config C
MUSTER_BRIDGE_REQUIRE_SIG=false bash examples/muster-bridge/run-demo.sh   # config B
```

Every line it prints is an assertion: the run fails if a status moves. Its last
step is the §10.3 matrix, measured — this is the config C output:

```text
==> [4/9] register the consumer — bearer token only, NO signature (201)
    POST /agents (bearer, no signature)            -> 201
    {"id":"muster-bridge","public_key":"3622469f03b00c5f0a5aa70f00d1eced7ece90265c96f2377171a0140a59c78e","capabilities":["inbox"],"status":"online","registered_at":"2026-09-25T01:02:26.683608904-05:00","last_seen":"2026-09-25T01:02:26.683608904-05:00"}
    POST /agents without public_key (config C)     -> 400
    {"error":"public_key is required"}

==> [5/9] deliver into its inbox — bearer token only, NO signature (201, 201)
    POST /agents/muster-bridge/inbox #1 (no signature) -> 201
    POST /agents/muster-bridge/inbox #2 (no signature) -> 201
    {"id":"816583122b3815b05790b1c4","transport":"inbox","expires_at":"2026-09-26T06:02:26.719729018Z"}
    (the inbox is durable and pull-based: a delivery is stored until it is acked)

==> [6/9] retrieve — GET /agents/muster-bridge/inbox
    unsigned retrieve (config C)                   -> 401
    {"error":"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"}
    ^ this is the boundary for a spec-generated (bearer-only) client.

==> [7/9] sign the request — the leg no generated client can do
    signed retrieve                                -> 200
    signed as: hex(ed25519("GET\n/agents/muster-bridge/inbox\n1790316146"), agent.key)
    lease_id=b1e9bbaef27a01f0f17c8d6fd45160f7  message_ids=["3810068b0517f0394413be49","816583122b3815b05790b1c4"]
    envelope: "queue_depth":2,"leased_count":2
    (a signature is bound to METHOD and path: the same key signing a
     different path is refused 401, and X-Agent-ID must be the {id} in
     the path or the request is refused 403.)

==> [8/9] ack the lease — 204, and the inbox is empty again
    signed ack (POST /agents/muster-bridge/inbox/ack) -> 204
    signed drain check (GET /agents/muster-bridge/inbox/stats) -> 200

==> [9/9] the MEASURED authorization matrix (INT-MUSTER-003)
    bearer-only client — every request carries Authorization: Bearer <token> and NO
    X-Agent-ID / X-Agent-Ts / X-Agent-Sig unless the line says otherwise.

    the five signature-required operations, unsigned (DELETE is last, below):
    GET  /agents/{id}/inbox (unsigned)             -> 401
    GET  /agents/{id}/inbox/stats (unsigned)       -> 401
    POST /agents/{id}/inbox/ack (unsigned, dummy ids) -> 401
    PATCH  /agents/{id} (unsigned)                 -> 401

    everything else a bearer-only client drives:
    GET /health (no token)                         -> 200
    GET /version (no token)                        -> 200
    GET /docs (no token)                           -> 200
    GET /status (bearer)                           -> 200
    GET /status (no token -> refused)              -> 401
    GET /agents (bearer)                           -> 200
    GET /agents (no token -> refused)              -> 401
    GET /agents/{id} (bearer)                      -> 200
    POST /agents (bearer, new id + public_key)     -> 201
    POST /agents/{id}/inbox (bearer, deliver)      -> 201
    GET /relay/topics (bearer)                     -> 200
    GET /mesh/peers (bearer)                       -> 200
    GET /fed/peers (bearer)                        -> 200
    POST /relay/publish (bearer + X-Agent-ID)      -> 202

    DELETE /agents/{id} (unsigned, the 5th signed op) -> 401

==> summary
    PASS — every measured status matched its claimed code (config C, CR_REQUIRE_AGENT_SIG=true)
```

The runner's README (`examples/muster-bridge/README.md`) carries the same matrix,
the environment knobs (`MUSTER_BRIDGE_REQUIRE_SIG`, the token, the port block,
the 18801+ scratch-port rotation) and the cleanup contract.

