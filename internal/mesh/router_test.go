package mesh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// startAgentEndpoint runs a bare WS upgrade endpoint and returns helpers to
// dial a client connection and to fetch the server-side upgraded connection.
func startAgentEndpoint(t *testing.T) (client func() *websocket.Conn, server func() *websocket.Conn) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	var mu sync.Mutex
	var serverConns []*websocket.Conn
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		serverConns = append(serverConns, conn)
		mu.Unlock()
	}))
	t.Cleanup(s.Close)

	wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
	client = func() *websocket.Conn {
		t.Helper()
		c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial agent endpoint: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	server = func() *websocket.Conn {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			if len(serverConns) > 0 {
				conn := serverConns[0]
				serverConns = serverConns[1:]
				mu.Unlock()
				return conn
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting for server-side connection")
		return nil
	}
	return client, server
}

// acceptAgent wires a freshly dialed agent into the mesh as a peer.
func acceptAgent(t *testing.T, m *Mesh, agentID string, clientConn *websocket.Conn, serverConn *websocket.Conn) {
	t.Helper()
	pc := NewAcceptedPeerConnection(agentID, serverConn)
	m.AcceptPeer(agentID, pc)
	pc.StartReadLoop()
	t.Cleanup(func() { _ = pc.Close() })
}

// readUntilType reads WS messages until one has the given envelope type or
// the deadline expires.
func readUntilType(t *testing.T, conn *websocket.Conn, want MessageType, timeout time.Duration) ([]byte, Envelope) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var env Envelope
		if err := json.Unmarshal(msg, &env); err != nil {
			continue
		}
		if env.Type == want {
			return msg, env
		}
	}
	t.Fatalf("timed out waiting for %s message", want)
	return nil, Envelope{}
}

func TestAgentToAgentRequestRouting(t *testing.T) {
	clientA, serverA := startAgentEndpoint(t)
	clientB, serverB := startAgentEndpoint(t)
	connA, connB := clientA(), clientB()

	m := NewMesh(DefaultMeshConfig("relay"))
	acceptAgent(t, m, "agent-a", connA, serverA())
	acceptAgent(t, m, "agent-b", connB, serverB())

	// agent-a sends a REQUEST addressed to agent-b
	req := &Request{
		Envelope: Envelope{
			Type:      TypeRequest,
			Version:   1,
			MessageID: "req-123",
			Timestamp: time.Now(),
		},
		Source:    PeerRef{AgentID: "agent-a"},
		Target:    PeerRef{AgentID: "agent-b"},
		Method:    "GET",
		Path:      "/intel",
		Body:      map[string]any{"query": "coordinates"},
		TraceID:   "trace-1",
		TimeoutMs: 5000,
	}
	data, err := Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := connA.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}

	// agent-b receives the forwarded REQUEST
	msg, env := readUntilType(t, connB, TypeRequest, 5*time.Second)
	if env.MessageID != "req-123" {
		t.Fatalf("message_id = %q, want req-123", env.MessageID)
	}
	var got Request
	if err := json.Unmarshal(msg, &got); err != nil {
		t.Fatal(err)
	}
	if got.Source.AgentID != "agent-a" || got.Target.AgentID != "agent-b" {
		t.Fatalf("routing wrong: source=%s target=%s", got.Source.AgentID, got.Target.AgentID)
	}
	if got.Path != "/intel" {
		t.Fatalf("path = %q, want /intel", got.Path)
	}

	// agent-b replies with a RESPONSE
	resp := &Response{
		Envelope: Envelope{
			Type:      TypeResponse,
			Version:   1,
			MessageID: "resp-1",
			Timestamp: time.Now(),
		},
		RequestID:  "req-123",
		Source:     PeerRef{AgentID: "agent-b"},
		StatusCode: 200,
		TraceID:    "trace-1",
	}
	resp.Body, _ = json.Marshal(map[string]any{"intel": "41.9N 87.6W"})
	respData, err := Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if err := connB.WriteMessage(websocket.TextMessage, respData); err != nil {
		t.Fatal(err)
	}

	// agent-a receives the routed-back RESPONSE
	_, respEnv := readUntilType(t, connA, TypeResponse, 5*time.Second)
	if respEnv.MessageID != "resp-1" {
		t.Fatalf("response message_id = %q, want resp-1", respEnv.MessageID)
	}
}

func TestAgentRequestToUnknownPeerGetsError(t *testing.T) {
	clientA, serverA := startAgentEndpoint(t)
	connA := clientA()

	m := NewMesh(DefaultMeshConfig("relay"))
	acceptAgent(t, m, "agent-a", connA, serverA())

	req := &Request{
		Envelope: Envelope{
			Type:      TypeRequest,
			Version:   1,
			MessageID: "req-404",
			Timestamp: time.Now(),
		},
		Source:  PeerRef{AgentID: "agent-a"},
		Target:  PeerRef{AgentID: "ghost"},
		Method:  "GET",
		Path:    "/nowhere",
		TraceID: "trace-404",
	}
	data, err := Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := connA.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}

	// agent-a receives an ERROR referencing the original request
	msg, env := readUntilType(t, connA, TypeError, 5*time.Second)
	if env.MessageID == "" {
		t.Fatal("error envelope missing message_id")
	}
	var errMsg ErrorMessage
	if err := json.Unmarshal(msg, &errMsg); err != nil {
		t.Fatal(err)
	}
	if errMsg.RequestID != "req-404" {
		t.Fatalf("error request_id = %q, want req-404", errMsg.RequestID)
	}
	if errMsg.Error.Code != ErrCodeControllerOffline {
		t.Fatalf("error code = %q, want %q", errMsg.Error.Code, ErrCodeControllerOffline)
	}
}

// testWSResponderServer runs a WS server that answers each REQUEST it
// receives with a RESPONSE carrying the same message_id as request_id.
func testWSResponderServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			var env Envelope
			if json.Unmarshal(msg, &env) != nil || env.Type != TypeRequest {
				continue
			}
			var req Request
			if json.Unmarshal(msg, &req) != nil {
				continue
			}
			resp := &Response{
				Envelope: Envelope{
					Type:      TypeResponse,
					Version:   1,
					MessageID: "resp-" + req.MessageID,
					Timestamp: time.Now(),
				},
				RequestID:  req.MessageID,
				Source:     PeerRef{AgentID: "peer-echo"},
				StatusCode: 200,
				TraceID:    req.TraceID,
			}
			resp.Body, _ = json.Marshal(map[string]any{"echo": true})
			data, err := Marshal(resp)
			if err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		}
	}))
	return s, "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestServerInitiatedRequestStillWorks(t *testing.T) {
	// Regression: the router must not break Mesh.SendRequest (server as client).
	server, wsURL := testWSResponderServer(t)
	defer server.Close()

	m := NewMesh(DefaultMeshConfig("relay"))
	if err := m.ConnectPeer(context.Background(), "peer-echo", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	defer m.Stop()

	body := map[string]any{"n": 1}
	resp, err := m.SendRequest(context.Background(), "peer-echo", "GET", "/echo", body)
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response")
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
