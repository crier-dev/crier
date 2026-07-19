package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/totalwindupflightsystems/crier/internal/registry"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "crier-mcp"
	serverVersion   = "0.1.0"
)

// MCPServer reads JSON-RPC from stdin and writes to stdout.
// It is a thin wrapper around a registry.Store.
type MCPServer struct {
	store  registry.Store
	tools  map[string]toolHandler
	stdin  *bufio.Scanner
	stdout *json.Encoder
}

type toolHandler func(args json.RawMessage) (any, error)

// New creates an MCPServer backed by the given Store.
func New(store registry.Store) *MCPServer {
	s := &MCPServer{
		store:  store,
		tools:  make(map[string]toolHandler),
		stdin:  bufio.NewScanner(os.Stdin),
		stdout: json.NewEncoder(os.Stdout),
	}
	s.registerTools()
	return s
}

// registerTools populates the tool registry (spec §4).
func (s *MCPServer) registerTools() {
	s.tools["register_agent"] = s.handleRegisterAgent
	s.tools["list_agents"] = s.handleListAgents
	s.tools["get_agent"] = s.handleGetAgent
	s.tools["unregister_agent"] = s.handleUnregisterAgent
	s.tools["deliver_message"] = s.handleDeliverMessage
	s.tools["retrieve_inbox"] = s.handleRetrieveInbox
	s.tools["ack_messages"] = s.handleAckMessages
	s.tools["inbox_stats"] = s.handleInboxStats
}

// Serve runs the stdio JSON-RPC loop. Blocks until shutdown.
func (s *MCPServer) Serve(ctx context.Context) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("crier-mcp starting", "version", serverVersion, "transport", "stdio")

	for s.stdin.Scan() {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return nil
		default:
		}

		line := s.stdin.Bytes()
		if len(line) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(nil, -32700, "parse error")
			continue
		}

		resp := s.dispatch(ctx, &req)
		if resp == nil {
			continue // notification, no response
		}
		if err := s.stdout.Encode(resp); err != nil {
			slog.Error("write error", "error", err)
			return err
		}
		}

	if err := s.stdin.Err(); err != nil {
		return fmt.Errorf("stdin: %w", err)
	}
	return nil
}

// dispatch routes a JSON-RPC request to the appropriate handler.
func (s *MCPServer) dispatch(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "notifications/initialized":
		return nil // notification, no response
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		return s.errorResponse(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *MCPServer) handleInitialize(req *jsonRPCRequest) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		Result: initializeResult{
			ProtocolVersion: protocolVersion,
			ServerInfo: serverInfo{
				Name:    serverName,
				Version: serverVersion,
			},
			Capabilities: serverCapabilities{
				Tools: toolsCapability{},
			},
		},
		ID: req.ID,
	}
}

func (s *MCPServer) handleToolsList(req *jsonRPCRequest) *jsonRPCResponse {
	defs := s.toolDefinitions()
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		Result:  map[string]any{"tools": defs},
		ID:      req.ID,
	}
}

func (s *MCPServer) handleToolsCall(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	var params toolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.errorResponse(req.ID, -32602, fmt.Sprintf("invalid arguments: %v", err))
	}

	handler, ok := s.tools[params.Name]
	if !ok {
		return s.errorResponse(req.ID, -32602, fmt.Sprintf("unknown tool: %s", params.Name))
	}

	result, err := handler(params.Arguments)
	if err != nil {
		_, msg := mcpError(err)
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			Result: toolsCallResult{
				Content: []contentItem{{Type: "text", Text: msg}},
				IsError: true,
			},
			ID: req.ID,
		}
	}

	jsonBytes, err := json.Marshal(result)
	if err != nil {
		return s.errorResponse(req.ID, -32603, fmt.Sprintf("result serialization error: %v", err))
	}

	return &jsonRPCResponse{
		JSONRPC: "2.0",
		Result: toolsCallResult{
			Content: []contentItem{{Type: "text", Text: string(jsonBytes)}},
		},
		ID: req.ID,
	}
}

// toolDefinitions returns the MCP tool list (spec §4).
func (s *MCPServer) toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			Name:        "register_agent",
			Description: "Register a new agent with an ed25519 public key and optional capabilities.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Unique agent identifier"},"public_key":{"type":"string","description":"Hex-encoded ed25519 public key (64 hex characters)","pattern":"^[0-9a-fA-F]{64}$"},"capabilities":{"type":"array","items":{"type":"string"},"description":"Capability tags","default":[]}},"required":["id","public_key"]}`),
		},
		{
			Name:        "list_agents",
			Description: "List all registered agents with their status and capabilities.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		},
		{
			Name:        "get_agent",
			Description: "Get detailed information about a specific agent.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Agent identifier"}},"required":["id"]}`),
		},
		{
			Name:        "unregister_agent",
			Description: "Remove an agent and clean up its inbox.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Agent identifier to remove"}},"required":["id"]}`),
		},
		{
			Name:        "deliver_message",
			Description: "Deliver a message to an agent's persistent inbox.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Target agent identifier"},"payload":{"type":"object","description":"Message payload (arbitrary JSON)"}},"required":["agent_id","payload"]}`),
		},
		{
			Name:        "retrieve_inbox",
			Description: "Retrieve leased messages from an agent's inbox.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Agent identifier"},"max_messages":{"type":"integer","description":"Maximum messages to retrieve (1-100)","default":10,"minimum":1,"maximum":100},"lease_seconds":{"type":"integer","description":"Lease duration in seconds","default":30,"minimum":1,"maximum":3600}},"required":["agent_id"]}`),
		},
		{
			Name:        "ack_messages",
			Description: "Acknowledge previously retrieved messages — permanently removes them from the inbox.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Agent identifier"},"lease_id":{"type":"string","description":"Lease ID from retrieve_inbox response"},"message_ids":{"type":"array","items":{"type":"string"},"description":"Message IDs to acknowledge","minItems":1}},"required":["agent_id","lease_id","message_ids"]}`),
		},
		{
			Name:        "inbox_stats",
			Description: "Get inbox statistics for an agent — queue depth, leased count, oldest message age.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Agent identifier"}},"required":["agent_id"]}`),
		},
	}
}

func (s *MCPServer) errorResponse(id any, code int, message string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		Error:   &rpcError{Code: code, Message: message},
		ID:      id,
	}
}

func (s *MCPServer) writeError(id any, code int, message string) {
	resp := s.errorResponse(id, code, message)
	if err := s.stdout.Encode(resp); err != nil {
		slog.Error("write error", "error", err)
	}
}

// mcpError maps Store errors to MCP error codes (spec §5.3).
// Sentinel errors get specific messages; all other errors (including validation)
// get their message passed through with code -32602.
func mcpError(err error) (code int, message string) {
	msg := err.Error()
	switch {
	case errors.Is(err, registry.ErrAgentNotFound):
		return -32602, "agent not found"
	case errors.Is(err, registry.ErrAgentExists):
		return -32602, "agent already registered"
	case errors.Is(err, registry.ErrLeaseConflict):
		return -32602, "message is not leased under the supplied lease"
	case errors.Is(err, registry.ErrInvalidStoreInput):
		return -32602, msg
	default:
		// Check if the error message looks like a validation error (not a store crash)
		// Store-level errors (DB unavailable, etc.) get -32603
		// Validation errors (from tool handlers) get -32602 with the message
		if strings.Contains(msg, "storage") || strings.Contains(msg, "unavailable") {
			slog.Warn("store error", "error", err)
			return -32603, "registry storage unavailable"
		}
		return -32602, msg
	}
}

// Ensure io is used (for interface compliance).
var _ io.Reader = os.Stdin
