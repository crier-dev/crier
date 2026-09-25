package registry

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/guard"
)

// Sender-supplied idempotency keys (CR-FEAT-025).
//
// The lease is the lock: a retrieve claims a batch and an ack is the only thing
// that removes it, so a worker that dies mid-task gets its message back after
// the lease lapses. What the lease does NOT cover is the SENDER's side of a
// retry. A sender that delivers and then loses the connection before it reads
// the 201 has no way to ask "did my message land?" — it re-delivers and the
// target now has two copies of the same work.
//
// An optional `idempotency_key` closes that gap: a delivery carrying one is
// deduplicated against the key's own recent history, and a duplicate answers
// with the ORIGINAL accept — same message id, same status, same transport —
// instead of storing a second message. The target's inbox therefore contains
// exactly one message per (agent, key), and the sender learns which id it was.
//
// Scope, stated rather than implied:
//
//   - per (target agent, key) pair: the same key aimed at two different agents
//     is two deliveries, because they are two pieces of work;
//   - within DefaultIdempotencyWindow (24h), configurable per relay with
//     Handler.SetIdempotencyWindow;
//   - process-scoped and in-memory, like the long-poll notifier: this is a
//     retry-deduplication window, not a durable ledger. A relay restart closes
//     the window early (the sender's next attempt is a fresh delivery, which
//     is safe: the message it would duplicate is still in the target's inbox
//     and still retrievable). The key is also recorded ON the stored message
//     (InboxEntry.IdempotencyKey) and on its dead letter, so the provenance
//     outlives the window in every durable backend;
//   - a REJECTED delivery (400/403/404/502 …) records nothing: only an accept
//     is replayed. A sender that mistyped a payload and retries under the same
//     key gets its delivery attempted again rather than a replay of the
//     rejection;
//   - over a federation hop the key travels WITH the forwarded request, so the
//     relay that owns the target agent applies its own window there. A delivery
//     held at the source for a bounded retry is not recorded here: it has no
//     accept yet, and the hold machinery is already its own exactly-once path.

// DefaultIdempotencyWindow is the documented deduplication window: a delivery
// whose key was accepted within it is answered with the original accept, and
// nothing new is stored. After it, the same key delivers normally — the
// message it duplicated has either been acked (nothing to duplicate) or is
// still queued (and the sender is deliberately given a fresh delivery rather
// than a replay of a receipt it may no longer be able to act on).
const DefaultIdempotencyWindow = 24 * time.Hour

// maxIdempotencyKeyLen bounds a key so an unbounded request cannot grow the
// replay registry without limit. A longer key is rejected with 400 rather than
// truncated: silently shortening it would merge two distinct keys into one.
const maxIdempotencyKeyLen = 128

// idempotencyClaimWait bounds how long a delivery waits for a CONCURRENT
// delivery under the same key to finish before it answers 409. Two simultaneous
// duplicates normally serialize in milliseconds; the bound exists so a wedged
// claimant cannot park a request forever.
const idempotencyClaimWait = 2 * time.Second

// idempotentReceipt is the recorded outcome of the delivery a key first
// produced: everything a replay needs to answer with the same accept body
// without consulting a transport or the store again.
type idempotentReceipt struct {
	Status       int
	ID           string
	Transport    string
	DeliveryMode string
	ExpiresAt    *MessageExpiry
	Guard        *guard.Meta
	// Blocking marks a request/response accept: its Reply is part of the
	// recorded outcome, because answering a replayed blocking delivery with an
	// empty reply would be worse than not deduplicating it at all.
	Blocking   bool
	Reply      json.RawMessage
	RecordedAt time.Time
}

// replayResponse renders the original accept with the replay marker set, so a
// sender can tell a replayed accept from a fresh one while both carry the same
// message id.
func (r idempotentReceipt) replayResponse() deliverResponse {
	return deliverResponse{
		ID:               r.ID,
		Transport:        r.Transport,
		DeliveryMode:     r.DeliveryMode,
		ExpiresAt:        r.ExpiresAt,
		Guard:            r.Guard,
		IdempotentReplay: true,
	}
}

// idempotencyOutcome is the decision Acquire returns.
type idempotencyOutcome int

const (
	// idempotencyProceed: the caller owns the key and must Finish the attempt.
	idempotencyProceed idempotencyOutcome = iota
	// idempotencyReplay: the key was already accepted — answer with the
	// recorded receipt and deliver nothing.
	idempotencyReplay
	// idempotencyBusy: another delivery under this key is still in flight and
	// did not finish inside idempotencyClaimWait. Nothing was stored by this
	// caller; 409 is the honest answer.
	idempotencyBusy
)

// idempotencyRegistry is the handler-owned replay window. It is safe for
// concurrent use.
type idempotencyRegistry struct {
	mu     sync.Mutex
	window time.Duration
	// records holds the accept produced by a key, keyed by agent+key.
	records map[string]*idempotentReceipt
	// claims holds the keys with a delivery in flight.
	claims map[string]*idempotencyClaim
}

