# Remote MCP mode

Start a local Crier server first (the default is an in-memory server on port 8767):

```bash
CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false make run
```

Copy `env.template`, then launch the bridge with the server URL, bridge identity,
and mesh URL set:

```bash
set -a
. examples/mcp-remote/env.template
set +a
make build-mcp && ./bin/crier-mcp
```

The bridge starts with the 11 always-available tools when these variables are
unset. The cumulative startup tiers are 11 by default, 15 after setting
`CRIER_AGENT_ID`, 22 after also setting `CRIER_HTTP_URL`, and 23 after also
setting `CRIER_MESH_URL`. The startup diagnostic reports these counts, while
`tools/list` continues to advertise all 23 tools at every tier.

The following is the exact stable capability line captured from the runnable
local flow documented by this example. The captured startup line is kept in
[`local-transcript.txt`](local-transcript.txt); ephemeral-key and version lines
are intentionally omitted because they vary per run.

```text
CRIER_PORT=18767 CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false PIDFILE=.rev-crier-2.pid make run
CR_REQUIRE_AGENT_SIG=false CR_GUARD_ENABLED=false CRIER_HTTP_URL=http://127.0.0.1:18767 CRIER_AGENT_ID=mcp-agent CRIER_MESH_URL=ws://127.0.0.1:18767/mesh/connect/mcp-agent CRIER_AGENT_PRIVATE_KEY_FILE= CRIER_AUTH_TOKEN= CR_AUTH_TOKEN= timeout 4s ./bin/crier-mcp
2026/11/13 14:30:39 INFO MCP tool surface: all 23 advertised tools are available in this mode mode=remote/bridge tools=23 identity=confirmed
```

The server's default auth setting is secure, so the invocation explicitly turns
off `CR_REQUIRE_AGENT_SIG` for this local demonstration. With enforcement off,
the bridge's automatic `CRIER_AGENT_ID` registration completes and the startup
line confirms `identity=confirmed`.

The server must be running before the bridge starts. In remote mode, crier-mcp
registers `CRIER_AGENT_ID` automatically. `CRIER_AGENT_PRIVATE_KEY_FILE` is
optional when the server has signature enforcement disabled as in the command
above; use a stable key for a long-lived bridge against the secure default.

The complete capability report is generated from the registration metadata:
`make mcp-capabilities-check` verifies all 23 tools and their unlock conditions.

ch:trace row=REV-CRIER-2 spec=docs/mcp-capabilities.md wave=crier-foreman-2026-11-11-23-05-04 test=go test ./internal/mcp -run 'TestToolDescriptionsNameTheirPrerequisites|TestUnavailableToolsInMode|TestToolSurfaceReportPinsStartupLine|TestCapabilityReportMatchesToolsListAndNamesEveryUnlock' -count=1 doc=docs witness=path@commit verdict=none:worker commit=none:worker memory=none:foreman-closeout
