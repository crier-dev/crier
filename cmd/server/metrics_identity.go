// metrics_identity.go — the identity/health series of the observability
// contract (docs/observability-spec.md §3.2, MON-CRIER-001).
//
// Four series, and no more than four: they answer the questions the previous
// 18 could not — is this target alive at all (crier_up), which build is
// answering (crier_build_info), is the serving store answering right now
// (crier_store_healthy), and when did the process start (uptime/restart
// detection via crier_start_time_seconds). Everything is registered on
// metrics.Default from registerIdentityGauges, called once per boot by
// registerObservabilityGauges, and every registration is idempotent (the
// registry replaces a same-name gauge), so a second in-process boot rebinds
// the callback rather than duplicating the series.
//
// Labels stay bounded by construction: the build identity is a per-release
// constant and the backend is the single resolved store name — no agent id,
// message id, request id, path or URL ever reaches a label (§3.4).
package main

import (
	"math"
	"strconv"
	"time"

	"github.com/crier-dev/crier/internal/buildinfo"
	"github.com/crier-dev/crier/internal/metrics"
)

// processStart is the process-lifetime start instant, captured when this
// package initializes — i.e. before run() reads any configuration, so it is
// the earliest instant this binary can honestly call "start". It is not a
// wall-clock timestamp of the systemd unit or of a container start (neither
// is visible from inside the process); on a restart the value moves forward,
// which is exactly what `time() - crier_start_time_seconds` and restart
// detection need.
var processStart = time.Now()

// storeHealthyBackendOther buckets any backend name outside the documented
// store enum, so the label domain is closed even for a hand-built Config or an
// operator value the resolver passed through.
const storeHealthyBackendOther = "other"

// storeHealthyBackendLabel bounds the backend label to the documented store
// enum (memory|sqlite|postgres — cmd/server/status.go names the first and
// last, sqlite is selected in run(); docs/observability-spec.md §3.2 lists
// the same three).
func storeHealthyBackendLabel(backend string) string {
	switch backend {
	case "memory", "sqlite", "postgres":
		return backend
	default:
		return storeHealthyBackendOther
	}
}

// registerIdentityGauges registers the §3.2 identity/health series. It reads
// the boot's own observabilityDeps: storeHealth is the same HealthReporter
// probe GET /status grades with, storeBackend is the backend run() selected,
// and queueDepth is the reader the inbox_queue_* gauges already use — no new
// store abstraction is introduced for the health bit.
func registerIdentityGauges(deps observabilityDeps) {
	// crier_up: constant 1 while the process is serving. Prometheus derives
	// "process gone" from the ABSENCE of this series on a failed scrape, so a
	// stale zero would be worse than useless.
	metrics.Default.RegisterGaugeFunc("crier_up",
		"Always 1 while the crier process is serving. Absence on a scrape means the process is gone; a 0 would be a lie this endpoint can never tell about itself.",
		func() float64 { return 1 })

	// crier_build_info: the standard *_build_info pattern. The label values
	// are the build-time identity internal/buildinfo already resolves for
	// GET /version — one identity source, so the metric and the route cannot
	// disagree (DF-CRIER-171).
	info := buildinfo.Resolve()
	metrics.Default.RegisterGaugeFuncVec("crier_build_info",
		"Build identity of the running binary (value is always 1): version, commit and whether the working tree was dirty at build time, resolved from the same internal/buildinfo source as GET /version.",
		[]string{"version", "commit", "dirty"},
		[]string{info.Version, info.Commit, strconv.FormatBool(info.Modified)},
		func() float64 { return 1 })

	// crier_start_time_seconds: unix seconds of process start. Uptime is
	// time() - this value; the value moving forward is a restart.
	start := float64(processStart.Unix())
	metrics.Default.RegisterGaugeFunc("crier_start_time_seconds",
		"Unix timestamp (seconds) at which this crier process started. Uptime is time() - crier_start_time_seconds; a larger value means the process restarted.",
		func() float64 { return start })

	// crier_store_healthy{backend}: 1 when the serving store answers, 0 when a
	// probe answered "unhealthy", NaN when nothing in this layer can probe it
	// (the absence convention the inbox_queue_* gauges already use — a
	// fabricated 0 would read as a degraded store that nobody measured).
	metrics.Default.RegisterGaugeFuncVec("crier_store_healthy",
		"Whether the store serving this process answers a live health probe (1 = healthy, 0 = answered unhealthy, NaN = this backend exposes no probe this layer can consult). Backend is the resolved registry backend.",
		[]string{"backend"},
		[]string{storeHealthyBackendLabel(deps.storeBackend)},
		func() float64 { return storeHealthyValue(deps.storeHealth, deps.queueDepth) })
}

// storeHealthyValue grades the serving store: the HealthReporter probe first
// (the same one GET /status uses, so the metric and the status word cannot
// disagree), then — for a backend that exposes no HealthReporter, e.g. a
// remote proxy or a test double — a successful queue-depth read, which is the
// only live store call this composition layer already makes. Neither
// available: NaN, never a made-up zero.
func storeHealthyValue(probe healthProbe, read queueDepthReader) float64 {
	if probe != nil {
		if healthy, known := probe(); known {
			if healthy {
				return 1
			}
			return 0
		}
	}
	if read != nil {
		if _, ok := read(); ok {
			return 1
		}
		return 0
	}
	return math.NaN()
}
