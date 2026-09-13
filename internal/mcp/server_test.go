package mcp

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// validTestKey returns a hex-encoded ed25519 public key (64 hex chars).
func validTestKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return hex.EncodeToString(pub)
}

// callTool sends a tools/call JSON-RPC request to the server and returns the response.
func callTool(t *testing.T, s *MCPServer, tool string, args any) *jsonRPCResponse {
	t.Helper()
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	callParams, _ := json.Marshal(toolsCallParams{Name: tool, Arguments: argsJSON})
	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  callParams,
		ID:      1,
	}
	return s.dispatch(t.Context(), req)
}

func parseToolResult(t *testing.T, resp *jsonRPCResponse) (string, bool) {
	t.Helper()
	resultJSON, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var result toolsCallResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(result.Content) == 0 {
		return "", result.IsError
	}
	return result.Content[0].Text, result.IsError
}

// =============================================================================
// register_agent tests (spec §9.2)
// =============================================================================

func TestRegisterAgent_Valid(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)

	resp := callTool(t, s, "register_agent", RegisterAgentInput{
		ID:           "agent-1",
		PublicKey:    key,
		Capabilities: []string{"coding", "deploy"},
	})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var agent registry.Agent
	if err := json.Unmarshal([]byte(text), &agent); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	if agent.ID != "agent-1" {
		t.Errorf("expected id agent-1, got %s", agent.ID)
	}
	if agent.Status != registry.StatusOnline {
		t.Errorf("expected status online, got %s", agent.Status)
	}
	if len(agent.Capabilities) != 2 {
		t.Errorf("expected 2 capabilities, got %d", len(agent.Capabilities))
	}
}

func TestRegisterAgent_Duplicate(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)

	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})
	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: validTestKey(t)})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for duplicate registration")
	}
	if !strings.Contains(text, "agent already registered") {
		t.Errorf("expected 'agent already registered', got: %s", text)
	}
}

func TestRegisterAgent_InvalidHexKey(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: "too-short"})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for invalid hex key")
	}
	if !strings.Contains(text, "64 hex characters") {
		t.Errorf("expected '64 hex characters', got: %s", text)
	}
}

// =============================================================================
// list_agents tests (spec §9.2)
// =============================================================================

func TestListAgents_Empty(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	resp := callTool(t, s, "list_agents", struct{}{})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out ListAgentsOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Agents) != 0 {
		t.Errorf("expected 0 agents, got %d", len(out.Agents))
	}
}

func TestListAgents_TwoRegistered(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: validTestKey(t)})
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-2", PublicKey: validTestKey(t)})

	resp := callTool(t, s, "list_agents", struct{}{})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out ListAgentsOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(out.Agents))
	}
}

// =============================================================================
// get_agent tests (spec §9.2)
// =============================================================================

func TestGetAgent_Existing(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	resp := callTool(t, s, "get_agent", GetAgentInput{ID: "agent-1"})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var agent registry.Agent
	if err := json.Unmarshal([]byte(text), &agent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if agent.ID != "agent-1" {
		t.Errorf("expected id agent-1, got %s", agent.ID)
	}
}

func TestGetAgent_NotFound(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	resp := callTool(t, s, "get_agent", GetAgentInput{ID: "nonexistent"})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for nonexistent agent")
	}
	if !strings.Contains(text, "agent not found") {
		t.Errorf("expected 'agent not found', got: %s", text)
	}
}

// =============================================================================
// unregister_agent tests (spec §9.2)
// =============================================================================

func TestUnregisterAgent_Existing(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	resp := callTool(t, s, "unregister_agent", UnregisterAgentInput{ID: "agent-1"})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out UnregisterAgentOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Unregistered != "agent-1" {
		t.Errorf("expected unregistered agent-1, got %s", out.Unregistered)
	}

	// Verify agent is gone
	resp2 := callTool(t, s, "get_agent", GetAgentInput{ID: "agent-1"})
	_, isErr2 := parseToolResult(t, resp2)
	if !isErr2 {
		t.Fatal("expected not-found after unregister")
	}
}

func TestUnregisterAgent_NotFound(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	resp := callTool(t, s, "unregister_agent", UnregisterAgentInput{ID: "nonexistent"})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for nonexistent agent")
	}
	if !strings.Contains(text, "agent not found") {
		t.Errorf("expected 'agent not found', got: %s", text)
	}
}

