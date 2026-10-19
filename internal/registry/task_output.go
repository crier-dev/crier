package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/crier-dev/crier/internal/a2a"
)

// TaskOutput is the first explicit agent-role output observed for an A2A task.
// It is separate from immutable acknowledgement/cancellation observations:
// output can arrive before OR after Ack without rewriting either fact.
// Like observations, memory lifetime is the backend's lifetime; PostgreSQL
// retains the snapshot across restart, with no automatic expiry.
type TaskOutput struct {
	TaskID     string
	ObservedAt time.Time
	Artifact   json.RawMessage
}

// TaskOutputStore is optional; lacking it never licenses an invented answer.
type TaskOutputStore interface {
	RecordTaskOutput(agentID string, output TaskOutput) error
	TaskOutput(agentID, taskID string) (TaskOutput, bool, error)
}

func validTaskOutput(agentID string, o TaskOutput) error {
	var artifact a2a.Artifact
	if agentID == "" || o.TaskID == "" || o.ObservedAt.IsZero() || json.Unmarshal(o.Artifact, &artifact) != nil || artifact.ArtifactID == "" || !a2a.HasOutputParts(artifact.Parts) {
		return fmt.Errorf("%w: task output requires agent, task, time and a nonempty artifact", ErrInvalidStoreInput)
	}
	return nil
}

func (s *MemoryStore) RecordTaskOutput(agentID string, o TaskOutput) error {
	if err := validTaskOutput(agentID, o); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taskOutputs == nil {
		s.taskOutputs = make(map[inboxAckKey]TaskOutput)
	}
	key := inboxAckKey{agentID, o.TaskID}
	if _, exists := s.taskOutputs[key]; !exists {
		o.Artifact = append(json.RawMessage(nil), o.Artifact...)
		s.taskOutputs[key] = o
	}
	return nil
}

func (s *MemoryStore) TaskOutput(agentID, taskID string) (TaskOutput, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, found := s.taskOutputs[inboxAckKey{agentID, taskID}]
	o.Artifact = append(json.RawMessage(nil), o.Artifact...)
	return o, found, nil
}

func (s *PostgresStore) RecordTaskOutput(agentID string, o TaskOutput) error {
	if err := validTaskOutput(agentID, o); err != nil {
		return err
	}
	ctx, cancel := s.operationContext()
	defer cancel()
	_, err := s.pool.Exec(ctx, `INSERT INTO a2a_task_outputs (agent_id, task_id, observed_at, artifact)
VALUES ($1, $2, $3, $4) ON CONFLICT (agent_id, task_id) DO NOTHING;`, agentID, o.TaskID, o.ObservedAt, []byte(o.Artifact))
	return err
}

func (s *PostgresStore) TaskOutput(agentID, taskID string) (TaskOutput, bool, error) {
	ctx, cancel := s.operationContext()
	defer cancel()
	o := TaskOutput{TaskID: taskID}
	err := s.pool.QueryRow(ctx, `SELECT observed_at, artifact FROM a2a_task_outputs WHERE agent_id = $1 AND task_id = $2;`, agentID, taskID).Scan(&o.ObservedAt, &o.Artifact)
	if errors.Is(err, pgx.ErrNoRows) {
		return TaskOutput{}, false, nil
	}
	return o, err == nil, err
}

func (s *AuthorityStore) RecordTaskOutput(agentID string, o TaskOutput) error {
	backend, ok := s.Store.(TaskOutputStore)
	if !ok {
		return fmt.Errorf("task outputs unsupported by inbox backend")
	}
	return backend.RecordTaskOutput(agentID, o)
}

func (s *AuthorityStore) TaskOutput(agentID, taskID string) (TaskOutput, bool, error) {
	backend, ok := s.Store.(TaskOutputStore)
	if !ok {
		return TaskOutput{}, false, nil
	}
	return backend.TaskOutput(agentID, taskID)
}

var (
	_ TaskOutputStore = (*MemoryStore)(nil)
	_ TaskOutputStore = (*PostgresStore)(nil)
	_ TaskOutputStore = (*AuthorityStore)(nil)
)
