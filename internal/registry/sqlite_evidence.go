package registry

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func sqliteSaveObservation(ctx context.Context, q *sqliteQueries, id string, o TaskObservation) error {
	_, err := q.ExecContext(ctx, saveTaskObservationSQL, id, o.TaskID, sqliteTime(o.ObservedAt), o.Outcome, []byte(o.Evidence))
	return err
}
func (s *SQLiteStore) RecordTaskObservation(id string, o TaskObservation) error {
	if err := validTaskObservation(id, o); err != nil {
		return err
	}
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error { return sqliteSaveObservation(ctx, q, id, o) })
}
func scanSQLiteObservation(row sqliteScanner) (TaskObservation, error) {
	var o TaskObservation
	var at int64
	err := row.Scan(&o.TaskID, &at, &o.Outcome, &o.Evidence)
	o.ObservedAt = time.Unix(0, at).UTC()
	return o, err
}
func (s *SQLiteStore) TaskObservation(id, task string) (TaskObservation, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o, err := scanSQLiteObservation(s.db.QueryRowContext(ctx, `SELECT task_id,observed_at,outcome,evidence FROM a2a_task_observations WHERE agent_id=? AND task_id=?`, id, task))
	if errors.Is(err, sql.ErrNoRows) {
		return TaskObservation{}, false, nil
	}
	return o, err == nil, err
}
func (s *SQLiteStore) TaskObservations(id string) ([]TaskObservation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT task_id,observed_at,outcome,evidence FROM a2a_task_observations WHERE agent_id=? ORDER BY task_id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]TaskObservation, 0)
	for rows.Next() {
		o, err := scanSQLiteObservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (s *SQLiteStore) CloseObservedTask(id string, o TaskObservation) error {
	if err := validTaskObservation(id, o); err != nil {
		return err
	}
	if o.Outcome != "canceled" {
		return ErrInvalidStoreInput
	}
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		if err := sqliteClose(ctx, q, id, o.TaskID); err != nil {
			return err
		}
		return sqliteSaveObservation(ctx, q, id, o)
	})
}
func (s *SQLiteStore) RecordTaskOutput(id string, o TaskOutput) error {
	if err := validTaskOutput(id, o); err != nil {
		return err
	}
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		_, err := q.ExecContext(ctx, `INSERT INTO a2a_task_outputs(agent_id,task_id,observed_at,artifact) VALUES(?,?,?,?) ON CONFLICT(agent_id,task_id) DO NOTHING`, id, o.TaskID, sqliteTime(o.ObservedAt), []byte(o.Artifact))
		return err
	})
}
func (s *SQLiteStore) TaskOutput(id, task string) (TaskOutput, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	o := TaskOutput{TaskID: task}
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT observed_at,artifact FROM a2a_task_outputs WHERE agent_id=? AND task_id=?`, id, task).Scan(&at, &o.Artifact)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskOutput{}, false, nil
	}
	o.ObservedAt = time.Unix(0, at).UTC()
	return o, err == nil, err
}

var (
	_ TaskObservationStore = (*SQLiteStore)(nil)
	_ TaskOutputStore      = (*SQLiteStore)(nil)
)
