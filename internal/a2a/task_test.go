package a2a

// task_test.go — INT-A2A-004: the task lifecycle MAPPING, as a gate.
//
// The row's central risk is stated in its own evidence record: "State mapping
// must be documented in the spec and asserted in tests, because this is where an
// interop adapter most easily lies about state." This file is that assertion. It
// pins, literally and one row at a time, the table in specs/A2A-OPTION.md §5.5.2
// — which crier record produces which A2A state — plus the parameters of the
// three operations, the ordering and cursor §3.1.4 requires, and the error
// codes §5.4/§5.5 assign to each refusal.
//
// Everything here is a function of its inputs: no server, no store, no clock.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// lifecycleNow is the fixed clock every case below is decided against, so a
// state cannot pass by accident of timing.
var lifecycleNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// entryView builds the snapshot of one inbox row.
func entryView(id string, created time.Time, leaseID string, leasedAt *time.Time, expires time.Time, payload string) *InboxView {
	return &InboxView{
		ID:        id,
		Payload:   json.RawMessage(payload),
		CreatedAt: created,
		LeasedAt:  leasedAt,
		LeaseID:   leaseID,
		ExpiresAt: expires,
	}
}

// TestTaskLifecycleStateMappingIsTheDocumentedTable asserts §5.5.2 row by row:
// the crier record on the left produces the A2A state on the right, and the
// state reports which record it came from.
func TestTaskLifecycleStateMappingIsTheDocumentedTable(t *testing.T) {
	leased := lifecycleNow.Add(-30 * time.Second)
	cases := []struct {
		name       string
		evidence   TaskEvidence
		wantState  TaskState
		wantBasis  string
		wantAbsent bool // no state at all: TaskNotFoundError
	}{
		{
			name: "an undelivered message (stored, unclaimed) is SUBMITTED",
			evidence: TaskEvidence{Entry: entryView("m-1", lifecycleNow.Add(-time.Minute), "",
				nil, lifecycleNow.Add(time.Hour), `{"parts":[]}`)},
			wantState: TaskStateSubmitted,
			wantBasis: StateBasisSubmitted,
		},
		{
			name: "a leased message (a consumer retrieved it) is WORKING",
			evidence: TaskEvidence{Entry: entryView("m-2", lifecycleNow.Add(-time.Minute), "lease-1",
				&leased, lifecycleNow.Add(time.Hour), `{"parts":[]}`)},
			wantState: TaskStateWorking,
			wantBasis: StateBasisWorking,
		},
		{
			name: "a message past its TTL, not yet swept, is FAILED",
			evidence: TaskEvidence{Entry: entryView("m-3", lifecycleNow.Add(-time.Hour), "",
				nil, lifecycleNow.Add(-time.Second), `{"parts":[]}`)},
			wantState: TaskStateFailed,
			wantBasis: StateBasisExpired,
		},
		{
			name: "a CLOSED row a backend retained is COMPLETED",
			evidence: TaskEvidence{Entry: func() *InboxView {
				v := entryView("m-4", lifecycleNow.Add(-time.Minute), "", nil, lifecycleNow.Add(time.Hour), `{"parts":[]}`)
				v.ACKed = true
				return v
			}()},
			wantState: TaskStateCompleted,
			wantBasis: StateBasisClosed,
		},
		{
			name:      "a swept message with a recorded expiry is FAILED",
			evidence:  TaskEvidence{DeadLetteredAt: lifecycleNow.Add(-time.Minute)},
			wantState: TaskStateFailed,
			wantBasis: StateBasisDeadLetter,
		},
		{
			// The row's honesty case: an acknowledged message is REMOVED
			// (DF-CRIER-32) and crier keeps no tombstone, so "gone" cannot be
			// told from "never existed". COMPLETED here would be a claim crier
			// cannot support.
			name:       "no record at all has no state — not an invented COMPLETED",
			evidence:   TaskEvidence{},
			wantAbsent: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, basis, rpcErr := tc.evidence.Resolve("agent-a", "m-1", lifecycleNow)
			if tc.wantAbsent {
				if rpcErr == nil {
					t.Fatalf("state = %q (basis %q), want a refusal: crier holds no record of this task", state, basis)
				}
				if rpcErr.Code != CodeTaskNotFoundError {
					t.Errorf("code = %d, want %d (TaskNotFoundError)", rpcErr.Code, CodeTaskNotFoundError)
				}
				return
			}
			if rpcErr != nil {
				t.Fatalf("Resolve refused a task crier has a record of: %v", rpcErr)
			}
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if basis != tc.wantBasis {
				t.Errorf("basis = %q, want %q (the mapping table's own column)", basis, tc.wantBasis)
			}
		})
	}
}

