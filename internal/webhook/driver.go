package webhook

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// CodeWebhookFailed is the machine-readable error code carried by the
// durable sender notification emitted when an async delivery exhausts its
// bounded retries (DF-CRIER-8, spec §4).
const CodeWebhookFailed = "WEBHOOK_FAILED"

// FailureNotification describes one queued delivery that exhausted its
// bounded retries (DF-CRIER-8). It is emitted exactly once per dropped
// item, after exhaustion only, via the notifier installed with
// SetFailureNotifier. StatusCode is 0 on transport-level failure; Err then
// carries the error string.
type FailureNotification struct {
	MessageID   string `json:"message_id"`
	Sender      string `json:"sender"`
	TargetAgent string `json:"target_agent"`
	Retries     int    `json:"retries"`
	StatusCode  int    `json:"status_code,omitempty"`
	Err         string `json:"error,omitempty"`
}

// Driver orchestrates webhook delivery: immediate POST attempts, bounded
// retries with exponential backoff, a durable queue for offline endpoints,
// a circuit breaker that degrades poisoned endpoints (probe + drain), and —
// CR-FEAT-005 — async fire-and-forget (enqueue, don't wait) plus batch mode
// (per-endpoint buffer coalesced into ONE batch POST).
//
// Ticket: CR-FEAT-001/CR-FEAT-005. Spec: specs/WEBHOOK-DELIVERY.md §3-§4.
type Driver struct {
	client        *Client
	queue         Queue
	cfg           DriverConfig
	resolveConf   func(agentID string) (*Config, error)
	notifyFailure func(FailureNotification) error
	gate          *sessionGate
	mu            sync.Mutex
	degraded      map[string]time.Time // agentID -> degraded-since
	failures      map[string]int       // agentID -> consecutive failures
	batches       map[string]*batchBuffer
	drainWakeCh   chan struct{} // nudges redeliverLoop (async enqueues)
	flushWakeCh   chan struct{} // nudges batchLoop (batch enqueues)
	stopCh        chan struct{}
	stopped       bool
	wg            sync.WaitGroup
}

// DriverConfig holds env-driven tuning (spec §9).
type DriverConfig struct {
	MaxRetries         int           // CR_WEBHOOK_MAX_RETRIES (default 5)
	RedeliverEvery     time.Duration // CR_WEBHOOK_REDELIVER_S (default 30s)
	ProbeEvery         time.Duration // CR_WEBHOOK_PROBE_S (default 60s)
	CircuitThreshold   int           // CR_WEBHOOK_CIRCUIT_THRESHOLD (default 10)
	BatchMaxMessages   int           // CR_WEBHOOK_BATCH_MAX (default 10)
	BatchFlushInterval time.Duration // CR_WEBHOOK_BATCH_FLUSH_S (default 5s)
	BatchTick          time.Duration // batch loop granularity (default 50ms; tests may tune)
}

// DefaultDriverConfig returns the spec defaults.
func DefaultDriverConfig() DriverConfig {
	return DriverConfig{
		MaxRetries:         5,
		RedeliverEvery:     30 * time.Second,
		ProbeEvery:         60 * time.Second,
		CircuitThreshold:   10,
		BatchMaxMessages:   10,
		BatchFlushInterval: 5 * time.Second,
		BatchTick:          50 * time.Millisecond,
	}
}

