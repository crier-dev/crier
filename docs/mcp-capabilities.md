# MCP capability unlocks

Generated from the MCP registration surface and prerequisite metadata; do not edit by hand.

| Tool | Unlock condition |
| --- | --- |
| `register_agent` | Always available (no bridge environment or remote dependency) |
| `list_agents` | Always available (no bridge environment or remote dependency) |
| `get_agent` | Always available (no bridge environment or remote dependency) |
| `unregister_agent` | Always available (no bridge environment or remote dependency) |
| `deliver_message` | Always available (no bridge environment or remote dependency) |
| `retrieve_inbox` | Always available (no bridge environment or remote dependency) |
| `ack_messages` | Always available (no bridge environment or remote dependency) |
| `inbox_stats` | Always available (no bridge environment or remote dependency) |
| `send_message` | Always available (no bridge environment or remote dependency) |
| `get_messages` | CRIER_AGENT_ID is set (the bridge identity owns the inbox lease and acknowledgements) |
| `ask_agent` | CRIER_AGENT_ID is set (the bridge identity owns the inbox lease and reply polling) |
| `mesh_peers` | CRIER_HTTP_URL is set to the Crier server base URL |
| `mesh_request` | CRIER_MESH_URL and CRIER_AGENT_ID are set (the bridge opens its WebSocket as that identity) |
| `create_run` | CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes |
| `run_status` | CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes |
| `cancel_run` | CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes |
| `resume_run` | CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes |
| `rewind_run` | CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes |
| `run_skill` | CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes |
| `courier_unread_summary` | CRIER_AGENT_ID is set (the default scope is the bridge identity's unread mail) |
| `courier_unread_list` | CRIER_AGENT_ID is set (the default scope is the bridge identity's unread mail) |
| `harness_list` | Always available (no bridge environment or remote dependency) |
| `harness_probe` | Always available (no bridge environment or remote dependency) |
