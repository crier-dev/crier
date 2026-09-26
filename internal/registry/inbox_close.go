package registry

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// InboxCloser is an OPTIONAL Store capability: CLOSE one inbox entry by id
// (INT-A2A-004).
//
// "Close" is the removal Ack performs, WITHOUT Ack's lease precondition: the
// entry is deleted from the agent's inbox whatever its lease state, and any
// outstanding lease on it goes with it (the row is gone, so there is nothing
// left to retrieve, acknowledge or revoke). It is neither a dead letter nor an
// expiry: nothing is recorded elsewhere, and no receipt is sent — a closed
// entry is a message the caller decided would not be worked, not a message
// crier gave up on.
//
// It exists because the A2A `CancelTask` operation (specs/A2A-OPTION.md §5.5.4)
// is performed by the ADAPTER on behalf of an A2A client that never held a
// lease: Ack's "each id must currently be leased under leaseID" rule cannot
// express it, and the alternatives are worse — leasing the entry first would
// hand a lease to nobody (leaving the message unackable until it lapsed), and
// combining RevokeLeases with Ack would still fail on a queued (unleased)
// message, which is exactly the case a cancel must reach.
//
// Error contract, shared by both backends:
//
//   - ErrMessageNotFound: the id is not in that agent's inbox (never delivered,
//     acknowledged, expired and swept, or already closed);
//   - ErrAgentNotFound: the id names no registered agent;
//   - ErrInvalidStoreInput: a blank agent or message id;
//   - the backend's own error on a storage failure.
//
// Nothing is removed unless the one requested message is removed: a close is
// never partial, and it never touches another entry.
type InboxCloser interface {
	CloseEntry(agentID, messageID string) error
}

// Compile-time capability assertions: both shipped backends can close an entry,
// so the A2A task-lifecycle surface (INT-A2A-004) cancels on the default
// in-memory backend and on PostgreSQL alike.
var (
	_ InboxCloser = (*MemoryStore)(nil)
	_ InboxCloser = (*PostgresStore)(nil)
)

// CloseEntry removes ONE message from agentID's inbox, whatever its lease state
// (InboxCloser). The memory backend's half.
func (s *MemoryStore) CloseEntry(agentID, messageID string) error {
	if agentID == "" || messageID == "" {
		return fmt.Errorf("%w: blank agent ID or message ID", ErrInvalidStoreInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.agents[agentID]; !ok {
		return fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
	}
	queue := s.inboxes[agentID]
	kept := make([]*InboxEntry, 0, len(queue))
	closed := false
	for _, entry := range queue {
		if entry.ID == messageID {
			closed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !closed {
		return fmt.Errorf("%w: %q is not in agent %q's inbox", ErrMessageNotFound, messageID, agentID)
	}
	s.inboxes[agentID] = kept
	return nil
}

// CloseEntry is the PostgreSQL half of InboxCloser: one DELETE by (agent_id,
// id), with no lease predicate — which is the whole difference from Ack's
// lease-scoped delete. The DELETEd row is read back by the statement itself
// (RETURNING id) so a close can never report success for a row it did not
// remove.
func (s *PostgresStore) CloseEntry(agentID, messageID string) error {
	if agentID == "" || messageID == "" {
		return fmt.Errorf("%w: blank agent ID or message ID", ErrInvalidStoreInput)
	}
	ctx, cancel := s.operationContext()
	defer cancel()

	var closed string
	err := s.pool.QueryRow(ctx, `
DELETE FROM inbox_entries
WHERE agent_id = $1 AND id = $2
RETURNING id;`, agentID, messageID).Scan(&closed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing was deleted: the message is not in that inbox, or the
			// agent does not exist. The two are different answers, and the
			// caller must be able to tell them apart, so the agent row is
			// checked before the not-found is reported.
			var exists int
			if checkErr := s.pool.QueryRow(ctx, `SELECT 1 FROM agents WHERE id = $1;`, agentID).Scan(&exists); checkErr != nil {
				if errors.Is(checkErr, pgx.ErrNoRows) {
					return fmt.Errorf("%w: %q", ErrAgentNotFound, agentID)
				}
				return fmt.Errorf("close entry agent check: %w", checkErr)
			}
			return fmt.Errorf("%w: %q is not in agent %q's inbox", ErrMessageNotFound, messageID, agentID)
		}
		return fmt.Errorf("close entry: %w", err)
	}
	return nil
}
