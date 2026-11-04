package webhook

import (
	"time"

	"github.com/crier-dev/crier/internal/metrics"
)

// MON-CRIER-002 / observability-spec §3.3 (R2):
// crier_webhook_delivery_duration_seconds{outcome} — how long a push takes to
// reach a TERMINAL state, retries included.
//
// It is the distribution complement of webhookDeliveriesTotal: the counter
// says how many deliveries ended each way, this says how long they took. The
// clock starts at the ACCEPT (the moment the delivery was handed to the
// driver) and stops where the driver decides the outcome:
//
//   - delivered    — the endpoint answered 2xx;
//   - deadlettered — a permanent (4xx) rejection, or a 2xx whose body could
//     not be mapped to the reply schema;
//   - dropped      — the item left the queue without a successful POST (agent
//     paused / gone, or the retry budget exhausted);
//   - failed       — the delivery ended a failed state without being queued
//     again (a blocking delivery that exhausted its caller budget or was
//     cancelled; the blocking path retries inside one call, so it records ONE
//     observation for the whole call, not one per attempt — the per-attempt
//     view is crier_webhook_attempts_total).
//
// A retryable failure that REQUEUES the item is not terminal and records
// nothing: the item's CreatedAt is preserved across retries (enqueue stamps
// the accept, requeueBatch keeps the oldest), so the single observation made
// when the item finally ends carries the full accept-to-terminal latency,
// backoff and flush-interval waits included.
//
// The `outcome` label is the driver's closed vocabulary — the same four values
// webhookDeliveriesTotal uses — and all four children are seeded at 0 so the
// series is present on a scrape before any delivery happens.
var webhookDeliveryDuration = metrics.Default.NewHistogramVec(
	"crier_webhook_delivery_duration_seconds",
	"Webhook delivery time to a terminal outcome in seconds, from accept through every retry (outcome=delivered|failed|deadlettered|dropped).",
	metrics.DefaultBuckets, "outcome")

// webhookOutcomes is the closed outcome domain. It is the label contract of
// both the duration histogram and webhookDeliveriesTotal.
var webhookOutcomes = []string{"delivered", "failed", "deadlettered", "dropped"}

// init seeds every outcome at 0: "no delivery ended this way yet" is a
// reading, not a scrape gap.
func init() {
	for _, outcome := range webhookOutcomes {
		webhookDeliveryDuration.With(outcome)
	}
}

// observeDeliveryDuration records one terminal delivery, measured from the
// accept stamp. A zero stamp (a caller-supplied QueueItem that never went
// through enqueue) is skipped rather than recorded as a fabricated age.
func observeDeliveryDuration(outcome string, accepted time.Time) {
	if accepted.IsZero() {
		return
	}
	d := time.Since(accepted)
	if d < 0 {
		d = 0 // a clock step backwards must not record a negative duration
	}
	webhookDeliveryDuration.Observe(d.Seconds(), outcome)
}
