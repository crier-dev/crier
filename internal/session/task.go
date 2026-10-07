// task.go — the TASK message kind and its lifecycle (CR-CHAT-030).
//
// specs/CHAT-ADDRESSING.md §2.5 (D12): a tag is ADDRESSING, never an action.
// Only the explicit `task` message kind may create work, and it is a DIFFERENT
// authority from sending a message (specs/CHAT-PERMISSIONS.md §6.11): creating
// one requires `invoke` on the TARGET plus `send` on the SESSION it sits in.
// A bare `addressed` tag from the same agent produces NO task record and NO
// execution — the kinds are data, not inference (CHAT-SESSIONS.md §4.4).
//
// Record shape (specs/CHAT-STORAGE.md §3.7): the task rides its message record
// as `task:{id, state, owner}` with `body.kind:"task"`. Every state transition
// is a NEW record-version (a fresh seq over the same message id, keep-LAST
// applied on read) — never an in-place rewrite, because the log is
// append-only and a rewritten transition is unauditable.
package session

import (
	"fmt"
	"time"
)

// TaskState is the closed task lifecycle vocabulary (§2.5, §3.7). A task is
// born `open`; `claim` moves it to `claimed` (naming its owner); `complete`
// moves it to `done`. `running` and `failed` are the reserved intermediate and
// terminal-failure states the vocabulary carries.
type TaskState string

const (
	TaskOpen    TaskState = "open"
	TaskClaimed TaskState = "claimed"
	TaskRunning TaskState = "running"
	TaskDone    TaskState = "done"
	TaskFailed  TaskState = "failed"
)

// TaskStates is the closed set, in lifecycle order. It is what ValidTaskState
// accepts and what a client can iterate.
var TaskStates = []TaskState{TaskOpen, TaskClaimed, TaskRunning, TaskDone, TaskFailed}

// ValidTaskState reports whether s is a member of the lifecycle vocabulary.
func ValidTaskState(s TaskState) bool {
	for _, want := range TaskStates {
		if s == want {
			return true
		}
	}
	return false
}

// Task is the lifecycle record a `task` message carries (§3.7). It is a value
// on the message, not a second store: the transcript IS the task's history,
// one record-version per transition.
type Task struct {
	ID    string    `json:"id"`
	State TaskState `json:"state"`
	// Owner names who holds the claimed task (the claimant's author ref id).
	// Absent while the task is open.
	Owner string `json:"owner,omitempty"`
	// UpdatedAt is the ts of the record-version that last moved the state.
	UpdatedAt time.Time `json:"updated_at"`
}

// validateTask checks the §3.7 shape at the record boundary: a task has an id
// and a state from the closed vocabulary.
func validateTask(t *Task) error {
	if t == nil {
		return nil
	}
	if t.ID == "" {
		return fmt.Errorf("%w: task without id", ErrInvalidRecord)
	}
	if !ValidTaskState(t.State) {
		return fmt.Errorf("%w: task %q has unknown state %q", ErrInvalidRecord, t.ID, t.State)
	}
	return nil
}

// checkTaskKindCoherence is the §3.7 structural rule: the `task` payload rides
// ONLY a `task`-kind message, and every `task`-kind message carries one. This
// is what makes the kinds structurally distinguishable in storage — a reader
// never guesses from the text (CHAT-SESSIONS.md §4.4 rule 3).
func checkTaskKindCoherence(kind MessageKind, t *Task) error {
	if kind == MessageTask {
		if t == nil {
			return fmt.Errorf("%w: a task-kind message without a task payload", ErrInvalidRecord)
		}
		return validateTask(t)
	}
	if t != nil {
		return fmt.Errorf("%w: message kind %q carries a task payload (task rides kind %q only)",
			ErrInvalidRecord, kind, MessageTask)
	}
	return nil
}
