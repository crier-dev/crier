// metrics_identity_test.go — MON-CRIER-001 acceptance evidence for the default
// rollout and the §3.2 identity/health series.
//
// The boot tests use the same in-process harness as observability_test.go
// (bootObservabilityServer → run(nil) with run-scoped cleanup); nothing shells
// out to a binary.
//
//   - default arm: CR_ENABLE_METRICS UNSET → GET /metrics is 200 and the body
//     carries crier_up, crier_build_info{version,commit,dirty},
//     crier_store_healthy{backend} and crier_start_time_seconds.
//   - opt-out arm: CR_ENABLE_METRICS=false → 404, i.e. the operator override
//     still works after the default flip.
//   - unit arm: the store-health grading and the bounded backend label, with
//     no server at all (probe-unknown → depth read → NaN ladder).
package main

import (
	"math"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// identitySeries is the §3.2 contract: name → the label names its sample line
// must carry (empty = an unlabelled sample).
var identitySeries = []struct {
	name   string
	labels []string
}{
	{"crier_up", nil},
	{"crier_build_info", []string{"version", "commit", "dirty"}},
	{"crier_store_healthy", []string{"backend"}},
	{"crier_start_time_seconds", nil},
}

// TestMetricsServedByDefault: a boot with NO CR_ENABLE_METRICS serves
// /metrics 200 and the four identity/health series (MON-CRIER-001).
func TestMetricsServedByDefault(t *testing.T) {
	t.Setenv("CR_ENABLE_PPROF", "")
	// "" is the unset spelling every config lookup in this repo treats as
	// absent, so the built-in default is what decides — do not replace with
	// "true": the point of the test is that an unconfigured deployment serves.
	t.Setenv("CR_ENABLE_METRICS", "")
	t.Setenv("CR_REGISTRY_BACKEND", "")
	baseURL := bootObservabilityServer(t)
	client := &http.Client{Timeout: 10 * time.Second}

	status, ctype, body := get(t, client, baseURL+"/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics with CR_ENABLE_METRICS unset: status %d, want 200 (a bare boot must serve the surface); body:\n%s", status, body)
	}
	if ctype != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("GET /metrics: Content-Type %q, want the Prometheus v0.0.4 text type", ctype)
	}

	for _, series := range identitySeries {
		if !strings.Contains(body, "# HELP "+series.name+" ") {
			t.Errorf("exposition missing \"# HELP %s\" line", series.name)
		}
		if !strings.Contains(body, "# TYPE "+series.name+" gauge") {
			t.Errorf("exposition missing \"# TYPE %s gauge\" line", series.name)
		}
		for _, label := range series.labels {
			if !strings.Contains(body, label+"=\"") {
				t.Errorf("%s sample missing label %q; body:\n%s", series.name, label, body)
			}
		}
	}

	// crier_up is a constant 1 while the process serves.
	if got := metricValue(body, "crier_up"); got != 1 {
		t.Errorf("crier_up = %v, want 1", got)
	}

	// crier_build_info carries exactly version/commit/dirty and the value 1.
	buildLine := regexp.MustCompile(`(?m)^crier_build_info\{version="([^"]*)",commit="([^"]*)",dirty="([^"]*)"\} 1$`)
	m := buildLine.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("crier_build_info sample is not the expected labelled form; body:\n%s", body)
	}
	if m[1] == "" || m[2] == "" {
		t.Errorf("crier_build_info labels must be non-empty: version=%q commit=%q", m[1], m[2])
	}
	if m[3] != "true" && m[3] != "false" {
		t.Errorf("crier_build_info dirty=%q, want \"true\" or \"false\"", m[3])
	}

	// crier_store_healthy{backend="memory"} is 1 on the in-memory boot: the
	// store answers its own HealthReporter probe.
	if got := labeledMetricValue(body, "crier_store_healthy", "backend", "memory"); got != 1 {
		t.Errorf("crier_store_healthy{backend=\"memory\"} = %v, want 1; body:\n%s", got, body)
	}

	// crier_start_time_seconds is a plausible unix-seconds process start: in
	// the past, and not older than this test binary can plausibly be.
	start := metricValue(body, "crier_start_time_seconds")
	now := float64(time.Now().Unix())
	if start <= 0 || start > now+5 {
		t.Errorf("crier_start_time_seconds = %v, want a unix timestamp <= now (%v)", start, now)
	}
	if now-start > 24*60*60 {
		t.Errorf("crier_start_time_seconds = %v is more than a day old (%v s before now)", start, now-start)
	}
}

