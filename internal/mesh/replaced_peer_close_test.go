package mesh

// CR-GAP-070 / CR-GAP-071 — a replaced peer must survive its predecessor's
// late close.
//
// Both connection tables are written without closing what they overwrite:
// AcceptPeer (server side) overwrites m.connections[agentID] when the same
// agent reconnects, and ConnectPeer's error/recovery paths can leave a newer
// connection registered for a peer id whose old socket is still closing. The
// old socket's OnClose then fired unconditionally and deleted the map entry —
// which by then belongs to the LIVE replacement — so the live peer's
// REQUEST/RESPONSE traffic drew "peer %s not connected" until it reconnected
// again. Both OnClose callbacks must therefore delete only when the stored
// entry still points at THIS socket, the same identity check the inbox_notify
// release already used (CR-FEAT-023).
//
// These tests pin that identity guard on both sides:
//
//   - accept side (CR-GAP-070): a real reconnect over the wire, then the OLD
//     socket is closed; the replacement must stay registered, keep its
//     inbox_notify grant, and still answer a routed REQUEST end to end;
//   - dial side (CR-GAP-071): ConnectPeer's own registered OnClose against a
//     replacement in the map — the late close of the dialed socket must not
//     unregister the replacement.

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

// awaitStaleCloseProcessed blocks until the server side of oldSocket has torn
// the connection down, which happens AFTER the read pump invoked the OnClose
// callback under test (readLoop calls fn(err) before its deferred
// conn.Close()) — so a test that asserts on the peer table only after this
// returns is never racing the close handler. Fails the test if the teardown
// does not arrive within 3s.
func awaitStaleCloseProcessed(t *testing.T, oldSocket *websocket.Conn) {
	t.Helper()
	_ = oldSocket.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, err := oldSocket.ReadMessage(); err != nil {
			return
		}
	}
}

// respondToRequests reads frames on a client-side socket and answers every
// REQUEST with a 200 RESPONSE, the way a live agent would. It exits when the
// socket closes (test cleanup).
func respondToRequests(conn *websocket.Conn, agentID string) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var env Envelope
		if json.Unmarshal(data, &env) != nil || env.Type != TypeRequest {
			continue
		}
		var req Request
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		resp, err := Marshal(&Response{
			Envelope:   Envelope{Type: TypeResponse, Version: 1, MessageID: newMessageID(), Timestamp: time.Now()},
			RequestID:  req.MessageID,
			Source:     PeerRef{AgentID: agentID},
			StatusCode: http.StatusOK,
		})
		if err != nil {
			return
		}
		if conn.WriteMessage(websocket.TextMessage, resp) != nil {
			return
		}
	}
}

