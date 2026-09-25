package registry

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// This file is the PostgreSQL arm of the ownership capabilities (CR-FEAT-025):
// a purge that reports what it removed, the durable dead-letter destination, and
// transfer/reassign. The in-memory backend implements the same contracts in
// memory_ownership.go; both are asserted against the capability interfaces at
// the bottom of their files, so neither can drift into a shape the handler does
// not recognize.

// deadLetterColumns is the column list shared by every dead-letter read, in the
// order the scan below expects.
const deadLetterColumns = `message_id, agent_id, COALESCE(sender, ''), payload,
       created_at, expires_at, dead_lettered_at, reason, COALESCE(idempotency_key, '')`

// scanDeadLetter reads one dead-letter row into a record.
func scanDeadLetter(scan func(dest ...any) error) (*DeadLetter, error) {
	var (
		dl        DeadLetter
		expiresAt pgtype.Timestamptz
	)
	if err := scan(&dl.MessageID, &dl.AgentID, &dl.Sender, &dl.Payload,
		&dl.CreatedAt, &expiresAt, &dl.DeadLetteredAt, &dl.Reason, &dl.IdempotencyKey); err != nil {
		return nil, err
	}
	dl.ExpiredAt = expiryFromTimestamptz(expiresAt)
	return &dl, nil
}

// PurgeExpired deletes TTL-expired messages, releases elapsed leases, and
// removes dead letters past their retention window. Returns the number of
// deleted TTL-expired messages.
func (s *PostgresStore) PurgeExpired() int {
	return s.PurgeExpiredReport(nil)
}

