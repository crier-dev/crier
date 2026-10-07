// wait.go — deliver-and-wait for DAG nodes (CR-CHAT-034).
//
// A DAG node on the executor side must be able to hand work to an agent and
// STOP until that agent acks or replies, because the next node needs the
// result. Crier does not execute the DAG (decision D18) — but it OWNS the
// inbox the work arrives in, so it is the one component that can honestly
// answer "has the agent finished what node N handed it?".
//
// The shape, stated rather than implied:
//
//   - POST /dagger/wait delivers a payload to an agent through the SHIPPED
//     inbox path (the same registry.Store.Deliver every delivery uses —
//     deliver.go's rule, applied to a wait) and BLOCKS, bounded, until the
//     agent acks the delivered message or posts a reply. The wait resolves
//     with the reply or the ack reference; a bounded budget that expires is a
//     NAMED DELIVERY_WAIT_TIMEOUT, never a silent 200.
//   - The idempotency key is recorded BEFORE the first delivery, in a JSONL
//     journal beside the run records. A retried call with the same key —
//     including one made after a crash and restart, recovered by replaying
//     the journal — returns the SAME pending (or resolved) wait without
//     re-delivering, so a resumed run never asks the agent to do the same
//     work twice.
//   - Cancelling the run releases its waiters with a named WAIT_CANCELLED: an
//     agent must not be left working on a node nobody is awaiting.
//
// The waiter registry is in-process (like the long-poll notifier) but its
// DECISIONS are durable: the journal is what survives a restart, not the
// channels. A recovered pending wait can no longer be woken by the original
// caller's goroutine — it then answers the named timeout, which is the honest
// reading of "the waiter died waiting".

package daggerctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// Named wait outcomes. They are inbox- and error-code vocabulary, in the same
// shape DAGGER_RUN_* already uses — a timeout is a fact an operator can grep
// for, never an empty success.
const (
	// CodeDeliveryWaitTimeout is the named outcome when the bounded budget
	// expired with neither an ack nor a reply (HTTP 504).
	CodeDeliveryWaitTimeout = "DELIVERY_WAIT_TIMEOUT"
	// CodeWaitCancelled is the named outcome when the run the wait belongs to
	// was cancelled and its waiters were released (HTTP 409).
	CodeWaitCancelled = "WAIT_CANCELLED"
)

var (
	// ErrWaitTimeout is the bounded budget expiring with no resolution.
	ErrWaitTimeout = errors.New("dagger delivery wait timed out")
	// ErrWaitCancelled is the run the wait belongs to being cancelled.
	ErrWaitCancelled = errors.New("dagger wait cancelled")
	// ErrWaitNotFound is a wait key the registry does not hold.
	ErrWaitNotFound = errors.New("dagger wait not found")
)

// Delivery-wait bounds. A wait parks an HTTP request, so it is bounded on
// BOTH ends: no budget at all and a budget past the ceiling are refused with
// a named error — the same ceiling the inbox long-poll applies
// (registry.MaxWaitSeconds), so the two request-level waits of this API
// cannot disagree about how long a caller may hold a request open.
const (
	MinDeliveryWaitSeconds = 1
	MaxDeliveryWaitSeconds = 120
	// MaxWaitKeyBytes bounds an idempotency key, the same bound the registry
	// deliver path applies to a sender-supplied key.
	MaxWaitKeyBytes = 128
)

// WaitRequest is a POST /dagger/wait body.
type WaitRequest struct {
	// AgentID is the target agent: the inbox the payload is delivered to.
	AgentID string `json:"agent_id"`
	// Payload is the work the DAG node hands the agent, delivered verbatim.
	Payload json.RawMessage `json:"payload"`
	// IdempotencyKey is the wait's identity. The FIRST call under a key
	// delivers; every later call with the same key — including one recovered
	// from the journal after a restart — joins the same wait without a second
	// delivery.
	IdempotencyKey string `json:"idempotency_key"`
	// TimeoutSeconds bounds the block, 1..MaxDeliveryWaitSeconds. Absent,
	// zero, negative or past the ceiling is refused with a named error:
	// unbounded waits are never accepted.
	TimeoutSeconds int `json:"timeout_seconds"`
	// RunID optionally names the DAG run this wait belongs to. Cancelling
	// that run releases the wait with WAIT_CANCELLED.
	RunID string `json:"run_id,omitempty"`
}

