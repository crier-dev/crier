package registry

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// PostgresStore's arm of the INBOX consumer contract (INBOX-CRIER-001).
//
// The contract lives on the AGENT row (`consumer` JSONB, migration 020); the
// messages live on inbox_entries. Both the detector and the expiry therefore
// join the two tables in ONE statement rather than reading every agent into Go:
// the query is the policy, and it cannot drift from the sweep that enforces it.
//
// `contract_ttl_expr` is the single SQL spelling of ConsumerContract.
// EffectiveTTL: a JSON number in ttl_seconds is the override; absent or
// non-numeric (only possible from a writer that bypassed validation) falls back
// to the deployment default $2. A resolved 0 means "no contract bound" and is
// excluded by every predicate below.

// contractTTLExpr resolves an agent's effective contract TTL in seconds.
const contractTTLExpr = `COALESCE(
	CASE WHEN jsonb_typeof(a.consumer->'ttl_seconds') = 'number'
	     THEN (a.consumer->>'ttl_seconds')::bigint END,
	$2::bigint)`

// ExpireContractOverdue deletes the oldest unacknowledged messages whose agent
// contract deadline has elapsed, bounded to contractSweepBatch rows per pass
// and reported after the transaction commits (the PurgeExpiredReport
// discipline: a report that never runs never claims a removal that did not
// happen).
//
// The returned entry's ExpiresAt is the CONTRACT DEADLINE that elapsed, not the
// message's own expiry — for a never-expiring message that is the only instant
// that means anything, and it is what the dead letter and receipt quote.
func (s *PostgresStore) ExpireContractOverdue(now time.Time, defaultTTL time.Duration, report func(agentID string, entry *InboxEntry)) int {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ctx, cancel := s.operationContext()
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		slog.Error("postgres contract expiry", "error", err)
		return 0
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
WITH candidates AS (
    SELECT e.id AS id,
           e.created_at + make_interval(secs => (`+contractTTLExpr+`)::double precision) AS deadline
    FROM inbox_entries e
    JOIN agents a ON a.id = e.agent_id
    WHERE e.acked = FALSE
      AND (`+contractTTLExpr+`) > 0
      AND (e.expires_at = 'infinity' OR e.expires_at > $1)
      AND e.created_at + make_interval(secs => (`+contractTTLExpr+`)::double precision) <= $1
    ORDER BY e.created_at ASC, e.id ASC
    LIMIT $3
), deleted AS (
    DELETE FROM inbox_entries d
    USING candidates c
    WHERE d.id = c.id AND d.acked = FALSE
    RETURNING d.id, d.agent_id, d.payload, COALESCE(d.sender, ''),
              COALESCE(d.idempotency_key, ''), COALESCE(d.principal_id, ''),
              d.created_at, c.deadline
)
SELECT * FROM deleted;`, now, int64(defaultTTL/time.Second), contractSweepBatch)
	if err != nil {
		slog.Error("postgres contract expiry", "error", err)
		return 0
	}

	var removed []*InboxEntry
	for rows.Next() {
		var entry InboxEntry
		var deadline time.Time
		if err := rows.Scan(&entry.ID, &entry.AgentID, &entry.Payload, &entry.Sender,
			&entry.IdempotencyKey, &entry.PrincipalID, &entry.CreatedAt, &deadline); err != nil {
			rows.Close()
			slog.Error("postgres contract expiry: scan removed row", "error", err)
			return 0
		}
		entry.ExpiresAt = deadline.UTC()
		removed = append(removed, &entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("postgres contract expiry: rows", "error", err)
		return 0
	}
	if err := tx.Commit(ctx); err != nil {
		slog.Error("postgres contract expiry: commit", "error", err)
		return 0
	}

	for _, entry := range removed {
		if report != nil {
			report(entry.AgentID, entry)
		}
	}
	return len(removed)
}

// StaleConsumers reports every agent whose oldest unacknowledged message is
// older than its contract. One grouped read; the HAVING-equivalent is applied
// on the derived row because the contract TTL is itself a computed column.
func (s *PostgresStore) StaleConsumers(now time.Time, defaultTTL time.Duration) ([]StaleConsumer, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ctx, cancel := s.operationContext()
	defer cancel()

	rows, err := s.pool.Query(ctx, `
SELECT id, reader, ttl, unacked, oldest
FROM (
    SELECT a.id AS id,
           COALESCE(a.consumer->>'reader', '') AS reader,
           (`+contractTTLExpr+`) AS ttl,
           count(e.id) AS unacked,
           min(e.created_at) AS oldest
    FROM agents a
    JOIN inbox_entries e ON e.agent_id = a.id
    WHERE e.acked = FALSE
      AND (e.expires_at = 'infinity' OR e.expires_at > $1)
    GROUP BY a.id, a.consumer
) AS census
WHERE ttl > 0
  AND oldest + make_interval(secs => ttl::double precision) <= $1
ORDER BY oldest ASC;`, now, int64(defaultTTL/time.Second))
	if err != nil {
		return nil, fmt.Errorf("postgres stale consumers: %w", err)
	}
	defer rows.Close()

	var out []StaleConsumer
	for rows.Next() {
		var (
			sc    StaleConsumer
			ttl   int64
			count int64
		)
		if err := rows.Scan(&sc.AgentID, &sc.Reader, &ttl, &count, &sc.Oldest); err != nil {
			return nil, fmt.Errorf("postgres stale consumers: scan: %w", err)
		}
		sc.ContractTTL = time.Duration(ttl) * time.Second
		sc.Unacked = int(count)
		sc.Oldest = sc.Oldest.UTC()
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres stale consumers: rows: %w", err)
	}
	return out, nil
}

// OldestUnacked is the stats aging read: unacknowledged messages still in the
// inbox (never-expiring included) and the creation instant of the oldest.
func (s *PostgresStore) OldestUnacked(agentID string) (int, time.Time, error) {
	if agentID == "" {
		return 0, time.Time{}, fmt.Errorf("%w: blank agent ID", ErrInvalidStoreInput)
	}
	ctx, cancel := s.operationContext()
	defer cancel()

	var (
		count  int64
		oldest pgtype.Timestamptz
	)
	err := s.pool.QueryRow(ctx, `
SELECT count(*), min(e.created_at)
FROM inbox_entries e
WHERE e.agent_id = $1
  AND e.acked = FALSE
  AND (e.expires_at = 'infinity' OR e.expires_at > now());`, agentID).Scan(&count, &oldest)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, time.Time{}, nil
		}
		return 0, time.Time{}, fmt.Errorf("postgres oldest unacked: %w", err)
	}
	return int(count), expiryFromTimestamptz(oldest), nil
}

var (
	_ ContractExpirer       = (*PostgresStore)(nil)
	_ StaleConsumerReporter = (*PostgresStore)(nil)
	_ InboxAgingReporter    = (*PostgresStore)(nil)
)
