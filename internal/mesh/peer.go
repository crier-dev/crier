package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/buildinfo"
)

type Mesh struct {
	agentID     string
	connections map[string]*PeerConnection
	mu          sync.RWMutex
	pending     map[string]chan *Response
	pendingMu   sync.RWMutex
	// routes tracks in-flight agent-to-agent requests so responses can be
	// routed back to the original requester peer, AND so a reply can be tied
	// to the peer the request was addressed to. Keyed by request message ID.
	routes   map[string]meshRoute
	routesMu sync.RWMutex
	stopCh   chan struct{}
	config   MeshConfig
	// keys resolves an agent's registered ed25519 public key for the connect
	// handshake (DF-CRIER-287). Nil until SetAgentKeyProvider is called; with
	// MeshAuthConfig.Required set and no provider, every connect is refused
	// (fail closed) rather than admitted unverified.
	keys AgentKeyProvider
}

// meshRoute is one in-flight agent-to-agent request: who asked, and who was
// asked. Both are needed — the requester is the destination of the reply, and
// the target is the only peer allowed to send it (DF-CRIER-287).
type meshRoute struct {
	requesterID string
	targetID    string
}

type MeshConfig struct {
	AgentID            string
	KeepaliveInterval  time.Duration
	LeaseTTL           time.Duration
	LeaseExpiryFactor  int
	MaxPendingRequests int
	RequestTimeout     time.Duration
	// Auth is the mesh authentication configuration (DF-CRIER-287). The zero
	// value — Required false, no signing key — is the default and leaves the
	// accept path and the frame rules exactly as they were before the flag
	// existed.
	Auth MeshAuthConfig
}

// SetAgentKeyProvider wires the registry lookup the connect handshake verifies
// signatures against (DF-CRIER-287). The server calls it at boot, after the
// registry store exists; without it a server that requires mesh authentication
// refuses every connect — never admits an unverified peer.
func (m *Mesh) SetAgentKeyProvider(keys AgentKeyProvider) {
	m.mu.Lock()
	m.keys = keys
	m.mu.Unlock()
}

// keyProvider returns the registered key source, or nil when none is wired.
func (m *Mesh) keyProvider() AgentKeyProvider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.keys
}

// authConfig returns the authentication configuration this mesh runs with.
// Like every other posture, it is fixed at construction: the accept path reads
// it per request rather than caching a decision made when the first peer
// connected.
func (m *Mesh) authConfig() MeshAuthConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.Auth
}

// AuthRequired reports whether this mesh requires the connect handshake. It is
// the single question the frame-level identity rules ask, so a caller cannot
// enforce the socket rules while the accept path skipped the proof.
func (m *Mesh) AuthRequired() bool {
	return m.authConfig().Required
}

// DefaultMeshConfig returns a MeshConfig with sensible defaults:
// 30s keepalive, 1h lease TTL, 50 max pending requests, 30s request timeout,
// and mesh authentication OFF (the shipped default; see
// docs/mesh-protocol.md §Authentication for how to turn it on).
func DefaultMeshConfig(agentID string) MeshConfig {
	return MeshConfig{
		AgentID:            agentID,
		KeepaliveInterval:  30 * time.Second,
		LeaseTTL:           1 * time.Hour,
		LeaseExpiryFactor:  3,
		MaxPendingRequests: 50,
		RequestTimeout:     30 * time.Second,
	}
}

// NewMesh creates a Mesh instance with the given configuration.
// Use DefaultMeshConfig to obtain a configuration with sensible defaults.
func NewMesh(config MeshConfig) *Mesh {
	return &Mesh{
		agentID:     config.AgentID,
		connections: make(map[string]*PeerConnection),
		pending:     make(map[string]chan *Response),
		routes:      make(map[string]meshRoute),
		stopCh:      make(chan struct{}),
		config:      config,
	}
}