// idempotencyClaim is one in-flight delivery under a key. Its done channel is
// closed when the delivery finishes (recording a receipt or abandoning), which
// is how a concurrent duplicate is woken to re-check the registry.
type idempotencyClaim struct {
	at   time.Time
	done chan struct{}
}

// newIdempotencyRegistry returns a replay window. A non-positive window means
// the documented default — never "deduplicate nothing", which a window of 0
// would otherwise mean and which would make the feature silently absent.
func newIdempotencyRegistry(window time.Duration) *idempotencyRegistry {
	if window <= 0 {
		window = DefaultIdempotencyWindow
	}
	return &idempotencyRegistry{
		window:  window,
		records: make(map[string]*idempotentReceipt),
		claims:  make(map[string]*idempotencyClaim),
	}
}

// Window is the deduplication window this registry applies.
func (r *idempotencyRegistry) Window() time.Duration {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.window
}

// SetWindow changes the deduplication window. A non-positive value restores the
// documented default. Records already older than the new window stop replaying
// on their next lookup.
func (r *idempotencyRegistry) SetWindow(window time.Duration) {
	if window <= 0 {
		window = DefaultIdempotencyWindow
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.window = window
}

// idempotencyRecordKey is the registry key of one (agent, key) pair. The NUL
// separator keeps a key from being able to impersonate another pair by
// embedding the separator in either half.
func idempotencyRecordKey(agentID, key string) string {
	return agentID + "\x00" + key
}

// recordLocked returns the receipt recorded for k, if it is still inside the
// window. An expired record is dropped here, so the window is enforced on
// read as well as on write. Callers must hold r.mu.
func (r *idempotencyRegistry) recordLocked(k string, now time.Time) (idempotentReceipt, bool) {
	rec, ok := r.records[k]
	if !ok {
		return idempotentReceipt{}, false
	}
	if now.Sub(rec.RecordedAt) >= r.window {
		delete(r.records, k)
		return idempotentReceipt{}, false
	}
	return *rec, true
}

// pruneLocked drops records and wedged claims older than the window. Callers
// must hold r.mu.
func (r *idempotencyRegistry) pruneLocked(now time.Time) {
	for k, rec := range r.records {
		if now.Sub(rec.RecordedAt) >= r.window {
			delete(r.records, k)
		}
	}
	for k, claim := range r.claims {
		if now.Sub(claim.at) >= r.window {
			delete(r.claims, k)
		}
	}
}

// Acquire resolves a delivery carrying an idempotency key. On
// idempotencyProceed the caller owns the key until it calls Finish on the
// returned attempt (recording the accept it produced) — or Abandons it, which
// releases the key without recording anything so the sender's next attempt is
// delivered rather than replayed.
func (r *idempotencyRegistry) Acquire(agentID, key string, now time.Time) (idempotentReceipt, idempotencyOutcome, *idempotencyAttempt) {
	k := idempotencyRecordKey(agentID, key)
	deadline := now.Add(idempotencyClaimWait)
	for {
		r.mu.Lock()
		if rec, ok := r.recordLocked(k, now); ok {
			r.mu.Unlock()
			return rec, idempotencyReplay, nil
		}
		if claim, ok := r.claims[k]; ok {
			r.mu.Unlock()
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return idempotentReceipt{}, idempotencyBusy, nil
			}
			timer := time.NewTimer(remaining)
			select {
			case <-claim.done:
				timer.Stop()
				// The holder finished: re-check under the lock (it either
				// recorded a receipt or released the key for a new owner).
				continue
			case <-timer.C:
				return idempotentReceipt{}, idempotencyBusy, nil
			}
		}
		claim := &idempotencyClaim{at: now, done: make(chan struct{})}
		r.claims[k] = claim
		r.pruneLocked(now)
		r.mu.Unlock()

		return idempotentReceipt{}, idempotencyProceed, &idempotencyAttempt{
			finish: func(rec idempotentReceipt) {
				r.mu.Lock()
				if rec.ID != "" {
					rec.RecordedAt = time.Now()
					r.records[k] = &rec
				}
				delete(r.claims, k)
				r.mu.Unlock()
				close(claim.done)
			},
		}
	}
}

// idempotencyAttempt is the caller's handle on a claimed key. Exactly one of
// Finish/Abandon runs: the first call wins, so a handler can `defer
// attempt.Abandon()` and call Finish on the accept path without double
// releasing the claim.
type idempotencyAttempt struct {
	once   sync.Once
	finish func(idempotentReceipt)
}

// Finish records the accept this delivery produced and releases the key.
func (a *idempotencyAttempt) Finish(rec idempotentReceipt) {
	if a == nil {
		return
	}
	a.once.Do(func() { a.finish(rec) })
}

// Abandon releases the key without recording anything.
func (a *idempotencyAttempt) Abandon() {
	a.Finish(idempotentReceipt{})
}

// validateIdempotencyKey enforces the documented bound on a sender-supplied
// key. The empty key is not a key at all (the caller does not deduplicate) and
// is handled by the caller.
func validateIdempotencyKey(key string) error {
	if len(key) > maxIdempotencyKeyLen {
		return fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLen)
	}
	return nil
}
