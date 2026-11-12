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
unset. Setting `CRIER_AGENT_ID`, `CRIER_HTTP_URL`, and `CRIER_MESH_URL` enables
the other 12 tools, for 23 advertised and available tools. The startup diagnostic
reports the same counts, while `tools/list` continues to advertise all 23 tools.

The server must be running before the bridge starts. In remote mode, crier-mcp
registers `CRIER_AGENT_ID` automatically. `CRIER_AGENT_PRIVATE_KEY_FILE` is
optional when the server has signature enforcement disabled as in the command
above; use a stable key for a long-lived bridge against the secure default.

The complete capability report is generated from the registration metadata:
`make mcp-capabilities-check` verifies all 23 tools and their unlock conditions.

ch:trace row=REV-CRIER-2 spec=docs/mcp-capabilities.md wave=crier-foreman-2026-11-11-23-05-04 test=go test ./internal/mcp -run 'TestToolDescriptionsNameTheirPrerequisites|TestUnavailableToolsInMode|TestToolSurfaceReportPinsStartupLine|TestCapabilityReportMatchesToolsListAndNamesEveryUnlock' -count=1 doc=docs witness=path@commit verdict=none:worker commit=none:worker memory=none:foreman-closeout
