package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/stretchr/testify/require"
)

func TestDeliveryFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		outcome   Outcome
		code      string
		retryable bool
		detail    string
	}{
		{"busy", &registry.DispatchError{Code: "DELIVERY_IN_PROGRESS", StatusCode: 409, Response: map[string]string{"error": "already in progress"}}, OutcomeInProgress, "DELIVERY_IN_PROGRESS", true, "already in progress"},
		{"other conflict is refusal", &registry.DispatchError{StatusCode: 409, Response: map[string]string{"error": "different payload for key"}}, OutcomeRefused, "DELIVERY_REFUSED", false, "different payload for key"},
		{"guard", &registry.DispatchError{StatusCode: 403, Response: map[string]any{"error": "GUARD_BLOCKED", "guard": map[string]string{"decision": "block"}}}, OutcomeRefused, "GUARD_BLOCKED", false, "GUARD_BLOCKED"},
		{"structured code", &registry.DispatchError{StatusCode: 403, Response: map[string]string{"code": "NAMESPACE_MISMATCH", "detail": "wrong realm"}}, OutcomeRefused, "NAMESPACE_MISMATCH", false, "wrong realm"},
		{"permanent target", &registry.DispatchError{StatusCode: 502, Cause: &webhook.BlockingDeliveryError{Attempts: 1, Budget: time.Second, TargetStatus: 403, Cause: webhook.ErrPermanent}}, OutcomeRefused, "TARGET_HTTP_403", false, "target answered status 403;"},
		{"unknown body", &registry.DispatchError{StatusCode: 500, Response: map[string]any{"other": true}}, OutcomeRefused, "DELIVERY_REFUSED", true, http.StatusText(500)},
		{"invalid JSON body", &registry.DispatchError{StatusCode: 500, Response: json.RawMessage(`invalid`)}, OutcomeRefused, "DELIVERY_REFUSED", true, http.StatusText(500)},
		{"not found", registry.ErrAgentNotFound, OutcomeRefused, "AGENT_NOT_FOUND", false, "no such agent"},
		{"transport", errors.New("connection refused"), OutcomeRefused, "DELIVERY_REFUSED", false, "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := deliveryFailure("receiver", tc.err)
			require.Equal(t, tc.outcome, o.Outcome)
			require.Equal(t, tc.code, o.Code)
			require.Equal(t, tc.retryable, o.Retryable)
			require.Contains(t, o.Reason, tc.detail)
			require.NotContains(t, o.Reason, `{"error"`)
		})
	}
}

func TestRestoreOutcomeMetadataPreservesLifecycle(t *testing.T) {
	at := time.Now().UTC()
	m := &Message{ID: "message", Outcomes: []DeliveryOutcome{{Target: "receiver", Outcome: OutcomeAcked, UpdatedAt: at}}}
	require.NoError(t, restoreMessageRequest([]*Message{m}, m.ID, "", `[{"target":"receiver","outcome":"in_progress","code":"DELIVERY_IN_PROGRESS","retryable":true,"reason":"already in progress"}]`))
	require.Equal(t, OutcomeAcked, m.Outcomes[0].Outcome)
	require.Equal(t, at, m.Outcomes[0].UpdatedAt)
	require.Empty(t, m.Outcomes[0].Code, "an acknowledgement must not inherit a stale in-progress code")
	require.False(t, m.Outcomes[0].Retryable)
}

// HTTP acceptance and a runtime FAILED reply are distinct; the new boolean
// must not silently say false for the runtime verdict that already allows retry.
type diagnosticReplyDeliverer struct {
	*registry.MemoryStore
	reply json.RawMessage
}

func (d diagnosticReplyDeliverer) DispatchDelivery(context.Context, string, registry.DeliveryRequest) (registry.DeliveryResult, error) {
	return registry.DeliveryResult{Transport: "webhook", DeliveryMode: "blocking", Reply: d.reply}, nil
}
func TestFanoutReplyDiagnosticRetryability(t *testing.T) {
	for _, tc := range []struct {
		reply, code string
		retryable   bool
	}{
		{`{"status":"FAILED","reason":"runtime failed"}`, "RUNTIME_FAILED", true},
		{`{"live":false,"reason":"demo only"}`, "RUNTIME_CANNED", false},
		{`{"result":{}}`, "UNREADABLE_REPLY", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			msg := &Message{ID: "msg", SessionID: "room", ThreadID: "msg", Payload: json.RawMessage(`{}`)}
			d := diagnosticReplyDeliverer{registry.NewMemoryStore(), json.RawMessage(tc.reply)}
			o, err := Fanout(context.Background(), d, msg, []string{"receiver"}, nil)
			require.NoError(t, err)
			require.Len(t, o, 1)
			require.Equal(t, tc.code, o[0].Code)
			require.Equal(t, tc.retryable, o[0].Retryable)
		})
	}
}
