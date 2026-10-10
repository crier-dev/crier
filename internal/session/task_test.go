// task_test.go — CR-CHAT-030: the agent-driven call-out.
//
// Acceptance criteria, each a measured test (not an assertion in prose):
//
//	a. an agent emits a TASK addressed to the correct agents in the correct
//	   thread; the task record is created with a lifecycle (§2.5, §6.11);
//	b. a bare `addressed` tag from the same agent creates NO task record and
//	   NO execution (D12 — a tag is addressing, never an action);
//	c. a TASK from a sender without the TASK authority is refused with the
//	   NAMED error 403 TASK_FORBIDDEN reason NO_TASK_AUTHORITY, never
//	   delivered as a message (§6.11);
//	d. the kinds are structurally distinguishable in storage (§3.7): a
//	   task-kind record carries its task payload, and the §3.7 coherence
//	   rule refuses a task payload on a non-task kind and its absence on a
//	   task kind;
//	e. the lifecycle: claim moves open→claimed naming its owner, complete
//	   moves claimed→done, a transition record lands in the SAME thread, and
//	   every transition is a NEW record-version over the same task id.
package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
)

// taskHarness is the apiHarness plus an armed ACL with the §6.11 authority
// rows seeded: prin_owner owns atlas, holds invoke on atlas, and send on the
// session; prin_stranger holds nothing. The task routes are registered on the
// harness router (registerSessionRoutes), so this drives the REAL surface.
type taskHarness struct {
	*apiHarness
	checker   *permissions.Checker
	permStore permissions.Store
}

// now2 is the test clock for permission records.
func (h *taskHarness) now2() time.Time { return time.Now().UTC() }

// seedGrant appends a grant to the harness's permission log.
func (h *taskHarness) seedGrant(t *testing.T, g *permissions.Grant) {
	t.Helper()
	now := h.now2()
	require.NoError(t, h.permStore.Append(t.Context(), g.Record(now)))
}

