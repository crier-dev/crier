package mcp

// Bridge-level messaging tools. These are the harness-facing surface:
// a harness calls send_message / get_messages / ask_agent and the bridge
// owns every transport detail (lease lifecycle, acks, correlation ids,
// polling). The mesh tools (mesh_peers / mesh_request) expose the live lane
// with the same property: the harness never sees a WebSocket frame.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// mergeBridgeField adds key=value to payload unless already present.
func mergeBridgeField(payload map[string]any, key, value string) map[string]any {
	out := make(map[string]any, len(payload)+1)
	for k, v := range payload {
		out[k] = v
	}
	if _, exists := out[key]; !exists {
		out[key] = value
	}
	return out
}

// payloadFromMap serializes a payload object for store delivery.
func payloadFromMap(payload map[string]any) (json.RawMessage, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode payload: %w", err)
	}
	return raw, nil
}

// parseEntryPayload decodes an inbox entry payload into a map.
func parseEntryPayload(entry *registry.InboxEntry) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(entry.Payload, &m); err != nil {
		m = map[string]any{"raw": string(entry.Payload)}
	}
	return m
}

// requireAgentID returns an error when the bridge has no agent identity.
func (s *MCPServer) requireAgentID() error {
	if s.agentID == "" {
		return fmt.Errorf("bridge has no agent identity: set CRIER_AGENT_ID")
	}
	return nil
}

// ---- send_message ---------------------------------------------------------

// handleSendMessage delivers a message to an agent's inbox. When reply_to is
// set, the payload gains a crier_reply_to field for correlation.
func (s *MCPServer) handleSendMessage(args json.RawMessage) (any, error) {
	var in SendMessageInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if in.Payload == nil {
		return nil, fmt.Errorf("payload must be a JSON object")
	}
	payload := in.Payload
	if in.ReplyTo != "" {
		payload = mergeBridgeField(payload, "crier_reply_to", in.ReplyTo)
	}
	raw, err := payloadFromMap(payload)
	if err != nil {
		return nil, err
	}
	entry := &registry.InboxEntry{
		AgentID: in.AgentID,
		Payload: raw,
	}
	if err := s.store.Deliver(in.AgentID, entry); err != nil {
		return nil, err
	}
	return SendMessageOutput{MessageID: entry.ID, AgentID: in.AgentID}, nil
}

// ---- get_messages ----------------------------------------------------------

// handleGetMessages retrieves the bridge's own inbox and acknowledges
// everything it returns. The harness never sees leases or acks.
func (s *MCPServer) handleGetMessages(args json.RawMessage) (any, error) {
	if err := s.requireAgentID(); err != nil {
		return nil, err
	}
	var in GetMessagesInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	max := in.Max
	if max <= 0 {
		max = 10
	}

	// Buffered messages first (pulled by ask_agent polling, unseen so far).
	s.mu.Lock()
	buf := s.buffered
	s.buffered = nil
	s.mu.Unlock()

	entries, leaseID, err := s.store.Retrieve(s.agentID, 30*time.Second, max)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	if len(ids) > 0 {
		_ = s.store.Ack(s.agentID, leaseID, ids) // best-effort: ack what we return
	}

	messages := make([]MessageView, 0, len(buf)+len(entries))
	for _, e := range buf {
		messages = append(messages, MessageView{ID: e.ID, Payload: parseEntryPayload(e)})
	}
	for _, e := range entries {
		messages = append(messages, MessageView{ID: e.ID, Payload: parseEntryPayload(e)})
	}
	return GetMessagesOutput{Messages: messages}, nil
}

// ---- ask_agent -------------------------------------------------------------

// handleAskAgent is a blocking request/reply over the durable inbox. The
// bridge delivers the payload (merged with crier_correlation_id), then polls
// its own inbox for a reply whose crier_reply_to matches. Non-matching
// messages are buffered so the harness still sees them via get_messages.
func (s *MCPServer) handleAskAgent(args json.RawMessage) (any, error) {
	if err := s.requireAgentID(); err != nil {
		return nil, err
	}
	var in AskAgentInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if in.Payload == nil {
		return nil, fmt.Errorf("payload must be a JSON object")
	}
	timeoutS := in.TimeoutS
	if timeoutS <= 0 {
		timeoutS = 30
	}
	if timeoutS > 300 {
		timeoutS = 300
	}

	correlationID := randomID()
	payload := mergeBridgeField(in.Payload, "crier_correlation_id", correlationID)
	raw, err := payloadFromMap(payload)
	if err != nil {
		return nil, err
	}
	entry := &registry.InboxEntry{AgentID: in.AgentID, Payload: raw}
	if err := s.store.Deliver(in.AgentID, entry); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(time.Duration(timeoutS) * time.Second)
	for time.Now().Before(deadline) {
		entries, leaseID, err := s.store.Retrieve(s.agentID, 30*time.Second, 20)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(entries))
		var reply *registry.InboxEntry
		for _, e := range entries {
			ids = append(ids, e.ID)
			m := parseEntryPayload(e)
			if m["crier_reply_to"] == correlationID {
				reply = e
			}
		}
		if len(ids) > 0 {
			_ = s.store.Ack(s.agentID, leaseID, ids)
		}
		if reply != nil {
			return AskAgentOutput{
				MessageID: reply.ID,
				Response:  parseEntryPayload(reply),
			}, nil
		}
		// Keep any other messages for the harness to see later.
		if len(entries) > 0 {
			s.mu.Lock()
			s.buffered = append(s.buffered, entries...)
			s.mu.Unlock()
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil, fmt.Errorf("ask_agent timeout: no reply from %q within %ds", in.AgentID, timeoutS)
}

// ---- mesh_peers -------------------------------------------------------------

// handleMeshPeers lists the agents currently connected to the mesh, via the
// server's HTTP API.
func (s *MCPServer) handleMeshPeers(args json.RawMessage) (any, error) {
	if s.httpURL == "" {
		return nil, fmt.Errorf("mesh_peers requires CRIER_HTTP_URL")
	}
	var out struct {
		Peers []struct {
			AgentID string `json:"agent_id"`
		} `json:"peers"`
		Count int `json:"count"`
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(s.httpURL + "/mesh/peers")
	if err != nil {
		return nil, fmt.Errorf("mesh peers: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mesh peers: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("mesh peers: decode: %w", err)
	}
	peers := make([]string, 0, len(out.Peers))
	for _, p := range out.Peers {
		peers = append(peers, p.AgentID)
	}
	return MeshPeersOutput{Peers: peers, Count: len(peers)}, nil
}

// ---- mesh_request ------------------------------------------------------------

// handleMeshRequest is a live REQUEST/RESPONSE round-trip over the mesh,
// through the bridge's own WebSocket connection.
func (s *MCPServer) handleMeshRequest(args json.RawMessage) (any, error) {
	if s.bridge == nil {
		return nil, fmt.Errorf("mesh_request requires CRIER_MESH_URL and CRIER_AGENT_ID")
	}
	var in MeshRequestInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.Target == "" || in.Method == "" || in.Path == "" {
		return nil, fmt.Errorf("target, method and path are required")
	}
	reply, err := s.bridge.request(in.Target, in.Method, in.Path, in.Body, in.TimeoutMs)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if len(reply.body) > 0 {
		_ = json.Unmarshal(reply.body, &body)
	}
	return MeshRequestOutput{StatusCode: reply.status, Body: body, TraceID: reply.trace}, nil
}
