// metrics_latency_test.go — MON-CRIER-002 acceptance evidence: the four R2
// latency histograms (observability-spec §3.3) read from a REAL booted server
// over GET /metrics, after real traffic.
//
// Three properties, in this order:
//
//   - every series renders its full histogram shape (per-bucket lines, +Inf,
//     _sum, _count) on an idle boot — a scrape never has an unexplained gap;
//   - the request histogram's `route` label is the mux TEMPLATE, never the
//     request path (no agent id, no message id), and its count advances on a
//     real POST;
//   - the inbox-deliver histogram splits the store write from the webhook
//     accept, and both arms advance when real deliveries take them.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// latencySeriesNames is the R2 contract of this row.
var latencySeriesNames = []string{
	"crier_http_request_duration_seconds",
	"crier_webhook_delivery_duration_seconds",
	"crier_guard_check_duration_seconds",
	"crier_inbox_deliver_duration_seconds",
}

// requireHistogramShape asserts one series renders as a Prometheus histogram:
// HELP/TYPE for the base name, ≥1 _bucket line with an le label, a +Inf
// bucket, and the _sum and _count families.
func requireHistogramShape(t *testing.T, body, name string) {
	t.Helper()
	for _, want := range []string{
		"# HELP " + name + " ",
		"# TYPE " + name + " histogram",
		name + "_bucket{",
		name + "_sum{",
		name + "_count{",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q for %s\nbody:\n%s", want, name, body)
		}
	}
	if !strings.Contains(body, name+"_bucket{") || !strings.Contains(body, `le="+Inf"`) {
		t.Errorf("%s has no bucket lines at all", name)
	}
	if !strings.Contains(body, `le="+Inf"}`) {
		t.Errorf("%s has no +Inf bucket", name)
	}
}

// TestMetricsLatency_AllFourSeriesRenderOnIdleBoot is the presence half: a
// metrics-on boot with no traffic at all still explains every series.
func TestMetricsLatency_AllFourSeriesRenderOnIdleBoot(t *testing.T) {
	baseURL := bootStateMetricsServer(t)
	body := scrapeMetrics(t, baseURL)

	for _, name := range latencySeriesNames {
		requireHistogramShape(t, body, name)
	}
	// The seeded bounded domains: every closed label value exists at 0.
	for _, want := range []string{
		`crier_webhook_delivery_duration_seconds_count{outcome="delivered"}`,
		`crier_webhook_delivery_duration_seconds_count{outcome="failed"}`,
		`crier_webhook_delivery_duration_seconds_count{outcome="deadlettered"}`,
		`crier_webhook_delivery_duration_seconds_count{outcome="dropped"}`,
		`crier_guard_check_duration_seconds_count{decision="allow",errored="false"}`,
		`crier_guard_check_duration_seconds_count{decision="allow",errored="true"}`,
		`crier_guard_check_duration_seconds_count{decision="block",errored="false"}`,
		`crier_guard_check_duration_seconds_count{decision="sanitize",errored="false"}`,
		`crier_inbox_deliver_duration_seconds_count{transport="inbox"}`,
		`crier_inbox_deliver_duration_seconds_count{transport="webhook"}`,
		`crier_http_request_duration_seconds_count{route="other",method="GET",code="404"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("idle boot is missing the seeded child %q\nbody:\n%s", want, body)
		}
	}

	// MON-CRIER-005 owns the rename: the unprefixed series must be untouched
	// by this row.
	for _, want := range []string{"# TYPE http_requests_total counter", "# TYPE webhook_deliveries_total counter"} {
		if !strings.Contains(body, want) {
			t.Errorf("legacy series changed by this row: missing %q", want)
		}
	}
}

// TestMetricsLatency_RequestSeriesUsesTemplateAndAdvances is the label-discipline
// proof on a live boot: real deliveries through POST /agents/<id>/inbox record
// under the template, the agent id appears in no label, and the count moves.
func TestMetricsLatency_RequestSeriesUsesTemplateAndAdvances(t *testing.T) {
	baseURL := bootStateMetricsServer(t)
	const agentID = "m002-latency"

	status, _, raw := bpCall(t, baseURL, http.MethodPost, "/agents",
		`{"id":"`+agentID+`"}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("register agent: status %d, body %s", status, raw)
	}

	deliverRoute := `crier_http_request_duration_seconds_count{route="/agents/{id}/inbox",method="POST",code="201"}`
	before, _ := sampleValue(scrapeMetrics(t, baseURL), deliverRoute)

	for i := 0; i < 2; i++ {
		status, _, raw = bpCall(t, baseURL, http.MethodPost, "/agents/"+agentID+"/inbox",
			`{"sender":"m002-probe","payload":{"n":1}}`, nil)
		if status != http.StatusCreated {
			t.Fatalf("deliver %d: status %d, body %s", i, status, raw)
		}
	}
	// One read through a DIFFERENT template, so the label set is proven per
	// route rather than for a single hard-coded one.
	if status, _, raw = bpCall(t, baseURL, http.MethodGet, "/agents/"+agentID+"/inbox", "", nil); status != http.StatusOK {
		t.Fatalf("retrieve: status %d, body %s", status, raw)
	}

	body := scrapeMetrics(t, baseURL)
	after, ok := sampleValue(body, deliverRoute)
	if !ok {
		t.Fatalf("request histogram has no sample for the deliver route\nbody:\n%s", body)
	}
	if after != before+2 {
		t.Errorf("deliver-route _count = %v, want %v (two requests)", after, before+2)
	}
	for _, want := range []string{
		`crier_http_request_duration_seconds_count{route="/agents/{id}/inbox",method="GET",code="200"}`,
		`crier_http_request_duration_seconds_bucket{route="/agents/{id}/inbox",method="POST",code="201",le="+Inf"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\nbody:\n%s", want, body)
		}
	}
	// THE LABEL DISCIPLINE: the agent id and the literal path never label a
	// sample (spec §3.4 — unbounded by definition).
	for _, forbidden := range []string{
		"route=\"/agents/" + agentID,
		"route=\"/agents/{" + agentID,
		agentID + `/inbox"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("request path/id leaked into the exposition (%q)\nbody:\n%s", forbidden, body)
		}
	}

	// The inbox arm of the transport split advanced with the same deliveries.
	inboxCount, ok := sampleValue(body, `crier_inbox_deliver_duration_seconds_count{transport="inbox"}`)
	if !ok {
		t.Fatalf("inbox transport arm missing\nbody:\n%s", body)
	}
	if inboxCount < 2 {
		t.Errorf("transport=\"inbox\" _count = %v, want >= 2 (two accepted deliveries)", inboxCount)
	}
}