// NewDriver builds a driver over a client, queue and config.
func NewDriver(client *Client, queue Queue, cfg DriverConfig) *Driver {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 5
	}
	if cfg.RedeliverEvery <= 0 {
		cfg.RedeliverEvery = 30 * time.Second
	}
	if cfg.ProbeEvery <= 0 {
		cfg.ProbeEvery = 60 * time.Second
	}
	if cfg.CircuitThreshold <= 0 {
		cfg.CircuitThreshold = 10
	}
	// Batch flush controls default from env (CR_WEBHOOK_*), then spec
	// defaults. Per-agent registration values override at flush time.
	if cfg.BatchMaxMessages <= 0 {
		cfg.BatchMaxMessages = 10
		if v := lookupEnv("CR_WEBHOOK_BATCH_MAX"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.BatchMaxMessages = n
			}
		}
	}
	if cfg.BatchFlushInterval <= 0 {
		cfg.BatchFlushInterval = 5 * time.Second
		if v := lookupEnv("CR_WEBHOOK_BATCH_FLUSH_S"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				cfg.BatchFlushInterval = time.Duration(n) * time.Second
			}
		}
	}
	if cfg.BatchTick <= 0 {
		cfg.BatchTick = 50 * time.Millisecond
	}
	return &Driver{
		client:      client,
		queue:       queue,
		cfg:         cfg,
		gate:        newSessionGate(),
		degraded:    make(map[string]time.Time),
		failures:    make(map[string]int),
		batches:     make(map[string]*batchBuffer),
		drainWakeCh: make(chan struct{}, 1),
		flushWakeCh: make(chan struct{}, 1),
		stopCh:      make(chan struct{}),
	}
}

// Start launches the redelivery, probe and batch-flush loops. Call once
// after construction.
func (d *Driver) Start() {
	d.wg.Add(3)
	go d.redeliverLoop()
	go d.probeLoop()
	go d.batchLoop()
}

// Stop terminates the loops and flushes pending batch buffers (best effort).
// Idempotent.
func (d *Driver) Stop() {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	close(d.stopCh)
	d.mu.Unlock()
	d.wg.Wait()
	// Pending batch envelopes are flushed on shutdown so a graceful stop
	// does not silently drop buffered messages.
	d.flushAllBatches()
}

// Deliver routes one envelope to an agent's webhook according to its
// delivery mode (spec §4):
//
//   - batch: appended to the per-endpoint buffer; a background flush POSTs
//     the coalesced batch when max_messages OR flush_interval_s is reached.
//     Returns immediately (accepted).
//   - async (fire-and-forget): enqueued immediately; the background drain
//     loop POSTs it (woken on the spot). Returns immediately — the sender
//     never waits on the endpoint.
//   - blocking (or legacy callers): one immediate POST attempt; on
//     transient failure the item is queued for redelivery.
//
// Returns (delivered, error); non-blocking modes return (false, nil) to
// signal "accepted, delivery in background".
func (d *Driver) Deliver(agentID string, cfg *Config, env *Envelope) (bool, error) {
	if cfg == nil {
		return false, fmt.Errorf("webhook: nil config for %s", agentID)
	}

	switch effectiveDeliveryMode(cfg, env) {
	case "batch":
		d.bufferEnqueue(agentID, cfg, env)
		return false, nil
	case "async":
		return d.enqueueAsync(agentID, env)
	default:
		// Legacy immediate-attempt path (blocking callers via Deliver).
	}

	d.mu.Lock()
	_, deg := d.degraded[agentID]
	d.mu.Unlock()
	if deg {
		// Circuit open: queue without hammering a poisoned endpoint.
		return false, d.enqueue(agentID, env, 0)
	}

	res := d.client.Post(cfg, env, 0)
	if res.Err == nil && !res.Retryable && res.StatusCode >= 200 && res.StatusCode < 300 {
		d.recordSuccess(agentID)
		return true, nil
	}
	d.recordFailure(agentID, res)
	// Transient failure / unreachable: queue for redelivery (bounded by retries).
	return false, d.enqueue(agentID, env, 0)
}

// effectiveDeliveryMode resolves the per-message mode: the envelope's own
// delivery_mode (sender override, spec §4) wins over the agent default.
func effectiveDeliveryMode(cfg *Config, env *Envelope) string {
	if env != nil && env.Crier.DeliveryMode != "" {
		return env.Crier.DeliveryMode
	}
	if cfg != nil && cfg.DeliveryMode != "" {
		return cfg.DeliveryMode
	}
	return "async"
}

