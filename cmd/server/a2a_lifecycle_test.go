package main

// a2a_lifecycle_test.go — INT-A2A-004: the task lifecycle, LIVE.
//
// The row's acceptance criteria are the shape of this file. Against a RUNNING
// crier (run(nil) — middleware, router and stores included) it walks one task
// from delivery to every terminal outcome crier can produce, asserting the A2A
// state at each step and naming the transition in a transcript:
//
//	undelivered (stored)  → TASK_STATE_SUBMITTED
//	retrieved (leased)    → TASK_STATE_WORKING
//	acknowledged          → the entry is REMOVED; a later read is TaskNotFoundError
//	TTL elapsed           → TASK_STATE_FAILED
//	TTL elapsed + swept   → TASK_STATE_FAILED, from the sweep's own record
//	canceled              → TASK_STATE_CANCELED, the lease released and the entry closed
//
// plus the operations' own refusals: a cancel of a terminal task is
// TaskNotCancelableError (-32002), a message aimed at a terminal task is
// UnsupportedOperationError (-32004), a task id crier holds no record of is
// TaskNotFoundError (-32001) — and NONE of them is a 200 pretending the
// operation happened.
//
// The A2A state mapping this exercises is specs/A2A-OPTION.md §5.5.2; the
// parameters, ordering and cursor are asserted in internal/a2a/task_test.go
// against the same table, purely.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

const a2aLifecycleTenant = "a2a-lifecycle-worker"

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newLifecycleHarness boots ONLY the A2A JSON-RPC binding over a store the test
// owns, which is what lets a test drive crier's own expiry sweep by hand (the
// real sweep runs on a 30-second timer inside run(), far outside a test's
// budget). The delivery route registered alongside it is the SAME
// registry.Handler.HandleDeliver the A2A binding calls, so nothing about a
// delivery is re-implemented for the test.
func newLifecycleHarness(t *testing.T) (*httptest.Server, registry.Store, *registry.Handler) {
	t.Helper()
	store := registry.NewMemoryStore()
	handler := registry.NewHandler(store)
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox", handler.HandleRetrieve).Methods(http.MethodGet)
	registerA2ARoute(r, store, handler.HandleDeliver, nil, a2aOptions{})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store, handler
}

// registerStoreAgent registers an agent straight into a store (the harness has
// no HTTP registration route), optionally opted in to A2A.
func registerStoreAgent(t *testing.T, store registry.Store, id string, optIn bool) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	agent := &registry.Agent{ID: id, PublicKey: registry.HexKey(pub), Capabilities: []string{}}
	if optIn {
		agent.A2A = &a2a.Config{Enabled: true}
	}
	if err := store.Register(agent); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

