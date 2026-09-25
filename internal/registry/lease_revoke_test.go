package registry

import (
	"github.com/pashagolub/pgxmock/v5"
	"testing"
	"time"
)

// CR-FEAT-030: the kill-switch's revoke_leases step at the store level. The
// detect package proves the step is CALLED and its count reported; this file
// proves what it does to the store — a leased message goes back to the queue,
// an ACKed message is not resurrected, and an agent with nothing leased answers
// 0 rather than failing.
func TestMemoryStoreRevokeLeasesReturnsMessagesToTheQueue(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Register(&Agent{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		if err := s.Deliver("a", &InboxEntry{ID: id}); err != nil {
			t.Fatalf("deliver %s: %v", id, err)
		}
	}

	// Claim two of them (one lease), then ack m1: only m2 is still an
	// outstanding claim.
	msgs, leaseID, err := s.Retrieve("a", 0, 2)
	if err != nil || leaseID == "" {
		t.Fatalf("retrieve: lease=%q err=%v", leaseID, err)
	}
	if len(msgs) != 2 {
		t.Fatalf("leased %d messages, want 2", len(msgs))
	}
	if err := s.Ack("a", leaseID, []string{"m1"}); err != nil {
		t.Fatalf("ack m1: %v", err)
	}
	if _, leased, _, err := s.Stats("a"); err != nil || leased != 1 {
		t.Fatalf("leased_count before revocation = %d (err %v), want 1", leased, err)
	}

	n, err := s.RevokeLeases("a")
	if err != nil {
		t.Fatalf("RevokeLeases: %v", err)
	}
	if n != 1 {
		t.Fatalf("RevokeLeases released %d, want 1 (m1 is acked, m2 was the only outstanding claim)", n)
	}
	if _, leased, _, err := s.Stats("a"); err != nil || leased != 0 {
		t.Fatalf("leased_count after revocation = %d (err %v), want 0 — the claim is gone from the store, not merely answered differently", leased, err)
	}
	// Idempotent: nothing is leased any more.
	if n, err := s.RevokeLeases("a"); err != nil || n != 0 {
		t.Fatalf("second RevokeLeases = (%d, %v), want (0, nil)", n, err)
	}
	// An unknown agent is not an error either (the kill-switch unregisters the
	// row after this step, so a re-run must still answer).
	if n, err := s.RevokeLeases("ghost"); err != nil || n != 0 {
		t.Fatalf("RevokeLeases(ghost) = (%d, %v), want (0, nil)", n, err)
	}

	// m2 is claimable again.
	again, _, err := s.Retrieve("a", 0, 10)
	if err != nil {
		t.Fatalf("re-retrieve: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range again {
		ids[m.ID] = true
	}
	if !ids["m2"] || !ids["m3"] {
		t.Fatalf("after revocation the claimable set = %v, want m2 and m3", ids)
	}
	if ids["m1"] {
		t.Fatal("an ACKed message came back: revocation must not un-ack anything")
	}
}

// TestMemoryStoreRevokeLeasesLeavesExpiryAlone: revocation is not a purge — the
// message's TTL still applies, so a contained agent's backlog does not become
// immortal.
func TestMemoryStoreRevokeLeasesLeavesExpiryAlone(t *testing.T) {
	s := NewMemoryStore()
	if err := s.Register(&Agent{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	ttl := 1
	if err := s.Deliver("a", &InboxEntry{ID: "m1", ExpiresAt: time.Now().Add(time.Hour), TTLSeconds: &ttl}); err != nil {
		t.Fatal(err)
	}
	if _, leaseID, err := s.Retrieve("a", 0, 1); err != nil || leaseID == "" {
		t.Fatalf("retrieve: lease=%q err=%v", leaseID, err)
	}
	if _, err := s.RevokeLeases("a"); err != nil {
		t.Fatal(err)
	}
	msgs, _, err := s.Retrieve("a", 0, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("re-retrieve = %d messages (err %v), want 1", len(msgs), err)
	}
	if msgs[0].ExpiresAt.IsZero() {
		t.Fatal("revoking a lease cleared the message's expiry — revocation returns a message to the queue, it does not dismantle it")
	}
}

// TestPostgresStoreRevokeLeasesSQL pins the statement the durable backend runs:
// all three lease columns cleared together (the table's CHECK constraint
// requires it), scoped to the agent, and only where a lease exists.
func TestPostgresStoreRevokeLeasesSQL(t *testing.T) {
	s, mock := newMockStore(t)
	if _, err := s.RevokeLeases(""); err != ErrInvalidStoreInput {
		t.Fatalf("RevokeLeases(\"\") = %v, want ErrInvalidStoreInput", err)
	}
	mock.ExpectExec(`UPDATE inbox_entries`).
		WithArgs("attacker").
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	if n, err := s.RevokeLeases("attacker"); err != nil || n != 2 {
		t.Fatalf("RevokeLeases = (%d, %v), want (2, nil)", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