// TestMeshStaleCloseDoesNotUnregisterTheReplacedPeer (CR-GAP-070): alice
// reconnects while her first socket is still live; the server accepts the new
// socket over the old map entry WITHOUT closing the old one. When the old
// socket's close finally fires, its OnClose must not unregister the LIVE
// replacement: the entry survives, the replacement's inbox_notify grant
// survives, and a routed REQUEST still reaches alice and is answered.
func TestMeshStaleCloseDoesNotUnregisterTheReplacedPeer(t *testing.T) {
	m, srv := inboxNotifyMesh(t)

	oldSocket := dialMesh(t, srv, "alice", "")
	newSocket := dialMesh(t, srv, "alice", "?inbox_notify=1")
	// The dial returns before the accept path has run, so wait for accept #2's
	// own side effect: the per-connection opt-in it grants. From here the map
	// entry for alice is the NEW connection.
	awaitCondition(t, "the replacement connection's inbox_notify opt-in", func() bool {
		return m.InboxNotifyEnabled("alice")
	})
	go respondToRequests(newSocket, "alice")

	// The stale close: alice's FIRST socket dies late, after the replacement
	// is already the registered one.
	_ = oldSocket.Close()
	awaitStaleCloseProcessed(t, oldSocket)

	// Read the map under the mesh's own lock (a bare read would race the
	// close handler's write under -race).
	m.mu.RLock()
	entry := m.connections["alice"]
	m.mu.RUnlock()
	if entry == nil {
		t.Fatal("the stale socket's OnClose unregistered the LIVE replacement: m.connections[alice] is gone")
	}
	if !m.InboxNotifyEnabled("alice") {
		t.Error("the stale close also revoked the replacement connection's inbox_notify grant")
	}
	if got := m.ActivePeers(); got != 1 {
		t.Errorf("ActivePeers = %d, want 1 (the replacement)", got)
	}

	// REQUEST routing still finds the replacement, end to end: the request is
	// written to the surviving connection, answered by the agent on the other
	// end, and matched back to this caller.
	resp, err := m.SendRequest(context.Background(), "alice", "GET", "/after-reconnect", nil)
	if err != nil {
		t.Fatalf("REQUEST to the surviving peer failed after the stale socket closed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("response status = %d, want 200", resp.StatusCode)
	}
	if resp.Source.AgentID != "alice" {
		t.Errorf("response source = %q, want alice", resp.Source.AgentID)
	}
}

// TestMeshDialSideStaleCloseDoesNotUnregisterTheReplacement (CR-GAP-071): the
// twin guard on the ConnectPeer side. The dialed socket's registered OnClose
// fires late — after a newer connection for the same peer id is the one in the
// map — and must not delete the replacement's entry.
func TestMeshDialSideStaleCloseDoesNotUnregisterTheReplacement(t *testing.T) {
	// A raw WebSocket endpoint with no mesh semantics: the handler hands the
	// server-side socket to the test and holds it open, so the test decides
	// when the dialed socket dies.
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 1)
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		defer close(handlerDone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	client := NewMesh(MeshConfig{
		AgentID:           "wanderer",
		KeepaliveInterval: time.Hour,
		RequestTimeout:    time.Second,
	})
	defer client.Stop()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/mesh"
	if err := client.ConnectPeer(context.Background(), "crier-under-test", wsURL); err != nil {
		t.Fatalf("ConnectPeer: %v", err)
	}
	var dialedSocket *websocket.Conn
	select {
	case dialedSocket = <-serverConns:
	case <-time.After(3 * time.Second):
		t.Fatal("the test WebSocket server never saw the dialed connection")
	}

	// A newer connection for the same peer id takes over the map entry — the
	// state a reconnect recovery leaves behind while the old socket lingers.
	replacement := &PeerConnection{PeerID: "crier-under-test", done: make(chan struct{})}
	client.mu.Lock()
	client.connections["crier-under-test"] = replacement
	client.mu.Unlock()

	// The dialed socket dies late. Its registered OnClose must remove only its
	// own registration; the replacement must still be the stored entry.
	// handlerDone proves the SERVER saw the close; the client-side readLoop's
	// OnClose callback runs on its own goroutine, so give it a bounded
	// settlement window — a buggy unconditional delete surfaces within it
	// (observed immediately on the pre-fix tree), the guarded callback
	// changes nothing and the entry stays.
	_ = dialedSocket.Close()
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the dialed socket's read pump never exited")
	}
	settlement := time.Now().Add(2 * time.Second)
	for time.Now().Before(settlement) {
		client.mu.RLock()
		gone := client.connections["crier-under-test"] != replacement
		client.mu.RUnlock()
		if gone {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Read under the mesh's lock: the close handler may still be writing, and
	// the lock is the happens-before edge the race detector needs.
	client.mu.RLock()
	entry := client.connections["crier-under-test"]
	client.mu.RUnlock()
	if entry != replacement {
		t.Fatalf("the dialed socket's late OnClose unregistered the replacement: stored entry = %v, want the replacement still registered", entry)
	}
	found := false
	for _, id := range client.PeerIDs() {
		if id == "crier-under-test" {
			found = true
		}
	}
	if !found {
		t.Error("PeerIDs() lost crier-under-test after the stale dialed socket closed")
	}
}
