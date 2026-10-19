package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrA2AReplayPending = errors.New("A2A messageId already reserved; delivery outcome is pending or unconfirmed, retry later without changing messageId")

// A2AReplayStore owns the send reservation and receipt in the registry backend.
// Acquire commits BEFORE dispatch; an interrupted send remains pending rather
// than speculatively dispatching twice. Finish(nil) releases a known rejection.
// Tokens fence a slow writer from overwriting a newer, post-retention attempt.
// Expired records (pending included) are pruned on acquisition, on all backends.
type A2AReplayStore interface {
	AcquireA2AReplay(agentID, key string, now time.Time, window time.Duration) (token string, record []byte, err error)
	FinishA2AReplay(agentID, key, token string, record []byte) error
}

type a2aReplayRecord struct {
	token     string
	record    []byte
	expiresAt time.Time
}

func replayWindow(window time.Duration) time.Duration {
	if window <= 0 {
		return DefaultIdempotencyWindow
	}
	return window
}

func (s *MemoryStore) AcquireA2AReplay(agentID, key string, now time.Time, window time.Duration) (string, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.a2aReplays == nil {
		s.a2aReplays = make(map[inboxAckKey]a2aReplayRecord)
	}
	for k, record := range s.a2aReplays {
		if !now.Before(record.expiresAt) {
			delete(s.a2aReplays, k)
		}
	}
	k := inboxAckKey{agentID, key}
	if record, found := s.a2aReplays[k]; found {
		if record.record == nil {
			return "", nil, ErrA2AReplayPending
		}
		return "", append([]byte(nil), record.record...), nil
	}
	token, err := newLeaseID()
	if err != nil {
		return "", nil, err
	}
	s.a2aReplays[k] = a2aReplayRecord{token: token, expiresAt: now.Add(replayWindow(window))}
	return token, nil, nil
}

func (s *MemoryStore) FinishA2AReplay(agentID, key, token string, record []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := inboxAckKey{agentID, key}
	prior, found := s.a2aReplays[k]
	if !found || prior.token != token {
		return fmt.Errorf("A2A replay reservation lost")
	}
	if record == nil {
		delete(s.a2aReplays, k)
	} else {
		prior.record = append([]byte(nil), record...)
		s.a2aReplays[k] = prior
	}
	return nil
}

const replayInsert = `INSERT INTO a2a_message_replays(agent_id,message_key,token,expires_at,record) VALUES($1,$2,$3,$4,NULL) ON CONFLICT(agent_id,message_key) DO NOTHING`
const replaySelect = `SELECT token,record FROM a2a_message_replays WHERE agent_id=$1 AND message_key=$2`
const replayDelete = `DELETE FROM a2a_message_replays WHERE agent_id=$1 AND message_key=$2 AND token=$3`
const replayUpdate = `UPDATE a2a_message_replays SET record=$4 WHERE agent_id=$1 AND message_key=$2 AND token=$3`

func resolveA2AReservation(own, held string, record []byte) (string, []byte, error) {
	if own == held {
		return own, nil, nil
	}
	if record == nil {
		return "", nil, ErrA2AReplayPending
	}
	return "", record, nil
}

func (s *SQLiteStore) AcquireA2AReplay(agentID, key string, now time.Time, window time.Duration) (string, []byte, error) {
	token, err := newLeaseID()
	if err != nil {
		return "", nil, err
	}
	var held string
	var record []byte
	err = s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		if _, err := q.ExecContext(ctx, `DELETE FROM a2a_message_replays WHERE expires_at <= $1`, now.UnixNano()); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, replayInsert, agentID, key, token, now.Add(replayWindow(window)).UnixNano()); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, replaySelect, agentID, key).Scan(&held, &record)
	})
	if err != nil {
		return "", nil, err
	}
	return resolveA2AReservation(token, held, record)
}

func (s *SQLiteStore) FinishA2AReplay(agentID, key, token string, record []byte) error {
	return s.transaction(func(ctx context.Context, q *sqliteQueries) error {
		args := []any{agentID, key, token}
		query := replayDelete
		if record != nil {
			query = replayUpdate
			args = append(args, record)
		}
		result, err := q.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err == nil && n != 1 {
			return sql.ErrNoRows
		}
		return err
	})
}

func (s *PostgresStore) AcquireA2AReplay(agentID, key string, now time.Time, window time.Duration) (string, []byte, error) {
	token, err := newLeaseID()
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := s.operationContext()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(ctx, `DELETE FROM a2a_message_replays WHERE expires_at <= $1`, now.UnixNano()); err != nil {
		return "", nil, err
	}
	if _, err = tx.Exec(ctx, replayInsert, agentID, key, token, now.Add(replayWindow(window)).UnixNano()); err != nil {
		return "", nil, err
	}
	var held string
	var record []byte
	if err = tx.QueryRow(ctx, replaySelect, agentID, key).Scan(&held, &record); err != nil {
		return "", nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", nil, err
	}
	return resolveA2AReservation(token, held, record)
}

func (s *PostgresStore) FinishA2AReplay(agentID, key, token string, record []byte) error {
	ctx, cancel := s.operationContext()
	defer cancel()
	args := []any{agentID, key, token}
	query := replayDelete
	if record != nil {
		query = replayUpdate
		args = append(args, record)
	}
	result, err := s.pool.Exec(ctx, query, args...)
	if err == nil && result.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return err
}

func (s *AuthorityStore) AcquireA2AReplay(agentID, key string, now time.Time, window time.Duration) (string, []byte, error) {
	backend, ok := s.Store.(A2AReplayStore)
	if !ok {
		return "", nil, fmt.Errorf("A2A replay storage unsupported by registry backend")
	}
	return backend.AcquireA2AReplay(agentID, key, now, window)
}
func (s *AuthorityStore) FinishA2AReplay(agentID, key, token string, record []byte) error {
	backend, ok := s.Store.(A2AReplayStore)
	if !ok {
		return fmt.Errorf("A2A replay storage unsupported by registry backend")
	}
	return backend.FinishA2AReplay(agentID, key, token, record)
}

var (
	_ A2AReplayStore = (*MemoryStore)(nil)
	_ A2AReplayStore = (*SQLiteStore)(nil)
	_ A2AReplayStore = (*PostgresStore)(nil)
	_ A2AReplayStore = (*AuthorityStore)(nil)
)
