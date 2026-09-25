package detect

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// clock is a settable clock so the windows are tested, not waited out.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }
func (c *clock) advance(d time.Duration) {
	c.at = c.at.Add(d)
}

// newTestDetector builds a detector with a fixed clock and (optionally) a log.
func newTestDetector(t *testing.T, cfg Config) (*Detector, *clock) {
	t.Helper()
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := &clock{at: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	d.Now = c.now
	return d, c
}

func observe(d *Detector, sender, target, verdict string) {
	d.Observe(registry.DeliveryObservation{
		Sender: sender, Target: target, MessageID: "m-" + target,
		Verdict: verdict, Transport: "inbox", Payload: []byte(`{"ok":true}`),
	})
}

func signalsOf(alerts []Alert) []string {
	out := make([]string, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, a.Signal)
	}
	return out
}

// TestFanoutSpikeTripsOnceAndRearms is the signal the acceptance scenario
// exercises: one sender reaching many distinct targets inside the window.
// It also pins the ALERT DISCIPLINE — one alert per window, not one per
// message — because a signal that fires 50 times is a signal nobody reads.
func TestFanoutSpikeTripsOnceAndRearms(t *testing.T) {
	d, c := newTestDetector(t, Config{FanoutWindow: 60 * time.Second, FanoutMinTargets: 5, NewPeerMinTargets: 99})

	for _, target := range []string{"peer-1", "peer-2", "peer-3", "peer-4"} {
		observe(d, "compromised", target, registry.VerdictDelivered)
	}
	if got := d.Alerts(); len(got) != 0 {
		t.Fatalf("4 distinct targets tripped %v, want nothing (threshold is 5)", signalsOf(got))
	}

	observe(d, "compromised", "peer-5", registry.VerdictDelivered)
	alerts := d.Alerts()
	if len(alerts) != 1 || alerts[0].Signal != SignalFanoutSpike {
		t.Fatalf("5 distinct targets = %v, want exactly one %s", signalsOf(alerts), SignalFanoutSpike)
	}
	if alerts[0].Agent != "compromised" {
		t.Fatalf("alert agent = %q, want compromised", alerts[0].Agent)
	}
	if alerts[0].Severity != SeverityCritical {
		t.Fatalf("alert severity = %q, want critical", alerts[0].Severity)
	}
	if got := alerts[0].Evidence["distinct_targets"]; got != 5 {
		t.Fatalf("evidence distinct_targets = %v, want 5", got)
	}

	// More targets inside the same window: still ONE alert.
	observe(d, "compromised", "peer-6", registry.VerdictDelivered)
	if got := d.Alerts(); len(got) != 1 {
		t.Fatalf("6th target produced %d alerts, want the window to stay quiet after the first", len(got))
	}

	// A different sender is its own baseline: the alert is per agent.
	observe(d, "quiet-agent", "worker-1", registry.VerdictDelivered)
	if got := d.Alerts(); len(got) != 1 {
		t.Fatalf("an unrelated sender raised an alert: %v", signalsOf(got))
	}

	// After the window slides, a fresh burst re-alarms.
	c.advance(2 * time.Minute)
	for _, target := range []string{"seven", "eight", "nine", "ten", "eleven"} {
		observe(d, "compromised", target, registry.VerdictDelivered)
	}
	if got := d.Alerts(); len(got) != 2 {
		t.Fatalf("after the window slid, alerts = %d, want 2 (the signal must re-arm)", len(got))
	}
}

// TestNewPeerBurstCountsFirstContactOnly: a new peer is normal, a BURST of
// first-ever conversations is an agent mapping the bus — and repeating an
// existing conversation must not count toward the burst.
func TestNewPeerBurstCountsFirstContactOnly(t *testing.T) {
	d, _ := newTestDetector(t, Config{NewPeerWindow: 60 * time.Second, NewPeerMinTargets: 3, FanoutMinTargets: 99})

	observe(d, "scanner", "peer-1", registry.VerdictDelivered)
	observe(d, "scanner", "peer-1", registry.VerdictDelivered) // same peer again
	observe(d, "scanner", "peer-2", registry.VerdictDelivered)
	observe(d, "scanner", "peer-2", registry.VerdictDelivered)
	if got := d.Alerts(); len(got) != 0 {
		t.Fatalf("two distinct new peers tripped %v, want nothing (threshold is 3)", signalsOf(got))
	}

	observe(d, "scanner", "peer-3", registry.VerdictDelivered)
	alerts := d.Alerts()
	if len(alerts) != 1 || alerts[0].Signal != SignalNewPeerBurst {
		t.Fatalf("3 distinct new peers = %v, want one %s", signalsOf(alerts), SignalNewPeerBurst)
	}
	if got := alerts[0].Evidence["new_peers"]; got != 3 {
		t.Fatalf("evidence new_peers = %v, want 3", got)
	}
	if detail, _ := alerts[0].Detail, ""; !strings.Contains(detail, "first-ever") {
		t.Fatalf("alert detail = %q, want it to say what a new peer means", detail)
	}
}

