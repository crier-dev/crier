// observability_namespace_test.go — MON-CRIER-005 acceptance evidence.
//
// ch:trace row=MON-CRIER-005 spec=docs/observability-spec.md#3.1 wave=.coding-hermes/waves/crier-foreman-2026-11-04-13-00-50.json test=internal/metrics/metrics_test.go::TestAliasRendersSameSamplesUnderBothNames doc=README.md#observability prompt=none:worker-brief evidence="curl -s http://127.0.0.1:18767/metrics | grep -cE '^crier_'" witness=none:not-pushed verdict=none:pending-tier2 commit=none:worker memory=none:worker
//
// The row is about what /metrics SERVES, so these tests drive the real
// exposition over HTTP rather than asserting registry internals:
//
//   - every legacy family keeps emitting while a namespaced crier_* twin emits
//     the same samples (dual-emit window, observability-spec §3.1);
//   - http_requests_total and its twin are labelled by the mux route TEMPLATE,
//     so `sum by (route) (rate(crier_http_requests_total{code=~"5.."}[5m]))`
//     answers "which route is erroring" (A3), and a request the router never
//     matched carries the single bounded value "unmatched" — never a raw path.
package main

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// familySamples parses every line of one family into
// "the text between the name and the value" -> value.
//
// The key is the rendered label block verbatim (including the braces and the
// empty string for an unlabelled family), so a test can compare the legacy and
// namespaced families of the same series for exact equality.
func familySamples(body, name string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, name) {
			continue
		}
		rest := line[len(name):]
		if rest != "" && !strings.HasPrefix(rest, "{") && !strings.HasPrefix(rest, " ") {
			continue // a longer family name (e.g. name_bucket for a histogram)
		}
		sep := strings.LastIndexByte(rest, ' ')
		if sep < 0 {
			continue
		}
		key := strings.TrimSpace(rest[:sep])
		v, err := strconv.ParseFloat(rest[sep+1:], 64)
		if err != nil {
			continue
		}
		out[key] = v
	}
	return out
}

// routeLabels returns the set of route label values used by the HTTP request
// counters (legacy and namespaced) in the exposition.
func routeLabels(body string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "http_requests_total{") &&
			!strings.HasPrefix(line, "crier_http_requests_total{") {
			continue
		}
		if !strings.Contains(line, `route="`) {
			continue
		}
		start := strings.Index(line, `route="`) + len(`route="`)
		end := strings.Index(line[start:], `"`)
		if end < 0 {
			continue
		}
		out[line[start:start+end]] = true
	}
	return out
}

