package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crier-dev/crier/internal/a2a"
)

// acknowledgementObservation uses the same evidence format as the A2A adapter.
// The task ID is the delivered inbox ID, never a payload-supplied task_id.
// Plain messages keep their existing hard-delete-only behavior. Build evidence
// before mutating memory, or inside the PostgreSQL deletion transaction.
func acknowledgementObservation(entry *InboxEntry, now time.Time) (TaskObservation, bool, error) {
	if _, ok := a2a.ParseEnvelope(entry.Payload); !ok {
		return TaskObservation{}, false, nil
	}
	ev := a2a.TaskEvidence{
		Entry: &a2a.InboxView{
			ID: entry.ID, Payload: entry.Payload, CreatedAt: entry.CreatedAt,
			ExpiresAt: entry.ExpiresAt, LeasedAt: entry.LeasedAt,
			LeaseID: entry.LeaseID, ACKed: entry.ACKed,
		},
		AcknowledgedAt: now,
	}
	raw, err := json.Marshal(ev)
	return TaskObservation{TaskID: entry.ID, ObservedAt: now, Outcome: "acknowledged", Evidence: raw}, true, err
}

// TaskObservation is evidence collected by the A2A adapter, not an inference
// from a missing inbox row. Evidence retains the pre-removal task snapshot.
// Records have no automatic expiry; MemoryStore loses them on restart, while
// PostgresStore keeps them in the inbox database until explicitly removed.
type TaskObservation struct {
	TaskID     string
	ObservedAt time.Time
	Outcome    string
	Evidence   json.RawMessage
}

// TaskObservationStore keeps observation lifetime independent of an HTTP stream.
// CloseObservedTask atomically closes an entry and records its cancellation.
// An unknown observation may be replaced by terminal evidence, never vice versa.
type TaskObservationStore interface {
	RecordTaskObservation(agentID string, observation TaskObservation) error
	TaskObservations(agentID string) ([]TaskObservation, error)
	TaskObservation(agentID, taskID string) (TaskObservation, bool, error)
	CloseObservedTask(agentID string, observation TaskObservation) error
}

func validTaskObservation(agentID string, o TaskObservation) error {
	if agentID == "" || o.TaskID == "" || o.ObservedAt.IsZero() || !json.Valid(o.Evidence) {
		return fmt.Errorf("%w: task observation requires agent, task, time and JSON evidence", ErrInvalidStoreInput)
	}
	switch o.Outcome {
	case "acknowledged", "canceled", "unknown":
		return nil
	default:
		return fmt.Errorf("%w: invalid task observation outcome", ErrInvalidStoreInput)
	}
}

func (s *MemoryStore) saveTaskObservation(agentID string, o TaskObservation) {
	if s.taskObservations == nil {
		s.taskObservations = make(map[inboxAckKey]TaskObservation)
	}
	key := inboxAckKey{agentID, o.TaskID}
	if prior, ok := s.taskObservations[key]; ok && prior.Outcome != "unknown" {
		return
	}
	o.Evidence = append(json.RawMessage(nil), o.Evidence...)
	s.taskObservations[key] = o
}

func (s *MemoryStore) RecordTaskObservation(agentID string, o TaskObservation) error {
	if err := validTaskObservation(agentID, o); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveTaskObservation(agentID, o)
	return nil
}

func (s *MemoryStore) TaskObservations(agentID string) ([]TaskObservation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TaskObservation, 0)
	for key, o := range s.taskObservations {
		if key.agentID == agentID {
			o.Evidence = append(json.RawMessage(nil), o.Evidence...)
			out = append(out, o)
		}
	}
	return out, nil
}

