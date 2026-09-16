package mcp

// Handler-level tests for the strict top-level arguments decode (DF-CRIER-190).
//
// These tests deliberately drive the tools through the JSON-RPC dispatch path
// (callTool / callToolWithArgs) and never touch the decode helper directly, so
// they are load-bearing: with the pre-fix handlers — which decoded with a bare
// json.Unmarshal and let an unknown member pass silently — every case in
// TestToolArgs_UnknownMemberRejectedByName fails.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// unknownMember is a member no tool's input schema declares.
const unknownMember = "totally_bogus_key"

// argsTestServer builds a server on which every one of the 13 tools is
// REACHABLE: the bridge has an identity (get_messages, ask_agent), an HTTP URL
// (mesh_peers) and a mesh bridge (mesh_request). Every handler's own
// precondition therefore passes, so the arguments decode is the first step that
// can fail — which is exactly what both tables need to observe.
func argsTestServer(t *testing.T) *MCPServer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"peers":[],"count":0}`))
	}))
	t.Cleanup(srv.Close)
	return NewWithOptions(registry.NewMemoryStore(), Options{
		AgentID: "bridge-self",
		HTTPURL: srv.URL,
		MeshURL: "ws://127.0.0.1:1/mesh/connect/bridge-self",
	})
}

// rawJSON marshals a value into the arguments object a tools/call carries.
func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	return b
}

// toolArgsCase is one advertised tool: how to bring the server into a state
// where a call is otherwise valid, and the valid arguments themselves.
type toolArgsCase struct {
	tool string
	// setup registers whatever the tool needs (agents, a live lease).
	setup func(t *testing.T, s *MCPServer)
	// args returns the arguments object carrying ONLY schema-declared members.
	args func(t *testing.T, s *MCPServer) json.RawMessage
	// wantOK is true when those valid arguments are expected to SUCCEED. When
	// false the call may still fail for a non-argument reason (ask_agent waits
	// for a reply that never comes; mesh_request has no reachable peer) — the
	// positive table then only requires the failure not to be an arguments
	// error.
	wantOK bool
}

// toolArgsCases covers all 13 advertised tools (spec §4).
func toolArgsCases(t *testing.T) []toolArgsCase {
	t.Helper()
	key := validTestKey(t)
	registerArgsAgent := func(t *testing.T, s *MCPServer) { registerAgentIn(t, s, "args-agent") }

	return []toolArgsCase{
		{
			tool: "register_agent",
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"id": "args-agent", "public_key": key})
			},
			wantOK: true,
		},
		{
			tool:   "list_agents",
			args:   func(*testing.T, *MCPServer) json.RawMessage { return json.RawMessage(`{}`) },
			wantOK: true,
		},
		{
			tool:  "get_agent",
			setup: registerArgsAgent,
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"id": "args-agent"})
			},
			wantOK: true,
		},
		{
			tool:  "unregister_agent",
			setup: func(t *testing.T, s *MCPServer) { registerAgentIn(t, s, "args-doomed") },
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"id": "args-doomed"})
			},
			wantOK: true,
		},
		{
			tool:  "deliver_message",
			setup: registerArgsAgent,
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"agent_id": "args-agent", "payload": map[string]any{"kind": "task"}})
			},
			wantOK: true,
		},
		{
			tool:  "retrieve_inbox",
			setup: registerArgsAgent,
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"agent_id": "args-agent", "max_messages": 5, "lease_seconds": 30})
			},
			wantOK: true,
		},
		{
			tool: "ack_messages",
			setup: func(t *testing.T, s *MCPServer) {
				registerArgsAgent(t, s)
				callTool(t, s, "deliver_message", DeliverMessageInput{AgentID: "args-agent", Payload: json.RawMessage(`{"kind":"task"}`)})
			},
			args: func(t *testing.T, s *MCPServer) json.RawMessage {
				text, isErr := parseToolResult(t, callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "args-agent", MaxMessages: 1}))
				if isErr {
					t.Fatalf("retrieve before ack: %s", text)
				}
				var out RetrieveInboxOutput
				if err := json.Unmarshal([]byte(text), &out); err != nil {
					t.Fatalf("decode retrieve: %v", err)
				}
				if len(out.Messages) == 0 {
					t.Fatal("no message to lease for ack_messages")
				}
				return rawJSON(t, map[string]any{
					"agent_id":    "args-agent",
					"lease_id":    out.LeaseID,
					"message_ids": []string{out.Messages[0].ID},
				})
			},
			wantOK: true,
		},
		{
			tool:  "inbox_stats",
			setup: registerArgsAgent,
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"agent_id": "args-agent"})
			},
			wantOK: true,
		},
		{
			tool:  "send_message",
			setup: registerArgsAgent,
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"agent_id": "args-agent", "payload": map[string]any{"kind": "task"}})
			},
			wantOK: true,
		},
		{
			tool: "get_messages",
			setup: func(t *testing.T, s *MCPServer) {
				registerAgentIn(t, s, "bridge-self") // the bridge's own inbox
			},
			args:   func(t *testing.T, _ *MCPServer) json.RawMessage { return rawJSON(t, map[string]any{"max": 5}) },
			wantOK: true,
		},
		{
			tool: "ask_agent",
			setup: func(t *testing.T, s *MCPServer) {
				registerArgsAgent(t, s)
				registerAgentIn(t, s, "bridge-self")
			},
			// timeout_s is short so a regression cannot stall the suite; no
			// reply ever arrives, so the call itself ends in a timeout error.
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"agent_id": "args-agent", "payload": map[string]any{"kind": "question"}, "timeout_s": 1})
			},
			wantOK: false,
		},
		{
			tool:   "mesh_peers",
			args:   func(*testing.T, *MCPServer) json.RawMessage { return json.RawMessage(`{}`) },
			wantOK: true,
		},
		{
			tool: "mesh_request",
			// No live mesh peer exists in-process, so the call fails at the
			// bridge — but only after its arguments were accepted.
			args: func(t *testing.T, _ *MCPServer) json.RawMessage {
				return rawJSON(t, map[string]any{"target": "args-agent", "method": "PING", "path": "/ping"})
			},
			wantOK: false,
		},
	}
}

// isArgumentsError reports whether a tool result is the strict-decode rejection.
func isArgumentsError(text string) bool {
	return strings.Contains(text, "invalid arguments")
}

// withUnknownMember re-marshals a valid arguments object with one member that
// no tool declares added, so the only defect in the call is that member.
func withUnknownMember(t *testing.T, valid json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(valid, &m); err != nil {
		t.Fatalf("valid arguments are not an object: %v (%s)", err, valid)
	}
	if _, clash := m[unknownMember]; clash {
		t.Fatalf("test name %q collides with a declared member", unknownMember)
	}
	m[unknownMember] = 12345
	return rawJSON(t, m)
}

// TestToolArgs_UnknownMemberRejectedByName is the load-bearing strictness test:
// every advertised tool must reject a member outside its input schema with an
// in-band error that names it, and must keep serving the next call.
func TestToolArgs_UnknownMemberRejectedByName(t *testing.T) {
	for _, tc := range toolArgsCases(t) {
		t.Run(tc.tool, func(t *testing.T) {
			s := argsTestServer(t)
			if tc.setup != nil {
				tc.setup(t, s)
			}
			valid := tc.args(t, s)

			resp := callToolWithArgs(t, s, tc.tool, withUnknownMember(t, valid))

			// AC2: a tool-level error (isError:true), never a JSON-RPC error.
			if resp.Error != nil {
				t.Fatalf("expected an in-band tool error, got a JSON-RPC error: %+v", resp.Error)
			}
			text, isErr := parseToolResult(t, resp)
			if !isErr {
				t.Fatalf("unknown member %q was silently accepted: %s", unknownMember, text)
			}
			if !isArgumentsError(text) {
				t.Errorf("error is not an arguments error: %s", text)
			}
			if !strings.Contains(text, unknownMember) {
				t.Errorf("error does not name the offending member %q: %s", unknownMember, text)
			}

			// AC2: the server survived and answers the next call.
			nextText, nextErr := parseToolResult(t, callTool(t, s, "list_agents", struct{}{}))
			if nextErr {
				t.Fatalf("server did not answer the call after a rejection: %s", nextText)
			}
		})
	}
}

// TestToolArgs_ValidArgumentsStillAccepted is the AC4 counterpart: schema-valid
// arguments are never rejected by the strict decode, and the tools that can
// succeed in-process still succeed.
func TestToolArgs_ValidArgumentsStillAccepted(t *testing.T) {
	for _, tc := range toolArgsCases(t) {
		t.Run(tc.tool, func(t *testing.T) {
			s := argsTestServer(t)
			if tc.setup != nil {
				tc.setup(t, s)
			}
			text, isErr := parseToolResult(t, callToolWithArgs(t, s, tc.tool, tc.args(t, s)))
			if isArgumentsError(text) {
				t.Fatalf("valid arguments were rejected: %s", text)
			}
			if tc.wantOK && isErr {
				t.Fatalf("expected success, got error: %s", text)
			}
		})
	}
}

// TestToolArgs_MisspelledMembersRejected pins the exact live repro: a typo'd
// argument used to succeed with a silently substituted default.
func TestToolArgs_MisspelledMembersRejected(t *testing.T) {
	cases := []struct {
		tool    string
		setup   func(t *testing.T, s *MCPServer)
		args    map[string]any
		member  string
		ignored string // what the pre-fix server did with the member
	}{
		{
			tool:    "retrieve_inbox",
			setup:   func(t *testing.T, s *MCPServer) { registerAgentIn(t, s, "args-agent") },
			args:    map[string]any{"agent_id": "args-agent", "max_messges": 1},
			member:  "max_messges",
			ignored: "used max_messages=10 (its default)",
		},
		{
			tool: "ask_agent",
			setup: func(t *testing.T, s *MCPServer) {
				registerAgentIn(t, s, "args-agent")
				registerAgentIn(t, s, "bridge-self")
			},
			args:    map[string]any{"agent_id": "args-agent", "payload": map[string]any{}, "timeout_ms": 1000},
			member:  "timeout_ms",
			ignored: "waited the 30s default instead of 1s",
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			s := argsTestServer(t)
			tc.setup(t, s)
			text, isErr := parseToolResult(t, callToolWithArgs(t, s, tc.tool, rawJSON(t, tc.args)))
			if !isErr {
				t.Fatalf("%s was silently accepted (%s): %s", tc.member, tc.ignored, text)
			}
			if !strings.Contains(text, tc.member) {
				t.Fatalf("error does not name %q: %s", tc.member, text)
			}
		})
	}
}

// TestToolArgs_NestedOpaqueValuesAccepted proves strictness stops at the top
// level (AC3): unknown keys inside payload / body belong to the caller.
func TestToolArgs_NestedOpaqueValuesAccepted(t *testing.T) {
	s := argsTestServer(t)
	registerAgentIn(t, s, "args-agent")

	// deliver_message: the opaque payload keeps its unknown keys through to the
	// store — acceptance alone would not prove the bytes survived.
	deliverArgs := rawJSON(t, map[string]any{
		"agent_id": "args-agent",
		"payload": map[string]any{
			"kind":                   "task",
			"unknown_inside_payload": map[string]any{"deep_unknown": 1},
			"another_unknown":        "kept",
		},
	})
	text, isErr := parseToolResult(t, callToolWithArgs(t, s, "deliver_message", deliverArgs))
	if isErr {
		t.Fatalf("deliver_message rejected a valid call with an opaque payload: %s", text)
	}

	entries, _, err := s.store.Retrieve("args-agent", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 delivered entry, got %d", len(entries))
	}
	var stored map[string]any
	if err := json.Unmarshal(entries[0].Payload, &stored); err != nil {
		t.Fatalf("decode stored payload: %v", err)
	}
	if _, ok := stored["unknown_inside_payload"]; !ok {
		t.Errorf("payload lost its unknown nested key: %v", stored)
	}
	if stored["another_unknown"] != "kept" {
		t.Errorf("payload unknown member was not passed through: %v", stored)
	}

	// send_message carries the payload as a map — same contract.
	sendArgs := rawJSON(t, map[string]any{
		"agent_id": "args-agent",
		"payload":  map[string]any{"unknown_inside_payload": 1},
	})
	if text, isErr := parseToolResult(t, callToolWithArgs(t, s, "send_message", sendArgs)); isErr {
		t.Fatalf("send_message rejected an opaque payload: %s", text)
	}

	// ask_agent: an unknown nested key is fine; the call fails only when its
	// wait times out, never as an arguments error.
	registerAgentIn(t, s, "bridge-self")
	askArgs := rawJSON(t, map[string]any{
		"agent_id":  "args-agent",
		"payload":   map[string]any{"unknown_inside_payload": map[string]any{"deep": 1}},
		"timeout_s": 1,
	})
	if text, _ := parseToolResult(t, callToolWithArgs(t, s, "ask_agent", askArgs)); isArgumentsError(text) {
		t.Fatalf("ask_agent rejected an opaque payload: %s", text)
	}

	// mesh_request.body is declared as opaque JSON.
	meshArgs := rawJSON(t, map[string]any{
		"target": "args-agent", "method": "PING", "path": "/ping",
		"body": map[string]any{"unknown_inside_body": map[string]any{"deep": 1}},
	})
	if text, _ := parseToolResult(t, callToolWithArgs(t, s, "mesh_request", meshArgs)); isArgumentsError(text) {
		t.Fatalf("mesh_request rejected an opaque body: %s", text)
	}
}

// TestNoArgumentToolsRejectAnyMember covers the two tools whose schema declares
// no properties at all, plus the empty-input forms that must still succeed.
func TestNoArgumentToolsRejectAnyMember(t *testing.T) {
	for _, tool := range []string{"list_agents", "mesh_peers"} {
		t.Run(tool, func(t *testing.T) {
			s := argsTestServer(t)

			for _, args := range []json.RawMessage{
				rawJSON(t, map[string]any{unknownMember: 1}),
				json.RawMessage(`{"max":1}`), // a real argument of ANOTHER tool
			} {
				text, isErr := parseToolResult(t, callToolWithArgs(t, s, tool, args))
				if !isErr {
					t.Errorf("arguments %s were silently accepted: %s", args, text)
					continue
				}
				if !isArgumentsError(text) {
					t.Errorf("expected an arguments error for %s, got: %s", args, text)
				}
			}

			for _, args := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{}`), json.RawMessage("  {}  ")} {
				text, isErr := parseToolResult(t, callToolWithArgs(t, s, tool, args))
				if isErr {
					t.Errorf("empty arguments %q were rejected: %s", args, text)
				}
			}
		})
	}
}