func newTaskHarness(t *testing.T, store Repository) *taskHarness {
	t.Helper()
	reg := registry.NewMemoryStore()
	for _, id := range []string{fixAgentA, fixAgentB} {
		require.NoError(t, reg.Register(&registry.Agent{ID: id}))
	}
	handler := NewHTTPHandler(store, HTTPOptions{Deliverer: reg, Agents: reg})

	// The ACL, armed: principals + the agent class rows + grants.
	now := time.Now().UTC()
	owner := &permissions.Principal{ID: "prin_owner", Kind: "principal", Status: permissions.PrincipalActive}
	stranger := &permissions.Principal{ID: "prin_stranger", Kind: "principal", Status: permissions.PrincipalActive}
	agentClass := &permissions.AgentInfo{ID: fixAgentA, Class: permissions.ClassPersonal, Owner: "prin_owner"}
	binding := &permissions.Binding{ID: "b_owner_atlas", Kind: "binding", Principal: "prin_owner", Agent: fixAgentA, AsAgent: true}
	permStore := memoryPermStore(t, []*permissions.Record{
		owner.Record(now),
		stranger.Record(now),
		agentClass.Record(now),
		binding.Record(now),
		(&permissions.Grant{
			ID: "g_invoke_atlas", Kind: "grant", Principal: "prin_owner",
			Subject:   permissions.Subject{Type: permissions.SubjectAgent, Ref: fixAgentA},
			Actions:   []permissions.Action{permissions.ActionInvoke},
			GrantedBy: "prin_owner", GrantedAt: now,
		}).Record(now),
	})
	checker := permissions.NewChecker(permStore)
	handler.opts.Permissions = checker
	r := mux.NewRouter()
	registerSessionRoutes(r, handler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	h := &apiHarness{srv: srv, reg: reg}
	return &taskHarness{apiHarness: h, checker: checker, permStore: permStore}
}

// newLocalJSONLStore is a JSONL store over a throwaway log dir.
func newLocalJSONLStore(t *testing.T) Repository {
	t.Helper()
	s, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// memoryPermStore is the in-memory permissions backend: a JSONL store over a
// temp dir, the shipped local backend for the CR-CHAT-003 log.
func memoryPermStore(t *testing.T, recs []*permissions.Record) permissions.Store {
	t.Helper()
	s, err := permissions.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	for _, rec := range recs {
		require.NoError(t, s.Append(t.Context(), rec))
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// createRoomAs posts the standard two-agent room as the owning principal and
// returns its id.
func (h *taskHarness) createRoomAs(t *testing.T) string {
	t.Helper()
	var created sessionView
	code := h.do(t, http.MethodPost, "/sessions", map[string]any{
		"title":        "task room",
		"principal_id": "prin_owner",
		"members": []map[string]string{
			{"member_type": "agent", "member_id": fixAgentA, "role": "member"},
			{"member_type": "agent", "member_id": fixAgentB, "role": "member"},
		},
	}, &created, nil)
	require.Equal(t, http.StatusCreated, code)

	// The send-on-session grant is seeded AFTER the room exists: the ACL's
	// subject refs are exact (§6.1), and the checker reads a FRESH snapshot
	// per call (§6.6), so the next task write sees it.
	h.seedGrant(t, &permissions.Grant{
		ID: "g_send_" + created.ID, Kind: "grant", Principal: "prin_owner",
		Subject:   permissions.Subject{Type: permissions.SubjectSession, Ref: created.ID},
		Actions:   []permissions.Action{permissions.ActionSend},
		GrantedBy: "prin_owner",
	})
	return created.ID
}

// AC-a: an agent emits a TASK addressed to the correct agents in the correct
// thread; the task record is created with a lifecycle.
func TestTaskCreatedWithLifecycle(t *testing.T) {
	h := newTaskHarness(t, newLocalJSONLStore(t))
	room := h.createRoomAs(t)

	var sent transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
		"principal_id": "prin_owner", "as_agent": "atlas",
		"payload": map[string]string{"text": "Run the data sweep for the new environment."},
		"targets": []map[string]string{{"kind": "agent", "id": fixAgentA}},
	}, &sent, nil)
	require.Equal(t, http.StatusCreated, code, "task create failed")

	// The record is structurally a TASK on the wire: kind task + a task
	// payload, not a bare tag.
	require.Equal(t, "task", sent.MessageKind)
	require.NotEmpty(t, sent.ID)
	require.NotEmpty(t, sent.ThreadID)
	require.Equal(t, sent.ID, sent.ThreadID, "a task opens its own thread")

	// The transcript carries it back with the lifecycle intact.
	var tr transcriptResponse
	code = h.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &tr, nil)
	require.Equal(t, http.StatusOK, code)
	var found *transcriptMessage
	for i := range tr.Messages {
		if tr.Messages[i].ID == sent.ID {
			found = &tr.Messages[i]
		}
	}
	require.NotNil(t, found, "task message not in transcript")
	require.Equal(t, "task", found.MessageKind)
	require.NotEmpty(t, found.Task, "task payload missing on the read-back")
	require.Equal(t, "open", found.Task["state"])
	require.NotEmpty(t, found.Task["task_id"])

	// The fan-out went through the shipped inbox path: the addressed agent
	// received a delivery (an outcome on the same record).
	require.NotEmpty(t, found.Outcomes, "a task must fan out through the inbox path")
}

// AC-b: a bare `addressed` tag from the same agent creates NO task record and
// NO execution.
func TestBareAddressedTagCreatesNoTask(t *testing.T) {
	h := newTaskHarness(t, newLocalJSONLStore(t))
	room := h.createRoomAs(t)

	// The SAME author sends an ADDRESSED message — a body tag naming atlas,
	// kind addressed. Nothing may execute.
	var sent transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/messages", map[string]any{
		"principal_id": "prin_owner", "as_agent": "atlas",
		"message_kind": "addressed",
		"payload":      map[string]string{"text": "@" + fixAgentA + " please run the data sweep."},
		"targets":      []map[string]string{{"kind": "agent", "id": fixAgentA}},
	}, &sent, nil)
	require.Equal(t, http.StatusCreated, code)

	var tr transcriptResponse
	code = h.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &tr, nil)
	require.Equal(t, http.StatusOK, code)
	for i := range tr.Messages {
		m := tr.Messages[i]
		if m.ID == sent.ID {
			require.Equal(t, "addressed", m.MessageKind, "the kind is data, not inference: an addressed message stays addressed")
			require.Empty(t, m.Task, "a bare tag must NOT create a task record (D12)")
		}
	}

	// And the room holds exactly the kinds it should: one addressed message,
	// zero tasks.
	taskCount, addressedCount := 0, 0
	for i := range tr.Messages {
		switch tr.Messages[i].MessageKind {
		case "task":
			taskCount++
		case "addressed":
			addressedCount++
		}
	}
	require.Equal(t, 0, taskCount, "a bare tag must create no task record")
	require.Equal(t, 1, addressedCount)
}

