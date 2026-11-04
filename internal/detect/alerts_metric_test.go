// alerts_metric_test.go — MON-CRIER-004 unit evidence for the detection signal:
// the AlertsByKind snapshot (bounded (signal, severity) pairs, no exported slice)
// and the crier_detect_alerts_total counter it must agree with.
package detect

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
)

// alertCounterValue reads one crier_detect_alerts_total child out of the process
// exposition. ok=false means the child is absent (it is seeded at 0 by the
// package init, so absence is a real failure, not an idle reading).
func alertCounterValue(t *testing.T, signal, severity string) (float64, bool) {
	t.Helper()
	body := renderExposition(t)
	prefix := `crier_detect_alerts_total{signal="` + signal + `",severity="` + severity + `"}`
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, false
			}
			v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

// renderExposition scrapes the process-wide registry the way /metrics does.
func renderExposition(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// TestAlertsByKind_CountsBoundedPairs is the accessor half: alerts are counted
// per (signal, severity) pair, pairs that never fired are absent, and a nil
// detector yields an empty map rather than a panic.
func TestAlertsByKind_CountsBoundedPairs(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatalf("detect.New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if got := d.AlertsByKind(); len(got) != 0 {
		t.Fatalf("a fresh detector reports alerts by kind %v, want an empty map", got)
	}

	now := time.Now()
	d.raise(Alert{Signal: SignalFanoutSpike, Agent: "sender-a", Severity: SeverityCritical}, now)
	d.raise(Alert{Signal: SignalFanoutSpike, Agent: "sender-b", Severity: SeverityCritical}, now)
	d.raise(Alert{Signal: SignalOddHourVolume, Agent: "sender-a", Severity: SeverityWarning}, now)

	got := d.AlertsByKind()
	want := map[AlertKind]int{
		{Signal: SignalFanoutSpike, Severity: SeverityCritical}:  2,
		{Signal: SignalOddHourVolume, Severity: SeverityWarning}: 1,
	}
	if len(got) != len(want) {
		t.Fatalf("AlertsByKind = %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("AlertsByKind[%+v] = %d, want %d (all pairs: %v)", k, got[k], n, got)
		}
	}
	// A pair that never fired is ABSENT, not zero: that is what makes "has this
	// signal ever fired" answerable without arithmetic.
	if _, ok := got[AlertKind{Signal: SignalCanaryTrip, Severity: SeverityCritical}]; ok {
		t.Errorf("AlertsByKind reported a pair that never fired: %v", got)
	}
	// The snapshot must agree with the slice it summarizes.
	if total := len(d.Alerts()); total != 3 {
		t.Errorf("Alerts() has %d entries, want the 3 raised", total)
	}

	var nilDetector *Detector
	if got := nilDetector.AlertsByKind(); len(got) != 0 {
		t.Errorf("nil detector AlertsByKind = %v, want an empty map", got)
	}
	if got := nilDetector.QuarantinedCount(); got != 0 {
		t.Errorf("nil detector QuarantinedCount = %d, want 0", got)
	}
}

// TestAlertsTotalMovesWithRaisedAlerts proves the counter behind
// crier_detect_alerts_total is incremented by raise(), not derived from the
// grow-only slice at scrape time.
func TestAlertsTotalMovesWithRaisedAlerts(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatalf("detect.New: %v", err)
	}
	defer func() { _ = d.Close() }()

	before, ok := alertCounterValue(t, SignalCanaryTrip, SeverityCritical)
	if !ok {
		t.Fatalf("crier_detect_alerts_total{%s/%s} is not seeded in the exposition", SignalCanaryTrip, SeverityCritical)
	}

	d.raise(Alert{Signal: SignalCanaryTrip, Agent: "trip-1", Severity: SeverityCritical}, time.Now())

	after, ok := alertCounterValue(t, SignalCanaryTrip, SeverityCritical)
	if !ok {
		t.Fatal("crier_detect_alerts_total disappeared after a raise")
	}
	if after < before+1 {
		t.Errorf("crier_detect_alerts_total{%s/%s} = %v after one raise, want >= %v", SignalCanaryTrip, SeverityCritical, after, before+1)
	}

	// The pair is the label: a raise at one severity must not be readable as a
	// different pair, and the other SEEDED pair (odd_hour_volume/warning) must
	// still be present as its own child.
	if _, ok := alertCounterValue(t, SignalOddHourVolume, SeverityWarning); !ok {
		t.Errorf("the seeded (odd_hour_volume, warning) child is absent — every bounded pair must render")
	}
	if got := d.AlertsByKind(); len(got) != 1 {
		t.Errorf("AlertsByKind = %v, want exactly the one pair this detector raised", got)
	}
}

// TestQuarantinedCount_TracksContainment pins the gauge source: the count follows
// the contained set, which is state, not a counter.
func TestQuarantinedCount_TracksContainment(t *testing.T) {
	d, err := New(Config{})
	if err != nil {
		t.Fatalf("detect.New: %v", err)
	}
	defer func() { _ = d.Close() }()

	if got := d.QuarantinedCount(); got != 0 {
		t.Fatalf("QuarantinedCount = %d on a fresh detector, want 0", got)
	}
	d.mu.Lock()
	d.quarantined["agent-a"] = Quarantine{AgentID: "agent-a", At: time.Now().UTC().Format(time.RFC3339Nano)}
	d.quarantined["agent-b"] = Quarantine{AgentID: "agent-b", At: time.Now().UTC().Format(time.RFC3339Nano)}
	d.mu.Unlock()

	if got := d.QuarantinedCount(); got != 2 {
		t.Errorf("QuarantinedCount = %d, want 2", got)
	}
	if roster := len(d.Quarantines()); roster != 2 {
		t.Errorf("Quarantines() roster = %d, want the same 2 the count reports", roster)
	}
}