func (m *Mesh) ConnectPeer(ctx context.Context, peerID, wsURL string) error {
	m.mu.Lock()
	if _, exists := m.connections[peerID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("already connected to %s", peerID)
	}
	conn := NewPeerConnection(peerID, wsURL, DefaultDialerConfig())
	m.connections[peerID] = conn
	m.mu.Unlock()

	// The inbound handler is installed BEFORE the dial (DF-CRIER-287). A server
	// that requires mesh authentication sends its AUTH_CHALLENGE as soon as the
	// socket upgrades, and the read pump starts inside Connect — so a handler
	// registered afterwards raced that first frame, and a frame read by the
	// pump with no handler yet is DROPPED. The challenge would vanish and the
	// handshake would sit until its deadline for no reason a client could see.
	authResult := make(chan error, 1)
	conn.OnMessage(func(data []byte) {
		m.handlePeerFrame(conn, peerID, data, authResult)
	})

	if err := conn.Connect(ctx); err != nil {
		m.mu.Lock()
		delete(m.connections, peerID)
		m.mu.Unlock()
		return fmt.Errorf("connect to %s: %w", peerID, err)
	}

	// A client that expects to authenticate waits for the outcome. It waits
	// only when its own configuration says the server requires auth: a server
	// that does not sends no challenge at all, so waiting unconditionally
	// would turn every connect to a default-configured server into a timeout.
	if auth := m.authConfig(); auth.Required {
		timeout := auth.effectiveTimeout()
		select {
		case err := <-authResult:
			if err != nil {
				conn.Close()
				m.mu.Lock()
				delete(m.connections, peerID)
				m.mu.Unlock()
				return fmt.Errorf("mesh auth with %s: %w", peerID, err)
			}
		case <-time.After(timeout):
			conn.Close()
			m.mu.Lock()
			delete(m.connections, peerID)
			m.mu.Unlock()
			return fmt.Errorf("mesh auth with %s: no AUTH_OK within %s (the server is expected to require mesh authentication; check that its registry holds the public key for this agent id)", peerID, timeout)
		}
	}

	conn.OnClose(func(err error) {
		m.mu.Lock()
		delete(m.connections, peerID)
		m.mu.Unlock()
		slog.Debug("mesh: peer disconnected", "agent_id", peerID, "error", err)
	})
	if err := m.register(ctx, conn); err != nil {
		conn.Close()
		m.mu.Lock()
		delete(m.connections, peerID)
		m.mu.Unlock()
		return fmt.Errorf("register with %s: %w", peerID, err)
	}
	go m.keepaliveLoop(peerID, conn)
	return nil
}

// handlePeerFrame is inbound dispatch for a connection THIS mesh dialed: it
// resolves the connect handshake (DF-CRIER-287) before the socket's ordinary
// frames reach the router, so an AUTH_CHALLENGE is never mistaken for a
// protocol error and a handshake refusal is never swallowed as an unplaceable
// ERROR.
//
// authResult carries the handshake outcome to ConnectPeer. Every send is
// non-blocking: after the handshake has settled nobody reads that channel, and
// a blocked read loop is a dead peer.
func (m *Mesh) handlePeerFrame(conn *PeerConnection, peerID string, data []byte, authResult chan error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		// Let the router refuse it: one place reports a malformed frame.
		m.handleMessage(peerID, data)
		return
	}
	switch env.Type {
	case TypeAuthChallenge:
		if err := m.answerAuthChallenge(conn, peerID, data, m.authConfig().SigningKey); err != nil {
			slog.Warn("mesh auth: challenge unanswered", "peer", peerID, "error", err)
			select {
			case authResult <- err:
			default:
			}
		}
		return
	case TypeAuthOK:
		slog.Debug("mesh auth: authenticated", "peer", peerID, "agent_id", m.agentID)
		select {
		case authResult <- nil:
		default:
		}
		return
	case TypeError:
		// A handshake refusal (AUTH_FAILED) has to surface as a connect error:
		// nothing this client sent is pending yet, so an ERROR at this point
		// belongs to the handshake, and forwarding it would leave ConnectPeer
		// waiting out its own timeout for a refusal the server already sent.
		var errMsg ErrorMessage
		if json.Unmarshal(data, &errMsg) == nil && errMsg.Error.Code == ErrCodeAuthFailed {
			select {
			case authResult <- fmt.Errorf("server refused the handshake: %s", errMsg.Error.Message):
			default:
			}
			return
		}
	}
	m.handleMessage(peerID, data)
}

