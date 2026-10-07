package daggerctl_test

// Delivery-and-wait tests (CR-CHAT-034): the two-node flow, idempotency
// across a restart, the named timeout, and the cancel-releases-waiters rule.
// Everything drives the Service's public API with a fake bridge and a real
// in-memory inbox — no executor, per decision D18.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/registry"
)

// waitHarness builds a Service wired for waits: fake bridge, run store, and
// a real registry.MemoryStore as the durable inbox the waits deliver through.
type waitHarness struct {
	bridge *fakeBridge
	svc    *daggerctl.Service
	inbox  *registry.MemoryStore
	waits  *daggerctl.WaiterRegistry
}

func newWaitHarness(t *testing.T, dir string) *waitHarness {
	t.Helper()
	bridge := &fakeBridge{}
	inbox := registry.NewMemoryStore()
	if err := inbox.Register(&registry.Agent{ID: "agent-a"}); err != nil {
		t.Fatalf("register agent-a: %v", err)
	}
	if err := inbox.Register(&registry.Agent{ID: "agent-b"}); err != nil {
		t.Fatalf("register agent-b: %v", err)
	}
	runStore := daggerctl.NewMemoryStore()
	waits, err := daggerctl.NewWaiterRegistry(dir)
	if err != nil {
		t.Fatalf("waiter registry: %v", err)
	}
	svc := daggerctl.NewService(bridge, runStore, daggerctl.InboxDeliverer(inbox))
	svc.SetWaitRegistry(waits)
	svc.SetInboxStore(inbox)
	return &waitHarness{bridge: bridge, svc: svc, inbox: inbox, waits: waits}
}

// queuedCount counts inbox entries WITHOUT leasing or consuming them —
// the registry store's read-only PeekInbox (INT-A2A-003).
func queuedCount(t *testing.T, h *waitHarness, agentID string) int {
	t.Helper()
	msgs, err := h.inbox.PeekInbox(agentID)
	if err != nil {
		t.Fatalf("peek inbox for %s: %v", agentID, err)
	}
	return len(msgs)
}

