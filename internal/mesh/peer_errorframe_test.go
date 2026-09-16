package mesh

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// malformedRequestFrame builds the wrong-shaped REQUEST a client sends when it
// uses its own "from"/"to" spellings instead of the wire's source/target
// PeerRef objects. It is assembled from a map, not from a Request value, so the
// frame genuinely carries the wrong field shape rather than a correctly-shaped
// envelope whose fields happen to be empty (DF-CRIER-143).
func malformedRequestFrame(t *testing.T, messageID string) []byte {
	t.Helper()
	frame := map[string]any{
		"type":       string(TypeRequest),
		"version":    1,
		"message_id": messageID,
		"timestamp":  time.Now().UTC().Format(time.RFC3339Nano),
		"from":       "agent-a",
		"to":         "ghost-143",
		"method":     "GET",
		"path":       "/ping",
		"body":       map[string]any{},
		"trace_id":   "trace-malformed",
		"timeout_ms": 5000,
	}
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal malformed REQUEST frame: %v", err)
	}
	return data
}

// TestMeshMalformedRequestGetsInvalidMessage drives the WS path end to end: a
// REQUEST whose target cannot be resolved must be answered INVALID_MESSAGE with
// a message that names the missing target, not CONTROLLER_OFFLINE with a blank
// peer identifier ("peer  not connected"), which misdirects the reader to the
// controller/connectivity subsystem (DF-CRIER-143).
func TestMeshMalformedRequestGetsInvalidMessage(t *testing.T) {
	const requestID = "req-malformed-143"

	clientA, serverA := startAgentEndpoint(t)
	connA := clientA()

	m := NewMesh(DefaultMeshConfig("relay"))
	acceptAgent(t, m, "agent-a", connA, serverA())

	if err := connA.WriteMessage(websocket.TextMessage, malformedRequestFrame(t, requestID)); err != nil {
		t.Fatalf("write malformed REQUEST: %v", err)
	}

	msg, env := readUntilType(t, connA, TypeError, 5*time.Second)
	if env.MessageID == "" {
		t.Fatal("ERROR envelope missing message_id")
	}
	var errMsg ErrorMessage
	if err := json.Unmarshal(msg, &errMsg); err != nil {
		t.Fatalf("unmarshal ERROR frame: %v", err)
	}
	if errMsg.RequestID != requestID {
		t.Errorf("ERROR request_id = %q, want %q", errMsg.RequestID, requestID)
	}
	if errMsg.Error.Code != ErrCodeInvalidMessage {
		t.Errorf("ERROR code = %q, want %q", errMsg.Error.Code, ErrCodeInvalidMessage)
	}

	got := errMsg.Error.Message
	if got == "" {
		t.Error("ERROR message is empty; a malformed REQUEST must name what is missing")
	}
	if strings.Contains(got, "  ") {
		t.Errorf("ERROR message = %q, contains a double space (a blank identifier)", got)
	}
	if strings.Contains(got, "not connected") {
		t.Errorf("ERROR message = %q, misclassifies a malformed REQUEST as an offline peer", got)
	}
	if !strings.Contains(got, "target") {
		t.Errorf("ERROR message = %q, does not name the missing target", got)
	}
}

// TestMeshWellFormedRequestToOfflinePeerGetsControllerOffline pins the
// neighbouring branch so the malformed-frame fix cannot regress it: a
// correctly-shaped REQUEST whose target simply is not connected still answers
// CONTROLLER_OFFLINE and still names that peer (DF-CRIER-143).
func TestMeshWellFormedRequestToOfflinePeerGetsControllerOffline(t *testing.T) {
	const (
		ghostPeer = "ghost-143"
		requestID = "req-offline-143"
	)

	clientA, serverA := startAgentEndpoint(t)
	connA := clientA()

	m := NewMesh(DefaultMeshConfig("relay"))
	acceptAgent(t, m, "agent-a", connA, serverA())

	req := &Request{
		Envelope: Envelope{
			Type:      TypeRequest,
			Version:   1,
			MessageID: requestID,
			Timestamp: time.Now(),
		},
		Source:    PeerRef{AgentID: "agent-a"},
		Target:    PeerRef{AgentID: ghostPeer},
		Method:    "GET",
		Path:      "/ping",
		TraceID:   "trace-offline",
		TimeoutMs: 5000,
	}
	data, err := Marshal(req)
	if err != nil {
		t.Fatalf("marshal REQUEST: %v", err)
	}
	if err := connA.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write REQUEST: %v", err)
	}

	msg, env := readUntilType(t, connA, TypeError, 5*time.Second)
	if env.MessageID == "" {
		t.Fatal("ERROR envelope missing message_id")
	}
	var errMsg ErrorMessage
	if err := json.Unmarshal(msg, &errMsg); err != nil {
		t.Fatalf("unmarshal ERROR frame: %v", err)
	}
	if errMsg.RequestID != requestID {
		t.Errorf("ERROR request_id = %q, want %q", errMsg.RequestID, requestID)
	}
	if errMsg.Error.Code != ErrCodeControllerOffline {
		t.Errorf("ERROR code = %q, want %q", errMsg.Error.Code, ErrCodeControllerOffline)
	}
	if !strings.Contains(errMsg.Error.Message, ghostPeer) {
		t.Errorf("ERROR message = %q, want it to contain the target peer id %q",
			errMsg.Error.Message, ghostPeer)
	}
}
