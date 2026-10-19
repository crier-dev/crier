package main

import (
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

// a2aIdempotencyInfo describes the actual store, not the configured backend
// string (which may be unset or hidden by AuthorityStore). Unknown custom stores
// receive only a process-lifetime guarantee, never an unproven durable claim.
func a2aIdempotencyInfo(store registry.Store, window time.Duration, replayed bool) *a2a.IdempotencyInfo {
	if window <= 0 {
		window = registry.DefaultIdempotencyWindow
	}
	backend, ok := store.(registry.A2AReplayDurability)
	return a2a.NewIdempotencyInfo(ok && backend.A2AReplayDurable(), window.Seconds(), replayed)
}
