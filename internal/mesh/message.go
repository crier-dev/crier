package mesh

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type MessageType string

const (
	TypeRegister    MessageType = "REGISTER"
	TypeRegisterAck MessageType = "REGISTER_ACK"
	TypeKeepalive   MessageType = "KEEPALIVE"
	TypeRequest     MessageType = "REQUEST"
	TypeResponse    MessageType = "RESPONSE"
	TypeError       MessageType = "ERROR"

	// The connect handshake (DF-CRIER-287). These three exist only on a
	// connection whose server requires mesh authentication
	// (CR_REQUIRE_MESH_AUTH=true): the server challenges the connecting peer,
	// the peer answers with an ed25519 signature over the challenge, and the
	// server admits its frames only after that proof. On the default
	// configuration none of them is ever sent or accepted, and a frame
	// naming one outside the handshake is refused INVALID_MESSAGE.
	TypeAuthChallenge MessageType = "AUTH_CHALLENGE"
	TypeAuthResponse  MessageType = "AUTH_RESPONSE"
	TypeAuthOK        MessageType = "AUTH_OK"

	// TypeInboxNotify is the server→agent new-message ping (CR-FEAT-023):
	// a tick on the agent's EXISTING mesh socket telling it that a message
	// has landed in its durable inbox, so an agent that would otherwise poll
	// on a timer can be woken instead. See InboxNotify.
	TypeInboxNotify MessageType = "INBOX_NOTIFY"
)

type Envelope struct {
	Type      MessageType `json:"type"`
	Version   int         `json:"version"`
	MessageID string      `json:"message_id"`
	Timestamp time.Time   `json:"timestamp"`
}

type Register struct {
	Envelope
	AgentID      string       `json:"agent_id"`
	LeaseID      string       `json:"lease_id"`
	LeaseTTLMs   int          `json:"lease_ttl_ms"`
	Capabilities Capabilities `json:"capabilities"`
}

type Capabilities struct {
	Version               string   `json:"version"`
	Topics                []string `json:"topics"`
	MaxConcurrentSessions int      `json:"max_concurrent_sessions"`
}

type RegisterAck struct {
	Envelope
	ExpiresAt           time.Time `json:"expires_at"`
	KeepaliveIntervalMs int       `json:"keepalive_interval_ms"`
}

type Keepalive struct {
	Envelope
	LeaseID string `json:"lease_id"`
	AgentID string `json:"agent_id"`
}

// AuthChallenge is the server's first frame on a connection it requires
// authentication for (DF-CRIER-287): it names the identity the connect URL
// claims and carries a single-use nonce the peer must sign.
//
// agent_id is the PATH claim under verification, not a fact — that is the
// whole point of the frame. The peer's answer is only accepted if its
// signature verifies against the public key the registry holds FOR THAT ID,
// so a peer that cannot sign as agentID never becomes agentID.
type AuthChallenge struct {
	Envelope
	AgentID string `json:"agent_id"`
	// Nonce is 32 hex chars from crypto/rand, single-use per connection.
	Nonce string `json:"nonce"`
	// ExpiresAt is the instant the challenge stops being accepted. The server
	// also enforces the same bound as a read deadline, so a peer that simply
	// stops sending is refused rather than parked forever.
	ExpiresAt time.Time `json:"expires_at"`
}

// AuthResponse is the peer's answer to an AuthChallenge.
//
// signature is the hex-encoded ed25519 signature over
// MeshAuthPayload(agent_id, nonce) — a payload that binds the claimed
// identity to this one challenge, so a signature captured from another
// connection (or replayed to this one later) verifies nothing.
type AuthResponse struct {
	Envelope
	AgentID   string `json:"agent_id"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

// AuthOK tells the peer its identity is verified and the server now admits
// its frames. The connection is not registered as a mesh peer before this
// frame, so a client that never receives it has no peer presence at all.
type AuthOK struct {
	Envelope
	AgentID string `json:"agent_id"`
}

type PeerStatus struct {
	PendingRequests int `json:"pending_requests"`
}

// InboxNotify is the server→agent new-message ping (CR-FEAT-023). It is sent
// by the server to the agent named in agent_id, and only to a connection that
// ASKED for it: the agent connects with `?inbox_notify=1` on
// /mesh/connect/{agentID}. The opt-in is per connection, so a client that did
// not ask can never receive an unsolicited frame.
//
// The frame is advisory and carries no payload: the message it names is
// already durable in the agent's inbox when the frame is sent, so a client
// that ignores the ping (or never receives it) loses nothing but latency — it
// still finds the message on its next retrieve. inbox_message_id is the id the
// delivery was accepted with (the same id GET /agents/{id}/inbox hands back in
// the entry), and sender is the originating agent id when the delivery named
// one. There is no reply: the client answers by retrieving its inbox.
//
// An inbound INBOX_NOTIFY is recognized and ignored — the frame has no meaning
// in that direction (see Mesh.handleMessage).
type InboxNotify struct {
	Envelope
	AgentID        string `json:"agent_id"`
	InboxMessageID string `json:"inbox_message_id,omitempty"`
	Sender         string `json:"sender,omitempty"`
}

type Request struct {
	Envelope
	Source    PeerRef `json:"source"`
	Target    PeerRef `json:"target"`
	Method    string  `json:"method"`
	Path      string  `json:"path"`
	Body      any     `json:"body,omitempty"`
	TraceID   string  `json:"trace_id"`
	TimeoutMs int     `json:"timeout_ms"`
}

type PeerRef struct {
	AgentID string `json:"agent_id"`
}

type Response struct {
	Envelope
	RequestID  string          `json:"request_id"`
	Source     PeerRef         `json:"source"`
	StatusCode int             `json:"status_code"`
	Body       json.RawMessage `json:"body,omitempty"`
	TraceID    string          `json:"trace_id"`
}

type ErrorMessage struct {
	Envelope
	RequestID string      `json:"request_id,omitempty"`
	Error     ErrorDetail `json:"error"`
	TraceID   string      `json:"trace_id,omitempty"`
}

type ErrorDetail struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	RetryAfterMs int    `json:"retry_after_ms,omitempty"`
}

const (
	ErrCodeControllerOffline = "CONTROLLER_OFFLINE"
	ErrCodeRateLimited       = "RATE_LIMITED"
	ErrCodeInvalidMessage    = "INVALID_MESSAGE"
	ErrCodeAuthFailed        = "AUTH_FAILED"
	ErrCodeForbidden         = "FORBIDDEN"
	ErrCodeInternal          = "INTERNAL"
)

// Marshal encodes v as JSON and appends a newline delimiter.
// This is the standard wire format for Crier mesh messages.
func Marshal(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return append(data, '\n'), nil
}

func newMessageID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
