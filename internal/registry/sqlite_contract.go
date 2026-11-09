package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// SQLiteStore's arm of the INBOX consumer contract (INBOX-CRIER-001).
//
// SQLite has no JSONB operators with the same spelling across the supported
// builds, so the contract is decoded in Go and the per-agent predicates stay
// plain SQL. The semantics are identical to the Postgres arm: unacknowledged,
// still-present messages past `created_at + contract TTL`, oldest first,
// bounded by contractSweepBatch per pass.

// sqliteContractAgents reads every agent's consumer contract in one pass.
func sqliteContractAgents(ctx context.Context, q *sqliteQueries) (map[string]*ConsumerContract, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, COALESCE(consumer,'') FROM agents`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]*ConsumerContract)
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if raw == "" {
			out[id] = nil
			continue
		}
		var c ConsumerContract
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			// A row written by something that bypassed validation is reported,
			// never silently treated as "no contract".
			return nil, fmt.Errorf("sqlite contract: agent %q: %w", id, err)
		}
		out[id] = &c
	}
	return out, rows.Err()
}

// ExpireContractOverdue is the contract half of the expiry sweep.
func (s *SQLiteStore) ExpireContractOverdue(now time.Time, defaultTTL time.Duration, report func(agentID string, entry *InboxEntry)) int {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var removed []*InboxEntry
	err := s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		contracts, err := sqliteContractAgents(ctx, q)
		if err != nil {
			return err
		}
		for agentID, contract := range contracts {
			ttl := contract.EffectiveTTL(defaultTTL)
			if ttl <= 0 {
				continue
			}
			remaining := contractSweepBatch - len(removed)
			if remaining <= 0 {
				break
			}
			cutoff := now.Add(-ttl).UnixNano()
			rows, err := q.QueryContext(ctx, `SELECT `+sqliteEntryColumns+` FROM inbox_entries
WHERE agent_id=? AND acked=FALSE AND created_at<=? AND (expires_at IS NULL OR expires_at>?)
ORDER BY created_at ASC, id ASC LIMIT ?`, agentID, cutoff, now.UnixNano(), remaining)
			if err != nil {
				return err
			}
			var candidates []*InboxEntry
			for rows.Next() {
				e, sErr := scanSQLiteEntry(rows)
				if sErr != nil {
					_ = rows.Close()
					return sErr
				}
				candidates = append(candidates, e)
			}
			rErr := rows.Err()
			_ = rows.Close()
			if rErr != nil {
				return rErr
			}
			for _, e := range candidates {
				// Delete guarded on acked=FALSE: a message acked between the
				// read and the delete is consumed, not expired.
				if _, err := q.ExecContext(ctx, `DELETE FROM inbox_entries WHERE id=? AND acked=FALSE`, e.ID); err != nil {
					return err
				}
				// The instant that elapsed is the contract deadline, which is
				// what the dead letter and receipt quote.
				e.ExpiresAt = e.CreatedAt.Add(ttl)
				removed = append(removed, e)
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("SQLite registry contract expiry", "error", err)
		return 0
	}
	for _, e := range removed {
		if report != nil {
			report(e.AgentID, e)
		}
	}
	return len(removed)
}

// StaleConsumers reports every agent whose oldest unacked message outlived its
// contract.
func (s *SQLiteStore) StaleConsumers(now time.Time, defaultTTL time.Duration) ([]StaleConsumer, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var out []StaleConsumer
	err := s.withTransaction("BEGIN", func(ctx context.Context, q *sqliteQueries) error {
		contracts, err := sqliteContractAgents(ctx, q)
		if err != nil {
			return err
		}
		for agentID, contract := range contracts {
			ttl := contract.EffectiveTTL(defaultTTL)
			if ttl <= 0 {
				continue
			}
			count, oldest, err := sqliteOldestUnacked(ctx, q, agentID, now)
			if err != nil {
				return err
			}
			if count == 0 || oldest.Add(ttl).After(now) {
				continue
			}
			reader := ""
			if contract != nil {
				reader = contract.Reader
			}
			out = append(out, StaleConsumer{
				AgentID:     agentID,
				Reader:      reader,
				ContractTTL: ttl,
				Unacked:     count,
				Oldest:      oldest,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite stale consumers: %w", err)
	}
	return out, nil
}

// OldestUnacked is the stats aging read.
func (s *SQLiteStore) OldestUnacked(agentID string) (int, time.Time, error) {
	var (
		count  int
		oldest time.Time
	)
	err := s.withTransaction("BEGIN", func(ctx context.Context, q *sqliteQueries) error {
		if _, err := sqliteGet(ctx, q, agentID); err != nil {
			return err
		}
		var qErr error
		count, oldest, qErr = sqliteOldestUnacked(ctx, q, agentID, time.Now())
		return qErr
	})
	if err != nil {
		return 0, time.Time{}, err
	}
	return count, oldest, nil
}

// sqliteOldestUnacked is the one definition of the aging read for this backend.
func sqliteOldestUnacked(ctx context.Context, q *sqliteQueries, agentID string, now time.Time) (int, time.Time, error) {
	var (
		count  int
		oldest sql.NullInt64
	)
	err := q.QueryRowContext(ctx, `SELECT count(*), min(created_at) FROM inbox_entries
WHERE agent_id=? AND acked=FALSE AND (expires_at IS NULL OR expires_at>=?)`, agentID, now.UnixNano()).Scan(&count, &oldest)
	if err != nil {
		return 0, time.Time{}, err
	}
	return count, sqliteAt(oldest), nil
}

var (
	_ ContractExpirer       = (*SQLiteStore)(nil)
	_ StaleConsumerReporter = (*SQLiteStore)(nil)
	_ InboxAgingReporter    = (*SQLiteStore)(nil)
)
