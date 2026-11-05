package registry

import (
	"time"

	"github.com/crier-dev/crier/internal/metrics"
)

// MON-CRIER-002 / observability-spec §3.3 (R2):
// crier_inbox_deliver_duration_seconds{transport} — what does delivering a
// message COST, split by the transport that accepted it:
//
//   - transport="inbox"   — the durable store write (and the wake-up ping that
//     follows it) on this relay;
//   - transport="webhook" — accepting the push: a blocking POST that waits for
//     the endpoint's answer, or a queue accept for the async/batch modes;
//   - transport="hermes-session" — one agent turn inside a Hermes gateway
//     session (create-if-needed + POST .../chat), measured across both calls.
//
// The measurement lives in Dispatch — the one choke point both delivery
// callers share (POST /agents/{id}/inbox through the HTTP handler, and
// Handler.DeliverComposed for an in-process caller) — and it starts at the
// delivery's OWN start stamp, so a caller that has already spent time decoding
// and authorizing is included in the cost rather than hidden by it.
//
// It records ONE observation per delivery, and only once a transport was
// chosen: a delivery refused before the branch (guard block, unknown agent,
// bad payload, idempotent replay) has no transport to attribute and stays out
// of the split rather than being filed under a transport it never used. Those
// refusals are counted by their own series, and their latency is visible in
// crier_http_request_duration_seconds.
//
// The `transport` label is a closed enum — {inbox, webhook} — and both
// children are seeded at 0 so the split exists before the first delivery.
var inboxDeliverDuration = metrics.Default.NewHistogramVec(
	"crier_inbox_deliver_duration_seconds",
	"Inbox delivery duration in seconds by accepting transport (transport=inbox is the durable store write, transport=webhook is the push accept).",
	metrics.DefaultBuckets, "transport")

// deliverTransports is the closed transport domain of that histogram.
var deliverTransports = []string{"inbox", "webhook", "hermes-session"}

// init seeds every transport at 0.
func init() {
	for _, transport := range deliverTransports {
		inboxDeliverDuration.With(transport)
	}
}

// observeDeliverDuration records one delivery's transport cost. The transport
// is looked up in the closed domain; anything else (including an empty
// result.Transport, i.e. no transport reached) records nothing, so no
// unbounded or invented label can ever reach the exposition.
func observeDeliverDuration(transport string, elapsed time.Duration) {
	known := false
	for _, t := range deliverTransports {
		if t == transport {
			known = true
			break
		}
	}
	if !known {
		return
	}
	if elapsed < 0 {
		elapsed = 0
	}
	inboxDeliverDuration.Observe(elapsed.Seconds(), transport)
}
