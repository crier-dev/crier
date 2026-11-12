package mcp

import (
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

func TestRegisterAgent_KeylessDuplicateSurfacesActionableMessage(t *testing.T) {
	s := NewWithOptions(registry.NewMemoryStore(), Options{AllowKeylessAgents: true})
	first := callTool(t, s, "register_agent", RegisterAgentInput{ID: "keyless-duplicate"})
	if text, isErr := parseToolResult(t, first); isErr {
		t.Fatalf("initial keyless registration failed: %s", text)
	}

	second := callTool(t, s, "register_agent", RegisterAgentInput{ID: "keyless-duplicate"})
	text, isErr := parseToolResult(t, second)
	if !isErr {
		t.Fatalf("duplicate keyless registration unexpectedly succeeded: %s", text)
	}
	if !strings.Contains(text, "agent already registered: keyless registration for \"keyless-duplicate\" already exists") {
		t.Fatalf("MCP error = %q, want the underlying actionable registration message", text)
	}
	if strings.Contains(text, "different public key") {
		t.Fatalf("MCP error misclassified keyless duplicate: %q", text)
	}
}

// ch:trace row=DF-CRIER-504 spec=none:dogfood-finding wave=crier-foreman-2026-11-12-15-18-43 test=TestRegisterAgent_KeylessDuplicateSurfacesActionableMessage doc=none:registry-error evidence=internal/mcp/registration_conflict_test.go witness=none:live-bridge-not-required verdict=none:foreman-gitreins commit=none:worker-commit memory=none:foreman-closeout
// STATUS: complete
// TIER: T1
// EVIDENCE: TestRegisterAgent_KeylessDuplicateSurfacesActionableMessage
