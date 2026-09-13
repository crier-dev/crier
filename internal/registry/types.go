package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/webhook"
)

// AgentStatus represents the online/offline state of an agent.
type AgentStatus string

const (
	StatusOnline  AgentStatus = "online"
	StatusOffline AgentStatus = "offline"
)

// HexKey is an ed25519.PublicKey that marshals as hex in JSON.
type HexKey ed25519.PublicKey

// MarshalJSON encodes the key as a hex string.
func (k HexKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(hex.EncodeToString(k))
}

// UnmarshalJSON decodes a hex string into the key.
func (k *HexKey) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("invalid hex key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid key length: %d, want %d", len(raw), ed25519.PublicKeySize)
	}
	*k = make(HexKey, ed25519.PublicKeySize)
	copy(*k, raw)
	return nil
}

// Agent represents a registered agent in the system.
type Agent struct {
	ID           string      `json:"id"`
	PublicKey    HexKey      `json:"public_key"`
	Capabilities []string    `json:"capabilities"`
	Status       AgentStatus `json:"status"`
	RegisteredAt time.Time   `json:"registered_at"`
	LastSeen     time.Time   `json:"last_seen"`
	// Webhook is the optional push-delivery endpoint (CR-FEAT-001).
	Webhook *webhook.Config `json:"webhook,omitempty"`
	// Guard is the optional LLM message-guard policy config (CR-FEAT-010).
	// API keys are never part of it — only env: refs (spec §4.1/§9.2).
	Guard *guard.AgentGuardConfig `json:"guard,omitempty"`
}

// InboxEntry is a message stored in an agent's persistent inbox.
type InboxEntry struct {
	ID            string        `json:"id"`
	AgentID       string        `json:"agent_id"`
	Payload       []byte        `json:"payload"`
	CreatedAt     time.Time     `json:"created_at"`
	ExpiresAt     time.Time     `json:"expires_at"`
	LeasedAt      *time.Time    `json:"leased_at,omitempty"`
	LeaseID       string        `json:"lease_id,omitempty"`
	LeaseDuration time.Duration `json:"-"` // not serialized; used by PurgeExpired
	ACKed         bool          `json:"acked"`
	// Guard carries the LLM message-guard metadata for this entry
	// (CR-FEAT-010, spec §9.3). Present on guarded deliveries; absent when
	// the guard is disabled.
	Guard *guard.Meta `json:"guard,omitempty"`
}
