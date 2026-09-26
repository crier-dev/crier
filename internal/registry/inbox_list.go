package registry

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// InboxLister is an OPTIONAL Store capability: a READ-ONLY, LEASE-FREE listing
// of ONE agent's inbox (INT-A2A-004).
//
// It is the multi-entry sibling of InboxPeeker and exists for the same reason:
// a surface can need to READ a queue without consuming it. Retrieve LEASES what
// it returns, so an observer built on it would steal every message it listed
// from the agent whose work it was describing. PeekInbox takes no lock on any
// entry, mints no lease and writes nothing.
//
// Contract for implementers:
//
//   - the returned entries are SNAPSHOTS in the store's own delivery order
//     (oldest first). Mutating a returned entry — or its Payload — must not
//     touch the stored message;
//   - a CLOSED entry (ACKed, which both shipped backends remove on ack and
//     which a hypothetical backend could retain) is returned AS STORED, flag
//     included: whether a closed entry is a task any more is the caller's
//     question, not this capability's;
//   - ErrAgentNotFound when the id names no registered agent, and an EMPTY
//     slice (no error) for a registered agent with an empty inbox: "this agent
//     has nothing queued" and "there is no such agent" are different facts and
//     a caller must be able to tell them apart;
//   - ErrInvalidStoreInput for a blank agent id, and the backend's own error
//     for a storage failure.
type InboxLister interface {
	PeekInbox(agentID string) ([]*InboxEntry, error)
}

// Compile-time capability assertions: both shipped backends can answer a
// listing, so the surfaces that need one (the A2A task-lifecycle binding,
// INT-A2A-004) work on the default in-memory backend and on PostgreSQL alike.
var (
	_ InboxLister = (*MemoryStore)(nil)
	_ InboxLister = (*PostgresStore)(nil)
)

// PeekInbox returns a SNAPSHOT of agentID's inbox in delivery order (oldest
// first), or ErrAgentNotFound when the id names no agent.
func (s *MemoryStore) PeekInbox(agentID string) ([]*InboxEntry, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, ok := s.agents[agentID]; !ok {
		return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}
	queue := s.inboxes[agentID]
	out := make([]*InboxEntry, 0, len(queue))
	for _, entry := range queue {
		out = append(out, cloneInboxEntry(entry))
	}
	return out, nil
}

// PeekInbox is the PostgreSQL half of InboxLister: one SELECT, no transaction,
// no FOR UPDATE and no lease columns written — it can never make a message
// unclaimable for its consumer. The rows come back in delivery order via the
// table's own delivery_sequence, so a listing is deterministic and matches the
// order Retrieve hands messages back in.
func (s *PostgresStore) PeekInbox(agentID string) ([]*InboxEntry, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	ctx, cancel := s.operationContext()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
SELECT id, agent_id, payload, COALESCE(sender, ''), COALESCE(idempotency_key, ''),
       created_at, expires_at, leased_at, lease_id, acked, priority
FROM inbox_entries
WHERE agent_id = $1
ORDER BY delivery_sequence;`, agentID)
	if err != nil {
		return nil, fmt.Errorf("peek inbox: %w", err)
	}
	defer rows.Close()

	out := make([]*InboxEntry, 0, 8)
	for rows.Next() {
		var (
			entry     InboxEntry
			expiresAt pgtype.Timestamptz
			leasedAt  *time.Time
			leaseID   *string
			acked     bool
			priority  int
		)
		if err := rows.Scan(&entry.ID, &entry.AgentID, &entry.Payload, &entry.Sender, &entry.IdempotencyKey,
			&entry.CreatedAt, &expiresAt, &leasedAt, &leaseID, &acked, &priority); err != nil {
			return nil, fmt.Errorf("peek inbox scan: %w", err)
		}
		// expires_at travels through pgtype.Timestamptz for the same reason
		// Peek reads it that way: a stored `infinity` (ttl_seconds=0 → never
		// expires, DF-CRIER-37) must decode instead of erroring.
		entry.ExpiresAt = expiryFromTimestamptz(expiresAt)
		entry.LeasedAt = leasedAt
		if leaseID != nil {
			entry.LeaseID = *leaseID
		}
		entry.ACKed = acked
		entry.Priority = priority
		out = append(out, &entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("peek inbox rows: %w", err)
	}
	if len(out) == 0 {
		// An empty listing is ambiguous on its own — "nothing queued" and "no
		// such agent" both produce no rows — so the agent row is checked
		// before an empty slice is reported as a successful read. It is one
		// extra query, and only on the empty path.
		var exists int
		if err := s.pool.QueryRow(ctx, `SELECT 1 FROM agents WHERE id = $1;`, agentID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
			}
			return nil, fmt.Errorf("peek inbox agent check: %w", err)
		}
	}
	return out, nil
}