// PurgeExpiredReport is PurgeExpired plus a report of what was removed
// (CR-FEAT-025): every message the sweep deleted is handed to `report` AFTER
// the deletion is committed, so a caller that dead-letters and notifies on the
// report can never resurrect a message that is still in the inbox, and a report
// that never runs (a commit failure) never claims a removal that did not
// happen.
//
// The rows are read back by the DELETE itself (… RETURNING), not by a separate
// SELECT: a delete-then-select pair could hand the caller a body that is no
// longer the one it removed, and the payload a dead letter preserves must be
// the payload that was actually deleted.
func (s *PostgresStore) PurgeExpiredReport(report func(agentID string, entry *InboxEntry)) int {
	ctx, cancel := s.operationContext()
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		slog.Error("postgres purge expired", "error", err)
		return 0
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()

	// expires_at <= now: a never-expiring message is stored as `infinity`
	// (ttl_seconds=0, DF-CRIER-37) and can never satisfy this predicate, so it
	// is never purged and never dead-lettered.
	rows, err := tx.Query(ctx, `
DELETE FROM inbox_entries
WHERE expires_at <= $1
RETURNING id, agent_id, payload, COALESCE(sender, ''), COALESCE(idempotency_key, ''),
          created_at, expires_at;`, now)
	if err != nil {
		slog.Error("postgres purge expired", "error", err)
		return 0
	}

	var removed []*InboxEntry
	for rows.Next() {
		var entry InboxEntry
		var expiresAt pgtype.Timestamptz
		if err := rows.Scan(&entry.ID, &entry.AgentID, &entry.Payload, &entry.Sender,
			&entry.IdempotencyKey, &entry.CreatedAt, &expiresAt); err != nil {
			rows.Close()
			slog.Error("postgres purge expired: scan removed row", "error", err)
			return 0
		}
		entry.ExpiresAt = expiryFromTimestamptz(expiresAt)
		removed = append(removed, &entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("postgres purge expired: rows", "error", err)
		return 0
	}

	_, err = tx.Exec(ctx, `
UPDATE inbox_entries
SET leased_at = NULL,
    lease_id = NULL,
    lease_expires_at = NULL
WHERE expires_at > $1
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at <= $1;`, now)
	if err != nil {
		slog.Error("postgres purge expired", "error", err)
		return 0
	}

	// Retention housekeeping: the dead-letter destination is bounded by TIME,
	// because it has no natural end — an operator that never reads it would
	// otherwise accumulate every message the relay ever dropped.
	if _, err := tx.Exec(ctx, `
DELETE FROM dead_letters
WHERE dead_lettered_at <= $1;`, now.Add(-DefaultDeadLetterRetention)); err != nil {
		slog.Error("postgres purge expired: dead-letter retention", "error", err)
		return 0
	}

	if err := tx.Commit(ctx); err != nil {
		slog.Error("postgres purge expired", "error", err)
		return 0
	}

	for _, entry := range removed {
		if report != nil {
			report(entry.AgentID, entry)
		}
	}
	return len(removed)
}

// AppendDeadLetter records a dead letter, reporting whether it was added
// (CR-FEAT-025). message_id is the primary key and a conflict is a NO-OP, so
// the same message cannot be recorded twice — the property the caller relies on
// to send exactly one expiry receipt per message.
func (s *PostgresStore) AppendDeadLetter(dl *DeadLetter) (bool, error) {
	if dl == nil || dl.MessageID == "" {
		return false, fmt.Errorf("%w: dead letter needs a message id", ErrInvalidStoreInput)
	}
	if dl.AgentID == "" {
		return false, fmt.Errorf("%w: dead letter needs a target agent", ErrInvalidStoreInput)
	}
	if dl.Reason == "" {
		return false, fmt.Errorf("%w: dead letter needs a reason", ErrInvalidStoreInput)
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	var id string
	err := s.pool.QueryRow(ctx, `
INSERT INTO dead_letters (
    message_id, agent_id, sender, payload, created_at, expires_at,
    dead_lettered_at, reason, idempotency_key
) VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9)
ON CONFLICT (message_id) DO NOTHING
RETURNING message_id;`,
		dl.MessageID, dl.AgentID, nullText(dl.Sender), dl.Payload,
		dl.CreatedAt, pgTimestamptz(dl.ExpiredAt), dl.DeadLetteredAt,
		dl.Reason, nullText(dl.IdempotencyKey),
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already recorded: the original expiry owns the record and its
		// receipt. This is not an error, it is the deduplication working.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("append dead letter: %w", err)
	}
	return true, nil
}

// ListDeadLetters returns up to limit dead letters for the given agent, newest
// first (CR-FEAT-025). The agent need not exist any more: the table is not
// foreign-keyed to agents precisely so a dead consumer's records survive it.
func (s *PostgresStore) ListDeadLetters(agentID string, limit int) ([]*DeadLetter, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	if limit > MaxDeadLetterListLimit {
		return nil, fmt.Errorf("%w: limit must be <= %d", ErrInvalidStoreInput, MaxDeadLetterListLimit)
	}
	if limit <= 0 {
		limit = DefaultDeadLetterListLimit
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
SELECT `+deadLetterColumns+`
FROM dead_letters
WHERE agent_id = $1
ORDER BY dead_lettered_at DESC, message_id DESC
LIMIT $2;`, agentID, limit)
	if err != nil {
		return nil, fmt.Errorf("list dead letters: %w", err)
	}
	defer rows.Close()

	out := make([]*DeadLetter, 0, limit)
	for rows.Next() {
		dl, err := scanDeadLetter(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list dead letters scan: %w", err)
		}
		out = append(out, dl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list dead letters rows: %w", err)
	}
	return out, nil
}

// Transfer moves messages from one agent's inbox into another's (CR-FEAT-025,
// Transferrer). It runs in ONE transaction: both agents are checked, every
// requested message is classified (missing / leased elsewhere), and the move
// happens with the same two error classes Ack uses — so a partial move is
// impossible and a stale lease cannot be displaced without `force`.
//
// The moved rows keep their id, delivery_sequence, payload, sender and expiry;
// only agent_id and the lease columns change, and the lease columns are cleared
// so the destination can claim the messages immediately. That is the whole
// point of the surface: a stuck lease becomes claimable work on a live worker
// without waiting out the lease or racing every other consumer for it.
//
// Error contract (shared with the in-memory backend): ErrInvalidStoreInput for
// a blank/identical agent, an empty id list or a duplicate id; ErrAgentNotFound
// when either agent is unknown; ErrMessageNotFound when a requested id is not in
// the source inbox; ErrLeaseConflict when a message is held under a DIFFERENT
// lease and force is not set. Nothing moves unless every requested message
// moves.
func (s *PostgresStore) Transfer(agentID, leaseID string, messageIDs []string, targetAgentID string, force bool) (int, error) {
	if agentID == "" || targetAgentID == "" {
		return 0, fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	if agentID == targetAgentID {
		return 0, fmt.Errorf("%w: transfer target %q is the source inbox", ErrInvalidStoreInput, targetAgentID)
	}
	if len(messageIDs) == 0 {
		return 0, fmt.Errorf("%w: message_ids must not be empty", ErrInvalidStoreInput)
	}
	seen := make(map[string]bool, len(messageIDs))
	for _, id := range messageIDs {
		if seen[id] {
			return 0, fmt.Errorf("%w: duplicate message ID %q", ErrInvalidStoreInput, id)
		}
		seen[id] = true
	}

	ctx, cancel := s.operationContext()
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("transfer begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Both agents must exist. FOR KEY SHARE mirrors the lock Retrieve/Ack take
	// on the agent row, so a concurrent unregister cannot slip between the
	// check and the move.
	for _, id := range []string{agentID, targetAgentID} {
		var agentCheck int
		err := tx.QueryRow(ctx, `
SELECT 1
FROM agents
WHERE id = $1
FOR KEY SHARE;`, id).Scan(&agentCheck)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, fmt.Errorf("%w: %q", ErrAgentNotFound, id)
			}
			return 0, fmt.Errorf("transfer agent check: %w", err)
		}
	}

	// Classify every requested id before moving anything: an id that is not in
	// this inbox is a not-found, and one held under a DIFFERENT lease is a
	// lease conflict unless the caller forced the move. An unleased message is
	// movable without force — it is claimable by any consumer anyway, so moving
	// it displaces nobody.
	lookup, err := tx.Query(ctx, `
SELECT id, COALESCE(lease_id, '') AS lease_id
FROM inbox_entries
WHERE agent_id = $1
  AND id = ANY($2::text[]);`, agentID, messageIDs)
	if err != nil {
		return 0, fmt.Errorf("transfer lookup: %w", err)
	}

	currentLease := make(map[string]string, len(messageIDs))
	for lookup.Next() {
		var (
			id       string
			existing string
		)
		if err := lookup.Scan(&id, &existing); err != nil {
			lookup.Close()
			return 0, fmt.Errorf("transfer lookup scan: %w", err)
		}
		currentLease[id] = existing
	}
	lookup.Close()
	if err := lookup.Err(); err != nil {
		return 0, fmt.Errorf("transfer lookup rows: %w", err)
	}

	var missing, mismatched []string
	for _, id := range messageIDs {
		current, ok := currentLease[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		if !force && current != "" && current != leaseID {
			mismatched = append(mismatched, id)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%w: message id(s) %s do not exist in agent %q's inbox (never delivered, already acknowledged, or expired)",
			ErrMessageNotFound, quoteMessageIDs(missing), agentID)
	}
	if len(mismatched) > 0 {
		return 0, fmt.Errorf("%w: message %q is not leased under lease %q (current lease: %q)",
			ErrLeaseConflict, mismatched[0], leaseID, currentLease[mismatched[0]])
	}

	// A forced move overrides the lease; an unforced one re-checks it in the
	// UPDATE itself (a live lease must still match, an unleased row is free to
	// move), so a lease that changed between the classification and the move
	// cannot be silently displaced.
	rows, err := tx.Query(ctx, `
UPDATE inbox_entries
SET agent_id = $3,
    leased_at = NULL,
    lease_id = NULL,
    lease_expires_at = NULL
WHERE agent_id = $1
  AND id = ANY($2::text[])
  AND ($4 OR lease_id IS NULL OR lease_id = $5)
RETURNING id;`, agentID, messageIDs, targetAgentID, force, leaseID)
	if err != nil {
		return 0, fmt.Errorf("transfer update: %w", err)
	}

	moved := 0
	for rows.Next() {
		moved++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("transfer update rows: %w", err)
	}
	if moved != len(messageIDs) {
		return 0, fmt.Errorf("%w: expected %d matching messages, got %d", ErrLeaseConflict, len(messageIDs), moved)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("transfer commit: %w", err)
	}
	return moved, nil
}

// Compile-time assertions: the PostgreSQL backend implements the ownership
// capabilities (CR-FEAT-025). A rename here must fail the build rather than
// silently drop the dead-letter destination or the transfer surface.
var (
	_ PurgeReporter   = (*PostgresStore)(nil)
	_ DeadLetterStore = (*PostgresStore)(nil)
	_ Transferrer     = (*PostgresStore)(nil)
)
