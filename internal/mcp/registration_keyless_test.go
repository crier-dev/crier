package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

// keylessServer returns an MCPServer with keyless registration allowed
// (mirroring !CR_REQUIRE_AGENT_SIG).
func keylessServer() *MCPServer {
	return NewWithOptions(registry.NewMemoryStore(), Options{AllowKeylessAgents: true})
}

// wireAgent reads an agent JSON document with the real public_key type:
// since DF-CRIER-198 registry.HexKey decodes the empty key into the keyless
// representation, these tests assert on the typed agent — the same decode
// the HTTP read path performs.
type wireAgent = registry.Agent

// =============================================================================
// DF-CRIER-197: register_agent mirrors the HTTP handler's conditional
// public_key rule (DF-CRIER-192).
// =============================================================================

// Default (New / Options{}): a keyless registration is refused with the
// unchanged error wording and stores nothing.
func TestRegisterAgent_DefaultRefusesKeyless(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store) // fail-closed default, no Options edit

	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "keyless-1"})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatalf("expected in-band error for keyless registration under the default, got success: %s", text)
	}
	if text != "public_key is required" {
		t.Errorf("expected 'public_key is required', got: %s", text)
	}

	listResp := callTool(t, s, "list_agents", struct{}{})
	listText, listErr := parseToolResult(t, listResp)
	if listErr {
		t.Fatalf("list_agents errored: %s", listText)
	}
	if strings.Contains(listText, "keyless-1") {
		t.Errorf("refused agent must not be stored, but list_agents returned: %s", listText)
	}
}

// Options{} zero value refuses keyless the same way as New.
func TestRegisterAgent_ZeroOptionsRefusesKeyless(t *testing.T) {
	s := NewWithOptions(registry.NewMemoryStore(), Options{})

	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "keyless-1"})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatalf("expected in-band error, got success: %s", text)
	}
	if text != "public_key is required" {
		t.Errorf("expected 'public_key is required', got: %s", text)
	}
}

// Keyless allowed: an omitted public_key registers a keyless agent with
// EMPTY key material (nothing fabricated) which list_agents/get_agent return.
func TestRegisterAgent_KeylessAllowedRegistersEmptyKey(t *testing.T) {
	store := registry.NewMemoryStore()
	s := NewWithOptions(store, Options{AllowKeylessAgents: true})

	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "keyless-1"})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected keyless registration to succeed, got error: %s", text)
	}

	var agent wireAgent
	if err := json.Unmarshal([]byte(text), &agent); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	if agent.ID != "keyless-1" {
		t.Errorf("expected id keyless-1, got %s", agent.ID)
	}
	if len(agent.PublicKey) != 0 {
		t.Errorf("expected empty public_key for keyless agent, got %x", []byte(agent.PublicKey))
	}

	// list_agents returns it.
	listResp := callTool(t, s, "list_agents", struct{}{})
	listText, listErr := parseToolResult(t, listResp)
	if listErr {
		t.Fatalf("list_agents errored: %s", listText)
	}
	var listOut struct {
		Agents []wireAgent `json:"agents"`
	}
	if err := json.Unmarshal([]byte(listText), &listOut); err != nil {
		t.Fatalf("unmarshal list output: %v", err)
	}
	if len(listOut.Agents) != 1 || listOut.Agents[0].ID != "keyless-1" {
		t.Errorf("expected list_agents to contain exactly keyless-1, got: %s", listText)
	}

	// get_agent returns it with an empty key.
	getResp := callTool(t, s, "get_agent", GetAgentInput{ID: "keyless-1"})
	getText, getErr := parseToolResult(t, getResp)
	if getErr {
		t.Fatalf("get_agent errored: %s", getText)
	}
	var fetched wireAgent
	if err := json.Unmarshal([]byte(getText), &fetched); err != nil {
		t.Fatalf("unmarshal fetched agent: %v", err)
	}
	if fetched.ID != "keyless-1" || len(fetched.PublicKey) != 0 {
		t.Errorf("expected keyless-1 with empty key via get_agent, got: %s", getText)
	}
}