// TestMetricsNamespacedDualEmitAndRouteLabel is A1 and A3 of MON-CRIER-005
// against a served /metrics: the namespaced families exist, they carry the same
// samples as their legacy twins, and the HTTP counter is labelled by route.
func TestMetricsNamespacedDualEmitAndRouteLabel(t *testing.T) {
	r := mux.NewRouter()
	r.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods(http.MethodGet)
	r.HandleFunc("/boom", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}).Methods(http.MethodGet)

	srv := httptest.NewServer(registerObservability(r, observabilityFlags{EnableMetrics: true}, observabilityDeps{}))
	defer srv.Close()
	client := srv.Client()

	// Drive three distinct route shapes plus one that no route serves. The
	// agent id is deliberately distinctive so the test can prove it never
	// reaches a label.
	const agentID = "mon-crier-005-probe-agent"
	for _, path := range []string{
		"/health",
		"/boom",
		"/agents/" + agentID + "/inbox",
		"/no-such-route/" + agentID,
	} {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = resp.Body.Close()
	}

	body := scrapeNamespacedMetrics(t, client, srv.URL)

	// A1: the namespaced families are served. The row's acceptance greps for
	// 18 `^crier_` families; this asserts the dual-emit half of that (the
	// families that predate MON-CRIER-001 are all present under crier_*).
	legacyToNamespaced := [][2]string{
		{"deliveries_total", "crier_delivery_accepted_total"},
		{"guard_decisions_total", "crier_guard_decisions_total"},
		{"webhook_deliveries_total", "crier_webhook_deliveries_total"},
		{"capability_routed_total", "crier_capability_routed_total"},
		{"capability_unheld_total", "crier_capability_unheld_total"},
		{"expired_messages_total", "crier_inbox_expired_total"},
		{"dead_lettered_messages_total", "crier_inbox_deadlettered_total"},
		{"expiry_receipts_total", "crier_inbox_expiry_receipts_total"},
		{"idempotent_replays_total", "crier_delivery_idempotent_replay_total"},
		{"transfers_total", "crier_inbox_transfers_total"},
		{"relay_events_total", "crier_relay_events_total"},
		{"http_requests_total", "crier_http_requests_total"},
		{"federation_held_current", "crier_federation_held_current"},
		{"ws_subscribers", "crier_ws_subscribers"},
		{"inbox_queue_depth", "crier_inbox_queue_depth"},
		{"inbox_queue_leased", "crier_inbox_queue_leased"},
		{"inbox_queue_oldest_age_seconds", "crier_inbox_queue_oldest_age_seconds"},
		{"inbox_shed_total", "crier_inbox_shed_total"},
	}
	for _, pair := range legacyToNamespaced {
		legacy, namespaced := pair[0], pair[1]
		if !strings.Contains(body, "# TYPE "+namespaced+" ") {
			t.Errorf("namespaced family %q missing from /metrics (A1)", namespaced)
			continue
		}
		want, got := familySamples(body, legacy), familySamples(body, namespaced)
		if len(want) != len(got) {
			t.Errorf("%s has %d samples, %s has %d — the twin must mirror every sample",
				legacy, len(want), namespaced, len(got))
			continue
		}
		for labels, v := range want {
			if got[labels] != v && !(math.IsNaN(v) && math.IsNaN(got[labels])) {
				t.Errorf("%s%s = %v but its twin %s%s = %v",
					legacy, labels, v, namespaced, labels, got[labels])
			}
		}
	}

	// A3: the route label exists on both the legacy and the namespaced HTTP
	// counter, it is the path TEMPLATE, and the per-route 5xx rate is
	// computable — that is exactly what the PromQL in the row asks for.
	namespacedHTTP := familySamples(body, "crier_http_requests_total")
	legacyHTTP := familySamples(body, "http_requests_total")
	if len(namespacedHTTP) == 0 || len(legacyHTTP) == 0 {
		t.Fatalf("http request counter has no samples:\n%s", body)
	}
	for _, want := range []string{
		`{code="200",route="/health"}`,
		`{code="200",route="/agents/{id}/inbox"}`,
		`{code="503",route="/boom"}`,
		`{code="404",route="unmatched"}`,
	} {
		if _, ok := namespacedHTTP[want]; !ok {
			t.Errorf("crier_http_requests_total sample %s missing:\n%s", want, body)
		}
		if _, ok := legacyHTTP[want]; !ok {
			t.Errorf("legacy http_requests_total sample %s missing (dual-emit):\n%s", want, body)
		}
	}

	// The 5xx series is grouped per route and is positive for the erroring
	// route: `sum by (route) (rate(crier_http_requests_total{code=~"5.."}[5m]))`
	// therefore returns a per-route error rate, not one global number.
	perRouteErrors := map[string]float64{}
	for labels, v := range namespacedHTTP {
		if !strings.Contains(labels, `code="5`) {
			continue
		}
		start := strings.Index(labels, `route="`) + len(`route="`)
		end := strings.Index(labels[start:], `"`)
		if end < 0 {
			continue
		}
		perRouteErrors[labels[start:start+end]] += v
	}
	if got := perRouteErrors["/boom"]; got < 1 {
		t.Errorf("per-route 5xx grouping: route=%q has %v, want >= 1 (routes=%v)", "/boom", got, perRouteErrors)
	}

	// Bounded label domain: every route value is a mux template, a literal
	// route this server registers, or the single "unmatched" fallback — never
	// a raw path, URL or query, and never an unbounded id.
	routes := routeLabels(body)
	if len(routes) > 100 {
		t.Errorf("route label has %d distinct values — it must be bounded by the route table (~66)", len(routes))
	}
	for route := range routes {
		if strings.Contains(route, agentID) {
			t.Errorf("route label leaked a raw path element: %q", route)
		}
		if route == "" || (!strings.HasPrefix(route, "/") && route != routeLabelUnmatched) {
			t.Errorf("route label %q is neither a path template nor %q", route, routeLabelUnmatched)
		}
	}
	if !routes[routeLabelUnmatched] {
		t.Errorf("no sample carries the bounded %q route for the unmatched request", routeLabelUnmatched)
	}
}

// scrapeNamespacedMetrics GETs /metrics from the live server and returns the
// body. (Named around the package's existing scrapeMetrics helper.)
func scrapeNamespacedMetrics(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	resp, err := client.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want 200", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	return string(b)
}
