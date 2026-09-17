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

// TestMemoryStore_Update_AdvancesLastSeen pins DF-CRIER-156 on the in-memory
// backend: a successful PATCH must move last_seen forward. Pre-fix Update
// copied the stored value back into the caller's agent, so the registry's
// last_seen stayed frozen at registration and a client could not use the
// PATCH response to confirm the write.
func TestMemoryStore_Update_AdvancesLastSeen(t *testing.T) {
	store := NewMemoryStore()
	agent := &Agent{ID: "agent-patch", Capabilities: []string{"relay", "mesh"}}
	if err := store.Register(agent); err != nil {
		t.Fatalf("register: %v", err)
	}
	registeredAt := agent.RegisteredAt
	registeredStatus := agent.Status
	before := agent.LastSeen

	// Make the update's instant distinguishable from registration's.
	time.Sleep(2 * time.Millisecond)

	agent.Capabilities = []string{"solver"}
	if err := store.Update(agent); err != nil {
		t.Fatalf("update: %v", err)
	}

	if !agent.LastSeen.After(before) {
		t.Fatalf("last_seen = %v after Update, want strictly after the pre-update %v",
			agent.LastSeen, before)
	}
	if !agent.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("registered_at = %v after Update, want the registration-time %v",
			agent.RegisteredAt, registeredAt)
	}
	if agent.Status != registeredStatus {
		t.Fatalf("status = %q after Update, want %q", agent.Status, registeredStatus)
	}
	if len(agent.Capabilities) != 1 || agent.Capabilities[0] != "solver" {
		t.Fatalf("capabilities = %v after Update, want [solver]", agent.Capabilities)
	}

	// The store now holds the advanced instant: the value a PATCH returns and
	// the value a following GET returns must agree.
	got, err := store.Get("agent-patch")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.LastSeen.Equal(agent.LastSeen) {
		t.Fatalf("stored last_seen = %v, want the updated %v", got.LastSeen, agent.LastSeen)
	}
}

// TestMemoryStore_Update_NotFound keeps the missing-agent contract (404) with
// the new write semantics in place.
func TestMemoryStore_Update_NotFound(t *testing.T) {
	store := NewMemoryStore()
	err := store.Update(&Agent{ID: "ghost"})
	if !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("expected ErrAgentNotFound for an unknown agent, got %v", err)
	}
}
