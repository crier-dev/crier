// metrics_state.go — the composition-layer gauges whose values live INSIDE a
// subsystem object rather than in the store (MON-CRIER-004, observability-spec
// §3.3 rows G5 / G7 / G8):
//
//	G5  crier_webhook_queue_depth, crier_webhook_queue_oldest_age_seconds
//	    (the retry/backoff backlog was visible only in log lines)
//	G7  crier_detect_quarantined_current
//	    (alerts and quarantines were pullable via GET /alerts, never trended)
//
// The two counters of this row live with the code that increments them, per the
// ownership rule in internal/metrics: crier_webhook_attempts_total in
// internal/webhook (incremented at the driver's POST sites) and
// crier_detect_alerts_total in internal/detect (incremented at raise()).
// crier_store_queue_depth_read_errors_total is declared in observability.go,
// beside the NaN path that increments it.
//
// Registration is idempotent (RegisterGaugeFunc replaces the previous fn), so a
// second boot in one process rebinds to the new objects instead of rendering
// duplicates. A nil object — no webhook driver, no federation links, detection
// disabled — renders 0, never NaN and never a panic: these gauges measure the
// backlog of a subsystem, and "the subsystem is not configured" IS an empty
// backlog. That is deliberately the opposite of the inbox_queue_* rule, where a
// zero would be indistinguishable from an empty queue on a store that failed to
// answer.
package main

import (
	"github.com/crier-dev/crier/internal/detect"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/webhook"
)

// registerStateGauges registers every subsystem-state gauge. It is called once
// per boot from registerObservabilityGauges.
func registerStateGauges(deps observabilityDeps) {
	registerWebhookBacklogGauges(deps.webhookDriver)
	registerFederationHoldAgeGauge(deps.fedHold)
	registerDetectGauges(deps.detector)
}

// registerWebhookBacklogGauges exposes the driver's retry/backoff queue (G5):
// how much outbound push work is waiting, and how stale the oldest piece of it
// is. Together they separate "the endpoint is down" (depth climbing, age
// climbing) from "the endpoint is slow" (depth flat, age bounded), which the
// outcome counter alone cannot say.
func registerWebhookBacklogGauges(d *webhook.Driver) {
	metrics.Default.RegisterGaugeFunc("crier_webhook_queue_depth",
		"Webhook deliveries waiting in the retry/backoff queue for redelivery (0 when nothing is queued).",
		func() float64 {
			if d == nil {
				return 0
			}
			return float64(d.QueueDepth())
		})
	metrics.Default.RegisterGaugeFunc("crier_webhook_queue_oldest_age_seconds",
		"Age in seconds of the oldest delivery waiting in the webhook retry/backoff queue (0 when the queue is empty).",
		func() float64 {
			if d == nil {
				return 0
			}
			return d.OldestAge().Seconds()
		})
}

// registerFederationHoldAgeGauge adds the AGE of the hold queue beside the
// existing depth gauge (G10 in the audit; crier_federation_held_age_seconds in
// §3.3): "holding 40 messages for 20 minutes" and "holding 40 for 20 seconds"
// render identically on a depth-only surface. The legacy unprefixed
// federation_held_current is left exactly as registered — this row adds a
// series, it renames nothing.
func registerFederationHoldAgeGauge(m *federation.HoldManager) {
	metrics.Default.RegisterGaugeFunc("crier_federation_held_age_seconds",
		"Age in seconds of the oldest federated delivery held at this relay awaiting link recovery (0 when nothing is held or no federation links are configured).",
		func() float64 {
			if m == nil {
				return 0
			}
			return m.OldestHeldAge().Seconds()
		})
}

// registerDetectGauges exposes the detection layer's containment footprint (G7).
// The alert RATE is the counter in internal/detect; this gauge answers the
// current-state question ("how many agents are contained right now"), which a
// counter cannot.
func registerDetectGauges(d *detect.Detector) {
	metrics.Default.RegisterGaugeFunc("crier_detect_quarantined_current",
		"Agents currently contained (quarantined) by the detection layer (0 when detection is disabled).",
		func() float64 {
			if d == nil {
				return 0
			}
			return float64(d.QuarantinedCount())
		})
}
