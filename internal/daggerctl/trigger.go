// trigger.go — task-created DAG triggers (CR-CHAT-034, outbound direction).
//
// The board row's second direction: when a crier TASK is created, crier may
// kick off a DAG run that carries the task's work. This is OPT-IN wiring in
// the same posture as the control surface itself — with CR_DAGGER_TRIGGER_ON_TASK
// unset nothing here runs, and with it set the trigger is a thin CLIENT:
//
//   - ONE CreateRun per task-created event, through the existing bridge (the
//     Service, so the run is recorded like any other and its outcome still
//     travels the shipped inbox path);
//   - the run id reference is recorded ON the trigger record, in the run
//     store's directory, so an operator can join a task to the DAG it armed;
//   - a firing failure is recorded and named — logged AND journalled — and
//     NEVER fails the user's task creation: the task is the user's intent and
//     it already succeeded; the DAG trigger is an automation layered on it.
package daggerctl

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TriggerEnvVar is the opt-in variable naming the DAG PROMPT the trigger
// fires. Its presence (with a non-empty value, and a dagger bridge wired)
// turns the trigger on; anything else leaves it off.
const TriggerEnvVar = "CR_DAGGER_TRIGGER_ON_TASK"

// TaskTrigger is the task-created → CreateRun component (CR-CHAT-034).
// Safe for concurrent callers.
type TaskTrigger struct {
	// svc is the control surface the trigger fires THROUGH — never a bare
	// bridge — so a fired run is recorded exactly like a user-created one.
	svc *Service
	// prompt is the DAG prompt the task's payload is appended to. The task
	// payload is data, not a prompt: the operator's configured prompt names
	// WHAT the pipeline does, the payload is what it works on.
	prompt string
	// requestingAgent names the inbox a terminal outcome is delivered to.
	// Empty resolves to the task's author at fire time.
	requestingAgent string
	// nowDate is stamped on the record for the operator joining the two.
	now func() time.Time

	// dir is the directory the trigger log lives in (the run store's dir).
	// Empty disables journalling (memory-only counters, tests).
	dir string

	mu      sync.Mutex
	logPath string
}

// NewTaskTrigger builds the trigger from the resolved configuration. A nil
// service or an empty prompt returns nil: the trigger is OFF, not a stub
// that pretends to fire.
func NewTaskTrigger(svc *Service, prompt, requestingAgent, dir string) *TaskTrigger {
	if svc == nil || strings.TrimSpace(prompt) == "" {
		return nil
	}
	return &TaskTrigger{
		svc:             svc,
		prompt:          strings.TrimSpace(prompt),
		requestingAgent: strings.TrimSpace(requestingAgent),
		dir:             dir,
		now:             time.Now,
	}
}

// Enabled reports whether the trigger is armed (the env var is set with a
// non-empty value). The wiring uses it to decide whether to install the
// hook at all — an unconfigured trigger is an absent one.
func TriggerEnabled() bool {
	return strings.TrimSpace(os.Getenv(TriggerEnvVar)) != ""
}

// TriggerRecord is the persisted line for one fire attempt: the task that
// armed it, the run that answered (or the named refusal), and when.
// It is the audit surface: a fire that failed is a RECORDED line, never a
// silence, and it never appears as a success.
type TriggerRecord struct {
	TaskID    string    `json:"task_id"`
	SessionID string    `json:"session_id,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
	// ErrorKind names the refusal class when Error is set: "bridge" (the
	// executor refused), "unconfigured" (no bridge wired), or "error"
	// (anything else). Empty on a successful fire.
	ErrorKind string `json:"error_kind,omitempty"`
}

// OnTaskCreated fires ONE CreateRun for a created task. It never returns an
// error the caller must handle: the task creation has already succeeded, and
// a trigger failure is recorded (log + journal) — visible, named, and
// deliberately non-fatal.
func (t *TaskTrigger) OnTaskCreated(taskID, sessionID, payload string) {
	if t == nil {
		return
	}
	rec := TriggerRecord{TaskID: taskID, SessionID: sessionID, At: t.now()}
	prompt := t.prompt + "\n\nTask payload:\n" + payload
	req := CreateRunRequest{Prompt: prompt}
	if t.requestingAgent != "" {
		req.AgentID = t.requestingAgent
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultBridgeTimeout)
	defer cancel()
	view, err := t.svc.CreateRun(ctx, req)
	switch {
	case err == nil:
		rec.RunID = view.RunID
		slog.Info("dagger task trigger fired", "task_id", taskID, "run_id", view.RunID)
	case isErrUnconfigured(err):
		rec.Error, rec.ErrorKind = err.Error(), "unconfigured"
		slog.Warn("dagger task trigger: no bridge wired", "task_id", taskID, "error", err)
	default:
		rec.Error, rec.ErrorKind = err.Error(), "bridge"
		slog.Warn("dagger task trigger failed", "task_id", taskID, "error", err)
	}
	t.record(rec)
}

// isErrUnconfigured classifies the unconfigured refusal (a named 503 class)
// without string matching.
func isErrUnconfigured(err error) bool { return errors.Is(err, ErrUnconfigured) }

// record appends one trigger record to the JSONL log. A journalling failure
// is logged, never fatal — the slog line above already named the outcome.
func (t *TaskTrigger) record(rec TriggerRecord) {
	if t.dir == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.logPath == "" {
		t.logPath = filepath.Join(t.dir, "triggers.jsonl")
	}
	line, err := json.Marshal(rec)
	if err != nil {
		slog.Warn("dagger task trigger: encode record", "error", err)
		return
	}
	line = append(line, '\n')
	f, err := os.OpenFile(t.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Warn("dagger task trigger: open log", "path", t.logPath, "error", err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		slog.Warn("dagger task trigger: append", "path", t.logPath, "error", err)
		return
	}
	_ = f.Sync()
}