// WaitResult is the resolved state a /dagger/wait call answers with.
type WaitResult struct {
	// State is "resolved" (the agent acked or replied) or "cancelled" (the
	// run was cancelled and the wait released). A timeout is NOT a result —
	// it is the named DELIVERY_WAIT_TIMEOUT error.
	State string `json:"state"`
	// MessageID is the delivered message the resolution names: the entry the
	// agent acked (an ack resolution) or the reply was posted against.
	MessageID string `json:"message_id,omitempty"`
	// Reply is the payload a replying agent posted to the wait's resolve
	// endpoint, verbatim. Absent on an ack resolution.
	Reply json.RawMessage `json:"reply,omitempty"`
}

// waitState is a wait's lifecycle state as the journal records it.
type waitState string

const (
	waitPending   waitState = "pending"
	waitResolved  waitState = "resolved"
	waitCancelled waitState = "cancelled"
	waitAbandoned waitState = "abandoned"
)

// waitEntry is one wait: its identity (key), the run it belongs to, the
// message the delivery produced, and its state. ready is closed on every
// terminal transition, so any number of joined callers wake together.
type waitEntry struct {
	Key       string          `json:"key"`
	RunID     string          `json:"run_id,omitempty"`
	AgentID   string          `json:"agent_id,omitempty"`
	State     waitState       `json:"state"`
	MessageID string          `json:"message_id,omitempty"`
	Reply     json.RawMessage `json:"reply,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`

	ready chan struct{} // closed on transition; never persisted
}

// snapshot returns a copy of the entry with its channel stripped, safe to
// hand across the API boundary.
func (e *waitEntry) snapshot() *waitEntry {
	cp := *e
	cp.ready = nil
	return &cp
}

// journalLine is the persisted shape of one wait-version line in waits.jsonl.
type journalLine struct {
	waitEntry
	// Reply marshals as raw JSON inline; nothing extra is needed.
}

// ---------------------------------------------------------------------------
// The waiter registry
// ---------------------------------------------------------------------------

// WaiterRegistry holds the in-process waiters and the JSONL journal their
// decisions are recovered from. It is safe for concurrent callers.
//
// The map is process-scoped: after a restart the CHANNELS are gone, but the
// journal replay re-creates every entry with its last recorded state — a
// pending entry stays pending (and still refuses a second delivery), a
// resolved entry resolves any joined caller immediately with its recorded
// reply.
type WaiterRegistry struct {
	mu      sync.Mutex
	entries map[string]*waitEntry
	byMsg   map[string][]string // delivered message id -> wait keys
	byRun   map[string][]string // run id -> wait keys
	journal *waitJournal
	now     func() time.Time
}

// NewWaiterRegistry builds a waiter registry. A non-empty dir persists every
// decision to <dir>/waits.jsonl and replays it on open (keep-LAST per key);
// an empty dir is memory-only, which is what tests and ephemeral deployments
// use. A journal that cannot be written fails HERE, at open, rather than
// silently dropping the idempotency guarantee at the first restart.
func NewWaiterRegistry(dir string) (*WaiterRegistry, error) {
	r := &WaiterRegistry{
		entries: make(map[string]*waitEntry),
		byMsg:   make(map[string][]string),
		byRun:   make(map[string][]string),
		now:     time.Now,
	}
	if strings.TrimSpace(dir) != "" {
		j, err := newWaitJournal(dir)
		if err != nil {
			return nil, err
		}
		r.journal = j
		if err := r.replay(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register records the key BEFORE the first delivery and returns the wait it
// names. created=false means the key already existed — the caller must NOT
// deliver again; it joins the returned entry's wait. The returned entry IS
// the registry's live one (its `ready` channel is the wake edge); use
// snapshot() when handing an entry across the API boundary.
func (r *WaiterRegistry) Register(key, runID, agentID string) (entry *waitEntry, created bool, err error) {
	if strings.TrimSpace(key) == "" {
		return nil, false, fmt.Errorf("%w: idempotency key is required", ErrInvalidInput)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[key]; ok {
		// An abandoned key was a DELIVERY that failed, not a wait: the key is
		// free to be used again. Every other recorded state joins.
		if e.State != waitAbandoned {
			return e, false, nil
		}
	}
	e := &waitEntry{
		Key:     key,
		RunID:   runID,
		AgentID: agentID,
		State:   waitPending,
		ready:   make(chan struct{}),
	}
	if r.journal != nil {
		if err := r.journal.append(e); err != nil {
			return nil, false, err
		}
	}
	r.entries[key] = e
	if runID != "" {
		r.byRun[runID] = append(r.byRun[runID], key)
	}
	return e, true, nil
}

// BindMessage records the inbox message id a successful delivery produced, so
// a later ack of that message can resolve the wait by id. Best effort by
// construction: a store that does not echo the id back simply leaves the wait
// resolvable only by key.
func (r *WaiterRegistry) BindMessage(key, messageID string) {
	if messageID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byMsg[messageID] = append(r.byMsg[messageID], key)
}

// Resolve marks the wait resolved: the agent acked the delivered message or
// posted a reply. It returns the resolved entry and whether this call was the
// transition (a second resolve of an already-resolved wait is a no-op that
// still returns the entry, so a duplicate resolve cannot unwind a caller).
func (r *WaiterRegistry) Resolve(key, messageID string, reply json.RawMessage) (*waitEntry, bool, error) {
	return r.transition(key, waitResolved, messageID, reply)
}

// ResolveByMessage resolves every wait bound to a delivered message id — the
// path the inbox ack hook drives. Unknown ids resolve nothing and are not an
// error: most acks ack messages no wait is parked on.
func (r *WaiterRegistry) ResolveByMessage(messageID string, reply json.RawMessage) []*waitEntry {
	r.mu.Lock()
	keys := append([]string(nil), r.byMsg[messageID]...)
	r.mu.Unlock()
	var out []*waitEntry
	for _, key := range keys {
		if e, _, err := r.Resolve(key, messageID, reply); err == nil && e != nil {
			out = append(out, e)
		}
	}
	return out
}

// ReleaseRun cancels every pending wait belonging to a run: the run is over,
// so nobody is awaiting the agents' results any more, and leaving them parked
// would strand the work. It returns the entries it transitioned; waits that
// already resolved keep their resolution.
func (r *WaiterRegistry) ReleaseRun(runID string) []*waitEntry {
	r.mu.Lock()
	keys := append([]string(nil), r.byRun[runID]...)
	r.mu.Unlock()
	var out []*waitEntry
	for _, key := range keys {
		if e, changed, err := r.transition(key, waitCancelled, "", nil); err == nil && changed && e != nil {
			out = append(out, e)
		}
	}
	return out
}

// Lookup returns the current state of a key, or ErrWaitNotFound.
func (r *WaiterRegistry) Lookup(key string) (*waitEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrWaitNotFound, key)
	}
	return e.snapshot(), nil
}

// transition is the shared shape of resolve and cancel: a pending entry moves
// to the target state, the journal records it, and every joined caller is
// woken. Any other prior state is left alone (a cancelled wait never becomes
// resolved; a resolved one never becomes cancelled).
func (r *WaiterRegistry) transition(key string, to waitState, messageID string, reply json.RawMessage) (*waitEntry, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok || e.State != waitPending {
		if !ok {
			return nil, false, fmt.Errorf("%w: %q", ErrWaitNotFound, key)
		}
		return e.snapshot(), false, nil
	}
	e.State = to
	if messageID != "" {
		e.MessageID = messageID
	}
	if len(reply) > 0 {
		e.Reply = reply
	}
	e.UpdatedAt = r.now()
	if r.journal != nil {
		if err := r.journal.append(e); err != nil {
			return nil, false, err
		}
	}
	close(e.ready)
	return e.snapshot(), true, nil
}

// Abandon withdraws a wait whose DELIVERY failed: the key never delivered, so
// it must not block a retry forever. The journal records the abandonment so a
// replay frees the key too.
func (r *WaiterRegistry) Abandon(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok || e.State != waitPending {
		return
	}
	e.State = waitAbandoned
	e.UpdatedAt = r.now()
	if r.journal != nil {
		_ = r.journal.append(e) // best effort: an unjournalled abandonment only costs a restart a stale pending entry
	}
	close(e.ready)
}

// replay rebuilds the registry from the journal, keep-LAST per key. A pending
// entry recovered here keeps refusing a second delivery — that is the whole
// point — but its channel can never be closed by the dead waiter, so joined
// callers end on the named timeout.
func (r *WaiterRegistry) replay() error {
	lines, err := r.journal.readAll()
	if err != nil {
		return err
	}
	for _, e := range lines {
		prev, existed := r.entries[e.Key]
		if existed && prev.State == waitAbandoned && e.State == waitAbandoned {
			continue
		}
		cp := *e
		cp.ready = make(chan struct{})
		if e.State != waitPending {
			close(cp.ready)
		}
		r.entries[e.Key] = &cp
		if e.RunID != "" {
			r.byRun[e.RunID] = append(r.byRun[e.RunID], e.Key)
		}
		if e.MessageID != "" {
			r.byMsg[e.MessageID] = append(r.byMsg[e.MessageID], e.Key)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// The journal: one append-only JSONL file of wait versions
// ---------------------------------------------------------------------------

// waitJournal is the durable half of the registry, in the shape of the run
// store (store.go): one append-only file, one line per STATE VERSION,
// fsynced per append, reduced keep-LAST per key on read.
type waitJournal struct {
	path string
	mu   sync.Mutex
}

const waitJournalFileName = "waits.jsonl"

func newWaitJournal(dir string) (*waitJournal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("daggerctl wait journal: create directory: %w", err)
	}
	return &waitJournal{path: filepath.Join(dir, waitJournalFileName)}, nil
}

func (j *waitJournal) append(e *waitEntry) error {
	line, err := json.Marshal(journalLine{waitEntry: *e})
	if err != nil {
		return fmt.Errorf("daggerctl wait journal: encode: %w", err)
	}
	line = append(line, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("daggerctl wait journal: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("daggerctl wait journal: append: %w", err)
	}
	return f.Sync()
}

func (j *waitJournal) readAll() ([]*waitEntry, error) {
	f, err := os.Open(j.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("daggerctl wait journal: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRecordLineBytes)
	var out []*waitEntry
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec waitEntry
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("daggerctl wait journal: %s line %d: %w", j.path, lineNo, err)
		}
		out = append(out, &rec)
	}
	return out, sc.Err()
}

// ---------------------------------------------------------------------------
// The Service verbs
// ---------------------------------------------------------------------------

// SetWaitRegistry installs the waiter registry. Nil (the default) leaves the
// wait verbs answering the named 503 a missing bridge does.
func (s *Service) SetWaitRegistry(r *WaiterRegistry) { s.waits = r }

// SetInboxStore wires the durable-inbox write a wait delivers through — the
// SAME store the notification path uses, never a second delivery path.
func (s *Service) SetInboxStore(store InboxStore) { s.inbox = store }

// waitSender is the Sender recorded on a wait's inbox entry. It is not an
// agent id: it names the COMPONENT that delivered the work, so a reader can
// tell a node's handoff from an agent-to-agent message.
const waitSender = "daggerctl"

// validateWaitRequest applies the boundary rules before anything is recorded:
// a key, a payload and a BOUNDED budget are all required, and an unbounded or
// absurd budget is a named refusal — never clamped into a silent shorter one.
func validateWaitRequest(req WaitRequest) error {
	if err := validateAgent(req.AgentID); err != nil {
		return err
	}
	if len(req.Payload) == 0 {
		return fmt.Errorf("%w: payload is required", ErrInvalidInput)
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return fmt.Errorf("%w: idempotency_key is required", ErrInvalidInput)
	}
	if len(req.IdempotencyKey) > MaxWaitKeyBytes {
		return fmt.Errorf("%w: idempotency_key exceeds %d bytes", ErrInvalidInput, MaxWaitKeyBytes)
	}
	if req.TimeoutSeconds < MinDeliveryWaitSeconds || req.TimeoutSeconds > MaxDeliveryWaitSeconds {
		return fmt.Errorf("%w: timeout_seconds must be between %d and %d (an unbounded wait is refused, never clamped)",
			ErrInvalidInput, MinDeliveryWaitSeconds, MaxDeliveryWaitSeconds)
	}
	return nil
}

// DeliverAndWait delivers payload to the agent's durable inbox and blocks,
// bounded, until the agent acks the delivered message or posts a reply.
//
// Idempotency: the key is recorded BEFORE the first delivery; a call with a
// key that already names a wait joins it without a second delivery — the
// property that makes a retried or resumed node safe. A delivery that fails
// abandons the freshly-recorded key so the retry can actually retry.
//
// Outcomes: the resolution (WaitResult, state resolved), the run's
// cancellation (named WAIT_CANCELLED), the budget expiring (named
// DELIVERY_WAIT_TIMEOUT), or the caller's own context ending — never a
// silent empty success.
func (s *Service) DeliverAndWait(ctx context.Context, req WaitRequest) (*WaitResult, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	if s.waits == nil {
		return nil, fmt.Errorf("%w: no waiter registry is wired", ErrUnconfigured)
	}
	if s.inbox == nil {
		return nil, fmt.Errorf("%w: no inbox store is wired for waits", ErrUnconfigured)
	}
	if err := validateWaitRequest(req); err != nil {
		return nil, err
	}

	// Register BEFORE the deliver: the key must exist before the work can,
	// or a crash between deliver and record would let a retry deliver twice.
	entry, created, err := s.waits.Register(req.IdempotencyKey, req.RunID, req.AgentID)
	if err != nil {
		return nil, err
	}
	if created {
		inboxEntry := &registry.InboxEntry{
			Payload:        []byte(req.Payload),
			IdempotencyKey: req.IdempotencyKey,
			Sender:         waitSender,
		}
		if derr := s.inbox.Deliver(req.AgentID, inboxEntry); derr != nil {
			s.waits.Abandon(req.IdempotencyKey)
			return nil, fmt.Errorf("%w: deliver to agent %q: %v", ErrInvalidInput, req.AgentID, derr)
		}
		// The store stamped the message id on the entry it accepted; bind it
		// so a later ack resolves this wait by message id.
		s.waits.BindMessage(req.IdempotencyKey, inboxEntry.ID)
	}

	timer := time.NewTimer(time.Duration(req.TimeoutSeconds) * time.Second)
	defer timer.Stop()
	select {
	case <-entry.ready:
		e, lerr := s.waits.Lookup(req.IdempotencyKey)
		if lerr != nil {
			return nil, lerr
		}
		switch e.State {
		case waitResolved:
			return &WaitResult{State: string(waitResolved), MessageID: e.MessageID, Reply: e.Reply}, nil
		case waitCancelled:
			return nil, fmt.Errorf("%w (%s): run %q was cancelled while waiting for key %q",
				ErrWaitCancelled, CodeWaitCancelled, req.RunID, req.IdempotencyKey)
		default:
			// abandoned (a racing delivery failure) reads as its own refusal.
			return nil, fmt.Errorf("%w: delivery for key %q did not land", ErrInvalidInput, req.IdempotencyKey)
		}
	case <-timer.C:
		return nil, fmt.Errorf("%w (%s): no ack or reply for key %q within %ds",
			ErrWaitTimeout, CodeDeliveryWaitTimeout, req.IdempotencyKey, req.TimeoutSeconds)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ResolveWait records a reply on a wait by key: the endpoint a REPLYING agent
// posts to. An already-resolved wait returns its recorded state unchanged
// (a duplicate resolve is a no-op, never an unwinding); an unknown key is a
// named 404.
func (s *Service) ResolveWait(key, messageID string, reply json.RawMessage) (*WaitResult, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("%w: wait key is required", ErrInvalidInput)
	}
	e, changed, err := s.waits.Resolve(key, messageID, reply)
	if err != nil {
		return nil, err
	}
	_ = changed // a no-op resolve still returns the recorded resolution
	return &WaitResult{State: string(waitResolved), MessageID: e.MessageID, Reply: e.Reply}, nil
}

// WaitStatus reports a wait's current state without joining it.
func (s *Service) WaitStatus(key string) (*WaitResult, error) {
	e, err := s.waits.Lookup(key)
	if err != nil {
		return nil, err
	}
	return &WaitResult{State: string(e.State), MessageID: e.MessageID, Reply: e.Reply}, nil
}

// NotifyAcked is the hook the inbox ack path calls: the agent has acked
// delivered messages, and every wait bound to one of them resolves. It never
// fails and never blocks — an ack that resolves no wait is the common case.
func (s *Service) NotifyAcked(agentID string, messageIDs []string) {
	if s == nil || s.waits == nil {
		return
	}
	for _, id := range messageIDs {
		for _, e := range s.waits.ResolveByMessage(id, nil) {
			slog.Debug("dagger wait resolved by ack", "key", e.Key, "message_id", id, "agent_id", agentID)
		}
	}
}

// releaseCancelledRunWaits frees every wait belonging to a run that just
// reached the cancelled state (commit drives it, so BOTH the explicit cancel
// verb and a status poll that observes the cancellation release the waiters).
func (s *Service) releaseCancelledRunWaits(runID string) {
	if s == nil || s.waits == nil || strings.TrimSpace(runID) == "" {
		return
	}
	for _, e := range s.waits.ReleaseRun(runID) {
		slog.Info("dagger wait released by run cancellation", "key", e.Key, "run_id", runID)
	}
}
