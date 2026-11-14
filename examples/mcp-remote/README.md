# Remote MCP mode

Start a local Crier server first (the default is an in-memory server on port 8767):

```bash
CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false make run
```

Copy `env.template`, then launch the bridge with the selected Tier 2 values:

```bash
set -a
. examples/mcp-remote/env.template
set +a
make build-mcp && ./bin/crier-mcp
```

The template selects one tier; sourcing it does not demonstrate every tier. The
cumulative startup tiers are 11 by default, 15 after setting `CRIER_AGENT_ID`,
22 after also setting `CRIER_HTTP_URL`, and 23 after also setting
`CRIER_MESH_URL`. To run the Tier 3 variant, uncomment `CRIER_MESH_URL` in the
template before sourcing it. The startup diagnostic reports `available_tools`
for the selected tier, while `tools/list` continues to advertise all 23 tools
at every tier (`tools=23`).

The following is the exact Tier 3 invocation and startup line captured from the
runnable local flow documented by this example. It intentionally uses an empty
`CRIER_AGENT_PRIVATE_KEY_FILE` (an empty value is equivalent to leaving it
unset): on a fresh in-memory server, the first invocation creates the agent and
reports `identity=confirmed`. This is not a repeatable restart against a reused
server that already has the agent registration. The captured startup line is kept in
[`local-transcript.txt`](local-transcript.txt); ephemeral-key and version lines
are omitted because they vary per run.

```text
CRIER_PORT=18767 CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false PIDFILE=.rev-crier-2.pid make run
CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false CRIER_HTTP_URL=http://127.0.0.1:18767 CRIER_AGENT_ID=mcp-agent CRIER_MESH_URL=ws://127.0.0.1:18767/mesh/connect/mcp-agent CRIER_AGENT_PRIVATE_KEY_FILE= CRIER_AUTH_TOKEN= CR_AUTH_TOKEN= timeout 4s ./bin/crier-mcp
2026/11/13 14:30:39 INFO MCP tool surface: all 23 advertised tools are available in this mode mode=remote/bridge tools=23 identity=confirmed
```

Here `tools=23` is the advertised `tools/list` surface. Because all three
bridge variables are set, the startup report has no `available_tools` field;
its Tier 3 value is 23. With the sourceable template as committed (Tier 2),
the corresponding startup attributes are `tools=23 available_tools=22`.

The server's default auth setting is secure, so the invocation explicitly turns
off `CR_REQUIRE_AGENT_SIG` for this local demonstration. With enforcement off,
the bridge's automatic `CRIER_AGENT_ID` registration completes on the fresh
server and the captured startup line confirms `identity=confirmed`.

The server must be running before the bridge starts. In remote mode, crier-mcp
registers `CRIER_AGENT_ID` automatically. Without
`CRIER_AGENT_PRIVATE_KEY_FILE`, or with `CRIER_AGENT_PRIVATE_KEY_FILE=` as in
the command above, the bridge generates an ephemeral signing key. That key is
regenerated on every run. Therefore the exact second invocation below, against
the same still-running server, does not confirm the existing identity:

```text
CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false CRIER_HTTP_URL=http://127.0.0.1:18767 CRIER_AGENT_ID=mcp-agent CRIER_MESH_URL=ws://127.0.0.1:18767/mesh/connect/mcp-agent CRIER_AGENT_PRIVATE_KEY_FILE= CR_AUTH_TOKEN= timeout 4s ./bin/crier-mcp
... identity=rejected
```

The server keeps the public key from the first registration, while the second
bridge has generated a different key. Its signed requests are consequently
rejected. Signature enforcement being disabled does not remove this
registration-key identity check. To repeat the empty-key command with
`identity=confirmed`, stop the server and start a fresh in-memory server (or
delete `mcp-agent` on the existing server first); do not claim that an empty
key is restart-stable.

For repeated startup against the same server, generate one stable PKCS#8 PEM
key once and reuse that exact file on every bridge invocation. The following
commands show the key creation and both runs; these runs must target a fresh
server, because a server that already registered a different ephemeral key
will continue to reject the new stable key until its `mcp-agent` registration
is deleted.

```bash
mkdir -p "$HOME/.config/crier"
openssl genpkey -algorithm ED25519 -out "$HOME/.config/crier/mcp-agent.key"
export CRIER_AGENT_PRIVATE_KEY_FILE="$HOME/.config/crier/mcp-agent.key"
CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false CRIER_HTTP_URL=http://127.0.0.1:18767 CRIER_AGENT_ID=mcp-agent CRIER_MESH_URL=ws://127.0.0.1:18767/mesh/connect/mcp-agent timeout 4s ./bin/crier-mcp
# identity=confirmed (first stable-key registration)
CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false CRIER_HTTP_URL=http://127.0.0.1:18767 CRIER_AGENT_ID=mcp-agent CRIER_MESH_URL=ws://127.0.0.1:18767/mesh/connect/mcp-agent timeout 4s ./bin/crier-mcp
# identity=confirmed (same stable key, same server)
```

The complete capability report is generated from the registration metadata:
`make mcp-capabilities-check` verifies all 23 tools and their unlock conditions.

ch:trace row=REV-CRIER-2 spec=none:board-acceptance wave=.coding-hermes/waves/crier-foreman-2026-11-14-00-37-54.json test=make mcp-capabilities-check;go test ./internal/mcp/ ./cmd/crier-mcp/ -count=1;go build ./...;go vet ./... doc=examples/mcp-remote/README.md evidence=none:commands-run-and-judge-rework-pending witness=none:local-acceptance-probe verdict=none:rework-judge-failed-placeholder-trace commit=8111938ec648c62676dbbfbf90ff9f4ea16edcec memory=none:pending-board-closeout
STATUS: PARTIAL
TIER: T3
EVIDENCE: make mcp-capabilities-check (exit 0); go test ./internal/mcp/ ./cmd/crier-mcp/ -count=1 (exit 0); go build ./... (exit 0); go vet ./... (exit 0); full go test ./... -count=1 (exit 0)