// enqueueAsync implements fire-and-forget: the item goes straight to the
// durable queue and the drain loop is woken to POST it in the background.
func (d *Driver) enqueueAsync(agentID string, env *Envelope) (bool, error) {
	if err := d.enqueue(agentID, env, 0); err != nil {
		return false, err
	}
	d.wake(d.drainWakeCh)
	return false, nil
}

// wake nudges a background loop to run now (non-blocking, coalesced).
func (d *Driver) wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// DeliverBlocking waits for the endpoint's reply and returns it extracted per
// the schema template (CR-FEAT-002). Retries happen within the caller's
// budget; the request fails fast with the last error once the budget is
// exhausted. Per-session FIFO: concurrent blocking deliveries for the same
// session are serialized (CR-FEAT-004).
func (d *Driver) DeliverBlocking(ctx context.Context, agentID string, cfg *Config, env *Envelope, budget time.Duration) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("webhook: nil config for %s", agentID)
	}
	if budget <= 0 {
		budget = 30 * time.Second
	}
	release := d.gate.acquire(env.Crier.SessionID)
	defer release()

	deadline := time.Now().Add(budget)
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("webhook: blocking delivery cancelled: %w", err)
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("webhook: blocking delivery timed out after %s (last: %v)", budget, lastErr)
			}
			return nil, fmt.Errorf("webhook: blocking delivery timed out after %s", budget)
		}
		res := d.client.Post(cfg, env, attempt)
		if res.Err == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
			reply, err := d.client.ExtractReply(cfg, res.Body)
			if err != nil {
				return nil, fmt.Errorf("webhook: reply extraction: %w", err)
			}
			d.recordSuccess(agentID)
			return reply, nil
		}
		if res.Err != nil {
			lastErr = res.Err
		} else {
			lastErr = fmt.Errorf("status %d", res.StatusCode)
		}
		if !res.Retryable {
			d.recordFailure(agentID, res)
			return nil, fmt.Errorf("webhook: permanent failure: %w", lastErr)
		}
		d.recordFailure(agentID, res)
		// Backoff within the remaining budget.
		wait := backoff(attempt + 1)
		if rem := time.Until(deadline); wait > rem {
			wait = rem
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("webhook: blocking delivery cancelled: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// sessionGate serializes blocking deliveries per session (CR-FEAT-004):
// FIFO within a session, free interleaving across sessions.
type sessionGate struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newSessionGate() *sessionGate {
	return &sessionGate{locks: make(map[string]*sync.Mutex)}
}

// acquire returns a release func. Empty session ids are not gated.
func (g *sessionGate) acquire(sessionID string) func() {
	if sessionID == "" {
		return func() {}
	}
	g.mu.Lock()
	m, ok := g.locks[sessionID]
	if !ok {
		m = &sync.Mutex{}
		g.locks[sessionID] = m
	}
	g.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// enqueue stores the item for redelivery. The guard verdict rides along
// (spec §2.1: redelivery never re-runs the guard).
func (d *Driver) enqueue(agentID string, env *Envelope, retries int) error {
	item := &QueueItem{
		AgentID:   agentID,
		Envelope:  env,
		Retries:   retries,
		CreatedAt: time.Now(),
	}
	if env != nil && env.Crier.Guard != nil {
		g := env.Crier.Guard.Result()
		item.Guard = &g
	}
	return d.queue.Push(item)
}

// redeliverLoop drains the queue on the redelivery interval (and on demand
// via wake), skipping agents whose circuit is open.
func (d *Driver) redeliverLoop() {
	defer d.wg.Done()
	t := time.NewTicker(d.cfg.RedeliverEvery)
	defer t.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-t.C:
			d.drainQueue()
		case <-d.drainWakeCh:
			d.drainQueue()
		}
	}
}

// drainQueue attempts every queued item once; permanent failures and
// retry-exhausted items are dropped with a log (dead-letter v1 = log + event).
func (d *Driver) drainQueue() {
	items := d.queue.PopBatch(100)
	for _, item := range items {
		cfg := d.agentConfig(item.AgentID)
		if cfg == nil {
			logf("webhook: queue item dropped (agent gone)", "agent", item.AgentID)
			continue
		}
		d.mu.Lock()
		_, deg := d.degraded[item.AgentID]
		d.mu.Unlock()
		if deg {
			_ = d.queue.Push(item) // re-queue while circuit open
			continue
		}

		var res Result
		if len(item.Batch) > 0 {
			// CR-FEAT-005: queued batch items redeliver as ONE batch POST
			// (the flush failed while the endpoint was down).
			res = d.client.PostBatch(cfg, item.Batch, item.Retries)
		} else {
			res = d.client.Post(cfg, item.Envelope, item.Retries)
		}
		if res.Err == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
			d.recordSuccess(item.AgentID)
			continue
		}
		if !res.Retryable {
			// Permanent (4xx): dead-letter.
			logf("webhook: delivery dead-lettered (permanent failure)",
				"agent", item.AgentID, "status", res.StatusCode)
			d.recordSuccess(item.AgentID) // reset failure streak
			continue
		}
		d.recordFailure(item.AgentID, res)
		item.Retries++
		if item.Retries <= d.cfg.MaxRetries {
			_ = d.queue.Push(item)
		} else {
			logf("webhook: delivery dropped (retries exhausted)",
				"agent", item.AgentID, "retries", item.Retries)
			d.notifyExhausted(item, res)
		}
	}
}

