package daggerctl_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/stretchr/testify/require"
)

func TestTaskCompletionEvidenceJournalJoin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    daggerctl.RunState
		linkLost bool
		want     bool
	}{
		{"running", daggerctl.StateRunning, false, false},
		{"unknown", daggerctl.StateUnknown, false, false},
		{"failed", daggerctl.StateFailed, false, false},
		{"cancelled", daggerctl.StateCancelled, false, false},
		{"link lost", daggerctl.StateSucceeded, true, false},
		{"succeeded", daggerctl.StateSucceeded, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := daggerctl.NewJSONLStore(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			bridge := &fakeBridge{}
			svc := daggerctl.NewService(bridge, store, nil)
			trigger := daggerctl.NewTaskTrigger(svc, "run tests", "agent-a", dir)
			trigger.OnTaskCreated("task-1", "session-1", `{}`)
			run, err := store.Get(t.Context(), "run-1")
			require.NoError(t, err)
			run.State, run.LinkLost = tc.state, tc.linkLost
			run.Evidence = []string{"artifact://test-output"}
			require.NoError(t, store.Append(t.Context(), run))
			// A new store/trigger must join from DISK, not a remembered callback.
			require.NoError(t, store.Close())
			reopened, err := daggerctl.NewJSONLStore(dir)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			reader := daggerctl.NewTaskTrigger(daggerctl.NewService(nil, reopened, nil), "run tests", "agent-a", dir)
			raw, err := reader.CompletionEvidence(t.Context(), "session-1", "task-1")
			require.NoError(t, err)
			if tc.want {
				var got struct {
					Source  string                  `json:"source"`
					Trigger daggerctl.TriggerRecord `json:"trigger"`
					Run     daggerctl.RunRecord     `json:"run"`
				}
				require.NoError(t, json.Unmarshal(raw, &got))
				require.Equal(t, "trigger_run", got.Source)
				require.Equal(t, "task-1", got.Trigger.TaskID)
				require.Equal(t, "session-1", got.Trigger.SessionID)
				require.Equal(t, "run-1", got.Trigger.RunID)
				require.Equal(t, *run, got.Run)
			} else {
				require.Empty(t, raw)
			}
			for _, ids := range [][2]string{{"session-2", "task-1"}, {"session-1", "task-2"}} {
				raw, err := reader.CompletionEvidence(t.Context(), ids[0], ids[1])
				require.NoError(t, err)
				require.Empty(t, raw, "an unrelated task/session must never borrow this run")
			}
			require.Equal(t, []string{"create"}, bridge.Calls(), "evidence read is read-only")
		})
	}
}

func TestTaskCompletionEvidenceFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, journal string
		wantErr       bool
	}{
		{"missing journal", "", false},
		{"missing run", `{"task_id":"task-1","session_id":"session-1","run_id":"absent"}`, false},
		{"failed fire", `{"task_id":"task-1","session_id":"session-1","run_id":"run-1","error":"bridge failed"}`, false},
		{"error kind", `{"task_id":"task-1","session_id":"session-1","run_id":"run-1","error_kind":"bridge"}`, false},
		{"malformed", `{`, true},
		{"torn tail", "{\"task_id\":\"task-1\",\"session_id\":\"session-1\",\"run_id\":\"run-1\"}\n{", true},
		{"newest attempt wins", "{\"task_id\":\"task-1\",\"session_id\":\"session-1\",\"run_id\":\"run-1\"}\n{\"task_id\":\"task-1\",\"session_id\":\"session-1\",\"error\":\"failed retry\"}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWaitHarness(t, t.TempDir())
			require.NoError(t, h.svc.Store().Append(t.Context(), &daggerctl.RunRecord{RunID: "run-1", State: daggerctl.StateSucceeded}))
			dir := t.TempDir()
			if tc.journal != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "triggers.jsonl"), []byte(tc.journal+"\n"), 0600))
			}
			trigger := daggerctl.NewTaskTrigger(h.svc, "run tests", "agent-a", dir)
			raw, err := trigger.CompletionEvidence(t.Context(), "session-1", "task-1")
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, raw)
		})
	}
}
