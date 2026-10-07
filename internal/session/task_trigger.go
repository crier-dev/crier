// task_trigger.go — the task-created DAG trigger hook (CR-CHAT-034, outbound).
//
// The session surface OWNS the moment a task is created; the dagger trigger
// wants to know about it. The two meet on this one function type, the same
// opt-in-posture seam the federation client and the group store already use:
// a Handler with no trigger wired (the default, and every pre-CR-CHAT-034
// caller) fires nothing and behaves byte-identically to before.
package session

// TaskTriggerHook is the callback a dagger task trigger installs
// (CR-CHAT-034): after a task's transcript record is durable and its fan-out
// is accepted, the hook fires once with the task's id, its session and the
// payload the task carries. It is called fire-and-forget: a trigger failure
// is recorded by the trigger itself and NEVER fails the task creation.
type TaskTriggerHook func(taskID, sessionID, payload string)
