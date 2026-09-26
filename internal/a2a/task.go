// task.go — the A2A task lifecycle mapped onto crier's inbox entry and its
// lease/ack lifecycle (INT-A2A-004, specs/A2A-OPTION.md §5.5).
//
// An A2A task is not a second thing crier has to keep: the task IS the inbox
// entry. Its id is the entry's message id, its state is what crier's own store
// can prove about that entry right now, and its terminal states are produced by
// the operations crier already has — Retrieve leases (WORKING), Ack acknowledges
// and removes (COMPLETED), the TTL sweep removes and records (FAILED), and
// CancelTask (this row) releases the lease and closes the entry (CANCELED).
//
// Two rules shape everything here, because this is where an interop adapter
// most easily lies about state:
//
//  1. a state is asserted only from a record crier actually holds. The entry is
//     one record; the expiry sweep's dead letter is another. Where crier holds
//     neither — the message was acknowledged and removed, or never existed at
//     all — the honest answer is TaskNotFoundError, which the specification
//     itself defines for a task that is "invalid, expired, or already completed
//     and purged" (§3.3.2). Inventing COMPLETED for it would report a state for
//     a task that may equally have been purged, transferred to another inbox,
//     or never delivered.
//  2. the mapping is a pure function of the evidence, so it is testable without
//     a server and cannot drift from the table the spec publishes.
package a2a

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Method names, verbatim from the specification (§9.4, PascalCase per §9.1).
// INT-A2A-003 shipped the two send operations; this row adds the task
// lifecycle. The push-notification methods and GetExtendedAgentCard remain
// later rows (INT-A2A-005/006) and are still answered with
// MethodNotFoundError.
const (
	// MethodGetTask is §9.4.3 / §3.1.3: the current state of one task.
	MethodGetTask = "GetTask"
	// MethodListTasks is §9.4.4 / §3.1.4: the tasks of one agent, filtered and
	// cursor-paginated.
	MethodListTasks = "ListTasks"
	// MethodCancelTask is §9.4.5 / §3.1.5: cancel a task that is not terminal.
	MethodCancelTask = "CancelTask"
	// MethodSubscribeToTask is §9.4.6 / §3.1.6: stream the updates of a task
	// that is not terminal.
	MethodSubscribeToTask = "SubscribeToTask"
)

// ListTasks bounds (§3.1.4): "If unspecified, at most 50 tasks will be
// returned. The minimum value is 1. The maximum value is 100." A pageSize
// outside that range is refused with InvalidParams rather than clamped: a
// client that asked for 200 tasks and silently got 100 would be told a page was
// the whole listing.
const (
	DefaultListPageSize = 50
	MinListPageSize     = 1
	MaxListPageSize     = 100
)

// TransportInbox is the crier transport every A2A task resolved from the store
// carries: the task's message is an inbox entry.
const TransportInbox = "inbox"

// State bases: the crier record a task's state was read from. They travel in
// the task's metadata under `crier.state_basis` so a client is never handed a
// state without the evidence behind it (the mapping table's own column).
const (
	// StateBasisSubmitted — an inbox entry no one has claimed.
	StateBasisSubmitted = "inbox-entry-unleased"
	// StateBasisWorking — an inbox entry leased by a consumer.
	StateBasisWorking = "inbox-entry-leased"
	// StateBasisExpired — an inbox entry past its TTL, still unacknowledged.
	StateBasisExpired = "inbox-entry-ttl-elapsed-unacked"
	// StateBasisClosed — a closed (ACKed) entry a backend retained.
	StateBasisClosed = "inbox-entry-closed"
	// StateBasisDeadLetter — the expiry sweep's durable failure record.
	StateBasisDeadLetter = "dead-letter-recorded"
	// StateBasisCanceled — the task this request itself canceled.
	StateBasisCanceled = "canceled-by-request"
)

// ---------------------------------------------------------------------------
// Parameters (§3.2.1, §3.2.5, §3.2.6, §3.1.3-§3.1.6)
// ---------------------------------------------------------------------------

