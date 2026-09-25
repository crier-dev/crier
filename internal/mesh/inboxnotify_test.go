package mesh

// CR-FEAT-023 (deliverable 2) — the new-message ping, as shipped: a tick on
// the agent's EXISTING mesh socket.
//
// The properties pinned here are the ones that make the ping safe to add to a
// live protocol:
//
//   - an OPTED-IN connection receives one INBOX_NOTIFY frame per ping, naming
//     the agent, the inbox message id and the sender — and carrying no payload
//     (it is a tick, not a delivery: the message is already durable);
//   - a connection that did NOT ask receives nothing at all, and PingInbox
//     reports false instead of writing;
//   - the opt-in is PER CONNECTION: it dies with the socket it was granted on;
//   - an INBOUND INBOX_NOTIFY is recognized and ignored, never answered — the
//     frame has no meaning in that direction, and a client that echoes one must
//     not draw an INVALID_MESSAGE.

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// inboxNotifyMesh boots a mesh with the routes cmd/server registers.
func inboxNotifyMesh(t *testing.T) (*Mesh, *httptest.Server) {
	t.Helper()
	m := NewMesh(DefaultMeshConfig("crier"))
	t.Cleanup(m.Stop)
	r := mux.NewRouter()
	r.HandleFunc("/mesh/connect/{agentID}", HandleConnect(m))
	r.HandleFunc("/mesh/peers", HandlePeers(m)).Methods("GET")
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return m, srv
}

func dialMesh(t *testing.T, srv *httptest.Server, agentID, query string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/mesh/connect/" + agentID + query
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", url, err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// awaitCondition polls until cond holds, failing the test after 3s.
func awaitCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func awaitOptIn(t *testing.T, m *Mesh, agentID string) {
	t.Helper()
	awaitCondition(t, "the inbox_notify opt-in for "+agentID, func() bool {
		return m.InboxNotifyEnabled(agentID)
	})
}

// expectNoFrame asserts nothing arrives on conn within wait, and (like every
// read on a gorilla connection) ends the connection's usefulness — call it last.
func expectNoFrame(t *testing.T, conn *websocket.Conn, wait time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	_, data, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("unexpected frame on a socket that should stay silent: %s", data)
	}
	timeout, ok := err.(net.Error)
	if !ok || !timeout.Timeout() {
		t.Fatalf("read failed with %v, want a read-deadline timeout (no frame)", err)
	}
}

func TestMeshInboxNotifyFrameReachesAnOptedInPeer(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	conn := dialMesh(t, srv, "bob", "?inbox_notify=1")
	awaitOptIn(t, m, "bob")

	if !m.PingInbox("bob", "inbox-msg-1", "alice") {
		t.Fatal("PingInbox reported false for an opted-in, connected agent")
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no INBOX_NOTIFY frame arrived: %v", err)
	}
	var frame InboxNotify
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode INBOX_NOTIFY %s: %v", data, err)
	}
	if frame.Type != TypeInboxNotify {
		t.Errorf("frame type = %q, want %q", frame.Type, TypeInboxNotify)
	}
	if frame.Version != 1 {
		t.Errorf("frame version = %d, want 1", frame.Version)
	}
	if frame.MessageID == "" || frame.Timestamp.IsZero() {
		t.Errorf("frame envelope incomplete: message_id=%q timestamp=%v", frame.MessageID, frame.Timestamp)
	}
	if frame.AgentID != "bob" {
		t.Errorf("frame agent_id = %q, want bob (the pinged agent, not the sender)", frame.AgentID)
	}
	if frame.InboxMessageID != "inbox-msg-1" {
		t.Errorf("frame inbox_message_id = %q, want inbox-msg-1", frame.InboxMessageID)
	}
	if frame.Sender != "alice" {
		t.Errorf("frame sender = %q, want alice", frame.Sender)
	}
	// A tick, not a delivery: no payload travels with the ping (the message is
	// already durable in the inbox, which is the whole point).
	if strings.Contains(string(data), "payload") {
		t.Errorf("INBOX_NOTIFY carried a payload: %s", data)
	}
}

func TestMeshInboxNotifyRequiresTheOptIn(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	conn := dialMesh(t, srv, "carol", "")
	// Connected, but not opted in.
	awaitCondition(t, "carol to be a connected peer", func() bool {
		for _, id := range m.PeerIDs() {
			if id == "carol" {
				return true
			}
		}
		return false
	})
	if m.InboxNotifyEnabled("carol") {
		t.Fatal("a connection without ?inbox_notify=1 is reported as opted in")
	}
	if m.PingInbox("carol", "inbox-msg-2", "alice") {
		t.Fatal("PingInbox wrote to a connection that never asked for pings")
	}
	expectNoFrame(t, conn, 300*time.Millisecond)
}

func TestMeshInboxNotifyForAnUnconnectedAgentIsFalse(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	conn := dialMesh(t, srv, "bob", "?inbox_notify=1")
	awaitOptIn(t, m, "bob")

	if m.PingInbox("nobody-here", "inbox-msg-3", "alice") {
		t.Fatal("PingInbox reported success for an agent with no connection")
	}
	// The opted-in agent is untouched by a ping aimed at someone else.
	expectNoFrame(t, conn, 200*time.Millisecond)
}

func TestMeshInboxNotifyOptInDiesWithItsConnection(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	conn := dialMesh(t, srv, "bob", "?inbox_notify=1")
	awaitOptIn(t, m, "bob")

	if err := conn.Close(); err != nil {
		t.Fatalf("close client socket: %v", err)
	}
	awaitCondition(t, "the opt-in to be released with the socket", func() bool {
		return !m.InboxNotifyEnabled("bob")
	})
	if m.PingInbox("bob", "inbox-msg-4", "alice") {
		t.Fatal("PingInbox wrote to a closed connection's opt-in")
	}
}

// TestMeshAcceptPeerDropsAStaleOptIn covers the reconnect path without a
// socket: the grant belongs to ONE connection, so a fresh accept (a client that
// reconnected WITHOUT asking) must not inherit it.
func TestMeshAcceptPeerDropsAStaleOptIn(t *testing.T) {
	m := NewMesh(DefaultMeshConfig("crier"))
	m.SetInboxNotify("bob", true)
	if !m.InboxNotifyEnabled("bob") {
		t.Fatal("SetInboxNotify(true) did not record the grant")
	}
	pc := &PeerConnection{PeerID: "bob", done: make(chan struct{})}
	m.AcceptPeer("bob", pc)
	if m.InboxNotifyEnabled("bob") {
		t.Fatal("a fresh connection inherited the previous connection's ping grant")
	}
	if m.PingInbox("bob", "inbox-msg-5", "alice") {
		t.Fatal("PingInbox wrote to a connection that never asked for pings")
	}
	m.SetInboxNotify("bob", false)
	m.Stop()
}

func TestMeshConnectRejectsANonBooleanInboxNotify(t *testing.T) {
	_, srv := inboxNotifyMesh(t)

	resp, err := http.Get(srv.URL + "/mesh/connect/bob?inbox_notify=maybe")
	if err != nil {
		t.Fatalf("GET connect with a bad opt-in: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode rejection body: %v", err)
	}
	if !strings.Contains(body.Error, inboxNotifyParam) {
		t.Errorf("rejection %q must name %s", body.Error, inboxNotifyParam)
	}
}

// TestMeshInboundInboxNotifyIsIgnoredNotRefused pins the direction rule: the
// frame is server→agent, so an inbound one is recognized and ignored. The
// assertion is the ABSENCE of a reply — a client that echoes a ping must not
// draw the INVALID_MESSAGE a genuinely unknown type draws.
func TestMeshInboundInboxNotifyIsIgnoredNotRefused(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	conn := dialMesh(t, srv, "bob", "?inbox_notify=1")
	awaitOptIn(t, m, "bob")

	echo, err := Marshal(&InboxNotify{
		Envelope:       Envelope{Type: TypeInboxNotify, Version: 1, MessageID: "echo-1", Timestamp: time.Now()},
		AgentID:        "bob",
		InboxMessageID: "inbox-msg-6",
		Sender:         "alice",
	})
	if err != nil {
		t.Fatalf("marshal echo: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, echo); err != nil {
		t.Fatalf("write echo: %v", err)
	}
	expectNoFrame(t, conn, 500*time.Millisecond)
}

// TestMeshUnknownFrameTypeIsStillRefused is the negative control for the test
// above: an actually-unknown type must still be answered with INVALID_MESSAGE,
// so "ignored" is a property of INBOX_NOTIFY and not of a muted router.
func TestMeshUnknownFrameTypeIsStillRefused(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	conn := dialMesh(t, srv, "bob", "")
	awaitCondition(t, "bob to be a connected peer", func() bool {
		for _, id := range m.PeerIDs() {
			if id == "bob" {
				return true
			}
		}
		return false
	})

	bogus, err := Marshal(&Envelope{Type: MessageType("NOT_A_FRAME_TYPE"), Version: 1,
		MessageID: "bogus-1", Timestamp: time.Now()})
	if err != nil {
		t.Fatalf("marshal bogus frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, bogus); err != nil {
		t.Fatalf("write bogus frame: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("no refusal for an unknown frame type: %v", err)
	}
	var refused ErrorMessage
	if err := json.Unmarshal(data, &refused); err != nil {
		t.Fatalf("decode refusal %s: %v", data, err)
	}
	if refused.Type != TypeError || refused.Error.Code != ErrCodeInvalidMessage {
		t.Errorf("refusal = %+v, want an ERROR frame with %s", refused, ErrCodeInvalidMessage)
	}
}