// notifyExhausted emits the DF-CRIER-8 failure notification for a dropped
// (retry-exhausted) queue item. Exactly-once per item: drainQueue pops the
// item and does not requeue it, so this runs at most once. Best-effort:
// a missing sender or a failing sink is logged, never requeued, never
// panics, and never routes back through webhook delivery.
func (d *Driver) notifyExhausted(item *QueueItem, res Result) {
	if d.notifyFailure == nil {
		return
	}
	// Metadata source: the single envelope, or the first envelope of a
	// coalesced batch (same drain path, CR-FEAT-005).
	env := item.Envelope
	if env == nil && len(item.Batch) > 0 {
		env = item.Batch[0]
	}
	var sender, msgID string
	if env != nil {
		sender = env.Crier.Sender
		msgID = env.Crier.MessageID
	}
	if sender == "" {
		logf("webhook: failure notification skipped (no sender on exhausted item)",
			"agent", item.AgentID, "message_id", msgID, "retries", item.Retries)
		return
	}
	n := FailureNotification{
		MessageID:   msgID,
		Sender:      sender,
		TargetAgent: item.AgentID,
		Retries:     item.Retries,
		StatusCode:  res.StatusCode,
	}
	if res.Err != nil {
		n.Err = res.Err.Error()
	}
	if err := d.notifyFailure(n); err != nil {
		logf("webhook: failure notification failed (best-effort)",
			"sender", sender, "agent", item.AgentID, "message_id", msgID, "error", err)
	}
}

// probeLoop health-checks degraded endpoints; on success it drains the queue.
func (d *Driver) probeLoop() {
	defer d.wg.Done()
	t := time.NewTicker(d.cfg.ProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-t.C:
			d.probeDegraded()
		}
	}
}

func (d *Driver) probeDegraded() {
	d.mu.Lock()
	ids := make([]string, 0, len(d.degraded))
	for id := range d.degraded {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	for _, id := range ids {
		cfg := d.agentConfig(id)
		if cfg == nil {
			continue
		}
		// Probe = HEAD-style empty POST is not safe for arbitrary endpoints;
		// instead we try one queued delivery (if any) or a minimal envelope.
		probe := &Envelope{
			Crier: EnvelopeMeta{
				Version: 1, MessageID: "probe", Kind: "probe",
				Sender: "crier", DeliveryMode: "async",
			},
			Payload: []byte(`{}`),
		}
		res := d.client.Post(cfg, probe, 0)
		if res.Err == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
			d.mu.Lock()
			delete(d.degraded, id)
			delete(d.failures, id)
			d.mu.Unlock()
			logf("webhook: endpoint recovered", "agent", id)
			d.drainQueue()
		}
	}
}

