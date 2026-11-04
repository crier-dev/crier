package main

import (
	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/metrics"
)

// MON-CRIER-003 (docs/observability-spec.md §2.4 G6, §3.3 R3): the LLM
// message guard fails OPEN. A saturated concurrency cap and an open provider
// circuit both end in a delivered message with nothing in /metrics to show
// why, so the guard's degradation was invisible from the outside. These two
// gauges are that visibility; the two fail-open counters
// (crier_guard_saturated_total, crier_guard_all_providers_failed_total) are
// owned and incremented by internal/guard itself.
//
// Both gauges render 0 — never NaN — when the guard is disabled or unwired:
// unlike the inbox-queue gauges there is no "unknown" state to report here,
// the guard simply is not inspecting anything. The HELP text says so, because
// a bare 0 would otherwise read as "circuit closed, nothing in flight".

// guardMetricsReader is the live guard state the gauges read. A nil value
// (guard disabled) is handled by the callbacks, which render the disabled
// posture instead of a fabricated series value.
type guardMetricsReader interface {
	Inflight() int
	OpenProviders() map[string]bool
}

// registerGuardGauges wires the guard's circuit and in-flight state into the
// exposition. Called once per boot from registerObservabilityGauges; the
// registry replaces both callbacks on a re-boot, so repeated boots in one
// process (tests) never duplicate the series.
func registerGuardGauges(g guardMetricsReader) {
	metrics.Default.RegisterGaugeFunc("crier_guard_inflight",
		"LLM message-guard checks currently holding the concurrency semaphore (cap = CR_GUARD_MAX_CONCURRENT, default 8). Excludes checks that failed open on the 2s acquire budget (see crier_guard_saturated_total). 0 when the guard is disabled.",
		func() float64 {
			if g == nil {
				return 0
			}
			return float64(g.Inflight())
		})

	// One sample per bounded provider label value: the built-in preset enum
	// plus "custom" (a policy-supplied endpoint class). Never a URL, model or
	// agent label — the circuit key stays inside the process.
	metrics.Default.RegisterGaugeFuncLabel("crier_guard_provider_circuit_open",
		"1 while the named provider's circuit breaker is open (after CR_GUARD_CIRCUIT_THRESHOLD consecutive failures, for CR_GUARD_CIRCUIT_COOLDOWN_S), 0 while closed. Providers the guard is not using, and every provider when the guard is disabled, render 0.",
		"provider",
		func() map[string]float64 {
			samples := make(map[string]float64, len(guard.ProviderNames()))
			for _, name := range guard.ProviderNames() {
				samples[name] = 0
			}
			if g == nil {
				return samples
			}
			for name, open := range g.OpenProviders() {
				if _, known := samples[name]; !known {
					continue // unbounded label values never reach the exposition
				}
				if open {
					samples[name] = 1
				}
			}
			return samples
		})
}
