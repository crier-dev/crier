package mesh

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// malformedFrameReply writes one frame and returns the FIRST frame the mesh
// answers with, requiring it to be an ERROR. Reading exactly one frame — rather
// than "the next ERROR frame" — is deliberate: it fails when a malformed frame
// draws no reply at all (the pre-DF-CRIER-40 behaviour this file exists for),
// and it also fails when the reply is something other than the refusal.
func malformedFrameReply(t *testing.T, conn *websocket.Conn, frame string) (ErrorMessage, []byte) {
	t.Helper()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
		t.Fatalf("write malformed frame %q: %v", frame, err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("malformed frame %q: no reply (read: %v) — the mesh must refuse it with INVALID_MESSAGE, not drop it", frame, err)
	}
	var env Envelope
	if err := json.Unmarshal(msg, &env); err != nil {
		t.Fatalf("malformed frame %q: reply %q is not an envelope: %v", frame, msg, err)
	}
	if env.Type != TypeError {
		t.Fatalf("malformed frame %q: reply type = %q, want %q", frame, env.Type, TypeError)
	}
	if env.MessageID == "" {
		t.Errorf("malformed frame %q: the ERROR frame carries no message_id of its own", frame)
	}
	var errMsg ErrorMessage
	if err := json.Unmarshal(msg, &errMsg); err != nil {
		t.Fatalf("malformed frame %q: unmarshal ERROR reply: %v", frame, err)
	}
	if errMsg.Error.Code != ErrCodeInvalidMessage {
		t.Errorf("malformed frame %q: ERROR code = %q, want %q", frame, errMsg.Error.Code, ErrCodeInvalidMessage)
	}
	if strings.TrimSpace(errMsg.Error.Message) == "" {
		t.Errorf("malformed frame %q: ERROR message is empty — a refusal has to name what was wrong", frame)
	}
	return errMsg, msg
}

// TestMeshMalformedFramesGetInvalidMessage is the DF-CRIER-40 acceptance test.
// Every inbound frame the mesh cannot make sense of is answered on the wire with
// the ERROR frame docs/mesh-protocol.md defines for exactly this case — code
// INVALID_MESSAGE, "Malformed frame" — instead of being dropped in silence.
// Before the fix every case below read a socket that stayed empty until the read
// deadline expired.
func TestMeshMalformedFramesGetInvalidMessage(t *testing.T) {
	cases := []struct {
		name          string
		frame         string
		wantRequestID string // "" means request_id must be ABSENT on the wire
		wantInMessage string
	}{
		{
			name:          "not JSON at all",
			frame:         `this is not a frame`,
			wantInMessage: "not a JSON envelope",
		},
		{
			name:          "truncated JSON",
			frame:         `{"type":"REQUEST","version":1,`,
			wantInMessage: "not a JSON envelope",
		},
		{
			name:          "JSON that is not an object",
			frame:         `["REQUEST",1]`,
			wantInMessage: "not a JSON envelope",
		},
		{
			name:          "JSON string",
			frame:         `"REQUEST"`,
			wantInMessage: "not a JSON envelope",
		},
		{
			name:          "envelope with no type",
			frame:         `{"version":1,"message_id":"no-type-1"}`,
			wantRequestID: "no-type-1",
			wantInMessage: "unknown message type",
		},
		{
			name:          "unknown message type",
			frame:         `{"type":"MESSAGE","version":1,"message_id":"typo-1"}`,
			wantRequestID: "typo-1",
			wantInMessage: `unknown message type "MESSAGE"`,
		},
		{
			name:          "REGISTER with a wrong-shaped payload",
			frame:         `{"type":"REGISTER","version":1,"message_id":"reg-bad-1","agent_id":"agent-a","lease_ttl_ms":"thirty"}`,
			wantRequestID: "reg-bad-1",
			wantInMessage: "malformed REGISTER frame",
		},
		{
			name:          "REGISTER_ACK with a wrong-shaped payload",
			frame:         `{"type":"REGISTER_ACK","version":1,"message_id":"ack-bad-1","lease_ttl_ms":"thirty"}`,
			wantRequestID: "ack-bad-1",
			wantInMessage: "malformed REGISTER_ACK frame",
		},
		{
			name:          "REQUEST with a wrong-shaped payload",
			frame:         `{"type":"REQUEST","version":1,"message_id":"req-bad-1","source":"agent-a","target":{"agent_id":"agent-b"},"method":"GET","path":"/ping","trace_id":"tr-bad-1","timeout_ms":5000}`,
			wantRequestID: "req-bad-1",
			wantInMessage: "malformed REQUEST frame",
		},
		{
			name:          "RESPONSE with a wrong-shaped payload",
			frame:         `{"type":"RESPONSE","version":1,"message_id":"resp-bad-1","request_id":7,"status_code":200}`,
			wantRequestID: "resp-bad-1",
			wantInMessage: "malformed RESPONSE frame",
		},
		{
			name:          "ERROR with a wrong-shaped payload",
			frame:         `{"type":"ERROR","version":1,"message_id":"err-bad-1","error":"not-an-object"}`,
			wantRequestID: "err-bad-1",
			wantInMessage: "malformed ERROR frame",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clientA, serverA := startAgentEndpoint(t)
			connA := clientA()

			m := NewMesh(DefaultMeshConfig("relay"))
			acceptAgent(t, m, "agent-a", connA, serverA())

			errMsg, raw := malformedFrameReply(t, connA, tc.frame)
			if !strings.Contains(errMsg.Error.Message, tc.wantInMessage) {
				t.Errorf("ERROR message = %q, want it to contain %q", errMsg.Error.Message, tc.wantInMessage)
			}

			// request_id is `omitempty`: for a frame that could not be read far
			// enough to have a message_id the field is ABSENT, never blank —
			// that is how a client tells "what I sent was unparseable" from
			// "this correlates to a request of mine".
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("ERROR reply is not a JSON object: %v", err)
			}
			gotID, present := decoded["request_id"]
			if tc.wantRequestID == "" {
				if present {
					t.Errorf("request_id = %s, want the field ABSENT for a frame with no readable message_id", gotID)
				}
				return
			}
			if !present {
				t.Fatalf("request_id is absent, want %q (the malformed frame's own message_id)", tc.wantRequestID)
			}
			var id string
			if err := json.Unmarshal(gotID, &id); err != nil {
				t.Fatalf("request_id = %s, which is not a JSON string: %v", gotID, err)
			}
			if id != tc.wantRequestID {
				t.Errorf("request_id = %q, want %q", id, tc.wantRequestID)
			}
		})
	}
}