// =============================================================================
// deliver_message tests (spec §9.2)
// =============================================================================

func TestDeliverMessage_ToExistingAgent(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	resp := callTool(t, s, "deliver_message", DeliverMessageInput{
		AgentID: "agent-1",
		Payload: json.RawMessage(`{"type":"task","body":"build scheduler"}`),
	})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out DeliverMessageOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.AgentID != "agent-1" {
		t.Errorf("expected agent_id agent-1, got %s", out.AgentID)
	}
	if out.MessageID == "" {
		t.Error("expected non-empty message_id")
	}
}

func TestDeliverMessage_ToNonexistentAgent(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	resp := callTool(t, s, "deliver_message", DeliverMessageInput{
		AgentID: "nonexistent",
		Payload: json.RawMessage(`{}`),
	})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for nonexistent agent")
	}
	if !strings.Contains(text, "agent not found") {
		t.Errorf("expected 'agent not found', got: %s", text)
	}
}

// =============================================================================
// retrieve_inbox tests (spec §9.2)
// =============================================================================

func TestRetrieveInbox_Empty(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	resp := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1"})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out RetrieveInboxOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Messages) != 0 {
		t.Errorf("expected 0 messages, got %d", len(out.Messages))
	}
	if out.LeaseID == "" {
		t.Error("expected non-empty lease_id")
	}
}

func TestRetrieveInbox_OneMessage(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})
	callTool(t, s, "deliver_message", DeliverMessageInput{
		AgentID: "agent-1",
		Payload: json.RawMessage(`{"type":"test"}`),
	})

	resp := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 10})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out RetrieveInboxOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Errorf("expected 1 message, got %d", len(out.Messages))
	}
	if out.Messages[0].LeaseID == "" {
		t.Error("expected leased message")
	}
}

func TestRetrieveInbox_MaxMessagesCap(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	for i := 0; i < 15; i++ {
		callTool(t, s, "deliver_message", DeliverMessageInput{
			AgentID: "agent-1",
			Payload: json.RawMessage(`{}`),
		})
	}

	resp := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 3})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out RetrieveInboxOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Errorf("expected 3 messages (max_messages cap), got %d", len(out.Messages))
	}
}

func TestRetrieveInbox_RejectsMaxAbove100(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	resp := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 101})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for max_messages > 100")
	}
	if !strings.Contains(text, "max_messages must be <= 100") {
		t.Errorf("expected 'max_messages must be <= 100', got: %s", text)
	}
}

// =============================================================================
// ack_messages tests (spec §9.2)
// =============================================================================

func TestAckMessages_OneMessage(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})
	callTool(t, s, "deliver_message", DeliverMessageInput{
		AgentID: "agent-1",
		Payload: json.RawMessage(`{}`),
	})

	retResp := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 10})
	retText, _ := parseToolResult(t, retResp)
	var retOut RetrieveInboxOutput
	json.Unmarshal([]byte(retText), &retOut)

	if len(retOut.Messages) == 0 {
		t.Fatal("expected message in inbox")
	}

	ackResp := callTool(t, s, "ack_messages", AckMessagesInput{
		AgentID:    "agent-1",
		LeaseID:    retOut.LeaseID,
		MessageIDs: []string{retOut.Messages[0].ID},
	})
	ackText, isErr := parseToolResult(t, ackResp)
	if isErr {
		t.Fatalf("expected success, got error: %s", ackText)
	}

	var ackOut AckMessagesOutput
	if err := json.Unmarshal([]byte(ackText), &ackOut); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ackOut.Acked != 1 {
		t.Errorf("expected 1 acked, got %d", ackOut.Acked)
	}

	// Verify inbox is now empty
	verifyResp := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 10})
	verifyText, _ := parseToolResult(t, verifyResp)
	var verifyOut RetrieveInboxOutput
	json.Unmarshal([]byte(verifyText), &verifyOut)
	if len(verifyOut.Messages) != 0 {
		t.Errorf("expected 0 messages after ACK, got %d", len(verifyOut.Messages))
	}
}

