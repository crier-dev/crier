// backlog_test.go — MON-CRIER-004 unit evidence for the driver accessors behind
// crier_webhook_queue_depth / crier_webhook_queue_oldest_age_seconds and the
// bounded `attempt` label of crier_webhook_attempts_total.
package webhook

import (
	"testing"
	"time"
)

// TestQueueAccessors_EmptyQueue proves the empty-queue contract: depth 0 and an
// age of exactly 0 (not a fabricated age, not NaN).
func TestQueueAccessors_EmptyQueue(t *testing.T) {
	d := NewDriver(nil, NewMemoryQueue(), DriverConfig{})
	if got := d.QueueDepth(); got != 0 {
		t.Errorf("QueueDepth on an empty queue = %d, want 0", got)
	}
	if got := d.OldestAge(); got != 0 {
		t.Errorf("OldestAge on an empty queue = %v, want 0", got)
	}
}

// TestQueueAccessors_SeededQueue proves depth and age over real items, including
// the case the FIFO order hides: a redelivery re-Pushes its item at the TAIL with
// its ORIGINAL CreatedAt, so items[0] is not necessarily the oldest. The scan must
// find the oldest CreatedAt wherever it sits.
func TestQueueAccessors_SeededQueue(t *testing.T) {
	q := NewMemoryQueue()
	now := time.Now()

	// Newest first, oldest last: index order deliberately opposes age order.
	if err := q.Push(&QueueItem{AgentID: "fresh", CreatedAt: now}); err != nil {
		t.Fatalf("push fresh: %v", err)
	}
	if err := q.Push(&QueueItem{AgentID: "oldest", CreatedAt: now.Add(-90 * time.Second)}); err != nil {
		t.Fatalf("push oldest: %v", err)
	}
	if err := q.Push(&QueueItem{AgentID: "middle", CreatedAt: now.Add(-30 * time.Second)}); err != nil {
		t.Fatalf("push middle: %v", err)
	}

	d := NewDriver(nil, q, DriverConfig{})
	if got := d.QueueDepth(); got != 3 {
		t.Errorf("QueueDepth = %d, want the 3 queued items", got)
	}
	age := d.OldestAge()
	if age < 90*time.Second {
		t.Errorf("OldestAge = %v, want >= 90s (the item at the tail, not items[0])", age)
	}
	if age > 90*time.Second+10*time.Second {
		t.Errorf("OldestAge = %v, want about 90s — a bogus timestamp is being aged", age)
	}

	// Draining the oldest must drop the reported age, so the gauge tracks the
	// backlog rather than the process start.
	q.PopBatch(2) // removes "fresh" and "oldest" (FIFO), leaving "middle"
	if got := d.QueueDepth(); got != 1 {
		t.Fatalf("QueueDepth after PopBatch(2) = %d, want 1", got)
	}
	if age := d.OldestAge(); age < 30*time.Second || age > 40*time.Second {
		t.Errorf("OldestAge after the drain = %v, want about 30s (the remaining item)", age)
	}
}

// noAgeQueue is a Queue that cannot report creation times: the driver must leave
// the age at 0 instead of inventing one, and must still report the depth.
type noAgeQueue struct{ n int }

func (q *noAgeQueue) Push(*QueueItem) error     { q.n++; return nil }
func (q *noAgeQueue) PopBatch(int) []*QueueItem { return nil }
func (q *noAgeQueue) Len() int                  { return q.n }

// TestQueueAccessors_QueueWithoutAgeCapability pins the graceful degradation: the
// age accessor is an OPTIONAL capability, read by assertion, so a queue that does
// not implement it neither panics nor reports a fake age.
func TestQueueAccessors_QueueWithoutAgeCapability(t *testing.T) {
	q := &noAgeQueue{}
	_ = q.Push(&QueueItem{CreatedAt: time.Now().Add(-time.Hour)})
	d := NewDriver(nil, q, DriverConfig{})
	if got := d.QueueDepth(); got != 1 {
		t.Errorf("QueueDepth = %d, want 1 (Len is part of the Queue interface)", got)
	}
	if got := d.OldestAge(); got != 0 {
		t.Errorf("OldestAge with no age capability = %v, want 0 (not measurable)", got)
	}
}

// TestQueueAccessors_NilDriver pins the nil-receiver safety the gauge callbacks
// rely on.
func TestQueueAccessors_NilDriver(t *testing.T) {
	var d *Driver
	if got := d.QueueDepth(); got != 0 {
		t.Errorf("nil driver QueueDepth = %d, want 0", got)
	}
	if got := d.OldestAge(); got != 0 {
		t.Errorf("nil driver OldestAge = %v, want 0", got)
	}
}

// TestAttemptLabelIsBounded pins the `attempt` label domain: 1..ceiling, with
// anything past the ceiling collapsed into it, so the label set can never grow
// with traffic.
func TestAttemptLabelIsBounded(t *testing.T) {
	cases := []struct {
		attempt int
		max     int
		want    string
	}{
		{1, 5, "1"},
		{3, 5, "3"},
		{6, 5, "6"}, // ceiling = retries + 1
		{7, 5, "6"}, // past the ceiling collapses into it
		{0, 5, "1"}, // an ordinal below 1 is the first attempt
		{-3, 5, "1"},
		{2, 0, "1"}, // a zero ceiling still yields a valid bucket
		{9, 9, "9"},
		{10, 9, "10"},
		{11, 9, "10"}, // ceiling = retries + 1
	}
	for _, c := range cases {
		if got := attemptLabel(c.attempt, c.max); got != c.want {
			t.Errorf("attemptLabel(%d, %d) = %q, want %q", c.attempt, c.max, got, c.want)
		}
	}
}