// TestMeshWellFormedFramesGetNoError is the negative control for the fix: the
// frames the protocol defines and the server deliberately ignores must NOT draw
// an INVALID_MESSAGE, or the new refusal would fire on healthy traffic —
// KEEPALIVE above all, which every connected peer sends every 30s.
func TestMeshWellFormedFramesGetNoError(t *testing.T) {
	clientA, serverA := startAgentEndpoint(t)
	connA := clientA()

	m := NewMesh(DefaultMeshConfig("relay"))
	acceptAgent(t, m, "agent-a", connA, serverA())

	wellFormed := []string{
		`{"type":"KEEPALIVE","version":1,"message_id":"ka-1","timestamp":"2026-09-19T00:00:00Z","lease_id":"","agent_id":"agent-a"}`,
		`{"type":"REGISTER","version":1,"message_id":"reg-1","timestamp":"2026-09-19T00:00:00Z","agent_id":"agent-a","lease_id":"","lease_ttl_ms":3600000,"capabilities":{"version":"1","topics":[],"max_concurrent_sessions":10}}`,
		`{"type":"REGISTER_ACK","version":1,"message_id":"ack-1","timestamp":"2026-09-19T00:00:00Z"}`,
	}
	for _, frame := range wellFormed {
		if err := connA.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
			t.Fatalf("write %s: %v", frame, err)
		}
	}

	// One malformed frame follows on the same socket, with no readable
	// message_id, so its refusal carries no request_id. The FIRST frame the
	// client reads must be that refusal: had the server answered any of the
	// frames above, that reply would arrive first (and would carry the id of
	// the frame it answered — ka-1/reg-1/ack-1 — which is exactly what the
	// request_id assertion below rejects).
	errMsg, raw := malformedFrameReply(t, connA, `definitely not a frame`)
	if !strings.Contains(errMsg.Error.Message, "not a JSON envelope") {
		t.Errorf("ERROR message = %q, want it to describe the unreadable frame", errMsg.Error.Message)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("ERROR reply is not a JSON object: %v", err)
	}
	if id, present := decoded["request_id"]; present {
		t.Fatalf("first reply on the socket carries request_id = %s — the server answered a well-formed KEEPALIVE/REGISTER/REGISTER_ACK frame", id)
	}

	// Nothing else may follow it. This read is the last thing done on this
	// connection on purpose: gorilla marks the socket failed after a read
	// timeout, so a deadline probe belongs only at the end.
	if err := connA.SetReadDeadline(time.Now().Add(750 * time.Millisecond)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, msg, err := connA.ReadMessage(); err == nil {
		t.Fatalf("received %q after the refusal, want no further frame", msg)
	}
}

// TestMeshStaysUsableAfterMalformedFrame pins that the refusal is not fatal for
// the connection: a client that sent one bad frame can still run a normal
// agent-to-agent REQUEST round trip on the same socket.
func TestMeshStaysUsableAfterMalformedFrame(t *testing.T) {
	clientA, serverA := startAgentEndpoint(t)
	clientB, serverB := startAgentEndpoint(t)
	connA, connB := clientA(), clientB()

	m := NewMesh(DefaultMeshConfig("relay"))
	acceptAgent(t, m, "agent-a", connA, serverA())
	acceptAgent(t, m, "agent-b", connB, serverB())

	malformedFrameReply(t, connA, `{"type":"REQUEST","version":1,"message_id":`)

	req := &Request{
		Envelope: Envelope{
			Type:      TypeRequest,
			Version:   1,
			MessageID: "req-after-malformed",
			Timestamp: time.Now(),
		},
		Source:    PeerRef{AgentID: "agent-a"},
		Target:    PeerRef{AgentID: "agent-b"},
		Method:    "GET",
		Path:      "/ping",
		TraceID:   "trace-after-malformed",
		TimeoutMs: 5000,
	}
	data, err := Marshal(req)
	if err != nil {
		t.Fatalf("marshal REQUEST: %v", err)
	}
	if err := connA.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write REQUEST: %v", err)
	}

	if err := connB.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, relayed, err := connB.ReadMessage()
	if err != nil {
		t.Fatalf("agent-b did not receive the REQUEST relayed after a malformed frame: %v", err)
	}
	var env Envelope
	if err := json.Unmarshal(relayed, &env); err != nil {
		t.Fatalf("relayed frame is not an envelope: %v", err)
	}
	if env.Type != TypeRequest || env.MessageID != req.MessageID {
		t.Errorf("relayed frame = type %q id %q, want type %q id %q",
			env.Type, env.MessageID, TypeRequest, req.MessageID)
	}
}
