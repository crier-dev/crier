package webhook

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// notifyRecorder captures FailureNotification callbacks (DF-CRIER-8).
type notifyRecorder struct {
	mu    sync.Mutex
	calls []FailureNotification
	err   error // when set, the sink fails (best-effort path)
}

func (r *notifyRecorder) notify(n FailureNotification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, n)
	return r.err
}

func (r *notifyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *notifyRecorder) first() FailureNotification {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return FailureNotification{}
	}
	return r.calls[0]
}

// TestDriver_AsyncRetryExhaustion_NotifiesSenderOnce: an async (fire-and-
// forget) delivery against an always-500 endpoint is accepted immediately,
// emits NO failure notification before bounded retry exhaustion, and after
// exhaustion emits EXACTLY ONE notification carrying the original message
// id, target agent, sender, final status and retry count (DF-CRIER-8).
func TestDriver_AsyncRetryExhaustion_NotifiesSenderOnce(t *testing.T) {
	cs := newCaptureServer(t, 500)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 2, RedeliverEvery: 30 * time.Millisecond, ProbeEvery: time.Hour,
		CircuitThreshold: 100, // keep the circuit closed so retries exhaust instead
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return testConfig(cs.server.URL), nil })
	rec := &notifyRecorder{}
	d.SetFailureNotifier(rec.notify)

	cfg := testConfig(cs.server.URL)
	env := &Envelope{Crier: EnvelopeMeta{
		Version: 1, MessageID: "msg-exhaust-1", Kind: "message", Sender: "agent-a",
	}}
	delivered, err := d.Deliver("agent-b", cfg, env)
	if err != nil || delivered {
		t.Fatalf("async deliver: delivered=%v err=%v (want accepted, no err)", delivered, err)
	}

	// Before exhaustion: attempts happen but no notification is emitted.
	waitFor(t, "first delivery attempt", func() bool { return cs.messageCount() >= 1 })
	if rec.count() != 0 {
		t.Fatalf("notification emitted before retry exhaustion: %d", rec.count())
	}

	// After bounded exhaustion: exactly one notification, well-formed.
	waitFor(t, "failure notification after exhaustion", func() bool { return rec.count() >= 1 })
	waitFor(t, "queue drained (dropped, not requeued)", func() bool { return d.queue.Len() == 0 })
	n := rec.first()
	if n.MessageID != "msg-exhaust-1" {
		t.Errorf("MessageID = %q, want msg-exhaust-1", n.MessageID)
	}
	if n.TargetAgent != "agent-b" {
		t.Errorf("TargetAgent = %q, want agent-b", n.TargetAgent)
	}
	if n.Sender != "agent-a" {
		t.Errorf("Sender = %q, want agent-a", n.Sender)
	}
	if n.Retries != 3 { // MaxRetries=2 → dropped on the 3rd failed attempt
		t.Errorf("Retries = %d, want 3 (MaxRetries+1)", n.Retries)
	}
	if n.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", n.StatusCode)
	}

	// Settle: the dropped item must never produce a duplicate notification.
	time.Sleep(200 * time.Millisecond)
	if got := rec.count(); got != 1 {
		t.Fatalf("notifications = %d, want exactly 1", got)
	}
}

// TestDriver_AsyncRetryExhaustion_TransportError: a transport-level failure
// (unreachable endpoint) reports the error string when no status exists.
func TestDriver_AsyncRetryExhaustion_TransportError(t *testing.T) {
	cs := newCaptureServer(t, 500)
	badURL := cs.server.URL
	cs.server.Close() // connections now refused → Err path, no status code
	d := NewDriver(NewClient(300*time.Millisecond, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 1, RedeliverEvery: 30 * time.Millisecond, ProbeEvery: time.Hour,
		CircuitThreshold: 100,
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return testConfig(badURL), nil })
	rec := &notifyRecorder{}
	d.SetFailureNotifier(rec.notify)

	_, err := d.Deliver("agent-b", testConfig(badURL), &Envelope{Crier: EnvelopeMeta{
		Version: 1, MessageID: "msg-transport-1", Kind: "message", Sender: "agent-a",
	}})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	waitFor(t, "failure notification", func() bool { return rec.count() >= 1 })
	n := rec.first()
	if n.Err == "" {
		t.Error("transport failure notification carries no error string")
	}
	if n.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 on transport error", n.StatusCode)
	}
	if n.Retries != 2 {
		t.Errorf("Retries = %d, want 2 (MaxRetries+1)", n.Retries)
	}
}