// GetTaskParams is the `params` object of GetTask (§9.4.3): the task's id, an
// optional history length, and the tenant that names the agent whose inbox the
// task lives in.
type GetTaskParams struct {
	Tenant        string `json:"tenant,omitempty"`
	ID            string `json:"id"`
	HistoryLength *int   `json:"historyLength,omitempty"`
}

// ListTasksParams is the `params` object of ListTasks (§9.4.4).
type ListTasksParams struct {
	Tenant               string `json:"tenant,omitempty"`
	ContextID            string `json:"contextId,omitempty"`
	Status               string `json:"status,omitempty"`
	PageSize             *int   `json:"pageSize,omitempty"`
	PageToken            string `json:"pageToken,omitempty"`
	HistoryLength        *int   `json:"historyLength,omitempty"`
	StatusTimestampAfter string `json:"statusTimestampAfter,omitempty"`
	IncludeArtifacts     bool   `json:"includeArtifacts,omitempty"`
}

// CancelTaskParams is the `params` object of CancelTask (§9.4.5).
type CancelTaskParams struct {
	Tenant   string         `json:"tenant,omitempty"`
	ID       string         `json:"id"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// SubscribeToTaskParams is the `params` object of SubscribeToTask (§9.4.6).
type SubscribeToTaskParams struct {
	Tenant string `json:"tenant,omitempty"`
	ID     string `json:"id"`
}

// DecodeGetTaskParams strictly decodes the `params` object of GetTask and
// refuses everything it cannot carry faithfully, naming the field. The
// discipline is DecodeSendMessageParams': a member this binding does not
// declare is a refusal listing the accepted set, never a silently dropped
// parameter.
func DecodeGetTaskParams(raw json.RawMessage) (*GetTaskParams, *RPCError) {
	var params GetTaskParams
	if rpcErr := decodeTaskParams(raw, &params); rpcErr != nil {
		return nil, rpcErr
	}
	if err := requireTaskID(params.ID); err != nil {
		return nil, refusalOf(err)
	}
	if err := checkHistoryLength(params.HistoryLength, "historyLength"); err != nil {
		return nil, refusalOf(err)
	}
	return &params, nil
}

// DecodeListTasksParams strictly decodes the `params` object of ListTasks
// (§9.4.4). Every filter it declares is honoured; a parameter it cannot honour
// is refused rather than ignored (DF-CRIER-180's rule for a request-level
// parameter).
func DecodeListTasksParams(raw json.RawMessage) (*ListTasksParams, *RPCError) {
	var params ListTasksParams
	if rpcErr := decodeTaskParams(raw, &params); rpcErr != nil {
		return nil, rpcErr
	}
	if err := checkHistoryLength(params.HistoryLength, "historyLength"); err != nil {
		return nil, refusalOf(err)
	}
	if params.PageSize != nil {
		switch {
		case *params.PageSize < MinListPageSize:
			return nil, refusalOf(invalidParams("pageSize", "must be at least %d, got %d", MinListPageSize, *params.PageSize))
		case *params.PageSize > MaxListPageSize:
			return nil, refusalOf(invalidParams("pageSize", "must be at most %d, got %d", MaxListPageSize, *params.PageSize))
		}
	}
	if params.Status != "" {
		if !isKnownTaskState(TaskState(params.Status)) {
			return nil, refusalOf(invalidParams("status", "must be one of %s, got %q",
				strings.Join(taskStateNames(), ", "), params.Status))
		}
	}
	if params.StatusTimestampAfter != "" {
		if _, err := time.Parse(time.RFC3339, params.StatusTimestampAfter); err != nil {
			return nil, refusalOf(invalidParams("statusTimestampAfter",
				"must be an ISO 8601 timestamp (e.g. \"2023-10-27T10:00:00Z\"), got %q", params.StatusTimestampAfter))
		}
	}
	return &params, nil
}

// DecodeCancelTaskParams strictly decodes the `params` object of CancelTask
// (§9.4.5).
func DecodeCancelTaskParams(raw json.RawMessage) (*CancelTaskParams, *RPCError) {
	var params CancelTaskParams
	if rpcErr := decodeTaskParams(raw, &params); rpcErr != nil {
		return nil, rpcErr
	}
	if err := requireTaskID(params.ID); err != nil {
		return nil, refusalOf(err)
	}
	return &params, nil
}

// DecodeSubscribeToTaskParams strictly decodes the `params` object of
// SubscribeToTask (§9.4.6).
func DecodeSubscribeToTaskParams(raw json.RawMessage) (*SubscribeToTaskParams, *RPCError) {
	var params SubscribeToTaskParams
	if rpcErr := decodeTaskParams(raw, &params); rpcErr != nil {
		return nil, rpcErr
	}
	if err := requireTaskID(params.ID); err != nil {
		return nil, refusalOf(err)
	}
	return &params, nil
}

// decodeTaskParams is the shared strict decode of a task-lifecycle params
// object: the object must be present and an object, and every member must be
// one the destination type declares.
func decodeTaskParams(raw json.RawMessage, dst any) *RPCError {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return NewRPCError(CodeInvalidParams, "params is required",
			InvalidParamsDetail(&InvalidParamsError{Field: "params", Detail: "is required"}))
	}
	if trimmed[0] != '{' {
		return NewRPCError(CodeInvalidParams, "params must be a JSON object",
			InvalidParamsDetail(&InvalidParamsError{Field: "params", Detail: "must be a JSON object"}))
	}
	if trimmed == "{}" {
		// Legal: the required members are checked by the caller, which names
		// the field that is missing rather than json's pathless message.
		return nil
	}
	if err := strictDecode([]byte(trimmed), dst); err != nil {
		return invalidParamsError("params", err)
	}
	return nil
}

// requireTaskID enforces §3.1.3's REQUIRED task id.
func requireTaskID(id string) error {
	if strings.TrimSpace(id) == "" {
		return invalidParams("id", "is required: it is the task's id, which is the crier message id the send answered with")
	}
	return nil
}

// checkHistoryLength enforces §3.2.4's historyLength domain.
func checkHistoryLength(length *int, field string) error {
	if length != nil && *length < 0 {
		return invalidParams(field, "must not be negative, got %d", *length)
	}
	return nil
}

// isKnownTaskState reports whether s is one of §4.1.3's states.
func isKnownTaskState(s TaskState) bool {
	for _, name := range taskStateNames() {
		if string(s) == name {
			return true
		}
	}
	return false
}

// taskStateNames is the enum's own names, in the order a refusal prints them.
func taskStateNames() []string {
	return []string{
		string(TaskStateSubmitted), string(TaskStateWorking), string(TaskStateCompleted),
		string(TaskStateFailed), string(TaskStateCanceled), string(TaskStateRejected),
		string(TaskStateUnspecified),
	}
}

// ---------------------------------------------------------------------------
// The mapping — crier's records ⇄ an A2A Task
// ---------------------------------------------------------------------------

// InboxView is a SNAPSHOT of one inbox entry, as crier's store holds it. It is
// a plain value so the mapping below is a pure function of it: the adapter
// copies the store's row into one of these, and everything after that is
// testable without a store, a clock or a server.
type InboxView struct {
	ID        string
	Payload   json.RawMessage
	CreatedAt time.Time
	LeasedAt  *time.Time
	LeaseID   string
	// ExpiresAt is the entry's resolved TTL. The zero time is "never expires"
	// (ttl_seconds=0, DF-CRIER-37) — not "expired long ago".
	ExpiresAt time.Time
	// ACKed is the store's own "closed" flag. Both shipped backends REMOVE an
	// entry on ack, so it is false on every row they hold; a backend that ever
	// retains a closed row is mapped honestly rather than reported as missing.
	ACKed bool
}

// expired reports whether the entry's TTL has elapsed, never-expires excluded.
func (v InboxView) expired(now time.Time) bool {
	return !v.ExpiresAt.IsZero() && now.After(v.ExpiresAt)
}

// TaskEvidence is everything crier can prove about ONE task id: the inbox entry
// it holds (nil when it holds none) and the expiry sweep's durable failure
// record, if that sweep ever removed the message.
//
// Both facts are needed because crier's store keeps no tombstone: an entry that
// is gone was either acknowledged (and is unobservable — "completed and purged"
// in the specification's own words for TaskNotFoundError) or swept after its
// TTL, and only the second of those leaves a record behind.
type TaskEvidence struct {
	Entry *InboxView
	// DeadLetteredAt is when the expiry sweep recorded this message as a dead
	// letter, zero when it recorded nothing.
	DeadLetteredAt time.Time
}

// Present reports whether crier holds ANY record for this task id.
func (e TaskEvidence) Present() bool { return e.Entry != nil || !e.DeadLetteredAt.IsZero() }

// TaskID is the task's id: crier's own message id (a task id the client sends
// is resolved against this, never minted by the client — §3.4.2).
func (e TaskEvidence) TaskID() string {
	if e.Entry != nil {
		return e.Entry.ID
	}
	return ""
}

// Resolve maps the evidence onto §4.1.3's state, and reports which crier record
// that state came from. agentID and taskID are carried into the not-found
// refusal so it can name what was looked for.
//
// The table, in the order the cases are decided (and asserted in tests):
//
//	entry present, closed (ACKed)  → TASK_STATE_COMPLETED   [inbox-entry-closed]
//	entry present, TTL elapsed     → TASK_STATE_FAILED      [inbox-entry-ttl-elapsed-unacked]
//	entry present, leased          → TASK_STATE_WORKING     [inbox-entry-leased]
//	entry present, unleased        → TASK_STATE_SUBMITTED   [inbox-entry-unleased]
//	no entry, failure recorded     → TASK_STATE_FAILED      [dead-letter-recorded]
//	no entry, no failure record    → TaskNotFoundError      [no crier record]
//
// Two of those are deliberate choices, stated so a client need not infer them:
//
//   - an entry PAST ITS TTL is FAILED even before the sweep removes it: it can
//     no longer be retrieved (every consumption path skips an expired row), so
//     the work cannot complete, and reporting the submitted state it still
//     stores would tell a client to keep waiting for a message nobody can
//     claim. The sweep will record the same outcome durably a moment later.
//   - a CLOSED row is COMPLETED: the store's own ACKed flag IS crier's "final"
//     marker (Retrieve, Stats, QueueDepth and RevokeLeases all skip one).
//     Neither shipped backend retains such a row — Ack deletes — so this case is
//     unreachable in production and exists so that a backend which ever changes
//     that is mapped honestly instead of reported as not-found.
func (e TaskEvidence) Resolve(agentID, taskID string, now time.Time) (TaskState, string, *RPCError) {
	if e.Entry != nil {
		switch {
		case e.Entry.ACKed:
			return TaskStateCompleted, StateBasisClosed, nil
		case e.Entry.expired(now):
			return TaskStateFailed, StateBasisExpired, nil
		case e.Entry.LeaseID != "":
			return TaskStateWorking, StateBasisWorking, nil
		default:
			return TaskStateSubmitted, StateBasisSubmitted, nil
		}
	}
	if !e.DeadLetteredAt.IsZero() {
		return TaskStateFailed, StateBasisDeadLetter, nil
	}
	return "", "", TaskNotFoundError(taskID, agentID)
}

// StatusTimestamp is when this task's status last moved, which is the ordering
// §3.1.4 requires a listing to use ("sorted by last update time in descending
// order"). It is read from crier's own timestamps and never invented: a lease
// stamps its instant, a TTL expiry stamps the instant that elapsed, an
// unclaimed message is as old as its delivery.
func (e TaskEvidence) StatusTimestamp(now time.Time) time.Time {
	switch {
	case e.Entry == nil:
		return e.DeadLetteredAt
	case e.Entry.expired(now):
		return e.Entry.ExpiresAt
	case e.Entry.LeasedAt != nil:
		return *e.Entry.LeasedAt
	default:
		return e.Entry.CreatedAt
	}
}

// Task builds the A2A Task for a resolved state: the id, the state, the
// timestamp, the creating message (bounded by §3.2.4's historyLength) and the
// crier facts A2A has no field for, under metadata.crier.
//
// The metadata is the SAME CrierMeta a SendMessage answer carries, so one task
// read two ways states the same facts the same way — plus `state_basis`, the
// crier record the state was read from, which is what keeps this adapter from
// asserting a state without its evidence.
func (e TaskEvidence) Task(state TaskState, basis string, historyLength *int, now time.Time) (*Task, error) {
	if !e.Present() {
		return nil, fmt.Errorf("a2a: no crier record to build a task from")
	}
	id := e.TaskID()
	contextID := id
	meta := CrierMeta{Transport: TransportInbox, StateBasis: basis}
	var history []Message

	if e.Entry != nil {
		meta.ExpiresAt = expiryJSON(e.Entry.ExpiresAt)
		// The creating message is the delivery payload, when that payload is
		// one of this binding's own envelopes (an A2A-originated task always
		// is; a task started by any other crier sender has whatever payload it
		// was given, and no A2A message can be reconstructed from it).
		if env, ok := ParseEnvelope(e.Entry.Payload); ok {
			if env.ContextID != "" {
				contextID = env.ContextID
			}
			if historyAllows(historyLength) {
				if msg, err := env.Message(); err == nil && msg != nil {
					history = []Message{*msg}
				}
			}
		}
	} else {
		meta.DeadLetteredAt = formatTimestamp(e.DeadLetteredAt)
	}

	task := &Task{
		ID:        id,
		ContextID: contextID,
		Status: TaskStatus{
			State:     state,
			Timestamp: formatTimestamp(e.StatusTimestamp(now)),
			Message:   statusMessage(state, e, contextID),
		},
		Metadata: map[string]any{"crier": meta},
	}
	if len(history) > 0 {
		task.History = history
	}
	return task, nil
}

// expiryJSON renders an entry's resolved expiry the way the delivery accept
// states it: JSON null for a message that never expires, the RFC 3339 instant
// otherwise, and absent when there is no inbox entry behind the task.
func expiryJSON(at time.Time) json.RawMessage {
	if at.IsZero() {
		return json.RawMessage("null")
	}
	raw, err := json.Marshal(at.UTC())
	if err != nil {
		return nil
	}
	return raw
}

// statusMessage is the one-sentence explanation a client gets for the states
// whose REASON is not in the state's name. SUBMITTED and WORKING carry none:
// the state says everything crier knows.
func statusMessage(state TaskState, e TaskEvidence, contextID string) *Message {
	var text string
	switch state {
	case TaskStateFailed:
		if e.Entry != nil {
			text = fmt.Sprintf(
				"the message's TTL elapsed unacknowledged at %s: every consumption path skips an expired entry, so the message can no longer be retrieved and the task cannot complete",
				formatTimestamp(e.Entry.ExpiresAt))
		} else {
			text = fmt.Sprintf(
				"the message expired unacknowledged and was removed from the inbox by the expiry sweep, which recorded it as a dead letter at %s",
				formatTimestamp(e.DeadLetteredAt))
		}
	case TaskStateCompleted:
		text = "the message was acknowledged: the inbox entry is closed and can no longer be retrieved"
	case TaskStateCanceled:
		text = "canceled by request: the lease on the message was released and its inbox entry was closed, so it can no longer be retrieved or acknowledged"
	}
	if text == "" {
		return nil
	}
	id := e.TaskID()
	context := contextID
	if context == "" {
		context = id
	}
	return &Message{
		MessageID: id,
		ContextID: context,
		TaskID:    id,
		Role:      RoleAgent,
		Parts:     []Part{TextPart(text)},
	}
}

// ---------------------------------------------------------------------------
// ListTasks — filtering, ordering and the cursor (§3.1.4)
// ---------------------------------------------------------------------------

// ListItem is one task a listing considered: the evidence it was resolved from,
// the state that resolution produced, the crier record behind it, and when that
// state last moved. The adapter builds these (it is the layer that reads the
// store); ordering, filtering and pagination are applied here, purely.
type ListItem struct {
	ID              string
	Evidence        TaskEvidence
	State           TaskState
	Basis           string
	StatusTimestamp time.Time
}

// ListTask is one task in a ListTasks response. §3.1.4 makes `artifacts`
// conditional on `includeArtifacts`: absent entirely when it is false, present
// (possibly as `[]`) when it is true. A pointer to a slice is what lets an
// EMPTY artifacts member serialize at all — `omitempty` drops an empty slice,
// which would silently ignore the parameter.
type ListTask struct {
	*Task
	Artifacts *[]Artifact `json:"artifacts,omitempty"`
}

// ForListing renders the task as one element of a ListTasks response.
//
// crier keeps no artifact for a task: a task's outputs are relay frames an
// observing stream forwards (§5.4.5), never stored rows, so a listing has none
// to return — and says so with an explicit empty array rather than by dropping
// a member the client asked for.
func (t *Task) ForListing(includeArtifacts bool) ListTask {
	out := ListTask{Task: t}
	if includeArtifacts {
		empty := []Artifact{}
		out.Artifacts = &empty
	}
	return out
}

// ListTasksResult is the `result` of ListTasks (§3.1.4). `nextPageToken` is
// always present and empty on the final page, as the specification requires.
type ListTasksResult struct {
	Tasks         []ListTask `json:"tasks"`
	NextPageToken string     `json:"nextPageToken"`
	PageSize      int        `json:"pageSize"`
	TotalSize     int        `json:"totalSize"`
}

// ListPage is one page of a listing plus the numbers the response must state.
type ListPage struct {
	Items []ListItem
	// NextPageToken is the cursor for the following page, empty when this is
	// the last one.
	NextPageToken string
	// PageSize is the size ACTUALLY used for this response, so a page that ran
	// out of items is not reported as a full one.
	PageSize int
	// TotalSize is how many tasks matched BEFORE pagination.
	TotalSize int
}

// pageCursor is the keyset a page token carries: the last task of the previous
// page, in the listing's own order. Encoding the position rather than an offset
// is what makes a page stable while messages are being claimed and removed
// underneath it — an offset would skip or repeat tasks as the queue shrinks.
type pageCursor struct {
	Timestamp string `json:"t"`
	ID        string `json:"id"`
}

// EncodePageToken renders the cursor a client passes back as `pageToken`. It is
// opaque by contract: a client must not construct or parse one.
func EncodePageToken(statusTimestamp time.Time, id string) string {
	raw, err := json.Marshal(pageCursor{Timestamp: statusTimestamp.UTC().Format(time.RFC3339Nano), ID: id})
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodePageToken reads a cursor this binding minted. A token that is not one
// is refused with InvalidParams naming the field rather than treated as absent:
// a client that invented a token asked for a page this server cannot identify.
func DecodePageToken(token string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("not a page token this server issued")
	}
	var cur pageCursor
	if err := json.Unmarshal(raw, &cur); err != nil {
		return time.Time{}, "", fmt.Errorf("not a page token this server issued")
	}
	at, err := time.Parse(time.RFC3339Nano, cur.Timestamp)
	if err != nil || cur.ID == "" {
		return time.Time{}, "", fmt.Errorf("not a page token this server issued")
	}
	return at, cur.ID, nil
}

// BuildListPage applies §3.1.4's filters, its ordering and its cursor
// pagination to the tasks one agent's store was read for.
//
// The ordering is the specification's own: status timestamp DESCENDING (most
// recently updated first), with the task id as a tie-break so the order is
// total and a cursor can address a position in it exactly.
func BuildListPage(items []ListItem, params *ListTasksParams) (ListPage, *RPCError) {
	var after time.Time
	if params != nil && params.StatusTimestampAfter != "" {
		parsed, err := time.Parse(time.RFC3339, params.StatusTimestampAfter)
		if err != nil {
			// DecodeListTasksParams already refused this; a caller that built
			// params by hand gets the same refusal here.
			return ListPage{}, refusalOf(invalidParams("statusTimestampAfter", "must be an ISO 8601 timestamp"))
		}
		after = parsed
	}

	matching := make([]ListItem, 0, len(items))
	for _, item := range items {
		if params != nil {
			if params.ContextID != "" && contextIDOf(item) != params.ContextID {
				continue
			}
			if params.Status != "" && string(item.State) != params.Status {
				continue
			}
			if !after.IsZero() && item.StatusTimestamp.Before(after) {
				continue
			}
		}
		matching = append(matching, item)
	}

	sort.SliceStable(matching, func(i, j int) bool {
		if !matching[i].StatusTimestamp.Equal(matching[j].StatusTimestamp) {
			return matching[i].StatusTimestamp.After(matching[j].StatusTimestamp)
		}
		return matching[i].ID > matching[j].ID
	})

	total := len(matching)
	page := make([]ListItem, 0, len(matching))
	pageSize := DefaultListPageSize
	if params != nil && params.PageSize != nil {
		pageSize = *params.PageSize
	}

	if params != nil && strings.TrimSpace(params.PageToken) != "" {
		at, id, err := DecodePageToken(params.PageToken)
		if err != nil {
			return ListPage{}, refusalOf(invalidParams("pageToken", "%s", err.Error()))
		}
		for _, item := range matching {
			// Strictly AFTER the cursor's position in this exact order (status
			// timestamp DESC, id DESC): an item that sorts lower comes later.
			if item.StatusTimestamp.Before(at) || (item.StatusTimestamp.Equal(at) && item.ID < id) {
				page = append(page, item)
			}
		}
	} else {
		page = append(page, matching...)
	}

	next := ""
	if len(page) > pageSize {
		last := page[pageSize-1]
		next = EncodePageToken(last.StatusTimestamp, last.ID)
		page = page[:pageSize]
	}
	return ListPage{Items: page, NextPageToken: next, PageSize: len(page), TotalSize: total}, nil
}

// contextIDOf is a task's A2A context id: the context its creating message
// stated, else the task's own id — the inference §3.4.1 sanctions when only a
// task id is known, and the same one Task uses, so a listing and a read of the
// same task agree.
func contextIDOf(item ListItem) string {
	if item.Evidence.Entry != nil {
		if env, ok := ParseEnvelope(item.Evidence.Entry.Payload); ok && env.ContextID != "" {
			return env.ContextID
		}
	}
	return item.ID
}

// ---------------------------------------------------------------------------
// The refusals this row adds (§5.5)
// ---------------------------------------------------------------------------

// TaskNotFoundError is -32001 for a task id crier holds NO record of.
//
// The message states the reason, because this answer is the one an interop
// client is most likely to misread as a failure of the request: crier's ack
// permanently removes an acknowledged entry (DF-CRIER-32) and keeps no
// tombstone, so an acknowledged task is not re-readable — which is exactly the
// case the specification's own definition of this error names ("invalid,
// expired, or already completed and purged", §3.3.2).
func TaskNotFoundError(id, agentID string) *RPCError {
	var what string
	switch {
	case id == "" && agentID == "":
		what = "no crier record exists for this task id"
	case agentID == "":
		what = fmt.Sprintf("no crier record exists for task %q", id)
	default:
		what = fmt.Sprintf("task %q is not in agent %q's inbox and this relay holds no failure record for it", id, agentID)
	}
	return NewRPCError(CodeTaskNotFoundError,
		what+": crier's ack permanently removes an acknowledged message and keeps no tombstone, so an acknowledged task is not re-readable — the A2A specification defines TaskNotFoundError for a task that is invalid, expired, or already completed and purged. A client that needs to observe the terminal state does so while the task still exists (a stream, or the ack's own answer)",
		ErrorInfo{Type: ErrorInfoType, Reason: ReasonTaskNotFound, Domain: ErrorDomain,
			Metadata: map[string]string{"taskId": id, "tenant": agentID}})
}

// TaskNotCancelableError is -32002 for a CancelTask aimed at a task that is
// already terminal (§3.1.5).
func TaskNotCancelableError(id string, state TaskState) *RPCError {
	return NewRPCError(CodeTaskNotCancelableError, fmt.Sprintf(
		"task %q is in the terminal state %s and cannot be canceled: a task that has already completed, failed or been canceled has no claim left to release and no entry left to close",
		id, state),
		ErrorInfo{Type: ErrorInfoType, Reason: ReasonTaskNotCancelable, Domain: ErrorDomain,
			Metadata: map[string]string{"taskId": id, "state": string(state)}})
}

// TerminalTaskMessageError is -32004 for a message aimed at a task in a
// TERMINAL state, which §3.1.1 and §3.1.2 make mandatory ("Messages sent to
// Tasks that are in a terminal state ... cannot accept further messages").
// Refusing is the point: accepting it would create work under a task that is
// finished.
func TerminalTaskMessageError(id string, state TaskState) *RPCError {
	return NewRPCError(CodeUnsupportedOperationError, fmt.Sprintf(
		"message.taskId %q names a task in the terminal state %s: the A2A specification forbids further messages to a task in a terminal state (TASK_STATE_COMPLETED, TASK_STATE_FAILED, TASK_STATE_CANCELED, TASK_STATE_REJECTED) and this relay refuses rather than accepting a message it has nowhere to record",
		id, state),
		ErrorInfo{Type: ErrorInfoType, Reason: ReasonTaskTerminal, Domain: ErrorDomain,
			Metadata: map[string]string{"taskId": id, "state": string(state)}})
}

// TaskContinuationUnsupportedError is -32004 for a message that names a task
// which is still open. crier cannot continue it, and says so instead of
// delivering a second message the client would see as a new task: a task IS an
// inbox entry (one message, one task) and crier has no primitive that appends a
// message to an existing entry, so a "continuation" would either be a second
// task under a second id or an entry the receiving agent's ack would then
// remove wholesale.
func TaskContinuationUnsupportedError(id string, state TaskState) *RPCError {
	return NewRPCError(CodeUnsupportedOperationError, fmt.Sprintf(
		"message.taskId %q names the open task %s, and continuing an existing task is not supported by this build: in crier a task IS its inbox entry, and no primitive appends a message to an existing entry — the message would land as a separate task under a different id, which is not a continuation. Send the message without message.taskId to start a new task, or point at the earlier one with message.referenceTaskIds",
		id, state),
		ErrorInfo{Type: ErrorInfoType, Reason: ReasonTaskContinuationUnsupported, Domain: ErrorDomain,
			Metadata: map[string]string{"taskId": id, "state": string(state)}})
}

// SubscribeToTerminalTaskError is -32004 for a SubscribeToTask aimed at a task
// that has already reached a terminal state (§9.4.6: "Returns
// UnsupportedOperationError if the task is in a terminal state"): there are no
// updates left to stream, and opening a stream that can only close would tell
// the client it is watching something that is still running.
func SubscribeToTerminalTaskError(id string, state TaskState) *RPCError {
	return NewRPCError(CodeUnsupportedOperationError, fmt.Sprintf(
		"task %q is in the terminal state %s: there are no updates left to stream. Read it with GetTask instead — a finished task has no further status changes to report",
		id, state),
		ErrorInfo{Type: ErrorInfoType, Reason: ReasonTaskTerminal, Domain: ErrorDomain,
			Metadata: map[string]string{"taskId": id, "state": string(state)}})
}

// TaskCapabilityUnsupportedError is -32004 for a task-lifecycle operation this
// relay's inbox backend cannot answer — a backend that exposes neither a
// read-only view of an entry nor a close, such as the remote proxy, which keeps
// no inbox of its own. The operation is genuinely unsupported here, so it is
// refused rather than approximated: an approximated state would be invented.
func TaskCapabilityUnsupportedError(operation, capability string) *RPCError {
	return NewRPCError(CodeUnsupportedOperationError, fmt.Sprintf(
		"%s is not supported by this relay's inbox backend: it does not implement %s, and this operation cannot be answered from a state the relay cannot read",
		operation, capability),
		ErrorInfo{Type: ErrorInfoType, Reason: ReasonBackendCapabilityUnsupported, Domain: ErrorDomain,
			Metadata: map[string]string{"operation": operation, "capability": capability}})
}

// ErrorInfo reasons owned by the task lifecycle (INT-A2A-004).
const (
	ReasonTaskNotFound                 = "TASK_NOT_FOUND"
	ReasonTaskNotCancelable            = "TASK_NOT_CANCELABLE"
	ReasonTaskTerminal                 = "TASK_TERMINAL"
	ReasonTaskContinuationUnsupported  = "TASK_CONTINUATION_UNSUPPORTED"
	ReasonBackendCapabilityUnsupported = "BACKEND_CAPABILITY_UNSUPPORTED"
)
