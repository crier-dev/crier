package daggerctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// CompletionEvidence joins the exact session/task to the run id recorded by
// OnTaskCreated, then reads the latest persisted executor observation. A caller
// cannot supply a run id. Running, failed, cancelled or link-lost runs never
// justify done. No bridge request or journal mutation occurs on this read.
// The snapshot becomes the task's own completion payload, so future reads do
// not depend on the continued availability of the executor or trigger log.
func (t *TaskTrigger) CompletionEvidence(ctx context.Context, sessionID, taskID string) (json.RawMessage, error) {
	if t == nil || t.dir == "" || t.svc == nil || t.svc.store == nil {
		return nil, nil
	}
	rec, err := t.readTaskTrigger(ctx, sessionID, taskID)
	if err != nil || rec == nil || rec.Error != "" || rec.ErrorKind != "" || rec.RunID == "" {
		return nil, err
	}
	run, err := t.svc.store.Get(ctx, rec.RunID)
	if errors.Is(err, ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read task trigger run %q: %w", rec.RunID, err)
	}
	if run == nil || run.RunID != rec.RunID || run.State != StateSucceeded || run.LinkLost {
		return nil, nil
	}
	return json.Marshal(struct {
		Source  string         `json:"source"`
		Trigger *TriggerRecord `json:"trigger"`
		Run     *RunRecord     `json:"run"`
	}{Source: "trigger_run", Trigger: rec, Run: run})
}

func (t *TaskTrigger) readTaskTrigger(ctx context.Context, sessionID, taskID string) (*TriggerRecord, error) {
	// Share the append mutex: never inspect a partly written in-process line.
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := os.Open(filepath.Join(t.dir, "triggers.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read task trigger journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var latest *TriggerRecord
	line := 0
	for scanner.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var rec TriggerRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("read task trigger journal line %d: %w", line, err)
		}
		if rec.SessionID == sessionID && rec.TaskID == taskID {
			latest = &rec
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read task trigger journal: %w", err)
	}
	return latest, nil
}