// TestOddHourVolumeAndWrapWindow covers the quiet-window signal including the
// midnight wrap an operator writes as "22-6".
func TestOddHourVolumeAndWrapWindow(t *testing.T) {
	d, c := newTestDetector(t, Config{QuietStartHour: 1, QuietEndHour: 5, QuietMinMessages: 3, FanoutMinTargets: 99, NewPeerMinTargets: 99})

	// 12:00 UTC is not quiet.
	observe(d, "night-owl", "a", registry.VerdictDelivered)
	observe(d, "night-owl", "b", registry.VerdictDelivered)
	observe(d, "night-owl", "c", registry.VerdictDelivered)
	if got := d.Alerts(); len(got) != 0 {
		t.Fatalf("daytime volume tripped %v, want nothing", signalsOf(got))
	}

	c.at = time.Date(2026, 9, 26, 2, 0, 0, 0, time.UTC) // inside 01:00-05:00
	observe(d, "night-owl", "d", registry.VerdictDelivered)
	observe(d, "night-owl", "e", registry.VerdictDelivered)
	observe(d, "night-owl", "f", registry.VerdictDelivered)
	alerts := d.Alerts()
	if len(alerts) != 1 || alerts[0].Signal != SignalOddHourVolume {
		t.Fatalf("quiet-window volume = %v, want one %s", signalsOf(alerts), SignalOddHourVolume)
	}
	if alerts[0].Severity != SeverityWarning {
		t.Fatalf("odd-hour severity = %q, want warning (volume is suspicious, not proof)", alerts[0].Severity)
	}

	// A wrapping window: 22:00-06:00 must include 23:00 and 05:00, and exclude 12:00.
	wrap, c2 := newTestDetector(t, Config{QuietStartHour: 22, QuietEndHour: 6, QuietMinMessages: 1, FanoutMinTargets: 99, NewPeerMinTargets: 99})
	for _, at := range []time.Time{
		time.Date(2026, 9, 26, 23, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 27, 5, 0, 0, 0, time.UTC),
	} {
		c2.at = at
		before := len(wrap.Alerts())
		observe(wrap, "night-owl", "x", registry.VerdictDelivered)
		if len(wrap.Alerts()) != before+1 {
			t.Fatalf("%s was not counted inside the wrapping quiet window 22-6", at.Format(time.RFC3339))
		}
	}
	c2.at = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	before := len(wrap.Alerts())
	observe(wrap, "night-owl", "y", registry.VerdictDelivered)
	if len(wrap.Alerts()) != before {
		t.Fatal("12:00 UTC counted as quiet with CR_DETECT_QUIET_HOURS=22-6")
	}
}

// TestQuarantinedDeliveriesAreNotBehaviour: once an agent is contained its
// refusals must not keep feeding the baselines that alerted on it, or a
// contained agent would look like a permanent spike.
func TestQuarantinedDeliveriesAreNotBehaviour(t *testing.T) {
	d, _ := newTestDetector(t, Config{FanoutMinTargets: 2, NewPeerMinTargets: 2})
	observe(d, "attacker", "peer-1", registry.VerdictDelivered)
	observe(d, "attacker", "peer-2", registry.VerdictDelivered)
	if got := len(d.Alerts()); got == 0 {
		t.Fatal("the baseline did not trip at all")
	}
	before := len(d.Alerts())
	for i := 0; i < 10; i++ {
		observe(d, "attacker", "peer-"+strings.Repeat("z", i+1), registry.VerdictQuarantined)
	}
	if got := len(d.Alerts()); got != before {
		t.Fatalf("contained-agent refusals raised %d more alerts, want 0 (a refusal is not behaviour)", got-before)
	}
}

