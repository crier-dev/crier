package mesh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDefaultMeshConfig(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	if cfg.AgentID != "agent-A" {
		t.Errorf("AgentID = %q, want agent-A", cfg.AgentID)
	}
	if cfg.KeepaliveInterval != 30*time.Second {
		t.Errorf("KeepaliveInterval = %v, want 30s", cfg.KeepaliveInterval)
	}
	if cfg.LeaseTTL != 1*time.Hour {
		t.Errorf("LeaseTTL = %v, want 1h", cfg.LeaseTTL)
	}
	if cfg.LeaseExpiryFactor != 3 {
		t.Errorf("LeaseExpiryFactor = %d, want 3", cfg.LeaseExpiryFactor)
	}
	if cfg.MaxPendingRequests != 50 {
		t.Errorf("MaxPendingRequests = %d, want 50", cfg.MaxPendingRequests)
	}
	if cfg.RequestTimeout != 30*time.Second {
		t.Errorf("RequestTimeout = %v, want 30s", cfg.RequestTimeout)
	}
}

func TestNewMeshInitialState(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	m := NewMesh(cfg)
	if m == nil {
		t.Fatal("NewMesh returned nil")
	}
	if m.ActivePeers() != 0 {
		t.Errorf("ActivePeers = %d, want 0", m.ActivePeers())
	}
	ids := m.PeerIDs()
	if len(ids) != 0 {
		t.Errorf("PeerIDs = %v, want empty", ids)
	}
}

func TestMeshAcceptPeer(t *testing.T) {
	upgrader := websocket.Upgrader{}
	connected := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connected <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	defer clientConn.Close()

	var serverConn *websocket.Conn
	select {
	case serverConn = <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server connection")
	}

	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)
	pc := NewAcceptedPeerConnection("agent-B", serverConn)
	pc.StartReadLoop()
	m.AcceptPeer("agent-B", pc)

	if m.ActivePeers() != 1 {
		t.Errorf("ActivePeers = %d, want 1", m.ActivePeers())
	}
	ids := m.PeerIDs()
	if len(ids) != 1 || ids[0] != "agent-B" {
		t.Errorf("PeerIDs = %v, want [agent-B]", ids)
	}
}

func TestMeshAcceptPeerAndStop(t *testing.T) {
	upgrader := websocket.Upgrader{}
	connected := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connected <- conn
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}
	defer clientConn.Close()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server connection")
	}

	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)
	pc := NewAcceptedPeerConnection("agent-B", clientConn)
	pc.StartReadLoop()
	m.AcceptPeer("agent-B", pc)

	if m.ActivePeers() != 1 {
		t.Fatal("expected 1 active peer")
	}

	m.Stop()

	if m.ActivePeers() != 0 {
		t.Errorf("ActivePeers after Stop = %d, want 0", m.ActivePeers())
	}
}

