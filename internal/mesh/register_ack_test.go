package mesh

// CR-REVIEW-002 — the handshake is answered: a REGISTER is followed by one
// REGISTER_ACK on the same socket, correlated on the REGISTER's message_id.
//
// The properties pinned here are the ones the task turns on:
//
//   - a FIRST-CONNECT client that sends REGISTER receives a REGISTER_ACK (the
//     gap was that it received nothing at all until the first KEEPALIVE, 30s
//     away, so "registered" and "silently dropped" were indistinguishable);
//   - the ack ECHOES the REGISTER's message_id in request_id — the same
//     correlation field RESPONSE and ERROR carry — and carries a message_id of
//     its own, so a client cannot confuse the two fields;
//   - the ack reports the SERVER's own cadence and horizon
//     (KeepaliveInterval, LeaseTTL), not values echoed from the REGISTER;
//   - the ack is specific to REGISTER: a KEEPALIVE still draws nothing, and an
//     inbound REGISTER_ACK (a server→agent frame with no meaning in this
//     direction) draws nothing back — no ack ping-pong;
//   - a REGISTER the server could not read is answered with the ERROR refusal
//     it always was — never an ack, and never silence.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// registerFrame builds the REGISTER the shipped clients send, with a caller
// chosen message_id so the ack can be correlated against it.
func registerFrame(t *testing.T, agentID, messageID string) []byte {
	t.Helper()
	data, err := Marshal(&Register{
		Envelope: Envelope{
			Type:      TypeRegister,
			Version:   1,
			MessageID: messageID,
			Timestamp: time.Now(),
		},
		AgentID:    agentID,
		LeaseID:    "",
		LeaseTTLMs: 3600000,
		Capabilities: Capabilities{
			Version:               "0.1.0",
			Topics:                []string{},
			MaxConcurrentSessions: 10,
		},
	})
	if err != nil {
		t.Fatalf("marshal REGISTER: %v", err)
	}
	return data
}

// readOneFrame reads exactly one frame from conn, with a deadline, and returns
// its raw bytes and envelope.
func readOneFrame(t *testing.T, conn *websocket.Conn, what string) ([]byte, Envelope) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("%s: no frame arrived (read: %v)", what, err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("%s: frame %q is not an envelope: %v", what, raw, err)
	}
	return raw, env
}

// TestMeshRegisterIsAnsweredWithARegisterAckEchoingItsMessageID is the
// acceptance test for the handshake signal: a WS mesh client sends REGISTER and
// the FIRST frame it reads back is a REGISTER_ACK whose request_id is the
// REGISTER's message_id. Before this existed the read timed out.
func TestMeshRegisterIsAnsweredWithARegisterAckEchoingItsMessageID(t *testing.T) {
	m, srv := inboxNotifyMesh(t)
	conn := dialMesh(t, srv, "agent-a", "")

	const registerMessageID = "f0e1d2c3b4a5968778695a4b"
	if err := conn.WriteMessage(websocket.TextMessage, registerFrame(t, "agent-a", registerMessageID)); err != nil {
		t.Fatalf("write REGISTER: %v", err)
	}

	raw, env := readOneFrame(t, conn, "REGISTER")
	if env.Type != TypeRegisterAck {
		t.Fatalf("the first frame after REGISTER is a %s, want %s (raw: %s) — a client must see the handshake answered",
			env.Type, TypeRegisterAck, raw)
	}

	var ack RegisterAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatalf("unmarshal REGISTER_ACK: %v (raw: %s)", err, raw)
	}

	// The correlation: the ack echoes the REGISTER it answers, in request_id.
	if ack.RequestID != registerMessageID {
		t.Errorf("REGISTER_ACK request_id = %q, want the REGISTER's message_id %q", ack.RequestID, registerMessageID)
	}
	// The frame's OWN id is a fresh one, never the correlation value: a client
	// that matched on message_id would otherwise match its own REGISTER.
	if !isMessageID(ack.MessageID) {
		t.Errorf("REGISTER_ACK message_id = %q, want a fresh 24-hex id", ack.MessageID)
	}
	if ack.MessageID == ack.RequestID {
		t.Errorf("REGISTER_ACK uses %q for both its message_id and its request_id — the two fields must be distinguishable", ack.MessageID)
	}
	if ack.Version != 1 {
		t.Errorf("REGISTER_ACK version = %d, want 1", ack.Version)
	}
	if ack.Timestamp.IsZero() {
		t.Error("REGISTER_ACK timestamp is the zero time")
	} else if skew := time.Since(ack.Timestamp); skew < 0 || skew > 10*time.Second {
		t.Errorf("REGISTER_ACK timestamp is %s away from now — not a live frame", skew)
	}

	// The server's OWN cadence and horizon, not the REGISTER's requested TTL.
	if want := int(m.config.KeepaliveInterval.Milliseconds()); ack.KeepaliveIntervalMs != want {
		t.Errorf("REGISTER_ACK keepalive_interval_ms = %d, want the server's own interval %d", ack.KeepaliveIntervalMs, want)
	}
	if ack.KeepaliveIntervalMs != 30000 {
		t.Errorf("REGISTER_ACK keepalive_interval_ms = %d, want 30000 (the documented default)", ack.KeepaliveIntervalMs)
	}
	if got := ack.ExpiresAt.Sub(ack.Timestamp); got != m.config.LeaseTTL {
		t.Errorf("REGISTER_ACK expires_at is %s after its timestamp, want the configured LeaseTTL %s (the granted horizon)", got, m.config.LeaseTTL)
	}
}