// AC-c: a TASK from a sender without the TASK authority is refused with the
// named error — not delivered as a message.
func TestTaskWithoutAuthorityRefusedNamed(t *testing.T) {
	h := newTaskHarness(t, newLocalJSONLStore(t))
	room := h.createRoomAs(t)

	// prin_stranger holds no grant: no invoke on the target, no send on the
	// session. §6.11's refusal, measured.
	var refused struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
		"principal_id": "prin_stranger",
		"as_agent":     "quill",
		"payload":      map[string]string{"text": "do the thing"},
		"targets":      []map[string]string{{"kind": "agent", "id": fixAgentA}},
	}, &refused, nil)
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "TASK_FORBIDDEN", refused.Error)
	require.Equal(t, "NO_TASK_AUTHORITY", refused.Reason)

	// The refusal is not a delivery: the transcript holds NO record of it.
	var tr transcriptResponse
	code = h.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &tr, nil)
	require.Equal(t, http.StatusOK, code)
	for i := range tr.Messages {
		require.NotEqual(t, "task", tr.Messages[i].MessageKind,
			"a refused task must never appear as a delivered message")
	}
}

// AC-c companion: the ACL is per-target — a task naming a target the sender
// cannot invoke is refused even when the session send is granted.
func TestTaskInvokeCheckedPerTarget(t *testing.T) {
	h := newTaskHarness(t, newLocalJSONLStore(t))
	room := h.createRoomAs(t)

	// The stranger's session-send is granted (a member may talk) but its
	// invoke on the target is not — the task must still be refused.
	_ = h.do(t, http.MethodPost, "/sessions/"+room+"/participants", map[string]any{
		"member_type": "principal", "member_id": "prin_stranger", "role": "member",
	}, nil, nil)

	var refused struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
		"principal_id": "prin_stranger",
		"as_agent":     "quill",
		"payload":      map[string]string{"text": "do the thing"},
		"targets":      []map[string]string{{"kind": "agent", "id": fixAgentA}},
	}, &refused, nil)
	require.Equal(t, http.StatusForbidden, code)
	require.Equal(t, "TASK_FORBIDDEN", refused.Error)
}

// AC-d: the §3.7 structural rules at the record boundary.
func TestTaskRecordStructuralRules(t *testing.T) {
	// A task-kind record without a task payload is malformed.
	rec := &Record{V: RecordFormatVersion, Type: RecordMessage, SessionID: "s", Seq: 1,
		TS: time.Now(), MessageID: "m1", ThreadID: "m1", MessageKind: MessageTask,
		Author: &AuthorRef{Agent: "atlas"}, Audience: &Audience{}}
	require.Error(t, rec.Validate(), "a task-kind record without its task payload must be refused")

	// A task payload on a plain record is malformed — a reader never guesses
	// a kind from a payload.
	rec2 := &Record{V: RecordFormatVersion, Type: RecordMessage, SessionID: "s", Seq: 2,
		TS: time.Now(), MessageID: "m2", ThreadID: "m2", MessageKind: MessagePlain,
		Task: &Task{ID: "task_x", State: TaskOpen}, Author: &AuthorRef{Agent: "atlas"}, Audience: &Audience{}}
	require.Error(t, rec2.Validate(), "a plain record carrying a task payload must be refused")

	// A task payload with an unknown state is malformed.
	rec3 := &Record{V: RecordFormatVersion, Type: RecordMessage, SessionID: "s", Seq: 3,
		TS: time.Now(), MessageID: "m3", ThreadID: "m3", MessageKind: MessageTask,
		Task: &Task{ID: "task_y", State: "zork"}, Author: &AuthorRef{Agent: "atlas"}, Audience: &Audience{}}
	require.Error(t, rec3.Validate(), "an unknown task state must be refused")

	// The well-formed task record round-trips through the JSONL log with its
	// lifecycle intact.
	rec4 := &Record{V: RecordFormatVersion, Type: RecordMessage, SessionID: "s", Seq: 4,
		TS: time.Now().UTC(), MessageID: "m4", ThreadID: "m4", MessageKind: MessageTask,
		Task: &Task{ID: "task_z", State: TaskOpen}, Author: &AuthorRef{Agent: "atlas"}, Audience: &Audience{}}
	require.NoError(t, rec4.Validate())
	line, err := rec4.MarshalLine()
	require.NoError(t, err)
	parsed, err := ParseRecord(line)
	require.NoError(t, err)
	require.NotNil(t, parsed.Task)
	require.Equal(t, TaskOpen, parsed.Task.State)
	require.Equal(t, "task_z", parsed.Task.ID)
}

