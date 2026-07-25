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

type PeerStatus struct {
	PendingRequests int `json:"pending_requests"`
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