func (s *MemoryStore) CloseObservedTask(agentID string, o TaskObservation) error {
	if err := validTaskObservation(agentID, o); err != nil {
		return err
	}
	if o.Outcome != "canceled" {
		return fmt.Errorf("%w: close requires cancellation evidence", ErrInvalidStoreInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.closeEntryLocked(agentID, o.TaskID); err != nil {
		return err
	}
	s.saveTaskObservation(agentID, o)
	return nil
}

const saveTaskObservationSQL = `INSERT INTO a2a_task_observations (agent_id, task_id, observed_at, outcome, evidence)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (agent_id, task_id) DO UPDATE SET observed_at = EXCLUDED.observed_at, outcome = EXCLUDED.outcome, evidence = EXCLUDED.evidence
WHERE a2a_task_observations.outcome = 'unknown';`

func (s *PostgresStore) RecordTaskObservation(agentID string, o TaskObservation) error {
	if err := validTaskObservation(agentID, o); err != nil {
		return err
	}
	ctx, cancel := s.operationContext()
	defer cancel()
	_, err := s.pool.Exec(ctx, saveTaskObservationSQL, agentID, o.TaskID, o.ObservedAt, o.Outcome, []byte(o.Evidence))
	return err
}

func (s *PostgresStore) TaskObservations(agentID string) ([]TaskObservation, error) {
	ctx, cancel := s.operationContext()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT task_id, observed_at, outcome, evidence FROM a2a_task_observations WHERE agent_id = $1;`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TaskObservation, 0)
	for rows.Next() {
		var o TaskObservation
		if err := rows.Scan(&o.TaskID, &o.ObservedAt, &o.Outcome, &o.Evidence); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CloseObservedTask(agentID string, o TaskObservation) error {
	if err := validTaskObservation(agentID, o); err != nil {
		return err
	}
	if o.Outcome != "canceled" {
		return fmt.Errorf("%w: close requires cancellation evidence", ErrInvalidStoreInput)
	}
	ctx, cancel := s.operationContext()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if err := tx.QueryRow(ctx, `DELETE FROM inbox_entries WHERE agent_id = $1 AND id = $2 RETURNING id;`, agentID, o.TaskID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMessageNotFound
		}
		return err
	}
	if _, err := tx.Exec(ctx, saveTaskObservationSQL, agentID, o.TaskID, o.ObservedAt, o.Outcome, []byte(o.Evidence)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *AuthorityStore) RecordTaskObservation(agentID string, o TaskObservation) error {
	backend, ok := s.Store.(TaskObservationStore)
	if !ok {
		return fmt.Errorf("task observations unsupported by inbox backend")
	}
	return backend.RecordTaskObservation(agentID, o)
}
func (s *AuthorityStore) TaskObservations(agentID string) ([]TaskObservation, error) {
	backend, ok := s.Store.(TaskObservationStore)
	if !ok {
		return nil, nil
	}
	return backend.TaskObservations(agentID)
}
func (s *AuthorityStore) CloseObservedTask(agentID string, o TaskObservation) error {
	backend, ok := s.Store.(TaskObservationStore)
	if !ok {
		return fmt.Errorf("task observations unsupported by inbox backend")
	}
	return backend.CloseObservedTask(agentID, o)
}

var (
	_ TaskObservationStore = (*MemoryStore)(nil)
	_ TaskObservationStore = (*PostgresStore)(nil)
	_ TaskObservationStore = (*AuthorityStore)(nil)
)

// TaskObservation uses the composite primary key, not a full history scan.
func (s *MemoryStore) TaskObservation(agentID, taskID string) (TaskObservation, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, found := s.taskObservations[inboxAckKey{agentID, taskID}]
	o.Evidence = append(json.RawMessage(nil), o.Evidence...)
	return o, found, nil
}
func (s *PostgresStore) TaskObservation(agentID, taskID string) (TaskObservation, bool, error) {
	ctx, cancel := s.operationContext()
	defer cancel()
	o := TaskObservation{TaskID: taskID}
	err := s.pool.QueryRow(ctx, `SELECT observed_at, outcome, evidence FROM a2a_task_observations WHERE agent_id = $1 AND task_id = $2;`, agentID, taskID).Scan(&o.ObservedAt, &o.Outcome, &o.Evidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskObservation{}, false, nil
	}
	return o, err == nil, err
}
func (s *AuthorityStore) TaskObservation(agentID, taskID string) (TaskObservation, bool, error) {
	backend, ok := s.Store.(TaskObservationStore)
	if !ok {
		return TaskObservation{}, false, nil
	}
	return backend.TaskObservation(agentID, taskID)
}
