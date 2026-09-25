// Package ratelimit implements the sliding-window hit counter crier's ingest
// lanes shed load with (CR-FEAT-035).
//
// ONE implementation, two lanes: the relay's per-agent publish cap
// (Relay.CheckRateLimit, CR_RATE_LIMIT_PER_MINUTE) and the global inbox-ingest
// budget on POST /agents/{id}/inbox (CR_RATE_LIMIT_GLOBAL_PER_MINUTE). Both
// need the same two answers — "is this caller over its budget?" and, when it
// is, "how long until a slot frees?" — and the second one is what a 429's
// `Retry-After` header reports, so a shed caller can back off instead of
// hammering a bus that is already over budget.
//
// The semantics are deliberate and load-bearing:
//
//   - EVERY attempt is recorded, including one that is refused. A refused
//     attempt keeps the window full while a flood continues, which is the
//     point of a shed: the bus is not obliged to admit work merely because the
//     work was offered. A caller that stops and waits the window out is
//     admitted again — RetryAfter answers for exactly that caller.
//   - limit <= 0 means "no limit at all" (Allow always true, RetryAfter 0).
//     That is how "off" is represented everywhere in this repo, and it is why
//     an unconfigured lane behaves byte-identically to a build without this
//     package.
//   - hits are counted per KEY: the relay keys on agent id, the inbox-ingest
//     budget on one fixed key today (a namespace key, once CR-FEAT-029 lands).
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Window is a sliding-window hit counter keyed by string. It is safe for
// concurrent use and starts a background cleanup loop that drops keys whose
// hits have all aged out, so a key set that grows with traffic (one entry per
// agent id) cannot grow without bound.
type Window struct {
	mu      sync.Mutex
	entries map[string][]time.Time
}

// NewWindow creates a Window and starts its cleanup loop. cleanupInterval is
// how often expired keys are swept — callers pass a small multiple of the
// window they use.
func NewWindow(cleanupInterval time.Duration) *Window {
	w := &Window{entries: make(map[string][]time.Time)}
	go w.cleanupLoop(cleanupInterval)
	return w
}

// Allow records one attempt for key and reports whether it is admitted: true
// while the number of attempts inside the window — this one included — is at
// most limit. When limit is 0 (or negative) rate limiting is disabled and every
// attempt is admitted.
func (w *Window) Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}

	now := time.Now()
	cutoff := now.Add(-window)

	w.mu.Lock()
	defer w.mu.Unlock()

	times := append(w.entries[key], now)
	times = trimBefore(times, cutoff)

	if len(times) == 0 {
		delete(w.entries, key)
	} else {
		w.entries[key] = times
	}

	// Count is the number of timestamps within the window (including this one).
	return len(times) <= limit
}

// RetryAfter reports how long a caller refused RIGHT NOW should wait before
// retrying, so that a retry at that instant is admitted if nothing else is
// competing for the budget. 0 means "a slot is free now" (no wait is owed) and
// is also what a disabled limit returns.
//
// It is the wait for the OLDEST recorded attempt that must age out: with n
// attempts inside the window and a limit of limit (n >= limit, or the caller
// was not refused by this budget), this attempt is admitted once n - limit + 1
// of them have aged out, i.e. once the (n-limit+1)-th oldest — index n-limit —
// has left the window.
//
// RetryAfter does NOT record an attempt: a 429's advisory header must not
// consume the very budget the caller is being told to wait for.
func (w *Window) RetryAfter(key string, limit int, window time.Duration) time.Duration {
	if limit <= 0 || window <= 0 {
		return 0
	}

	now := time.Now()
	cutoff := now.Add(-window)

	w.mu.Lock()
	times := trimBefore(w.entries[key], cutoff)
	if len(times) == 0 {
		delete(w.entries, key)
	} else {
		w.entries[key] = times
	}
	n := len(times)
	// The timestamp is read UNDER the lock: another caller's Allow can append
	// to this key's slice (writing into the same backing array) the moment the
	// lock is released, so a read afterwards would be a race on shared memory
	// rather than an answer about this caller.
	var wait time.Duration
	if n >= limit {
		wait = times[n-limit].Add(window).Sub(now)
	}
	w.mu.Unlock()

	if wait <= 0 {
		// A slot is free, or the oldest attempt that had to age out already has.
		return 0
	}
	return wait
}

// HeaderSeconds renders a RetryAfter duration as the whole number of seconds an
// HTTP `Retry-After` header carries: rounded UP, so a caller that waits exactly
// the reported number of seconds is never admitted a fraction early, and never
// below 1 — the header is only written when a shed actually happened, and
// "retry in 0 seconds" would read as "retry immediately", which is what the
// shed just refused.
func HeaderSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	secs := int(math.Ceil(d.Seconds()))
	if secs < 1 {
		return 1
	}
	return secs
}

// trimBefore returns the tail of times whose entries are NOT before cutoff
// (times is appended to in order, so the expired ones are always a prefix).
func trimBefore(times []time.Time, cutoff time.Time) []time.Time {
	start := 0
	for start < len(times) && times[start].Before(cutoff) {
		start++
	}
	if start == 0 {
		return times
	}
	return times[start:]
}

// cleanupLoop periodically drops keys whose every hit has aged out.
func (w *Window) cleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		w.cleanup()
	}
}

// cleanup removes entries that have no timestamps within the last 5 minutes.
// This is conservative — the rate limit window is typically 1 minute, so 5
// minutes ensures we never remove active entries while still preventing
// unbounded growth.
func (w *Window) cleanup() {
	cutoff := time.Now().Add(-5 * time.Minute)

	w.mu.Lock()
	defer w.mu.Unlock()

	for key, times := range w.entries {
		trimmed := trimBefore(times, cutoff)
		if len(trimmed) == 0 {
			delete(w.entries, key)
		} else {
			w.entries[key] = trimmed
		}
	}
}
