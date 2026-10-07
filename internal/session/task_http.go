// task_http.go — the TASK surface (CR-CHAT-030).
//
// specs/CHAT-ADDRESSING.md §2.5 NOT BUILT paragraph, now built:
//   - POST /sessions/{id}/tasks — create a TASK addressed to the correct
//     agents in the correct thread (the same D11 thread rules as a send: a
//     reply stays in its parent's thread).
//   - POST /sessions/{id}/tasks/{task_id}/claim and /complete — the
//     claim/running/done lifecycle (§2.5); each transition is a NEW
//     record-version over the same message id, and outputs (replies/updates)
//     land in the SAME thread so the human sees the agents' work there.
//
// Authority (specs/CHAT-PERMISSIONS.md §6.11): creating a TASK requires
// `invoke` on the TARGET plus `send` on the SESSION it is raised in — a
// different authority from sending a message. When the ACL is armed, an
// unauthorized creation is refused 403 TASK_FORBIDDEN reason NO_TASK_AUTHORITY,
// NEVER silently delivered as a message. The claim/complete transitions
// require `send` on the session (the actor acts in the room, on work already
// authorized into it).
//
// The fan-out follows the SHIPPED inbox path through sendMessage — no second
// delivery path (D1, §3.4).
package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/permissions"
)

// postTaskRequest is the POST /sessions/{id}/tasks body. The fields are the
// send body's addressing fields: payload (the instruction), targets (who is
// TASKED), and the thread anchoring. There is no message_kind field here —
// the route IS the task kind; a client cannot upgrade a kind on its own
// (CHAT-SESSIONS.md §4.4 rule 3).
type postTaskRequest struct {
	Payload        json.RawMessage  `json:"payload"`
	Sender         string           `json:"sender,omitempty"`
	PrincipalID    string           `json:"principal_id,omitempty"`
	AsAgent        string           `json:"as_agent,omitempty"`
	ThreadID       string           `json:"thread_id,omitempty"`
	ParentID       string           `json:"parent_id,omitempty"`
	Targets        []AudienceTarget `json:"targets,omitempty"`
	IdempotencyKey string           `json:"idempotency_key,omitempty"`
}

