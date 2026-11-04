package main

import (
	"bufio"
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"

	"github.com/crier-dev/crier/internal/detect"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/mesh"
	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/relay"
	"github.com/crier-dev/crier/internal/webhook"

	"github.com/gorilla/mux"
)

// DF-CRIER-142: crier's two live-inspection surfaces. GET /metrics is
// registered by default (MON-CRIER-001; CR_ENABLE_METRICS=false turns it off)
// and GET /debug/pprof/* only when CR_ENABLE_PPROF is on — a disabled surface
// answers 404 like any unregistered route. Neither path is added to the
// auth-exempt list in
// internal/middleware/auth.go: with CR_AUTH_TOKEN set they require the
// Bearer header like every other authenticated route; with auth off they
// are open. All handlers are net/http/pprof constructors wired onto the
// existing gorilla mux — no blank import, so http.DefaultServeMux is never
// hijacked.

// httpRequestsTotal counts every request through the mux by response status
// code and by route (DF-CRIER-142; MON-CRIER-005 added the route label). The
// route label is the mux PATH TEMPLATE (/agents/{id}/inbox) — bounded by the
// route table — never the raw path, URL or query, which carry unbounded agent
// and message ids (observability-spec §3.4). A request the mux did not route
// (its 404/405 fallbacks, a path-clean redirect) carries the single bounded
// literal "unmatched", so a scrape can be told apart from real traffic and an
// operator can ask which route is erroring:
//
//	sum by (route) (rate(crier_http_requests_total{code=~"5.."}[5m]))
//
// Registered at package scope so the series exists from the first scrape; the
// recording middleware itself is installed only when the metrics surface is
// enabled.
var httpRequestsTotal = metrics.Default.NewCounterVec("crier_http_requests_total",
	"HTTP requests processed by the server, by response status code and mux route template (route=\"unmatched\" = no route matched).", "code", "route")

// routeLabelUnmatched is the single bounded `route` value for a request the mux
// never routed: no template exists to name, and the raw path must never become
// a label. (The request-duration histogram in internal/middleware spells the
// same case "other"; this counter uses the literal MON-CRIER-005 specifies.)
const routeLabelUnmatched = "unmatched"

// requestRouteKey is the context key under which countRequests hands the
// per-request route holder to captureRequestRoute.
type requestRouteKey struct{}

// requestRoute is the per-request holder the route is recorded into. One
// goroutine serves the request end to end, so the write in the middleware
// happens-before the read after ServeHTTP returns.
type requestRoute struct{ label string }

// captureRequestRoute is the r.Use middleware that records the matched route
// template. gorilla/mux composes r.Use middlewares INSIDE Router.Match and
// invokes them with the request it has already enriched (requestWithRoute), so
// mux.CurrentRoute(r) — and therefore GetPathTemplate() — is readable here and
// only here. It cannot be read by the outer countRequests wrapper: mux derives
// a NEW request for its handlers, so the wrapper's copy never sees the route.
func captureRequestRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if holder, ok := r.Context().Value(requestRouteKey{}).(*requestRoute); ok {
			holder.label = routeTemplate(r)
		}
		next.ServeHTTP(w, r)
	})
}

// routeTemplate renders the bounded `route` label: the mux path template that
// served the request, or routeLabelUnmatched. It never returns a raw path — no
// query string, no agent id, no message id.
func routeTemplate(r *http.Request) string {
	if rt := mux.CurrentRoute(r); rt != nil {
		if tpl, err := rt.GetPathTemplate(); err == nil && tpl != "" {
			return tpl
		}
	}
	return routeLabelUnmatched
}

// registerObservability mounts /metrics and /debug/pprof/* on r per the
// flags, and installs the request-counting middleware around the mux. It is
// called once per boot from run(); every registration is idempotent, so
// repeated boots in one process (tests) never double-register series.
func registerObservability(r *mux.Router, cfg observabilityFlags, deps observabilityDeps) http.Handler {
	registerObservabilityGauges(deps)
	var h http.Handler = r
	if cfg.EnableMetrics {
		r.Handle("/metrics", metrics.Handler())
		// The recorder needs the matched route, which only mux's own middleware
		// chain can see (see captureRequestRoute); the outer wrapper counts
		// everything, including the requests no route matched.
		r.Use(captureRequestRoute)
		h = countRequests(r)
		slog.Info("observability: metrics enabled", "path", "/metrics",
			"format", "prometheus text 0.0.4", "auth", "not exempt (requires Bearer when CR_AUTH_TOKEN is set)")
	}
	if cfg.EnablePProf {
		registerPProf(r)
		slog.Info("observability: pprof enabled", "path", "/debug/pprof/",
			"auth", "not exempt (requires Bearer when CR_AUTH_TOKEN is set)")
	}
	return h
}

