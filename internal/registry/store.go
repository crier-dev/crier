package registry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/webhook"
)

// Store persists registered agents and their lease-based inboxes.
// Implementations must be safe for concurrent callers.
type Store interface {
	Register(agent *Agent) error
	Get(id string) (*Agent, error)
	List() []*Agent
	Unregister(id string) error
	Deliver(agentID string, entry *InboxEntry) error
	// Retrieve atomically leases up to maxMessages un-ACKed, un-expired
	// messages from the agent's FIFO inbox and returns them with the lease ID
	// that covers them. The lease ID is non-empty exactly when at least one
	// message was leased: a registered agent with nothing claimable (empty
	// inbox, or every queued message already leased and unexpired) yields a
	// non-nil empty slice and an EMPTY lease ID — no lease is minted, because
	// there is nothing an ack could legitimately cover (DF-CRIER-32).
	// Concurrent callers get disjoint batches.
	Retrieve(agentID string, leaseDuration time.Duration, maxMessages int) ([]*InboxEntry, string, error)
	// Ack permanently removes the given message IDs, but only when each one is
	// currently leased under leaseID. Error contract:
	//   - ErrMessageNotFound: at least one ID does not exist in the agent's
	//     inbox (never delivered, already acked, or expired and purged);
	//   - ErrLeaseConflict: every ID exists, but at least one is leased under
	//     a different (or no) lease.
	// A missing ID is reported before a lease mismatch, so a request whose IDs
	// are all unknown never masquerades as a stale-lease problem.
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

// ListErrorReporter is an optional Store capability for implementations
// whose List() cannot return an error but must not launder a failing
// backend into an indistinguishable empty registry (DF-CRIER-199).
// ListError reports the failure of the LAST List call: non-nil after a
// failed one, nil after a successful one. Stores that cannot fail
// (MemoryStore) simply do not implement it — List() then stays the whole
// contract.
type ListErrorReporter interface {
	ListError() error
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
	// fed forwards deliveries for agents unknown on this relay to linked
	// relays (CR-FEAT-006). Nil disables federation.
	fed *federation.Client
	// guard runs the LLM message-guard choke point on every delivery
	// (CR-FEAT-010, spec §2). Nil disables the guard (tests,
	// CR_GUARD_ENABLED=false).
	guard guard.Filter
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

// SetFederationClient enables relay-to-relay delivery routing (CR-FEAT-006):
// deliveries for agents unknown on this relay are forwarded to the client's
// linked relays. Nil disables federation (local 404 behavior unchanged).
func (h *Handler) SetFederationClient(c *federation.Client) {
	h.fed = c
}

// SetGuardFilter enables the LLM message guard (CR-FEAT-010): every
// delivery to a registered agent passes the choke point before the webhook
// driver or inbox store sees it. Nil disables the guard (spec §2.2 — kept
// for tests and for CR_GUARD_ENABLED=false).
//
// NOTE: spec §2.2 proposed a NewHandler parameter, but NewHandler is called
// from the Bane-pending internal/registry/remote_test.go (untouchable), so
// the setter keeps the public constructor stable — same pattern as
// SetWebhookDriver / SetFederationClient.
func (h *Handler) SetGuardFilter(f guard.Filter) {
	h.guard = f
}

// SetRequireAgentSig toggles per-agent ed25519 signature enforcement.
func (h *Handler) SetRequireAgentSig(enabled bool) {
	h.requireAgentSig = enabled
}

var (
	ErrAgentNotFound     = errors.New("agent not found")
	ErrAgentExists       = errors.New("agent already registered")
	ErrLeaseConflict     = errors.New("message is not leased under the supplied lease")
	ErrMessageNotFound   = errors.New("message not found")
	ErrInvalidStoreInput = errors.New("invalid store input")
)

// DefaultMessageTTL is the lifetime a stored message gets when the delivery
// does not request one (`ttl_seconds` absent). It is the 24h default the
// stores applied unconditionally before DF-CRIER-37 wired the documented
// field through; absent-field behavior is unchanged.
const DefaultMessageTTL = 24 * time.Hour

// resolveMessageExpiry fills entry.ExpiresAt in place from the delivery's
// requested TTL (DF-CRIER-37). Contract:
//
//   - an explicitly supplied ExpiresAt wins — store-internal callers pass an
//     absolute instant and are not second-guessed;
//   - TTLSeconds == nil  → CreatedAt + DefaultMessageTTL (the 24h default);
//   - *TTLSeconds == 0   → NEVER expires: ExpiresAt keeps the zero time, the
//     representation every consumption path (and the API contract) reads as
//     "no expiry";
//   - *TTLSeconds > 0    → CreatedAt + n seconds;
//   - *TTLSeconds < 0    → ErrInvalidStoreInput (the handler answers 400
//     before this is ever reached).
//
// Callers must set entry.CreatedAt first. Idempotent: re-resolving an entry
// whose ExpiresAt is already set leaves it untouched.
func resolveMessageExpiry(entry *InboxEntry) error {
	if entry == nil || !entry.ExpiresAt.IsZero() {
		return nil
	}
	if entry.TTLSeconds == nil {
		entry.ExpiresAt = entry.CreatedAt.Add(DefaultMessageTTL)
		return nil
	}
	ttl := *entry.TTLSeconds
	switch {
	case ttl < 0:
		return fmt.Errorf("%w: ttl_seconds must not be negative (%d)", ErrInvalidStoreInput, ttl)
	case ttl == 0:
		// Never expires — leave ExpiresAt at the zero time.
		return nil
	default:
		entry.ExpiresAt = entry.CreatedAt.Add(time.Duration(ttl) * time.Second)
		return nil
	}
}

// quoteMessageIDs renders message IDs for error details: quoted,
// comma-separated, in the caller's order, capped so a large bogus request
// cannot bloat the response body.
func quoteMessageIDs(ids []string) string {
	const maxListed = 5
	shown := ids
	if len(shown) > maxListed {
		shown = shown[:maxListed]
	}
	quoted := make([]string, 0, len(shown))
	for _, id := range shown {
		quoted = append(quoted, fmt.Sprintf("%q", id))
	}
	list := strings.Join(quoted, ", ")
	if len(ids) > maxListed {
		list = fmt.Sprintf("%s (+%d more)", list, len(ids)-maxListed)
	}
	return list
}

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