// AC-e: the lifecycle — claim names its owner, complete closes it, every
// transition is a NEW record-version in the SAME thread, and the outputs land
// in that thread.
func TestTaskLifecycleClaimCompleteInThread(t *testing.T) {
	h := newTaskHarness(t, newLocalJSONLStore(t))
	room := h.createRoomAs(t)

	var sent transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
		"principal_id": "prin_owner", "as_agent": "atlas",
		"payload": map[string]string{"text": "Sweep the data."},
		"targets": []map[string]string{{"kind": "agent", "id": fixAgentA}},
	}, &sent, nil)
	require.Equal(t, http.StatusCreated, code)
	taskID := sent.Task["task_id"].(string)
	require.NotEmpty(t, taskID)

	// Claim.
	var claimed struct {
		TaskID  string            `json:"task_id"`
		State   string            `json:"state"`
		Owner   string            `json:"owner"`
		Message transcriptMessage `json:"message"`
	}
	code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+taskID+"/claim", map[string]any{
		"actor":   fixAgentA,
		"payload": map[string]string{"text": "Claiming — starting the sweep."},
	}, &claimed, nil)
	require.Equal(t, http.StatusCreated, code, "claim failed")
	require.Equal(t, taskID, claimed.TaskID)
	require.Equal(t, "claimed", claimed.State)
	require.Equal(t, fixAgentA, claimed.Owner)

	// The claim record is a reply INSIDE the task's thread (D11: a reply
	// stays in thread; the human sees the agent's output there).
	require.Equal(t, sent.ThreadID, claimed.Message.ThreadID)
	require.Equal(t, sent.ID, claimed.Message.ParentID)

	// Complete.
	var done struct {
		TaskID  string            `json:"task_id"`
		State   string            `json:"state"`
		Message transcriptMessage `json:"message"`
	}
	code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+taskID+"/complete", map[string]any{
		"actor":   fixAgentA,
		"payload": map[string]string{"text": "Sweep done: 42 rows moved."},
	}, &done, nil)
	require.Equal(t, http.StatusCreated, code, "complete failed")
	require.Equal(t, "done", done.State)
	require.Equal(t, sent.ThreadID, done.Message.ThreadID, "the completion lands in the same thread")

	// The transcript now shows ONE task id in THREE record-versions —
	// open, claimed, done — each its own message, in seq order, the
	// append-only lifecycle §3.7 fixes.
	var tr transcriptResponse
	code = h.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &tr, nil)
	require.Equal(t, http.StatusOK, code)
	var versions []map[string]any
	for i := range tr.Messages {
		if tr.Messages[i].MessageKind == "task" && tr.Messages[i].Task != nil {
			if tr.Messages[i].Task["task_id"] == taskID {
				versions = append(versions, tr.Messages[i].Task)
			}
		}
	}
	require.Len(t, versions, 3, "open + claimed + done record-versions expected")
	require.Equal(t, "open", versions[0]["state"])
	require.Equal(t, "claimed", versions[1]["state"])
	require.Equal(t, "done", versions[2]["state"])
}

// The lifecycle's illegal transitions are refused with named errors.
func TestTaskLifecycleIllegalTransitions(t *testing.T) {
	h := newTaskHarness(t, newLocalJSONLStore(t))
	room := h.createRoomAs(t)

	var sent transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
		"principal_id": "prin_owner", "as_agent": "atlas",
		"payload": map[string]string{"text": "Sweep the data."},
		"targets": []map[string]string{{"kind": "agent", "id": fixAgentA}},
	}, &sent, nil)
	require.Equal(t, http.StatusCreated, code)
	taskID := sent.Task["task_id"].(string)

	// Completing an OPEN task is refused (it must be claimed first).
	var refused struct {
		Error string `json:"error"`
	}
	code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+taskID+"/complete", map[string]any{
		"actor": fixAgentA,
	}, &refused, nil)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "INVALID_TASK_TRANSITION", refused.Error)

	// A claim of a task that does not exist is a named 404.
	code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/nope/claim", map[string]any{
		"actor": fixAgentA,
	}, &refused, nil)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, "TASK_NOT_FOUND", refused.Error)

	// A double claim is refused — claimed is not open.
	var claimed struct {
		State string `json:"state"`
	}
	code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+taskID+"/claim", map[string]any{
		"actor": fixAgentA,
	}, &claimed, nil)
	require.Equal(t, http.StatusCreated, code)
	code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+taskID+"/claim", map[string]any{
		"actor": fixAgentA, // the SAME claimant: a second claim fails on STATE, not authority
	}, &refused, nil)
	require.Equal(t, http.StatusConflict, code)
	require.Equal(t, "INVALID_TASK_TRANSITION", refused.Error)
}

