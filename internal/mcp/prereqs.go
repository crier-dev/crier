package mcp

import (
	"fmt"
	"strings"
)

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
	// EnvOperatorScope arms cross-agent nudge reads (CRIER-MCP-NUDGE §5):
	// without it a bridge answers only for its own identity and any other
	// agent_id is NUDGE_SCOPE_DENIED.
	EnvOperatorScope = "CRIER_OPERATOR_SCOPE"
)

// toolPrerequisite is one tool's environment gate: EVERY listed variable must
// be non-empty for the tool to work in the running mode.
type toolPrerequisite struct {
	Tool      string
	EnvVars   []string
	Condition string
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
//	create_run, run_status, cancel_run, resume_run, rewind_run, run_skill
//	                        -> s.dagger == nil, i.e. CRIER_HTTP_URL is unset,
//	                          so there is no server whose run records the
//	                          tools could address (internal/mcp/dagger.go)
//
// Every tool listed here must also name its variables in its own Description:
// the tools/list response is the only metadata an MCP client sees before it
// calls a tool (DF-CRIER-153), and
// TestToolDescriptionsNameTheirPrerequisites derives that requirement from the
// handlers' real error strings rather than from this table.
var toolPrerequisites = []toolPrerequisite{
	{Tool: "get_messages", EnvVars: []string{EnvAgentID}, Condition: "CRIER_AGENT_ID is set (the bridge identity owns the inbox lease and acknowledgements)"},
	{Tool: "ask_agent", EnvVars: []string{EnvAgentID}, Condition: "CRIER_AGENT_ID is set (the bridge identity owns the inbox lease and reply polling)"},
	{Tool: "mesh_peers", EnvVars: []string{EnvHTTPURL}, Condition: "CRIER_HTTP_URL is set to the Crier server base URL"},
	{Tool: "mesh_request", EnvVars: []string{EnvMeshURL, EnvAgentID}, Condition: "CRIER_MESH_URL and CRIER_AGENT_ID are set (the bridge opens its WebSocket as that identity)"},
	// CR-CHAT-033: the dagger control tools address the run records the Crier
	// SERVER holds, so they need the server base URL like mesh_peers does.
	// The executor itself is reached by the SERVER (CR_DAGGER_URL), never by
	// the bridge — crier embeds no executor.
	{Tool: "create_run", EnvVars: []string{EnvHTTPURL}, Condition: "CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes"},
	{Tool: "run_status", EnvVars: []string{EnvHTTPURL}, Condition: "CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes"},
	{Tool: "cancel_run", EnvVars: []string{EnvHTTPURL}, Condition: "CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes"},
	{Tool: "resume_run", EnvVars: []string{EnvHTTPURL}, Condition: "CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes"},
	{Tool: "rewind_run", EnvVars: []string{EnvHTTPURL}, Condition: "CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes"},
	{Tool: "run_skill", EnvVars: []string{EnvHTTPURL}, Condition: "CRIER_HTTP_URL is set to a Crier server exposing the Dagger control routes"},
	// CRIER-MCP-NUDGE: with no bridge identity the nudge tools can answer only
	// for an agent_id the caller spells out in every call; the default scope —
	// "my own mail" — does not exist, so the startup report says so.
	{Tool: "courier_unread_summary", EnvVars: []string{EnvAgentID}, Condition: "CRIER_AGENT_ID is set (the default scope is the bridge identity's unread mail)"},
	{Tool: "courier_unread_list", EnvVars: []string{EnvAgentID}, Condition: "CRIER_AGENT_ID is set (the default scope is the bridge identity's unread mail)"},
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
// CapabilityReportMarkdown renders the generated capability table from the same
// prerequisite metadata that drives the startup diagnostic. Always-available
// tools are supplied by toolNames; gated tools are annotated from the table.
func CapabilityReportMarkdown(toolNames []string) string {
	gates := make(map[string]toolPrerequisite, len(toolPrerequisites))
	for _, p := range toolPrerequisites {
		gates[p.Tool] = p
	}
	var b strings.Builder
	b.WriteString("# MCP capability unlocks\n\n")
	b.WriteString("Generated from the MCP registration surface and prerequisite metadata; do not edit by hand.\n\n")
	b.WriteString("| Tool | Unlock condition |\n| --- | --- |\n")
	for _, name := range toolNames {
		if p, ok := gates[name]; ok {
			fmt.Fprintf(&b, "| `%s` | %s |\n", name, p.Condition)
		} else {
			fmt.Fprintf(&b, "| `%s` | Always available (no bridge environment or remote dependency) |\n", name)
		}
	}
	return b.String()
}

// ToolSurfaceReport renders the startup report without an identity verdict.
// It is retained for library callers that do not perform remote registration.
func ToolSurfaceReport(mode string, toolCount int, agentID, httpURL, meshURL string) (msg string, attrs []any) {
	return ToolSurfaceReportWithIdentity(mode, toolCount, agentID, httpURL, meshURL, "")
}

// ToolSurfaceReportWithIdentity renders the startup report and, when supplied,
// qualifies the all-available claim with the remote identity verdict. A remote
// bridge can have all environment prerequisites while its signing key is
// rejected by the server; emitting the unqualified claim in that state would
// tell the operator that the advertised surface is usable when every signed
// request will fail.
//
// identityState is empty when identity registration is not part of the caller's
// mode. A non-empty value is emitted as the structured identity attribute. Only
// "confirmed" permits the unqualified all-available wording.
func ToolSurfaceReportWithIdentity(mode string, toolCount int, agentID, httpURL, meshURL, identityState string) (msg string, attrs []any) {
	unavailable, missing := UnavailableTools(agentID, httpURL, meshURL)
	if len(unavailable) == 0 {
		msg = fmt.Sprintf("MCP tool surface: all %d advertised tools are available in this mode", toolCount)
		attrs = []any{"mode", mode, "tools", toolCount}
		if identityState != "" {
			attrs = append(attrs, "identity", identityState)
			if identityState != "confirmed" {
				msg += fmt.Sprintf(" (identity=%s)", identityState)
			}
		}
		return msg, attrs
	}
	attrs = []any{
		"mode", mode,
		"tools", toolCount,
		"available_tools", toolCount - len(unavailable),
		"unavailable_tools", unavailable,
		"missing_env", missing,
	}
	if identityState != "" {
		attrs = append(attrs, "identity", identityState)
	}
	return "MCP tool surface: some advertised tools cannot work in this mode — set the missing environment variables to enable them", attrs
}
