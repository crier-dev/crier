package registry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/totalwindupflightsystems/crier/internal/webhook"
)

// Store persists registered agents and their lease-based inboxes.
// Implementations must be safe for concurrent callers.
type Store interface {
	Register(agent *Agent) error
	Get(id string) (*Agent, error)
	List() []*Agent
	Unregister(id string) error
	Deliver(agentID string, entry *InboxEntry) error
	Retrieve(agentID string, leaseDuration time.Duration, maxMessages int) ([]*InboxEntry, string, error)
	Ack(agentID, leaseID string, messageIDs []string) error
	Stats(agentID string) (queueDepth, leasedCount int, oldestAge time.Duration, err error)
	PurgeExpired() int
}

// updater is an optional Store capability: persistence of an agent's
// mutable registration fields (CR-FEAT-007 PATCH /agents/{id}). Stores that
// cannot update (e.g. remote proxies) leave it unimplemented and the
// handler answers 501 for them.
type updater interface {
	Update(agent *Agent) error
}

// Handler keeps HTTP concerns separate from storage implementations.
type Handler struct {
	store Store
	// requireAgentSig enforces per-agent ed25519 request signing on the
	// agent-owned routes (retrieve/ack/stats/unregister). When false, only
	// the shared Bearer token is required (legacy behavior).
	requireAgentSig bool
	// webhooks pushes deliveries to agent webhook endpoints when configured
	// (CR-FEAT-001). Nil disables webhook delivery.
	webhooks *webhook.Driver
}

// NewHandler creates a Handler that delegates store operations to the
// provided Store implementation. The handler is safe for concurrent callers
// if the underlying store is.
func NewHandler(store Store) *Handler {
	return &Handler{store: store}
}

// SetWebhookDriver enables push delivery to agent webhook endpoints.
func (h *Handler) SetWebhookDriver(d *webhook.Driver) {
	h.webhooks = d
}

// SetRequireAgentSig toggles per-agent ed25519 signature enforcement.
func (h *Handler) SetRequireAgentSig(enabled bool) {
	h.requireAgentSig = enabled
}

var (
	ErrAgentNotFound     = errors.New("agent not found")
	ErrAgentExists       = errors.New("agent already registered")
	ErrLeaseConflict     = errors.New("message is not leased under the supplied lease")
	ErrInvalidStoreInput = errors.New("invalid store input")
)

// MemoryStore is a thread-safe in-memory agent registry with process-lifetime inboxes.
// State is ephemeral: agents and undelivered messages are lost on restart.
// Use PostgresStore (CR_DATABASE_URL) for durability across restarts.
type MemoryStore struct {
	mu      sync.RWMutex
	agents  map[string]*Agent
	inboxes map[string][]*InboxEntry
}

// NewMemoryStore returns an in-memory agent registry with process-lifetime
// inbox persistence (ephemeral across restarts).
// Uses crypto/rand for lease IDs and sync.RWMutex for thread safety.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		agents:  make(map[string]*Agent),
		inboxes: make(map[string][]*InboxEntry),
	}
}

var _ Store = (*MemoryStore)(nil)

// newLeaseID generates a 16-byte crypto-random hex string for lease identification.
func newLeaseID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
