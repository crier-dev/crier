# Tester Guide

Thanks for testing crier. This file tells you what to poke, what's already
known-broken (so you don't waste time), and how to report so your findings
are actionable. Time budget: **~30 minutes for the full pass**, less if you
only care about one mode.

Everything below runs against the **default configuration**: per-agent request
signatures are ON (`CR_REQUIRE_AGENT_SIG=true`). The LLM message guard is the
only thing switched off in §1, so a keyless run is deterministic. If a step
answers `401`, you are missing the three signature headers — **§2.0a** is the
fastest way to get them (`crier keygen` plus a client library that signs for
you), and **§2.0b** builds the keypair by hand if you would rather see the wire.

## 1. Get it running (5 minutes)

```bash
git clone https://github.com/crier-dev/crier.git && cd crier
make build
CR_GUARD_ENABLED=false ./bin/crier -port 8767 -pidfile .crier.pid &
```

The `-pidfile` matters: it is what pairs this start with `make stop` below.
Shared host? Port 8767 may already be taken by someone else's server. Check
first (`ss -tlnp | grep :8767` — empty output means free); if it is held,
start on a free port instead (`./bin/crier -port 8768 -pidfile .crier.pid` —
a failed bind now names the port and the holder-check command) and confirm
the build that answered with `curl -s localhost:8768/version`.

Keep that server running — every exercise below assumes `localhost:8767`.
Stop or restart it at any time with `make stop`: it reads the same `.crier.pid`
the start line above wrote, sends that pid one SIGTERM, waits for the port to
be released, and removes the file (no pidfile present — you never started
one — it just reports "nothing to stop"). If the launcher is lost (started
with `&`, then the shell closed), `make stop` still works — the pidfile names
the server process, not the launcher. A server started without `-pidfile` is
stopped manually: `ss -tlnp | grep :8767`, then `kill <pid>` (SIGTERM is the
graceful path).
Anything you send stays on your machine; crier phones home to **nothing**.

Confirm it is up. `$BASE` is just the address you started it on; `make
docs-check` executes this same block against its own throwaway server, so the
verdict comments below are load-bearing. `/health` answers `{"status":"ok"}` and
`/version` answers the build identity (`{"version":"...","commit":"..."}`).

<!-- doccheck -->
```bash
BASE=http://localhost:8767
curl -s $BASE/health     # -> 200
curl -s $BASE/version    # -> 200
```

Prefer containers? `docker` — see `examples/agent-ecosystem/` for a compose
stack. Prefer a browser? `GET /docs` serves a **static** HTML index of the API
contract: it links the machine-readable spec at `/openapi.json` and
`/openapi.yaml`, and it is *not* an interactive request console — fire your
requests with curl (or any HTTP client).

## 2. Eight ways to move a message (~20 minutes)

A reference harness that exercises all of these automatically lives in
`scripts/bunker-matrix.sh`; the manual curl versions below are the same
probes it runs.

The demo harnesses under `examples/` are guarded the same way this file tells you
to check a port: they refuse to run while anything already listens on their
scratch port (naming the holder's pid, its command line and the
`ss -tlnp | grep :<port>` audit command), assert after `/health` that the
listening process is the one they started, and abort if that process dies — so a
demo pass can never have been measured against someone else's server.
`make port-guard-selftest` exercises all three guards on a free port it picks
itself.

The two llm-mesh lanes do not merely refuse: their scratch port is chosen from a
bounded candidate list, and a candidate something else already holds is SKIPPED
with its holder named (pid, command line, audit command) instead of making the
probe skip — a long-lived unrelated listener on the first candidate, or a
squatter that took it between two runs, just moves the run along. An explicit
`CRIER_PORT` is the exception: it is checked and fails closed, never rotated,
and an exhausted candidate list fails with every attempted port and its holder
listed. `make port-guard-selftest` covers that rotation too (arms D–F).

**0. Register two agents** (every exercise needs them). Signatures are ON by
default, so an agent is only reachable with the private key whose public half it
was registered with — **keep the private key**, a public key on its own cannot
sign anything.

**0a. The no-ceremony path — `crier keygen` plus a client library.** If you are
here to exercise crier rather than to inspect its wire format, start here: no
openssl, no xxd, nothing to install.

```bash
# Build once (or use the binary you already have). `crier keygen` writes an
# ed25519 keypair as PKCS#8 PEM at mode 0600 and prints the agent config.
go build -o bin/crier ./cmd/server
./bin/crier keygen -out alice.key -id alice    # -> alice.key + the exact POST /agents body
./bin/crier keygen -out bob.key   -id bob      # a second identity, when you need one
```

Then, with the server up (`make run`), the Python client completes the whole
signed round-trip — register, deliver, signed retrieve, signed ack, signed
stats, relay publish/subscribe — and prints what it asked for and what came
back. The last two steps are negative controls: the same retrieve with no
signature, and the same retrieve signed with a different key, must both be
refused. If either one succeeds, the run says so instead of reporting a pass.

```bash
python3 clients/python/round_trip.py --server ${BASE} --id alice --key alice.key
```

The TypeScript client does the same (Node 22.6+; nothing to install):

```bash
node clients/typescript/round-trip.ts --server ${BASE} --id alice --key alice.key
```

Two identities on one bus — alice delivers to bob, bob's key reads and acks it,
and alice's key is refused (403) when it targets bob's inbox:

```bash
python3 clients/python/two_agents.py --server ${BASE} \
  --alice-id alice --alice-key alice.key --bob-id bob --bob-key bob.key
```

The two clients are documented in `clients/README.md` (method surface, signing
contract, requirements). Everything from §0b on is the same round-trip by hand:
it is the protocol reference, it is worth reading, and you do not need it to get
a message across.

**0b. The raw path — keys with openssl, signatures with your own helper.** Use
this when you want to see exactly what goes on the wire (or to sign from a
language that has no client yet). One ed25519 keypair per agent:

```bash
BASE=${BASE:-http://localhost:8767}

# One ed25519 keypair per agent (openssl 3.x + xxd). A fresh box usually has no
# xxd, and the register call below then 400s on an empty pubkey — the README's
# "Installing xxd without root" recipe gets it with no root at all.
# (Or write the same PKCS#8 key with `./bin/crier keygen -out /tmp/crier-alice.key -id alice`
# and skip openssl entirely — the helper below reads either key file.)
openssl genpkey -algorithm ED25519 -out /tmp/crier-alice.key >/dev/null 2>&1
openssl genpkey -algorithm ED25519 -out /tmp/crier-bob.key   >/dev/null 2>&1
ALICE_PUB=$(openssl pkey -in /tmp/crier-alice.key -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)
BOB_PUB=$(openssl pkey -in /tmp/crier-bob.key   -pubout -outform DER 2>/dev/null | tail -c 32 | xxd -p -c 64)

# sig(): the README Quick Start signing helper, with the key file lifted to the
# first argument so alice and bob can each sign with their own key.
# Hex-encodes ed25519_sign("<METHOD>\n<PATH>\n<unix-seconds>"). Requires OpenSSL
# >= 3 for `pkeyutl -sign -rawin`: on older OpenSSL it errors loudly instead of
# producing an empty (silently-401) signature. The payload is written to a FILE
# and signed with -in — piping it into openssl yields an empty signature and a
# misleading 401 (DF-CRIER-2). `-rawin` is a ONE-SHOT operation: it needs that
# seekable file, so the helper refuses a zero-byte signature loudly (rather than
# sending it and getting a 401 whose message names the empty X-Agent-Sig header).
sig() { if ! openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then echo "ERROR: this signing helper requires OpenSSL >= 3 (pkeyutl -sign -rawin); found $(openssl version)" >&2; return 1; fi; printf '%s\n%s\n%s' "$2" "$3" "$4" > /tmp/crier-payload.txt; _sig=$(openssl pkeyutl -sign -rawin -inkey "$1" -in /tmp/crier-payload.txt 2>/dev/null | xxd -p -c 128); if [ -z "$_sig" ]; then echo "ERROR: signing produced an EMPTY signature. pkeyutl -sign -rawin is a one-shot operation and needs a SEEKABLE payload passed with -in <file> — a piped or redirected payload fails with 'unable to determine file size for oneshot operation' and yields zero bytes, which the server rejects 401 naming the empty X-Agent-Sig header." >&2; return 1; fi; printf '%s\n' "$_sig"; }

# Register both agents with the public half of their own keypair.
curl -s -X POST $BASE/agents -H 'Content-Type: application/json' \
  -d "{\"id\":\"alice\",\"public_key\":\"${ALICE_PUB}\"}"          # -> 201
curl -s -X POST $BASE/agents -H 'Content-Type: application/json' \
  -d "{\"id\":\"bob\",\"public_key\":\"${BOB_PUB}\"}"              # -> 201
curl -s $BASE/agents                                             # -> 200, both ids listed
```

**1. Inbox (store-and-forward)** — deliver to bob, read it back **signed**:

```bash
# Deliver. bob's identity is not needed to send to him; anyone may deliver.
curl -s -X POST $BASE/agents/bob/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"bob"}}'
# -> 201 {"id":"...","transport":"inbox","expires_at":"..."}

# Retrieve — the gated op. Three headers are required, all derived from bob's key:
#   X-Agent-ID   the agent id (must be the one the key was registered under)
#   X-Agent-Ts   unix seconds, within ±30s of the server clock (never hardcode it)
#   X-Agent-Sig  hex ed25519 signature over "GET\n/agents/bob/inbox\n<ts>"
TS=$(date +%s)
curl -s $BASE/agents/bob/inbox \
  -H "X-Agent-ID: bob" -H "X-Agent-Ts: ${TS}" \
  -H "X-Agent-Sig: $(sig /tmp/crier-bob.key GET /agents/bob/inbox "$TS")"
# -> 200 {"messages":[{"id":"...","payload":"eyJoZWxsbyI6ImJvYiJ9","lease_id":"..."}],"lease_id":"..."}

# payloads travel BASE64-encoded ([],byte on the wire) — decode one:
python3 -c 'import base64,sys;print(base64.b64decode(sys.argv[1]).decode())' 'eyJoZWxsbyI6ImJvYiJ9'
# -> {"hello":"bob"}

# Ack — permanently removes the message (an ack with no message_ids is 400, and a
# retrieved-but-unacked message is redelivered once its 30s lease expires):
LEASE_ID=<lease_id from the retrieve>
MSG_ID=<id from the retrieve>
TS=$(date +%s)
curl -s -X POST $BASE/agents/bob/inbox/ack -H 'Content-Type: application/json' \
  -H "X-Agent-ID: bob" -H "X-Agent-Ts: ${TS}" \
  -H "X-Agent-Sig: $(sig /tmp/crier-bob.key POST /agents/bob/inbox/ack "$TS")" \
  -d "{\"lease_id\":\"${LEASE_ID}\",\"message_ids\":[\"${MSG_ID}\"]}"
# -> 204

# A delivery may carry an explicit lifetime. ttl_seconds is honoured; 0 means
# never expires, which the wire reports as "expires_at":null (the key is
# present, the value is null — not the zero time 0001-01-01T00:00:00Z):
curl -s -X POST $BASE/agents/bob/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"ping":1},"ttl_seconds":3600}'
# -> 201 with expires_at = now + 3600s
curl -s -X POST $BASE/agents/bob/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"ping":2},"ttl_seconds":0}'
# -> 201 {"id":"...","transport":"inbox","expires_at":null}
```

The marked block below is executed by `make docs-check` against its own server:

<!-- doccheck -->
```bash
curl -s -X POST $BASE/agents/bob/inbox -H 'Content-Type: application/json' -d '{"payload":{"never":1},"ttl_seconds":0}'   # -> 201
```


The retrieve above is the one **intentional negative** in this guide: drop the
headers and the default configuration rejects the call with
`{"error":"missing agent signature headers (X-Agent-ID, X-Agent-Ts, X-Agent-Sig)"}`.
This block is executed by `make docs-check` against its own server.

<!-- doccheck -->
```bash
curl -s $BASE/agents           # -> 200
curl -s $BASE/agents/bob/inbox # -> 401
```

**2. Relay pub/sub** — fan-out to live subscribers (topic patterns support `*`
for exactly one segment and a terminal `>` for one-or-more):

```bash
# terminal A: subscribe (leave running)
python3 - <<'EOF'
import socket
s = socket.create_connection(("localhost", 8767))
s.sendall(b"GET /relay/subscribe/demo-topic HTTP/1.1\r\nHost: localhost:8767\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
print(s.recv(4096)[:64])   # 101 -> subscribed
print(s.recv(4096))        # blocks until a publish arrives
EOF
```

The publish half is executed by `make docs-check`; terminal A prints the frame it
receives.

<!-- doccheck -->
```bash
curl -s -X POST $BASE/relay/publish -H 'Content-Type: application/json' -H 'X-Agent-ID: alice' -d '{"topic":"demo-topic","event":{"msg":"hi subscribers"}}'   # -> 202
```

`X-Agent-ID` is required on a publish — the rate limiter keys on it (a publish
without it is `401`). Terminal A prints the frame. Subscribe to `alerts.*`
instead and publish to `alerts.fire` and it still arrives; `alerts.fire.deep`
only matches `alerts.>`.

**A successful subscribe sends no frames until the first published event.** The
upgrade answers `101` and that is the whole handshake: the server sends no
welcome/hello frame, no subscription acknowledgement and no keepalive on the relay
socket, so terminal A's `recv` after the `101` blocks exactly until a publish fans
out — that silence is the designed state, not a broken subscription. To confirm a
subscription is live, check `GET /relay/topics` (a topic appears there while at
least one subscriber is connected) or publish to it and read the event back.

**3. Webhook delivery (blocking + async)** — crier calls YOUR http server:

```bash
# terminal A: a 3-line echo sink
python3 -c '
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.send_response(200); self.end_headers(); self.wfile.write(b"{\"echo\":true}")
HTTPServer(("127.0.0.1", 9911), H).serve_forever()' &
# terminal B: point bob at it, then deliver. PATCH is a gated op — bob signs it.
TS=$(date +%s)
curl -s -X PATCH $BASE/agents/bob -H 'Content-Type: application/json' \
  -H "X-Agent-ID: bob" -H "X-Agent-Ts: ${TS}" \
  -H "X-Agent-Sig: $(sig /tmp/crier-bob.key PATCH /agents/bob "$TS")" \
  -d '{"webhook":{"url":"http://127.0.0.1:9911/hook","delivery_mode":"blocking"}}'
# -> 200 with bob's stored config (a PATCH without the signature headers is 401)
curl -s -X POST $BASE/agents/bob/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"ping":1}}'   # -> 200 and the SINK's reply comes back inline
# switch to "async" in the PATCH and the deliver returns 202 instead — the body
# names where the message went: {"transport":"webhook","delivery_mode":"async"}.
# bob's messages go to the SINK, NOT his inbox ("accepted for webhook delivery,
# not stored"). No webhook delivery is ever stored, so a signed GET of the
# target's inbox shows only what earlier inbox deliveries left behind: for an
# agent that has ONLY ever been delivered to by webhook it is exactly
# {"messages":[],"lease_id":""} (the §2.1 message above is still unacked here).
# Now point the PATCH at a dead port (http://127.0.0.1:1/hook) and deliver as
# alice — pass "sender" explicitly, the failure notice is addressed to it:
curl -s -X POST $BASE/agents/bob/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"ping":2},"sender":"alice"}'
# the accept is STILL 202 (queued, not delivered), and once the retries
# run out (~2-3 min with the defaults) ALICE's inbox — the SENDER's, not bob's —
# receives {"kind":"error","code":"WEBHOOK_FAILED","message_id":"...",
# "target":"bob","retries":6,"status_code":0,"error":"post ...: connection refused"}.
# with CR_DATABASE_URL set (the postgres backend) this webhook config is
# PERSISTED: restart the server and GET /agents/bob still returns it.
# A PATCH with {"webhook":null} removes it.
```

**4. Mesh (advanced)** — the peer-to-peer WebSocket surfaces:

```bash
ls examples/ws-mesh-demo/   # Go demo (README + main.go + run-demo.sh): two registered peers join the mesh and show up in GET /mesh/peers as count:2, and a relay subscriber receives one publish
```

The `REQUEST`/`RESPONSE` frames the mesh relays are specified in
`docs/mesh-protocol.md`; this demo exercises live peer visibility and fan-out,
not an RPC round trip.

*Testing the mesh with authentication on (DF-CRIER-287).* By default the mesh
takes the agent id in the connect URL at face value — that is documented, and it
is what the demo above exercises. To see the other posture, restart the server
with `CR_REQUIRE_MESH_AUTH=true` (`CR_GUARD_ENABLED=false CR_REQUIRE_MESH_AUTH=true
./bin/crier -port 8767 -pidfile .crier.pid`): a connection to
`ws://localhost:8767/mesh/connect/<agentID>` then receives an `AUTH_CHALLENGE`
instead of being admitted, and a client that answers nothing — or signs with a
key other than the one registered for that agent — gets `ERROR` `AUTH_FAILED` and
never appears in `GET /mesh/peers`. A client that signs `mesh-auth-v1\n<agent_id>\n<nonce>`
with the registered key gets `AUTH_OK` and is listed. The exact frames, the three
refusal classes and the client-side migration note are in
`docs/mesh-protocol.md` §Authentication; `GET /status` reports
`"mesh_auth_required"` on both postures. Worth reporting: anything that
authenticates with the wrong key, any unauthenticated connection that shows up in
`GET /mesh/peers`, and any refusal whose message does not name the real cause.

**5. Federation (two buses)** — forward across relays:

```bash
CR_FED_LINKS=http://localhost:8767 ./bin/crier -port 8768 -pidfile .crier-fed.pid &
curl -s -X POST localhost:8768/agents/alice/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"via":"guest-bus"}}'
# alice lives on :8767 — the guest forwards (the guest answers 201 and the
# message lands in alice's inbox on the first bus). Kill 8767 first and re-send
# and the guest answers 202 {"status":"held","id":"...","target":"alice",
# "max_hold_s":300} instead, retrying for CR_FED_MAX_HOLD_S (default 300s);
# after that the SENDER's inbox — register the sender on the guest and pass
# "sender" explicitly, exactly as §2.3 does — receives {"kind":"error","code":
# "FEDERATION_FAILED","message_id":"...","target":"alice","sender":"...",
# "attempts":9,"error":"federation: no link reachable (last error: ...)"}.
# A request with no "sender" is NOT held at all (DF-CRIER-129): the terminal
# FEDERATION_FAILED is addressed to the sender, so an unreportable delivery is
# answered synchronously instead — the guest returns 502 {"error":
# "FEDERATION_FAILED",...,"detail":"...no sender..."} immediately and queues
# nothing. Stop the guest bus with `make stop PIDFILE=.crier-fed.pid`
# (its own pidfile: the default `.crier.pid` belongs to the §1 server).
```

**6. MCP mode (for agent frameworks)** — expose crier as tools:

```bash
go build -o bin/crier-mcp ./cmd/crier-mcp
CRIER_HTTP_URL=http://localhost:8767 CRIER_AGENT_ID=my-bridge ./bin/crier-mcp
# then point any MCP client at it: 13 tools (deliver_message, ask_agent, get_messages, ...)
```

## 3. Known rough edges (skip these, or expect them)

Tracked on the board; do **not** re-report unless your reproduction differs.
Every row of the earlier table was re-measured live on 2026-09-16 against a
binary built from the current HEAD, and **no open rough edge reproduced** — the
one mesh row that was open no longer hangs (see the list below), so the
remaining entry is behaviour, not a defect:

| # | Symptom | Status |
|---|---|---|
| 1 | A webhook agent's inbox stays empty after a `202` accept | by design — see "202 from a webhook agent" below, not a bug |

Verified good since the last revision of this file (do not report as broken):

- **A mesh `REQUEST` to a peer that is not connected is answered, not hung.**
  The requester receives an `ERROR` frame carrying `code: CONTROLLER_OFFLINE` in
  ~40 ms (measured at HEAD with a raw stdlib mesh client), whether or not the
  target id ever registered over HTTP — the `docs/mesh-protocol.md` promise
  holds, so a requester never waits for its own timeout.
- **Wildcard relay subscriptions work.** `GET /relay/subscribe/alerts.*` upgrades
  (`101`) and receives a publish to `alerts.fire`; `alerts.*` matches exactly one
  segment, and a terminal `alerts.>` receives `alerts.fire.deep`.
- **`ttl_seconds` on inbox delivery is honoured.** A deliver with
  `"ttl_seconds":3600` returns `201` with `expires_at` = now + 3600s; `0` means
  never expires and is reported as `"expires_at":null` (the key is present, the
  value is null — not the zero time `0001-01-01T00:00:00Z`).
- **Unacked messages redeliver at the 30-second lease boundary**, not on a lagging
  purge tick (measured: a message retrieved at T is retrievable again at T+30s).
- **Two retrievers do not both get everything.** A retrieve leases its batch: an
  immediate second retrieve of the same inbox returns `{"messages":[],"lease_id":""}`.
- **A malformed mesh frame is answered, not swallowed.** A frame the server cannot
  parse — unparseable JSON, a `type` the protocol does not define, a payload that
  does not match its `type` — comes back as an `ERROR` frame with
  `code: INVALID_MESSAGE` naming the failure in `error.message`; `request_id` is
  present only when the frame was readable enough to carry a `message_id`, and the
  socket stays usable for the next REQUEST. Well-formed frames are unaffected (a
  `KEEPALIVE` still draws no reply at all). Proven by
  `TestMeshMalformedFramesGetInvalidMessage` and `TestMeshWellFormedFramesGetNoError`
  in `internal/mesh`; the shapes are documented in `docs/mesh-protocol.md` §ERROR.
- **A dead agent's registry row goes `stale`.** `status` is DERIVED per read from
  the row's liveness evidence (`last_seen`) — a mesh socket accepted for the
  agent, a `KEEPALIVE` heartbeat on it, or a signed `PATCH /agents/{id}` —
  against one window, `CR_PRESENCE_STALE_AFTER_S` (default 90s = three missed
  heartbeats). So: connect a mesh client, kill it mid-session (no unregister, no
  PATCH), and `GET /agents/<id>` flips `online` → `stale` within that window
  while `last_seen` stays frozen at the last evidence; reconnect and the SAME row
  is `online` again on the accept. `GET /status` reports the effective window as
  `presence_stale_after_s`. Reporting `online` forever — the pre-CR-FEAT-024
  behaviour, and anything built on it — was the bug, found by this guide's own
  external review 'DISPATCH · CRI-001'. Proven by
  `TestPresenceGoesStaleWhenTheAgentDiesEndToEnd` in `cmd/server` (2s window, same
  rule). Known limit, not a bug: an agent that never opens a mesh socket and
  never PATCHes reports `stale` once the window passes even while its process
  runs — the registry is a mesh-presence signal (README §3), and `GET /mesh/peers`
  is the connection-table view.

Known-good as of this writing: register/deliver/signed retrieve/ack, blocking +
async webhook, relay publish→subscribe fan-out, bus-to-bus forwarding,
failure-notification after hold expiry, MCP stdio session end-to-end.

### 202 from a webhook agent means accepted, not stored

A delivery to an agent that has a webhook configured goes to that endpoint and
**bypasses the durable inbox** (`specs/WEBHOOK-DELIVERY.md` §4). The accept
tells you which: `202` + `"transport":"webhook"` (plus `"delivery_mode":
"async"|"batch"`) means the message was queued for the endpoint and is NOT in
the target's inbox; `201` + `"transport":"inbox"` (plus `expires_at`, an RFC
3339 instant or `null` when the message never expires) means it
is stored and retrievable; `200` + `"transport":"webhook"` is blocking mode,
with the endpoint's reply in the same body.

