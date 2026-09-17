package mcp

import "fmt"

// The three environment variables that gate the bridge-only MCP tools
// (DF-CRIER-153). These are the same names the handlers use in their gate
// errors, so a client that reads a tool description is told exactly which
// variable to set.
const (
	// EnvAgentID is the bridge's own identity: get_messages / ask_agent read
	// this agent's inbox, and mesh_request needs it for its mesh identity.
	EnvAgentID = "CRIER_AGENT_ID"
	// EnvHTTPURL is the Crier server base URL; mesh_peers reads it.
	EnvHTTPURL = "CRIER_HTTP_URL"
	// EnvMeshURL is the WebSocket mesh endpoint; mesh_request needs it.
	EnvMeshURL = "CRIER_MESH_URL"
)

// toolPrerequisite is one tool's environment gate: EVERY listed variable must
// be non-empty for the tool to work in the running mode.
type toolPrerequisite struct {
	Tool    string
	EnvVars []string
}

// toolPrerequisites is the single source of truth for "which advertised tool
// cannot work in the mode the bridge started in, and which variables are
// missing". It mirrors the gate each handler checks before it does anything
// else:
//
//	get_messages, ask_agent -> requireAgentID (internal/mcp/messaging.go)
//	mesh_peers              -> s.httpURL == ""
//	mesh_request           -> s.bridge == nil, i.e. CRIER_MESH_URL AND
//	                          CRIER_AGENT_ID are both set (NewWithOptions
//	                          builds the mesh bridge only when both are)
//
// Every tool listed here must also name its variables in its own Description:
// the tools/list response is the only metadata an MCP client sees before it
// calls a tool (DF-CRIER-153), and
// TestToolDescriptionsNameTheirPrerequisites derives that requirement from the
// handlers' real error strings rather than from this table.
var toolPrerequisites = []toolPrerequisite{
	{Tool: "get_messages", EnvVars: []string{EnvAgentID}},
	{Tool: "ask_agent", EnvVars: []string{EnvAgentID}},
	{Tool: "mesh_peers", EnvVars: []string{EnvHTTPURL}},
	{Tool: "mesh_request", EnvVars: []string{EnvMeshURL, EnvAgentID}},
}

// UnavailableTools returns the tools that cannot work in a server built with
// the given bridge identity and URLs, in toolPrerequisites order, plus the
// distinct variables missing for them (first mention wins). Both slices are
// empty when the mode can run every advertised tool.
//
// The arguments are exactly what NewWithOptions receives, so a caller that
// built a server from CRIER_AGENT_ID / CRIER_HTTP_URL / CRIER_MESH_URL passes
// the same three values here.
func UnavailableTools(agentID, httpURL, meshURL string) (unavailable, missing []string) {
	values := map[string]string{
		EnvAgentID: agentID,
		EnvHTTPURL: httpURL,
		EnvMeshURL: meshURL,
	}
	seen := make(map[string]bool)
	for _, p := range toolPrerequisites {
		blocked := false
		for _, v := range p.EnvVars {
			if values[v] != "" {
				continue
			}
			blocked = true
			if !seen[v] {
				seen[v] = true
				missing = append(missing, v)
			}
		}
		if blocked {
			unavailable = append(unavailable, p.Tool)
		}
	}
	return unavailable, missing
}

// ToolSurfaceReport renders the ONE startup line that tells an operator which
// advertised tools can work in the mode the bridge started in. The message and
// its structured attributes are returned separately so the caller can hand
// them straight to slog and tests can pin both halves.
//
// toolCount is what tools/list advertises (ToolCount) and mode is the store
// mode label. When every tool is available the line says so — silence is not an
// option, because "nothing to warn about" and "we forgot to report" must not
// look alike.
func ToolSurfaceReport(mode string, toolCount int, agentID, httpURL, meshURL string) (msg string, attrs []any) {
	unavailable, missing := UnavailableTools(agentID, httpURL, meshURL)
	if len(unavailable) == 0 {
		return fmt.Sprintf("MCP tool surface: all %d advertised tools are available in this mode", toolCount),
			[]any{"mode", mode, "tools", toolCount}
	}
	return "MCP tool surface: some advertised tools cannot work in this mode — set the missing environment variables to enable them",
		[]any{
			"mode", mode,
			"tools", toolCount,
			"available_tools", toolCount - len(unavailable),
			"unavailable_tools", unavailable,
			"missing_env", missing,
		}
}