// taskTransitionRequest is the claim/complete body: it names the actor and
// optionally the output that lands in the thread with the transition.
type taskTransitionRequest struct {
	Actor     string          `json:"actor,omitempty"`
	ActorKind string          `json:"actor_kind,omitempty"` // agent | principal
	Payload   json.RawMessage `json:"payload,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	NewState  string          `json:"new_state,omitempty"` // complete: done | failed
	AsAgent   string          `json:"as_agent,omitempty"`
}

// HandleCreateTask serves POST /sessions/{id}/tasks.
func (h *Handler) HandleCreateTask(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.loadOpenScoped(w, r)
	if !ok {
		return
	}
	var req postTaskRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if h.opts.Deliverer == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "SESSION_DELIVERY_UNCONFIGURED",
			"no inbox deliverer is wired, so a task cannot be fanned out")
		return
	}
	if err := validatePayload(req.Payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_PAYLOAD", err.Error())
		return
	}
	author, ok := resolveAuthor(nil, req.Sender, req.PrincipalID, req.AsAgent)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "AUTHOR_REQUIRED",
			"sender (an agent id), or principal_id (optionally with as_agent), is required: a task records its author")
		return
	}

	// A TASK addresses an explicit set — there is no "everyone" default
	// (CHAT-ADDRESSING.md §1.3: an address that names nothing is not an
	// address, and execution is never guessed). A task with no targets is
	// refused, not defaulted to the room.
	if len(req.Targets) == 0 {
		writeAPIError(w, http.StatusBadRequest, "INVALID_ADDRESS",
			"a task addresses an explicit target set (targets); there is no everyone default for work (D12, §6.11)")
		return
	}
	for _, t := range req.Targets {
		if t.ID == "" {
			writeAPIError(w, http.StatusBadRequest, "INVALID_ADDRESS",
				"every task target needs an id")
			return
		}
	}

	// The TASK authority (§6.11): `invoke` on EACH target plus `send` on the
	// session. Armed-ACL only: an unarmed deployment keeps the shipped
	// trust-by-reach posture for every kind alike (the ACL states nothing).
	if !h.authorizeTask(w, r, sess, author, req.Targets) {
		return
	}

	// The task rides its message record: kind `task` + a lifecycle payload
	// with a fresh id (§3.7). The message id, thread and fan-out follow the
	// SAME §3.2 ordering and D11 thread rules as a send.
	taskID, err := h.newID()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ID_ERROR", err.Error())
		return
	}
	msgID, err := h.newID()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ID_ERROR", err.Error())
		return
	}
	now := h.now()
	msg := &Message{
		ID:        msgID,
		SessionID: sess.ID,
		Kind:      MessageTask,
		Author:    author,
		Payload:   req.Payload,
		CreatedAt: now,
		Task: Task{
			ID:        taskID,
			State:     TaskOpen,
			UpdatedAt: now,
		},
		IdempotencyKey: req.IdempotencyKey,
	}

	// Thread resolution: identical to HandlePostMessage's (D11) — a reply
	// stays in its parent's thread; a root's thread is its own id.
	if msg.ParentID != "" {
		st, err := h.store.Load(r.Context(), sess.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "STORE_ERROR", err.Error())
			return
		}
		parent := st.Message(msg.ParentID)
		if parent == nil {
			writeAPIError(w, http.StatusBadRequest, "PARENT_NOT_FOUND",
				fmt.Sprintf("parent_id %q is not in session %q", msg.ParentID, sess.ID))
			return
		}
		if strings.TrimSpace(req.ThreadID) != "" && req.ThreadID != parent.ThreadID {
			writeAPIError(w, http.StatusBadRequest, "THREAD_MISMATCH",
				fmt.Sprintf("thread_id %q does not match parent %q's thread %q (a reply never moves out of its thread)",
					req.ThreadID, msg.ParentID, parent.ThreadID))
			return
		}
		msg.ThreadID = parent.ThreadID
	} else {
		if strings.TrimSpace(req.ThreadID) != "" && req.ThreadID != msgID {
			writeAPIError(w, http.StatusBadRequest, "THREAD_MISMATCH",
				fmt.Sprintf("a thread root's thread_id must be its own message id %q", msgID))
			return
		}
		msg.ThreadID = msgID
	}

	aud, err := h.resolveAudience(r.Context(), sess, &postMessageRequest{Targets: req.Targets}, author.AgentID())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_AUDIENCE", err.Error())
		return
	}

	view, ok := h.sendMessage(w, r, sess, msg, aud, nil)
	if !ok {
		return
	}
	// The task is durable and fanned out: the optional dagger DAG trigger
	// (CR-CHAT-034) fires ONCE, fire-and-forget. The hook owns its own
	// failure handling — it records and logs, and it NEVER fails the task
	// the user asked for. A trigger-unarmed deployment (the default, nil)
	// runs nothing here.
	if h.taskTrigger != nil {
		go h.taskTrigger(taskID, sess.ID, string(req.Payload))
	}
	writeJSON(w, http.StatusCreated, view)
}

// authorizeTask runs §6.11's authority check: invoke on EACH target plus send
// on the session. It answers true when the task may proceed, having written
// the refusal (403 TASK_FORBIDDEN / NO_TASK_AUTHORITY) itself otherwise.
func (h *Handler) authorizeTask(w http.ResponseWriter, r *http.Request, sess *Session, author AuthorRef, targets []AudienceTarget) bool {
	if h.opts.Permissions == nil || !h.opts.Permissions.Armed() {
		return true
	}
	sender := senderOf(author)
	// send on the session the task is raised in.
	res, err := h.opts.Permissions.MayDeliver(r.Context(), permissions.CheckInput{
		Sender:  sender,
		Action:  permissions.ActionSend,
		Subject: permissions.Subject{Type: permissions.SubjectSession, Ref: sess.ID},
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "PERMISSIONS_ERROR", err.Error())
		return false
	}
	if !res.Allowed {
		h.writeTaskForbidden(w, res, "send", permissions.Subject{Type: permissions.SubjectSession, Ref: sess.ID})
		return false
	}
	// invoke on the TARGET — one check per target, never one check for the
	// set (§6.4 rule 1's per-target discipline).
	for _, t := range targets {
		st, ok := permissionsSubjectOf(t)
		if !ok {
			writeAPIError(w, http.StatusBadRequest, "INVALID_ADDRESS",
				fmt.Sprintf("target kind %q is not addressable as a task target", t.Kind))
			return false
		}
		res, err := h.opts.Permissions.MayDeliver(r.Context(), permissions.CheckInput{
			Sender:  sender,
			Action:  permissions.ActionInvoke,
			Subject: st,
		})
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "PERMISSIONS_ERROR", err.Error())
			return false
		}
		if !res.Allowed {
			h.writeTaskForbidden(w, res, string(permissions.ActionInvoke), st)
			return false
		}
	}
	return true
}

// writeTaskForbidden renders the §6.11 named refusal: 403 TASK_FORBIDDEN,
// reason NO_TASK_AUTHORITY — never a silent delivery as an ordinary message.
func (h *Handler) writeTaskForbidden(w http.ResponseWriter, res permissions.Result, action string, subject permissions.Subject) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"error":   "TASK_FORBIDDEN",
		"reason":  "NO_TASK_AUTHORITY",
		"action":  action,
		"subject": subject,
		"detail":  res.Detail,
	})
}

// permissionsSubjectOf maps an audience target to the ACL subject it is
// checked against. A principal target has no ACL subject of its own (a
// principal is reached through a bound agent, CR-CHAT-007) — a task addressed
// only to principals is refused as unaddressable-for-work.
func permissionsSubjectOf(t AudienceTarget) (permissions.Subject, bool) {
	switch t.Kind {
	case TargetAgent:
		return permissions.Subject{Type: permissions.SubjectAgent, Ref: t.ID}, true
	case TargetGroup:
		return permissions.Subject{Type: permissions.SubjectGroup, Ref: t.ID}, true
	case TargetCapability:
		return permissions.Subject{Type: permissions.SubjectCapability, Ref: t.ID}, true
	default:
		return permissions.Subject{}, false
	}
}

// senderOf renders an AuthorRef as the effective sender the ACL evaluates.
func senderOf(author AuthorRef) permissions.EffectiveSender {
	if p := author.Principal; p != "" {
		return permissions.EffectiveSender{Kind: permissions.SenderPrincipal, Principal: p, AsAgent: author.AsAgent}
	}
	if a := author.Agent; a != "" {
		return permissions.EffectiveSender{Kind: permissions.SenderAgent, AgentID: a}
	}
	return permissions.EffectiveSender{Kind: permissions.SenderAnonymous}
}

// findTask locates a task by id in a session's transcript and returns the
// message carrying its LATEST record-version plus that state.
func (h *Handler) findTask(r *http.Request, sess *Session, taskID string) (*Message, *Task, bool) {
	st, err := h.store.Load(r.Context(), sess.ID)
	if err != nil {
		return nil, nil, false
	}
	var taskMsg *Message
	var task *Task
	found := false
	for _, m := range st.Messages {
		if m.Kind == MessageTask && m.Task.ID == taskID {
			// The LATEST record-version wins (§3.7): the transcript holds
			// every transition, and the current state is the last one.
			t := m.Task
			taskMsg, task = m, &t
			found = true
		}
	}
	return taskMsg, task, found
}

// transitionTask serves POST /sessions/{id}/tasks/{task_id}/claim and
// /complete: the §2.5 lifecycle, one NEW record-version per transition over
// the same message id. The transition record is appended to the SAME thread —
// claim and complete records are thread messages (replies anchored to the
// task's message), so the human sees every lifecycle step and output in the
// thread.
func (h *Handler) transitionTask(w http.ResponseWriter, r *http.Request, to TaskState, complete bool) {
	sess, ok := h.loadOpenScoped(w, r)
	if !ok {
		return
	}
	vars := mux.Vars(r)
	taskID := vars["task_id"]
	var req taskTransitionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	taskMsg, task, found := h.findTask(r, sess, taskID)
	if !found {
		writeAPIError(w, http.StatusNotFound, "TASK_NOT_FOUND",
			fmt.Sprintf("task %q is not in session %q", taskID, sess.ID))
		return
	}
	// Actor: who moves the task (§6.11 — the transitions act in the room).
	var actor AuthorRef
	switch req.ActorKind {
	case "", "agent":
		if a := strings.TrimSpace(req.Actor); a != "" {
			actor = AuthorRef{Agent: a}
		} else if a := strings.TrimSpace(req.AsAgent); a != "" {
			actor = AuthorRef{Agent: a}
		}
	case "principal":
		if p := strings.TrimSpace(req.Actor); p != "" {
			actor = AuthorRef{Principal: p, AsAgent: strings.TrimSpace(req.AsAgent)}
		}
	}
	if actor.IsZero() {
		writeAPIError(w, http.StatusBadRequest, "AUTHOR_REQUIRED",
			"actor (an agent id, or principal_id with actor_kind=principal) is required: a transition records who made it")
		return
	}

	// The send authority on the session governs the transitions (§6.11).
	if h.opts.Permissions != nil && h.opts.Permissions.Armed() {
		res, err := h.opts.Permissions.MayDeliver(r.Context(), permissions.CheckInput{
			Sender:  senderOf(actor),
			Action:  permissions.ActionSend,
			Subject: permissions.Subject{Type: permissions.SubjectSession, Ref: sess.ID},
		})
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "PERMISSIONS_ERROR", err.Error())
			return
		}
		if !res.Allowed {
			h.writeTaskForbidden(w, res, string(permissions.ActionSend),
				permissions.Subject{Type: permissions.SubjectSession, Ref: sess.ID})
			return
		}
	}

	// Lifecycle legality (§2.5): a claim moves open → claimed (naming its
	// owner); complete moves claimed|running → done (or failed by
	// new_state). A transition out of a terminal state is refused.
	var next TaskState
	owner := task.Owner
	switch {
	case !complete:
		if task.State != TaskOpen {
			writeAPIError(w, http.StatusConflict, "INVALID_TASK_TRANSITION",
				fmt.Sprintf("task %q is %q; only an open task can be claimed", taskID, task.State))
			return
		}
		next = TaskClaimed
		owner = actorRefID(actor)
	default: // complete
		if task.State != TaskClaimed && task.State != TaskRunning {
			writeAPIError(w, http.StatusConflict, "INVALID_TASK_TRANSITION",
				fmt.Sprintf("task %q is %q; only a claimed or running task can be completed", taskID, task.State))
			return
		}
		next = TaskDone
		if s := strings.TrimSpace(req.NewState); s != "" {
			next = TaskState(s)
			if next != TaskDone && next != TaskFailed {
				writeAPIError(w, http.StatusBadRequest, "INVALID_TASK_TRANSITION",
					fmt.Sprintf("new_state must be %q or %q", TaskDone, TaskFailed))
				return
			}
		}
	}

	// The transition is a NEW record in the SAME thread: a reply anchored to
	// the task's message, kind `task`, carrying the payload of the NEXT
	// state. The human sees the agents' outputs in the thread (§2.5).
	updated := Task{
		ID:        task.ID,
		State:     next,
		Owner:     owner,
		UpdatedAt: h.now(),
	}
	msgID, err := h.newID()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "ID_ERROR", err.Error())
		return
	}
	msg := &Message{
		ID:        msgID,
		ParentID:  taskMsg.ID,
		ThreadID:  taskMsg.ThreadID, // a reply STAYS in the task's thread (D11)
		SessionID: sess.ID,
		Kind:      MessageTask,
		Author:    actor,
		Payload:   req.Payload,
		CreatedAt: updated.UpdatedAt,
		Task:      updated,
	}
	if msg.Payload == nil {
		msg.Payload = json.RawMessage(`{}`)
	}
	// The audience of a transition is the task's OWN audience — the agents
	// the task was addressed to see the lifecycle event (their outputs land
	// in the thread; the delivery loop keeps them notified).
	aud, err := h.resolveAudience(r.Context(), sess, &postMessageRequest{Targets: taskAudienceTargets(taskMsg)}, actor.AgentID())
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "INVALID_AUDIENCE", err.Error())
		return
	}
	view, ok := h.sendMessage(w, r, sess, msg, aud, nil)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"task_id": updated.ID,
		"state":   string(updated.State),
		"owner":   updated.Owner,
		"message": view,
	})
}

// taskAudienceTargets re-renders a task message's recorded audience as
// explicit targets (§4.2: the record carries the RESOLVED set, and a
// transition routes to exactly the set the task named).
func taskAudienceTargets(taskMsg *Message) []AudienceTarget {
	return taskMsg.Audience.Targets
}

// actorRefID renders an AuthorRef as the owner string a claimed task records.
func actorRefID(a AuthorRef) string {
	if a.Principal != "" {
		return a.Principal
	}
	return a.Agent
}

// HandleClaimTask serves POST /sessions/{id}/tasks/{task_id}/claim.
func (h *Handler) HandleClaimTask(w http.ResponseWriter, r *http.Request) {
	h.transitionTask(w, r, TaskClaimed, false)
}

// HandleCompleteTask serves POST /sessions/{id}/tasks/{task_id}/complete.
func (h *Handler) HandleCompleteTask(w http.ResponseWriter, r *http.Request) {
	h.transitionTask(w, r, TaskDone, true)
}