func TestAckMessages_WrongLease(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})
	callTool(t, s, "deliver_message", DeliverMessageInput{
		AgentID: "agent-1",
		Payload: json.RawMessage(`{}`),
	})

	retResp := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 10})
	retText, _ := parseToolResult(t, retResp)
	var retOut RetrieveInboxOutput
	json.Unmarshal([]byte(retText), &retOut)

	if len(retOut.Messages) == 0 {
		t.Fatal("expected message in inbox")
	}

	ackResp := callTool(t, s, "ack_messages", AckMessagesInput{
		AgentID:    "agent-1",
		LeaseID:    "wrong-lease-id",
		MessageIDs: []string{retOut.Messages[0].ID},
	})
	ackText, isErr := parseToolResult(t, ackResp)
	if !isErr {
		t.Fatal("expected error for wrong lease")
	}
	if !strings.Contains(ackText, "not leased") {
		t.Errorf("expected lease conflict message, got: %s", ackText)
	}
}

func TestAckMessages_EmptyMessageIDs(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	resp := callTool(t, s, "ack_messages", AckMessagesInput{
		AgentID:    "agent-1",
		LeaseID:    "some-lease",
		MessageIDs: []string{},
	})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for empty message_ids")
	}
	if !strings.Contains(text, "message_ids must not be empty") {
		t.Errorf("expected 'message_ids must not be empty', got: %s", text)
	}
}

// =============================================================================
// inbox_stats tests (spec §9.2)
// =============================================================================

func TestInboxStats_Empty(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	resp := callTool(t, s, "inbox_stats", InboxStatsInput{AgentID: "agent-1"})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out InboxStatsOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.QueueDepth != 0 {
		t.Errorf("expected queue_depth 0, got %d", out.QueueDepth)
	}
	if out.LeasedCount != 0 {
		t.Errorf("expected leased_count 0, got %d", out.LeasedCount)
	}
}

func TestInboxStats_WithMessages(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	// Deliver 5 messages
	for i := 0; i < 5; i++ {
		callTool(t, s, "deliver_message", DeliverMessageInput{
			AgentID: "agent-1",
			Payload: json.RawMessage(`{}`),
		})
	}

	// Retrieve 3 (leased, not ACKed yet) — all 5 still in queue, 3 are leased
	callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 3})

	resp := callTool(t, s, "inbox_stats", InboxStatsInput{AgentID: "agent-1"})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	var out InboxStatsOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Stats counts all non-ACKed, non-expired — including leased ones
	if out.QueueDepth != 5 {
		t.Errorf("expected queue_depth 5 (all non-ACKed), got %d", out.QueueDepth)
	}
	if out.LeasedCount != 3 {
		t.Errorf("expected leased_count 3, got %d", out.LeasedCount)
	}
}

func TestInboxStats_NonExistentAgent(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	resp := callTool(t, s, "inbox_stats", InboxStatsInput{AgentID: "nonexistent"})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for nonexistent agent")
	}
	if !strings.Contains(text, "agent not found") {
		t.Errorf("expected 'agent not found', got: %s", text)
	}
}

// =============================================================================
// server lifecycle tests (spec §9.2)
// =============================================================================

func TestInitialize(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	params, _ := json.Marshal(initializeParams{ProtocolVersion: "2024-11-05"})
	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "initialize",
		Params:  params,
		ID:      1,
	}
	resp := s.dispatch(t.Context(), req)

	if resp.Error != nil {
		t.Fatalf("expected success, got error: %s", resp.Error.Message)
	}

	resultJSON, _ := json.Marshal(resp.Result)
	var result initializeResult
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.ServerInfo.Name != "crier-mcp" {
		t.Errorf("expected server name crier-mcp, got %s", result.ServerInfo.Name)
	}
	if result.ServerInfo.Version != "0.1.0" {
		t.Errorf("expected version 0.1.0, got %s", result.ServerInfo.Version)
	}
	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("expected protocol 2024-11-05, got %s", result.ProtocolVersion)
	}
}

