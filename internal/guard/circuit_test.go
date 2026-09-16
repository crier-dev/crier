package guard

import (
	"testing"
	"time"
)

// TestCircuit_FailedProbeReopens is the regression test supplied by the
// reporter (GitHub issue #1, item 1): after the cooldown expires exactly one
// request is let through as the probe, and if that probe fails the circuit
// must re-open for a fresh cooldown window instead of staying permanently
// half-open (every request an unthrottled probe).
func TestCircuit_FailedProbeReopens(t *testing.T) {
	now := time.Unix(1000, 0)
	c := NewCircuit(3, 300*time.Second)
	c.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		c.recordFailure("k")
	}
	if !c.open("k") {
		t.Fatalf("circuit did not open after 3 failures")
	}

	now = now.Add(301 * time.Second)
	if c.open("k") {
		t.Fatalf("circuit still open after cooldown; the probe must be allowed")
	}

	// The probe fails: the circuit must be re-armed for a full cooldown.
	c.recordFailure("k")
	if !c.open("k") {
		t.Fatal("circuit did not re-open after failed probe")
	}

	// And the re-armed window is a full cooldown from the failed probe, not
	// the original trip time.
	now = now.Add(299 * time.Second)
	if !c.open("k") {
		t.Errorf("circuit closed %s into the re-armed window; want open", 299*time.Second)
	}
	now = now.Add(2 * time.Second)
	if c.open("k") {
		t.Errorf("circuit still open after the re-armed cooldown expired; want probe allowed")
	}
}

// TestCircuit_FailureInsideOpenWindowDoesNotExtend pins the other half of the
// contract: a failure recorded while the circuit is still open (no probe was
// allowed through) must NOT push the trip time forward — otherwise a dead
// provider that keeps being retried by other call sites would stay open
// forever and never even get its one probe.
func TestCircuit_FailureInsideOpenWindowDoesNotExtend(t *testing.T) {
	t0 := time.Unix(2000, 0)
	now := t0
	c := NewCircuit(2, 300*time.Second)
	c.now = func() time.Time { return now }

	c.recordFailure("k")
	c.recordFailure("k")
	if !c.open("k") {
		t.Fatalf("circuit did not open at the threshold")
	}

	// A third failure 10s into the window (streak grows, window is open).
	now = t0.Add(10 * time.Second)
	c.recordFailure("k")

	now = t0.Add(299 * time.Second)
	if !c.open("k") {
		t.Errorf("circuit closed at t0+299s; want still open (window must not be extended)")
	}
	now = t0.Add(300 * time.Second)
	if c.open("k") {
		t.Errorf("circuit still open at t0+300s; the original trip time must govern the window")
	}
}

// TestCircuit_SuccessClosesAndResets pins that a successful probe clears the
// streak and the trip, so the endpoint is not open, and that a single later
// failure does not re-open a circuit at a threshold above 1.
func TestCircuit_SuccessClosesAndResets(t *testing.T) {
	now := time.Unix(3000, 0)
	c := NewCircuit(2, 300*time.Second)
	c.now = func() time.Time { return now }

	c.recordFailure("k")
	c.recordFailure("k")
	if !c.open("k") {
		t.Fatalf("circuit did not open at the threshold")
	}

	c.recordSuccess("k")
	if c.open("k") {
		t.Errorf("circuit still open after a successful probe")
	}

	// One failure after the success must not re-open: the streak was reset.
	c.recordFailure("k")
	if c.open("k") {
		t.Errorf("circuit re-opened below the threshold after a success reset")
	}
	// The second consecutive failure trips it again, from this new window.
	c.recordFailure("k")
	if !c.open("k") {
		t.Errorf("circuit did not open after a fresh threshold of failures")
	}
}