func (m *Mesh) SendRequest(ctx context.Context, targetID, method, path string, body any) (*Response, error) {
	m.mu.RLock()
	conn, ok := m.connections[targetID]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("peer %s not connected", targetID)
	}
	msgID := newMessageID()
	req := &Request{
		Envelope: Envelope{
			Type:      TypeRequest,
			Version:   1,
			MessageID: msgID,
			Timestamp: time.Now(),
		},
		Source:    PeerRef{AgentID: m.agentID},
		Target:    PeerRef{AgentID: targetID},
		Method:    method,
		Path:      path,
		Body:      body,
		TraceID:   newMessageID(),
		TimeoutMs: int(m.config.RequestTimeout.Milliseconds()),
	}
	data, err := Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	respCh := make(chan *Response, 1)
	m.pendingMu.Lock()
	if len(m.pending) >= m.config.MaxPendingRequests {
		m.pendingMu.Unlock()
		return nil, fmt.Errorf("too many pending requests (max %d)", m.config.MaxPendingRequests)
	}
	m.pending[msgID] = respCh
	m.pendingMu.Unlock()
	defer func() {
		m.pendingMu.Lock()
		delete(m.pending, msgID)
		m.pendingMu.Unlock()
	}()
	if err := conn.Send(data); err != nil {
		return nil, fmt.Errorf("send to %s: %w", targetID, err)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, m.config.RequestTimeout)
	defer cancel()
	select {
	case resp := <-respCh:
		return resp, nil
	case <-timeoutCtx.Done():
		slog.Debug("mesh: REQUEST timed out", "target", targetID, "message_id", msgID,
			"timeout", m.config.RequestTimeout)
		return nil, fmt.Errorf("request to %s timed out after %s", targetID, m.config.RequestTimeout)
	}
}

func (m *Mesh) Stop() {
	close(m.stopCh)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, conn := range m.connections {
		conn.Close()
	}
	m.connections = make(map[string]*PeerConnection)
}

func (m *Mesh) ActivePeers() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.connections)
}

// AcceptPeer registers an already-connected peer and starts the keepalive loop.
// Used by the server when accepting incoming WebSocket connections.
func (m *Mesh) AcceptPeer(agentID string, conn *PeerConnection) {
	m.mu.Lock()
	m.connections[agentID] = conn
	m.mu.Unlock()

	conn.OnMessage(func(data []byte) {
		m.handleMessage(agentID, data)
	})
	conn.OnClose(func(err error) {
		m.mu.Lock()
		delete(m.connections, agentID)
		m.mu.Unlock()
		slog.Debug("mesh: peer disconnected", "agent_id", agentID, "error", err)
	})

	go m.keepaliveLoop(agentID, conn)
}

// PeerIDs returns the list of connected peer agent IDs.
func (m *Mesh) PeerIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.connections))
	for id := range m.connections {
		ids = append(ids, id)
	}
	return ids
}

