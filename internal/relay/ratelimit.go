package relay

import (
	"time"

	"github.com/crier-dev/crier/internal/ratelimit"
)

// RateLimiter is the relay's per-agent publish budget: the sliding-window hit
// counter keyed by agent id.
//
// The implementation lives in internal/ratelimit because the inbox-ingest
// global budget (CR-FEAT-035) needs the same two answers from it — "is this
// caller over budget?" and "how long until a slot frees?" (the 429's
// `Retry-After`) — and two copies of a sliding window are two chances to shed
// with two different rules. The name and the `Allow(key, limit, window)`
// signature are unchanged, so every existing caller and test is untouched.
type RateLimiter = ratelimit.Window

// NewRateLimiter creates a new RateLimiter and starts a background goroutine
// that periodically cleans up expired entries.
func NewRateLimiter(cleanupInterval time.Duration) *RateLimiter {
	return ratelimit.NewWindow(cleanupInterval)
}