// TestDriver_AsyncRetryExhaustion_NoSenderIsBestEffort: an exhausted item
// without a sender is logged and dropped — no notification, no requeue, no
// panic (DF-CRIER-8 negative path).
func TestDriver_AsyncRetryExhaustion_NoSenderIsBestEffort(t *testing.T) {
	cs := newCaptureServer(t, 500)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 1, RedeliverEvery: 30 * time.Millisecond, ProbeEvery: time.Hour,
		CircuitThreshold: 100,
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return testConfig(cs.server.URL), nil })
	rec := &notifyRecorder{}
	d.SetFailureNotifier(rec.notify)

	_, err := d.Deliver("agent-b", testConfig(cs.server.URL), &Envelope{Crier: EnvelopeMeta{
		Version: 1, MessageID: "msg-nosender-1", Kind: "message", // Sender empty
	}})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	waitFor(t, "queue drained (dropped)", func() bool {
		return d.queue.Len() == 0 && cs.messageCount() >= 2
	})
	time.Sleep(100 * time.Millisecond)
	if got := rec.count(); got != 0 {
		t.Fatalf("notifications = %d, want 0 for sender-less item", got)
	}
}

// TestDriver_AsyncRetryExhaustion_SinkFailureIsBestEffort: when the
// notification sink fails, the failure is logged, the item is NOT requeued,
// and the sink is not retried (no duplicates) (DF-CRIER-8 negative path).
func TestDriver_AsyncRetryExhaustion_SinkFailureIsBestEffort(t *testing.T) {
	cs := newCaptureServer(t, 500)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 1, RedeliverEvery: 30 * time.Millisecond, ProbeEvery: time.Hour,
		CircuitThreshold: 100,
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return testConfig(cs.server.URL), nil })
	rec := &notifyRecorder{err: errors.New("inbox write failed")}
	d.SetFailureNotifier(rec.notify)

	_, err := d.Deliver("agent-b", testConfig(cs.server.URL), &Envelope{Crier: EnvelopeMeta{
		Version: 1, MessageID: "msg-sinkfail-1", Kind: "message", Sender: "agent-a",
	}})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	waitFor(t, "one sink call", func() bool { return rec.count() >= 1 })
	waitFor(t, "queue drained (not requeued)", func() bool { return d.queue.Len() == 0 })
	time.Sleep(150 * time.Millisecond)
	if got := rec.count(); got != 1 {
		t.Fatalf("sink calls = %d, want exactly 1 (no retry of the notification itself)", got)
	}
}

// TestFailureNotification_WireShape: the notification marshals to a stable
// machine-readable shape with the WEBHOOK_FAILED code (DF-CRIER-8).
func TestFailureNotification_WireShape(t *testing.T) {
	if CodeWebhookFailed != "WEBHOOK_FAILED" {
		t.Fatalf("CodeWebhookFailed = %q", CodeWebhookFailed)
	}
	n := FailureNotification{
		MessageID: "m1", Sender: "agent-a", TargetAgent: "agent-b",
		Retries: 3, StatusCode: 500,
	}
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}
	var round FailureNotification
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatalf("notification round-trip: %v", err)
	}
	if round != n {
		t.Fatalf("round-trip mismatch: %+v vs %+v", round, n)
	}
}
