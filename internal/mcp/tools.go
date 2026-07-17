package mcp

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/totalwindupflightsystems/crier/internal/registry"
)

// handleRegisterAgent — spec §4.1, Appendix B.
func (s *MCPServer) handleRegisterAgent(args json.RawMessage) (any, error) {
	var in RegisterAgentInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	if in.PublicKey == "" {
		return nil, fmt.Errorf("public_key is required")
	}
	rawKey, err := hex.DecodeString(in.PublicKey)
	if err != nil || len(rawKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public_key must be 64 hex characters (ed25519)")
	}
	agent := &registry.Agent{
		ID:           in.ID,
		PublicKey:    registry.HexKey(rawKey),
		Capabilities: in.Capabilities,
	}
	if agent.Capabilities == nil {
		agent.Capabilities = []string{}
	}
	if err := s.store.Register(agent); err != nil {
		return nil, err
	}
	return agent, nil
}

// handleListAgents — spec §4.2.
func (s *MCPServer) handleListAgents(args json.RawMessage) (any, error) {
	agents := s.store.List()
	if agents == nil {
		agents = []*registry.Agent{}
	}
	return ListAgentsOutput{Agents: agents}, nil
}

// handleGetAgent — spec §4.3.
func (s *MCPServer) handleGetAgent(args json.RawMessage) (any, error) {
	var in GetAgentInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	agent, err := s.store.Get(in.ID)
	if err != nil {
		return nil, err
	}
	return agent, nil
}

// handleUnregisterAgent — spec §4.4.
func (s *MCPServer) handleUnregisterAgent(args json.RawMessage) (any, error) {
	var in UnregisterAgentInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.ID == "" {
		return nil, fmt.Errorf("id is required")
	}
	if err := s.store.Unregister(in.ID); err != nil {
		return nil, err
	}
	return UnregisterAgentOutput{Unregistered: in.ID}, nil
}

// handleDeliverMessage — spec §4.5.
func (s *MCPServer) handleDeliverMessage(args json.RawMessage) (any, error) {
	var in DeliverMessageInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if len(in.Payload) == 0 {
		return nil, fmt.Errorf("payload must be a JSON object")
	}

	entry := &registry.InboxEntry{
		AgentID: in.AgentID,
		Payload: []byte(in.Payload),
	}
	if err := s.store.Deliver(in.AgentID, entry); err != nil {
		return nil, err
	}
	return DeliverMessageOutput{
		MessageID: entry.ID,
		AgentID:   in.AgentID,
	}, nil
}

// handleRetrieveInbox — spec §4.6, Appendix B.
func (s *MCPServer) handleRetrieveInbox(args json.RawMessage) (any, error) {
	var in RetrieveInboxInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if in.MaxMessages == 0 {
		in.MaxMessages = 10
	}
	if in.MaxMessages > 100 {
		return nil, fmt.Errorf("max_messages must be <= 100")
	}
	if in.LeaseSeconds == 0 {
		in.LeaseSeconds = 30
	}
	if in.LeaseSeconds > 3600 {
		return nil, fmt.Errorf("lease_seconds must be <= 3600")
	}
	msgs, leaseID, err := s.store.Retrieve(in.AgentID, time.Duration(in.LeaseSeconds)*time.Second, in.MaxMessages)
	if err != nil {
		return nil, err
	}
	if msgs == nil {
		msgs = []*registry.InboxEntry{}
	}
	return RetrieveInboxOutput{Messages: msgs, LeaseID: leaseID}, nil
}

// handleAckMessages — spec §4.7.
func (s *MCPServer) handleAckMessages(args json.RawMessage) (any, error) {
	var in AckMessagesInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	if in.LeaseID == "" {
		return nil, fmt.Errorf("lease_id is required")
	}
	if len(in.MessageIDs) == 0 {
		return nil, fmt.Errorf("message_ids must not be empty")
	}
	if err := s.store.Ack(in.AgentID, in.LeaseID, in.MessageIDs); err != nil {
		return nil, err
	}
	return AckMessagesOutput{Acked: len(in.MessageIDs)}, nil
}

// handleInboxStats — spec §4.8.
func (s *MCPServer) handleInboxStats(args json.RawMessage) (any, error) {
	var in InboxStatsInput
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if in.AgentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	queueDepth, leasedCount, oldestAge, err := s.store.Stats(in.AgentID)
	if err != nil {
		return nil, err
	}
	return InboxStatsOutput{
		QueueDepth:  queueDepth,
		LeasedCount: leasedCount,
		OldestAgeMs: oldestAge.Milliseconds(),
	}, nil
}
