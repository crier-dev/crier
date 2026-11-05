package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crier-dev/crier/internal/a2a"
)

// SendRequest is one outbound `message/send`.
//
// MessageID IS the idempotency key: crier's binding maps it onto the delivery's
// `idempotency_key` (§5.4.2), and its replay window is the deployment's
// (§5.4.1a). That is why it is a field a caller can PIN: a caller that retries
// a send in its own loop passes the same MessageID and the peer answers with
// the FIRST task, not a second one. When it is empty this client mints one and
// reports it on the result, and the client's own transport retry reuses it.
type SendRequest struct {
	// MessageID is the idempotency key. Empty means "mint one".
	MessageID string
	// ContextID is the conversation this belongs to (`contextId` ⇄ crier's
	// session_id, §5.4.2).
	ContextID string
	// Text is the shorthand for a single text part. Ignored when Parts is set.
	Text string
	// Parts are the message's parts, in internal/a2a's own model.
	Parts []a2a.Part
	// Metadata is carried verbatim (§5.4.2: never interpreted by the peer's
	// projection).
	Metadata map[string]any
	// HistoryLength is §3.2.4's `configuration.historyLength`: nil omits it,
	// 0 asks for no history on the returned Task.
	HistoryLength *int
	// ReturnImmediately sets §3.2.2's `configuration.returnImmediately`, which
	// in crier's binding selects the async delivery override (§5.4.6).
	ReturnImmediately bool
}

// SendResult is what one send established, in the terms the caller needs to
// write down: which messageId it used (so a retry is possible and provably the
// same task), which task the PEER minted, and which card it addressed.
type SendResult struct {
	// MessageID is the idempotency key the request carried — minted here when
	// the request left it empty. A retry reusing this value cannot create a
	// second task on the peer.
	MessageID string
	// TaskID is the peer's own task id (crier's inbox entry id, for a crier
	// peer). Empty when the peer answered with a direct Message instead — a
	// simple interaction has no task to track (§3.1.1, §5.4.4).
	TaskID string
	// CardDigest is the "sha256:<hex>" digest of the card bytes this send
	// addressed.
	CardDigest string
	// Endpoint is the JSON-RPC URL the request went to.
	Endpoint string
	// Tenant is `params.tenant` as sent.
	Tenant string
	// Method is the wire method name this dialect used.
	Method string
	// Attempts is how many HTTP attempts the send made (1 = no retry).
	Attempts int
	// Replayed reports that the peer served this messageId from its own
	// idempotency store instead of creating a new task.
	Replayed bool
	// Task and Message are the peer's answer, in internal/a2a's own types.
	Task    *a2a.Task
	Message *a2a.Message
}

// TaskResult is one task-lifecycle answer (GetTask or CancelTask).
type TaskResult struct {
	Task       *a2a.Task
	TaskID     string
	CardDigest string
	Endpoint   string
	Tenant     string
	Method     string
}

// Send performs `message/send` against card's endpoint.
//
// Retry rule: only a TRANSPORT failure (PEER_UNREACHABLE) is retried, at most
// MaxAttempts times, and every attempt carries the same messageId — which is
// exactly what makes the retry safe. A 401, an invalid response and a peer's
// JSON-RPC refusal are answered at once: they are not conditions a second
// identical request changes, and retrying them is a storm.
func (c *Client) Send(ctx context.Context, card *Card, req SendRequest) (*SendResult, error) {
	if card == nil {
		return nil, newError("send", NameConfigInvalid, "send needs a discovered card", nil)
	}
	messageID := strings.TrimSpace(req.MessageID)
	if messageID == "" {
		messageID = newID("crier-a2a-msg")
	}
	if len(messageID) > maxMessageIDBytes {
		return nil, newError("send", NameConfigInvalid, fmt.Sprintf(
			"messageId %q is %d bytes, over the %d-byte limit the peer enforces on its idempotency key",
			messageID, len(messageID), maxMessageIDBytes), nil)
	}

	parts := req.Parts
	if len(parts) == 0 {
		if strings.TrimSpace(req.Text) == "" {
			return nil, newError("send", NameConfigInvalid, "a message needs at least one part (or some text)", nil)
		}
		parts = []a2a.Part{a2a.TextPart(req.Text)}
	}

	msg := a2a.Message{
		MessageID: messageID,
		ContextID: strings.TrimSpace(req.ContextID),
		Role:      a2a.RoleUser,
		Parts:     parts,
		Metadata:  req.Metadata,
	}
	params := a2a.SendMessageParams{Tenant: card.Tenant, Message: &msg}
	if req.HistoryLength != nil || req.ReturnImmediately {
		params.Configuration = &a2a.SendMessageConfiguration{
			HistoryLength:     req.HistoryLength,
			ReturnImmediately: req.ReturnImmediately,
		}
	}

	result := &SendResult{
		MessageID:  messageID,
		CardDigest: card.Digest,
		Endpoint:   card.Endpoint,
		Tenant:     card.Tenant,
		Method:     c.methods.SendMessage,
	}

	raw, attempts, err := c.callWithRetry(ctx, "send", card, c.methods.SendMessage, params)
	result.Attempts = attempts
	if err != nil {
		// The messageId travels with the failure: a caller that retries after
		// an error must reuse it, or it really does create a second task.
		var e *Error
		if errors.As(err, &e) && e.Detail == "" {
			e.Detail = "messageId " + messageID
		}
		return nil, err
	}

	var answer a2a.SendMessageResponse
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, newError("send", NameInvalidResponse,
			"the result is not a SendMessage result (task or message): "+err.Error(), err)
	}
	switch {
	case answer.Task != nil:
		if strings.TrimSpace(answer.Task.ID) == "" {
			return nil, newError("send", NameInvalidResponse, "the peer returned a task with no id", nil)
		}
		result.Task = answer.Task
		result.TaskID = answer.Task.ID
		result.Replayed = replayMarked(answer.Task)
	case answer.Message != nil:
		if strings.TrimSpace(answer.Message.MessageID) == "" {
			return nil, newError("send", NameInvalidResponse, "the peer returned a message with no messageId", nil)
		}
		result.Message = answer.Message
		result.TaskID = strings.TrimSpace(answer.Message.TaskID)
	default:
		return nil, newError("send", NameInvalidResponse,
			"the result carries neither a task nor a message (§9.4.1's oneof)", nil)
	}
	return result, nil
}

