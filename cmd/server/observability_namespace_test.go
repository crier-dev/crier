// observability_namespace_test.go — MON-CRIER-007 acceptance evidence.
//
// ch:trace row=MON-CRIER-007 spec=docs/observability-spec.md#3.1 test=cmd/server/observability_namespace_test.go::TestMetricsLegacyFamiliesRetired doc=README.md#observability-metrics--profiling evidence="curl -s http://127.0.0.1:<port>/metrics" prompt=none:worker-brief witness=none:not-pushed verdict=none:pending-tier2 commit=none:worker memory=none:worker
//
// MON-CRIER-005 opened a dual-emit window: every family was served twice, under
// its pre-rename bare name and under its crier_* name. MON-CRIER-007 closed it,
// so /metrics must now serve the namespaced name ONLY. This is the row's
// headline regression: it renders the REAL default registry (this package's
// binary links every production registration site — registry, relay, webhook,
// cmd/server itself) through the served exposition and fails if any retired
// bare family comes back, whether as a real registration or as a revived
// alias.
//
// It also keeps the MON-CRIER-005 route-label proof: crier_http_requests_total
// is labelled by the mux route TEMPLATE, so
// `sum by (route) (rate(crier_http_requests_total{code=~"5.."}[5m]))` answers
// "which route is erroring" (A3), and a request the router never matched
// carries the single bounded value "unmatched" — never a raw path.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// retiredFamily pairs a pre-rename (MON-CRIER-005 legacy) family name with the
// namespaced name that replaced it. The legacy half is RETIRED: it must not be
// served at all.
type retiredFamily struct {
	retired    string
	namespaced string
}

// retiredFamilies is the full 18-pair rename map from the README §Observability
// table. After MON-CRIER-007 only the right-hand column exists.
var retiredFamilies = []retiredFamily{
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

// familySamples parses every line of one family into
// "the text between the name and the value" -> value.
//
// The key is the rendered label block verbatim (including the braces and the
// empty string for an unlabelled family).
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

// servedFamilyNames returns every family name an exposition serves: the name on
// each sample line and on each HELP/TYPE metadata line. This is the whole served
// surface, so a retired name is caught whether it came back as a real
// registration, as an alias, or as a renamed sample line.
func servedFamilyNames(body string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "# HELP "):
			line = strings.TrimPrefix(line, "# HELP ")
		case strings.HasPrefix(line, "# TYPE "):
			line = strings.TrimPrefix(line, "# TYPE ")
		case strings.HasPrefix(line, "#"), line == "":
			continue
		}
		end := strings.IndexAny(line, "{ ")
		if end < 0 {
			continue
		}
		out[line[:end]] = true
	}
	return out
}

// routeLabels returns the set of route label values used by the namespaced HTTP
// request counter in the exposition.
func routeLabels(body string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "crier_http_requests_total{") {
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

// TestMetricsLegacyFamiliesRetired is MON-CRIER-007's headline criterion against
// a served /metrics: the 18 namespaced families are there, and none of the 18
// retired bare names is — not as a sample line, not as a HELP/TYPE family.
func TestMetricsLegacyFamiliesRetired(t *testing.T) {
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
	const agentID = "mon-crier-007-probe-agent"
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
	served := servedFamilyNames(body)

	for _, pair := range retiredFamilies {
		// The namespaced name must exist — the rename kept every series.
		if !strings.Contains(body, "# TYPE "+pair.namespaced+" ") {
			t.Errorf("namespaced family %q missing from /metrics", pair.namespaced)
		}
		// The retired bare name must NOT exist anywhere on the served surface.
		if served[pair.retired] {
			t.Errorf("retired legacy family %q is still served — the MON-CRIER-005 dual-emit window is not closed:\n%s",
				pair.retired, body)
		}
	}

	// The identity/health series the row's live proof also greps for.
	for _, want := range []string{"crier_up", "crier_store_healthy"} {
		if !served[want] {
			t.Errorf("family %q missing from /metrics", want)
		}
	}
	if len(familySamples(body, "crier_inbox_queue_depth")) != 1 {
		t.Errorf("crier_inbox_queue_depth must render exactly one sample:\n%s", body)
	}

	// A3: the route label exists on the namespaced HTTP counter, it is the path
	// TEMPLATE, and the per-route 5xx rate is computable.
	httpSamples := familySamples(body, "crier_http_requests_total")
	if len(httpSamples) == 0 {
		t.Fatalf("crier_http_requests_total has no samples:\n%s", body)
	}
	for _, want := range []string{
		`{code="200",route="/health"}`,
		`{code="200",route="/agents/{id}/inbox"}`,
		`{code="503",route="/boom"}`,
		`{code="404",route="unmatched"}`,
	} {
		if _, ok := httpSamples[want]; !ok {
			t.Errorf("crier_http_requests_total sample %s missing:\n%s", want, body)
		}
	}

	// The 5xx series is grouped per route and is positive for the erroring
	// route: `sum by (route) (rate(crier_http_requests_total{code=~"5.."}[5m]))`
	// therefore returns a per-route error rate, not one global number.
	perRouteErrors := map[string]float64{}
	for labels, v := range httpSamples {
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