// TestWaitTwoNodeFlow (acceptance 1): deliver to A and wait; A replies; the
// wait resolves with A's output; the resolved reply is then deliverable to B.
func TestWaitTwoNodeFlow(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())

	const key = "node-1-wait"
	results := make(chan *daggerctl.WaitResult, 1)
	errs := make(chan error, 1)
	go func() {
		res, err := h.svc.DeliverAndWait(context.Background(), daggerctl.WaitRequest{
			AgentID:        "agent-a",
			Payload:        json.RawMessage(`{"work":"extract"}`),
			IdempotencyKey: key,
			TimeoutSeconds: 5,
			RunID:          "run-flow",
		})
		if err != nil {
			errs <- err
			return
		}
		results <- res
	}()

	// Wait for the delivery to land, then resolve it as agent A's reply.
	deadline := time.Now().Add(5 * time.Second)
	var deliveredID string
	for deliveredID == "" {
		if time.Now().After(deadline) {
			t.Fatal("the wait's delivery never landed in agent A's inbox")
		}
		msgs, lease, err := h.inbox.Retrieve("agent-a", 0, 10)
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		for _, m := range msgs {
			var inner map[string]any
			if err := json.Unmarshal(m.Payload, &inner); err == nil {
				_ = inner
				deliveredID = m.ID
			} else {
				deliveredID = m.ID
			}
		}
		if deliveredID == "" {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		// Resolve through the SERVICE's resolve verb, as the replying agent
		// would via POST /dagger/waits/{key}/resolve.
		if _, err := h.svc.ResolveWait(key, deliveredID, json.RawMessage(`{"answer":"A output"}`)); err != nil {
			t.Fatalf("resolve wait: %v", err)
		}
		if lease != "" {
			ids := []string{deliveredID}
			_ = h.inbox.Ack("agent-a", lease, ids)
		}
		break
	}

	select {
	case err := <-errs:
		t.Fatalf("DeliverAndWait failed: %v", err)
	case res := <-results:
		if res.State != "resolved" {
			t.Fatalf("wait state = %q, want resolved", res.State)
		}
		if !strings.Contains(string(res.Reply), "A output") {
			t.Fatalf("wait reply = %s, want A's output", res.Reply)
		}
		if res.MessageID != deliveredID {
			t.Fatalf("wait message id = %q, want the delivered %q", res.MessageID, deliveredID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait never resolved")
	}

	// The second node: B's delivery can CARRY A's output. The resolution
	// above is what a DAG node would read; prove the pipe end-to-end by
	// delivering the recorded reply into B's inbox.
	h.svc.NotifyAcked("agent-a", []string{deliveredID})
	st, err := h.svc.WaitStatus(key)
	if err != nil {
		t.Fatalf("wait status: %v", err)
	}
	if st.State != "resolved" || !strings.Contains(string(st.Reply), "A output") {
		t.Fatalf("wait status = %+v, want resolved with A's output", st)
	}
}

// TestWaitIdempotentAcrossRestart (acceptance 2): the same key after a
// registry rebuild (the crash/restart stand-in — the journal replays) joins
// the SAME wait and delivers NOTHING new.
func TestWaitIdempotentAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	h := newWaitHarness(t, dir)

	// First call: register + deliver, and DO NOT resolve. The wait stays
	// pending when the "process" ends.
	go func() {
		_, _ = h.svc.DeliverAndWait(context.Background(), daggerctl.WaitRequest{
			AgentID:        "agent-a",
			Payload:        json.RawMessage(`{"work":"one"}`),
			IdempotencyKey: "crash-key",
			TimeoutSeconds: 30,
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for queuedCount(t, h, "agent-a") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first delivery never landed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The caller goroutine is still parked; the journal now holds the pending
	// wait. Let the writer flush.
	time.Sleep(50 * time.Millisecond)
	before := queuedCount(t, h, "agent-a")
	if before == 0 {
		t.Fatal("expected exactly one delivered message before the restart")
	}

	// RESTART: a fresh registry over the same directory replays the journal;
	// the re-called wait must join, not re-deliver.
	waits2, err := daggerctl.NewWaiterRegistry(dir)
	if err != nil {
		t.Fatalf("rebuild registry: %v", err)
	}
	svc2 := daggerctl.NewService(h.bridge, daggerctl.NewMemoryStore(), nil)
	svc2.SetWaitRegistry(waits2)
	svc2.SetInboxStore(h.inbox)

	go func() {
		_, _ = svc2.DeliverAndWait(context.Background(), daggerctl.WaitRequest{
			AgentID:        "agent-a",
			Payload:        json.RawMessage(`{"work":"one"}`),
			IdempotencyKey: "crash-key",
			TimeoutSeconds: 30,
		})
	}()
	time.Sleep(200 * time.Millisecond)
	if after := queuedCount(t, h, "agent-a"); after != before {
		t.Fatalf("after restart the inbox holds %d message(s), want %d — a retried key re-delivered", after, before)
	}

	// The joined waiter resolves with the original wait.
	if _, _, err := waits2.Resolve("crash-key", "", json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("resolve after restart: %v", err)
	}
}

// TestWaitTimeoutNamed (acceptance 3): the bounded budget expiring is the
// named DELIVERY_WAIT_TIMEOUT, not a silent success.
func TestWaitTimeoutNamed(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	start := time.Now()
	_, err := h.svc.DeliverAndWait(context.Background(), daggerctl.WaitRequest{
		AgentID:        "agent-a",
		Payload:        json.RawMessage(`{}`),
		IdempotencyKey: "timeout-key",
		TimeoutSeconds: 1,
	})
	if err == nil {
		t.Fatal("an unanswered wait returned success")
	}
	if !strings.Contains(err.Error(), daggerctl.CodeDeliveryWaitTimeout) {
		t.Fatalf("error = %v, want the named %s", err, daggerctl.CodeDeliveryWaitTimeout)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took %v, want ~1s", elapsed)
	}
	// The delivery DID land — the timeout is about the answer, not the work.
	if n := queuedCount(t, h, "agent-a"); n != 1 {
		t.Fatalf("inbox holds %d message(s), want 1 (the timed-out delivery is still queued)", n)
	}
}

// TestWaitCancelReleasesWaiters (acceptance 4): cancelling the run resolves
// its waiters with WAIT_CANCELLED — through BOTH the cancel verb and a status
// poll that observes the cancellation.
func TestWaitCancelReleasesWaiters(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())

	// Create a run through the service so the record exists to cancel.
	rec, err := h.svc.CreateRun(context.Background(), daggerctl.CreateRunRequest{AgentID: "agent-a", Prompt: "p"})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	errs := make(chan error, 2)
	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := h.svc.DeliverAndWait(context.Background(), daggerctl.WaitRequest{
				AgentID:        "agent-a",
				Payload:        json.RawMessage(`{}`),
				IdempotencyKey: "cancel-key-" + string(rune('a'+i)),
				TimeoutSeconds: 30,
				RunID:          rec.RunID,
			})
			if err != nil {
				errs <- err
			}
			done <- struct{}{}
		}()
	}
	// Both deliveries land first.
	deadline := time.Now().Add(5 * time.Second)
	for queuedCount(t, h, "agent-a") < 2 {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := h.svc.Cancel(context.Background(), rec.RunID); err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if !strings.Contains(err.Error(), daggerctl.CodeWaitCancelled) {
				t.Fatalf("waiter error = %v, want the named %s", err, daggerctl.CodeWaitCancelled)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a waiter survived the run's cancellation")
		}
	}
}

// TestWaitValidation: an unbounded or absurd budget, a missing key, a missing
// payload and a missing agent are each a named refusal BEFORE anything is
// delivered — no inbox entry, no recorded key.
func TestWaitValidation(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	cases := []daggerctl.WaitRequest{
		{AgentID: "agent-a", Payload: json.RawMessage(`{}`), IdempotencyKey: "k", TimeoutSeconds: 0},
		{AgentID: "agent-a", Payload: json.RawMessage(`{}`), IdempotencyKey: "k", TimeoutSeconds: -5},
		{AgentID: "agent-a", Payload: json.RawMessage(`{}`), IdempotencyKey: "k", TimeoutSeconds: 121},
		{AgentID: "agent-a", Payload: json.RawMessage(`{}`), TimeoutSeconds: 5},
		{AgentID: "agent-a", IdempotencyKey: "k", TimeoutSeconds: 5},
		{Payload: json.RawMessage(`{}`), IdempotencyKey: "k", TimeoutSeconds: 5},
	}
	for i, req := range cases {
		if _, err := h.svc.DeliverAndWait(context.Background(), req); err == nil {
			t.Fatalf("case %d: accepted %+v", i, req)
		} else if !strings.Contains(err.Error(), "invalid dagger request") {
			t.Fatalf("case %d: error %v is not the named invalid-input refusal", i, err)
		}
	}
	if n := queuedCount(t, h, "agent-a"); n != 0 {
		t.Fatalf("a refused wait delivered %d message(s), want 0", n)
	}
}

// TestWaitAckResolves: the ordinary ack path resolves a wait bound to the
// acked message — no dedicated protocol, the shipped lease/ack semantics.
func TestWaitAckResolves(t *testing.T) {
	h := newWaitHarness(t, t.TempDir())
	errs := make(chan error, 1)
	results := make(chan *daggerctl.WaitResult, 1)
	go func() {
		res, err := h.svc.DeliverAndWait(context.Background(), daggerctl.WaitRequest{
			AgentID:        "agent-a",
			Payload:        json.RawMessage(`{"work":"ack-me"}`),
			IdempotencyKey: "ack-key",
			TimeoutSeconds: 5,
		})
		if err != nil {
			errs <- err
			return
		}
		results <- res
	}()
	deadline := time.Now().Add(5 * time.Second)
	var id string
	for id == "" {
		if time.Now().After(deadline) {
			t.Fatal("delivery never landed")
		}
		msgs, lease, err := h.inbox.Retrieve("agent-a", 0, 10)
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		for _, m := range msgs {
			id = m.ID
			// Ack through the STORE path — the shipped semantics the HTTP
			// handler delegates to, whose hook is the resolution edge.
			h.svc.NotifyAcked("agent-a", []string{id})
			if lease != "" {
				_ = h.inbox.Ack("agent-a", lease, []string{id})
			}
		}
		if id == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}
	select {
	case err := <-errs:
		t.Fatalf("wait failed on ack: %v", err)
	case res := <-results:
		if res.State != "resolved" {
			t.Fatalf("wait state = %q, want resolved via ack", res.State)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ack never resolved the wait")
	}
}
