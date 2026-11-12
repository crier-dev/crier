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
	"sync"
	"syscall"
	"time"

	"github.com/crier-dev/crier/internal/buildinfo"
	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/hermesession"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
)

const (
	protocolVersion = "2024-11-05"
	serverName      = "crier-mcp"
)

// serverVersion is the version the MCP handshake advertises as
// serverInfo.version, and the version the startup line logs. It is the version
// segment of the ONE build identity (internal/buildinfo), never a literal: the
// handshake used to answer a hardcoded "0.1.0" while `crier-mcp --version`
// printed the real stamped identity, so the same binary introduced itself two
// different ways (DF-CRIER-171). The MCP catalogue convention for
// serverInfo.version is a bare version with no commit suffix, which is exactly
// the segment buildinfo.String() carries between its leading "v" and the
// commit — so the two surfaces are slices of one format, not two formats.
func serverVersion() string { return buildinfo.VersionSegment() }

// MCPServer reads JSON-RPC from stdin and writes to stdout.
// It is a thin wrapper around a registry.Store, plus the bridge layer that
// lets harnesses use messaging without touching the transport: leases,
// acks, correlation ids and the mesh connection are all owned here.
type MCPServer struct {
	store             registry.Store
	deliver           func(string, *registry.InboxEntry) error
	tools             map[string]toolHandler
	stdin             *bufio.Scanner
	stdout            *json.Encoder
	allowKeylessAgent bool

	agentID string // bridge identity (own inbox / mesh identity)
	httpURL string // remote server URL for mesh_peers
	bridge  *meshBridge
	// dagger is the dagger control surface (CR-CHAT-033). It drives the run
	// records the crier SERVER holds — never a second copy — so a run started
	// from a thread is visible in that thread. Nil means the dagger tools
	// refuse with the CRIER_HTTP_URL gate.
	dagger daggerctl.Control

	// crier is the read side of the nudge (CRIER-MCP-NUDGE). Nil means this
	// process has no session store wired: the summary then reports the
	// agent-inbox count alone rather than inventing session rows.
	crier CrierSource
	// harness is the adapter registry the read-only harness_list /
	// harness_probe tools report on (CRIER-HARNESS §4.4). Nil = no
	// adapters configured in this process.
	harness *harness.Registry
	// operatorScope permits reading ANOTHER agent's unread summary
	// (NUDGE_SCOPE_DENIED otherwise).
	operatorScope bool
	// nudgeDisabled removes the nudge tool/resource/prompt surface entirely
	// (mcp.nudge.enabled=false). Zero value = enabled.
	nudgeDisabled bool

	mu       sync.Mutex
	buffered []*registry.InboxEntry // pulled by ask_agent polling, unseen by the harness
}

type toolHandler func(args json.RawMessage) (any, error)

// Options configures the bridge layer of the MCP server.
type Options struct {
	// DeliveryHandler shares an embedding server's live guard, webhook driver
	// and namespace policies. Standalone local binaries configure it at boot.
	// RemoteStore is already HTTP ingress and must not be dispatched locally.
	DeliveryHandler *registry.Handler
	// AgentID is the bridge's own identity: the agent whose inbox
	// get_messages/ask_agent read, and the mesh identity for mesh_request.
	AgentID string
	// HTTPURL is the Crier server base URL (http://host:port). Enables
	// mesh_peers. Also the target of the RemoteStore when the store itself
	// is remote.
	HTTPURL string
	// MeshURL is the WebSocket mesh endpoint (ws://host:port/mesh/connect/<id>).
	// Enables mesh_request.
	MeshURL string
	// AllowKeylessAgents mirrors !CR_REQUIRE_AGENT_SIG (DF-CRIER-197): when
	// true, register_agent may omit public_key and registers a keyless agent
	// (empty key material), matching the HTTP handler's conditional rule. The
	// ZERO value is false and preserves the fail-closed default — public_key
	// required — for every constructor that does not opt in.
	AllowKeylessAgents bool
	// Dagger is the explicit dagger control surface (CR-CHAT-033). Leave it
	// nil and the bridge builds one from HTTPURL when that is set, which is
	// what cmd/crier-mcp does; a caller that already holds a *daggerctl.Service
	// (an in-process bridge) passes it here instead.
	Dagger daggerctl.Control
	// DaggerToken is the bearer token the auto-built dagger REST client
	// presents to the Crier server. It is ignored when Dagger is set.
	DaggerToken string
	// Crier answers the unread nudge (CRIER-MCP-NUDGE). Nil means this
	// process cannot see session state and the summary reports the
	// agent-inbox count alone. Build it with a *Crier over the registry
	// store, session repository and read-cursor service.
	Crier CrierSource
	// Harness is the adapter registry behind harness_list / harness_probe
	// (CRIER-HARNESS §4.4). Nil = both tools report an empty list.
	Harness *harness.Registry
	// OperatorScope permits reading another agent's unread summary. Default
	// false: a bridge answers only for its own identity (NUDGE_SCOPE_DENIED).
	OperatorScope bool
	// NudgeDisabled is mcp.nudge.enabled=false: the nudge tool, resource and
	// prompt are not registered at all. The zero value leaves the surface
	// enabled (the shipped default).
	NudgeDisabled bool
}

