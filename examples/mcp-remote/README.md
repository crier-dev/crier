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
`CRIER_AGENT_PRIVATE_KEY_FILE`: this is a fresh/empty in-memory server flow,
not a repeatable restart against an already populated server. The captured
startup line is kept in [`local-transcript.txt`](local-transcript.txt);
ephemeral-key and version lines are omitted because they vary per run.

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
the bridge's automatic `CRIER_AGENT_ID` registration completes and the captured
startup line confirms `identity=confirmed`.

The server must be running before the bridge starts. In remote mode, crier-mcp
registers `CRIER_AGENT_ID` automatically. Without
`CRIER_AGENT_PRIVATE_KEY_FILE`, the bridge generates an ephemeral signing key.
That key is regenerated on every run: starting the bridge again against the same
persistent server therefore produces `identity=rejected` unless the original
stable key is supplied. Use a stable key from the first bridge start, or delete
the existing agent server-side / start a fresh in-memory server before repeating
the empty-key flow. Signature enforcement being disabled does not remove this
registration-key identity check.

The complete capability report is generated from the registration metadata:
`make mcp-capabilities-check` verifies all 23 tools and their unlock conditions.

ch:trace row=REV-CRIER-2 spec=docs/mcp-capabilities.md wave=crier-foreman-2026-11-11-23-05-04 test=go test ./internal/mcp -run 'TestToolDescriptionsNameTheirPrerequisites|TestUnavailableToolsInMode|TestToolSurfaceReportPinsStartupLine|TestCapabilityReportMatchesToolsListAndNamesEveryUnlock' -count=1 doc=docs witness=path@commit verdict=none:worker commit=none:worker memory=none:foreman-closeout
