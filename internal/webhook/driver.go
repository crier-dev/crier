package webhook

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Driver orchestrates webhook delivery: immediate POST attempts, bounded
// retries with exponential backoff, a durable queue for offline endpoints,
// and a circuit breaker that degrades poisoned endpoints (probe + drain).
//
// Ticket: CR-FEAT-001. Spec: specs/WEBHOOK-DELIVERY.md §3-§4.
type Driver struct {
	client      *Client
	queue       Queue
	cfg         DriverConfig
	resolveConf func(agentID string) (*Config, error)
	gate        *sessionGate
	mu          sync.Mutex
	degraded    map[string]time.Time // agentID -> degraded-since
	failures    map[string]int       // agentID -> consecutive failures
	stopCh      chan struct{}
	stopped     bool
	wg          sync.WaitGroup
}

// DriverConfig holds env-driven tuning (spec §9).
type DriverConfig struct {
	MaxRetries      int           // CR_WEBHOOK_MAX_RETRIES (default 5)
	RedeliverEvery  time.Duration // CR_WEBHOOK_REDELIVER_S (default 30s)
	ProbeEvery      time.Duration // CR_WEBHOOK_PROBE_S (default 60s)
	CircuitThreshold int          // CR_WEBHOOK_CIRCUIT_THRESHOLD (default 10)
}

// DefaultDriverConfig returns the spec defaults.
func DefaultDriverConfig() DriverConfig {
	return DriverConfig{
		MaxRetries:       5,
		RedeliverEvery:   30 * time.Second,
		ProbeEvery:       60 * time.Second,
		CircuitThreshold: 10,
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
	return &Driver{
		client:   client,
		queue:    queue,
		cfg:      cfg,
		gate:     newSessionGate(),
		degraded: make(map[string]time.Time),
		failures: make(map[string]int),
		stopCh:   make(chan struct{}),
	}
}

// Start launches the redelivery + probe loops. Call once after construction.
func (d *Driver) Start() {
	d.wg.Add(2)
	go d.redeliverLoop()
	go d.probeLoop()
}

// Stop terminates the loops. Idempotent.
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
}

// Deliver attempts immediate delivery of one envelope to an agent's webhook.
// If the endpoint is unreachable or transiently failing, the item is queued
// for redelivery. Returns (delivered, error). Blocking-mode reply extraction
// is CR-FEAT-002 — v1 always returns after the POST attempt (async semantics).
func (d *Driver) Deliver(agentID string, cfg *Config, env *Envelope) (bool, error) {
	if cfg == nil {
		return false, fmt.Errorf("webhook: nil config for %s", agentID)
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

// enqueue stores the item for redelivery.
func (d *Driver) enqueue(agentID string, env *Envelope, retries int) error {
	return d.queue.Push(&QueueItem{
		AgentID:   agentID,
		Envelope:  env,
		Retries:   retries,
		CreatedAt: time.Now(),
	})
}

// redeliverLoop drains the queue on the redelivery interval, skipping agents
// whose circuit is open.
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

		res := d.client.Post(cfg, item.Envelope, item.Retries)
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
		}
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