// deliverToInbox posts one delivery through crier's own deliver handler and
// returns the status and body.
func deliverToInbox(t *testing.T, base, agentID, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(base+"/agents/"+agentID+"/inbox", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// transcript collects the evidence the row owes: one line per state transition,
// in the order crier produced it.
type transcript struct {
	t     *testing.T
	lines []string
}

// step records one transition.
func (tr *transcript) step(transition, detail string) {
	tr.lines = append(tr.lines, fmt.Sprintf("%-34s %s", transition, detail))
}

// print emits the transcript, which is the evidence record for this row.
func (tr *transcript) print() {
	tr.t.Logf("lifecycle transcript (%d steps):\n%s", len(tr.lines), strings.Join(tr.lines, "\n"))
}

// sentTask returns the Task a SendMessage answered with (`result.task`), which
// is where §9.4.1 puts it.
func sentTask(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	task, ok := result["task"].(map[string]any)
	if !ok {
		t.Fatalf("SendMessage answered without a task: %v", result)
	}
	return task
}

// taskIDOf reads a Task's id, failing the test when it carries none.
func taskIDOf(t *testing.T, task map[string]any) string {
	t.Helper()
	id, _ := task["id"].(string)
	if id == "" {
		t.Fatalf("the Task carries no id: %v", task)
	}
	return id
}

// rpcTaskState reads `result.status.state`.
func rpcTaskState(t *testing.T, result map[string]any) string {
	t.Helper()
	status, ok := result["status"].(map[string]any)
	if !ok {
		t.Fatalf("result carries no status object: %v", result)
	}
	state, _ := status["state"].(string)
	if state == "" {
		t.Fatalf("result carries no status.state: %v", result)
	}
	return state
}

// rpcTaskBasis reads `result.metadata.crier.state_basis` — the crier record the
// state was resolved from, which the spec's mapping table names per row.
func rpcTaskBasis(t *testing.T, result map[string]any) string {
	t.Helper()
	meta, ok := result["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("result carries no metadata: %v", result)
	}
	crier, ok := meta["crier"].(map[string]any)
	if !ok {
		t.Fatalf("result carries no metadata.crier: %v", meta)
	}
	basis, _ := crier["state_basis"].(string)
	return basis
}

// rpcTasks reads `result.tasks`.
func rpcTasks(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	raw, ok := result["tasks"].([]any)
	if !ok {
		t.Fatalf("result carries no tasks array: %v", result)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		task, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("tasks carries a non-object: %v", item)
		}
		out = append(out, task)
	}
	return out
}

// rpcNumber reads a numeric member of a result.
func rpcNumber(t *testing.T, result map[string]any, key string) int {
	t.Helper()
	n, ok := result[key].(float64)
	if !ok {
		t.Fatalf("result.%s is not a number: %v", key, result)
	}
	return int(n)
}

// signedInboxGet issues the agent's own signed retrieve.
func signedInboxGet(t *testing.T, base, agentID string, priv ed25519.PrivateKey, path string) (int, string) {
	t.Helper()
	status, body := signedAs(t, &http.Client{Timeout: 5 * time.Second}, http.MethodGet, base, path, agentID, priv, nil)
	return status, string(body)
}

// inboxLeaseOf reads the lease of a retrieve body.
func inboxLeaseOf(t *testing.T, body string) (messageIDs []string, leaseID string, queueDepth int) {
	t.Helper()
	var out struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
		LeaseID    string `json:"lease_id"`
		QueueDepth int    `json:"queue_depth"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode the retrieve body %s: %v", body, err)
	}
	for _, m := range out.Messages {
		messageIDs = append(messageIDs, m.ID)
	}
	return messageIDs, out.LeaseID, out.QueueDepth
}

// ---------------------------------------------------------------------------
// The transcript
// ---------------------------------------------------------------------------

// TestA2ATaskLifecycle_Transcript is the row's acceptance test: one task walked
// through the whole lifecycle, every transition named, plus the terminal-state
// refusals the specification makes mandatory.
func TestA2ATaskLifecycle_Transcript(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	priv := registerA2AAgent(t, base, a2aLifecycleTenant, true, nil)
	tr := &transcript{t: t}

	// --- 1. undelivered: a send creates a SUBMITTED task ---------------------
	status, body, _ := a2aRPC(t, client, base, sendMessageBody(a2aLifecycleTenant, a2a.MethodSendMessage, "msg-life-1"))
	if status != http.StatusOK {
		t.Fatalf("SendMessage = %d: %s", status, body)
	}
	accepted := sentTask(t, rpcResult(t, body))
	taskID := taskIDOf(t, accepted)
	if got := rpcTaskState(t, accepted); got != "TASK_STATE_SUBMITTED" {
		t.Fatalf("SendMessage answered %s, want TASK_STATE_SUBMITTED", got)
	}
	tr.step("deliver → stored, unclaimed", "SendMessage/Task "+taskID+" = TASK_STATE_SUBMITTED")

	// --- 2. GetTask agrees with what the send said ---------------------------
	status, body, _ = a2aRPC(t, client, base, getTaskBody(a2aLifecycleTenant, taskID, ""))
	if status != http.StatusOK {
		t.Fatalf("GetTask = %d: %s", status, body)
	}
	got := rpcResult(t, body)
	if state := rpcTaskState(t, got); state != "TASK_STATE_SUBMITTED" {
		t.Fatalf("GetTask = %s, want TASK_STATE_SUBMITTED", state)
	}
	if basis := rpcTaskBasis(t, got); basis != a2a.StateBasisSubmitted {
		t.Errorf("state_basis = %q, want %q (the spec's own column for this row)", basis, a2a.StateBasisSubmitted)
	}
	if len(historyOf(t, got)) != 1 {
		t.Errorf("the task's history = %v, want the creating message", historyOf(t, got))
	}
	tr.step("read → GetTask", "TASK_STATE_SUBMITTED, state_basis="+a2a.StateBasisSubmitted)

	// --- 3. ListTasks sees it, newest first, and says so ---------------------
	status, body, _ = a2aRPC(t, client, base, listTasksBody(a2aLifecycleTenant, ""))
	if status != http.StatusOK {
		t.Fatalf("ListTasks = %d: %s", status, body)
	}
	listed := rpcResult(t, body)
	if got := rpcTasks(t, listed); len(got) != 1 || got[0]["id"] != taskID {
		t.Fatalf("ListTasks = %v, want exactly the task %s", got, taskID)
	}
	if total := rpcNumber(t, listed, "totalSize"); total != 1 {
		t.Errorf("totalSize = %d, want 1", total)
	}
	if next, _ := listed["nextPageToken"].(string); next != "" {
		t.Errorf("nextPageToken = %q, want an empty string on the final page (§3.1.4)", next)
	}
	tr.step("list → ListTasks", "1 task, TASK_STATE_SUBMITTED, totalSize=1, nextPageToken=''")

	// --- 4. retrieved: a lease makes it WORKING -----------------------------
	status, retrieveBody := signedInboxGet(t, base, a2aLifecycleTenant, priv, "/agents/"+a2aLifecycleTenant+"/inbox")
	if status != http.StatusOK {
		t.Fatalf("the agent's own retrieve = %d: %s", status, retrieveBody)
	}
	leased, leaseID, _ := inboxLeaseOf(t, retrieveBody)
	if len(leased) != 1 || leased[0] != taskID || leaseID == "" {
		t.Fatalf("retrieve leased %v (lease %q), want the task %s under a lease", leased, leaseID, taskID)
	}
	status, body, _ = a2aRPC(t, client, base, getTaskBody(a2aLifecycleTenant, taskID, ""))
	if status != http.StatusOK {
		t.Fatalf("GetTask (leased) = %d: %s", status, body)
	}
	got = rpcResult(t, body)
	if state := rpcTaskState(t, got); state != "TASK_STATE_WORKING" {
		t.Fatalf("GetTask after a lease = %s, want TASK_STATE_WORKING", state)
	}
	if basis := rpcTaskBasis(t, got); basis != a2a.StateBasisWorking {
		t.Errorf("state_basis = %q, want %q", basis, a2a.StateBasisWorking)
	}
	tr.step("retrieve (lease) → leased", "GetTask = TASK_STATE_WORKING, state_basis="+a2a.StateBasisWorking)

	// --- 5. acknowledged: the entry is REMOVED ------------------------------
	ackBody := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, leaseID, taskID)
	if status, ackOut := signedAs(t, client, http.MethodPost, base,
		"/agents/"+a2aLifecycleTenant+"/inbox/ack", a2aLifecycleTenant, priv, []byte(ackBody)); status != http.StatusNoContent {
		t.Fatalf("ack = %d: %s", status, ackOut)
	}
	status, body, _ = a2aRPC(t, client, base, getTaskBody(a2aLifecycleTenant, taskID, ""))
	if status != http.StatusOK {
		t.Fatalf("the JSON-RPC answer to GetTask after an ack must still be HTTP 200 (the error travels in the JSON-RPC object): %d %s", status, body)
	}
	code, message := rpcError(t, body)
	if code != a2a.CodeTaskNotFoundError {
		t.Fatalf("GetTask after an ack = %d (%s), want %d TaskNotFoundError: crier's ack REMOVES the entry and keeps no tombstone, so a completed task is not re-readable — the answer must not be an invented TASK_STATE_COMPLETED",
			code, message, a2a.CodeTaskNotFoundError)
	}
	if !strings.Contains(message, "no tombstone") {
		t.Errorf("the TaskNotFoundError must state why (no tombstone / completed and purged): %q", message)
	}
	tr.step("ack → entry removed", "GetTask = -32001 TaskNotFoundError: the entry is removed and no tombstone is kept, so a completed task is not re-readable (the spec's \"already completed or purged\"); no invented TASK_STATE_COMPLETED")

	status, body, _ = a2aRPC(t, client, base, listTasksBody(a2aLifecycleTenant, ""))
	if status != http.StatusOK {
		t.Fatalf("ListTasks after an ack = %d: %s", status, body)
	}
	if n := len(rpcTasks(t, rpcResult(t, body))); n != 0 {
		t.Errorf("ListTasks after an ack = %d tasks, want none: the entry is gone", n)
	}
	tr.step("ack → ListTasks", "0 tasks (the queue really is empty)")

	// --- 6. TTL elapsed: FAILED, from the entry itself -----------------------
	expiredID := taskIDOf(t, sentTask(t, rpcResult(t, mustA2AOK(t, client, base,
		sendMessageBodyWithTTL(a2aLifecycleTenant, "msg-life-2", 1)))))
	time.Sleep(1200 * time.Millisecond)
	status, body, _ = a2aRPC(t, client, base, getTaskBody(a2aLifecycleTenant, expiredID, ""))
	if status != http.StatusOK {
		t.Fatalf("GetTask (expired) = %d: %s", status, body)
	}
	got = rpcResult(t, body)
	if state := rpcTaskState(t, got); state != "TASK_STATE_FAILED" {
		t.Fatalf("GetTask after the TTL elapsed = %s, want TASK_STATE_FAILED", state)
	}
	if basis := rpcTaskBasis(t, got); basis != a2a.StateBasisExpired {
		t.Errorf("state_basis = %q, want %q", basis, a2a.StateBasisExpired)
	}
	tr.step("TTL elapsed (unacked) → FAILED", "GetTask = TASK_STATE_FAILED, state_basis="+a2a.StateBasisExpired)

	// --- 7. a message aimed at a TERMINAL task is REFUSED --------------------
	before := inboxDepth(t, base, a2aLifecycleTenant, priv)
	status, body, _ = a2aRPC(t, client, base, sendMessageWithTaskBody(a2aLifecycleTenant, "msg-life-2b", expiredID))
	if status != http.StatusOK {
		t.Fatalf("SendMessage to a terminal task = %d: %s", status, body)
	}
	code, message = rpcError(t, body)
	if code != a2a.CodeUnsupportedOperationError {
		t.Fatalf("a message to a TERMINAL task = %d (%s), want %d UnsupportedOperationError (§3.1.1)", code, message, a2a.CodeUnsupportedOperationError)
	}
	if !strings.Contains(message, "TASK_STATE_FAILED") {
		t.Errorf("the refusal must name the terminal state: %q", message)
	}
	if after := inboxDepth(t, base, a2aLifecycleTenant, priv); after != before {
		t.Errorf("a refused message reached the inbox: queue_depth %d → %d", before, after)
	}
	tr.step("message → terminal task", fmt.Sprintf("REJECTED -32004 UnsupportedOperationError naming TASK_STATE_FAILED; queue_depth unchanged (%d)", before))

	// --- 8. canceling a terminal task is REFUSED ---------------------------
	status, body, _ = a2aRPC(t, client, base, cancelTaskBody(a2aLifecycleTenant, expiredID))
	if status != http.StatusOK {
		t.Fatalf("CancelTask = %d: %s", status, body)
	}
	code, message = rpcError(t, body)
	if code != a2a.CodeTaskNotCancelableError {
		t.Fatalf("CancelTask on a terminal task = %d (%s), want %d TaskNotCancelableError (§3.1.5)", code, message, a2a.CodeTaskNotCancelableError)
	}
	tr.step("cancel → terminal task", "REJECTED -32002 TaskNotCancelableError (already FAILED)")

	// --- 9. canceling a LEASED task releases the lease and closes it --------
	canceledID := taskIDOf(t, sentTask(t, rpcResult(t, mustA2AOK(t, client, base,
		sendMessageBody(a2aLifecycleTenant, a2a.MethodSendMessage, "msg-life-3")))))
	_, cancelLease, _ := inboxLeaseOf(t, mustSignedGet(t, base, a2aLifecycleTenant, priv,
		"/agents/"+a2aLifecycleTenant+"/inbox"))
	if cancelLease == "" {
		t.Fatal("the task was not leased before the cancel, so the release is not being tested")
	}
	status, body, _ = a2aRPC(t, client, base, cancelTaskBody(a2aLifecycleTenant, canceledID))
	if status != http.StatusOK {
		t.Fatalf("CancelTask = %d: %s", status, body)
	}
	canceledTask := rpcResult(t, body)
	if state := rpcTaskState(t, canceledTask); state != "TASK_STATE_CANCELED" {
		t.Fatalf("CancelTask answered %s, want TASK_STATE_CANCELED (§3.1.5: the updated Task)", state)
	}
	if basis := rpcTaskBasis(t, canceledTask); basis != a2a.StateBasisCanceled {
		t.Errorf("state_basis = %q, want %q", basis, a2a.StateBasisCanceled)
	}
	tr.step("cancel (leased task) → closed", "CancelTask = TASK_STATE_CANCELED, state_basis="+a2a.StateBasisCanceled)

	// The entry is closed: the agent cannot retrieve or acknowledge it any more,
	// which is what "releases the lease and closes the entry" means.
	msgs, lease, depth := inboxLeaseOf(t, mustSignedGet(t, base, a2aLifecycleTenant, priv,
		"/agents/"+a2aLifecycleTenant+"/inbox"))
	if len(msgs) != 0 || lease != "" || depth != 0 {
		t.Errorf("after a cancel the inbox still offers %v (lease %q, depth %d), want nothing claimable", msgs, lease, depth)
	}
	staleAck := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, cancelLease, canceledID)
	if status, out := signedAs(t, client, http.MethodPost, base,
		"/agents/"+a2aLifecycleTenant+"/inbox/ack", a2aLifecycleTenant, priv, []byte(staleAck)); status != http.StatusNotFound {
		t.Errorf("acking a canceled task with the old lease = %d (%s), want 404: the entry is closed", status, out)
	}
	tr.step("cancel → retrieve/ack", "nothing claimable; the stale lease cannot ack (404)")

	// A later read is the not-found §3.1.5 sanctions for a canceled task.
	status, body, _ = a2aRPC(t, client, base, getTaskBody(a2aLifecycleTenant, canceledID, ""))
	if status != http.StatusOK {
		t.Fatalf("GetTask after a cancel = %d: %s", status, body)
	}
	if code, message = rpcError(t, body); code != a2a.CodeTaskNotFoundError {
		t.Errorf("GetTask after a cancel = %d (%s), want %d: a canceled task is purged", code, message, a2a.CodeTaskNotFoundError)
	}
	// A duplicate cancel is TaskNotFoundError, which §3.1.5 explicitly allows
	// ("MAY return TaskNotFoundError if the task has already been canceled and
	// purged") — and is never a second, false cancellation.
	status, body, _ = a2aRPC(t, client, base, cancelTaskBody(a2aLifecycleTenant, canceledID))
	if status != http.StatusOK {
		t.Fatalf("the duplicate CancelTask = %d: %s", status, body)
	}
	if code, message = rpcError(t, body); code != a2a.CodeTaskNotFoundError {
		t.Errorf("a duplicate CancelTask = %d (%s), want %d for a canceled-and-purged task", code, message, a2a.CodeTaskNotFoundError)
	}
	tr.step("cancel → read again", "GetTask/CancelTask = -32001 TaskNotFoundError (canceled and purged, §3.1.5)")

	// --- 10. an OPEN task cannot be continued ------------------------------
	liveID := taskIDOf(t, sentTask(t, rpcResult(t, mustA2AOK(t, client, base,
		sendMessageBody(a2aLifecycleTenant, a2a.MethodSendMessage, "msg-life-4")))))
	before = inboxDepth(t, base, a2aLifecycleTenant, priv)
	status, body, _ = a2aRPC(t, client, base, sendMessageWithTaskBody(a2aLifecycleTenant, "msg-life-4b", liveID))
	if status != http.StatusOK {
		t.Fatalf("SendMessage to an open task = %d: %s", status, body)
	}
	if code, message = rpcError(t, body); code != a2a.CodeUnsupportedOperationError {
		t.Fatalf("a message continuing an OPEN task = %d (%s), want %d", code, message, a2a.CodeUnsupportedOperationError)
	}
	if !strings.Contains(message, "continuation") {
		t.Errorf("the refusal must say crier cannot represent a continuation: %q", message)
	}
	if after := inboxDepth(t, base, a2aLifecycleTenant, priv); after != before {
		t.Errorf("a refused continuation reached the inbox: queue_depth %d → %d", before, after)
	}
	tr.step("message → open task", fmt.Sprintf("REJECTED -32004 (continuation not representable); queue_depth unchanged (%d)", before))

	// --- 11. an unknown task id is the specification's not-found -----------
	status, body, _ = a2aRPC(t, client, base, sendMessageWithTaskBody(a2aLifecycleTenant, "msg-life-5", "task-that-never-existed"))
	if status != http.StatusOK {
		t.Fatalf("SendMessage with an unknown taskId = %d: %s", status, body)
	}
	if code, message = rpcError(t, body); code != a2a.CodeTaskNotFoundError {
		t.Errorf("an unknown taskId = %d (%s), want %d (§3.4.2)", code, message, a2a.CodeTaskNotFoundError)
	}
	if status, body, _ = a2aRPC(t, client, base, getTaskBody(a2aLifecycleTenant, "task-that-never-existed", "")); status != http.StatusOK {
		t.Fatalf("GetTask with an unknown id = %d: %s", status, body)
	}
	if code, message = rpcError(t, body); code != a2a.CodeTaskNotFoundError {
		t.Errorf("GetTask with an unknown id = %d (%s), want %d", code, message, a2a.CodeTaskNotFoundError)
	}
	tr.step("unknown task id → any op", "-32001 TaskNotFoundError (never invented a state)")

	// --- 12. SubscribeToTask: a terminal task has no updates to stream ------
	status, body, header := a2aRPC(t, client, base, subscribeBody(a2aLifecycleTenant, expiredID))
	if status != http.StatusOK {
		t.Fatalf("SubscribeToTask on a terminal task = %d: %s", status, body)
	}
	if ct := header.Get("Content-Type"); ct != a2aMediaType {
		t.Errorf("a refused subscription must answer application/a2a+json, got %q: an empty stream would be a lie about its own body", ct)
	}
	if code, message = rpcError(t, body); code != a2a.CodeUnsupportedOperationError {
		t.Errorf("SubscribeToTask on a terminal task = %d (%s), want %d (§9.4.6)", code, message, a2a.CodeUnsupportedOperationError)
	}
	tr.step("subscribe → terminal task", "REJECTED -32004 UnsupportedOperationError (as a JSON-RPC answer, not an empty stream)")

	tr.print()
}

// TestA2ATaskLifecycle_CancelIsCanceledEvenWhenNothingHoldsTheLease: a cancel
// reaches a QUEUED message too — the case an ack-based cancel could not express,
// because Ack requires the caller to hold the lease.
func TestA2ATaskLifecycle_CancelIsCanceledEvenWhenNothingHoldsTheLease(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	priv := registerA2AAgent(t, base, a2aLifecycleTenant, true, nil)

	taskID := taskIDOf(t, sentTask(t, rpcResult(t, mustA2AOK(t, client, base,
		sendMessageBody(a2aLifecycleTenant, a2a.MethodSendMessage, "msg-queued-1")))))
	// Nothing is leased: the message is queued, which is what queue_depth counts.
	if depth := inboxDepth(t, base, a2aLifecycleTenant, priv); depth != 1 {
		t.Fatalf("stats queue_depth = %d before the cancel, want 1 (the message is queued and unleased)", depth)
	}

	canceled := rpcResult(t, mustA2AOK(t, client, base, cancelTaskBody(a2aLifecycleTenant, taskID)))
	if state := rpcTaskState(t, canceled); state != "TASK_STATE_CANCELED" {
		t.Fatalf("CancelTask on a queued (unleased) task answered %s, want TASK_STATE_CANCELED", state)
	}
	// The stats POSTURE after the cancel: the queue is empty again (the entry
	// was closed), so the message is not merely leased-and-hidden.
	if depth := inboxDepth(t, base, a2aLifecycleTenant, priv); depth != 0 {
		t.Errorf("after canceling a queued task, stats queue_depth = %d, want 0: the entry was closed", depth)
	}
	if _, _, depth := inboxLeaseOf(t, mustSignedGet(t, base, a2aLifecycleTenant, priv,
		"/agents/"+a2aLifecycleTenant+"/inbox")); depth != 0 {
		t.Errorf("the canceled message is still offered by a retrieve (depth %d)", depth)
	}
}

// TestA2ATaskLifecycle_ListTasksPaginationLive drives the cursor over a real
// store: two pages, no repeats, and the totals §3.1.4 requires.
func TestA2ATaskLifecycle_ListTasksPaginationLive(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	registerA2AAgent(t, base, a2aLifecycleTenant, true, nil)

	var sent []string
	for i := 0; i < 3; i++ {
		id := taskIDOf(t, sentTask(t, rpcResult(t, mustA2AOK(t, client, base,
			sendMessageBody(a2aLifecycleTenant, a2a.MethodSendMessage, fmt.Sprintf("msg-page-%d", i))))))
		sent = append(sent, id)
		// The listing orders by status timestamp, and two deliveries can share
		// an instant: stagger them so the expected order is deterministic.
		time.Sleep(15 * time.Millisecond)
	}

	first := rpcResult(t, mustA2AOK(t, client, base, listTasksBody(a2aLifecycleTenant, `"pageSize":2`)))
	page1 := rpcTasks(t, first)
	if len(page1) != 2 {
		t.Fatalf("page 1 = %d tasks, want 2", len(page1))
	}
	if total := rpcNumber(t, first, "totalSize"); total != 3 {
		t.Errorf("page 1 totalSize = %d, want 3 (every match, before pagination)", total)
	}
	// Newest first: the last two deliveries, in reverse order.
	if page1[0]["id"] != sent[2] || page1[1]["id"] != sent[1] {
		t.Errorf("page 1 = %v, want the two newest first (%v)", []any{page1[0]["id"], page1[1]["id"]}, []string{sent[2], sent[1]})
	}
	token, _ := first["nextPageToken"].(string)
	if token == "" {
		t.Fatal("page 1 must carry a nextPageToken")
	}

	second := rpcResult(t, mustA2AOK(t, client, base,
		listTasksBody(a2aLifecycleTenant, `"pageSize":2,"pageToken":`+fmt.Sprintf("%q", token))))
	page2 := rpcTasks(t, second)
	if len(page2) != 1 || page2[0]["id"] != sent[0] {
		t.Fatalf("page 2 = %v, want only the oldest task %s", page2, sent[0])
	}
	if next, _ := second["nextPageToken"].(string); next != "" {
		t.Errorf("the final page = nextPageToken %q, want an empty string", next)
	}
	if total := rpcNumber(t, second, "totalSize"); total != 3 {
		t.Errorf("page 2 totalSize = %d, want 3", total)
	}
}

// TestA2ATaskLifecycle_SweptTaskIsFailedFromItsDeadLetter walks the one terminal
// arm the live transcript cannot wait for: the expiry sweep's own record.
//
// The sweep runs on a 30-second timer inside run(), so this test boots the
// binding over a store it owns and drives the REAL sweep — registry.Handler's
// PurgeExpired, the function the timer calls, which dead-letters what it removes
// (CR-FEAT-025) — by hand.
func TestA2ATaskLifecycle_SweptTaskIsFailedFromItsDeadLetter(t *testing.T) {
	srv, store, handler := newLifecycleHarness(t)
	registerStoreAgent(t, store, a2aLifecycleTenant, true)
	client := &http.Client{Timeout: 5 * time.Second}

	status, body := deliverToInbox(t, srv.URL, a2aLifecycleTenant,
		`{"payload":{"parts":[{"type":"text","text":"expire me"}]},"sender":"a2a-sender","ttl_seconds":1}`)
	if status != http.StatusCreated {
		t.Fatalf("deliver = %d: %s", status, body)
	}
	var accepted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &accepted); err != nil || accepted.ID == "" {
		t.Fatalf("the accept carries no id: %s (%v)", body, err)
	}

	time.Sleep(1100 * time.Millisecond)
	// Before the sweep: the entry is still stored, and past its TTL.
	got := rpcResult(t, mustA2AOK(t, client, srv.URL, getTaskBody(a2aLifecycleTenant, accepted.ID, "")))
	if state := rpcTaskState(t, got); state != "TASK_STATE_FAILED" {
		t.Fatalf("an expired-but-unswept task = %s, want TASK_STATE_FAILED", state)
	}
	if basis := rpcTaskBasis(t, got); basis != a2a.StateBasisExpired {
		t.Errorf("state_basis = %q, want %q", basis, a2a.StateBasisExpired)
	}

	// The real sweep: it removes the entry and records it.
	if removed := handler.PurgeExpired(); removed != 1 {
		t.Fatalf("the sweep removed %d messages, want 1", removed)
	}
	if entry, err := store.(registry.InboxPeeker).Peek(a2aLifecycleTenant, accepted.ID); err == nil {
		t.Fatalf("the swept message is still in the inbox: %+v", entry)
	}
	got = rpcResult(t, mustA2AOK(t, client, srv.URL, getTaskBody(a2aLifecycleTenant, accepted.ID, "")))
	if state := rpcTaskState(t, got); state != "TASK_STATE_FAILED" {
		t.Fatalf("a swept task = %s, want TASK_STATE_FAILED (from the sweep's own record)", state)
	}
	if basis := rpcTaskBasis(t, got); basis != a2a.StateBasisDeadLetter {
		t.Errorf("state_basis = %q, want %q", basis, a2a.StateBasisDeadLetter)
	}
	meta, _ := got["metadata"].(map[string]any)
	crier, _ := meta["crier"].(map[string]any)
	if recorded, _ := crier["dead_lettered_at"].(string); recorded == "" {
		t.Errorf("a state read from a dead letter must say when it was recorded: %v", crier)
	}

	// A task crier has NO record of is still the not-found — the dead-letter arm
	// must not turn every unknown id into a FAILED task.
	httpStatus, unknownBody, _ := a2aRPC(t, client, srv.URL, getTaskBody(a2aLifecycleTenant, "task-that-never-existed", ""))
	if httpStatus != http.StatusOK {
		t.Fatalf("GetTask with an unknown id = %d: %s", httpStatus, unknownBody)
	}
	if code, message := rpcError(t, unknownBody); code != a2a.CodeTaskNotFoundError {
		t.Errorf("an unknown id = %d (%s), want %d", code, message, a2a.CodeTaskNotFoundError)
	}
}

// TestA2ATaskLifecycle_SubscribeToTaskStreamsUntilTerminal: subscribing to a
// live task opens with the Task (§9.4.6) and closes when crier's own store shows
// a terminal outcome — here, the consumer's ack.
func TestA2ATaskLifecycle_SubscribeToTaskStreamsUntilTerminal(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	priv := registerA2AAgent(t, base, a2aLifecycleTenant, true, nil)

	taskID := taskIDOf(t, sentTask(t, rpcResult(t, mustA2AOK(t, client, base,
		sendMessageBody(a2aLifecycleTenant, a2a.MethodSendMessage, "msg-sub-1")))))
	msgs, leaseID, _ := inboxLeaseOf(t, mustSignedGet(t, base, a2aLifecycleTenant, priv,
		"/agents/"+a2aLifecycleTenant+"/inbox"))
	if len(msgs) != 1 || msgs[0] != taskID || leaseID == "" {
		t.Fatalf("the retrieve did not lease the task: %v (lease %q)", msgs, leaseID)
	}

	stream := openSSE(t, base, subscribeBody(a2aLifecycleTenant, taskID))
	if stream.status != http.StatusOK {
		t.Fatalf("SubscribeToTask = %d, want 200", stream.status)
	}
	if stream.ctype != a2a.StreamMediaType {
		t.Fatalf("Content-Type = %q, want %q", stream.ctype, a2a.StreamMediaType)
	}
	first := stream.next(t, "the task")
	opened, ok := first["task"].(map[string]any)
	if !ok {
		t.Fatalf("the first event must be the Task (§9.4.6), got %v", first)
	}
	if state := rpcTaskState(t, opened); state != "TASK_STATE_WORKING" {
		t.Errorf("the opening Task = %s, want the state the store shows (TASK_STATE_WORKING)", state)
	}

	// The agent acknowledges while the subscription is open.
	ackBody := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, leaseID, taskID)
	if status, out := signedAs(t, client, http.MethodPost, base,
		"/agents/"+a2aLifecycleTenant+"/inbox/ack", a2aLifecycleTenant, priv, []byte(ackBody)); status != http.StatusNoContent {
		t.Fatalf("ack = %d: %s", status, out)
	}

	terminal := stream.next(t, "the terminal state update")
	update, ok := terminal["statusUpdate"].(map[string]any)
	if !ok {
		t.Fatalf("expected a statusUpdate, got %v", terminal)
	}
	if state := rpcTaskState(t, update); state != "TASK_STATE_COMPLETED" {
		t.Errorf("the terminal update = %s, want TASK_STATE_COMPLETED for an acknowledged message", state)
	}
	// §3.1.2/§9.4.6: the stream MUST close when the task reaches a terminal state.
	stream.waitClosed(t)
}

// lifecycleParams builds a valid params object for one lifecycle method: the
// tenant always (crier hosts many agents on one origin), and the task id for the
// methods that take one.
func lifecycleParams(method, tenant, id string) string {
	if method == a2a.MethodListTasks {
		return fmt.Sprintf(`{"tenant":%q}`, tenant)
	}
	return fmt.Sprintf(`{"tenant":%q,"id":%q}`, tenant, id)
}

// TestA2ATaskLifecycle_NoNewAuthAndTheSameGate: the lifecycle sits behind the
// SAME middleware chain as every other A2A request — it needs no agent signature
// of its own (the switch-on posture here has signature enforcement ON, the
// default, and these calls carry no X-Agent-* header) — and the per-agent gate is
// the same one the send operations enforce.
func TestA2ATaskLifecycle_NoNewAuthAndTheSameGate(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_A2A_ENABLED":       "true",
		"CR_REQUIRE_AGENT_SIG": "true",
	})
	client := &http.Client{Timeout: 5 * time.Second}
	registerA2AAgent(t, base, a2aLifecycleTenant, true, nil)
	registerA2AAgent(t, base, "a2a-not-opted-in", false, nil)

	// An opted-in agent: every lifecycle method answers without an agent
	// signature (the route is behind the same chain it always was).
	for _, method := range []string{a2a.MethodGetTask, a2a.MethodListTasks, a2a.MethodCancelTask} {
		status, body, _ := a2aRPC(t, client, base,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, lifecycleParams(method, a2aLifecycleTenant, "m-1")))
		if status != http.StatusOK {
			t.Errorf("%s = %d (no signature header was sent, and none is required of an A2A request): %s", method, status, body)
		}
	}

	// An agent that did not opt in gets the same answer a send gets: it is not
	// an A2A agent on this relay at all.
	for _, method := range []string{a2a.MethodGetTask, a2a.MethodListTasks, a2a.MethodCancelTask, a2a.MethodSubscribeToTask} {
		status, body, _ := a2aRPC(t, client, base,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, lifecycleParams(method, "a2a-not-opted-in", "m-1")))
		if status != http.StatusOK {
			t.Fatalf("%s = %d: %s", method, status, body)
		}
		code, message := rpcError(t, body)
		if code != a2a.CodeInvalidParams {
			t.Errorf("%s against a non-opted-in agent = %d (%s), want %d (the per-agent gate)", method, code, message, a2a.CodeInvalidParams)
		}
		if !strings.Contains(message, "not an A2A agent on this relay") {
			t.Errorf("%s refusal = %q, want the gate's own answer", method, message)
		}
	}
}

// TestA2ATaskLifecycle_SwitchOffRegistersNothing is the separability half for
// this row's surface: with CR_A2A_ENABLED unset every lifecycle method answers
// the router's own 404 — the path does not exist, exactly as it did not before
// this row.
func TestA2ATaskLifecycle_SwitchOffRegistersNothing(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": ""})
	client := &http.Client{Timeout: 5 * time.Second}
	for _, method := range []string{
		a2a.MethodSendMessage, a2a.MethodSendStreamingMessage,
		a2a.MethodGetTask, a2a.MethodListTasks, a2a.MethodCancelTask, a2a.MethodSubscribeToTask,
	} {
		status, body, _ := a2aRPC(t, client, base,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{"tenant":"a","id":"m"}}`, method))
		if status != http.StatusNotFound {
			t.Errorf("POST %s (method %s) = %d with the option off, want 404: %s", a2a.JSONRPCBindingPath, method, status, body)
		}
	}
}

