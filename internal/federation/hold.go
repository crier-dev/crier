package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Hold queue and retry worker (DF-CRIER-7, specs/WEBHOOK-DELIVERY.md §8):
//
//	Link down → durable queue at source; ERROR after CR_FED_MAX_HOLD_S.
//
// A delivery whose links all fail *transiently* (transport error, retryable
// status) is no longer reported to the sender as an instant 404. It is
// handed to a HoldQueue, retried by a bounded background worker until it is
// delivered or its hold budget expires, and only then reported as an
// explicit FEDERATION_FAILED outcome. A definitive all-links-404 is *not*
// transient: the agent really is unknown everywhere and the caller still
// answers 404 immediately.

// CodeFederationFailed is the machine-readable code carried by every
// terminal federation failure: the FEDERATION_FAILED notification written
// into the sender's inbox after CR_FED_MAX_HOLD_S expires, and the
// synchronous HTTP 502 body used when no hold queue is configured.
const CodeFederationFailed = "FEDERATION_FAILED"

// DefaultMaxHold is the default hold budget (CR_FED_MAX_HOLD_S): how long a
// delivery whose links are all transiently down is held at the source relay
// before the sender gets an explicit FEDERATION_FAILED (spec §8, §9).
const DefaultMaxHold = 300 * time.Second

// Retry-worker and queue defaults. Every one is overridable (HoldConfig /
// queue fields) so tests inject millisecond timings and never wait real
// seconds.
const (
	// DefaultRetryEvery is the first retry delay and the worker tick.
	DefaultRetryEvery = 2 * time.Second
	// DefaultMaxRetryInterval caps the exponential backoff between retries.
	DefaultMaxRetryInterval = 60 * time.Second
	// DefaultMaxHoldItems bounds the number of held deliveries.
	DefaultMaxHoldItems = 1000
	// DefaultMaxHoldBodyBytes caps one held delivery body (the encoded
	// deliver JSON) so a single message cannot pin unbounded memory or
	// disk.
	DefaultMaxHoldBodyBytes = 1 << 20
)

