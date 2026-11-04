package guard

import (
	"sort"
	"strconv"
	"time"

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

// MON-CRIER-002 / observability-spec §3.3 (R2):
// crier_guard_check_duration_seconds{decision,errored} — is the LLM guard
// slowing ingest, or failing open? The pair is already computed for the §7.3
// audit line; this is the same verdict rendered as a distribution, recorded in
// audit() so the series and the log line can never disagree.
//
// Both labels are closed enums: `decision` is the guard's own Decision
// vocabulary (allow|block|sanitize) and `errored` is the Result.Errored flag
// spelled "true"/"false" — the exact pair the decision line carries. Nothing
// here is free text: no model name, no provider, no prompt, no error string.
//
// Every (decision, errored) child is seeded at 0, so a scrape of a booted
// server shows the whole bounded domain — including the two sanitize arms and
// the errored allow that a fail-open produces — before any check happens.
var guardCheckDuration = metrics.Default.NewHistogramVec(
	"crier_guard_check_duration_seconds",
	"LLM message-guard check duration in seconds, by decision (allow|block|sanitize) and whether the check errored (fail-open/fail-closed).",
	metrics.DefaultBuckets, "decision", "errored")

// guardDecisions is the closed decision domain of the histogram's label.
var guardDecisions = []Decision{DecisionAllow, DecisionBlock, DecisionSanitize}

// init seeds the bounded (decision, errored) domain at 0.
func init() {
	for _, decision := range guardDecisions {
		for _, errored := range []string{"false", "true"} {
			guardCheckDuration.With(string(decision), errored)
		}
	}
}

// observeCheckDuration records one completed check. decision is clamped to the
// Decision vocabulary: a future decision value must not be able to smuggle
// free text into a label, so anything unrecognised renders as "other" (absent
// today — the enum has exactly three members).
func observeCheckDuration(decision Decision, errored bool, elapsed time.Duration) {
	label := "other"
	for _, d := range guardDecisions {
		if d == decision {
			label = string(d)
			break
		}
	}
	d := elapsed
	if d < 0 {
		d = 0
	}
	guardCheckDuration.Observe(d.Seconds(), label, strconv.FormatBool(errored))
}

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