// observabilityFlags is the subset of config.Config registerObservability
// needs — a struct so the wiring stays testable without env plumbing.
type observabilityFlags struct {
	EnableMetrics bool
	EnablePProf   bool
}

// observabilityDeps carries the live objects the gauges read. federation
// hold may be nil (no links configured — depth is then constantly 0), and
// queueDepth may be nil (the serving store cannot report a queue depth — the
// inbox-queue gauges are then NaN rather than a fabricated zero).
type observabilityDeps struct {
	relaySvc *relay.Relay
	meshSvc  *mesh.Mesh
	fedHold  *federation.HoldManager
	// webhookDriver is the live push driver (MON-CRIER-004): its retry/backoff
	// queue is the G5 backlog the depth/age gauges read. Always non-nil on a
	// booted server; nil renders 0.
	webhookDriver *webhook.Driver
	// detector is the live detection layer (MON-CRIER-004), nil when detection
	// is disabled — the quarantine gauge then renders 0.
	detector *detect.Detector
	// queueDepth reads the store-wide inbox queue (CR-FEAT-035). Wired from
	// the store that actually serves, so the gauges report the queue of the
	// backend answering — memory or postgres — and never a copied number.
	queueDepth queueDepthReader
	// storeBackend names the registry backend run() selected — the same value
	// GET /status advertises (memory|sqlite|postgres). It is the bounded
	// `backend` label on crier_store_healthy (MON-CRIER-001) and on the depth
	// read-error counter (MON-CRIER-004), so it is bounded by that enum —
	// never a DSN, path or URL.
	storeBackend string
	// storeHealth is the serving store's live health probe — the same seam
	// GET /status grades its health word with (nil when the backend exposes
	// no HealthReporter, which crier_store_healthy then grades through the
	// queue-depth read instead).
	storeHealth healthProbe
	// guardStats is the live LLM guard (MON-CRIER-003): nil when the guard
	// is disabled, in which case its gauges render 0 rather than a
	// fabricated circuit/inflight reading.
	guardStats guardMetricsReader
}

// registerObservabilityGauges registers the composition-layer gauges whose
// values live on objects assembled in run(): the federation hold-queue
// depth and the live WebSocket subscriber count (relay topic subscribers +
// connected mesh peers, summed — HELP says so). Re-registering replaces the
// previous fn, so a second boot in the same process rebinds to the new
// objects.
func registerObservabilityGauges(deps observabilityDeps) {
	metrics.Default.RegisterGaugeFunc("crier_federation_held_current",
		"Federated deliveries currently held at this relay awaiting link recovery (0 when no federation links are configured).",
		func() float64 {
			if deps.fedHold == nil {
				return 0
			}
			return float64(deps.fedHold.Pending())
		})
	metrics.Default.RegisterGaugeFunc("crier_ws_subscribers",
		"Live WebSocket subscribers: relay topic subscribers plus connected mesh peers, summed.",
		func() float64 {
			n := 0
			if deps.relaySvc != nil {
				n += deps.relaySvc.SubscriberCount()
			}
			if deps.meshSvc != nil {
				n += deps.meshSvc.ActivePeers()
			}
			return float64(n)
		})
	registerQueueDepthGauges(deps.queueDepth, deps.storeBackend)
	registerIdentityGauges(deps)
	// MON-CRIER-004: the subsystem-state series (webhook backlog, federation
	// hold age, detection containment).
	registerStateGauges(deps)
	// MON-CRIER-003: guard circuit/inflight fail-open state.
	registerGuardGauges(deps.guardStats)
}

// storeQueueDepthReadErrors counts scrapes where the serving store reported no
// inbox queue depth — exactly the reads that render inbox_queue_* as NaN
// (MON-CRIER-004, spec §3.3 / G8). NaN is honest, but an intermittently
// unreadable store produces gaps a rate cannot explain; this counter makes the
// gap countable. The backend label is bounded by depthBackendLabel.
var storeQueueDepthReadErrors = metrics.Default.NewCounterVec("crier_store_queue_depth_read_errors_total",
	"Scrapes where the serving store reported no inbox queue depth (the reads that render inbox_queue_* as NaN), by backend.", "backend")

// depthBackendLabel bounds the `backend` label of the read-error counter. It is
// one of the resolved store backends (memory|sqlite|postgres — the same value
// GET /status advertises) or:
//
//   - "proxy" — there is no depth reader at all: the serving store does not
//     implement registry.DepthReporter (the remote proxy). The NaN is then a
//     capability gap, permanently present, and must not be read as a flapping
//     store; the label is what tells the two apart.
//   - "unknown" — a reader with no resolved backend name (a hand-wired caller).
//     Still bounded, never a DSN, path or URL.
func depthBackendLabel(read queueDepthReader, backend string) string {
	if read == nil {
		return "proxy"
	}
	switch backend {
	case "memory", "sqlite", "postgres":
		return backend
	}
	return "unknown"
}

