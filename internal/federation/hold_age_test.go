// hold_age_test.go — MON-CRIER-004 unit evidence for HoldManager.OldestHeldAge,
// the accessor behind the crier_federation_held_age_seconds gauge: the age of the
// oldest held delivery, which depth alone cannot express ("holding 40 for 20s"
// and "holding 40 for 20 minutes" look identical on a depth-only surface).
package federation

import (
	"encoding/json"
	"testing"
	"time"
)

// heldItem builds a valid HoldItem at the given enqueue time.
func heldItem(id string, enqueued time.Time) *HoldItem {
	return &HoldItem{
		ID:         id,
		AgentID:    "target",
		Body:       json.RawMessage(`{"payload":{"n":1}}`),
		EnqueuedAt: enqueued,
	}
}

// TestHoldManager_OldestHeldAge_EmptyQueue proves the empty-queue contract: an
// age of 0, not a fabricated one.
func TestHoldManager_OldestHeldAge_EmptyQueue(t *testing.T) {
	m := NewHoldManager(nil, NewMemoryHoldQueue(), HoldConfig{})
	if got := m.OldestHeldAge(); got != 0 {
		t.Errorf("OldestHeldAge on an empty queue = %v, want 0", got)
	}
	if got := m.Pending(); got != 0 {
		t.Errorf("Pending on an empty queue = %d, want 0", got)
	}
}

// TestHoldManager_OldestHeldAge_SeededQueue proves the age tracks the OLDEST item
// regardless of insertion order — a reopened file queue restores items in its own
// order, so position is not age.
func TestHoldManager_OldestHeldAge_SeededQueue(t *testing.T) {
	now := time.Now()
	m := NewHoldManager(nil, NewMemoryHoldQueue(), HoldConfig{MaxHold: time.Hour})

	// The manager fills a zero EnqueuedAt, but keeps an explicit one (the case a
	// restarted process reopens): seed an explicit 10-minute-old item LAST so the
	// newest is at index 0.
	if err := m.Enqueue(heldItem("fresh", now)); err != nil {
		t.Fatalf("enqueue fresh: %v", err)
	}
	if err := m.Enqueue(heldItem("oldest", now.Add(-10*time.Minute))); err != nil {
		t.Fatalf("enqueue oldest: %v", err)
	}
	if got := m.Pending(); got != 2 {
		t.Fatalf("Pending = %d, want 2", got)
	}
	age := m.OldestHeldAge()
	if age < 10*time.Minute {
		t.Errorf("OldestHeldAge = %v, want >= 10m (the oldest EnqueuedAt, not the queue head)", age)
	}
	if age > 10*time.Minute+30*time.Second {
		t.Errorf("OldestHeldAge = %v, want about 10m", age)
	}

	// Removing the oldest must drop the reported age: the gauge follows the hold
	// set, not the manager's lifetime.
	if err := m.Queue().Remove("oldest"); err != nil {
		t.Fatalf("remove oldest: %v", err)
	}
	if age := m.OldestHeldAge(); age < 0 || age > 30*time.Second {
		t.Errorf("OldestHeldAge after removing the oldest = %v, want ~0 (the fresh item)", age)
	}
}

// TestHoldManager_OldestHeldAge_SkipsUnstampableItems pins the honesty rule: an
// item with no EnqueuedAt is skipped rather than reported as infinitely old, and a
// nil manager renders 0 instead of panicking (the gauge callback can see nil).
func TestHoldManager_OldestHeldAge_SkipsUnstampableItems(t *testing.T) {
	q := NewMemoryHoldQueue()
	if err := q.Enqueue(heldItem("unstamped", time.Time{})); err != nil {
		t.Fatalf("enqueue unstamped: %v", err)
	}
	m := NewHoldManager(nil, q, HoldConfig{})
	if got := m.Pending(); got != 1 {
		t.Fatalf("Pending = %d, want 1", got)
	}
	if got := m.OldestHeldAge(); got != 0 {
		t.Errorf("OldestHeldAge with only an unstamped item = %v, want 0", got)
	}

	var nilManager *HoldManager
	if got := nilManager.OldestHeldAge(); got != 0 {
		t.Errorf("nil manager OldestHeldAge = %v, want 0", got)
	}
}
