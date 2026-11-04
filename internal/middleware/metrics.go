package middleware

import (
	"net/http"
	"strconv"

	"github.com/crier-dev/crier/internal/metrics"

	"github.com/gorilla/mux"
)

// MON-CRIER-002 / observability-spec §3.3 (R2): crier recorded request
// latency ONLY as a slog attribute ("duration", middleware.go), so the
// exposition could not answer "which route is slow" or "is the p99 rising".
// This file adds the distribution beside that log line, emitted by the same
// Logging middleware that measures it — one measurement, two surfaces.
//
// THE ROUTE LABEL. The label MUST be the mux TEMPLATE (/agents/{id}/inbox),
// never the request path: the path carries agent ids and message ids, which
// are unbounded by definition (spec §3.4). Logging is registered with
// r.Use(...), so gorilla/mux composes it INSIDE Router.Match and invokes it
// with the request it has already enriched (requestWithRoute). That is what
// makes mux.CurrentRoute(r) — and therefore the template — readable here.
// http.Request.Pattern is NOT usable for this: it is net/http's own field and
// gorilla/mux never sets it.
//
// Two honest consequences, both deliberate:
//
//   - An UNMATCHED request (the router's 404/405 fallbacks, which wrap
//     Logging themselves in cmd/server/fallback.go) has no current route, so
//     it collapses into the single bounded value "other". The literal path
//     never becomes a label.
//   - A request whose route has no path template (a prefix or custom
//     matcher) collapses into "other" for the same reason. Every route this
//     server registers is a path template today.
//
// The method label is clamped to the seven standard methods: a client can
// send any token in the request line, and an unclamped label would let a
// single caller grow the exposition without bound. The code label is the
// three-digit status net/http allows, and nothing else.

// otherRoute is the single bounded label value every request whose mux
// template is unknown collapses into: the router's own 404/405 answers, and
// any handler chain mounted outside the mux.
const otherRoute = "other"

// requestDuration is the R2 request-latency distribution. The bucket set is
// the spec's fixed twelve (metrics.DefaultBuckets), shared by every child —
// no per-request allocation.
var requestDuration = metrics.Default.NewHistogramVec(
	"crier_http_request_duration_seconds",
	"HTTP request duration in seconds, by mux route template, request method and response status code (route=\"other\" = no template matched).",
	metrics.DefaultBuckets, "route", "method", "code")

// init seeds ONE child so the histogram family is never absent from the first
// scrape of a fresh process: without a child a HistogramVec renders only its
// HELP/TYPE lines, and an operator would have to guess whether the series is
// missing or simply idle. The seeded labels are the canonical unmatched
// request (a GET to a path the router does not serve answers 404 through the
// fallback, which records into exactly this child), so the seed is the same
// series real traffic fills — it fabricates no observation, it pre-creates a
// zero.
func init() {
	requestDuration.With(otherRoute, http.MethodGet, strconv.Itoa(http.StatusNotFound))
}

// recordRequestDuration observes one finished request. It is called from
// Logging's deferred block, so the panicking path records too (the client is
// served the 500 Recovery writes).
func recordRequestDuration(r *http.Request, status int, seconds float64) {
	requestDuration.Observe(seconds, routeTemplate(r), methodLabel(r.Method), strconv.Itoa(status))
}

// routeTemplate renders the bounded `route` label: the mux path template that
// served the request, or otherRoute. It never returns a raw path — no query
// string, no agent id, no message id.
func routeTemplate(r *http.Request) string {
	if rt := mux.CurrentRoute(r); rt != nil {
		tpl, err := rt.GetPathTemplate()
		if err == nil && tpl != "" {
			return tpl
		}
	}
	return otherRoute
}

// methodLabel bounds the `method` label to the standard method set. Anything
// else — a typo, a probe, a deliberately weird token — is "OTHER": closed
// enum, never the caller's bytes.
func methodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return method
	}
	return "OTHER"
}