// HoldItem is one delivery held at the source relay because every configured
// link failed transiently. It carries enough context to retry the forward
// byte-for-byte and to report a terminal FEDERATION_FAILED with correlation
// details.
type HoldItem struct {
	// ID is the message id assigned to the delivery by the source relay
	// (it is also the queue key). Reported to the sender on failure.
	ID string `json:"id"`
	// AgentID is the target agent id the delivery is addressed to.
	AgentID string `json:"agent_id"`
	// Body is the exact deliver JSON that was (and will again be) POSTed to
	// the linked relay — never re-encoded, so a retry forwards the same
	// bytes the original attempt sent.
	Body json.RawMessage `json:"body"`
	// Sender, RequestID and SessionID are the correlation context echoed in
	// the terminal report.
	Sender    string `json:"sender,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// EnqueuedAt/Deadline bound the hold: the delivery is retried until
	// Deadline (EnqueuedAt + CR_FED_MAX_HOLD_S), then failed.
	EnqueuedAt time.Time `json:"enqueued_at"`
	Deadline   time.Time `json:"deadline"`
	// NextAttemptAt is when the next retry is due (exponential backoff).
	NextAttemptAt time.Time `json:"next_attempt_at"`
	// Attempts counts forwarded attempts made for this delivery, including
	// the first synchronous pass that discovered the outage.
	Attempts int `json:"attempts"`
	// LastStatus/LastError describe the most recent failed attempt
	// (LastStatus is 0 when the failure was transport-level).
	LastStatus int    `json:"last_status,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// clone returns a copy of the item with an independent Body slice, so
// callers of List can neither mutate the queue's stored copy nor race with
// the retry worker.
func (i *HoldItem) clone() *HoldItem {
	c := *i
	if i.Body != nil {
		c.Body = append(json.RawMessage(nil), i.Body...)
	}
	return &c
}

// HoldQueue stores deliveries held at the source relay. Implementations must
// be safe for concurrent callers.
//
// Durability is per implementation and stated in its doc comment:
// MemoryHoldQueue is process-lifetime (matching the documented in-memory
// registry backend contract), FileHoldQueue is a durable, atomically
// rewritten document that survives a restart.
type HoldQueue interface {
	// Enqueue stores a held delivery. It rejects items without an id/target,
	// with an empty or non-JSON body, with a body above the queue's body
	// cap, and when the queue is full — the caller then reports the failure
	// synchronously instead of silently dropping the message.
	Enqueue(item *HoldItem) error
	// Update replaces the stored item with the same ID (attempt counters).
	Update(item *HoldItem) error
	// Remove deletes the item with the given ID. Removing an unknown id is
	// not an error.
	Remove(id string) error
	// List returns copies of every pending item.
	List() []*HoldItem
	// Len returns the number of pending items.
	Len() int
}

// validateHoldItem applies the shared queue invariants.
func validateHoldItem(item *HoldItem, bodyCap int) error {
	if item == nil {
		return errors.New("federation: hold queue: nil item")
	}
	if item.ID == "" {
		return errors.New("federation: hold queue: item has no message id")
	}
	if item.AgentID == "" {
		return errors.New("federation: hold queue: item has no target agent")
	}
	if len(item.Body) == 0 {
		return errors.New("federation: hold queue: refusing to hold an empty body")
	}
	if bodyCap > 0 && len(item.Body) > bodyCap {
		return fmt.Errorf("federation: hold queue: body %d bytes exceeds the %d-byte cap", len(item.Body), bodyCap)
	}
	// A non-nil zero-length or malformed RawMessage is JSON poison: it would
	// fail the enclosing marshal of the durable document.
	if !json.Valid(item.Body) {
		return errors.New("federation: hold queue: body is not valid JSON")
	}
	return nil
}

// MemoryHoldQueue is the default in-process hold queue. Held deliveries live
// for the process lifetime only and are lost on restart — the same contract
// the in-memory registry backend documents for inboxes ("State is ephemeral
// ... Use PostgresStore for durability across restarts"). Configure
// OpenFileHoldQueue when the queue must survive a restart.
type MemoryHoldQueue struct {
	mu    sync.Mutex
	items []*HoldItem
	// MaxItems / MaxBodyBytes bound the queue; zero means the package
	// defaults (DefaultMaxHoldItems / DefaultMaxHoldBodyBytes).
	MaxItems     int
	MaxBodyBytes int
}

// NewMemoryHoldQueue returns an empty process-lifetime hold queue with the
// default bounds.
func NewMemoryHoldQueue() *MemoryHoldQueue {
	return &MemoryHoldQueue{}
}

var _ HoldQueue = (*MemoryHoldQueue)(nil)

func (q *MemoryHoldQueue) bodyCap() int {
	if q.MaxBodyBytes > 0 {
		return q.MaxBodyBytes
	}
	return DefaultMaxHoldBodyBytes
}

func (q *MemoryHoldQueue) itemCap() int {
	if q.MaxItems > 0 {
		return q.MaxItems
	}
	return DefaultMaxHoldItems
}

// Enqueue appends a held delivery.
func (q *MemoryHoldQueue) Enqueue(item *HoldItem) error {
	if err := validateHoldItem(item, q.bodyCap()); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if cap := q.itemCap(); len(q.items) >= cap {
		return fmt.Errorf("federation: hold queue full (%d held deliveries)", cap)
	}
	q.items = append(q.items, item.clone())
	return nil
}

// Update replaces the stored item with the same ID.
func (q *MemoryHoldQueue) Update(item *HoldItem) error {
	if err := validateHoldItem(item, q.bodyCap()); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, existing := range q.items {
		if existing.ID == item.ID {
			q.items[i] = item.clone()
			return nil
		}
	}
	return fmt.Errorf("federation: hold queue: unknown item %q", item.ID)
}

// Remove deletes the item with the given ID (no-op when absent).
func (q *MemoryHoldQueue) Remove(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, existing := range q.items {
		if existing.ID == id {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return nil
		}
	}
	return nil
}

// List returns copies of the pending items.
func (q *MemoryHoldQueue) List() []*HoldItem {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*HoldItem, 0, len(q.items))
	for _, item := range q.items {
		out = append(out, item.clone())
	}
	return out
}

