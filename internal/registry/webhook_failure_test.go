package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/webhook"
)

// TestWebhookFailureSink_DeliversDurableNotification: the sink wires a
// driver's exhaustion callback to the Store's direct inbox Deliver path —
// the WEBHOOK_FAILED notification lands in the originating sender's inbox
// as a durable, machine-readable entry (DF-CRIER-8). It bypasses webhook
// routing entirely, so it cannot recurse.
func TestWebhookFailureSink_DeliversDurableNotification(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	store := NewMemoryStore()
	sink := WebhookFailureSink(store)
	err := sink(webhook.FailureNotification{
		MessageID: "m3", Sender: "ghost", TargetAgent: "agent-b", Retries: 3, StatusCode: 500,
	})
	if err == nil {
		t.Fatal("sink must report an error for an unregistered sender")
	}
}

// DF-CRIER-320 (1): an exhausted delivery with NO sender still produces
// exactly ONE durable WEBHOOK_FAILED entry — written into the TARGET agent's
// own inbox, because the operator whose endpoint failed is the one who must
// learn about it — carrying the additive no_sender marker.
func TestWebhookFailureSink_NoSenderDeliversToTargetInbox_DFCRIER320(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: "agent-b"}); err != nil {
		t.Fatalf("register target: %v", err)
	}
	sink := WebhookFailureSink(store)
	if err := sink(webhook.FailureNotification{
		MessageID:   "msg-nosender-1",
		Sender:      "", // the documented default for fire-and-forget callers
		TargetAgent: "agent-b",
		Retries:     6,
		StatusCode:  500,
	}); err != nil {
		t.Fatalf("sink: %v", err)
	}

	msgs, _, err := store.Retrieve("agent-b", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve target inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("target inbox messages = %d, want exactly 1", len(msgs))
	}
	entry := msgs[0]
	if entry.AgentID != "agent-b" {
		t.Errorf("entry.AgentID = %q, want agent-b (the target's own durable inbox)", entry.AgentID)
	}
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("payload not machine-readable JSON: %v", err)
	}
	checks := map[string]any{
		"kind":        "error",
		"code":        webhook.CodeWebhookFailed,
		"message_id":  "msg-nosender-1",
		"target":      "agent-b",
		"retries":     float64(6),
		"status_code": float64(500),
		"no_sender":   true,
	}
	for k, want := range checks {
		if got := payload[k]; got != want {
			t.Errorf("payload[%q] = %v, want %v (full payload: %s)", k, got, want, entry.Payload)
		}
	}

	// Settle: exactly one write, never a duplicate.
	again, _, err := store.Retrieve("agent-b", time.Minute, 10)
	if err != nil {
		t.Fatalf("re-retrieve target inbox: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("duplicate notification written into the target inbox: %d extra entr(ies)", len(again))
	}
}