func TestMeshConnectPeerSuccess(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = conn.WriteMessage(mt, msg)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	if err := m.ConnectPeer(context.Background(), "agent-B", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer m.Stop()

	if m.ActivePeers() != 1 {
		t.Errorf("ActivePeers = %d, want 1", m.ActivePeers())
	}
	ids := m.PeerIDs()
	if len(ids) != 1 || ids[0] != "agent-B" {
		t.Errorf("PeerIDs = %v, want [agent-B]", ids)
	}
}

func TestMeshConnectPeerAlreadyConnected(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	if err := m.ConnectPeer(context.Background(), "agent-B", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer m.Stop()

	if err := m.ConnectPeer(context.Background(), "agent-B", wsURL); err == nil {
		t.Fatal("expected error for duplicate peer, got nil")
	} else if !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("expected already connected error, got %v", err)
	}
}

func TestMeshConnectPeerDialFailure(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	dialerCfg := DefaultDialerConfig()
	dialerCfg.HandshakeTimeout = 10 * time.Millisecond
	pc := NewPeerConnection("agent-B", "ws://127.0.0.1:1/ws", dialerCfg)
	if err := m.ConnectPeer(context.Background(), "agent-B", pc.URL); err == nil {
		t.Fatal("expected dial failure, got nil")
	}
	if m.ActivePeers() != 0 {
		t.Errorf("ActivePeers = %d, want 0", m.ActivePeers())
	}
}

func TestMeshSendRequestTimeout(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cfg := DefaultMeshConfig("agent-A")
	cfg.RequestTimeout = 50 * time.Millisecond
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	if err := m.ConnectPeer(context.Background(), "agent-B", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer m.Stop()

	_, err := m.SendRequest(context.Background(), "agent-B", "GET", "/v1/state", nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestMeshSendRequestPeerNotConnected(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	cfg.RequestTimeout = 50 * time.Millisecond
	m := NewMesh(cfg)

	_, err := m.SendRequest(context.Background(), "agent-B", "GET", "/v1/state", nil)
	if err == nil {
		t.Fatal("expected error for missing peer, got nil")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("expected not connected error, got %v", err)
	}
}

func TestMeshSendRequestReceivesResponse(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req Request
			if err := json.Unmarshal(msg, &req); err != nil {
				continue
			}
			resp := &Response{
				Envelope: Envelope{
					Type:      TypeResponse,
					Version:   1,
					MessageID: newMessageID(),
					Timestamp: time.Now(),
				},
				RequestID:  req.MessageID,
				Source:     PeerRef{AgentID: "agent-B"},
				StatusCode: 200,
				Body:       json.RawMessage(`{"ok":true}`),
				TraceID:    req.TraceID,
			}
			data, _ := Marshal(resp)
			_ = conn.WriteMessage(websocket.TextMessage, data)
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cfg := DefaultMeshConfig("agent-A")
	cfg.RequestTimeout = 1 * time.Second
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	if err := m.ConnectPeer(context.Background(), "agent-B", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer m.Stop()

	resp, err := m.SendRequest(context.Background(), "agent-B", "GET", "/v1/state", nil)
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("Body = %s, want {\"ok\":true}", resp.Body)
	}
}

func TestMeshSendRequestTooManyPending(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cfg := DefaultMeshConfig("agent-A")
	cfg.MaxPendingRequests = 1
	cfg.RequestTimeout = 5 * time.Second
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	if err := m.ConnectPeer(context.Background(), "agent-B", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer m.Stop()

	go m.SendRequest(context.Background(), "agent-B", "GET", "/one", nil)
	time.Sleep(50 * time.Millisecond)

	_, err := m.SendRequest(context.Background(), "agent-B", "GET", "/two", nil)
	if err == nil {
		t.Fatal("expected too many pending error, got nil")
	}
	if !strings.Contains(err.Error(), "too many pending") {
		t.Fatalf("expected too many pending error, got %v", err)
	}
}

func TestMeshHandleMessageResponse(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	m := NewMesh(cfg)

	respCh := make(chan *Response, 1)
	reqID := newMessageID()
	m.pendingMu.Lock()
	m.pending[reqID] = respCh
	m.pendingMu.Unlock()

	resp := &Response{
		Envelope: Envelope{
			Type:      TypeResponse,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		RequestID:  reqID,
		Source:     PeerRef{AgentID: "agent-B"},
		StatusCode: 200,
		Body:       json.RawMessage(`{"ok":true}`),
	}
	data, _ := Marshal(resp)
	m.handleMessage("agent-B", data)

	select {
	case got := <-respCh:
		if got.StatusCode != 200 {
			t.Errorf("StatusCode = %d, want 200", got.StatusCode)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for response")
	}
}

func TestMeshHandleMessageError(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	m := NewMesh(cfg)

	respCh := make(chan *Response, 1)
	reqID := newMessageID()
	m.pendingMu.Lock()
	m.pending[reqID] = respCh
	m.pendingMu.Unlock()

	errMsg := &ErrorMessage{
		Envelope: Envelope{
			Type:      TypeError,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		RequestID: reqID,
		Error: ErrorDetail{
			Code:    ErrCodeInternal,
			Message: "boom",
		},
		TraceID: newMessageID(),
	}
	data, _ := Marshal(errMsg)
	m.handleMessage("agent-B", data)

	select {
	case got := <-respCh:
		if got.StatusCode != 500 {
			t.Errorf("StatusCode = %d, want 500", got.StatusCode)
		}
		if !strings.Contains(string(got.Body), "boom") {
			t.Errorf("Body = %s, want boom", got.Body)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for error response")
	}
}

func TestMeshHandleMessageUnknownType(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	m := NewMesh(cfg)

	respCh := make(chan *Response, 1)
	reqID := newMessageID()
	m.pendingMu.Lock()
	m.pending[reqID] = respCh
	m.pendingMu.Unlock()

	keepalive := &Keepalive{
		Envelope: Envelope{
			Type:      TypeKeepalive,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		AgentID: "agent-B",
	}
	data, _ := Marshal(keepalive)
	m.handleMessage("agent-B", data)

	select {
	case <-respCh:
		t.Fatal("unexpected response for keepalive message")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMeshHandleMessageInvalidJSON(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	m := NewMesh(cfg)

	m.handleMessage("agent-B", []byte(`not json`))
	// No panic and no state change is sufficient.
	if m.ActivePeers() != 0 {
		t.Errorf("ActivePeers = %d, want 0", m.ActivePeers())
	}
}

func TestMeshHandleMessageResponseWithoutPending(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	m := NewMesh(cfg)

	resp := &Response{
		Envelope: Envelope{
			Type:      TypeResponse,
			Version:   1,
			MessageID: newMessageID(),
			Timestamp: time.Now(),
		},
		RequestID:  newMessageID(),
		Source:     PeerRef{AgentID: "agent-B"},
		StatusCode: 200,
	}
	data, _ := Marshal(resp)
	m.handleMessage("agent-B", data)

	if m.ActivePeers() != 0 {
		t.Errorf("ActivePeers = %d, want 0", m.ActivePeers())
	}
}

func TestMeshRegister(t *testing.T) {
	upgrader := websocket.Upgrader{}
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case received <- msg:
			default:
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	if err := m.ConnectPeer(context.Background(), "agent-B", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer m.Stop()

	select {
	case msg := <-received:
		var env Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if env.Type != TypeRegister {
			t.Errorf("Type = %q, want REGISTER", env.Type)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for register message")
	}
}
