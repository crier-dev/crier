package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/stretchr/testify/require"
)

// Boot the production composition root: a real task fires the trigger, its
// journal binds the task to the run, and only a persisted terminal observation
// authorizes an evidence-free completion request. The executor alone is a stub.
func TestTaskCompletionTriggerEvidenceEndToEnd(t *testing.T) {
	var succeeded atomic.Bool
	stub := daggerStub(t, "run-task-evidence", func() string {
		if succeeded.Load() {
			return "succeeded"
		}
		return "running"
	})
	dir := t.TempDir()
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG": "false", "CR_DAGGER_URL": stub.URL,
		"CR_DAGGER_STORE_DIR": dir, "CR_DAGGER_POLL_S": "0",
		"CR_DAGGER_TRIGGER_ON_TASK": "execute the task", "CR_DAGGER_TRIGGER_AGENT": "task-worker",
		"CR_SESSION_BACKEND": "jsonl", "CR_SESSION_LOG_ROOT": t.TempDir(),
	})
	client := &http.Client{Timeout: 5 * time.Second}
	code, body := postJSON(t, client, base+"/agents", `{"id":"task-worker"}`)
	require.Equal(t, http.StatusCreated, code, "%s", body)
	code, body = postJSON(t, client, base+"/sessions", `{"created_by":{"agent":"task-worker"},"members":[{"member_type":"agent","member_id":"task-worker","role":"member"}]}`)
	require.Equal(t, http.StatusCreated, code, "%s", body)
	var room struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &room))
	code, body = postJSON(t, client, base+"/sessions/"+room.ID+"/tasks", `{"sender":"task-worker","payload":{"text":"run tests"},"targets":[{"kind":"agent","id":"task-worker"}]}`)
	require.Equal(t, http.StatusCreated, code, "%s", body)
	var task struct {
		TaskID string `json:"task_id"`
	}
	require.NoError(t, json.Unmarshal(body, &task))
	url := base + "/sessions/" + room.ID + "/tasks/" + task.TaskID
	require.Eventually(t, func() bool {
		raw, err := os.ReadFile(filepath.Join(dir, "triggers.jsonl"))
		if err != nil {
			return false
		}
		var rec daggerctl.TriggerRecord
		return json.Unmarshal(raw, &rec) == nil && rec.TaskID == task.TaskID && rec.SessionID == room.ID && rec.RunID == "run-task-evidence"
	}, 5*time.Second, 10*time.Millisecond, "the real trigger never journalled this task's run")
	code, body = postJSON(t, client, url+"/claim", `{"actor":"task-worker"}`)
	require.Equal(t, http.StatusCreated, code, "%s", body)
	code, body = postJSON(t, client, url+"/complete", `{"actor":"task-worker"}`)
	require.Equal(t, http.StatusConflict, code, "running must not count as evidence: %s", body)
	require.Contains(t, string(body), "TASK_EVIDENCE_REQUIRED")
	succeeded.Store(true)
	code, body = getJSON(t, client, base+"/dagger/runs/run-task-evidence")
	require.Equal(t, http.StatusOK, code, "%s", body)
	require.Contains(t, string(body), `"state":"succeeded"`)
	code, body = postJSON(t, client, url+"/complete", `{"actor":"task-worker"}`)
	require.Equal(t, http.StatusCreated, code, "%s", body)
	// Prove the join is a persisted task snapshot: destroy the scratch journal
	// and read both task surfaces; neither may need the bridge at read time.
	require.NoError(t, os.Remove(filepath.Join(dir, "triggers.jsonl")))
	code, body = getJSON(t, client, url)
	require.Equal(t, http.StatusOK, code)
	var got struct {
		State    string `json:"state"`
		Versions int    `json:"versions"`
		Evidence struct {
			Source  string                  `json:"source"`
			Trigger daggerctl.TriggerRecord `json:"trigger"`
			Run     daggerctl.RunRecord     `json:"run"`
		} `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, "done", got.State)
	require.Equal(t, 3, got.Versions)
	require.Equal(t, "trigger_run", got.Evidence.Source)
	require.Equal(t, task.TaskID, got.Evidence.Trigger.TaskID)
	require.Equal(t, room.ID, got.Evidence.Trigger.SessionID)
	require.Equal(t, "run-task-evidence", got.Evidence.Run.RunID)
	require.Equal(t, daggerctl.StateSucceeded, got.Evidence.Run.State)
	require.Equal(t, []string{"ckpt/1"}, got.Evidence.Run.Evidence)
	code, body = getJSON(t, client, fmt.Sprintf("%s/sessions/%s/tasks", base, room.ID))
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), `"source":"trigger_run"`)
}
