package registry

import (
	"context"
	"log/slog"
	"time"
)

// PurgeExpiredDeadLetters removes retention-expired rows unless archive refuses.
func (s *SQLiteStore) PurgeExpiredDeadLetters(archive func(*DeadLetter) bool) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT `+sqliteDeadColumns+` FROM dead_letters WHERE dead_lettered_at<?`, time.Now().Add(-DefaultDeadLetterRetention).UnixNano())
	if err != nil {
		slog.Error("SQLite dead-letter archive selection", "error", err)
		return 0
	}
	var records []*DeadLetter
	for rows.Next() {
		dl, scanErr := scanSQLiteDead(rows)
		if scanErr != nil {
			_ = rows.Close()
			slog.Error("SQLite dead-letter archive scan", "error", scanErr)
			return 0
		}
		records = append(records, dl)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		slog.Error("SQLite dead-letter archive rows", "error", err)
		return 0
	}
	removed := 0
	for _, dl := range records {
		if archive != nil && !archive(dl) {
			continue
		}
		err = s.transaction(func(ctx context.Context, q *sqliteQueries) error {
			_, err := q.ExecContext(ctx, `DELETE FROM dead_letters WHERE message_id=? AND agent_id=?`, dl.MessageID, dl.AgentID)
			return err
		})
		if err != nil {
			slog.Error("SQLite dead-letter retention delete", "error", err)
			continue
		}
		removed++
	}
	return removed
}

// PurgeExpiredDeadLetters removes retention-expired rows unless archive refuses.
func (s *PostgresStore) PurgeExpiredDeadLetters(archive func(*DeadLetter) bool) int {
	ctx, cancel := s.operationContext()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT `+deadLetterColumns+` FROM dead_letters WHERE dead_lettered_at < $1`, time.Now().Add(-DefaultDeadLetterRetention))
	if err != nil {
		slog.Error("PostgreSQL dead-letter archive selection", "error", err)
		return 0
	}
	var records []*DeadLetter
	for rows.Next() {
		dl, scanErr := scanDeadLetter(rows.Scan)
		if scanErr != nil {
			rows.Close()
			slog.Error("PostgreSQL dead-letter archive scan", "error", scanErr)
			return 0
		}
		records = append(records, dl)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("PostgreSQL dead-letter archive rows", "error", err)
		return 0
	}
	removed := 0
	for _, dl := range records {
		if archive != nil && !archive(dl) {
			continue
		}
		command, err := s.pool.Exec(ctx, `DELETE FROM dead_letters WHERE message_id=$1 AND agent_id=$2`, dl.MessageID, dl.AgentID)
		if err != nil {
			slog.Error("PostgreSQL dead-letter retention delete", "error", err)
			continue
		}
		if command.RowsAffected() > 0 {
			removed++
		}
	}
	return removed
}

var (
	_ ArchiveHook = (*SQLiteStore)(nil)
	_ ArchiveHook = (*PostgresStore)(nil)
)
