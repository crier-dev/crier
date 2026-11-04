// metrics_test.go — MON-CRIER-002 acceptance evidence for
// crier_http_request_duration_seconds (observability-spec §3.3, R2).
//
// The hard part of that series is the `route` label: it must be the mux
// TEMPLATE, and an unmatched request must collapse into one bounded value
// ("other") instead of emitting the literal path. These tests drive a real
// gorilla/mux router with the real Logging middleware and then read the real
// exposition, so the label is proven where it is actually produced.
package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/metrics"

	"github.com/gorilla/mux"
)

// scrapeDefault renders the process-wide registry (what /metrics serves).
func scrapeDefault(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(metrics.Default.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET registry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	return string(body)
}

// TestRequestDurationLabelsRouteTemplate is the label-discipline proof: a
// request to /agents/<real id>/inbox is recorded under the TEMPLATE, and the
// agent id appears nowhere in the exposition.
func TestRequestDurationLabelsRouteTemplate(t *testing.T) {
	r := mux.NewRouter()
	r.Use(Logging)
	r.HandleFunc("/agents/{id}/inbox", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}).Methods(http.MethodPost)

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/agents/agent-SECRET-1234/inbox", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST deliver: %v", err)
	}
	_ = resp.Body.Close()

	body := scrapeDefault(t)
	if !strings.Contains(body, "# TYPE crier_http_request_duration_seconds histogram") {
		t.Fatalf("request-duration histogram is not exposed:\n%s", body)
	}
	for _, want := range []string{
		`crier_http_request_duration_seconds_bucket{route="/agents/{id}/inbox",method="POST",code="201",le="+Inf"}`,
		`crier_http_request_duration_seconds_count{route="/agents/{id}/inbox",method="POST",code="201"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q\nbody:\n%s", want, body)
		}
	}
	// THE LABEL DISCIPLINE: neither the agent id nor the literal path may
	// ever become a label value (spec §3.4 — unbounded).
	for _, forbidden := range []string{"agent-SECRET-1234", `route="/agents/agent`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("request path/id leaked into the exposition (%q)\nbody:\n%s", forbidden, body)
		}
	}
}

// TestRequestDurationUnmatchedIsOther: a request the router cannot match
// records under the one bounded sentinel, never the requested path.
func TestRequestDurationUnmatchedIsOther(t *testing.T) {
	r := mux.NewRouter()
	r.Use(Logging)
	// The router default 404 runs OUTSIDE the Use chain, which is why
	// cmd/server wraps its fallbacks itself (fallback.go) — mirror the
	// wrapping so this test exercises the same chain.
	r.NotFoundHandler = Logging(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such route", http.StatusNotFound)
	}))
	r.HandleFunc("/known", func(w http.ResponseWriter, _ *http.Request) {}).Methods(http.MethodGet)

	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/no/such/path/abc-999")
	if err != nil {
		t.Fatalf("GET unmatched: %v", err)
	}
	_ = resp.Body.Close()

	body := scrapeDefault(t)
	if !strings.Contains(body, `crier_http_request_duration_seconds_count{route="other",method="GET",code="404"}`) {
		t.Fatalf("unmatched request did not record under route=\"other\":\n%s", body)
	}
	if strings.Contains(body, "abc-999") || strings.Contains(body, `route="/no/such/path"`) {
		t.Fatalf("unmatched request path leaked into a label:\n%s", body)
	}
}

// TestRequestDurationMethodClamped: a caller-chosen method token collapses to
// "OTHER" — the `method` label is a closed enum, not the request's bytes.
func TestRequestDurationMethodClamped(t *testing.T) {
	r := mux.NewRouter()
	r.Use(Logging)
	r.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {})

	srv := httptest.NewServer(r)
	defer srv.Close()

	req, err := http.NewRequest("TRACEATTACK", srv.URL+"/ping", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	_ = resp.Body.Close()

	body := scrapeDefault(t)
	if !strings.Contains(body, `route="/ping",method="OTHER"`) {
		t.Fatalf("non-standard method was not clamped to OTHER:\n%s", body)
	}
	if strings.Contains(body, `method="TRACEATTACK"`) {
		t.Fatalf("caller-chosen method token became a label value:\n%s", body)
	}
}

// TestRequestDurationSeededOnFreshProcess: the family is present (with its
// +Inf bucket and _count) before any real traffic, so a first scrape never
// shows an unexplained gap.
func TestRequestDurationSeededOnFreshProcess(t *testing.T) {
	body := scrapeDefault(t)
	for _, want := range []string{
		"# HELP crier_http_request_duration_seconds ",
		"# TYPE crier_http_request_duration_seconds histogram",
		`crier_http_request_duration_seconds_bucket{route="other",method="GET",code="404",le="0.005"}`,
		`crier_http_request_duration_seconds_bucket{route="other",method="GET",code="404",le="+Inf"}`,
		`crier_http_request_duration_seconds_sum{route="other",method="GET",code="404"}`,
		`crier_http_request_duration_seconds_count{route="other",method="GET",code="404"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q\nbody:\n%s", want, body)
		}
	}
}
