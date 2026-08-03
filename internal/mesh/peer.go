package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type Mesh struct {
	agentID     string
	connections map[string]*PeerConnection
	mu          sync.RWMutex
	pending     map[string]chan *Response
	pendingMu   sync.RWMutex
	// routes tracks in-flight agent-to-agent requests so responses can be
	// routed back to the original requester peer. Keyed by request message ID.
	routes   map[string]string
	routesMu sync.RWMutex
	stopCh   chan struct{}
	config   MeshConfig
}

type MeshConfig struct {
	AgentID            string
	KeepaliveInterval  time.Duration
	LeaseTTL           time.Duration
	LeaseExpiryFactor  int
	MaxPendingRequests int
	RequestTimeout     time.Duration
}

// DefaultMeshConfig returns a MeshConfig with sensible defaults:
// 30s keepalive, 1h lease TTL, 50 max pending requests, 30s request timeout.
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
		routes:      make(map[string]string),
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
	if err := conn.Connect(ctx); err != nil {
		m.mu.Lock()
		delete(m.connections, peerID)
		m.mu.Unlock()
		return fmt.Errorf("connect to %s: %w", peerID, err)
	}
	conn.OnMessage(func(data []byte) {
		m.handleMessage(peerID, data)
	})
	conn.OnClose(func(err error) {
		m.mu.Lock()
		delete(m.connections, peerID)
		m.mu.Unlock()
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
			Version:               "0.1.0",
			MaxConcurrentSessions: 10,
		},
	}
	data, err := Marshal(reg)
	if err != nil {
		return fmt.Errorf("marshal register: %w", err)
	}
	return conn.Send(data)
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
		return
	}
	switch env.Type {
	case TypeRequest:
		m.handleAgentRequest(peerID, data)
	case TypeResponse:
		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
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
		// Agent-initiated request? Forward the response back to the requester.
		m.forwardResponse(resp.RequestID, data)
	case TypeError:
		var errMsg ErrorMessage
		if err := json.Unmarshal(data, &errMsg); err != nil {
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
		m.forwardResponse(errMsg.RequestID, data)
	}
}

// handleAgentRequest forwards an agent-to-agent REQUEST to its target peer.
// A route is recorded so the eventual RESPONSE can be returned to the
// requester. If the target is not connected, an ERROR is sent back.
func (m *Mesh) handleAgentRequest(requesterID string, data []byte) {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}
	targetID := req.Target.AgentID

	m.mu.RLock()
	targetConn, ok := m.connections[targetID]
	m.mu.RUnlock()
	if !ok {
		m.sendErrorTo(requesterID, req.MessageID, req.TraceID,
			ErrCodeControllerOffline, fmt.Sprintf("peer %s not connected", targetID))
		return
	}

	// Record the route before forwarding so the response finds its way back.
	m.routesMu.Lock()
	pruneRoutesLocked(m.routes, time.Now())
	if len(m.routes) >= m.config.MaxPendingRequests {
		m.routesMu.Unlock()
		m.sendErrorTo(requesterID, req.MessageID, req.TraceID,
			ErrCodeInternal, "mesh route table full")
		return
	}
	m.routes[req.MessageID] = requesterID
	m.routesMu.Unlock()

	if err := targetConn.Send(data); err != nil {
		m.routesMu.Lock()
		delete(m.routes, req.MessageID)
		m.routesMu.Unlock()
		m.sendErrorTo(requesterID, req.MessageID, req.TraceID,
			ErrCodeControllerOffline, fmt.Sprintf("deliver to %s: %v", targetID, err))
	}
}

// forwardResponse routes a RESPONSE or ERROR back to the agent that issued
// the original request, if that agent is still connected.
func (m *Mesh) forwardResponse(requestID string, data []byte) {
	m.routesMu.Lock()
	requesterID, ok := m.routes[requestID]
	if ok {
		delete(m.routes, requestID)
	}
	m.routesMu.Unlock()
	if !ok {
		return
	}

	m.mu.RLock()
	conn, ok := m.connections[requesterID]
	m.mu.RUnlock()
	if ok {
		_ = conn.Send(data)
	}
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

// pruneRoutesLocked keeps the route table bounded. Routes are plain requester
// lookups without timestamps; when the table grows past a hard cap it is
// flushed wholesale (stale routes are harmless to keep — they are only ever
// consulted on a matching response, and responses to flushed routes are
// dropped). Caller holds routesMu.
func pruneRoutesLocked(routes map[string]string, now time.Time) {
	if len(routes) < 4096 {
		return
	}
	for k := range routes {
		delete(routes, k)
	}
}
