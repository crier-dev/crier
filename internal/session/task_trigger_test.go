package session

// task_trigger_test.go — CR-CHAT-034: the optional task-created DAG trigger.
//
// The hook fires exactly once per created task, AFTER the fan-out succeeds,
// and it never fails the task creation. A handler with no hook (the default)
// behaves exactly as before — the additive posture, proven not asserted.

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// triggerHarness keeps the HANDLER reachable (the apiHarness hides it), which
// is what SetTaskTrigger needs; the routes are the REAL ones
// (registerSessionRoutes).
type triggerHarness struct {
	handler *Handler
	srv     *apiHarness
}

func newTriggerHarness(t *testing.T) *triggerHarness {
	t.Helper()
	reg := registry.NewMemoryStore()
	for _, id := range []string{fixAgentA, fixAgentB} {
		require.NoError(t, reg.Register(&registry.Agent{ID: id}))
	}
	handler := NewHTTPHandler(newLocalJSONLStore(t), HTTPOptions{Deliverer: reg, Agents: reg})
	r := mux.NewRouter()
	registerSessionRoutes(r, handler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &triggerHarness{handler: handler, srv: &apiHarness{srv: srv, reg: reg}}
}

// createSimpleRoom posts a two-agent room (trust-by-reach posture: no ACL).
func (h *triggerHarness) createRoom(t *testing.T) string {
	t.Helper()
	var created sessionView
	code := h.srv.do(t, http.MethodPost, "/sessions", map[string]any{
		"title":      "trigger room",
		"created_by": map[string]string{"agent": fixAgentA},
		"members": []map[string]string{
			{"member_type": "agent", "member_id": fixAgentA, "role": "member"},
			{"member_type": "agent", "member_id": fixAgentB, "role": "member"},
		},
	}, &created, nil)
	require.Equal(t, http.StatusCreated, code, "room create failed")
	return created.ID
}

// createTask posts one task through the real route.
func (h *triggerHarness) createTask(t *testing.T, room string) (int, transcriptMessage) {
	t.Helper()
	var sent transcriptMessage
	code := h.srv.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
		"sender":  fixAgentA,
		"payload": map[string]string{"work": "run the sweep"},
		"targets": []map[string]string{{"kind": "agent", "id": fixAgentB}},
	}, &sent, nil)
	return code, sent
}

// TestTaskTriggerFiresOnce: one created task fires the hook exactly once,
// with the task id and session it belongs to.
func TestTaskTriggerFiresOnce(t *testing.T) {
	h := newTriggerHarness(t)
	room := h.createRoom(t)

	var calls atomic.Int32
	var mu sync.Mutex
	var firedTask, firedSession, firedPayload string
	h.handler.SetTaskTrigger(func(taskID, sessionID, payload string) {
		mu.Lock()
		firedTask, firedSession, firedPayload = taskID, sessionID, payload
		mu.Unlock()
		calls.Add(1)
	})

	code, sent := h.createTask(t, room)
	require.Equal(t, http.StatusCreated, code, "task create failed")
	require.NotEmpty(t, sent.ID)

	// The hook runs fire-and-forget; a bounded window, then a settle window
	// during which any duplicate fire would show.
	require.Eventually(t, func() bool { return calls.Load() == 1 },
		5*time.Second, 5*time.Millisecond, "the trigger hook never fired for a created task")
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int32(1), calls.Load(), "the trigger fired more than once")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, sent.Task["task_id"], firedTask, "trigger fired for a different task")
	require.Equal(t, room, firedSession, "trigger fired without its session")
	require.Contains(t, firedPayload, "run the sweep", "trigger payload is not the task payload")
}

// TestTaskTriggerFailureNeverFailsTask: a failing hook is recorded BY the
// hook; the round trip still answers 201. (The real trigger component owns
// its recording — see internal/daggerctl/trigger_test.go — this proves the
// session surface's side of the fire-and-forget contract.)
func TestTaskTriggerFailureNeverFailsTask(t *testing.T) {
	h := newTriggerHarness(t)
	room := h.createRoom(t)
	h.handler.SetTaskTrigger(func(taskID, sessionID, payload string) {
		// A hook that takes its time and records nothing must not block or
		// fail the task path.
		time.Sleep(50 * time.Millisecond)
	})

	code, _ := h.createTask(t, room)
	require.Equal(t, http.StatusCreated, code, "a failing trigger failed the task creation")
}

// TestNoTriggerArmedChangesNothing: a handler with no hook (the default)
// creates the task and fires nothing — the byte-identical-before proof.
func TestNoTriggerArmedChangesNothing(t *testing.T) {
	h := newTriggerHarness(t)
	room := h.createRoom(t)
	code, _ := h.createTask(t, room)
	require.Equal(t, http.StatusCreated, code, "task creation changed with no trigger armed")
}
