package daggerctl_test

// Task-created DAG trigger tests (CR-CHAT-034, outbound): exactly one
// CreateRun per task event when armed, zero when not, and a failing bridge
// records a named refusal without surfacing an error to the task path.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/daggerctl"
)

// TestTriggerFiresOncePerTask (acceptance 5a): one created task → exactly
// one CreateRun through the bridge, recorded with the run id.
func TestTriggerFiresOncePerTask(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	trigger := daggerctl.NewTaskTrigger(h.svc, "process the payload", "agent-a", t.TempDir())
	if trigger == nil {
		t.Fatal("an armed trigger built to nil")
	}

	trigger.OnTaskCreated("task-1", "sess-1", `{"job":"run tests"}`)

	creates := 0
	for _, c := range h.bridge.Calls() {
		if strings.HasPrefix(c, "create") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("bridge saw %d create call(s), want exactly 1", creates)
	}
	// The run is recorded like any other (the create went through the Service).
	rec, err := h.svc.RunStatus(t.Context(), "run-1")
	if err != nil {
		t.Fatalf("triggered run not recorded: %v", err)
	}
	if rec.State != daggerctl.StateRunning || rec.RequestingAgent != "agent-a" {
		t.Fatalf("triggered run record = %+v", rec)
	}
}

// TestTriggerZeroFiresWhenUnarmed (acceptance 5b): a nil trigger (not
// configured) fires nothing, and task creation is untouched.
func TestTriggerZeroFiresWhenUnarmed(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	var nilTrigger *daggerctl.TaskTrigger
	nilTrigger.OnTaskCreated("task-1", "sess-1", `{}`) // nil hook: the default deployment

	if creates := len(h.bridge.Calls()); creates != 0 {
		t.Fatalf("unarmed trigger made %d bridge call(s), want 0", creates)
	}
	// An empty prompt also refuses to arm at all.
	if daggerctl.NewTaskTrigger(h.svc, "", "agent-a", "") != nil {
		t.Fatal("an empty prompt armed a trigger")
	}
}

// TestTriggerFailureRecorded (acceptance 5c): a failing bridge records a
// NAMED refusal in the trigger log and does not return an error — the task
// creation path is never failed by the automation.
func TestTriggerFailureRecorded(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	h.bridge.createErr = fmt.Errorf("%w: dagger is down", daggerctl.ErrBridge)
	dir := t.TempDir()
	trigger := daggerctl.NewTaskTrigger(h.svc, "process", "agent-a", dir)

	trigger.OnTaskCreated("task-9", "sess-9", `{}`) // must NOT panic or error

	// The fire attempt is recorded with its named error kind.
	raw, err := os.ReadFile(filepath.Join(dir, "triggers.jsonl"))
	if err != nil {
		t.Fatalf("trigger log: %v", err)
	}
	var rec daggerctl.TriggerRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode trigger record: %v (%s)", err, raw)
	}
	if rec.TaskID != "task-9" || rec.Error == "" || rec.ErrorKind != "bridge" {
		t.Fatalf("trigger record = %+v, want the named bridge failure", rec)
	}
	if rec.RunID != "" {
		t.Fatalf("a failed fire recorded run id %q", rec.RunID)
	}
}
