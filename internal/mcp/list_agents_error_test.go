package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
)

// TestListAgents_BackendFailureIsError pins the DF-CRIER-199 bridge
// surface: list_agents through a RemoteStore whose backend is failing must
// answer with an ERROR result carrying the upstream failure — not an empty
// agent list, which is indistinguishable from an empty registry. Drives
// the real RemoteStore (the store cmd/crier-mcp builds when CRIER_HTTP_URL
// is set) against a 500 server, so both halves are proven: the store
// records the failure and the handler surfaces it.
func TestListAgents_BackendFailureIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registry down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	s := New(registry.NewRemoteStore(srv.URL, "bridge", ""))

	resp := callTool(t, s, "list_agents", struct{}{})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatalf("expected an error result for a failing backend, got success: %s", text)
	}
	if !strings.Contains(text, "500") {
		t.Errorf("error text %q should mention the upstream HTTP status", text)
	}
}

// TestListAgents_EmptyRegistryViaRemoteStore pins the success side of the
// distinction: through the SAME RemoteStore path, a healthy server with an
// empty registry still answers an empty (non-nil) agents array, not an
// error.
func TestListAgents_EmptyRegistryViaRemoteStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"agents":[]}`))
	}))
	t.Cleanup(srv.Close)

	s := New(registry.NewRemoteStore(srv.URL, "bridge", ""))

	resp := callTool(t, s, "list_agents", struct{}{})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success for a healthy empty registry, got error: %s", text)
	}
	var out ListAgentsOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(out.Agents))
	}
}
