package guard

import (
	"sort"

	"github.com/crier-dev/crier/internal/metrics"
)

// MON-CRIER-003: the guard FAILS OPEN by design. A saturated concurrency cap
// (ErrConcurrencySaturated) and an exhausted provider chain
// (ErrAllProvidersFailed) both end with the message delivered and only a log
// line to show for it, so silence was indistinguishable from health. These
// two counters are incremented exactly where those errors are returned —
// series ownership follows the incrementing code (internal/metrics package
// comment) — and the circuit/inflight state is exposed as gauges from
// cmd/server (cmd/server/metrics_guard.go).
var (
	// saturatedTotal counts checks rejected because every slot of the
	// concurrency semaphore stayed held for the 2s acquire budget.
	saturatedTotal = metrics.Default.NewCounter("crier_guard_saturated_total",
		"LLM message-guard checks that failed open because the concurrency cap stayed full for the 2s acquire budget (ErrConcurrencySaturated).")

	// allProvidersFailedTotal counts checks whose whole fallback chain
	// failed, including the no-api-key arm — the guard then fails open or
	// closes per policy.
	allProvidersFailedTotal = metrics.Default.NewCounter("crier_guard_all_providers_failed_total",
		"LLM message-guard checks whose entire provider fallback chain failed (ErrAllProvidersFailed), including the no-provider-api-key case.")
)

// ProviderNames returns the bounded label domain of
// crier_guard_provider_circuit_open: every built-in preset plus "custom", the
// one policy-supplied endpoint class the router accepts. Sorted so the
// exposition is deterministic and every provider renders a 0 even when the
// guard is disabled or nothing has tripped.
func ProviderNames() []string {
	names := make([]string, 0, len(Presets)+1)
	for name := range Presets {
		names = append(names, name)
	}
	names = append(names, "custom")
	sort.Strings(names)
	return names
}