func TestToolsList(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "tools/list",
		Params:  nil,
		ID:      2,
	}
	resp := s.dispatch(t.Context(), req)

	resultJSON, _ := json.Marshal(resp.Result)
	var result map[string]json.RawMessage
	if err := json.Unmarshal(resultJSON, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	var tools []toolDefinition
	if err := json.Unmarshal(result["tools"], &tools); err != nil {
		t.Fatalf("unmarshal tools array: %v", err)
	}
	if len(tools) != 13 {
		t.Fatalf("expected 13 tools, got %d", len(tools))
	}
}

func TestUnknownMethod(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "nonexistent/method",
		Params:  nil,
		ID:      3,
	}
	resp := s.dispatch(t.Context(), req)

	if resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("expected code -32601, got %d", resp.Error.Code)
	}
}

func TestMalformedJSON_Error(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	// Simulate a malformed request via dispatch (unmarshal will fail)
	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":}`), // malformed
		ID:      4,
	}
	resp := s.dispatch(t.Context(), req)

	// Should return a JSON-RPC error, not a tools/call result
	if resp.Error == nil {
		t.Fatal("expected error for malformed params")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("expected code -32602, got %d", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "invalid") {
		t.Errorf("expected 'invalid' in error, got: %s", resp.Error.Message)
	}
}

// =============================================================================
// concurrency test (spec §9.2)
// =============================================================================

func TestConcurrentRetrieve_DisjointMessages(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})

	// Deliver 6 messages
	for i := 0; i < 6; i++ {
		callTool(t, s, "deliver_message", DeliverMessageInput{
			AgentID: "agent-1",
			Payload: json.RawMessage(`{}`),
		})
	}

	// Two retrievals — should get disjoint sets
	resp1 := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 3})
	resp2 := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 3})

	text1, _ := parseToolResult(t, resp1)
	text2, _ := parseToolResult(t, resp2)

	var out1, out2 RetrieveInboxOutput
	json.Unmarshal([]byte(text1), &out1)
	json.Unmarshal([]byte(text2), &out2)

	// Each should get 3
	if len(out1.Messages) != 3 {
		t.Errorf("retrieval 1: expected 3 messages, got %d", len(out1.Messages))
	}
	if len(out2.Messages) != 3 {
		t.Errorf("retrieval 2: expected 3 messages, got %d", len(out2.Messages))
	}

	// Messages should be disjoint (different IDs)
	idSet := make(map[string]bool)
	for _, m := range out1.Messages {
		idSet[m.ID] = true
	}
	for _, m := range out2.Messages {
		if idSet[m.ID] {
			t.Errorf("duplicate message ID across retrievals: %s", m.ID)
		}
	}
}

// =============================================================================
// lease expiry test (spec §9.2)
// =============================================================================

func TestLeaseExpiry_UnackedReturns(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)
	callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: key})
	callTool(t, s, "deliver_message", DeliverMessageInput{
		AgentID: "agent-1",
		Payload: json.RawMessage(`{"type":"lease-test"}`),
	})

	// Retrieve with 1-second lease
	resp := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 10, LeaseSeconds: 1})
	text, _ := parseToolResult(t, resp)
	var out RetrieveInboxOutput
	json.Unmarshal([]byte(text), &out)
	if len(out.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out.Messages))
	}

	// Wait for lease to expire
	time.Sleep(1500 * time.Millisecond)

	// PurgeExpired returns expired leases to unleased state
	store.PurgeExpired()

	// Retrieve again — message should return to queue
	resp2 := callTool(t, s, "retrieve_inbox", RetrieveInboxInput{AgentID: "agent-1", MaxMessages: 10})
	text2, _ := parseToolResult(t, resp2)
	var out2 RetrieveInboxOutput
	json.Unmarshal([]byte(text2), &out2)

	if len(out2.Messages) != 1 {
		t.Fatalf("expected message to return after lease expiry, got %d messages", len(out2.Messages))
	}
	if out2.Messages[0].ID != out.Messages[0].ID {
		t.Errorf("expected same message ID after expiry")
	}
}

// =============================================================================
// coverage tests (COV-003)
// =============================================================================

func TestMcpErrorStorageUnavailable(t *testing.T) {
	code, message := mcpError(fmt.Errorf("postgres storage is unavailable"))
	if code != -32603 {
		t.Errorf("expected code -32603, got %d", code)
	}
	if message != "registry storage unavailable" {
		t.Errorf("expected 'registry storage unavailable', got: %s", message)
	}
}