func (d *Driver) recordSuccess(agentID string) {
	d.mu.Lock()
	delete(d.failures, agentID)
	delete(d.degraded, agentID)
	d.mu.Unlock()
}

func (d *Driver) recordFailure(agentID string, res Result) {
	d.mu.Lock()
	d.failures[agentID]++
	if d.failures[agentID] >= d.cfg.CircuitThreshold {
		if _, exists := d.degraded[agentID]; !exists {
			d.degraded[agentID] = time.Now()
			logf("webhook: endpoint degraded (circuit open)",
				"agent", agentID, "threshold", d.cfg.CircuitThreshold)
		}
	}
	d.mu.Unlock()
}

// SetConfigResolver installs the registry-backed resolver (wired once at
// server startup). Without it, Deliver returns an error for unknown agents.
func (d *Driver) SetConfigResolver(fn func(agentID string) (*Config, error)) {
	d.resolveConf = fn
}

// SetFailureNotifier installs the sink invoked exactly once when a queued
// delivery exhausts its bounded retries (DF-CRIER-8). Wire it to the
// registry Store's direct inbox Deliver path (registry.WebhookFailureSink)
// so the notification is durable and bypasses webhook routing (no
// recursion). Nil disables notifications (dropped items are log-only, the
// pre-DF-CRIER-8 behavior).
func (d *Driver) SetFailureNotifier(fn func(FailureNotification) error) {
	d.notifyFailure = fn
}

// agentConfig is the internal accessor used by the loops.
func (d *Driver) agentConfig(id string) *Config {
	if d.resolveConf == nil {
		return nil
	}
	cfg, err := d.resolveConf(id)
	if err != nil || cfg == nil {
		return nil
	}
	return cfg
}

// batchBuffer accumulates envelopes for one agent's batch-mode endpoint
// (CR-FEAT-005). Flush fires when the buffer reaches max_messages OR when
// the oldest envelope has waited flush_interval_s — whichever first.
type batchBuffer struct {
	mu      sync.Mutex
	agentID string
	cfg     *Config // latest registration config (refreshed on each add)
	items   []*QueueItem
	firstAt time.Time
}

// add appends an envelope and refreshes the buffer config. The guard
// verdict rides on the inner envelope (and in the item, spec §2.1).
func (b *batchBuffer) add(cfg *Config, env *Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg = cfg
	if len(b.items) == 0 {
		b.firstAt = time.Now()
	}
	item := &QueueItem{
		AgentID:   b.agentID,
		Envelope:  env,
		CreatedAt: time.Now(),
	}
	if env != nil && env.Crier.Guard != nil {
		g := env.Crier.Guard.Result()
		item.Guard = &g
	}
	b.items = append(b.items, item)
}

// take atomically removes all pending items, resetting the age clock.
func (b *batchBuffer) take() ([]*QueueItem, *Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return nil, b.cfg
	}
	out, cfg := b.items, b.cfg
	b.items = nil
	b.firstAt = time.Time{}
	return out, cfg
}

// pending reports the buffered count and the age of the oldest envelope.
func (b *batchBuffer) pending() (n int, age time.Duration, cfg *Config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.items) == 0 {
		return 0, 0, b.cfg
	}
	return len(b.items), time.Since(b.firstAt), b.cfg
}

// bufferEnqueue routes a batch-mode envelope into the per-agent buffer and
// wakes the flush loop. Never blocks on the endpoint.
func (d *Driver) bufferEnqueue(agentID string, cfg *Config, env *Envelope) {
	d.mu.Lock()
	buf := d.batches[agentID]
	if buf == nil {
		buf = &batchBuffer{agentID: agentID}
		d.batches[agentID] = buf
	}
	d.mu.Unlock()
	buf.add(cfg, env)
	d.wake(d.flushWakeCh)
}