// The SQL view serves the same task lifecycle the JSONL log does (§2.1: the
// dual backends are measured against ONE State).
func TestTaskLifecycleSQLStoreRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("SQL battery skipped in -short")
	}
	for _, sc := range localBackends() {
		t.Run(sc.name, func(t *testing.T) {
			store := sc.open(t, sc.pathFn(t))
			h := newTaskHarness(t, store)
			room := h.createRoomAs(t)

			var sent transcriptMessage
			code := h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
				"principal_id": "prin_owner", "as_agent": "atlas",
				"payload": map[string]string{"text": "Sweep."},
				"targets": []map[string]string{{"kind": "agent", "id": fixAgentA}},
			}, &sent, nil)
			require.Equal(t, http.StatusCreated, code)
			taskID := sent.Task["task_id"].(string)

			code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+taskID+"/claim", map[string]any{
				"actor": fixAgentA,
			}, nil, nil)
			require.Equal(t, http.StatusCreated, code)
			code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+taskID+"/complete", map[string]any{
				"actor": fixAgentA,
			}, nil, nil)
			require.Equal(t, http.StatusCreated, code)

			// The view read-back: latest version per (session, message) is
			// `done`, and the transcript holds all three record-versions.
			var tr transcriptResponse
			code = h.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &tr, nil)
			require.Equal(t, http.StatusOK, code)
			states := map[string]string{}
			count := 0
			for i := range tr.Messages {
				if tr.Messages[i].MessageKind == "task" && tr.Messages[i].Task != nil && tr.Messages[i].Task["task_id"] == taskID {
					count++
					states[tr.Messages[i].ID] = tr.Messages[i].Task["state"].(string)
				}
			}
			require.Equal(t, 3, count)
			// One message per state: the three record-versions are three
			// distinct messages, each carrying its own version's state.
			got := map[string]bool{}
			for _, s := range states {
				got[s] = true
			}
			require.True(t, got["open"] && got["claimed"] && got["done"], "states: %v", states)
		})
	}
}

// taskCreateView is the POST /sessions/{id}/tasks 201 body as a caller reads
// it: the top-level task id + state DF-CRIER-304 added, plus the message fields
// and the nested task payload that must survive the change.
type taskCreateView struct {
	ID       string         `json:"id"`
	ThreadID string         `json:"thread_id"`
	TaskID   string         `json:"task_id"`
	State    string         `json:"state"`
	Task     map[string]any `json:"task"`
}

// taskReadView is one task as GET /sessions/{id}/tasks/{task_id} renders it.
type taskReadView struct {
	SessionID string    `json:"session_id"`
	TaskID    string    `json:"task_id"`
	State     string    `json:"state"`
	Owner     string    `json:"owner"`
	UpdatedAt time.Time `json:"updated_at"`
	MessageID string    `json:"message_id"`
	ThreadID  string    `json:"thread_id"`
	Versions  int       `json:"versions"`
}

// taskListView is the GET /sessions/{id}/tasks body.
type taskListView struct {
	SessionID string         `json:"session_id"`
	Tasks     []taskReadView `json:"tasks"`
	Count     int            `json:"count"`
}

