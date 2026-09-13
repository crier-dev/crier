package registry

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"
)

func BenchmarkRetrieve(b *testing.B) {
	const agentID = "bench-agent"

	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: agentID}); err != nil {
		b.Fatalf("Register: %v", err)
	}
	for i := 0; i < 100; i++ {
		entry := &InboxEntry{
			ID:      strconv.Itoa(i),
			Payload: json.RawMessage(`{"event":"benchmark"}`),
		}
		if err := store.Deliver(agentID, entry); err != nil {
			b.Fatalf("Deliver: %v", err)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		messages, _, err := store.Retrieve(agentID, 30*time.Second, 50)
		if err != nil {
			b.Fatalf("Retrieve: %v", err)
		}
		if len(messages) != 50 {
			b.Fatalf("Retrieve returned %d messages, want 50", len(messages))
		}

		b.StopTimer()
		for _, message := range messages {
			message.LeasedAt = nil
			message.LeaseID = ""
			message.LeaseDuration = 0
		}
		b.StartTimer()
	}
}

func BenchmarkAck(b *testing.B) {
	const agentID = "bench-agent"

	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: agentID}); err != nil {
		b.Fatalf("Register: %v", err)
	}
	for i := 0; i < 20; i++ {
		entry := &InboxEntry{
			ID:      strconv.Itoa(i),
			Payload: json.RawMessage(`{"event":"benchmark"}`),
		}
		if err := store.Deliver(agentID, entry); err != nil {
			b.Fatalf("Deliver: %v", err)
		}
	}
	messages, leaseID, err := store.Retrieve(agentID, 30*time.Second, 20)
	if err != nil {
		b.Fatalf("Retrieve: %v", err)
	}
	messageIDs := make([]string, len(messages))
	for i, message := range messages {
		messageIDs[i] = message.ID
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.Ack(agentID, leaseID, messageIDs); err != nil {
			b.Fatalf("Ack: %v", err)
		}

		b.StopTimer()
		store.inboxes[agentID] = messages
		b.StartTimer()
	}
}

func TestRetrieve_LeaseExpiryReleasedInline(t *testing.T) {
	const agentID = "agent-lease"

	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: agentID}); err != nil {
		t.Fatalf("register: %v", err)
	}
	entry := &InboxEntry{ID: "msg-1", Payload: json.RawMessage(`{"event":"lease"}`)}
	if err := store.Deliver(agentID, entry); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	// A short lease, just long enough that the in-window check below cannot
	// race it on a loaded box.
	const lease = 100 * time.Millisecond

	first, firstLease, err := store.Retrieve(agentID, lease, 10)
	if err != nil {
		t.Fatalf("first retrieve: %v", err)
	}
	if len(first) != 1 || first[0].ID != "msg-1" {
		t.Fatalf("first retrieve returned %d message(s), want 1 (msg-1)", len(first))
	}

	// Inside the lease window the message stays invisible.
	inside, _, err := store.Retrieve(agentID, lease, 10)
	if err != nil {
		t.Fatalf("retrieve inside lease: %v", err)
	}
	if len(inside) != 0 {
		t.Fatalf("retrieve inside active lease returned %d message(s), want 0", len(inside))
	}

	// Past the lease, Retrieve must release the expired lease INLINE and
	// redeliver — without PurgeExpired ever running.
	time.Sleep(150 * time.Millisecond)

	second, secondLease, err := store.Retrieve(agentID, lease, 10)
	if err != nil {
		t.Fatalf("second retrieve: %v", err)
	}
	if len(second) != 1 || second[0].ID != "msg-1" {
		t.Fatalf("second retrieve returned %d message(s), want 1 (msg-1 redelivered)", len(second))
	}
	if secondLease == "" {
		t.Fatal("second retrieve returned an empty leaseID")
	}
	if secondLease == firstLease {
		t.Fatalf("second retrieve reused lease %q, want a new leaseID", secondLease)
	}
}

func TestAck_EmptyMessageIDs(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: "agent-1"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// A lease-only ack is a silent no-op (CR-GAP-014) — the store must reject it.
	if err := store.Ack("agent-1", "lease", nil); !errors.Is(err, ErrInvalidStoreInput) {
		t.Fatalf("expected ErrInvalidStoreInput for empty message_ids, got %v", err)
	}
}