func (m *Mesh) register(ctx context.Context, conn *PeerConnection) error {
	reg := &Register{
		Envelope: Envelope{
			Type:      TypeRegister,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		AgentID:    m.agentID,
		LeaseID:    "",
		LeaseTTLMs: 3600000,
		Capabilities: Capabilities{
			// The build identity, not a literal (DF-CRIER-171): the mesh
			// REGISTER is another place where one checkout introduces
			// itself, and the MCP bridge sends the same segment.
			Version:               buildinfo.VersionSegment(),
			MaxConcurrentSessions: 10,
		},
	}
	data, err := Marshal(reg)
	if err != nil {
		return fmt.Errorf("marshal register: %w", err)
	}
	if err := conn.Send(data); err != nil {
		return err
	}
	slog.Debug("mesh: REGISTER sent", "agent_id", m.agentID, "message_id", reg.MessageID)
	return nil
}

func (m *Mesh) keepaliveLoop(peerID string, conn *PeerConnection) {
	ticker := time.NewTicker(m.config.KeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			msg := &Keepalive{
				Envelope: Envelope{
					Type:      TypeKeepalive,
					Version:   1,
					MessageID: newMessageID(),
					Timestamp: time.Now(),
				},
				LeaseID: "",
				AgentID: m.agentID,
			}
			data, err := Marshal(msg)
			if err != nil {
				continue
			}
			if err := conn.Send(data); err != nil {
				return
			}
		}
	}
}

func (m *Mesh) handleMessage(peerID string, data []byte) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		// A frame that is not even an envelope used to be dropped with a
		// debug log (DF-CRIER-141), which made a wrong-shaped client
		// indistinguishable from a server that simply never answered. It is
		// refused on the wire instead (DF-CRIER-40). Nothing could be read
		// from it, so the refusal carries no message_id (see
		// reportInvalidMessage).
		m.reportInvalidMessage(peerID, "",
			fmt.Sprintf("malformed frame: not a JSON envelope (%v)", err))
		return
	}
	switch env.Type {
	case TypeRequest:
		m.handleAgentRequest(peerID, env, data)
	case TypeRegister, TypeRegisterAck:
		// The handshake payload is logged at info (agent id + peer) so the
		// REGISTER lifecycle is traceable. Inbound REGISTER/REGISTER_ACK
		// still have no state handler in the mesh router — the handshake is
		// owned by the connecting side (Mesh.register) — so this logs what
		// arrived rather than claiming it was processed. A payload that does
		// not match the REGISTER shape is malformed, and malformed frames are
		// refused rather than dropped (DF-CRIER-40).
		var reg Register
		if err := json.Unmarshal(data, &reg); err != nil {
			m.reportInvalidMessage(peerID, env.MessageID,
				fmt.Sprintf("malformed %s frame: %v", env.Type, err))
			return
		}
		slog.Info("mesh: REGISTER received", "type", env.Type, "agent_id", reg.AgentID,
			"peer", peerID, "message_id", reg.MessageID)
	case TypeKeepalive:
		// Recognized and deliberately ignored: there is no liveness
		// bookkeeping and no reply of any kind — not even the
		// INVALID_MESSAGE a malformed frame now draws, because a KEEPALIVE is
		// perfectly well-formed. The loop keeps the socket warm and detects
		// dead connections via read errors; it has no other effect.
		slog.Debug("mesh: KEEPALIVE ignored", "peer", peerID, "message_id", env.MessageID)
	case TypeResponse:
		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			m.reportInvalidMessage(peerID, env.MessageID,
				fmt.Sprintf("malformed RESPONSE frame: %v", err))
			return
		}
		// Server-initiated request? Deliver to the waiting caller.
		m.pendingMu.RLock()
		ch, ok := m.pending[resp.RequestID]
		m.pendingMu.RUnlock()
		if ok {
			select {
			case ch <- &resp:
			default:
			}
			return
		}
		// Agent-initiated request? Forward the response back to the requester —
		// if this peer is the one the request was addressed to (DF-CRIER-287).
		m.forwardResponse(peerID, resp.RequestID, data)
	case TypeError:
		var errMsg ErrorMessage
		if err := json.Unmarshal(data, &errMsg); err != nil {
			m.reportInvalidMessage(peerID, env.MessageID,
				fmt.Sprintf("malformed ERROR frame: %v", err))
			return
		}
		m.pendingMu.RLock()
		ch, ok := m.pending[errMsg.RequestID]
		m.pendingMu.RUnlock()
		if ok {
			resp := &Response{
				RequestID:  errMsg.RequestID,
				StatusCode: 500,
				Source:     PeerRef{AgentID: peerID},
			}
			body, _ := json.Marshal(errMsg.Error)
			resp.Body = body
			select {
			case ch <- resp:
			default:
			}
			return
		}
		// Agent-initiated request that failed at the target side.
		m.forwardResponse(peerID, errMsg.RequestID, data)
	case TypeAuthChallenge, TypeAuthResponse, TypeAuthOK:
		// The connect handshake frames (DF-CRIER-287) are defined only INSIDE
		// the handshake: this connection is past it (it is in the peer table,
		// so it was either admitted or the server never required auth), which
		// makes an AUTH_* frame here a client error rather than a handshake
		// step. Refused with the malformed-frame code, naming why — the
		// alternative (silence) is the failure mode DF-CRIER-40 removed.
		m.reportInvalidMessage(peerID, env.MessageID,
			fmt.Sprintf("malformed frame: %s is only valid during the mesh connect handshake (CR_REQUIRE_MESH_AUTH on the server), and this connection is already admitted", env.Type))
	default:
		// An envelope naming a type the protocol does not define (the nine in
		// the envelope table of docs/mesh-protocol.md) is malformed, not
		// merely unhandled. It used to be dropped with a debug log, so a
		// client that misspelled a type saw nothing but silence (DF-CRIER-40).
		m.reportInvalidMessage(peerID, env.MessageID,
			fmt.Sprintf("malformed frame: unknown message type %q", env.Type))
	}
}

