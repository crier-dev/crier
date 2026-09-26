package registry

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// InboxPeeker is an OPTIONAL Store capability: a READ-ONLY view of ONE inbox
// entry (INT-A2A-003).
//
// It exists because a surface can need to OBSERVE a message's lifecycle without
// consuming it. The only other read of an inbox is Retrieve, and that LEASES
// what it returns — so an observer built on it would steal the message from the
// agent whose work it was watching, which is a side effect no read-only surface
// may have. Peek takes no lock, mints no lease and changes nothing.
//
// It is optional in the same way `updater` and `ListErrorReporter` are: a store
// that cannot answer (the remote proxy, which has no inbox of its own) simply
// does not implement it, and the caller degrades to its documented
// no-lifecycle-observation path instead of failing. A store that DOES implement
// it must keep the row it returns a snapshot — mutating it must not touch the
// stored message.
//
// Error contract: ErrMessageNotFound when the id is not in that agent's inbox
// (never delivered, already acknowledged, expired and purged), ErrInvalidStoreInput
// for a blank agent or message id, and the backend's own error for a storage
// failure. A caller MUST be able to tell those apart: "the message is gone" is
// a terminal answer, "the store is down" is not.
type InboxPeeker interface {
	Peek(agentID, messageID string) (*InboxEntry, error)
}

// Compile-time capability assertions: both shipped backends can answer a Peek,
// so the surfaces that need it (the A2A streaming adapter) work on the default
// in-memory backend and on PostgreSQL alike.
var (
	_ InboxPeeker = (*MemoryStore)(nil)
	_ InboxPeeker = (*PostgresStore)(nil)
)

// Peek returns a SNAPSHOT of one message in agentID's inbox, or
// ErrMessageNotFound. The entry is copied — payload included — so the caller
// cannot mutate stored state through it, and the returned entry carries the same
// lease fields a Retrieve would set (LeasedAt / LeaseID) so a caller can see
// whether the message is currently claimed.
//
// Expiry is NOT applied here: an expired-but-unswept row is returned as it is
// stored, and the caller decides what an expired entry means for it. That keeps
// this a view of the store rather than a second implementation of the expiry
// rule.
func (s *MemoryStore) Peek(agentID, messageID string) (*InboxEntry, error) {
	if agentID == "" || messageID == "" {
		return nil, fmt.Errorf("%w: blank agent ID or message ID", ErrInvalidStoreInput)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, e := range s.inboxes[agentID] {
		if e.ID != messageID {
			continue
		}
		return cloneInboxEntry(e), nil
	}
	return nil, fmt.Errorf("%w: %q is not in agent %q's inbox", ErrMessageNotFound, messageID, agentID)
}

// cloneInboxEntry returns a copy of one stored entry — payload and lease
// timestamp included — so a caller holding it cannot mutate stored state. It is
// shared by the two read-only capabilities (Peek, PeekInbox) so their snapshots
// cannot drift apart.
func cloneInboxEntry(e *InboxEntry) *InboxEntry {
	clone := *e
	clone.Payload = append([]byte(nil), e.Payload...)
	if e.LeasedAt != nil {
		at := *e.LeasedAt
		clone.LeasedAt = &at
	}
	return &clone
}

// Peek reads one inbox row without claiming it, the PostgreSQL half of
// InboxPeeker. It is a single SELECT: no transaction, no FOR UPDATE, no lease
// columns written, so it can never make a message unclaimable for its consumer.
func (s *PostgresStore) Peek(agentID, messageID string) (*InboxEntry, error) {
	if agentID == "" || messageID == "" {
		return nil, fmt.Errorf("%w: blank agent ID or message ID", ErrInvalidStoreInput)
	}
	ctx, cancel := s.operationContext()
	defer cancel()

	var (
		entry     InboxEntry
		expiresAt pgtype.Timestamptz
		leasedAt  *time.Time
		leaseID   *string
		acked     bool
	)
	err := s.pool.QueryRow(ctx, `
SELECT id, agent_id, payload, COALESCE(sender, ''), COALESCE(idempotency_key, ''),
       created_at, expires_at, leased_at, lease_id, acked
FROM inbox_entries
WHERE agent_id = $1 AND id = $2;`,
		agentID, messageID,
	).Scan(&entry.ID, &entry.AgentID, &entry.Payload, &entry.Sender, &entry.IdempotencyKey,
		&entry.CreatedAt, &expiresAt, &leasedAt, &leaseID, &acked)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: %q is not in agent %q's inbox", ErrMessageNotFound, messageID, agentID)
		}
		return nil, fmt.Errorf("peek: %w", err)
	}
	// expires_at is read through pgtype.Timestamptz for the same reason
	// Retrieve does: a stored `infinity` (ttl_seconds=0 → never expires,
	// DF-CRIER-37) must decode instead of erroring.
	entry.ExpiresAt = expiryFromTimestamptz(expiresAt)
	entry.LeasedAt = leasedAt
	if leaseID != nil {
		entry.LeaseID = *leaseID
	}
	entry.ACKed = acked
	return &entry, nil
}