// DF-CRIER-320 (2): the sender-present path is unchanged — the notification
// goes to the SENDER's inbox only, and the payload carries no no_sender
// marker (it stays byte-compatible with the pre-DF-CRIER-320 shape).
func TestWebhookFailureSink_SenderPresentUnchanged_DFCRIER320(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	for _, id := range []string{"agent-a", "agent-b"} {
		if err := store.Register(&Agent{ID: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	sink := WebhookFailureSink(store)
	if err := sink(webhook.FailureNotification{
		MessageID: "msg-sender-1", Sender: "agent-a", TargetAgent: "agent-b",
		Retries: 4, StatusCode: 500,
	}); err != nil {
		t.Fatalf("sink: %v", err)
	}

	senderMsgs, _, err := store.Retrieve("agent-a", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve sender inbox: %v", err)
	}
	if len(senderMsgs) != 1 {
		t.Fatalf("sender inbox messages = %d, want exactly 1", len(senderMsgs))
	}
	var payload map[string]any
	if err := json.Unmarshal(senderMsgs[0].Payload, &payload); err != nil {
		t.Fatalf("payload not machine-readable JSON: %v", err)
	}
	if payload["target"] != "agent-b" {
		t.Errorf("payload[target] = %v, want agent-b", payload["target"])
	}
	if _, has := payload["no_sender"]; has {
		t.Errorf("sender notification must not carry the no_sender marker: %s", senderMsgs[0].Payload)
	}

	targetMsgs, _, err := store.Retrieve("agent-b", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve target inbox: %v", err)
	}
	if len(targetMsgs) != 0 {
		t.Fatalf("target inbox messages = %d, want 0 (no duplicate into the target)", len(targetMsgs))
	}
}

// DF-CRIER-320 (3): a no-sender notification whose fallback destination does
// not exist (unregistered target, or an empty target) returns an error for
// the driver to log best-effort — no panic, no write, no requeue.
func TestWebhookFailureSink_NoSenderUnregisteredTargetErrors_DFCRIER320(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
	}{
		{"unregistered target", "ghost"},
		{"empty target", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			sink := WebhookFailureSink(store)
			err := sink(webhook.FailureNotification{
				MessageID: "m-320", Sender: "", TargetAgent: tc.target,
				Retries: 2, StatusCode: 500,
			})
			if err == nil {
				t.Fatal("sink must report an error when there is no sender and no deliverable target")
			}
			if msgs, _, rerr := store.Retrieve("ghost", time.Minute, 10); rerr == nil && len(msgs) != 0 {
				t.Fatalf("nothing must be written on the failed path, got %d message(s)", len(msgs))
			}
		})
	}
}

// DF-CRIER-320 (end-to-end): a real webhook driver that exhausts a
// sender-less async delivery against an always-500 endpoint writes exactly
// one durable WEBHOOK_FAILED entry into the target agent's own inbox — the
// silent drop is gone. Exhaustion accounting is unchanged (one POST per
// attempt, drop after the budget).
func TestWebhookFailureSink_ExhaustedNoSenderLandsInTargetInbox_DFCRIER320(t *testing.T) {
	t.Parallel()
	posts := make(chan struct{}, 64)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer endpoint.Close()

	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: "agent-b"}); err != nil {
		t.Fatalf("register target: %v", err)
	}

	d := webhook.NewDriver(webhook.NewClient(2*time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{
		MaxRetries: 1, RedeliverEvery: 30 * time.Millisecond, ProbeEvery: time.Hour,
		CircuitThreshold: 100, // keep the circuit closed so retries exhaust instead
	})
	cfg := &webhook.Config{URL: endpoint.URL, DeliveryMode: "async", TimeoutMs: 5000}
	d.SetConfigResolver(func(string) (*webhook.Config, error) { return cfg, nil })
	d.SetFailureNotifier(WebhookFailureSink(store))
	d.Start()
	defer d.Stop()

	if _, err := d.Deliver("agent-b", cfg, &webhook.Envelope{Crier: webhook.EnvelopeMeta{
		Version: 1, MessageID: "msg-320-e2e", Kind: "message", // Sender deliberately absent
	}}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	var msgs []*InboxEntry
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _, err := store.Retrieve("agent-b", time.Minute, 10)
		if err != nil {
			t.Fatalf("retrieve target inbox: %v", err)
		}
		if len(got) > 0 {
			msgs = got
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(msgs) != 1 {
		t.Fatalf("target inbox entries = %d, want exactly 1 (the silent drop is the bug)", len(msgs))
	}
	var payload map[string]any
	if err := json.Unmarshal(msgs[0].Payload, &payload); err != nil {
		t.Fatalf("payload not machine-readable JSON: %v", err)
	}
	checks := map[string]any{
		"kind":        "error",
		"code":        webhook.CodeWebhookFailed,
		"message_id":  "msg-320-e2e",
		"target":      "agent-b",
		"retries":     float64(2), // MaxRetries=1 → dropped on the 2nd failed attempt
		"status_code": float64(500),
		"no_sender":   true,
	}
	for k, want := range checks {
		if got := payload[k]; got != want {
			t.Errorf("payload[%q] = %v, want %v (full payload: %s)", k, got, want, msgs[0].Payload)
		}
	}
	// Exhaustion accounting unchanged: MaxRetries=1 → exactly 2 POSTs.
	attempts := 0
	for len(posts) > 0 {
		<-posts
		attempts++
	}
	if attempts != 2 {
		t.Errorf("POST attempts = %d, want 2 (MaxRetries=1 → 1 initial + 1 retry)", attempts)
	}

	// Settle: the dropped item must never produce a second entry.
	time.Sleep(250 * time.Millisecond)
	again, _, err := store.Retrieve("agent-b", time.Minute, 10)
	if err != nil {
		t.Fatalf("re-retrieve target inbox: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("duplicate notification written into the target inbox: %d extra entr(ies)", len(again))
	}
}