// GetTask performs `tasks/get` against card's endpoint (§9.4.3).
//
// historyLength is §3.2.4's optional bound on the returned Task's history: nil
// omits the member entirely.
func (c *Client) GetTask(ctx context.Context, card *Card, taskID string, historyLength *int) (*TaskResult, error) {
	if card == nil {
		return nil, newError("getTask", NameConfigInvalid, "getTask needs a discovered card", nil)
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, newError("getTask", NameConfigInvalid, "a task id is required", nil)
	}
	params := a2a.GetTaskParams{Tenant: card.Tenant, ID: taskID, HistoryLength: historyLength}
	raw, err := c.post(ctx, "getTask", card, c.methods.GetTask, params)
	if err != nil {
		return nil, err
	}
	task, err := decodeTask("getTask", raw)
	if err != nil {
		return nil, err
	}
	return &TaskResult{
		Task:       task,
		TaskID:     task.ID,
		CardDigest: card.Digest,
		Endpoint:   card.Endpoint,
		Tenant:     card.Tenant,
		Method:     c.methods.GetTask,
	}, nil
}

// CancelTask performs `tasks/cancel` against card's endpoint (§9.4.5) and
// returns the peer's CANCELED task.
//
// No automatic retry: a cancel is not a request to re-send. The transport may
// have carried the first one, in which case the peer's second answer is a
// not-found for a task it already closed (§5.5.4) — reporting that as a
// refusal would be worse than reporting the transport failure that actually
// happened.
func (c *Client) CancelTask(ctx context.Context, card *Card, taskID string) (*TaskResult, error) {
	if card == nil {
		return nil, newError("cancelTask", NameConfigInvalid, "cancelTask needs a discovered card", nil)
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, newError("cancelTask", NameConfigInvalid, "a task id is required", nil)
	}
	params := a2a.CancelTaskParams{Tenant: card.Tenant, ID: taskID}
	raw, err := c.post(ctx, "cancelTask", card, c.methods.CancelTask, params)
	if err != nil {
		return nil, err
	}
	task, err := decodeTask("cancelTask", raw)
	if err != nil {
		return nil, err
	}
	return &TaskResult{
		Task:       task,
		TaskID:     task.ID,
		CardDigest: card.Digest,
		Endpoint:   card.Endpoint,
		Tenant:     card.Tenant,
		Method:     c.methods.CancelTask,
	}, nil
}

// callWithRetry posts one operation, retrying only a transport failure and
// reusing the identical params (and therefore the identical messageId) on
// every attempt. It returns the number of attempts made so the result can state
// whether a retry happened.
func (c *Client) callWithRetry(ctx context.Context, op string, card *Card, method string, params any) (json.RawMessage, int, error) {
	attempts := 0
	var lastErr error
	for attempt := 1; attempt <= c.attempts; attempt++ {
		attempts = attempt
		raw, err := c.post(ctx, op, card, method, params)
		if err == nil {
			return raw, attempts, nil
		}
		if !errors.Is(err, ErrPeerUnreachable) {
			return nil, attempts, err
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, attempts, lastErr
}

// decodeTask decodes a Task result and refuses one without an id.
func decodeTask(op string, raw json.RawMessage) (*a2a.Task, error) {
	var task a2a.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		return nil, newError(op, NameInvalidResponse, "the result is not a task: "+err.Error(), err)
	}
	if strings.TrimSpace(task.ID) == "" {
		return nil, newError(op, NameInvalidResponse, "the peer returned a task with no id", nil)
	}
	return &task, nil
}

// replayMarked reports whether the peer's own metadata says it served this
// message from its idempotency store: the boolean marker §5.4.1a documents
// (`idempotent_replay`) or the idempotency object's `replay: "replayed"`. Both
// are the peer's statements; this client does not infer one.
func replayMarked(task *a2a.Task) bool {
	if task == nil {
		return false
	}
	crier, ok := task.Metadata["crier"].(map[string]any)
	if !ok {
		return false
	}
	if replay, ok := crier["idempotent_replay"].(bool); ok && replay {
		return true
	}
	if info, ok := crier["idempotency"].(map[string]any); ok {
		if state, ok := info["replay"].(string); ok && state == "replayed" {
			return true
		}
	}
	return false
}
