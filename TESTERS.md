# Tester Guide

Thanks for testing crier. This file tells you what to poke, what's already
known-broken (so you don't waste time), and how to report so your findings
are actionable. Time budget: **~30 minutes for the full pass**, less if you
only care about one mode.

## 1. Get it running (5 minutes)

```bash
git clone https://github.com/crier-dev/crier.git && cd crier
make build
CR_GUARD_ENABLED=false ./bin/crier -port 8767 &
curl -s localhost:8767/health        # → {"status":"ok",...}
```

Keep that server running — every exercise below assumes `localhost:8767`
with the guard off (no API key needed). Anything you send stays on your
machine; crier phones home to **nothing**.

Prefer containers? `docker` — see `examples/agent-ecosystem/` for a
compose stack. Prefer a UI? open the interactive API docs at
**http://localhost:8767/docs** and fire requests from the browser.

## 2. Eight ways to move a message (~20 minutes)

A reference harness that exercises all of these automatically lives in
`scripts/bunker-matrix.sh`; the manual curl versions below are the same
probes it runs.

**0. Register two agents** (every exercise needs them):

```bash
curl -s -X POST localhost:8767/agents -H 'Content-Type: application/json' \
  -d '{"id":"alice","public_key":"'$(python3 -c 'import secrets;print(secrets.token_hex(32))')'"}'
# → 201
curl -s -X POST localhost:8767/agents -H 'Content-Type: application/json' \
  -d '{"id":"bob","public_key":"'$(python3 -c 'import secrets;print(secrets.token_hex(32))')'"}'
```

**1. Inbox (store-and-forward)** — deliver to bob, read it back:

```bash
curl -s -X POST localhost:8767/agents/bob/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"hello":"bob"}}'                                   # → 201
curl -s localhost:8767/agents/bob/inbox | python3 -m json.tool
# payload arrives BASE64-ENCODED — decode it: echo "<b64>" | base64 -d
```

**2. Relay pub/sub** — fan-out to live subscribers:

```bash
# terminal A: subscribe (leave running)
python3 - <<'EOF'
import socket, base64, os, json, struct
s = socket.create_connection(("localhost", 8767))
s.sendall(b"GET /relay/subscribe/demo-topic HTTP/1.1\r\nHost: localhost:8767\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
print(s.recv(4096)[:64])   # 101 → subscribed
print(s.recv(4096))        # blocks until a publish arrives
EOF
# terminal B: publish
curl -s -X POST localhost:8767/relay/publish -H 'Content-Type: application/json' \
  -H 'X-Agent-ID: alice' \
  -d '{"topic":"demo-topic","event":{"msg":"hi subscribers"}}'        # → 202
```

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
# terminal B: point bob at it, then deliver
curl -s -X PATCH localhost:8767/agents/bob -H 'Content-Type: application/json' \
  -d '{"webhook":{"url":"http://127.0.0.1:9911/hook","delivery_mode":"blocking"}}'
curl -s -X POST localhost:8767/agents/bob/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"ping":1}}'   # → 200 and the SINK's reply comes back inline
# switch to "async" in the PATCH and the deliver returns 202 instead
# with CR_DATABASE_URL set (the postgres backend) this webhook config is
# PERSISTED: restart the server and GET /agents/bob still returns it.
# A PATCH with {"webhook":null} removes it.
```

**4. Mesh request/response (advanced)** — peer-to-peer RPC over WebSocket:

```bash
ls examples/ws-mesh-demo/   # Go demo: README + main.go — a mesh client showing REGISTER → REQUEST → RESPONSE
```

**5. Federation (two buses)** — forward across relays:

```bash
CR_FED_LINKS=http://localhost:8767 ./bin/crier -port 8768 &
curl -s -X POST localhost:8768/agents/alice/inbox -H 'Content-Type: application/json' \
  -d '{"payload":{"via":"guest-bus"}}'
# alice lives on :8767 — the guest forwards. Kill 8767 first and re-send:
# the guest holds + retries, then the sender's inbox gets a FEDERATION_FAILED notice.
```

**6. MCP mode (for agent frameworks)** — expose crier as tools:

```bash
go build -o bin/crier-mcp ./cmd/crier-mcp
CRIER_HTTP_URL=http://localhost:8767 CRIER_AGENT_ID=my-bridge ./bin/crier-mcp
# then point any MCP client at it: 13 tools (deliver_message, ask_agent, get_messages, ...)
```

## 3. Known rough edges (skip these, or expect them)

Tracked on the board; do **not** re-report unless your reproduction differs:

| # | Symptom | Status |
|---|---|---|
| 1 | Wildcard topic subscribe (`alerts.*`) rejected with a reason-free 400; docs promise it | open, P1 |
| 2 | `ttl_seconds` on inbox delivery accepted but ignored (expiry hard-coded 24h) | open, P1 |
| 3 | Unacked inbox messages redeliver after ~60s, not the documented 30s | open, P1 |
| 4 | Mesh REQUEST to an id never registered over HTTP hangs forever (no error frame) | open, P1 |
| 5 | No work distribution: two retrievers on one inbox both get every message | open, P2 |
| 6 | Malformed mesh frames are dropped silently (spec says INVALID_MESSAGE reply) | open, P2 |

Known-good as of this writing: register/deliver/retrieve/ack, blocking +
async webhook, relay publish→subscribe fan-out, bus-to-bus forwarding,
failure-notification after hold expiry, MCP stdio session end-to-end.

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

- API contract: `docs/openapi.yaml` (also served live at `/docs`)
- Architecture: `docs/architecture.md` · mesh wire protocol: `docs/mesh-protocol.md`
- Runnable examples: `examples/` (start with `examples/demo.sh`)
