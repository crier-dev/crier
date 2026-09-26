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
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/ratelimit"
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

// PurgeReporter is an optional Store capability for a purge that reports what
// it removed (CR-FEAT-025). PurgeExpired's `int` return is the whole contract
// for a store that cannot report — the count is all a caller can act on — but a
// silent count is exactly how a TTL expiry became a mystery: the message was
// removed and nothing downstream could learn its body, its id or its sender.
//
// Contract for implementers:
//
//   - the removal is completed (durably, for a persisting backend) BEFORE
//     report is called for it, so a caller that dead-letters on the report can
//     never resurrect a message that is still in the inbox;
//   - report is called EXACTLY ONCE per removed message, in any order;
//   - the report callback must not be invoked while the store's own write lock
//     is held — the caller's report path writes back to the store (a dead
//     letter and a receipt to the sender), and a store that calls it under its
//     own lock deadlocks against itself;
//   - a nil report is legal and means "count only" (the plain PurgeExpired
//     contract), so one implementation can serve both.
type PurgeReporter interface {
	PurgeExpiredReport(report func(agentID string, entry *InboxEntry)) int
}

// DeadLetterStore is an optional Store capability: the durable destination for
// messages an expiry sweep removed unacknowledged (CR-FEAT-025). A store that
// does not implement it answers 501 on the dead-letter read path and
// dead-letters nothing — the receipt to the sender still goes out, because a
// sender must not lose the notification merely because the backend keeps no
// archive.
type DeadLetterStore interface {
	// AppendDeadLetter records dl, and reports whether it was ADDED. A message
	// id already recorded is not added twice, and false is returned — that is
	// what keeps one expiry from producing two records (and, through the
	// caller's exactly-once rule, two receipts) if a store ever reports the
	// same removal twice.
	AppendDeadLetter(dl *DeadLetter) (bool, error)
	// ListDeadLetters returns up to limit dead letters for the given agent
	// (the inbox they expired in), newest first. limit <= 0 means the store's
	// documented default.
	ListDeadLetters(agentID string, limit int) ([]*DeadLetter, error)
}

// Transferrer is an optional Store capability: moving messages out of one
// inbox and into another's (CR-FEAT-025). This is the operator's answer to a
// STUCK lease — a holder that took a message and never acked it, where the
// alternative is waiting out the lease and then racing every other consumer for
// it.
//
// Contract:
//
//   - agentID is the CURRENT holder's inbox, targetAgentID the destination;
//     both must exist (ErrAgentNotFound otherwise) and must differ;
//   - messageIDs must be non-empty and must all exist in the source inbox
//     (ErrMessageNotFound otherwise) — a partial move is never performed;
//   - without force, a message is movable when it is UNLEASED or leased under
//     leaseID; one held under a DIFFERENT lease is ErrLeaseConflict and nothing
//     moves. An unleased message is claimable by any consumer anyway, so moving
//     it displaces nobody; a leased message is somebody's live work, and only
//     its own lease (or an explicit force) may move it. This keeps the lease a
//     real lock;
//   - with force, the lease is overridden: the messages move whatever their
//     lease state. This is the deliberate operator escape hatch for a holder
//     that is gone, and it is explicit in the API so it is never accidental;
//   - moved messages keep their id, payload, creation and expiry — only their
//     agent, lease and ack state change: they land in the destination UNLEASED
//     and claimable, which is the point of the transfer;
//   - the number moved is returned, and equals len(messageIDs) on success.
type Transferrer interface {
	Transfer(agentID, leaseID string, messageIDs []string, targetAgentID string, force bool) (int, error)
}

