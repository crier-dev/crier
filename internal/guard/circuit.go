package guard

import (
	"sync"
	"time"
)

// Circuit is a per-endpoint (base_url+model) circuit breaker following the
// webhook driver's recordFailure/recordSuccess/degraded semantics
// (spec §5.4): CR_GUARD_CIRCUIT_THRESHOLD consecutive failures open the
// circuit for CR_GUARD_CIRCUIT_COOLDOWN_S; while open the provider is
// skipped in the chain (cheap failover, no wasted calls); the first call
// after cooldown expiry is allowed through as the probe — success closes
// the circuit, failure reopens it. In-memory per process (same as the
// webhook driver's degraded map — restart resets it).
type Circuit struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	failures  map[string]int
	openedAt  map[string]time.Time
}

// NewCircuit builds a circuit breaker. threshold <= 0 defaults to 10;
// cooldown <= 0 defaults to 300s.
func NewCircuit(threshold int, cooldown time.Duration) *Circuit {
	if threshold <= 0 {
		threshold = 10
	}
	if cooldown <= 0 {
		cooldown = 300 * time.Second
	}
	return &Circuit{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		failures:  make(map[string]int),
		openedAt:  make(map[string]time.Time),
	}
}

// open reports whether the endpoint is currently tripped. After the
// cooldown expires the endpoint is NOT open: the next call is the probe.
func (c *Circuit) open(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.failures[key]
	if n < c.threshold {
		return false
	}
	opened, ok := c.openedAt[key]
	if !ok {
		return false
	}
	return c.now().Sub(opened) < c.cooldown
}

// recordSuccess resets the failure streak and closes the circuit.
func (c *Circuit) recordSuccess(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.failures, key)
	delete(c.openedAt, key)
}

// recordFailure counts a consecutive failure; at the threshold the circuit
// trips for the cooldown window.
func (c *Circuit) recordFailure(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures[key]++
	if c.failures[key] >= c.threshold {
		if _, exists := c.openedAt[key]; !exists {
			c.openedAt[key] = c.now()
		}
	}
}
