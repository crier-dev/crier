package main

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/mesh"
	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/relay"

	"github.com/gorilla/mux"
)

// DF-CRIER-142: crier's two opt-in live-inspection surfaces. Both are
// registered ONLY when their env flag is on (CR_ENABLE_PPROF /
// CR_ENABLE_METRICS, default false → the paths answer 404 like any
// unregistered route). Neither path is added to the auth-exempt list in
// internal/middleware/auth.go: with CR_AUTH_TOKEN set they require the
// Bearer header like every other authenticated route; with auth off they
// are open. All handlers are net/http/pprof constructors wired onto the
// existing gorilla mux — no blank import, so http.DefaultServeMux is never
// hijacked.

// httpRequestsTotal counts every request through the mux by response status
// (DF-CRIER-142). Registered at package scope so the series exists from the
// first scrape; the recording middleware itself is installed only when the
// metrics surface is enabled.
var httpRequestsTotal = metrics.Default.NewCounterVec("http_requests_total",
	"HTTP requests processed by the server, by response status code.", "code")

// registerObservability mounts /metrics and /debug/pprof/* on r per the
// flags, and installs the request-counting middleware around the mux. It is
// called once per boot from run(); every registration is idempotent, so
// repeated boots in one process (tests) never double-register series.
func registerObservability(r *mux.Router, cfg observabilityFlags, deps observabilityDeps) http.Handler {
	registerObservabilityGauges(deps)
	var h http.Handler = r
	if cfg.EnableMetrics {
		r.Handle("/metrics", metrics.Handler())
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
// hold may be nil (no links configured — depth is then constantly 0).
type observabilityDeps struct {
	relaySvc *relay.Relay
	meshSvc  *mesh.Mesh
	fedHold  *federation.HoldManager
}

// registerObservabilityGauges registers the composition-layer gauges whose
// values live on objects assembled in run(): the federation hold-queue
// depth and the live WebSocket subscriber count (relay topic subscribers +
// connected mesh peers, summed — HELP says so). Re-registering replaces the
// previous fn, so a second boot in the same process rebinds to the new
// objects.
func registerObservabilityGauges(deps observabilityDeps) {
	metrics.Default.RegisterGaugeFunc("federation_held_current",
		"Federated deliveries currently held at this relay awaiting link recovery (0 when no federation links are configured).",
		func() float64 {
			if deps.fedHold == nil {
				return 0
			}
			return float64(deps.fedHold.Pending())
		})
	metrics.Default.RegisterGaugeFunc("ws_subscribers",
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

// countRequests wraps the mux with the http_requests_total recorder: every
// response, whatever its status (including 404s the mux itself produces),
// increments the counter under its status code. Installed only when the
// metrics surface is enabled.
func countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		httpRequestsTotal.With(strconv.Itoa(rec.status)).Inc()
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