// Handler keeps HTTP concerns separate from storage implementations.
type Handler struct {
	store Store
	// requireAgentSig enforces per-agent ed25519 request signing on the
	// agent-owned routes (retrieve/ack/stats/unregister). When false, only
	// the shared Bearer token is required (legacy behavior).
	requireAgentSig bool
	// maxInboxBodyBytes bounds the raw body on both inbox delivery routes.
	// It is set during server wiring and defaults for direct test handlers.
	maxInboxBodyBytes int64
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
	// notifier wakes reads parked by a long-poll retrieve (?wait_seconds,
	// CR-FEAT-023) when a delivery lands in an inbox. Created by NewHandler;
	// every method tolerates a nil receiver, so a zero-value Handler degrades
	// to fallback polling instead of panicking.
	notifier *inboxNotifier
	// inboxPing is the optional new-message ping (CR-FEAT-023) fired after a
	// delivery is stored, for agents that cannot hold a long-poll open. Nil
	// disables pings entirely (tests, and any deployment that wires none).
	inboxPing InboxPinger
	// presence derives the status a row REPORTS from its liveness evidence —
	// last_seen plus the documented staleness window (CR-FEAT-024, presence.go).
	// Read-time only: no store write, no sweeper. The zero value means the
	// documented default window, so a Handler built without SetPresence still
	// reports a crashed agent as stale instead of online forever.
	presence Presence
	// detector is the optional detection layer (CR-FEAT-030, detection.go):
	// every delivery outcome is observed once, and a contained agent is
	// refused before anything is delivered. Nil (the default) means the
	// delivery path is byte-identical to a server without detection.
	detector Detector
	// idempotency is the sender-supplied-key replay window (CR-FEAT-025). It is
	// created by NewHandler; a Handler that was never built by it (a zero-value
	// one in a test) has no window and therefore deduplicates nothing — the
	// pre-CR-FEAT-025 behaviour — instead of panicking on the deliver path.
	idempotency *idempotencyRegistry
	// capMu guards capCursors.
	capMu sync.Mutex
	// capCursors is the per-capability round-robin cursor of
	// capability-routed delivery (CR-FEAT-026, capability.go): one counter per
	// capability, advanced once per dispatched delivery to that capability and
	// resolved modulo the pool size. It is created on first use rather than by
	// NewHandler so a zero-value Handler still rotates instead of panicking.
	// Per process, in memory, deliberately not persisted: this is fairness
	// across holders, not exactly-once dispatch.
	capCursors map[string]uint64
	// globalRateLimit and globalLimiter are the global inbox-ingest budget
	// (CR-FEAT-035, see backpressure.go): a shed on the delivery path itself,
	// so a runaway producer cannot push every inbox deeper without limit. Zero
	// / nil — the default — means the budget does not exist and nothing is ever
	// shed.
	globalRateLimit int
	globalLimiter   *ratelimit.Window
	// namespaces is the realm policy set (CR-FEAT-029, namespace.go). Nil (the
	// default) means one implicit namespace: every target resolves to the
	// default realm, retention and guard settings resolve exactly as they did
	// before the feature existed, and no response body grows a namespace
	// member. All methods on *namespace.Registry are nil-safe for exactly this
	// reason — the zero Handler is the unconfigured deployment.
	namespaces *namespace.Registry
}

// NewHandler creates a Handler that delegates store operations to the
// provided Store implementation. The handler is safe for concurrent callers
// if the underlying store is.
func NewHandler(store Store) *Handler {
	return &Handler{
		store:    store,
		notifier: newInboxNotifier(),
		// The raw request body cap is shared by by-id and capability delivery.
		maxInboxBodyBytes: defaultMaxInboxBodyBytes,
		// The documented default window: a Handler that is never configured
		// derives status the same way a configured one does, just with the
		// shipped window (CR-FEAT-024).
		presence: NewPresence(DefaultStalenessWindow),
		// The documented deduplication window a sender's idempotency key is
		// honored within (CR-FEAT-025).
		idempotency: newIdempotencyRegistry(DefaultIdempotencyWindow),
	}
}

// SetMaxInboxBodyBytes configures the raw HTTP body limit shared by
// POST /agents/{id}/inbox and POST /capabilities/{capability}/inbox. A
// non-positive value restores the documented 1 MiB default.
func (h *Handler) SetMaxInboxBodyBytes(limit int) {
	if limit <= 0 {
		h.maxInboxBodyBytes = defaultMaxInboxBodyBytes
		return
	}
	h.maxInboxBodyBytes = int64(limit)
}

// SetIdempotencyWindow configures the window within which a sender-supplied
// idempotency key deduplicates a delivery (CR-FEAT-025). A non-positive window
// restores the documented default — never "deduplicate nothing", which a window
// of 0 would otherwise mean and which would make the feature silently absent.
func (h *Handler) SetIdempotencyWindow(window time.Duration) {
	if h.idempotency == nil {
		h.idempotency = newIdempotencyRegistry(window)
		return
	}
	h.idempotency.SetWindow(window)
}

// SetPresence configures the rule that derives the status a row REPORTS from
// its liveness evidence (CR-FEAT-024). A zero or negative window means the
// documented default — never "every row is stale", which a window of 0 would
// otherwise mean and which would make the signal useless exactly when an
// operator is trying to read it.
func (h *Handler) SetPresence(p Presence) {
	h.presence = p
}

// SetInboxPinger wires the new-message ping fired after a delivery lands in an
// agent's inbox (CR-FEAT-023). Nil disables it — the poll-only and long-poll
// lanes stay the whole contract, which is what every pre-CR-FEAT-023 caller
// and test gets.
func (h *Handler) SetInboxPinger(p InboxPinger) {
	h.inboxPing = p
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
	// deadLetters is the process-lifetime dead-letter destination
	// (CR-FEAT-025), guarded by mu.
	deadLetters *memoryDeadLetters
}

// NewMemoryStore returns an in-memory agent registry with process-lifetime
// inbox persistence (ephemeral across restarts).
// Uses crypto/rand for lease IDs and sync.RWMutex for thread safety.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		agents:      make(map[string]*Agent),
		inboxes:     make(map[string][]*InboxEntry),
		deadLetters: newMemoryDeadLetters(DefaultDeadLetterCapacity),
	}
}

var _ Store = (*MemoryStore)(nil)

// Compile-time capability assertion: MemoryStore records liveness evidence
// (the mesh heartbeat path, presence.go, CR-FEAT-024), so the shipped default
// backend — the one a local dev server runs — keeps its registry rows honest
// too, not just the PostgreSQL backend.
var _ Toucher = (*MemoryStore)(nil)

// newLeaseID generates a 16-byte crypto-random hex string for lease identification.
func newLeaseID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
