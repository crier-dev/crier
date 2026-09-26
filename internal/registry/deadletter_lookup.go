package registry

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DeadLetterLookup is an OPTIONAL Store capability: whether a durable failure
// record exists for ONE message (INT-A2A-004).
//
// It is deliberately not a second method on DeadLetterStore. ListDeadLetters is
// a bounded, newest-first READ of an archive — a page of dead letters — so it
// cannot answer "was THIS message id's expiry recorded?" once the record is
// older than a page, and answering "no record" from a window that simply did
// not reach far enough would turn a recorded failure into an invented
// not-found. A lookup is a different question, so it is a different capability,
// optional in the same way InboxPeeker and InboxLister are: a backend that
// cannot answer it (the remote proxy, which keeps no dead letters of its own)
// simply does not implement it.
//
// Contract for implementers: an id with no record returns (nil, false, nil) —
// "not recorded" is not an error — and a storage failure returns its own error
// so a caller can tell "no record" from "could not read".
type DeadLetterLookup interface {
	LookupDeadLetter(agentID, messageID string) (*DeadLetter, bool, error)
}

// Compile-time capability assertions: both shipped backends keep dead letters,
// so both can look one up.
var (
	_ DeadLetterLookup = (*MemoryStore)(nil)
	_ DeadLetterLookup = (*PostgresStore)(nil)
)

// LookupDeadLetter reports whether agentID's inbox has a durable failure record
// for one message id, the memory backend's half. It is a map probe on an index
// the destination already maintains — not a scan of the archive — so the answer
// does not depend on how much is stored.
func (s *MemoryStore) LookupDeadLetter(agentID, messageID string) (*DeadLetter, bool, error) {
	if agentID == "" || messageID == "" {
		return nil, false, fmt.Errorf("%w: blank agent ID or message ID", ErrInvalidStoreInput)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	rec, ok := s.deadLetters.byID[messageID]
	if !ok || rec.AgentID != agentID {
		return nil, false, nil
	}
	cp := *rec
	cp.Payload = append([]byte(nil), rec.Payload...)
	return &cp, true, nil
}

// LookupDeadLetter is the PostgreSQL half of DeadLetterLookup: one SELECT by
// (agent_id, message_id) — the dead_letters table's own primary-key predicate —
// so it answers in one round trip whatever the archive's size. It reads through
// the same column list and scan the listing path uses, so the two cannot
// disagree about what a record holds.
func (s *PostgresStore) LookupDeadLetter(agentID, messageID string) (*DeadLetter, bool, error) {
	if agentID == "" || messageID == "" {
		return nil, false, fmt.Errorf("%w: blank agent ID or message ID", ErrInvalidStoreInput)
	}
	ctx, cancel := s.operationContext()
	defer cancel()

	row := s.pool.QueryRow(ctx, `
SELECT `+deadLetterColumns+`
FROM dead_letters
WHERE agent_id = $1 AND message_id = $2;`, agentID, messageID)
	dl, err := scanDeadLetter(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lookup dead letter: %w", err)
	}
	return dl, true, nil
}