// handleAgentRequest forwards an agent-to-agent REQUEST to its target peer.
// A route is recorded so the eventual RESPONSE can be returned to the
// requester. If the target is not connected, an ERROR is sent back; a REQUEST
// that carries no resolvable target at all is rejected as malformed, and so is
// one whose payload does not match the REQUEST shape (DF-CRIER-40).
//
// env is the envelope already decoded from the same bytes by handleMessage: a
// payload that fails to decode into Request still has a readable message_id
// there, which is what lets the refusal carry the id the requester sent.
func (m *Mesh) handleAgentRequest(requesterID string, env Envelope, data []byte) {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		// The envelope named REQUEST but the body does not match that shape
		// (a non-object source/target, a string timeout_ms, …). Dropping it
		// left the requester waiting out its own timeout with no evidence of
		// why; it is refused instead.
		m.reportInvalidMessage(requesterID, env.MessageID,
			fmt.Sprintf("malformed REQUEST frame: %v", err))
		return
	}
	targetID := req.Target.AgentID
	if targetID == "" {
		// A wrong-shaped envelope (a client's own "from"/"to" spellings
		// instead of the wire's source/target PeerRef objects) leaves no
		// target to resolve at all. Falling through to the lookup below
		// would answer CONTROLLER_OFFLINE for a frame that never named a
		// reachable peer, and would blame an identifier that is blank —
		// "peer  not connected" misdirects the reader to the wrong
		// subsystem. Answer INVALID_MESSAGE ("Malformed frame" in the
		// protocol's ERROR-code table) and name the field that is missing.
		slog.Debug("mesh: REQUEST dropped (missing target peer id)",
			"requester", requesterID, "message_id", req.MessageID)
		m.sendErrorTo(requesterID, req.MessageID, req.TraceID,
			ErrCodeInvalidMessage, "malformed REQUEST: missing target peer id (expected target.agent_id)")
		return
	}

	// Identity is bound to the SOCKET, not to the frame (DF-CRIER-287). With
	// mesh authentication on, this connection proved it holds requesterID's
	// private key at connect, so a REQUEST that names a different
	// source.agent_id is an attempt to speak as another agent: the target
	// would see the victim's id on a request the victim never sent. Refuse it
	// (FORBIDDEN, "Not authorized") rather than forward it — without this
	// check the connect handshake would authenticate the socket and then let
	// it claim any identity per frame, which is the same hole one layer up.
	//
	// Auth-off deployments keep the old behaviour: with no verified identity
	// there is nothing to bind the field to, and refusing it would break
	// existing relays that rewrite `source` for their own purposes.
	if m.AuthRequired() && req.Source.AgentID != requesterID {
		slog.Warn("mesh: REQUEST refused (source is not the authenticated peer)",
			"requester", requesterID, "claimed_source", req.Source.AgentID,
			"target", targetID, "message_id", req.MessageID)
		m.sendErrorTo(requesterID, req.MessageID, req.TraceID, ErrCodeForbidden,
			fmt.Sprintf("REQUEST refused: source.agent_id %q is not the authenticated peer %q (on a mesh that requires authentication an agent may only request as itself)",
				req.Source.AgentID, requesterID))
		return
	}

	m.mu.RLock()
	targetConn, ok := m.connections[targetID]
	m.mu.RUnlock()
	if !ok {
		slog.Debug("mesh: REQUEST dropped (target peer not connected)",
			"requester", requesterID, "target", targetID, "message_id", req.MessageID)
		m.sendErrorTo(requesterID, req.MessageID, req.TraceID,
			ErrCodeControllerOffline, fmt.Sprintf("peer %s not connected", targetID))
		return
	}

	// Record the route before forwarding so the response finds its way back.
	// The table is bounded by the same configured cap that bounds the pending
	// requests (DF-CRIER-187): flushing only at a higher literal left the
	// refusal branch below permanently reachable with the default cap.
	m.routesMu.Lock()
	pruneRoutesLocked(m.routes, time.Now(), m.config.MaxPendingRequests)
	if len(m.routes) >= m.config.MaxPendingRequests {
		// Defensive guard, no longer reachable through the normal path: with a
		// positive cap the flush above always leaves the table below it, so a
		// new route is admitted instead of refused. It still fires for a
		// non-positive cap, where nothing can be bounded (fail closed).
		m.routesMu.Unlock()
		slog.Debug("mesh: REQUEST dropped (route table full)",
			"requester", requesterID, "target", targetID, "message_id", req.MessageID,
			"limit", m.config.MaxPendingRequests)
		m.sendErrorTo(requesterID, req.MessageID, req.TraceID,
			ErrCodeInternal, "mesh route table full")
		return
	}
	m.routes[req.MessageID] = meshRoute{requesterID: requesterID, targetID: targetID}
	m.routesMu.Unlock()

	if err := targetConn.Send(data); err != nil {
		m.routesMu.Lock()
		delete(m.routes, req.MessageID)
		m.routesMu.Unlock()
		slog.Debug("mesh: REQUEST dropped (send failed)",
			"requester", requesterID, "target", targetID, "message_id", req.MessageID, "error", err)
		m.sendErrorTo(requesterID, req.MessageID, req.TraceID,
			ErrCodeControllerOffline, fmt.Sprintf("deliver to %s: %v", targetID, err))
	}
}