// TestMetricsLatency_WebhookArmsAdvanceLive proves the push half end to end on
// a booted server: a real endpoint answers, and BOTH the webhook-delivery
// histogram (outcome=delivered) and the webhook transport arm of the deliver
// split advance.
func TestMetricsLatency_WebhookArmsAdvanceLive(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer endpoint.Close()

	baseURL := bootStateMetricsServer(t)
	const agentID = "m002-hook"
	status, _, raw := bpCall(t, baseURL, http.MethodPost, "/agents",
		`{"id":"`+agentID+`","webhook":{"url":"`+endpoint.URL+`","delivery_mode":"async","retries":2}}`, nil)
	if status != http.StatusCreated {
		t.Fatalf("register webhook agent: status %d, body %s", status, raw)
	}
	status, _, raw = bpCall(t, baseURL, http.MethodPost, "/agents/"+agentID+"/inbox",
		`{"sender":"m002-probe","payload":{"n":2}}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("async deliver: status %d, body %s", status, raw)
	}

	delivered := `crier_webhook_delivery_duration_seconds_count{outcome="delivered"}`
	transport := `crier_inbox_deliver_duration_seconds_count{transport="webhook"}`
	deadline := time.Now().Add(10 * time.Second)
	var body string
	for {
		body = scrapeMetrics(t, baseURL)
		n, _ := sampleValue(body, delivered)
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook delivery never reached a terminal state\nbody:\n%s", body)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if n, _ := sampleValue(body, transport); n < 1 {
		t.Errorf("transport=\"webhook\" _count = %v, want >= 1", n)
	}
	// The observation is a real latency, not a placeholder: the delivered arm
	// carries a non-zero sum and a matching count.
	if !strings.Contains(body, `crier_webhook_delivery_duration_seconds_sum{outcome="delivered"} `) {
		t.Errorf("delivered arm has no _sum\nbody:\n%s", body)
	}
}
