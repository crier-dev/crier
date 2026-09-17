package mcp

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

// =============================================================================
// DF-CRIER-153 — the advertised MCP surface must be truthful about the mode it
// started in: a tool that cannot work without environment says so in its own
// description (tools/list is the only metadata a client sees), the agent-scoped
// tools state the bridge scope restriction, and the stdio server logs ONE line
// naming the tools that cannot work.
// =============================================================================

// expectedPrerequisites is the pinned truth table: the tools whose handlers
// refuse to run without environment, and the variables each needs.
var expectedPrerequisites = map[string][]string{
	"get_messages": {EnvAgentID},
	"ask_agent":    {EnvAgentID},
	"mesh_peers":   {EnvHTTPURL},
	"mesh_request": {EnvMeshURL, EnvAgentID},
}

// uniqueStrings returns the input with duplicates removed, order preserved.
func uniqueStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// TestToolDescriptionsNameTheirPrerequisites derives the environment gates from
// the HANDLERS, not from a hand-copied list: every tool is called against a
// server with zero Options — the default in-process stdio mode, where
// cmd/crier-mcp starts with no CRIER_* variable set — and any CRIER_* name the
// error text mentions must appear in that tool's advertised description. Adding
// a mode-gated tool without the note therefore fails this test.
func TestToolDescriptionsNameTheirPrerequisites(t *testing.T) {
	s := New(registry.NewMemoryStore())
	defs := s.toolDefinitions()
	if len(defs) != 13 {
		t.Fatalf("advertised tools = %d, want 13", len(defs))
	}

	advertised := make(map[string]bool, len(defs))
	envVar := regexp.MustCompile(`CRIER_[A-Z0-9_]+`)
	derived := make(map[string][]string)
	for _, def := range defs {
		advertised[def.Name] = true

		resp := callTool(t, s, def.Name, json.RawMessage(`{}`))
		text, isErr := parseToolResult(t, resp)
		if !isErr {
			continue
		}
		vars := uniqueStrings(envVar.FindAllString(text, -1))
		if len(vars) == 0 {
			continue // not an environment gate (argument validation, ...)
		}
		derived[def.Name] = vars
		for _, v := range vars {
			if !strings.Contains(def.Description, v) {
				t.Errorf("%s: the handler refuses to run without %s (error: %q) but the advertised description never names it, so an MCP client cannot know before it calls: %q",
					def.Name, v, text, def.Description)
			}
		}
	}

	if !reflect.DeepEqual(derived, expectedPrerequisites) {
		t.Errorf("gated tools derived from handler behavior = %v, want %v", derived, expectedPrerequisites)
	}

	// The startup report reads toolPrerequisites; the descriptions are read by
	// clients. They must describe the same gates or the surface lies again.
	tableGates := make(map[string][]string, len(toolPrerequisites))
	for _, p := range toolPrerequisites {
		if !advertised[p.Tool] {
			t.Errorf("toolPrerequisites lists %q, which tools/list does not advertise", p.Tool)
		}
		tableGates[p.Tool] = p.EnvVars
	}
	if !reflect.DeepEqual(tableGates, expectedPrerequisites) {
		t.Errorf("toolPrerequisites (startup report) = %v, want %v", tableGates, expectedPrerequisites)
	}
}

// TestUnavailableToolsInMode pins the startup helper across the modes the
// bridge can start in.
func TestUnavailableToolsInMode(t *testing.T) {
	if n := New(registry.NewMemoryStore()).ToolCount(); n != 13 {
		t.Fatalf("ToolCount() = %d, want 13", n)
	}

	cases := []struct {
		name            string
		agentID         string
		httpURL         string
		meshURL         string
		wantUnavailable []string
		wantMissing     []string
	}{
		{
			name:            "in-process, no env at all",
			wantUnavailable: []string{"get_messages", "ask_agent", "mesh_peers", "mesh_request"},
			wantMissing:     []string{EnvAgentID, EnvHTTPURL, EnvMeshURL},
		},
		{
			name:            "bridge identity only",
			agentID:         "bridge",
			wantUnavailable: []string{"mesh_peers", "mesh_request"},
			wantMissing:     []string{EnvHTTPURL, EnvMeshURL},
		},
		{
			name:            "remote bridge (http url + identity), no mesh url",
			agentID:         "bridge",
			httpURL:         "http://localhost:8767",
			wantUnavailable: []string{"mesh_request"},
			wantMissing:     []string{EnvMeshURL},
		},
		{
			// The mesh bridge is built only when BOTH are set, so a mesh URL
			// without an identity leaves mesh_request unavailable too — and
			// the missing list names the two variables that are actually
			// unset (CRIER_AGENT_ID is already named by get_messages, so
			// mesh_request adds no new one).
			name:            "mesh url without identity",
			meshURL:         "ws://localhost:8767/mesh/connect/bridge",
			wantUnavailable: []string{"get_messages", "ask_agent", "mesh_peers", "mesh_request"},
			wantMissing:     []string{EnvAgentID, EnvHTTPURL},
		},
		{
			name:            "fully configured",
			agentID:         "bridge",
			httpURL:         "http://localhost:8767",
			meshURL:         "ws://localhost:8767/mesh/connect/bridge",
			wantUnavailable: nil,
			wantMissing:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unavailable, missing := UnavailableTools(tc.agentID, tc.httpURL, tc.meshURL)
			if !reflect.DeepEqual(unavailable, tc.wantUnavailable) {
				t.Errorf("unavailable = %v, want %v", unavailable, tc.wantUnavailable)
			}
			if !reflect.DeepEqual(missing, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", missing, tc.wantMissing)
			}
		})
	}
}