// Len returns the number of pending items.
func (q *MemoryHoldQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// HoldMeta is the correlation context captured when a delivery is held. It
// rides into the terminal FEDERATION_FAILED report so the sender can match
// the failure to the request it sent.
type HoldMeta struct {
	MessageID string
	Sender    string
	RequestID string
	SessionID string
}

// FailureReport is the terminal FEDERATION_FAILED outcome for one held
// delivery: every link was still failing when the hold budget expired. It
// carries the correlation context required by spec §8 — message id, target
// agent, sender, request/session ids when present, attempts, and the last
// observed error/status. The message body is never included, so the report
// is bounded.
type FailureReport struct {
	Code      string `json:"code"`
	MessageID string `json:"message_id"`
	Target    string `json:"target"`
	Sender    string `json:"sender,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Attempts  int    `json:"attempts"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// FailureNotifier delivers a terminal FEDERATION_FAILED report. The server
// wires it to the registry store's direct inbox write
// (registry.FederationFailureSink), so the notification cannot route back
// through federation and cannot requeue itself. A non-nil error is logged
// and never retried.
type FailureNotifier func(FailureReport) error

// HoldConfig tunes the hold/retry worker. Zero values fall back to the
// package defaults; tests inject millisecond values so no test waits real
// seconds.
type HoldConfig struct {
	MaxHold          time.Duration // CR_FED_MAX_HOLD_S (DefaultMaxHold)
	RetryEvery       time.Duration // first retry delay + worker tick (DefaultRetryEvery)
	MaxRetryInterval time.Duration // backoff ceiling (DefaultMaxRetryInterval)
}

func (c HoldConfig) withDefaults() HoldConfig {
	if c.MaxHold <= 0 {
		c.MaxHold = DefaultMaxHold
	}
	if c.RetryEvery <= 0 {
		c.RetryEvery = DefaultRetryEvery
	}
	if c.MaxRetryInterval <= 0 {
		c.MaxRetryInterval = DefaultMaxRetryInterval
	}
	return c
}

// HoldManager retries held deliveries and reports terminal failures. One
// instance owns one goroutine for the process lifetime (Start/Stop); the
// sweep itself is exported so tests can drive deterministic passes without
// the timer.
type HoldManager struct {
	client *Client
	queue  HoldQueue
	cfg    HoldConfig

	mu      sync.Mutex
	notify  FailureNotifier
	started bool
	stopped bool
	wg      sync.WaitGroup
	stopCh  chan struct{}
	wakeCh  chan struct{}
}

// NewHoldManager builds the retry worker over queue. client is used for the
// retry forwards (it must be the same Client the handler forwards with, so
// link auth, the hop header and the body passthrough stay identical).
func NewHoldManager(client *Client, queue HoldQueue, cfg HoldConfig) *HoldManager {
	return &HoldManager{
		client: client,
		queue:  queue,
		cfg:    cfg.withDefaults(),
		stopCh: make(chan struct{}),
		wakeCh: make(chan struct{}, 1),
	}
}

// SetNotifier installs the terminal-failure notifier. Nil (the default)
// logs the FEDERATION_FAILED report instead of delivering it.
func (m *HoldManager) SetNotifier(n FailureNotifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify = n
}

// Queue exposes the underlying queue (observability/tests).
func (m *HoldManager) Queue() HoldQueue { return m.queue }

// MaxHold returns the configured hold budget.
func (m *HoldManager) MaxHold() time.Duration { return m.cfg.MaxHold }

// Pending returns the number of held deliveries.
func (m *HoldManager) Pending() int { return m.queue.Len() }

// Start launches the retry loop. Idempotent; a no-op after Stop.
func (m *HoldManager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.stopped {
		return
	}
	m.started = true
	m.wg.Add(1)
	go m.loop()
}

// Stop terminates the retry loop and waits for it, so shutdown leaks no
// goroutine. Idempotent, and safe to call without Start. Pending items stay
// in the queue: a memory queue dies with the process (documented), a file
// queue is already on disk and is resumed by the next process.
func (m *HoldManager) Stop() {
	m.mu.Lock()
	if !m.started || m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	close(m.stopCh)
	m.mu.Unlock()
	m.wg.Wait()
}

// Enqueue holds one delivery. Zero-valued timestamps are filled from the
// manager's clock and budget, so a delivery held by a *previous* process
// (reopened file queue) keeps its original deadline and attempts.
func (m *HoldManager) Enqueue(item *HoldItem) error {
	if item == nil {
		return errors.New("federation: hold: nil item")
	}
	now := time.Now()
	if item.EnqueuedAt.IsZero() {
		item.EnqueuedAt = now
	}
	if item.Deadline.IsZero() {
		item.Deadline = item.EnqueuedAt.Add(m.cfg.MaxHold)
	}
	if item.Attempts == 0 {
		item.Attempts = 1 // the synchronous pass that discovered the outage
	}
	if item.NextAttemptAt.IsZero() {
		item.NextAttemptAt = item.EnqueuedAt.Add(m.delay(item.Attempts))
	}
	if err := m.queue.Enqueue(item); err != nil {
		return err
	}
	slog.Warn("federation: link down, delivery held at source",
		"message_id", item.ID, "target", item.AgentID, "attempts", item.Attempts,
		"hold_s", int(m.cfg.MaxHold.Seconds()))
	m.wake()
	return nil
}

// wake nudges the retry loop without blocking (safe before Start and after
// Stop: the channel is buffered and never closed).
func (m *HoldManager) wake() {
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
}

func (m *HoldManager) loop() {
	defer m.wg.Done()
	timer := time.NewTimer(m.cfg.RetryEvery)
	defer timer.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-m.wakeCh:
		case <-timer.C:
		}
		m.Sweep(time.Now())
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(m.cfg.RetryEvery)
	}
}

// Sweep runs one retry pass over every pending item: due items are
// re-forwarded, exhausted items are failed. Exported (and time-injectable)
// so tests drive it directly.
func (m *HoldManager) Sweep(now time.Time) {
	for _, item := range m.queue.List() {
		m.attempt(item, now)
	}
}