// A supplied key is validated identically in both modes.
func TestRegisterAgent_KeyValidationIdenticalInBothModes(t *testing.T) {
	servers := map[string]*MCPServer{
		"default":         New(registry.NewMemoryStore()),
		"keyless-allowed": NewWithOptions(registry.NewMemoryStore(), Options{AllowKeylessAgents: true}),
	}

	// Valid 64-hex key still registers in both modes.
	for mode, s := range servers {
		resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "keyed-" + mode, PublicKey: validTestKey(t)})
		text, isErr := parseToolResult(t, resp)
		if isErr {
			t.Errorf("%s: expected valid key to register, got error: %s", mode, text)
		}
	}

	// 63-char key still errors with the same wording in both modes.
	short := strings.Repeat("a", 63)
	for mode, s := range servers {
		resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "short-" + mode, PublicKey: short})
		text, isErr := parseToolResult(t, resp)
		if !isErr {
			t.Errorf("%s: expected 63-char key to error, got success: %s", mode, text)
			continue
		}
		if !strings.Contains(text, "public_key must be 64 hex characters (ed25519)") {
			t.Errorf("%s: expected 64-hex wording, got: %s", mode, text)
		}
	}

	// Non-hex key still errors in both modes.
	nonHex := "zz" + strings.Repeat("a", 62)
	for mode, s := range servers {
		resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "nonhex-" + mode, PublicKey: nonHex})
		text, isErr := parseToolResult(t, resp)
		if !isErr {
			t.Errorf("%s: expected non-hex key to error, got success: %s", mode, text)
			continue
		}
		if !strings.Contains(text, "public_key must be 64 hex characters (ed25519)") {
			t.Errorf("%s: expected 64-hex wording, got: %s", mode, text)
		}
	}

	// Empty id still errors in both modes.
	for mode, s := range servers {
		resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "", PublicKey: validTestKey(t)})
		text, isErr := parseToolResult(t, resp)
		if !isErr {
			t.Errorf("%s: expected empty id to error, got success: %s", mode, text)
			continue
		}
		if text != "id is required" {
			t.Errorf("%s: expected 'id is required', got: %s", mode, text)
		}
	}
}

// tools/list must expose the relaxed schema: required == ["id"] while the
// public_key property and its pattern are retained (DF-CRIER-197 step 2 —
// a schema-validating MCP client refuses a call with a missing required
// argument, so presence is enforced by the handler per mode instead).
func TestToolsList_RegisterAgentSchemaConditionalKey(t *testing.T) {
	for name, s := range map[string]*MCPServer{
		"default":         New(registry.NewMemoryStore()),
		"keyless-allowed": keylessServer(),
	} {
		resp := s.dispatch(t.Context(), &jsonRPCRequest{
			JSONRPC: "2.0",
			Method:  "tools/list",
			Params:  nil,
			ID:      2,
		})
		resultJSON, _ := json.Marshal(resp.Result)
		var result map[string]json.RawMessage
		if err := json.Unmarshal(resultJSON, &result); err != nil {
			t.Fatalf("%s: unmarshal result: %v", name, err)
		}
		var tools []toolDefinition
		if err := json.Unmarshal(result["tools"], &tools); err != nil {
			t.Fatalf("%s: unmarshal tools: %v", name, err)
		}
		var def *toolDefinition
		for i := range tools {
			if tools[i].Name == "register_agent" {
				def = &tools[i]
			}
		}
		if def == nil {
			t.Fatalf("%s: register_agent missing from tools/list", name)
		}

		var schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
			t.Fatalf("%s: unmarshal InputSchema: %v", name, err)
		}
		if len(schema.Required) != 1 || schema.Required[0] != "id" {
			t.Errorf("%s: expected required == [\"id\"], got %v", name, schema.Required)
		}
		pubProp, ok := schema.Properties["public_key"]
		if !ok {
			t.Fatalf("%s: public_key property missing from schema", name)
		}
		var prop struct {
			Pattern string `json:"pattern"`
		}
		if err := json.Unmarshal(pubProp, &prop); err != nil {
			t.Fatalf("%s: unmarshal public_key property: %v", name, err)
		}
		if prop.Pattern != "^[0-9a-fA-F]{64}$" {
			t.Errorf("%s: public_key pattern not retained, got %q", name, prop.Pattern)
		}
	}
}
