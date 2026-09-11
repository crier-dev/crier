package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/totalwindupflightsystems/crier/internal/webhook"
)

// TestWebhookFailureSink_DeliversDurableNotification: the sink wires a
// driver's exhaustion callback to the Store's direct inbox Deliver path —
// the WEBHOOK_FAILED notification lands in the originating sender's inbox
// as a durable, machine-readable entry (DF-CRIER-8). It bypasses webhook
// routing entirely, so it cannot recurse.
func TestWebhookFailureSink_DeliversDurableNotification(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: "agent-a"}); err != nil {
		t.Fatalf("register sender: %v", err)
	}

	sink := WebhookFailureSink(store)
	n := webhook.FailureNotification{
		MessageID:   "msg-orig-1",
		Sender:      "agent-a",
		TargetAgent: "agent-b",
		Retries:     6,
		StatusCode:  500,
	}
	if err := sink(n); err != nil {
		t.Fatalf("sink: %v", err)
	}

	msgs, _, err := store.Retrieve("agent-a", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("inbox messages = %d, want exactly 1", len(msgs))
	}
	entry := msgs[0]
	if entry.AgentID != "agent-a" {
		t.Errorf("entry.AgentID = %q, want agent-a (durable inbox of the sender)", entry.AgentID)
	}
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("payload not machine-readable JSON: %v", err)
	}
	checks := map[string]any{
		"kind":        "error",
		"code":        webhook.CodeWebhookFailed,
		"message_id":  "msg-orig-1",
		"target":      "agent-b",
		"status_code": float64(500),
		"retries":     float64(6),
	}
	for k, want := range checks {
		if got := payload[k]; got != want {
			t.Errorf("payload[%q] = %v, want %v (full payload: %s)", k, got, want, entry.Payload)
		}
	}
}

// TestWebhookFailureSink_TransportErrorCarriesErrString: a transport-level
// failure (no HTTP status) surfaces the error string instead.
func TestWebhookFailureSink_TransportErrorCarriesErrString(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: "agent-a"}); err != nil {
		t.Fatalf("register sender: %v", err)
	}
	sink := WebhookFailureSink(store)
	if err := sink(webhook.FailureNotification{
		MessageID: "m2", Sender: "agent-a", TargetAgent: "agent-b",
		Retries: 3, Err: "post http://x: connection refused",
	}); err != nil {
		t.Fatalf("sink: %v", err)
	}
	msgs, _, err := store.Retrieve("agent-a", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(msgs[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["error"] != "post http://x: connection refused" {
		t.Errorf("payload[error] = %v", payload["error"])
	}
	if _, hasStatus := payload["status_code"]; hasStatus {
		t.Errorf("status_code should be omitted on transport error: %s", msgs[0].Payload)
	}
}

// TestWebhookFailureSink_UnregisteredSenderErrors: an unknown/departed
// sender makes the sink return an error — the driver treats that as
// best-effort (logged, no requeue), so the error must surface here.
func TestWebhookFailureSink_UnregisteredSenderErrors(t *testing.T) {
	store := NewMemoryStore()
	sink := WebhookFailureSink(store)
	err := sink(webhook.FailureNotification{
		MessageID: "m3", Sender: "ghost", TargetAgent: "agent-b", Retries: 3, StatusCode: 500,
	})
	if err == nil {
		t.Fatal("sink must report an error for an unregistered sender")
	}
}
