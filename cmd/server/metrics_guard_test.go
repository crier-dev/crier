// metrics_guard_test.go — MON-CRIER-003 acceptance evidence: the LLM
// message guard fails OPEN, so its degradation must be visible in GET
// /metrics. These tests boot the REAL server (same harness as
// observability_test.go) and read the exposition a Prometheus would see:
//
//   - guard enabled            → all four R3 series exist with HELP/TYPE,
//     every bounded provider renders a 0, inflight is 0 at rest.
//   - guard disabled           → the same series render 0 (never NaN, never
//     absent): "no guard" is a reading, not a gap.
//   - provider unreachable     → the circuit-open series for that provider
//     reaches 1 and crier_guard_all_providers_failed_total advances, i.e. the
//     fail-open the guard performs in silence is now counted.
package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/guard"
)

// guardMetricSeries is the §3.3 R3 contract (docs/observability-spec.md).
var guardMetricSeries = []string{
	"crier_guard_provider_circuit_open",
	"crier_guard_inflight",
	"crier_guard_saturated_total",
	"crier_guard_all_providers_failed_total",
}

// requireGuardSeriesMetadata asserts every R3 series is exposable and typed.
func requireGuardSeriesMetadata(t *testing.T, body string) {
	t.Helper()
	for _, name := range guardMetricSeries {
		if !strings.Contains(body, "# HELP "+name+" ") {
			t.Errorf("exposition missing \"# HELP %s\" line", name)
		}
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("exposition missing \"# TYPE %s\" line", name)
		}
	}
	for _, want := range []string{
		"# TYPE crier_guard_provider_circuit_open gauge",
		"# TYPE crier_guard_inflight gauge",
		"# TYPE crier_guard_saturated_total counter",
		"# TYPE crier_guard_all_providers_failed_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q", want)
		}
	}
}

// TestMetricsGuardSeriesOnBoot: with the guard enabled, all four series and
// one 0 sample per bounded provider label are present.
func TestMetricsGuardSeriesOnBoot(t *testing.T) {
	t.Setenv("CR_ENABLE_METRICS", "true")
	t.Setenv("CR_GUARD_ENABLED", "true")
	t.Setenv("DEEPSEEK_API_KEY", "") // no call is made: nothing is delivered
	baseURL := bootObservabilityServer(t)
	client := &http.Client{Timeout: 10 * time.Second}

	status, _, body := get(t, client, baseURL+"/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: status %d, want 200", status)
	}
	requireGuardSeriesMetadata(t, body)

	if got := metricValue(body, "crier_guard_inflight"); got != 0 {
		t.Errorf("crier_guard_inflight at idle = %v, want 0", got)
	}
	for _, provider := range guard.ProviderNames() {
		got := labeledMetricValue(body, "crier_guard_provider_circuit_open", "provider", provider)
		if got != 0 {
			t.Errorf("crier_guard_provider_circuit_open{provider=%q} = %v, want 0 (closed / unused)", provider, got)
		}
	}
}

// TestMetricsGuardDisabledRendersZero: with the guard disabled the series
// still render 0 — an operator must not have to distinguish "guard off" from
// "scrape gap" by guesswork, and a NaN here would read as a broken metric.
func TestMetricsGuardDisabledRendersZero(t *testing.T) {
	t.Setenv("CR_ENABLE_METRICS", "true")
	t.Setenv("CR_GUARD_ENABLED", "false")
	baseURL := bootObservabilityServer(t)
	client := &http.Client{Timeout: 10 * time.Second}

	status, _, body := get(t, client, baseURL+"/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics: status %d, want 200", status)
	}
	requireGuardSeriesMetadata(t, body)
	if got := metricValue(body, "crier_guard_inflight"); got != 0 {
		t.Errorf("crier_guard_inflight with the guard disabled = %v, want 0", got)
	}
	for _, provider := range guard.ProviderNames() {
		if got := labeledMetricValue(body, "crier_guard_provider_circuit_open", "provider", provider); got != 0 {
			t.Errorf("circuit_open{provider=%q} with the guard disabled = %v, want 0 (never NaN)", provider, got)
		}
	}
}

// TestMetricsGuardCircuitOpenReachesOne drives the real fail-open path: the
// guard's deepseek lane points at a closed port, one delivery trips the
// circuit (threshold 1), and the message is still ACCEPTED — exactly the
// silent degradation MON-CRIER-003 exists to make visible.
func TestMetricsGuardCircuitOpenReachesOne(t *testing.T) {
	t.Setenv("CR_ENABLE_METRICS", "true")
	t.Setenv("CR_GUARD_ENABLED", "true")
	t.Setenv("DEEPSEEK_API_KEY", "dummy-key-not-used")
	t.Setenv("CR_GUARD_DEEPSEEK_BASE_URL", "http://127.0.0.1:1")
	t.Setenv("CR_GUARD_CIRCUIT_THRESHOLD", "1")
	t.Setenv("CR_GUARD_TIMEOUT_MS", "2000")
	baseURL := bootObservabilityServer(t)
	client := &http.Client{Timeout: 20 * time.Second}

	regBody := `{"id":"guard-metrics-probe","public_key":"` + strings.Repeat("ab", 32) + `"}`
	regResp, err := http.Post(baseURL+"/agents", "application/json", strings.NewReader(regBody))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("register agent: status %d", regResp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, regResp.Body)
	_ = regResp.Body.Close()

	_, _, before := get(t, client, baseURL+"/metrics", "")
	failBefore := metricValue(before, "crier_guard_all_providers_failed_total")
	if failBefore < 0 {
		t.Fatalf("crier_guard_all_providers_failed_total absent from the exposition:\n%s", before)
	}

	// Small, clean payload → the guard must consult the LLM. The provider is
	// unreachable, so the check fails open and the delivery is STILL stored.
	body := `{"payload":{"text":"a short benign status note"}}`
	resp, err := http.Post(baseURL+"/agents/guard-metrics-probe/inbox", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("deliver guard probe: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		t.Fatalf("deliver with an unreachable guard provider: status %d, want 2xx (the guard fails OPEN); body: %s",
			resp.StatusCode, raw)
	}

	_, _, after := get(t, client, baseURL+"/metrics", "")
	requireGuardSeriesMetadata(t, after)

	open := labeledMetricValue(after, "crier_guard_provider_circuit_open", "provider", "deepseek")
	if open != 1 {
		t.Fatalf("crier_guard_provider_circuit_open{provider=\"deepseek\"} = %v, want 1 after reaching the circuit threshold; exposition:\n%s",
			open, after)
	}
	for _, provider := range guard.ProviderNames() {
		if provider == "deepseek" {
			continue
		}
		if got := labeledMetricValue(after, "crier_guard_provider_circuit_open", "provider", provider); got != 0 {
			t.Errorf("circuit_open{provider=%q} = %v, want 0 (only deepseek failed)", provider, got)
		}
	}
	if got := metricValue(after, "crier_guard_inflight"); got != 0 {
		t.Errorf("crier_guard_inflight after the delivery returned = %v, want 0", got)
	}
	failAfter := metricValue(after, "crier_guard_all_providers_failed_total")
	if !(failAfter > failBefore) {
		t.Errorf("crier_guard_all_providers_failed_total did not advance on a failed provider chain: before=%v after=%v",
			failBefore, failAfter)
	}
}
