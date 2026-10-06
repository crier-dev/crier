package chat

import "errors"

var (
	// ErrInvalidTransition is returned when a task moves to a state that is
	// not a legal successor of its current state.
	ErrInvalidTransition = errors.New("chat: invalid task state transition")
	// ErrNotATask is returned when a transition is attempted on a message
	// that is not a TASK.
	ErrNotATask = errors.New("chat: not a task message")
)

// taskTransitions maps each task state to its legal successors. The only
// legal path is open→claimed→running→done/failed; anything else (including
// skipping states and going backwards) is refused.
var taskTransitions = map[TaskState][]TaskState{
	TaskOpen:    {TaskClaimed},
	TaskClaimed: {TaskRunning},
	TaskRunning: {TaskDone, TaskFailed},
	// done and failed are terminal.
}

// Transition returns a copy of m advanced to the requested task state.
//
// Only a TASK may be transitioned: any other kind returns ErrNotATask. A TASK
// may only move along the legal path above; anything else returns
// ErrInvalidTransition. Transitioning to the current state is also refused —
// a state change is a state change.
func Transition(m Message, to TaskState) (Message, error) {
	if m.Kind != KindTask {
		return Message{}, ErrNotATask
	}
	if !IsValidTaskState(to) || to == "" {
		return Message{}, ErrInvalidTransition
	}
	succs, ok := taskTransitions[m.TaskState]
	if !ok {
		return Message{}, ErrInvalidTransition
	}
	for _, s := range succs {
		if s == to {
			return Message{Kind: m.Kind, From: m.From, Thread: m.Thread, Body: m.Body, Addressees: m.Addressees, TaskState: to}, nil
		}
	}
	return Message{}, ErrInvalidTransition
}