So an empty `GET /agents/{id}/inbox` for a webhook-configured agent is the
documented behavior — do not report it. Instead, watch the SENDER's inbox: when
a queued async/batch delivery exhausts its bounded retries
(`CR_WEBHOOK_MAX_RETRIES`, default 5 — or the endpoint's own smaller
`webhook.retries` budget — one attempt per `CR_WEBHOOK_REDELIVER_S`
tick, default 30s, so ~2-3 minutes after the accept by default; longer while
the endpoint is degraded and redeliveries pause until a probe
(`CR_WEBHOOK_PROBE_S`) succeeds), exactly one durable notification lands there:

```json
{"kind":"error","code":"WEBHOOK_FAILED","message_id":"<original>","target":"<recipient agent>","retries":6,"status_code":503,"error":"status 503"}
```

`status_code` is omitted (and `error` carries the transport error string) when
the endpoint never answered at all. The notification is best-effort: an
unregistered sender, or a rejected message with no `sender`, is logged instead
of notified, and it is never retried.

Blocking mode answers synchronously instead: a permanent rejection by the
endpoint (non-retryable 4xx — bad auth, wrong URL — or a 2xx whose body the
reply schema cannot map) returns `502` ("retrying cannot succeed"), while a
timeout / exhausted budget returns `504`.

## 4. How to report

Open a GitHub issue: **https://github.com/crier-dev/crier/issues/new**
(.security issues: see SECURITY.md — do not file those publicly).

A great report has:

1. **What you did** — exact commands / the mode from §2
2. **What you expected** vs **what happened** — paste the actual status codes
3. **Version** — `./bin/crier --version` (or "built from main @ <commit>")
4. **Logs** — the server prints structured logs; include the lines around
   the failure (redact anything you consider sensitive)

Format the title as `[mode] short symptom`, e.g. `[relay] publish to empty
topic returns 202 but nothing arrives`. One issue per finding — it keeps
triage honest.

## 5. Where things live

- API contract: `docs/openapi.yaml` (also served live at `/docs`, which indexes
  the machine-readable `/openapi.json` and `/openapi.yaml`)
- Architecture: `docs/architecture.md` · mesh wire protocol: `docs/mesh-protocol.md`
- Runnable examples: `examples/` (start with `examples/demo.sh`)