// TestMetricsDisabledByOperator: after the default flip, CR_ENABLE_METRICS=false
// must still answer 404 — the surface is default-on, not mandatory.
func TestMetricsDisabledByOperator(t *testing.T) {
	t.Setenv("CR_ENABLE_PPROF", "")
	t.Setenv("CR_ENABLE_METRICS", "false")
	baseURL := bootObservabilityServer(t)
	client := &http.Client{Timeout: 5 * time.Second}

	status, _, body := get(t, client, baseURL+"/metrics", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET /metrics with CR_ENABLE_METRICS=false: status %d, want 404; body:\n%s", status, body)
	}
}

// TestStoreHealthyBackendLabel: the backend label is a closed set, so a
// hand-built Config or an unexpected resolver value cannot grow the label
// domain per deployment.
func TestStoreHealthyBackendLabel(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite", "postgres"} {
		if got := storeHealthyBackendLabel(backend); got != backend {
			t.Errorf("storeHealthyBackendLabel(%q) = %q, want the same value", backend, got)
		}
	}
	for _, backend := range []string{"", "Memory", "remote", "proxy"} {
		if got := storeHealthyBackendLabel(backend); got != "other" {
			t.Errorf("storeHealthyBackendLabel(%q) = %q, want \"other\" (bounded domain)", backend, got)
		}
	}
}

// TestStoreHealthyValue covers the grading ladder: a probe that KNOWS the
// answer decides it; a backend with no HealthReporter is graded through the
// queue-depth read the other gauges already use; nothing measurable is NaN,
// never a fabricated 0.
func TestStoreHealthyValue(t *testing.T) {
	healthy := func() (bool, bool) { return true, true }
	unhealthy := func() (bool, bool) { return false, true }
	unknown := func() (bool, bool) { return false, false }
	readOK := func() (registry.QueueDepth, bool) { return registry.QueueDepth{Pending: 1}, true }
	readFail := func() (registry.QueueDepth, bool) { return registry.QueueDepth{}, false }

	cases := []struct {
		name  string
		probe healthProbe
		read  queueDepthReader
		want  float64
	}{
		{"probe healthy", healthy, nil, 1},
		{"probe unhealthy", unhealthy, nil, 0},
		{"probe unhealthy wins over a readable queue", unhealthy, readOK, 0},
		{"no probe, depth read answers", unknown, readOK, 1},
		{"no probe, depth read fails", unknown, readFail, 0},
		{"nothing measurable", unknown, nil, math.NaN()},
		{"nil probe, nil reader", nil, nil, math.NaN()},
	}
	for _, tc := range cases {
		got := storeHealthyValue(tc.probe, tc.read)
		if math.IsNaN(tc.want) {
			if !math.IsNaN(got) {
				t.Errorf("%s: got %v, want NaN", tc.name, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestProcessStartIsPast keeps the start-time source honest: it is captured at
// package init, so it can only be in the past.
func TestProcessStartIsPast(t *testing.T) {
	if processStart.IsZero() {
		t.Fatal("processStart was not captured")
	}
	if now := time.Now(); processStart.After(now) {
		t.Errorf("processStart %v is in the future (now %v)", processStart, now)
	}
	// The exposition renders unix SECONDS; a zero value there would be a
	// parse-visible lie about the process age.
	if s := processStart.Unix(); s <= 0 {
		t.Errorf("processStart.Unix() = %d, want a positive unix timestamp", s)
	}
}
