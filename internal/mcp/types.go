package mcp

import (
	"encoding/json"

	"github.com/totalwindupflightsystems/crier/internal/registry"
)

// JSON-RPC message types.

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      any             `json:"id,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
	ID      any       `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP initialization types.

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	ServerInfo      serverInfo         `json:"serverInfo"`
	Capabilities    serverCapabilities `json:"capabilities"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type serverCapabilities struct {
	Tools toolsCapability `json:"tools"`
}

type toolsCapability struct{}

// MCP tool call types.

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type toolsCallResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Tool definition for tools/list.

type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Tool input types matching the spec §6.2.

type RegisterAgentInput struct {
	ID           string   `json:"id"`
	PublicKey    string   `json:"public_key"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type GetAgentInput struct {
	ID string `json:"id"`
}

type UnregisterAgentInput struct {
	ID string `json:"id"`
}

type DeliverMessageInput struct {
	AgentID string          `json:"agent_id"`
	Payload json.RawMessage `json:"payload"`
}

type RetrieveInboxInput struct {
	AgentID      string `json:"agent_id"`
	MaxMessages  int    `json:"max_messages,omitempty"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
}

type AckMessagesInput struct {
	AgentID    string   `json:"agent_id"`
	LeaseID    string   `json:"lease_id"`
	MessageIDs []string `json:"message_ids"`
}

type InboxStatsInput struct {
	AgentID string `json:"agent_id"`
}

// Tool output types matching the spec §6.3.

type DeliverMessageOutput struct {
	MessageID string `json:"message_id"`
	AgentID   string `json:"agent_id"`
}

type RetrieveInboxOutput struct {
	Messages []*registry.InboxEntry `json:"messages"`
	LeaseID  string                 `json:"lease_id"`
}

type AckMessagesOutput struct {
	Acked int `json:"acked"`
}

type InboxStatsOutput struct {
	QueueDepth  int   `json:"queue_depth"`
	LeasedCount int   `json:"leased_count"`
	OldestAgeMs int64 `json:"oldest_age_ms"`
}

type UnregisterAgentOutput struct {
	Unregistered string `json:"unregistered"`
}

type ListAgentsOutput struct {
	Agents []*registry.Agent `json:"agents"`
}