// attrsMap turns the (key, value, key, value, ...) attribute list into a map.
func attrsMap(t *testing.T, attrs []any) map[string]any {
	t.Helper()
	if len(attrs)%2 != 0 {
		t.Fatalf("odd attribute count: %v", attrs)
	}
	out := make(map[string]any, len(attrs)/2)
	for i := 0; i < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if !ok {
			t.Fatalf("attribute %d is not a string key: %v", i, attrs[i])
		}
		out[key] = attrs[i+1]
	}
	return out
}

// TestToolSurfaceReportPinsStartupLine pins the one startup line for the
// default in-process mode and for a fully configured bridge. Silence is not an
// option for the unavailable case, and the all-available case must still say so
// (a missing report and a clean report must not look alike).
func TestToolSurfaceReportPinsStartupLine(t *testing.T) {
	s := New(registry.NewMemoryStore())
	tools := s.ToolCount()

	t.Run("in-process", func(t *testing.T) {
		msg, attrs := ToolSurfaceReport("in-process", tools, "", "", "")
		const want = "MCP tool surface: some advertised tools cannot work in this mode — set the missing environment variables to enable them"
		if msg != want {
			t.Errorf("msg = %q, want %q", msg, want)
		}
		got := attrsMap(t, attrs)
		if got["mode"] != "in-process" {
			t.Errorf("mode = %v, want in-process", got["mode"])
		}
		if got["tools"] != 13 || got["available_tools"] != 9 {
			t.Errorf("tools/available_tools = %v/%v, want 13/9", got["tools"], got["available_tools"])
		}
		wantUnavailable := []string{"get_messages", "ask_agent", "mesh_peers", "mesh_request"}
		if !reflect.DeepEqual(got["unavailable_tools"], wantUnavailable) {
			t.Errorf("unavailable_tools = %v, want %v", got["unavailable_tools"], wantUnavailable)
		}
		wantMissing := []string{EnvAgentID, EnvHTTPURL, EnvMeshURL}
		if !reflect.DeepEqual(got["missing_env"], wantMissing) {
			t.Errorf("missing_env = %v, want %v", got["missing_env"], wantMissing)
		}
	})

	t.Run("fully configured", func(t *testing.T) {
		msg, attrs := ToolSurfaceReport("remote/bridge", tools,
			"bridge", "http://localhost:8767", "ws://localhost:8767/mesh/connect/bridge")
		const want = "MCP tool surface: all 13 advertised tools are available in this mode"
		if msg != want {
			t.Errorf("msg = %q, want %q", msg, want)
		}
		got := attrsMap(t, attrs)
		if got["mode"] != "remote/bridge" || got["tools"] != 13 {
			t.Errorf("attrs = %v", got)
		}
		if _, ok := got["unavailable_tools"]; ok {
			t.Errorf("all-available report must not carry unavailable_tools: %v", got)
		}
	})
}

// TestScopeRestrictedToolsStateTheirScope pins which tools carry the bridge
// scope restriction and where the note lives. In remote mode the bridge sends
// its own identity on every agent-scoped route, and the server answers 403
// "agent <caller> may only access its own resources" for any other agent
// (internal/registry/{remote.go,agentsig.go}); the tools that hit those routes
// must not imply unrestricted cross-agent access. The note goes on the agent-id
// property (where the argument is) or the tool description — this test accepts
// either and fails if it disappears, or if a tool that is NOT scope-restricted
// claims one.
func TestScopeRestrictedToolsStateTheirScope(t *testing.T) {
	const marker = "own identity only"
	wantScopeRestricted := []string{"unregister_agent", "retrieve_inbox", "ack_messages", "inbox_stats"}

	gotScopeRestricted := []string{}
	for _, def := range New(registry.NewMemoryStore()).toolDefinitions() {
		inDescription := strings.Contains(def.Description, marker)
		inSchema := strings.Contains(string(def.InputSchema), marker)
		if !inDescription && !inSchema {
			continue
		}
		gotScopeRestricted = append(gotScopeRestricted, def.Name)
		if !strings.Contains(def.Description, "403") {
			t.Errorf("%s: scope note is present but the tool description never names the 403 the server answers: %q", def.Name, def.Description)
		}
	}

	if !reflect.DeepEqual(gotScopeRestricted, wantScopeRestricted) {
		t.Errorf("tools carrying the scope note = %v, want %v", gotScopeRestricted, wantScopeRestricted)
	}
}