func TestMcpErrorDefaultPassThrough(t *testing.T) {
	code, message := mcpError(fmt.Errorf("something weird happened"))
	if code != -32602 {
		t.Errorf("expected code -32602, got %d", code)
	}
	if message != "something weird happened" {
		t.Errorf("expected passthrough message, got: %s", message)
	}
}

func TestDispatchNotification(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
		Params:  nil,
		ID:      5,
	}
	resp := s.dispatch(t.Context(), req)
	if resp != nil {
		t.Fatalf("expected nil response for notification, got: %+v", resp)
	}
}

func TestHandleToolsCallUnknownTool(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	args, _ := json.Marshal(toolsCallParams{Name: "nonexistent_tool", Arguments: json.RawMessage(`{}`)})
	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  args,
		ID:      6,
	}
	resp := s.dispatch(t.Context(), req)
	if resp.Error == nil {
		t.Fatal("expected error for unknown tool")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("expected code -32602, got %d", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "unknown tool") {
		t.Errorf("expected 'unknown tool' in error, got: %s", resp.Error.Message)
	}
}

func TestRegisterAgentBlankID(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)
	key := validTestKey(t)

	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "", PublicKey: key})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for blank id")
	}
	if !strings.Contains(text, "id is required") {
		t.Errorf("expected 'id is required', got: %s", text)
	}
}

func TestRegisterAgentTooShortKey(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "agent-1", PublicKey: "aabbcc"})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for too short key")
	}
	if !strings.Contains(text, "64 hex characters") {
		t.Errorf("expected '64 hex characters', got: %s", text)
	}
}

func TestDeliverMessageBlankAgentID(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	resp := callTool(t, s, "deliver_message", DeliverMessageInput{
		AgentID: "",
		Payload: json.RawMessage(`{}`),
	})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for blank agent_id")
	}
	if !strings.Contains(text, "agent_id is required") {
		t.Errorf("expected 'agent_id is required', got: %s", text)
	}
}

func TestDeliverMessageInvalidPayload(t *testing.T) {
	store := registry.NewMemoryStore()
	s := New(store)

	// Omit payload entirely to trigger the empty-payload validation path.
	args, _ := json.Marshal(map[string]any{"agent_id": "agent-1"})
	resp := callToolWithArgs(t, s, "deliver_message", args)
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error for invalid payload")
	}
	if !strings.Contains(text, "payload must be a JSON object") {
		t.Errorf("expected 'payload must be a JSON object', got: %s", text)
	}
}

func callToolWithArgs(t *testing.T, s *MCPServer, tool string, args json.RawMessage) *jsonRPCResponse {
	t.Helper()
	callParams, _ := json.Marshal(toolsCallParams{Name: tool, Arguments: args})
	req := &jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params:  callParams,
		ID:      1,
	}
	return s.dispatch(t.Context(), req)
}

func TestServeHappyPath(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	store := registry.NewMemoryStore()
	s := New(store)
	s.stdin = bufio.NewScanner(stdinR)
	s.stdout = json.NewEncoder(stdoutW)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		defer stdinW.Close()
		initReq, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", Method: "initialize", Params: mustMarshalJSON(t, initializeParams{ProtocolVersion: "2024-11-05"}), ID: 1})
		listReq, _ := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", Method: "tools/list", ID: 2})
		fmt.Fprintln(stdinW, string(initReq))
		fmt.Fprintln(stdinW, string(listReq))
		// Give Serve time to process before EOF closes stdin.
		time.Sleep(100 * time.Millisecond)
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Serve(ctx)
	}()

	reader := bufio.NewReader(stdoutR)
	responses := make([]jsonRPCResponse, 0, 2)
	for len(responses) < 2 {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			break
		}
		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err == nil {
			responses = append(responses, resp)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop after context cancel")
	}

	if len(responses) < 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}
	if responses[0].Error != nil {
		t.Fatalf("initialize returned error: %+v", responses[0].Error)
	}
	if responses[1].Error != nil {
		t.Fatalf("tools/list returned error: %+v", responses[1].Error)
	}
	if responses[1].Result == nil {
		t.Fatal("tools/list returned nil result")
	}
}

func mustMarshalJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
