package harness_test

// metrics_test.go — CRIER-OBS-1 acceptance evidence, unit arm: the
// ingress → harness dispatch series exist with their family metadata and the
// labelled samples a real Registry.Deliver produces, rendered through the
// registry's real exposition handler.
//
// The live arm (a booted server, a fake harness binary, GET /metrics over
// HTTP) lives in cmd/server/harness_metrics_test.go; this file proves the
// wiring itself, including the exactly-once counting of the failure paths.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/metrics"
)

// failingStub is the coded-failure twin of registry_test.go's stubAdapter:
// every delivery fails carrying HARNESS_FAILED, which is the class a real
// adapter puts on a receipt when the harness process itself failed.
type failingStub struct {
	name string
}

func (f *failingStub) Name() string { return f.name }

func (f *failingStub) Probe(context.Context) (harness.Capability, error) {
	return harness.Capability{Available: true, Version: "stub"}, nil
}

func (f *failingStub) Deliver(_ context.Context, _ harness.SessionRef, _ harness.Message) (harness.Receipt, error) {
	return harness.FailedReceipt(f.name, "HARNESS_FAILED", "boom"), nil
}

// scrape renders the process-wide registry through its real handler (the
// same shape internal/registry's metrics tests use).
func scrape(t *testing.T) string {
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
		t.Fatalf("read exposition: %v", err)
	}
	return string(body)
}

// TestHarnessDispatchMetrics dispatches one success and one coded failure
// through the registry, then asserts the three family names and the exact
// labelled samples. The harness name "fake" is used by no other test in this
// package, so the exact sample values are deterministic regardless of test
// order: exactly the two dispatches below have ever been counted under it.
func TestHarnessDispatchMetrics(t *testing.T) {
	// A FIXED injected clock keeps the histogram deterministic: the elapsed
	// between Deliver's start stamp and its deferred observation is exactly
	// zero no matter how many clock reads the body makes, so every
	// observation is the first bucket with _sum 0 — an assertion about
	// wiring, not about timing.
	reg := harness.NewRegistry(harness.RegistryOptions{
		Enabled: true,
		Default: "fake",
		Now:     func() time.Time { return time.Unix(1700000000, 0) },
	})
	reg.Register(&stubAdapter{name: "fake"})

	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		SessionRef: harness.SessionRef{ID: "ses_metrics"},
		Message:    harness.Message{ID: "m_ok", SessionID: "s_metrics", Text: "x"},
	})
	if rc.State != harness.StateDispatched {
		t.Fatalf("success dispatch: state %q, want dispatched", rc.State)
	}

	// A failed receipt carrying its code. Registering under the SAME harness
	// id is deliberate (registration replaces, which is a documented
	// registry property): both samples land under harness="fake", so the
	// two state children of one adapter are asserted side by side.
	reg.Register(&failingStub{name: "fake"})
	rc = reg.Deliver(context.Background(), harness.DispatchRequest{
		SessionRef: harness.SessionRef{ID: "ses_metrics"},
		Message:    harness.Message{ID: "m_fail", SessionID: "s_metrics", Text: "x"},
	})
	if rc.State != harness.StateFailed || rc.Error != "HARNESS_FAILED" {
		t.Fatalf("failure dispatch: state %q error %q, want failed/HARNESS_FAILED", rc.State, rc.Error)
	}

	body := scrape(t)
	for _, want := range []string{
		"# HELP crier_harness_dispatches_total ",
		"# TYPE crier_harness_dispatches_total counter",
		"# HELP crier_harness_dispatch_errors_total ",
		"# TYPE crier_harness_dispatch_errors_total counter",
		"# HELP crier_harness_dispatch_duration_seconds ",
		"# TYPE crier_harness_dispatch_duration_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q:\n%s", want, body)
		}
	}
	for _, want := range []string{
		`crier_harness_dispatches_total{harness="fake",state="dispatched"} 1`,
		`crier_harness_dispatches_total{harness="fake",state="failed"} 1`,
		`crier_harness_dispatch_errors_total{harness="fake",class="HARNESS_FAILED"} 1`,
		`crier_harness_dispatch_duration_seconds_bucket{harness="fake",le="0.005"} 2`,
		`crier_harness_dispatch_duration_seconds_bucket{harness="fake",le="+Inf"} 2`,
		`crier_harness_dispatch_duration_seconds_count{harness="fake"} 2`,
		`crier_harness_dispatch_duration_seconds_sum{harness="fake"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing sample %q:\n%s", want, body)
		}
	}
}
