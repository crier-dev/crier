package harness

import (
	"time"

	"github.com/crier-dev/crier/internal/metrics"
)

// The ingress → harness dispatch metrics (CRIER-OBS-1).
//
// Registry.Deliver is the one choke point every ingress push funnels through
// to reach an agent adapter, and until this file it emitted no series at all:
// a failed dispatch existed only as a log line plus the fields of its
// persisted Receipt. Three families make the path countable, and they are
// wired into Deliver's deferred close (registry.go) so that every return path
// — the refusals the body can take and the adapter's receipt alike — records
// exactly one sample per call.
//
// Label cardinality is bounded by construction. The harness label takes the
// adapter id (or the none label below); the state label is the closed pair
// {dispatched, failed}; the class label is the §5 error-code vocabulary plus
// the none label. Never a session id, message id or agent id — those would
// grow the exposition without bound and are already on the Receipt where they
// belong.
//
// A metric must never panic or block a delivery: every helper here only
// reads its arguments, does one bounded map lookup on a vec whose children
// are created on first use, and the vec itself drops (rather than panics on)
// a wrong-arity call — all in-tree call sites pass literals, so that path is
// unreachable.

// metricNone replaces an empty label value with a closed-domain word. A
// failed receipt can name no code (the registry normalises a missing state
// but not a missing Error), and a refusal before resolution (harness.enabled
// false with no harness named) can carry no adapter id; the exposition then
// renders harness="" / class="", which is legal in the text format but easy
// to misread as a missing value. The replacement follows the same convention
// as the §5 codes: a fixed word, not a blank.
const metricNone = "none"

// The closed state domain of crier_harness_dispatches_total. A receipt whose
// State is dispatched OR accepted counts as dispatched — both mean the
// harness has the message, the difference is whether a local process owned
// the turn. Everything else is failed, which is what Receipt.Failed means.
const (
	metricStateDispatched = "dispatched"
	metricStateFailed     = "failed"
)

// harnessDispatchesTotal counts harness dispatches by adapter and terminal
// state. state=dispatched is a receipt whose State is dispatched/accepted;
// state=failed is a failed receipt — the registry's own refusals
// (HARNESS_DISABLED, HARNESS_UNKNOWN, HARNESS_CONCURRENCY_FULL,
// HARNESS_UNAVAILABLE) and an adapter-reported failure alike, so the counter
// set is the audit trail of the whole Deliver call.
var harnessDispatchesTotal = metrics.Default.NewCounterVec(
	"crier_harness_dispatches_total",
	"Harness dispatches by adapter and terminal state (state=dispatched means the harness has the message, state=failed means the receipt says failed).",
	"harness", "state")

// harnessDispatchErrorsTotal counts the failed half of those dispatches by
// their error class: the Receipt.Error code — one of the §5 vocabulary words
// such as HARNESS_EXIT_NONZERO or HARNESS_DISABLED — so a failure is
// aggregable by cause, not only visible in a log line. It increments only
// for failed receipts; a success records nothing here.
var harnessDispatchErrorsTotal = metrics.Default.NewCounterVec(
	"crier_harness_dispatch_errors_total",
	"Failed harness dispatches by error class (the receipt's error code, e.g. HARNESS_EXIT_NONZERO; class=none is a failure that named no code).",
	"harness", "class")

// harnessDispatchDurationSeconds is the wall time of the whole Deliver call
// — from the registry's own start stamp to the deferred observation, so the
// registry's refusals are priced too — by adapter, over the shared default
// bucket set. Measured with the registry's clock (r.now()), so a test with an
// injected clock stays deterministic.
var harnessDispatchDuration = metrics.Default.NewHistogramVec(
	"crier_harness_dispatch_duration_seconds",
	"Wall time of one harness dispatch in seconds by adapter (the whole Registry.Deliver call, refusals included).",
	metrics.DefaultBuckets, "harness")

// observeDispatchMetrics records ONE completed Deliver call: the dispatch
// counter under (harness, state), the error counter only when the receipt
// failed, and one duration observation. It runs in Deliver's deferred close,
// which is what makes the exactly-once property structural — a future return
// path is counted by default, never by remembering to add a line. A receipt
// that names no state (only reachable if the body panicked) records nothing:
// an unlabelled half-delivery must not invent a sample.
func observeDispatchMetrics(elapsed time.Duration, rc Receipt) {
	if rc.State == "" {
		return
	}
	name := rc.Harness
	if name == "" {
		name = metricNone
	}
	state := metricStateFailed
	if rc.State == StateDispatched || rc.State == StateAccepted {
		state = metricStateDispatched
	}
	harnessDispatchesTotal.With(name, state).Inc()
	if rc.Failed() {
		class := rc.Error
		if class == "" {
			class = metricNone
		}
		harnessDispatchErrorsTotal.With(name, class).Inc()
	}
	// Observe clamps a negative elapsed (an inverted injected clock) to 0
	// and drops NaN, so a clock anomaly cannot poison the distribution.
	harnessDispatchDuration.Observe(elapsed.Seconds(), name)
}
