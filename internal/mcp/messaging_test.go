package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

func newTestServer(t *testing.T, opts Options) (*MCPServer, registry.Store) {
	t.Helper()
	store := registry.NewMemoryStore()
	s := NewWithOptions(store, opts)
	return s, store
}

// registerAgentIn registers an agent through the MCP tool (needed because the
// memory store rejects deliveries to unknown agents).
func registerAgentIn(t *testing.T, s *MCPServer, id string) {
	t.Helper()
	resp := callTool(t, s, "register_agent", RegisterAgentInput{
		ID: id, PublicKey: validTestKey(t), Capabilities: []string{"test"},
	})
	if _, isErr := parseToolResult(t, resp); isErr {
		t.Fatalf("register_agent failed: %s", resp.Error.Message)
	}
}

func TestSendMessage_MergesReplyTo(t *testing.T) {
	s, store := newTestServer(t, Options{AgentID: "alice"})
	registerAgentIn(t, s, "alice")
	registerAgentIn(t, s, "bob")

	resp := callTool(t, s, "send_message", SendMessageInput{
		AgentID: "bob",
		Payload: map[string]any{"text": "hi"},
		ReplyTo: "corr-123",
	})
	_, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("send_message error: %s", resp.Error.Message)
	}

	entries, _, err := store.Retrieve("bob", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("bob inbox = %d entries", len(entries))
	}
	var payload map[string]any
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload["crier_reply_to"] != "corr-123" {
		t.Fatalf("payload missing crier_reply_to: %v", payload)
	}
	if payload["text"] != "hi" {
		t.Fatalf("payload lost original field: %v", payload)
	}
}

func TestGetMessages_OwnsLeaseAndAck(t *testing.T) {
	s, store := newTestServer(t, Options{AgentID: "alice"})
	registerAgentIn(t, s, "alice")

	// A question lands in alice's inbox directly through the store.
	raw := json.RawMessage(`{"kind":"question","text":"where is the lettuce?"}`)
	if err := store.Deliver("alice", &registry.InboxEntry{AgentID: "alice", Payload: raw}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	resp := callTool(t, s, "get_messages", GetMessagesInput{})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("get_messages error: %s", resp.Error.Message)
	}
	var out GetMessagesOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Payload["kind"] != "question" {
		t.Fatalf("messages = %+v", out.Messages)
	}

	// The bridge acked what it returned: the inbox is now empty.
	again, _, err := store.Retrieve("alice", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("inbox not empty after get_messages: %d", len(again))
	}
}

func TestAskAgent_CorrelationRoundTrip(t *testing.T) {
	// Two bridges (alice asks, bob answers) sharing one store: the durable
	// conversation is fully driven through the bridge tools.
	store := registry.NewMemoryStore()
	alice := NewWithOptions(store, Options{AgentID: "alice"})
	bob := NewWithOptions(store, Options{AgentID: "bob"})
	registerAgentIn(t, alice, "alice")
	registerAgentIn(t, bob, "bob")

	done := make(chan string, 1)
	go func() {
		resp := callTool(t, alice, "ask_agent", AskAgentInput{
			AgentID: "bob", Payload: map[string]any{"kind": "question", "text": "which plot is lettuce?"},
			TimeoutS: 15,
		})
		text, isErr := parseToolResult(t, resp)
		if isErr {
			done <- "ERR:" + resp.Error.Message
			return
		}
		done <- text
	}()

	// Bob's bridge sees the question in its inbox, extracts the correlation id
	// the bridge injected, and answers with reply_to.
	var corrID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, _, _ := store.Retrieve("bob", 30*time.Second, 10)
		for _, e := range entries {
			var m map[string]any
			_ = json.Unmarshal(e.Payload, &m)
			if m["kind"] == "question" {
				corrID, _ = m["crier_correlation_id"].(string)
			}
		}
		if corrID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if corrID == "" {
		t.Fatal("bob never received the question with a correlation id")
	}

	// Bob answers through his own bridge's send_message with reply_to.
	resp := callTool(t, bob, "send_message", SendMessageInput{
		AgentID: "alice",
		Payload: map[string]any{"kind": "answer", "text": "plot C is lettuce"},
		ReplyTo: corrID,
	})
	if _, isErr := parseToolResult(t, resp); isErr {
		t.Fatalf("bob send_message error: %s", resp.Error.Message)
	}

	select {
	case text := <-done:
		if strings.HasPrefix(text, "ERR:") {
			t.Fatalf("ask_agent failed: %s", text)
		}
		var out AskAgentOutput
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Response["text"] != "plot C is lettuce" {
			t.Fatalf("response = %+v", out.Response)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ask_agent never returned")
	}
}

func TestAskAgent_Timeout(t *testing.T) {
	s, store := newTestServer(t, Options{AgentID: "alice"})
	registerAgentIn(t, s, "alice")
	registerAgentIn(t, s, "bob")

	start := time.Now()
	resp := callTool(t, s, "ask_agent", AskAgentInput{
		AgentID: "bob", Payload: map[string]any{"text": "anyone there?"}, TimeoutS: 1,
	})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error on timeout")
	}
	if !strings.Contains(text, "timeout") {
		t.Fatalf("error = %s", text)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("returned too early")
	}
	_ = store
}

func TestMeshPeers_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"peers":[{"agent_id":"agent-a"},{"agent_id":"agent-b"}],"count":2}`))
	}))
	t.Cleanup(srv.Close)

	s, _ := newTestServer(t, Options{AgentID: "alice", HTTPURL: srv.URL})
	resp := callTool(t, s, "mesh_peers", map[string]any{})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("mesh_peers error: %s", resp.Error.Message)
	}
	var out MeshPeersOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Count != 2 || len(out.Peers) != 2 || out.Peers[0] != "agent-a" {
		t.Fatalf("peers = %+v", out)
	}
}

func TestMeshRequest_RequiresBridge(t *testing.T) {
	s, _ := newTestServer(t, Options{AgentID: "alice"}) // no MeshURL
	resp := callTool(t, s, "mesh_request", MeshRequestInput{
		Target: "bob", Method: "PING", Path: "/ping",
	})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error without mesh bridge configured")
	}
	if !strings.Contains(text, "CRIER_MESH_URL") {
		t.Fatalf("error = %s", text)
	}
}

func TestGetMessages_RequiresAgentID(t *testing.T) {
	s, _ := newTestServer(t, Options{}) // no identity
	resp := callTool(t, s, "get_messages", GetMessagesInput{})
	text, isErr := parseToolResult(t, resp)
	if !isErr {
		t.Fatal("expected error without agent identity")
	}
	if !strings.Contains(text, "CRIER_AGENT_ID") {
		t.Fatalf("error = %s", text)
	}
}