// batchMax returns the effective max_messages for a config: the per-agent
// registration value, else the driver (env) default.
func (d *Driver) batchMax(cfg *Config) int {
	if cfg != nil && cfg.Batch != nil && cfg.Batch.MaxMessages > 0 {
		return cfg.Batch.MaxMessages
	}
	return d.cfg.BatchMaxMessages
}

// batchInterval returns the effective flush interval for a config: the
// per-agent registration value, else the driver (env) default.
func (d *Driver) batchInterval(cfg *Config) time.Duration {
	if cfg != nil && cfg.Batch != nil && cfg.Batch.FlushIntervalS > 0 {
		return time.Duration(cfg.Batch.FlushIntervalS) * time.Second
	}
	return d.cfg.BatchFlushInterval
}

// batchLoop flushes due batches on the ticker and on demand (wake).
func (d *Driver) batchLoop() {
	defer d.wg.Done()
	t := time.NewTicker(d.cfg.BatchTick)
	defer t.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-t.C:
			d.flushBatches(false)
		case <-d.flushWakeCh:
			d.flushBatches(false)
		}
	}
}

// flushBatches flushes every buffer that is due: count >= max_messages OR
// oldest envelope older than flush_interval_s. force flushes everything.
func (d *Driver) flushBatches(force bool) {
	d.mu.Lock()
	ids := make([]string, 0, len(d.batches))
	for id := range d.batches {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	for _, id := range ids {
		d.mu.Lock()
		buf := d.batches[id]
		d.mu.Unlock()
		if buf == nil {
			continue
		}
		n, age, cfg := buf.pending()
		if n == 0 {
			continue
		}
		if !force && n < d.batchMax(cfg) && age < d.batchInterval(cfg) {
			continue
		}
		d.flushBatch(buf)
	}
}

// flushBatch POSTs one batch envelope for the buffer. On transient failure
// the whole batch is re-queued as a single durable batch item (spec §4:
// "queue flush retry"); permanent failures dead-letter.
func (d *Driver) flushBatch(buf *batchBuffer) {
	items, cfg := buf.take()
	if len(items) == 0 {
		return
	}
	agentID := buf.agentID

	d.mu.Lock()
	_, deg := d.degraded[agentID]
	d.mu.Unlock()
	if deg {
		d.requeueBatch(agentID, items)
		return
	}

	envs := make([]*Envelope, 0, len(items))
	for _, it := range items {
		envs = append(envs, it.Envelope)
	}
	res := d.client.PostBatch(cfg, envs, 0)
	if res.Err == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
		d.recordSuccess(agentID)
		logf("webhook: batch delivered", "agent", agentID, "messages", len(envs), "status", res.StatusCode)
		return
	}
	if !res.Retryable {
		// Permanent (4xx): dead-letter.
		logf("webhook: batch dead-lettered (permanent failure)",
			"agent", agentID, "status", res.StatusCode, "messages", len(envs))
		d.recordSuccess(agentID)
		return
	}
	d.recordFailure(agentID, res)
	d.requeueBatch(agentID, items)
}

// requeueBatch pushes a failed batch into the durable queue as ONE batch
// item, preserving the coalescing across retries.
func (d *Driver) requeueBatch(agentID string, items []*QueueItem) {
	envs := make([]*Envelope, 0, len(items))
	for _, it := range items {
		envs = append(envs, it.Envelope)
	}
	_ = d.queue.Push(&QueueItem{
		AgentID:   agentID,
		Batch:     envs,
		Retries:   0,
		CreatedAt: time.Now(),
	})
}

// flushAllBatches force-flushes every buffer (used on shutdown).
func (d *Driver) flushAllBatches() {
	d.flushBatches(true)
}