// DF-CRIER-304: the task lifecycle is OBSERVABLE, end to end, over the real
// router. Before this row the create response buried the task id at
// `task.task_id` (a caller reading the obvious top-level `state` saw null, and
// claiming the top-level `id` — the MESSAGE id — was refused TASK_NOT_FOUND on
// a task created moments earlier), and there was no read route at all (GET
// /tasks was 405, GET /tasks/{id} 404). This drives create → read → claim →
// read → complete → read, on the JSONL log always and the SQLite view when the
// suite is not -short (the dogfood ran on the SQL view, so it is the backend
// that matters most).
func TestTaskLifecycle_Verifiable(t *testing.T) {
	backends := localBackends()
	t.Run(backends[0].name, func(t *testing.T) { runTaskLifecycleVerifiable(t, backends[0]) })
	t.Run(backends[1].name, func(t *testing.T) {
		if testing.Short() {
			t.Skip("SQL view skipped in -short")
		}
		runTaskLifecycleVerifiable(t, backends[1])
	})
}

func runTaskLifecycleVerifiable(t *testing.T, b localBackend) {
	t.Helper()
	s := b.open(t, b.pathFn(t))
	if c, ok := s.(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = c.Close() })
	}
	h := newTaskHarness(t, s)
	room := h.createRoomAs(t)

	// AC1 — the 201 names the task's OWN id and state at the TOP level, and
	// keeps the nested task payload and every message field (additive).
	var created taskCreateView
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
		"principal_id": "prin_owner", "as_agent": "atlas",
		"payload": map[string]string{"text": "Sweep the data."},
		"targets": []map[string]string{{"kind": "agent", "id": fixAgentA}},
	}, &created, nil)
	require.Equal(t, http.StatusCreated, code)
	require.NotEmpty(t, created.TaskID, "the 201 carries the task's own id at the top level")
	require.Equal(t, "open", created.State, "the 201 carries the created task's state at the top level")
	require.NotEqual(t, created.ID, created.TaskID, "the message id and the task id are distinct")
	require.NotNil(t, created.Task, "the nested task payload is still present (additive change)")
	require.Equal(t, created.TaskID, created.Task["task_id"])
	require.Equal(t, "open", created.Task["state"])

	// AC2/AC3 — GET the task: 200, current state open, its own record and thread.
	var got taskReadView
	code = h.do(t, http.MethodGet, "/sessions/"+room+"/tasks/"+created.TaskID, nil, &got, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, created.TaskID, got.TaskID)
	require.Equal(t, "open", got.State)
	require.Equal(t, 1, got.Versions)
	require.Equal(t, created.ID, got.MessageID)
	require.Equal(t, created.ThreadID, got.ThreadID)
	require.Empty(t, got.Owner)

	// The session-level list shows the same task (the route that used to 405).
	var list taskListView
	code = h.do(t, http.MethodGet, "/sessions/"+room+"/tasks", nil, &list, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, list.Count)
	require.Len(t, list.Tasks, 1)
	require.Equal(t, "open", list.Tasks[0].State)

	// After claim, GET reads "claimed" and names the owner.
	code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+created.TaskID+"/claim",
		map[string]any{"actor": fixAgentA}, nil, nil)
	require.Equal(t, http.StatusCreated, code)
	code = h.do(t, http.MethodGet, "/sessions/"+room+"/tasks/"+created.TaskID, nil, &got, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "claimed", got.State)
	require.Equal(t, fixAgentA, got.Owner)
	require.Equal(t, 2, got.Versions)

	// After complete, GET reads "done" over three record-versions.
	code = h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+created.TaskID+"/complete",
		map[string]any{"actor": fixAgentA}, nil, nil)
	require.Equal(t, http.StatusCreated, code)
	code = h.do(t, http.MethodGet, "/sessions/"+room+"/tasks/"+created.TaskID, nil, &got, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "done", got.State)
	require.Equal(t, 3, got.Versions)

	// An id that is not a task in this session is the SAME named 404 the
	// transition routes give — and the MESSAGE id is not a task id.
	for _, bad := range []string{"nope", created.ID} {
		var body struct {
			Error string `json:"error"`
		}
		code = h.do(t, http.MethodGet, "/sessions/"+room+"/tasks/"+bad, nil, &body, nil)
		require.Equal(t, http.StatusNotFound, code, "GET task %q", bad)
		require.Equal(t, "TASK_NOT_FOUND", body.Error)
	}

	// A session with no tasks lists an EMPTY array, never a null.
	empty := h.createRoomAs(t)
	var emptyList taskListView
	code = h.do(t, http.MethodGet, "/sessions/"+empty+"/tasks", nil, &emptyList, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 0, emptyList.Count)
	require.NotNil(t, emptyList.Tasks, "an empty task list is [], not null")
	require.Empty(t, emptyList.Tasks)
}