// delay is the backoff before the next attempt: RetryEvery doubled per
// failed attempt, capped at MaxRetryInterval.
func (m *HoldManager) delay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := attempts - 1
	if shift > 5 {
		shift = 5
	}
	d := m.cfg.RetryEvery << uint(shift)
	if d > m.cfg.MaxRetryInterval || d <= 0 {
		d = m.cfg.MaxRetryInterval
	}
	return d
}

// attempt retries one held delivery: it fails an item whose budget has
// expired, forwards a due item, and either completes it (delivered),
// reports it (definitive answer that is not a delivery), or records the
// transient failure and backs off.
func (m *HoldManager) attempt(item *HoldItem, now time.Time) {
	if now.Before(item.NextAttemptAt) {
		return
	}
	if !now.Before(item.Deadline) {
		// Budget exhausted: explicit terminal FEDERATION_FAILED, never a
		// silent drop and never a misleading 404 (spec §8, DF-CRIER-7).
		m.fail(item, item.LastStatus, item.LastError, "hold budget exhausted")
		return
	}

	timeout := m.client.timeout()
	if remaining := item.Deadline.Sub(now); remaining < timeout {
		timeout = remaining
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// forwardPass (not ForwardToAny) so a retry reuses the exact
	// single-pass classification: first non-404, non-retryable answer wins,
	// transport errors and retryable statuses mean "link still down".
	status, body, err := m.client.forwardPass(ctx, item.AgentID, item.Body)
	switch {
	case err == nil && status >= 200 && status < 300:
		// Recovered: delivered exactly once, then removed so no later pass
		// can forward it again.
		m.complete(item, status, body)
	case err == nil:
		// A definitive non-2xx answer (rejection): the attempt completed and
		// retrying cannot help — report the outcome instead of looping.
		m.fail(item, status, fmt.Sprintf("linked relay answered %d", status), "definitive rejection")
	default:
		var transient *TransientError
		if errors.As(err, &transient) {
			item.Attempts++
			item.LastStatus = transient.LastStatus
			item.LastError = transient.Error()
			item.NextAttemptAt = now.Add(m.delay(item.Attempts))
			if uerr := m.queue.Update(item); uerr != nil {
				slog.Warn("federation: could not persist hold retry state",
					"message_id", item.ID, "error", uerr)
			}
			return
		}
		// Every link answered 404: the agent exists nowhere in the
		// federation, so the hold cannot ever succeed.
		m.fail(item, http.StatusNotFound, err.Error(), "agent not found on any linked relay")
	}
}

func (m *HoldManager) complete(item *HoldItem, status int, body []byte) {
	if err := m.queue.Remove(item.ID); err != nil {
		// The item stays queued, so the next pass may forward it again —
		// at-least-once, logged loudly rather than hidden.
		slog.Warn("federation: held delivery delivered but not removed from the queue",
			"message_id", item.ID, "error", err)
	}
	slog.Info("federation: held delivery recovered — relayed to the linked relay",
		"message_id", item.ID, "target", item.AgentID, "sender", item.Sender,
		"attempts", item.Attempts, "status", status,
		"response_bytes", len(body), "response", snippet(body))
}

// fail removes the item and emits exactly one FEDERATION_FAILED report.
// Remove-then-notify: the item leaves the queue before the report is
// attempted, so a re-run sweep (or a concurrent sweep) can never emit a
// second report — exactly-once in normal operation, at-most-once across a
// crash in that window.
func (m *HoldManager) fail(item *HoldItem, status int, lastErr, reason string) {
	if err := m.queue.Remove(item.ID); err != nil {
		slog.Warn("federation: could not remove exhausted delivery from the queue",
			"message_id", item.ID, "error", err)
	}
	report := FailureReport{
		Code:      CodeFederationFailed,
		MessageID: item.ID,
		Target:    item.AgentID,
		Sender:    item.Sender,
		RequestID: item.RequestID,
		SessionID: item.SessionID,
		Attempts:  item.Attempts,
		Status:    status,
		Error:     lastErr,
	}
	slog.Warn("federation: held delivery failed (FEDERATION_FAILED)",
		"message_id", item.ID, "target", item.AgentID, "sender", item.Sender,
		"attempts", item.Attempts, "status", status, "reason", reason)

	m.mu.Lock()
	notify := m.notify
	m.mu.Unlock()
	if notify == nil {
		return
	}
	if err := notify(report); err != nil {
		slog.Warn("federation: FEDERATION_FAILED notification failed (best-effort)",
			"message_id", item.ID, "sender", item.Sender, "error", err)
	}
}

// snippet bounds a response body for logging.
func snippet(body []byte) string {
	const max = 200
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "…"
}
