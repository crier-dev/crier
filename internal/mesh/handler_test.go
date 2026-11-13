package mesh

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

func TestHandleConnectSuccess(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	router := mux.NewRouter()
	router.HandleFunc("/mesh/connect/{agentID}", HandleConnect(m)).Methods("GET")
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/mesh/connect/agent-B"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if err := waitFor(func() bool { return m.ActivePeers() == 1 }, 2*time.Second); err != nil {
		t.Fatalf("expected 1 active peer: %v", err)
	}
	ids := m.PeerIDs()
	if len(ids) != 1 || ids[0] != "agent-B" {
		t.Errorf("PeerIDs = %v, want [agent-B]", ids)
	}
}

// TestHandleConnectImmediateMalformedFrames proves the accepted connection's
// reader is wired before a client can write its first frame. A WebSocket client
// may send immediately after Dial returns; every one of these writes must be
// observed and refused, rather than disappearing in a start-reader-before-
// callback window (QA-CRIER-57).
func TestHandleConnectImmediateMalformedFrames(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = time.Hour
	m := NewMesh(cfg)
	defer m.Stop()

	router := mux.NewRouter()
	router.HandleFunc("/mesh/connect/{agentID}", HandleConnect(m)).Methods("GET")
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/mesh/connect/qa-crier-57"
	const iterations = 200
	for i := 0; i < iterations; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("iteration %d/%d: Dial: %v", i+1, iterations, err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte("this is not a frame")); err != nil {
			_ = conn.Close()
			t.Fatalf("iteration %d/%d: write malformed frame: %v", i+1, iterations, err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			_ = conn.Close()
			t.Fatalf("iteration %d/%d: set read deadline: %v", i+1, iterations, err)
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			_ = conn.Close()
			t.Fatalf("iteration %d/%d: immediate malformed frame was not answered: %v", i+1, iterations, err)
		}
		var reply ErrorMessage
		if err := json.Unmarshal(raw, &reply); err != nil {
			conn.Close()
			t.Fatalf("iteration %d/%d: reply is not an ERROR envelope: %v (raw %s)", i+1, iterations, err, raw)
		}
		if reply.Error.Code != ErrCodeInvalidMessage {
			conn.Close()
			t.Fatalf("iteration %d/%d: error code = %q, want %q (raw %s)", i+1, iterations, reply.Error.Code, ErrCodeInvalidMessage, raw)
		}
		conn.Close()
	}
}

func TestHandleConnectMissingAgentID(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	m := NewMesh(cfg)

	router := mux.NewRouter()
	router.HandleFunc("/mesh/connect/{agentID:.*}", HandleConnect(m)).Methods("GET")
	server := httptest.NewServer(router)
	defer server.Close()

	resp, err := http.Get(server.URL + "/mesh/connect/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", resp.StatusCode)
	}
	// DF-CRIER-212: the rejection body is JSON, so the response must declare
	// it. net/http's Error helper hard-codes "text/plain; charset=utf-8" (and
	// overwrites any Content-Type set before it), which is what this endpoint
	// used to answer.
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json — the rejection body is JSON", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	var payload map[string]string
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("response body %q is not valid JSON: %v", body, err)
	}
	if payload["error"] != "agentID is required" {
		t.Errorf("error = %q, want agentID is required", payload["error"])
	}
	// Byte-identical wire body, captured from the pre-fix build (b140a6f).
	if got, want := string(body), "{\"error\":\"agentID is required\"}\n"; got != want {
		t.Errorf("body = %q, want %q (pre-fix wire bytes)", got, want)
	}
}

func TestHandlePeersEmpty(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	m := NewMesh(cfg)

	server := httptest.NewServer(HandlePeers(m))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"count":0`) {
		t.Errorf("body = %q, want count 0", body)
	}
}

func TestHandlePeersWithAcceptedPeer(t *testing.T) {
	cfg := DefaultMeshConfig("agent-A")
	cfg.KeepaliveInterval = 10 * time.Second
	m := NewMesh(cfg)

	upgrader := websocket.Upgrader{}
	connected := make(chan *websocket.Conn, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connected <- conn
	}))
	defer wsServer.Close()

	wsURL := "ws" + strings.TrimPrefix(wsServer.URL, "http")
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

	pc := NewAcceptedPeerConnection("agent-B", serverConn)
	pc.StartReadLoop()
	m.AcceptPeer("agent-B", pc)

	if err := waitFor(func() bool { return m.ActivePeers() == 1 }, 2*time.Second); err != nil {
		t.Fatalf("expected 1 active peer: %v", err)
	}

	server := httptest.NewServer(HandlePeers(m))
	defer server.Close()

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"agent_id":"agent-B"`) {
		t.Errorf("body = %q, want agent-B", body)
	}
	if !strings.Contains(string(body), `"count":1`) {
		t.Errorf("body = %q, want count 1", body)
	}
}

func waitFor(fn func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}
