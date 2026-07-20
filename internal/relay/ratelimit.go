package relay

import (
	"sync"
	"time"
)

// RateLimiter is a sliding-window rate limiter that tracks requests per key.
// It is safe for concurrent use.
type RateLimiter struct {
	mu      sync.Mutex
	entries map[string][]time.Time
}

// NewRateLimiter creates a new RateLimiter and starts a background goroutine
// that periodically cleans up expired entries.
func NewRateLimiter(cleanupInterval time.Duration) *RateLimiter {
	rl := &RateLimiter{
		entries: make(map[string][]time.Time),
	}
	go rl.cleanupLoop(cleanupInterval)
	return rl
}

// Allow checks whether the given key is under the limit within the sliding window.
// Returns true if the request is allowed, false if the rate limit has been exceeded.
// When limit is 0, rate limiting is disabled and Allow always returns true.
func (rl *RateLimiter) Allow(key string, limit int, window time.Duration) bool {
	if limit <= 0 {
		return true
	}

	now := time.Now()
	cutoff := now.Add(-window)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Get or create the entry slice for this key.
	times := rl.entries[key]

	// Append the current timestamp.
	times = append(times, now)

	// Trim timestamps outside the window.
	start := 0
	for start < len(times) && times[start].Before(cutoff) {
		start++
	}
	times = times[start:]

	// Store the trimmed slice back.
	if len(times) == 0 {
		delete(rl.entries, key)
	} else {
		rl.entries[key] = times
	}

	// Count is the number of timestamps within the window (including this one).
	return len(times) <= limit
}

// cleanupLoop periodically removes expired entries to prevent unbounded memory growth.
func (rl *RateLimiter) cleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		rl.cleanup(interval)
	}
}

// cleanup removes entries that have no timestamps within the last 5 minutes.
// This is conservative — the rate limit window is typically 1 minute, so 5 minutes
// ensures we never remove active entries while still preventing unbounded growth.
func (rl *RateLimiter) cleanup(_ time.Duration) {
	cutoff := time.Now().Add(-5 * time.Minute)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	for key, times := range rl.entries {
		start := 0
		for start < len(times) && times[start].Before(cutoff) {
			start++
		}
		if start == len(times) {
			delete(rl.entries, key)
		} else {
			rl.entries[key] = times[start:]
		}
	}
}