// forwardResponse routes a RESPONSE or ERROR back to the agent that issued
// the original request, if that agent is still connected — and only if the
// sender is the peer the request was addressed to (DF-CRIER-287).
//
// The sender check is the reply half of socket-bound identity: with mesh
// authentication on, `senderID` is the peer that proved its key at connect, so
// a frame answering a request addressed to someone else is not a reply at all
// — it is a third peer injecting a response into another pair's exchange. Such
// a frame is refused (FORBIDDEN, to the sender) and NOT forwarded, so the
// requester never sees it. With authentication off there is no verified sender
// to compare against, and the frame is forwarded as before.
func (m *Mesh) forwardResponse(senderID, requestID string, data []byte) {
	m.routesMu.Lock()
	route, ok := m.routes[requestID]
	if ok {
		delete(m.routes, requestID)
	}
	m.routesMu.Unlock()
	if !ok {
		slog.Debug("mesh: RESPONSE dropped (no route for request)", "request_id", requestID)
		return
	}

	if m.AuthRequired() && route.targetID != senderID {
		slog.Warn("mesh: reply refused (sender is not the request's target)",
			"sender", senderID, "target", route.targetID, "request_id", requestID)
		m.sendErrorTo(senderID, requestID, "", ErrCodeForbidden,
			fmt.Sprintf("reply refused: request %s was addressed to %q, and this connection is %q",
				requestID, route.targetID, senderID))
		return
	}

	m.mu.RLock()
	conn, ok := m.connections[route.requesterID]
	m.mu.RUnlock()
	if ok {
		_ = conn.Send(data)
	}
}