// registerQueueDepthGauges exposes the store-wide inbox queue (CR-FEAT-035):
// how many messages are waiting, how many of those are held under a live lease,
// and how old the oldest one is.
//
// A store that cannot report (a remote proxy — no depth route) makes all three
// gauges NaN, Prometheus's own spelling for "no value": a zero would read as an
// empty queue, which is the one reading an operator must never be handed by
// accident. NaN is valid in the text exposition format, so a scrape stays
// parseable either way.
//
// storeBackend names the backend behind read (memory|sqlite|postgres) and is
// only ever used as the bounded label of crier_store_queue_depth_read_errors_total
// (MON-CRIER-004), which is incremented ONCE per scrape that produced no value.
func registerQueueDepthGauges(read queueDepthReader, storeBackend string) {
	backend := depthBackendLabel(read, storeBackend)
	// Seed the child at 0 so the series exists on a healthy boot: a store that
	// answers every read never increments it, and an absent sample would read
	// as "this counter does not exist" (spec §3.3 asks for the gap to be
	// countable AND visible).
	storeQueueDepthReadErrors.With(backend)

	// readOnce performs one depth read. countFailure is true for exactly ONE of
	// the three gauges: the exposition evaluates each gauge separately, so
	// counting in all three would report three errors for one failed scrape.
	readOnce := func(countFailure bool) (registry.QueueDepth, bool) {
		if read == nil {
			if countFailure {
				storeQueueDepthReadErrors.With(backend).Inc()
			}
			return registry.QueueDepth{}, false
		}
		depth, ok := read()
		if !ok && countFailure {
			storeQueueDepthReadErrors.With(backend).Inc()
		}
		return depth, ok
	}
	sample := func(pick func(registry.QueueDepth) float64, countFailure bool) func() float64 {
		return func() float64 {
			depth, ok := readOnce(countFailure)
			if !ok {
				return math.NaN()
			}
			return pick(depth)
		}
	}

	metrics.Default.RegisterGaugeFunc("crier_inbox_queue_depth",
		"Inbox messages queued and unacknowledged across all agents (leased messages included), read from the serving store. NaN when the store cannot report a depth.",
		sample(func(d registry.QueueDepth) float64 { return float64(d.Pending) }, true))
	metrics.Default.RegisterGaugeFunc("crier_inbox_queue_leased",
		"How many of crier_inbox_queue_depth are currently held under a live lease. NaN when the store cannot report a depth.",
		sample(func(d registry.QueueDepth) float64 { return float64(d.Leased) }, false))
	metrics.Default.RegisterGaugeFunc("crier_inbox_queue_oldest_age_seconds",
		"Age of the oldest unacknowledged, unexpired inbox message, in seconds (0 when nothing is queued). NaN when the store cannot report a depth.",
		sample(func(d registry.QueueDepth) float64 { return d.OldestAge.Seconds() }, false))
}

// registerPProf mounts the net/http/pprof surface on the gorilla mux:
// the index plus the named profile endpoints. Handler constructors only
// (pprof.Index, pprof.Cmdline, … pprof.Handler(name)) — a blank
// net/http/pprof import would register everything on
// http.DefaultServeMux, which this server never serves.
func registerPProf(r *mux.Router) {
	r.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
	r.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
	r.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
	r.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
	r.Handle("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
	for _, name := range []string{"heap", "goroutine", "block", "mutex", "threadcreate"} {
		r.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}
}

// countRequests wraps the mux with the crier_http_requests_total recorder: every
// response, whatever its status (including 404s the mux itself produces),
// increments the counter under its status code and its matched route template.
// Installed only when the metrics surface is enabled.
//
// The route travels from captureRequestRoute (inside the mux, where the match
// is known) to here (outside it, where the status is final) through a request
// context value. A request that never reached a matched handler leaves the
// holder at routeLabelUnmatched — the bounded answer, never the raw path.
func countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := &requestRoute{label: routeLabelUnmatched}
		r = r.WithContext(context.WithValue(r.Context(), requestRouteKey{}, route))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		httpRequestsTotal.With(strconv.Itoa(rec.status), route.label).Inc()
	})
}

// statusRecorder captures the response status for the request counter. It
// forwards Hijack (WebSocket upgrades must pass through) and Flush, the two
// optional interfaces handlers in this server rely on.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Hijack lets gorilla/websocket take over the connection (same forwarding
// as middleware.responseWriter).
func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := sr.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Flush forwards explicit flushes (streaming responses).
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController find the underlying writer.
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }
