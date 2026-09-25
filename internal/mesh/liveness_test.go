package mesh

// CR-FEAT-024 — the mesh's half of presence: the traffic that already flowed is
// now recorded as liveness evidence for the agent whose socket it arrived on.
//
// The properties pinned here:
//
//   - an ACCEPTED socket records evidence for the agent the connect URL named;
//   - an inbound KEEPALIVE records evidence too, and still draws NO reply (the
//     protocol's "no response to KEEPALIVE" rule is unchanged);
//   - the evidence is attributed to the SOCKET's id, never to the frame's own
//     `agent_id` field — a peer cannot name another agent and keep a dead
//     agent's row looking alive;
//   - a mesh with no liveness sink wired records nothing and behaves exactly as
//     it did before the sink existed.

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// livenessWhen returns a sink that reports every recorded heartbeat on a
// buffered channel, plus the channel. Buffered so a sink call from the accept
// path can never block the server's HTTP handler.
func livenessWhen() (chan string, func(string, time.Time)) {
	records := make(chan string, 16)
	return records, func(agentID string, at time.Time) {
		select {
		case records <- agentID:
		default:
		}
	}
}

// awaitRecord waits for one recorded agent id.
func awaitRecord(t *testing.T, what string, records chan string) string {
	t.Helper()
	select {
	case id := <-records:
		return id
	case <-time.After(3 * time.Second):
		t.Fatalf("no liveness evidence recorded for %s", what)
		return ""
	}
}

// expectNoRecord asserts that nothing was recorded within a short window.
func expectNoRecord(t *testing.T, what string, records chan string) {
	t.Helper()
	select {
	case id := <-records:
		t.Fatalf("%s: unexpected liveness evidence for %q", what, id)
	case <-time.After(200 * time.Millisecond):
	}
}

// keepaliveFrame builds the KEEPALIVE frames the shipped clients send. agent_id
// is the frame's OWN claim — the field the mesh deliberately does not trust for
// identity.
func keepaliveFrame(t *testing.T, agentID string) []byte {
	t.Helper()
	data, err := Marshal(&Keepalive{
		Envelope: Envelope{Type: TypeKeepalive, Version: 1, MessageID: "ka-1", Timestamp: time.Now()},
		AgentID:  agentID,
	})
	if err != nil {
		t.Fatalf("marshal KEEPALIVE: %v", err)
	}
	return data
}

// expectNoReply asserts the socket receives nothing for a short window: the
// frame was recognized, and recognized means "no reply of any kind", never an
// ERROR a well-formed frame must not draw.
func expectNoReply(t *testing.T, conn *websocket.Conn, what string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, data, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("%s: the peer received a frame back (%s); a KEEPALIVE draws no reply", what, data)
	}
	if !websocket.IsCloseError(err, websocket.CloseNormalClosure) && !isTimeout(err) {
		t.Fatalf("%s: unexpected read error %v", what, err)
	}
}

func isTimeout(err error) bool {
	type timeouter interface{ Timeout() bool }
	if te, ok := err.(timeouter); ok {
		return te.Timeout()
	}
	return false
}

func TestMeshRecordsLivenessOnAcceptAndOnHeartbeat(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	records, sink := livenessWhen()
	m.SetLivenessRecorder(sink)

	conn := dialMesh(t, srv, "walker", "")

	// The accept itself is evidence: the peer is up and speaking as this id.
	if id := awaitRecord(t, "the accepted socket", records); id != "walker" {
		t.Fatalf("accept recorded liveness for %q, want walker", id)
	}

	// A heartbeat is evidence, and still draws no reply.
	if err := conn.WriteMessage(websocket.TextMessage, keepaliveFrame(t, "walker")); err != nil {
		t.Fatalf("write KEEPALIVE: %v", err)
	}
	if id := awaitRecord(t, "the inbound KEEPALIVE", records); id != "walker" {
		t.Fatalf("KEEPALIVE recorded liveness for %q, want the SOCKET id walker", id)
	}
	expectNoReply(t, conn, "KEEPALIVE")

	// Identity is bound to the socket: a heartbeat that CLAIMS another agent
	// must not record evidence for it. Without this rule any connected peer
	// could keep a dead agent's row looking alive, which is the lie this whole
	// change removes.
	if err := conn.WriteMessage(websocket.TextMessage, keepaliveFrame(t, "victim")); err != nil {
		t.Fatalf("write spoofing KEEPALIVE: %v", err)
	}
	if id := awaitRecord(t, "the spoofing KEEPALIVE", records); id != "walker" {
		t.Fatalf("a KEEPALIVE claiming %q recorded liveness for it; evidence must be attributed to the socket (walker)", id)
	}
}

func TestMeshWithoutLivenessRecorderRecordsNothing(t *testing.T) {
	// No SetLivenessRecorder call at all — the default, and what every caller
	// that never wires a sink gets. Behaviour must be exactly as before: the
	// frame is recognized, nothing is recorded, nothing is sent back.
	m, srv := inboxNotifyMesh(t)
	if m.recorder() != nil {
		t.Fatal("a freshly built mesh has a liveness recorder — the default must be none")
	}

	conn := dialMesh(t, srv, "quiet", "")
	if err := conn.WriteMessage(websocket.TextMessage, keepaliveFrame(t, "quiet")); err != nil {
		t.Fatalf("write KEEPALIVE: %v", err)
	}
	expectNoReply(t, conn, "KEEPALIVE with no sink wired")
}

// TestMeshLivenessRecorderSwap is the wiring seam the server depends on:
// SetLivenessRecorder can be called after the mesh is constructed (the registry
// store only exists later in cmd/server), and a sink set to nil stops recording.
func TestMeshLivenessRecorderSwap(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	records, sink := livenessWhen()
	m.SetLivenessRecorder(sink)

	first := dialMesh(t, srv, "agent-one", "")
	if id := awaitRecord(t, "the first peer", records); id != "agent-one" {
		t.Fatalf("recorded %q, want agent-one", id)
	}

	m.SetLivenessRecorder(nil)
	second := dialMesh(t, srv, "agent-two", "")
	if err := second.WriteMessage(websocket.TextMessage, keepaliveFrame(t, "agent-two")); err != nil {
		t.Fatalf("write KEEPALIVE: %v", err)
	}
	// Nothing new: the swap really removed the sink. (The channel is empty, so
	// this also fails if the first peer's accept recorded twice.)
	expectNoRecord(t, "after the sink was set to nil", records)

	if first == nil {
		t.Fatal("first connection was dropped")
	}
}