// reportInvalidMessage answers a malformed inbound frame with the protocol's
// INVALID_MESSAGE error frame ("Malformed frame", docs/mesh-protocol.md §ERROR).
//
// It exists because the alternative — what the mesh did before DF-CRIER-40 —
// was to drop such a frame with at most a debug log, leaving a wrong-shaped
// client indistinguishable from a server that simply never answers.
//
// requestID is the malformed frame's own message_id, or "" when the frame was
// too broken to have one (it did not decode as an envelope at all). It becomes
// the refusal's request_id, which is `omitempty`: the field is ABSENT on the
// wire for an unreadable frame, never blank, so a client can tell "I sent
// something unparseable" from "the reply correlates to a request of mine".
//
// Best effort, like every other mesh ERROR: a peer that has already
// disconnected is skipped (sendErrorTo finds no connection).
func (m *Mesh) reportInvalidMessage(peerID, requestID, reason string) {
	slog.Warn("mesh: malformed frame refused",
		"peer", peerID, "message_id", requestID, "reason", reason)
	m.sendErrorTo(peerID, requestID, "", ErrCodeInvalidMessage, reason)
}

// sendErrorTo sends an ERROR message to a peer (best effort).
func (m *Mesh) sendErrorTo(peerID, requestID, traceID, code, message string) {
	m.mu.RLock()
	conn, ok := m.connections[peerID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	errMsg := &ErrorMessage{
		Envelope: Envelope{
			Type:      TypeError,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		RequestID: requestID,
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
		TraceID: traceID,
	}
	data, err := Marshal(errMsg)
	if err != nil {
		return
	}
	_ = conn.Send(data)
}

// pruneRoutesLocked keeps the route table bounded by the configured cap. Routes
// are plain requester lookups without timestamps; once the table reaches cap
// entries it is flushed wholesale (stale routes are harmless to keep — they are
// only ever consulted on a matching response, and responses to flushed routes
// are dropped). Caller holds routesMu.
//
// The cap is the caller's MeshConfig.MaxPendingRequests, passed in because the
// route table shares that bound: flushing only at a hard-coded 4096 while new
// routes are refused at MaxPendingRequests made the flush unreachable and let
// unanswered routes wedge the table forever (DF-CRIER-187). Timestamps are
// still not tracked, so this remains a wholesale flush rather than an age-based
// prune.
//
// A non-positive cap means the table cannot be bounded at all, so nothing is
// flushed and the caller's `len(routes) >= cap` refusal branch stays the
// effective guard, refusing every agent-to-agent REQUEST. That is fail-closed
// and identical to the behaviour before the cap was plumbed through — a
// misconfigured cap must not silently disable the limit.
func pruneRoutesLocked(routes map[string]meshRoute, now time.Time, cap int) {
	if cap <= 0 || len(routes) < cap {
		return
	}
	for k := range routes {
		delete(routes, k)
	}
}