// TestTaskLifecycleAnUnexpiredNeverExpiringEntryIsNotFailed pins the zero-TTL
// case: ttl_seconds=0 means NEVER expires (DF-CRIER-37), so a row whose
// ExpiresAt is the zero time is SUBMITTED/WORKING and never FAILED — a
// comparison against the zero time would report every such task as expired.
func TestTaskLifecycleAnUnexpiredNeverExpiringEntryIsNotFailed(t *testing.T) {
	ev := TaskEvidence{Entry: entryView("m-never", lifecycleNow.Add(-100*time.Hour), "",
		nil, time.Time{}, `{"parts":[]}`)}
	state, basis, rpcErr := ev.Resolve("agent-a", "m-never", lifecycleNow)
	if rpcErr != nil {
		t.Fatalf("Resolve: %v", rpcErr)
	}
	if state != TaskStateSubmitted {
		t.Errorf("state = %q, want %q: a never-expiring entry does not expire", state, TaskStateSubmitted)
	}
	if basis != StateBasisSubmitted {
		t.Errorf("basis = %q, want %q", basis, StateBasisSubmitted)
	}
}

// TestTaskLifecycleStatusTimestamp follows the state, so a listing's ordering is
// crier's own timestamps and never an invented one.
func TestTaskLifecycleStatusTimestamp(t *testing.T) {
	leased := lifecycleNow.Add(-30 * time.Second)
	expiredAt := lifecycleNow.Add(-time.Second)
	created := lifecycleNow.Add(-time.Hour)

	cases := []struct {
		name     string
		evidence TaskEvidence
		want     time.Time
	}{
		{
			name:     "unclaimed: the delivery instant",
			evidence: TaskEvidence{Entry: entryView("m", created, "", nil, lifecycleNow.Add(time.Hour), `{}`)},
			want:     created,
		},
		{
			name:     "leased: when the consumer claimed it",
			evidence: TaskEvidence{Entry: entryView("m", created, "lease-1", &leased, lifecycleNow.Add(time.Hour), `{}`)},
			want:     leased,
		},
		{
			name:     "expired: the instant that elapsed",
			evidence: TaskEvidence{Entry: entryView("m", created, "", nil, expiredAt, `{}`)},
			want:     expiredAt,
		},
		{
			name:     "swept: when the sweep recorded it",
			evidence: TaskEvidence{DeadLetteredAt: lifecycleNow.Add(-time.Minute)},
			want:     lifecycleNow.Add(-time.Minute),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.evidence.StatusTimestamp(lifecycleNow); !got.Equal(tc.want) {
				t.Errorf("StatusTimestamp = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestTaskLifecycleTaskProjection pins what a read task carries: crier's own id
// and context, the state and its timestamp, the creating message as history
// (bounded by §3.2.4), and the crier facts under metadata.crier — including the
// state basis, so a client is never handed a state without its evidence.
func TestTaskLifecycleTaskProjection(t *testing.T) {
	msg := &Message{
		MessageID: "msg-client-1",
		ContextID: "ctx-9",
		Role:      RoleUser,
		Parts:     []Part{TextPart("deploy the canary")},
	}
	env, err := EnvelopeFromMessage(msg)
	if err != nil {
		t.Fatalf("EnvelopeFromMessage: %v", err)
	}
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal the envelope: %v", err)
	}
	leased := lifecycleNow.Add(-10 * time.Second)
	ev := TaskEvidence{Entry: entryView("m-task", lifecycleNow.Add(-time.Minute), "lease-7",
		&leased, lifecycleNow.Add(time.Hour), string(payload))}

	historyLen := 1
	task, err := ev.Task(TaskStateWorking, StateBasisWorking, &historyLen, lifecycleNow)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if task.ID != "m-task" {
		t.Errorf("id = %q, want the crier message id %q", task.ID, "m-task")
	}
	if task.ContextID != "ctx-9" {
		t.Errorf("contextId = %q, want the message's own context", task.ContextID)
	}
	if task.Status.State != TaskStateWorking {
		t.Errorf("state = %q, want %q", task.Status.State, TaskStateWorking)
	}
	if task.Status.Timestamp != formatTimestamp(leased) {
		t.Errorf("timestamp = %q, want the lease instant %q", task.Status.Timestamp, formatTimestamp(leased))
	}
	if task.Status.Message != nil {
		t.Errorf("a WORKING task carries no status message, got %+v", task.Status.Message)
	}
	if len(task.History) != 1 || task.History[0].MessageID != "msg-client-1" {
		t.Errorf("history = %+v, want the creating message", task.History)
	}
	meta, ok := task.Metadata["crier"].(CrierMeta)
	if !ok {
		t.Fatalf("metadata.crier = %#v, want a CrierMeta", task.Metadata["crier"])
	}
	if meta.Transport != TransportInbox {
		t.Errorf("transport = %q, want %q", meta.Transport, TransportInbox)
	}
	if meta.StateBasis != StateBasisWorking {
		t.Errorf("state_basis = %q, want %q: a read task states which record its state came from", meta.StateBasis, StateBasisWorking)
	}

	// §3.2.4: historyLength 0 asks for NO history, and this server's default
	// (absent) is the creating message.
	zero := 0
	noHistory, err := ev.Task(TaskStateWorking, StateBasisWorking, &zero, lifecycleNow)
	if err != nil {
		t.Fatalf("Task(historyLength=0): %v", err)
	}
	if len(noHistory.History) != 0 {
		t.Errorf("historyLength=0 returned %d history messages, want none", len(noHistory.History))
	}
	def, err := ev.Task(TaskStateWorking, StateBasisWorking, nil, lifecycleNow)
	if err != nil {
		t.Fatalf("Task(historyLength absent): %v", err)
	}
	if len(def.History) != 1 {
		t.Errorf("historyLength absent returned %d history messages, want the creating one", len(def.History))
	}
}

// TestTaskLifecycleFailedStatusExplainsItself: the states whose REASON is not in
// their name carry a one-sentence explanation, and a settled task's metadata
// says when the failure was recorded.
func TestTaskLifecycleFailedStatusExplainsItself(t *testing.T) {
	expiredAt := lifecycleNow.Add(-time.Second)
	ev := TaskEvidence{Entry: entryView("m-exp", lifecycleNow.Add(-time.Hour), "", nil, expiredAt, `{}`)}
	task, err := ev.Task(TaskStateFailed, StateBasisExpired, nil, lifecycleNow)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if task.Status.Message == nil || len(task.Status.Message.Parts) != 1 || task.Status.Message.Parts[0].Text == nil {
		t.Fatalf("a FAILED task must say why: %+v", task.Status.Message)
	}
	if !strings.Contains(*task.Status.Message.Parts[0].Text, "TTL elapsed unacknowledged") {
		t.Errorf("status message = %q, want it to name the TTL expiry", *task.Status.Message.Parts[0].Text)
	}

	swept := TaskEvidence{DeadLetteredAt: lifecycleNow.Add(-time.Minute)}
	task, err = swept.Task(TaskStateFailed, StateBasisDeadLetter, nil, lifecycleNow)
	if err != nil {
		t.Fatalf("Task(swept): %v", err)
	}
	meta, ok := task.Metadata["crier"].(CrierMeta)
	if !ok {
		t.Fatalf("metadata.crier = %#v, want a CrierMeta", task.Metadata["crier"])
	}
	if meta.DeadLetteredAt != formatTimestamp(lifecycleNow.Add(-time.Minute)) {
		t.Errorf("dead_lettered_at = %q, want the sweep's instant", meta.DeadLetteredAt)
	}
}

// TestTaskLifecycleParamsAreStrict: a parameter this binding cannot honour is
// refused, naming the field — never silently dropped (DF-CRIER-180's rule, which
// the send operations are already held to).
func TestTaskLifecycleParamsAreStrict(t *testing.T) {
	idCases := []struct {
		name   string
		raw    string
		field  string
		decode func([]byte) *RPCError
	}{
		{"GetTask: params absent", ``, "params", func(b []byte) *RPCError { _, e := DecodeGetTaskParams(b); return e }},
		{"GetTask: params not an object", `[]`, "params", func(b []byte) *RPCError { _, e := DecodeGetTaskParams(b); return e }},
		{"GetTask: id missing", `{}`, "id", func(b []byte) *RPCError { _, e := DecodeGetTaskParams(b); return e }},
		{"GetTask: a misspelled member", `{"id":"m","historLength":2}`, "unknown field", func(b []byte) *RPCError { _, e := DecodeGetTaskParams(b); return e }},
		{"GetTask: negative historyLength", `{"id":"m","historyLength":-1}`, "historyLength", func(b []byte) *RPCError { _, e := DecodeGetTaskParams(b); return e }},
		{"ListTasks: status is not a state", `{"status":"TASK_STATE_RUNNING"}`, "status", func(b []byte) *RPCError { _, e := DecodeListTasksParams(b); return e }},
		{"ListTasks: pageSize 0", `{"pageSize":0}`, "pageSize", func(b []byte) *RPCError { _, e := DecodeListTasksParams(b); return e }},
		{"ListTasks: pageSize 101", `{"pageSize":101}`, "pageSize", func(b []byte) *RPCError { _, e := DecodeListTasksParams(b); return e }},
		{"ListTasks: statusTimestampAfter is not a timestamp", `{"statusTimestampAfter":"yesterday"}`, "statusTimestampAfter", func(b []byte) *RPCError { _, e := DecodeListTasksParams(b); return e }},
		{"CancelTask: id missing", `{"tenant":"a"}`, "id", func(b []byte) *RPCError { _, e := DecodeCancelTaskParams(b); return e }},
		{"SubscribeToTask: id missing", `{}`, "id", func(b []byte) *RPCError { _, e := DecodeSubscribeToTaskParams(b); return e }},
	}
	for _, tc := range idCases {
		t.Run(tc.name, func(t *testing.T) {
			rpcErr := tc.decode([]byte(tc.raw))
			if rpcErr == nil {
				t.Fatalf("params %s were accepted, want a refusal", tc.raw)
			}
			if rpcErr.Code != CodeInvalidParams {
				t.Errorf("code = %d, want %d (InvalidParams)", rpcErr.Code, CodeInvalidParams)
			}
			if !strings.Contains(rpcErr.Message, tc.field) {
				t.Errorf("message = %q, want it to name %q", rpcErr.Message, tc.field)
			}
		})
	}

	// The accepted shapes decode, including the empty object (whose REQUIRED
	// members the caller then checks).
	if _, e := DecodeListTasksParams([]byte(`{}`)); e != nil {
		t.Errorf("ListTasks with no filters was refused: %v", e)
	}
	params, e := DecodeListTasksParams([]byte(`{"tenant":"a","contextId":"c","status":"TASK_STATE_WORKING","pageSize":2,"pageToken":"x","historyLength":3,"statusTimestampAfter":"2026-09-25T00:00:00Z","includeArtifacts":true}`))
	if e != nil {
		t.Fatalf("a fully specified ListTasks request was refused: %v", e)
	}
	if params.ContextID != "c" || params.Status != "TASK_STATE_WORKING" || *params.PageSize != 2 ||
		params.PageToken != "x" || *params.HistoryLength != 3 || !params.IncludeArtifacts {
		t.Errorf("decoded params = %+v, want every filter carried", params)
	}
}

// TestTaskLifecyclePageTokenIsOpaqueAndChecked: a cursor this server did not
// mint is refused by name, and one it did mint round-trips exactly.
func TestTaskLifecyclePageTokenIsOpaqueAndChecked(t *testing.T) {
	at := lifecycleNow.Add(-time.Minute)
	token := EncodePageToken(at, "m-17")
	if token == "" {
		t.Fatal("EncodePageToken returned an empty token")
	}
	if strings.Contains(token, "m-17") {
		t.Errorf("token %q leaks its contents; it is opaque by contract", token)
	}
	gotAt, gotID, err := DecodePageToken(token)
	if err != nil {
		t.Fatalf("DecodePageToken(%q): %v", token, err)
	}
	if !gotAt.Equal(at) || gotID != "m-17" {
		t.Errorf("round trip = (%s, %q), want (%s, %q)", gotAt, gotID, at, "m-17")
	}
	for _, bad := range []string{"not-base64!!", "e30", "eyJ0Ijoibm93IiwiaWQiOiJtIn0"} {
		if _, _, err := DecodePageToken(bad); err == nil {
			t.Errorf("DecodePageToken(%q) accepted a token this server never issued", bad)
		}
	}
	if _, _, err := DecodePageToken("bWludGU"); err != nil {
		t.Logf("(a non-JSON token is refused: %v)", err)
	}
}

// TestTaskLifecycleListFilterOrderAndPagination exercises §3.1.4: the filters,
// the DESCENDING status-timestamp ordering with a total tie-break, cursor
// pagination whose pages do not shift when the queue does, and the response
// numbers the specification requires.
func TestTaskLifecycleListFilterOrderAndPagination(t *testing.T) {
	base := lifecycleNow.Add(-time.Hour)
	// Four tasks with four distinct ids, two of them sharing the NEWEST
	// timestamp: the id is the tie-break that makes the order total, which is
	// what lets a cursor name a position in it exactly.
	item := func(id, contextID string, state TaskState, ts time.Time) ListItem {
		ev := TaskEvidence{Entry: entryView(id, base, "", nil, lifecycleNow.Add(time.Hour),
			`{"context_id":"`+contextID+`","parts":[]}`)}
		return ListItem{ID: id, Evidence: ev, State: state, Basis: StateBasisSubmitted, StatusTimestamp: ts}
	}
	items := []ListItem{
		item("m-a", "ctx-a", TaskStateSubmitted, base),
		item("m-b", "ctx-b", TaskStateSubmitted, base.Add(10*time.Minute)),
		item("m-c", "ctx-a", TaskStateFailed, base.Add(20*time.Minute)),
		item("m-d", "ctx-a", TaskStateSubmitted, base.Add(20*time.Minute)),
	}
	pageSize := 2
	params := &ListTasksParams{PageSize: &pageSize}

	page, rpcErr := BuildListPage(items, params)
	if rpcErr != nil {
		t.Fatalf("BuildListPage: %v", rpcErr)
	}
	if page.TotalSize != 4 {
		t.Errorf("totalSize = %d, want 4 (every match, before pagination)", page.TotalSize)
	}
	if page.PageSize != 2 || len(page.Items) != 2 {
		t.Errorf("page = %d items (pageSize %d), want 2", len(page.Items), page.PageSize)
	}
	if got := []string{page.Items[0].ID, page.Items[1].ID}; got[0] != "m-d" || got[1] != "m-c" {
		t.Errorf("first page = %v, want the two newest first (m-d, m-c: newest timestamp, id descending)", got)
	}
	if page.NextPageToken == "" {
		t.Fatal("a full page with more items behind it must carry a nextPageToken")
	}

	// The cursor is a POSITION, not an offset: removing the items already
	// returned (which is what acking them does) must not shift the next page.
	next, rpcErr := BuildListPage(items, &ListTasksParams{PageSize: &pageSize, PageToken: page.NextPageToken})
	if rpcErr != nil {
		t.Fatalf("BuildListPage(page 2): %v", rpcErr)
	}
	if len(next.Items) != 2 || next.Items[0].ID != "m-b" || next.Items[1].ID != "m-a" {
		t.Fatalf("page 2 = %+v, want m-b then m-a", next.Items)
	}
	if next.NextPageToken != "" {
		t.Errorf("the final page carries a nextPageToken (%q); §3.1.4 requires an empty one", next.NextPageToken)
	}
	// The first page (m-d, m-c) was consumed — which is what acknowledging them
	// does — so only m-a and m-b remain. The next page must be unchanged.
	pruned := []ListItem{items[0], items[1]}
	again, rpcErr := BuildListPage(pruned, &ListTasksParams{PageSize: &pageSize, PageToken: page.NextPageToken})
	if rpcErr != nil {
		t.Fatalf("BuildListPage(page 2, pruned): %v", rpcErr)
	}
	if len(again.Items) != 2 || again.Items[0].ID != "m-b" {
		t.Errorf("a consumed first page must not change the next page, got %+v", again.Items)
	}

	// Filters.
	byContext, rpcErr := BuildListPage(items, &ListTasksParams{ContextID: "ctx-b"})
	if rpcErr != nil {
		t.Fatalf("BuildListPage(contextId): %v", rpcErr)
	}
	if len(byContext.Items) != 1 || byContext.Items[0].ID != "m-b" {
		t.Errorf("contextId filter = %+v, want only m-b", byContext.Items)
	}
	byStatus, rpcErr := BuildListPage(items, &ListTasksParams{Status: string(TaskStateFailed)})
	if rpcErr != nil {
		t.Fatalf("BuildListPage(status): %v", rpcErr)
	}
	if len(byStatus.Items) != 1 || byStatus.Items[0].State != TaskStateFailed {
		t.Errorf("status filter = %+v, want only the FAILED task", byStatus.Items)
	}
	after, rpcErr := BuildListPage(items, &ListTasksParams{StatusTimestampAfter: base.Add(5 * time.Minute).Format(time.RFC3339)})
	if rpcErr != nil {
		t.Fatalf("BuildListPage(statusTimestampAfter): %v", rpcErr)
	}
	if len(after.Items) != 3 {
		t.Errorf("statusTimestampAfter = %d tasks, want the 3 at or after that instant", len(after.Items))
	}

	// A page token this server did not mint is refused by name.
	_, rpcErr = BuildListPage(items, &ListTasksParams{PageToken: "not-a-token"})
	if rpcErr == nil || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("a bogus pageToken = %v, want InvalidParams", rpcErr)
	}
	if !strings.Contains(rpcErr.Message, "pageToken") {
		t.Errorf("message = %q, want it to name pageToken", rpcErr.Message)
	}
}

// TestTaskLifecycleListRendersArtifactsOnlyWhenAsked pins §3.1.4's conditional
// artifacts member: absent when includeArtifacts is false, an explicit (empty)
// array when it is true — crier keeps no artifact for a task, and says so
// instead of dropping a member the client asked for.
func TestTaskLifecycleListRendersArtifactsOnlyWhenAsked(t *testing.T) {
	ev := TaskEvidence{Entry: entryView("m-1", lifecycleNow.Add(-time.Minute), "", nil, lifecycleNow.Add(time.Hour), `{}`)}
	task, err := ev.Task(TaskStateSubmitted, StateBasisSubmitted, nil, lifecycleNow)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	plain, err := json.Marshal(task.ForListing(false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plain), "artifacts") {
		t.Errorf("includeArtifacts=false must omit the member entirely: %s", plain)
	}
	withArtifacts, err := json.Marshal(task.ForListing(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(withArtifacts, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, ok := decoded["artifacts"]
	if !ok {
		t.Fatalf("includeArtifacts=true must include the member: %s", withArtifacts)
	}
	if string(raw) != "[]" {
		t.Errorf("artifacts = %s, want an empty array", raw)
	}
}

// TestTaskLifecycleRefusals pins the code, the reason and the honesty of each
// refusal this row adds: the wrong answer here is a 200 pretending the
// operation happened.
func TestTaskLifecycleRefusals(t *testing.T) {
	notFound := TaskNotFoundError("m-1", "agent-a")
	if notFound.Code != CodeTaskNotFoundError {
		t.Errorf("TaskNotFoundError code = %d, want %d", notFound.Code, CodeTaskNotFoundError)
	}
	if !strings.Contains(notFound.Message, "no tombstone") ||
		!strings.Contains(notFound.Message, "completed and purged") {
		t.Errorf("TaskNotFoundError must state WHY a purged task has no state: %q", notFound.Message)
	}
	if reason := reasonOf(t, notFound); reason != ReasonTaskNotFound {
		t.Errorf("reason = %q, want %q", reason, ReasonTaskNotFound)
	}

	notCancelable := TaskNotCancelableError("m-1", TaskStateFailed)
	if notCancelable.Code != CodeTaskNotCancelableError {
		t.Errorf("TaskNotCancelableError code = %d, want %d", notCancelable.Code, CodeTaskNotCancelableError)
	}
	if reason := reasonOf(t, notCancelable); reason != ReasonTaskNotCancelable {
		t.Errorf("reason = %q, want %q", reason, ReasonTaskNotCancelable)
	}
	if !strings.Contains(notCancelable.Message, string(TaskStateFailed)) {
		t.Errorf("TaskNotCancelableError must name the state: %q", notCancelable.Message)
	}

	for _, tc := range []struct {
		name string
		err  *RPCError
		code int
	}{
		{"a message to a terminal task", TerminalTaskMessageError("m-1", TaskStateCompleted), CodeUnsupportedOperationError},
		{"a message continuing an open task", TaskContinuationUnsupportedError("m-1", TaskStateWorking), CodeUnsupportedOperationError},
		{"a subscription to a terminal task", SubscribeToTerminalTaskError("m-1", TaskStateFailed), CodeUnsupportedOperationError},
		{"a backend without the capability", TaskCapabilityUnsupportedError("GetTask", "peek"), CodeUnsupportedOperationError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Code != tc.code {
				t.Errorf("code = %d, want %d", tc.err.Code, tc.code)
			}
			if len(tc.err.Data) == 0 {
				t.Error("an A2A-specific refusal must carry its ErrorInfo detail")
			}
		})
	}

	// The refusal for a message aimed at a task in a terminal state must name
	// the terminal states, because that is the specification's own rule (§3.1.1)
	// and the client has to know which states are involved.
	terminal := TerminalTaskMessageError("m-1", TaskStateCanceled)
	for _, state := range []TaskState{TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected} {
		if !strings.Contains(terminal.Message, string(state)) {
			t.Errorf("the terminal-state refusal does not name %s: %q", state, terminal.Message)
		}
	}
}

// reasonOf extracts an A2A detail object's reason.
func reasonOf(t *testing.T, err *RPCError) string {
	t.Helper()
	for _, detail := range err.Data {
		info, ok := detail.(ErrorInfo)
		if !ok {
			continue
		}
		return info.Reason
	}
	return ""
}

// TestTaskLifecycleStateNamesAreTheEnumsOwn: the states this binding can name
// are §4.1.3's, verbatim, and the four terminal ones are exactly the four
// §3.1.1 refuses messages to.
func TestTaskLifecycleStateNamesAreTheEnumsOwn(t *testing.T) {
	names := taskStateNames()
	for _, want := range []string{
		"TASK_STATE_SUBMITTED", "TASK_STATE_WORKING", "TASK_STATE_COMPLETED",
		"TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_REJECTED", "TASK_STATE_UNSPECIFIED",
	} {
		if !contains(names, want) {
			t.Errorf("%s is missing from the state names this binding accepts", want)
		}
	}
	terminal := 0
	for _, name := range names {
		if TaskState(name).IsTerminal() {
			terminal++
		}
	}
	if terminal != 4 {
		t.Errorf("%d terminal states, want 4 (COMPLETED, FAILED, CANCELED, REJECTED)", terminal)
	}
}