// ---------------------------------------------------------------------------
// Request bodies
// ---------------------------------------------------------------------------

// getTaskBody is a GetTask request.
func getTaskBody(tenant, id, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"req-get","method":"GetTask","params":{"tenant":%q,"id":%q%s}}`,
		tenant, id, extra)
}

// listTasksBody is a ListTasks request; filters, when given, are pasted in as
// JSON members.
func listTasksBody(tenant, filters string) string {
	if filters != "" {
		filters = "," + filters
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"req-list","method":"ListTasks","params":{"tenant":%q%s}}`, tenant, filters)
}

// cancelTaskBody is a CancelTask request.
func cancelTaskBody(tenant, id string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"req-cancel","method":"CancelTask","params":{"tenant":%q,"id":%q}}`, tenant, id)
}

// subscribeBody is a SubscribeToTask request.
func subscribeBody(tenant, id string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"req-sub","method":"SubscribeToTask","params":{"tenant":%q,"id":%q}}`, tenant, id)
}

// sendMessageWithTaskBody is a SendMessage that NAMES an existing task.
func sendMessageWithTaskBody(tenant, messageID, taskID string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"req-cont","method":"SendMessage","params":{"tenant":%q,"message":{"messageId":%q,"taskId":%q,"role":"ROLE_USER","parts":[{"text":"carry on"}]}}}`,
		tenant, messageID, taskID)
}

// sendMessageBodyWithTTL is a SendMessage whose delivery expires in ttl seconds.
func sendMessageBodyWithTTL(tenant, messageID string, ttl int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"req-ttl","method":"SendMessage","params":{"tenant":%q,"message":{"messageId":%q,"contextId":"ctx-life","role":"ROLE_USER","parts":[{"text":"expire me"}]},"metadata":{"ttlSeconds":%d}}}`,
		tenant, messageID, ttl)
}