// TestCanaryTripByTargetAndByPayload covers both ways an exfiltrating agent
// trips a canary: addressing one, or carrying its token in a payload.
func TestCanaryTripByTargetAndByPayload(t *testing.T) {
	d, _ := newTestDetector(t, Config{CanaryTokens: []string{"crier-canary-test-token-1"}, FanoutMinTargets: 99, NewPeerMinTargets: 99})

	canaries := d.Canaries()
	if len(canaries) != 1 {
		t.Fatalf("canaries = %d, want the planted one", len(canaries))
	}
	if canaries[0].ID != "canary-"+hashPrefix("crier-canary-test-token-1") {
		t.Fatalf("canary id = %q, want a DETERMINISTIC id derived from the token (stable across restarts)", canaries[0].ID)
	}

	observe(d, "leaker", canaries[0].ID, registry.VerdictAgentNotFound)
	if got := d.Alerts(); len(got) != 1 || got[0].Signal != SignalCanaryTrip {
		t.Fatalf("delivery to a canary id = %v, want one %s", signalsOf(got), SignalCanaryTrip)
	}
	if got := d.Alerts()[0].Severity; got != SeverityCritical {
		t.Fatalf("canary severity = %q, want critical", got)
	}

	d.Observe(registry.DeliveryObservation{
		Sender: "leaker", Target: "peer-1", Verdict: registry.VerdictDelivered,
		Payload: []byte(`{"stolen":"crier-canary-test-token-1"}`),
	})
	alerts := d.Alerts()
	if len(alerts) != 2 || alerts[1].Signal != SignalCanaryTrip {
		t.Fatalf("token in payload = %v, want a second %s", signalsOf(alerts), SignalCanaryTrip)
	}

	// An oversize payload is NOT scanned (documented bound, so the delivery
	// path cannot be turned into a scanning DoS) — and that is asserted, not
	// assumed.
	big := strings.Repeat("x", maxCanaryScanBytes+1) + "crier-canary-test-token-1"
	d.Observe(registry.DeliveryObservation{
		Sender: "leaker", Target: "peer-2", Verdict: registry.VerdictDelivered, Payload: []byte(big),
	})
	if got := len(d.Alerts()); got != 2 {
		t.Fatalf("an oversize payload raised an alert (%d), want the documented cap to skip the scan", got)
	}
}

func hashPrefix(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:6])
}

// TestGeneratedCanariesRotateUnlessPinned: the default canaries exist (the
// feature must work with no configuration) and operator tokens are what make
// them stable.
func TestGeneratedCanariesRotateUnlessPinned(t *testing.T) {
	a, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if len(a.Canaries()) != DefaultCanaryCount {
		t.Fatalf("generated canaries = %d, want %d", len(a.Canaries()), DefaultCanaryCount)
	}
	if a.Canaries()[0].Token == b.Canaries()[0].Token {
		t.Fatal("two boots generated the same canary token; generated canaries must rotate")
	}

	pinned, err := New(Config{CanaryTokens: []string{"pin-1", "pin-2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	again, err := New(Config{CanaryTokens: []string{"pin-1", "pin-2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if pinned.Canaries()[0].ID != again.Canaries()[0].ID {
		t.Fatal("a pinned canary's id changed between boots")
	}
}

// TestObservationsAndAlertsLandInTheLog: the log is the durable artifact, so
// both delivery outcomes and the alerts they raised must be in the FILE, with
// the chain intact.
func TestObservationsAndAlertsLandInTheLog(t *testing.T) {
	dir := t.TempDir()
	d, _ := newTestDetector(t, Config{
		LogPath:           filepath.Join(dir, "delivery.jsonl"),
		KeyPath:           filepath.Join(dir, "delivery.key"),
		FanoutMinTargets:  2,
		NewPeerMinTargets: 99,
	})

	observe(d, "attacker", "peer-1", registry.VerdictDelivered)
	observe(d, "attacker", "peer-2", registry.VerdictDelivered)
	observe(d, "attacker", "peer-3", registry.VerdictQuarantined)

	audit := d.Audit()
	if audit == nil {
		t.Fatal("no audit log was opened")
	}
	records := audit.Entries(0, "")
	if len(records) != 4 {
		t.Fatalf("log records = %d, want 3 deliveries + 1 alert", len(records))
	}
	var deliveries, alerts int
	for _, r := range records {
		switch r.Kind {
		case KindDelivery:
			deliveries++
			if r.Sender == "" || r.Target == "" || r.Verdict == "" {
				t.Fatalf("delivery record is missing who/what/verdict: %+v", r)
			}
		case KindAlert:
			alerts++
			if r.Signal != SignalFanoutSpike {
				t.Fatalf("alert record signal = %q, want %s", r.Signal, SignalFanoutSpike)
			}
		}
	}
	if deliveries != 3 || alerts != 1 {
		t.Fatalf("kinds = %d deliveries / %d alerts, want 3/1", deliveries, alerts)
	}
	if rep := audit.Verify(); !rep.OK {
		t.Fatalf("log does not verify: %+v", rep)
	}
}

// TestStatsReportsVerdictsAndQuarantines pins the operator-facing tally.
func TestStatsReportsVerdictsAndQuarantines(t *testing.T) {
	d, _ := newTestDetector(t, Config{FanoutMinTargets: 99, NewPeerMinTargets: 99})
	observe(d, "a", "b", registry.VerdictDelivered)
	observe(d, "a", "b", registry.VerdictAgentNotFound)
	observe(d, "a", "b", registry.VerdictQuarantined)

	st := d.Stats()
	if st.Observed != 3 {
		t.Fatalf("observed = %d, want 3", st.Observed)
	}
	if st.Verdicts[registry.VerdictDelivered] != 1 || st.Verdicts[registry.VerdictAgentNotFound] != 1 {
		t.Fatalf("verdicts = %v, want one of each", st.Verdicts)
	}
	if st.Canaries != DefaultCanaryCount {
		t.Fatalf("canaries = %d, want %d", st.Canaries, DefaultCanaryCount)
	}
}
