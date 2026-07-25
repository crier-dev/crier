package registry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
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

// Handler keeps HTTP concerns separate from storage implementations.
type Handler struct {
	store Store
}

// NewHandler creates a Handler that delegates store operations to the
// provided Store implementation. The handler is safe for concurrent callers
// if the underlying store is.
func NewHandler(store Store) *Handler {
	return &Handler{store: store}
}

var (
	ErrAgentNotFound     = errors.New("agent not found")
	ErrAgentExists       = errors.New("agent already registered")
	ErrLeaseConflict     = errors.New("message is not leased under the supplied lease")
	ErrInvalidStoreInput = errors.New("invalid store input")
)

// MemoryStore is a thread-safe in-memory agent registry with persistent inboxes.
type MemoryStore struct {
	mu      sync.RWMutex
	agents  map[string]*Agent
	inboxes map[string][]*InboxEntry
}

// NewMemoryStore returns an in-memory agent registry with inbox persistence.
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