// ---------------------------------------------------------------------------
// Small assertions
// ---------------------------------------------------------------------------

// mustA2AOK performs a JSON-RPC call and returns the raw body, failing the test
// on an HTTP or JSON-RPC error.
func mustA2AOK(t *testing.T, client *http.Client, base, body string) []byte {
	t.Helper()
	status, raw, _ := a2aRPC(t, client, base, body)
	if status != http.StatusOK {
		t.Fatalf("JSON-RPC call = %d: %s", status, raw)
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("decode the response %s: %v", raw, err)
	}
	if len(probe.Error) > 0 {
		t.Fatalf("the call was refused: %s", probe.Error)
	}
	return raw
}

// mustSignedGet performs a signed agent read and returns the body.
func mustSignedGet(t *testing.T, base, agentID string, priv ed25519.PrivateKey, path string) string {
	t.Helper()
	status, body := signedInboxGet(t, base, agentID, priv, path)
	if status != http.StatusOK {
		t.Fatalf("signed GET %s = %d: %s", path, status, body)
	}
	return body
}

// inboxDepth reads the agent's own queue depth through crier's pre-existing
// stats route, which is how "a refused message really did not land" is proved
// without touching a state the A2A surface owns.
func inboxDepth(t *testing.T, base, agentID string, priv ed25519.PrivateKey) int {
	t.Helper()
	body := mustSignedGet(t, base, agentID, priv, "/agents/"+agentID+"/inbox/stats")
	var stats struct {
		QueueDepth int `json:"queue_depth"`
	}
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatalf("decode the stats body %s: %v", body, err)
	}
	return stats.QueueDepth
}

// historyOf reads result.history.
func historyOf(t *testing.T, result map[string]any) []any {
	t.Helper()
	history, _ := result["history"].([]any)
	return history
}
