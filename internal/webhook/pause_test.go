package webhook

import (
	"errors"
	"testing"
	"time"
)

// CR-FEAT-030: the kill-switch's outbound step. Pausing an agent must (a) stop
// the redelivery queue from POSTing anything else for it, (b) refuse new
// deliveries on that lane, and (c) drop what it had buffered — while leaving
// every OTHER agent's lane untouched, which is the half that matters most: a
// containment call must not become a bus outage.
func TestPauseAgentDropsOnlyThatAgentAndRefusesItsLane(t *testing.T) {
	q := NewMemoryQueue()
	d := NewDriver(NewClient(2*time.Second, nil), q, DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})

	// Two queued deliveries for the target, one for an innocent bystander.
	for _, item := range []*QueueItem{
		{AgentID: "attacker", Envelope: &Envelope{Crier: EnvelopeMeta{MessageID: "a1"}}},
		{AgentID: "attacker", Envelope: &Envelope{Crier: EnvelopeMeta{MessageID: "a2"}}},
		{AgentID: "bystander", Envelope: &Envelope{Crier: EnvelopeMeta{MessageID: "b1"}}},
	} {
		if err := q.Push(item); err != nil {
			t.Fatal(err)
		}
	}

	dropped, err := d.PauseAgent("attacker")
	if err != nil {
		t.Fatalf("PauseAgent: %v", err)
	}
	if dropped != 2 {
		t.Fatalf("dropped = %d, want the 2 queued deliveries for the paused agent", dropped)
	}
	if q.Len() != 1 {
		t.Fatalf("queue len = %d, want 1 (the bystander's delivery must survive)", q.Len())
	}
	if !d.Paused("attacker") {
		t.Fatal("Paused(attacker) = false after PauseAgent")
	}
	if d.Paused("bystander") {
		t.Fatal("Paused(bystander) = true — containment leaked onto another agent")
	}

	// New deliveries on the paused lane are refused, terminally.
	cfg := &Config{URL: "http://127.0.0.1:1/hook", DeliveryMode: "async", Retries: 5, TimeoutMs: 1000}
	if _, err := d.DeliverContext(t.Context(), "attacker", cfg, &Envelope{Crier: EnvelopeMeta{MessageID: "a3"}}); !errors.Is(err, ErrPaused) {
		t.Fatalf("Deliver for a paused agent = %v, want ErrPaused", err)
	}
	if _, err := d.DeliverBlocking(t.Context(), "attacker", cfg, &Envelope{Crier: EnvelopeMeta{MessageID: "a4"}}, time.Second); !errors.Is(err, ErrPaused) {
		t.Fatalf("DeliverBlocking for a paused agent = %v, want ErrPaused", err)
	}
	// …and the bystander's lane still works (nothing queued for it by this
	// call, but it is not refused either: assert the refusal is aimed).
	if err := d.refuseIfPaused("bystander"); err != nil {
		t.Fatalf("bystander lane refused: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("a refused delivery changed the queue: len = %d, want 1", q.Len())
	}
}

// TestPauseAgentDropsBufferedBatch: batch mode holds envelopes in a per-agent
// buffer, so a pause that only drained the shared queue would leave the
// contained agent's messages waiting to be flushed. Both are counted.
func TestPauseAgentDropsBufferedBatch(t *testing.T) {
	q := NewMemoryQueue()
	d := NewDriver(NewClient(2*time.Second, nil), q, DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
		BatchFlushInterval: time.Hour,
	})
	cfg := &Config{URL: "http://127.0.0.1:1/hook", DeliveryMode: "batch",
		Batch: &BatchConfig{MaxMessages: 10, FlushIntervalS: 3600}, Retries: 5, TimeoutMs: 1000}

	for i := 0; i < 3; i++ {
		if _, err := d.DeliverContext(t.Context(), "attacker", cfg, &Envelope{Crier: EnvelopeMeta{MessageID: "b"}}); err != nil {
			t.Fatalf("buffer delivery %d: %v", i, err)
		}
	}
	dropped, err := d.PauseAgent("attacker")
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 3 {
		t.Fatalf("dropped = %d, want the 3 buffered envelopes", dropped)
	}
	d.mu.Lock()
	_, stillBuffered := d.batches["attacker"]
	d.mu.Unlock()
	if stillBuffered {
		t.Fatal("the paused agent's batch buffer is still registered — it would flush later")
	}
}

// TestPauseAgentEmptyIDIsRefused: an empty id is a caller bug, and treating it
// as "pause everything" would be a bus-wide outage from a typo.
func TestPauseAgentEmptyIDIsRefused(t *testing.T) {
	d := NewDriver(NewClient(time.Second, nil), NewMemoryQueue(), DriverConfig{})
	if _, err := d.PauseAgent(""); err == nil {
		t.Fatal("PauseAgent(\"\") succeeded; an empty id must be refused")
	}
}
