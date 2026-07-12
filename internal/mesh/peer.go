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
	stopCh      chan struct{}
	config      MeshConfig
}

type MeshConfig struct {
	AgentID            string
	KeepaliveInterval  time.Duration
	LeaseTTL           time.Duration
	LeaseExpiryFactor  int
	MaxPendingRequests int
	RequestTimeout     time.Duration
}

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

func NewMesh(config MeshConfig) *Mesh {
	return &Mesh{
		agentID:     config.AgentID,
		connections: make(map[string]*PeerConnection),
		pending:     make(map[string]chan *Response),
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
	case TypeResponse:
		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			return
		}
		m.pendingMu.RLock()
		ch, ok := m.pending[resp.RequestID]
		m.pendingMu.RUnlock()
		if ok {
			select {
			case ch <- &resp:
			default:
			}
		}
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
		}
	}
}