// New creates an MCPServer backed by the given Store.
func New(store registry.Store) *MCPServer {
	return NewWithOptions(store, Options{})
}

// NewWithOptions creates an MCPServer with bridge options.
func NewWithOptions(store registry.Store, opts Options) *MCPServer {
	s := &MCPServer{
		store:             store,
		tools:             make(map[string]toolHandler),
		stdin:             bufio.NewScanner(os.Stdin),
		stdout:            json.NewEncoder(os.Stdout),
		allowKeylessAgent: opts.AllowKeylessAgents,
		agentID:           opts.AgentID,
		httpURL:           strings.TrimSuffix(opts.HTTPURL, "/"),
		crier:             opts.Crier,
		harness:           opts.Harness,
		operatorScope:     opts.OperatorScope,
		nudgeDisabled:     opts.NudgeDisabled,
	}
	if remote, ok := store.(*registry.RemoteStore); ok {
		// RemoteStore is not a local append: it signs and POSTs to the
		// owning server's HTTP choke point. Wrapping it in a local handler
		// would check the guard twice and lose signed service authority.
		s.deliver = remote.DeliverConfirmed
	} else {
		h := opts.DeliveryHandler
		if h == nil {
			// Library default: guarded core with no configured guard policy.
			// Blocking-only delivery needs no background queue lifecycle.
			h = registry.NewHandler(store)
			h.SetWebhookDriver(webhook.NewDriver(webhook.NewClient(30*time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{}))
			h.SetHermesSessionClient(hermesession.NewClient(hermesession.DefaultTimeoutMs * time.Millisecond))
		}
		s.deliver = h.Deliver
	}
	if opts.MeshURL != "" && opts.AgentID != "" {
		s.bridge = newMeshBridge(opts.AgentID, opts.MeshURL)
	}
	// The dagger control surface is reachable whenever the server URL is
	// known: the tools then drive the SAME run records the server holds
	// through its documented REST surface (CR-CHAT-033). With no URL there is
	// nothing to address, so the tools refuse with the CRIER_HTTP_URL gate.
	s.dagger = opts.Dagger
	if s.dagger == nil && s.httpURL != "" {
		s.dagger = daggerctl.NewRESTClient(s.httpURL, opts.DaggerToken)
	}
	s.registerTools()
	return s
}

// ToolCount reports how many tools tools/list advertises, so a caller (the
// startup report) can state the size of the surface without duplicating the
// definition list.
func (s *MCPServer) ToolCount() int { return len(s.toolDefinitions()) }

// ToolNames returns the names advertised by tools/list in registration order.
func (s *MCPServer) ToolNames() []string {
	defs := s.toolDefinitions()
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	return names
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
	s.tools["send_message"] = s.handleSendMessage
	s.tools["get_messages"] = s.handleGetMessages
	s.tools["ask_agent"] = s.handleAskAgent
	s.tools["mesh_peers"] = s.handleMeshPeers
	s.tools["mesh_request"] = s.handleMeshRequest
	// Dagger control (CR-CHAT-033). The executor is the dagger surface crier
	// calls; these tools only create, observe and control a run.
	s.tools["create_run"] = s.handleCreateRun
	s.tools["run_status"] = s.handleRunStatus
	s.tools["cancel_run"] = s.handleCancelRun
	s.tools["resume_run"] = s.handleResumeRun
	s.tools["rewind_run"] = s.handleRewindRun
	s.tools["run_skill"] = s.handleRunSkill
	// CRIER-MCP-NUDGE (§2.1): the nudge informs. The summary carries counts
	// only; the list is the explicit, bounded read the nudge names.
	if !s.nudgeDisabled {
		s.tools["courier_unread_summary"] = s.handleCrierUnreadSummary
		s.tools["courier_unread_list"] = s.handleCrierUnreadList
	}
	// CRIER-HARNESS §4.4, carried into this row: read-only adapter
	// introspection. There is deliberately NO delivery tool.
	s.tools["harness_list"] = s.handleHarnessList
	s.tools["harness_probe"] = s.handleHarnessProbe
}

// Serve runs the stdio JSON-RPC loop. Blocks until shutdown.
func (s *MCPServer) Serve(ctx context.Context) error {
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("crier-mcp starting", "version", serverVersion(), "transport", "stdio")

	if s.bridge != nil {
		go s.bridge.connectWithRetry(5, 2*time.Second)
	}

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
	// CRIER-MCP-NUDGE (§2.1): the resource and prompt halves of the nudge.
	case "resources/list":
		return s.resultResponse(req.ID, s.handleResourcesList())
	case "resources/templates/list":
		return s.resultResponse(req.ID, s.handleResourceTemplatesList())
	case "resources/read":
		result, err := s.handleResourcesRead(req.Params)
		if err != nil {
			return s.errorResponse(req.ID, -32602, err.Error())
		}
		return s.resultResponse(req.ID, result)
	case "prompts/list":
		return s.resultResponse(req.ID, s.handlePromptsList())
	case "prompts/get":
		result, err := s.handlePromptsGet(req.Params)
		if err != nil {
			return s.errorResponse(req.ID, -32602, err.Error())
		}
		return s.resultResponse(req.ID, result)
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
				Version: serverVersion(),
			},
			Capabilities: serverCapabilities{
				Tools:     toolsCapability{},
				Resources: resourcesCapability{},
				Prompts:   promptsCapability{},
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
//
// A tool that cannot work in the mode the server started in must say so in its
// Description: tools/list is the only metadata an MCP client sees before it
// calls anything (DF-CRIER-153). The prerequisites themselves are owned by
// toolPrerequisites in prereqs.go, which the startup report reads too.
func (s *MCPServer) toolDefinitions() []toolDefinition {
	defs := []toolDefinition{
		{
			Name:        "register_agent",
			Description: "Register a new agent with an ed25519 public key and optional capabilities. public_key is required unless the server runs with signature enforcement disabled (CR_REQUIRE_AGENT_SIG=false), in which case it may be omitted to register a keyless agent.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Unique agent identifier"},"public_key":{"type":"string","description":"Hex-encoded ed25519 public key (64 hex characters); omit only when the server allows keyless registration (CR_REQUIRE_AGENT_SIG=false)","pattern":"^[0-9a-fA-F]{64}$"},"capabilities":{"type":"array","items":{"type":"string"},"description":"Capability tags","default":[]}},"required":["id"]}`),
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
			Description: "Remove an agent and clean up its inbox. Scope: with a remote bridge (CRIER_HTTP_URL set) against a server running signature enforcement on (CR_REQUIRE_AGENT_SIG=true, the default) the caller may only act on its own identity (CRIER_AGENT_ID) — the server rejects any other agent with 403.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Agent identifier to remove (own identity only on a remote signed bridge — the server answers 403 for any other agent)"}},"required":["id"]}`),
		},
		{
			Name:        "deliver_message",
			Description: "Deliver a message to an agent's persistent inbox.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string"},"payload":{"type":"object"},"model":{},"provider":{},"reply_to":{"type":"string"},"idempotency_key":{"type":"string"}},"required":["agent_id","payload"]}`),
		},
		{
			Name:        "retrieve_inbox",
			Description: "Retrieve leased messages from an agent's inbox. Scope: with a remote bridge (CRIER_HTTP_URL set) against a server running signature enforcement on (CR_REQUIRE_AGENT_SIG=true, the default) agent_id may only be the bridge's own identity (CRIER_AGENT_ID) — the server rejects any other agent with 403 (agent may only access its own resources).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Agent identifier (own identity only on a remote signed bridge — the server answers 403 for any other agent)"},"max_messages":{"type":"integer","description":"Maximum messages to retrieve (1-100)","default":10,"minimum":1,"maximum":100},"lease_seconds":{"type":"integer","description":"Lease duration in seconds","default":30,"minimum":1,"maximum":3600}},"required":["agent_id"]}`),
		},
		{
			Name:        "ack_messages",
			Description: "Acknowledge previously retrieved messages — permanently removes them from the inbox. Scope: with a remote bridge (CRIER_HTTP_URL set) against a server running signature enforcement on (CR_REQUIRE_AGENT_SIG=true, the default) agent_id may only be the bridge's own identity (CRIER_AGENT_ID) — the server rejects any other agent with 403.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Agent identifier (own identity only on a remote signed bridge — the server answers 403 for any other agent)"},"lease_id":{"type":"string","description":"Lease ID from retrieve_inbox response"},"message_ids":{"type":"array","items":{"type":"string"},"description":"Message IDs to acknowledge","minItems":1}},"required":["agent_id","lease_id","message_ids"]}`),
		},
		{
			Name:        "inbox_stats",
			Description: "Get inbox statistics for an agent — queue depth, leased count, oldest message age. Scope: with a remote bridge (CRIER_HTTP_URL set) against a server running signature enforcement on (CR_REQUIRE_AGENT_SIG=true, the default) agent_id may only be the bridge's own identity (CRIER_AGENT_ID) — the server rejects any other agent with 403.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Agent identifier (own identity only on a remote signed bridge — the server answers 403 for any other agent)"}},"required":["agent_id"]}`),
		},
		{
			Name:        "send_message",
			Description: "Send a message to an agent's durable inbox (bridge-level: no leases, no acks to manage). Optionally reply_to a message's correlation id to answer an ask_agent.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string"},"payload":{"type":"object"},"model":{},"provider":{},"reply_to":{"type":"string","description":"Top-level envelope correlation id"},"idempotency_key":{"type":"string"}},"required":["agent_id","payload"]}`),
		},
		{
			Name:        "get_messages",
			Description: "Get the messages in this agent's own inbox (the bridge owns the lease and acks them for you). Requires a bridge identity: set CRIER_AGENT_ID, otherwise the call fails before it reaches the inbox.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"max":{"type":"integer","description":"Maximum messages to return","default":10,"minimum":1,"maximum":100}},"required":[]}`),
		},
		{
			Name:        "ask_agent",
			Description: "Blocking request/reply to another agent over the durable inbox: sends the payload (merged with a correlation id) and waits for a reply that answers it. Use this to ask an agent a question and get its answer in one call. Requires a bridge identity: set CRIER_AGENT_ID — the reply is read from that agent's own inbox.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Target agent identifier"},"payload":{"type":"object","description":"The question payload"},"timeout_s":{"type":"integer","description":"How long to wait for the reply (default 30, max 300)"}},"required":["agent_id","payload"]}`),
		},
		{
			Name:        "mesh_peers",
			Description: "List the agents currently connected to the live mesh. Requires CRIER_HTTP_URL (the Crier server base URL), otherwise the call fails.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		},
		{
			Name:        "mesh_request",
			Description: "Live REQUEST/RESPONSE round-trip to another agent over the mesh (requires the bridge's own WebSocket connection). Use for liveness/RPC; LLM content should ride the durable inbox (ask_agent). Requires CRIER_MESH_URL and CRIER_AGENT_ID — the bridge opens its own mesh connection only when both are set, so without them the call fails.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","description":"Target agent identifier"},"method":{"type":"string","description":"Application-level method, e.g. GET or PING"},"path":{"type":"string","description":"Application-level path, e.g. /ping"},"body":{"description":"Opaque JSON body"},"timeout_ms":{"type":"integer","description":"Timeout in milliseconds (default 15000)"}},"required":["target","method","path"]}`),
		},
		// Dagger control (CR-CHAT-033). crier CLIENTs the dagger surface; it
		// never executes a DAG. Requires CRIER_HTTP_URL — the run records live
		// on the Crier server, and these tools address them there.
		{
			Name:        "create_run",
			Description: "Create a Dagger run and return its run id and record. The DAG is executed by the dagger surface crier calls, never by crier itself. Requires CRIER_HTTP_URL (the Crier server base URL exposing the /dagger control routes).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"The agent the run is started for — the inbox a terminal outcome is delivered to"},"prompt":{"type":"string","description":"The prompt describing the DAG to run"}},"required":["agent_id","prompt"]}`),
		},
		{
			Name:        "run_status",
			Description: "Observe a Dagger run: returns its record (run id, state, evidence references, requesting agent). Requires CRIER_HTTP_URL (the Crier server base URL exposing the /dagger control routes).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string","description":"Run id returned by create_run or run_skill"}},"required":["run_id"]}`),
		},
		{
			Name:        "cancel_run",
			Description: "Cancel a running Dagger run and return its updated record. Requires CRIER_HTTP_URL (the Crier server base URL exposing the /dagger control routes).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string","description":"Run id to cancel"}},"required":["run_id"]}`),
		},
		{
			Name:        "resume_run",
			Description: "Resume a paused Dagger run from its last checkpoint and return its updated record. Requires CRIER_HTTP_URL (the Crier server base URL exposing the /dagger control routes).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string","description":"Run id to resume"}},"required":["run_id"]}`),
		},
		{
			Name:        "rewind_run",
			Description: "Rewind a Dagger run to a node, discarding checkpoints from that node onward, and return its updated record. Requires CRIER_HTTP_URL (the Crier server base URL exposing the /dagger control routes).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"run_id":{"type":"string","description":"Run id to rewind"},"node_id":{"type":"string","description":"Node to rewind to — passed to the executor verbatim"}},"required":["run_id","node_id"]}`),
		},
		{
			Name:        "run_skill",
			Description: "Run a skill registered with the dagger executor and return the new run's id and record. Requires CRIER_HTTP_URL (the Crier server base URL exposing the /dagger control routes).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"The agent the run is started for — the inbox a terminal outcome is delivered to"},"skill":{"type":"string","description":"Name of a skill registered with the executor"},"args":{"type":"object","description":"Optional skill arguments (opaque JSON)"}},"required":["agent_id","skill"]}`),
		},
		// CRIER-MCP-NUDGE (§2.1): counts only. The summary's whole point is
		// that it never returns a message body, so the schema has no way to
		// ask for one.
		{
			Name:        "courier_unread_summary",
			Description: "How much Crier mail is waiting for this agent: total unread, counts by tag category, the oldest unread timestamp and per-session counts. COUNTS ONLY — it never returns message text. Answers for the bridge's own identity (require CRIER_AGENT_ID) unless agent_id is supplied — another agent's counts need an operator scope, otherwise NUDGE_SCOPE_DENIED. An unregistered agent answers total:0, not an error. Read-only: it never marks anything read. Session counts need the session backend this process can read (CR_SESSION_BACKEND / CR_SESSION_LOG_ROOT).",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Agent to report on (default: the bridge's own identity, CRIER_AGENT_ID)"},"session_id":{"type":"string","description":"Narrow the count to one Crier session"}},"required":[]}`),
		},
		{
			Name:        "courier_unread_list",
			Description: "Read the unread Crier messages (message_id, session_id, author, tags, text, seq) — the \"read them\" the nudge names. Bounded: default 20, maximum 100 (an over-max limit is clamped). Defaults to the bridge's own identity (requires CRIER_AGENT_ID) unless agent_id is supplied. Read-only: it does not mark read, lease or ack anything.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string","description":"Agent whose unread messages to read (default: the bridge's own identity, CRIER_AGENT_ID)"},"session_id":{"type":"string","description":"Narrow the list to one Crier session"},"limit":{"type":"integer","description":"Maximum messages to return (default 20, max 100; over-max is clamped)","default":20,"minimum":1,"maximum":100}},"required":[]}`),
		},
		// CRIER-HARNESS §4.4: read-only adapter introspection, no delivery.
		{
			Name:        "harness_list",
			Description: "List the harness adapters this process knows, with the capability each Probe measured (available, version, binary, features, reason). Read-only: there is no delivery tool — a message is delivered by ingress, not by an MCP client.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		},
		{
			Name:        "harness_probe",
			Description: "Probe ONE harness adapter and report its capability (available, version, binary, features, reason for unavailability). An unregistered name answers HARNESS_UNKNOWN naming the known adapters. Read-only.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"Adapter name, as reported by harness_list"}},"required":["name"]}`),
		},
	}

	if s.nudgeDisabled {
		kept := defs[:0]
		for _, def := range defs {
			if def.Name == "courier_unread_summary" || def.Name == "courier_unread_list" {
				continue
			}
			kept = append(kept, def)
		}
		defs = kept
	}
	return defs
}

func (s *MCPServer) errorResponse(id any, code int, message string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		Error:   &rpcError{Code: code, Message: message},
		ID:      id,
	}
}

// resultResponse wraps a successful result for a method that answers with a
// plain JSON body (the resource and prompt surface).
func (s *MCPServer) resultResponse(id any, result any) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", Result: result, ID: id}
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
		// Preserve registrationConflict's actionable keyless detail. The
		// duplicate sentinel is still used for classification, but its wrapped
		// message is the user-facing contract for register_agent.
		return -32602, msg
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
