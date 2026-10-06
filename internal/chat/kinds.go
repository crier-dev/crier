// Package chat implements crier's message kinds: PLAIN (an ordinary thread
// message), ADDRESSED (carries @agent targets in the body — "this is for you",
// never an execution instruction), and TASK (the only kind that may create
// work, with its own lifecycle state). The load-bearing safety property is
// that a tag is structurally incapable of creating work: only an explicitly
// constructed TASK message carries task state.
package chat

import "errors"

// Kind enumerates the three message kinds.
type Kind string

const (
	KindPlain     Kind = "plain"
	KindAddressed Kind = "addressed"
	KindTask      Kind = "task"
)

// TaskState is the lifecycle state of a TASK message. Non-task messages
// always carry the empty state; any other value on a non-task message is a
// construction error.
type TaskState string

const (
	TaskOpen    TaskState = "open"
	TaskClaimed TaskState = "claimed"
	TaskRunning TaskState = "running"
	TaskDone    TaskState = "done"
	TaskFailed  TaskState = "failed"
)

// ValidTaskStates is the ordered lifecycle path of a task.
var ValidTaskStates = []TaskState{TaskOpen, TaskClaimed, TaskRunning, TaskDone, TaskFailed}

// IsValidTaskState reports whether s is a recognized task state (including
// the empty state, which means "not a task").
func IsValidTaskState(s TaskState) bool {
	if s == "" {
		return true
	}
	for _, v := range ValidTaskStates {
		if v == s {
			return true
		}
	}
	return false
}

// Named errors. Every refusal in this package is a named error so callers can
// classify it without string matching.
var (
	// ErrInvalidKind is returned when an unrecognized Kind is used.
	ErrInvalidKind = errors.New("chat: invalid kind")
	// ErrInvalidTaskState is returned when a TaskState value is not recognized.
	ErrInvalidTaskState = errors.New("chat: invalid task state")
	// ErrTaskStateOnNonTask is returned when a non-task message carries task state.
	ErrTaskStateOnNonTask = errors.New("chat: task state set on non-task message")
	// ErrAddresseesOnNonAddressed is returned when addressees are set on a
	// message that is not ADDRESSED.
	ErrAddresseesOnNonAddressed = errors.New("chat: addressees set on non-addressed message")
)

// Message is a kind-typed chat message. Kind is always required; TaskState
// may only be non-empty on a TASK, and Addressees may only be non-empty on an
// ADDRESSED message (a TASK's targets live in its body/tags, not here).
type Message struct {
	Kind       Kind      `json:"kind"`
	From       string    `json:"from"`
	Thread     string    `json:"thread"`
	Body       string    `json:"body"`
	Addressees []string  `json:"addressees,omitempty"`
	TaskState  TaskState `json:"task_state,omitempty"`
}

// NewMessage constructs a Message of the given kind, validating the kind and
// the kind/state invariants. For a TASK, an empty state defaults to open.
func NewMessage(k Kind, from, thread, body string, addressees []string, ts TaskState) (Message, error) {
	switch k {
	case KindPlain, KindAddressed, KindTask:
	default:
		return Message{}, ErrInvalidKind
	}
	if !IsValidTaskState(ts) {
		return Message{}, ErrInvalidTaskState
	}
	if k != KindTask && ts != "" {
		return Message{}, ErrTaskStateOnNonTask
	}
	if k == KindTask && ts == "" {
		ts = TaskOpen
	}
	if k != KindAddressed && len(addressees) > 0 {
		return Message{}, ErrAddresseesOnNonAddressed
	}
	return Message{Kind: k, From: from, Thread: thread, Body: body, Addressees: addressees, TaskState: ts}, nil
}