// TestMeshRegisterAckIsOneFramePerRegister pins the "one ack per accepted
// REGISTER" contract the protocol doc states: a socket that registers twice
// reads an ack for each, each correlated with ITS register.
func TestMeshRegisterAckIsOneFramePerRegister(t *testing.T) {
	_, srv := inboxNotifyMesh(t)
	conn := dialMesh(t, srv, "agent-double", "")

	ids := []string{"1111111111111111111111aa", "2222222222222222222222bb"}
	for _, id := range ids {
		if err := conn.WriteMessage(websocket.TextMessage, registerFrame(t, "agent-double", id)); err != nil {
			t.Fatalf("write REGISTER %s: %v", id, err)
		}
		raw, env := readOneFrame(t, conn, "REGISTER "+id)
		if env.Type != TypeRegisterAck {
			t.Fatalf("after REGISTER %s the next frame is a %s, want %s", id, env.Type, TypeRegisterAck)
		}
		var ack RegisterAck
		if err := json.Unmarshal(raw, &ack); err != nil {
			t.Fatalf("unmarshal REGISTER_ACK: %v", err)
		}
		if ack.RequestID != id {
			t.Errorf("REGISTER_ACK request_id = %q, want %q", ack.RequestID, id)
		}
	}
}

// TestMeshRegisterAckAnswersOnlyRegister is the negative control that keeps the
// new emit path from becoming a general "answer everything" behaviour: a
// KEEPALIVE still draws nothing at all, and an inbound REGISTER_ACK (which has
// no meaning in this direction) draws nothing back — so a client that echoes an
// ack cannot start an ack ping-pong. Each arm reads on ITS OWN socket, because a
// read deadline that expires marks the gorilla connection failed for good.
func TestMeshRegisterAckAnswersOnlyRegister(t *testing.T) {
	m, srv := inboxNotifyMesh(t)

	t.Run("KEEPALIVE still draws no reply", func(t *testing.T) {
		conn := dialMesh(t, srv, "agent-quiet", "")
		if err := conn.WriteMessage(websocket.TextMessage, keepaliveFrame(t, "agent-quiet")); err != nil {
			t.Fatalf("write KEEPALIVE: %v", err)
		}
		expectNoReply(t, conn, "KEEPALIVE after the ack path landed")
	})

	t.Run("inbound REGISTER_ACK draws no reply", func(t *testing.T) {
		conn := dialMesh(t, srv, "agent-echo", "")
		// A well-formed ack, as a peer would echo one back.
		ackData, err := Marshal(&RegisterAck{
			Envelope: Envelope{
				Type:      TypeRegisterAck,
				Version:   1,
				MessageID: "3333333333333333333333cc",
				Timestamp: time.Now(),
			},
			RequestID:           "4444444444444444444444dd",
			ExpiresAt:           time.Now().Add(time.Hour),
			KeepaliveIntervalMs: 30000,
		})
		if err != nil {
			t.Fatalf("marshal REGISTER_ACK: %v", err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, ackData); err != nil {
			t.Fatalf("write REGISTER_ACK: %v", err)
		}
		// It is recognized (not refused): no ERROR either.
		expectNoReply(t, conn, "inbound REGISTER_ACK")
		// ...and the peer is still registered and usable on that socket: the
		// echo changed no state.
		if m.ActivePeers() == 0 {
			t.Error("the echo of a REGISTER_ACK dropped the peer from the mesh")
		}
	})
}

// TestMeshRegisterAckShapeIsValidatedOnTheWire pins the two halves of the ack's
// malformed handling in one place: an unreadable REGISTER is answered with the
// ERROR refusal and NOTHING else (no ack), and a REGISTER_ACK whose payload is
// not the ack's own shape (here a REGISTER-only lease_ttl_ms) is still refused
// INVALID_MESSAGE — the ack type is not a hole in the malformed-frame contract.
func TestMeshRegisterAckShapeIsValidatedOnTheWire(t *testing.T) {
	t.Run("a malformed REGISTER draws the ERROR, not an ack", func(t *testing.T) {
		clientA, serverA := startAgentEndpoint(t)
		connA := clientA()

		m := NewMesh(DefaultMeshConfig("relay"))
		acceptAgent(t, m, "agent-a", connA, serverA())

		// malformedFrameReply requires the FIRST frame on the socket to be the
		// INVALID_MESSAGE refusal correlated with this frame — so an ack sent
		// for an unreadable REGISTER fails this arm.
		errMsg, _ := malformedFrameReply(t, connA,
			`{"type":"REGISTER","version":1,"message_id":"reg-unreadable","agent_id":"agent-a","lease_ttl_ms":"thirty"}`)
		if errMsg.Error.Code != ErrCodeInvalidMessage {
			t.Errorf("ERROR code = %q, want %q", errMsg.Error.Code, ErrCodeInvalidMessage)
		}
	})

	t.Run("a REGISTER_ACK carrying register-only fields is refused", func(t *testing.T) {
		clientA, serverA := startAgentEndpoint(t)
		connA := clientA()

		m := NewMesh(DefaultMeshConfig("relay"))
		acceptAgent(t, m, "agent-a", connA, serverA())

		malformedFrameReply(t, connA,
			`{"type":"REGISTER_ACK","version":1,"message_id":"ack-bad-shape","lease_ttl_ms":"thirty"}`)
	})
}
