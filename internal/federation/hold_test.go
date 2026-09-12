package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakyRelay is a controllable linked relay: it answers `status` until the
// test flips it, records every forwarded request, and can be told to fail
// the first N attempts.
type flakyRelay struct {
	t        *testing.T
	mu       sync.Mutex
	status   atomic.Int64
	failNext atomic.Int64
	calls    atomic.Int64
	bodies   [][]byte
	statuses []int
	hops     []string
	auths    []string
}

func newFlakyRelay(t *testing.T, status int) *flakyRelay {
	r := &flakyRelay{t: t}
	r.status.Store(int64(status))
	return r
}

func (r *flakyRelay) setStatus(status int) { r.status.Store(int64(status)) }

func (r *flakyRelay) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.calls.Add(1)
		body := make([]byte, req.ContentLength)
		if req.ContentLength > 0 {
			if _, err := req.Body.Read(body); err != nil && err.Error() != "EOF" {
				r.t.Errorf("read forwarded body: %v", err)
			}
		}
		status := int(r.status.Load())
		if r.failNext.Load() > 0 {
			r.failNext.Add(-1)
			status = http.StatusInternalServerError
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.statuses = append(r.statuses, status)
		r.hops = append(r.hops, req.Header.Get(HopHeader))
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write([]byte(`{"id":"remote-1","reply":{"text":"pong"}}`))
		}
	}
}

func (r *flakyRelay) callCount() int { return int(r.calls.Load()) }

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// testHoldConfig returns millisecond timings so no test waits real seconds.
func testHoldConfig(maxHold time.Duration) HoldConfig {
	return HoldConfig{
		MaxHold:          maxHold,
		RetryEvery:       time.Millisecond,
		MaxRetryInterval: 3 * time.Millisecond,
	}
}

// mustHold asserts the ForwardOrHold transient contract: the delivery was
// held AND the transient error is returned alongside it (err == nil means
// relayed, so a held delivery is never mistaken for a success).
func mustHold(t *testing.T, held *HoldItem, err error) {
	t.Helper()
	if held == nil {
		t.Fatalf("ForwardOrHold did not hold the delivery (err = %v)", err)
	}
	var transient *TransientError
	if !asTransient(err, &transient) {
		t.Fatalf("held delivery err = %v (%T), want *TransientError", err, err)
	}
}

// ---------- HoldQueue (memory) ----------

func TestMemoryHoldQueueCRUDAndBounds(t *testing.T) {
	q := NewMemoryHoldQueue()
	item := &HoldItem{ID: "m1", AgentID: "remote", Body: json.RawMessage(`{"payload":{}}`)}

	if err := q.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if q.Len() != 1 {
		t.Fatalf("Len = %d, want 1", q.Len())
	}
	// Lists are copies: mutating one must not touch the stored item.
	got := q.List()
	got[0].Attempts = 99
	got[0].Body[0] = 'X'
	if again := q.List(); again[0].Attempts != 0 || string(again[0].Body) != `{"payload":{}}` {
		t.Errorf("List did not return an isolated copy: %+v", again[0])
	}

	item.Attempts = 3
	item.LastError = "boom"
	if err := q.Update(item); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := q.List()[0]; got.Attempts != 3 || got.LastError != "boom" {
		t.Errorf("Update did not persist: %+v", got)
	}
	if err := q.Update(&HoldItem{ID: "absent", AgentID: "a", Body: json.RawMessage(`{}`)}); err == nil {
		t.Error("Update of an unknown item must fail")
	}
	if err := q.Remove("m1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := q.Remove("m1"); err != nil {
		t.Errorf("Remove of an unknown id must be a no-op, got %v", err)
	}
	if q.Len() != 0 {
		t.Errorf("Len = %d after Remove, want 0", q.Len())
	}
}

func TestHoldQueueRejectsUnusableItems(t *testing.T) {
	q := NewMemoryHoldQueue()
	cases := []struct {
		name string
		item *HoldItem
		want string
	}{
		{"no id", &HoldItem{AgentID: "a", Body: json.RawMessage(`{}`)}, "no message id"},
		{"no target", &HoldItem{ID: "m", Body: json.RawMessage(`{}`)}, "no target agent"},
		{"empty body", &HoldItem{ID: "m", AgentID: "a"}, "empty body"},
		{"non-json body", &HoldItem{ID: "m", AgentID: "a", Body: json.RawMessage(`nope`)}, "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := q.Enqueue(tc.item)
			if err == nil {
				t.Fatalf("Enqueue(%s) = nil, want error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
	if q.Len() != 0 {
		t.Errorf("rejected items must not be stored, Len = %d", q.Len())
	}

	t.Run("body cap", func(t *testing.T) {
		q := &MemoryHoldQueue{MaxBodyBytes: 8}
		if err := q.Enqueue(&HoldItem{ID: "big", AgentID: "a", Body: json.RawMessage(`{"payload":"way too big"}`)}); err == nil {
			t.Fatal("body above the cap must be refused")
		}
	})
	t.Run("item cap", func(t *testing.T) {
		q := &MemoryHoldQueue{MaxItems: 1}
		if err := q.Enqueue(&HoldItem{ID: "1", AgentID: "a", Body: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("first Enqueue: %v", err)
		}
		if err := q.Enqueue(&HoldItem{ID: "2", AgentID: "a", Body: json.RawMessage(`{}`)}); err == nil {
			t.Fatal("queue above MaxItems must be refused")
		}
	})
}

// ---------- ForwardOrHold classification ----------

func TestForwardOrHoldHoldsTransientOutage(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close() // unreachable link

	client := NewClient([]string{dead.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	client.SetHoldManager(NewHoldManager(client, queue, testHoldConfig(time.Minute)))

	body := []byte(`{"payload":{"text":"hi"},"sender":"agent-local","request_id":"req-1","session_id":"sess-1"}`)
	status, respBody, held, err := client.ForwardOrHold(context.Background(), "agent-remote", body, HoldMeta{
		MessageID: "msg-7", Sender: "agent-local", RequestID: "req-1", SessionID: "sess-1",
	})
	// Contract: only err == nil means "relayed"; a held delivery still
	// reports the transient error alongside the held item.
	if held == nil {
		t.Fatalf("a transient outage must hold the delivery, not fail it (err = %v)", err)
	}
	var transient *TransientError
	if !asTransient(err, &transient) {
		t.Fatalf("err = %v (%T), want *TransientError alongside the held item", err, err)
	}
	if status != 0 || respBody != nil {
		t.Errorf("held delivery returned (%d, %s), want no relayed response", status, respBody)
	}
	if held.ID != "msg-7" || held.AgentID != "agent-remote" || held.Sender != "agent-local" ||
		held.RequestID != "req-1" || held.SessionID != "sess-1" {
		t.Errorf("held item lost correlation context: %+v", held)
	}
	if held.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (the pass that discovered the outage)", held.Attempts)
	}
	if string(held.Body) != string(body) {
		t.Errorf("held body = %s, want the original deliver JSON", held.Body)
	}
	if got := held.Deadline.Sub(held.EnqueuedAt); got != time.Minute {
		t.Errorf("hold window = %s, want the configured MaxHold", got)
	}
	if held.LastError == "" {
		t.Error("held item must record the last transport error")
	}
	if queue.Len() != 1 {
		t.Errorf("queue Len = %d, want 1", queue.Len())
	}
}

func TestForwardOrHoldDefinitiveAllLinks404IsNotHeld(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"agent not found"}`))
	}))
	defer notFound.Close()

	client := NewClient([]string{notFound.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	client.SetHoldManager(NewHoldManager(client, queue, testHoldConfig(time.Minute)))

	_, _, held, err := client.ForwardOrHold(context.Background(), "ghost", []byte(`{"payload":{}}`), HoldMeta{MessageID: "m"})
	if held != nil {
		t.Error("a definitive 404 must not be held")
	}
	if err == nil || !strings.Contains(err.Error(), ErrNotFoundOnAnyLink.Error()) {
		t.Fatalf("err = %v, want ErrNotFoundOnAnyLink", err)
	}
	if queue.Len() != 0 {
		t.Errorf("queue Len = %d, want 0 (definitive 404 waits for nothing)", queue.Len())
	}
}

func TestForwardOrHoldWithoutQueueReportsTransient(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	client := NewClient([]string{dead.URL}, time.Second, "")
	_, _, held, err := client.ForwardOrHold(context.Background(), "agent-remote", []byte(`{"payload":{}}`), HoldMeta{MessageID: "m"})
	if held != nil {
		t.Error("no hold manager: nothing may be held")
	}
	var transient *TransientError
	if !asTransient(err, &transient) {
		t.Fatalf("err = %v (%T), want *TransientError", err, err)
	}
	if transient.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", transient.Attempts)
	}
}

// asTransient is errors.As for *TransientError without importing errors in
// every test helper (keeps the assertion explicit).
func asTransient(err error, target **TransientError) bool {
	for err != nil {
		if te, ok := err.(*TransientError); ok {
			*target = te
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestRetryableStatusClassification(t *testing.T) {
	retryable := []int{500, 502, 503, 504, 408, 429}
	for _, s := range retryable {
		if !retryableStatus(s) {
			t.Errorf("status %d must be retryable", s)
		}
	}
	// Not retryable: a relay's answer, including the 404 that moves to the
	// next link and the 4xx definitive rejections.
	for _, s := range []int{200, 201, 202, 301, 400, 401, 403, 404, 409, 422} {
		if retryableStatus(s) {
			t.Errorf("status %d must not be retryable", s)
		}
	}
}

// ---------- HoldManager ----------

// TestHoldManagerRetriesUntilRecoveryDeliversOnce is the core DF-CRIER-7
// contract: the link is transiently down, the delivery is held, the link
// recovers inside the hold budget, and the delivery is forwarded EXACTLY
// once with the original bytes and the hop header — the recovered request is
// indistinguishable from a direct forward.
func TestHoldManagerRetriesUntilRecoveryDeliversOnce(t *testing.T) {
	relay := newFlakyRelay(t, http.StatusServiceUnavailable)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, 2*time.Second, "")
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(2*time.Second))
	client.SetHoldManager(mgr)

	body := []byte(`{"payload":{"text":"hi"},"sender":"agent-local","delivery_mode":"blocking","request_id":"req-1"}`)
	_, _, held, err := client.ForwardOrHold(context.Background(), "agent-remote", body, HoldMeta{MessageID: "msg-1", Sender: "agent-local", RequestID: "req-1"})
	mustHold(t, held, err)

	mgr.Start()
	defer mgr.Stop()

	// Hold budget not yet exhausted: the manager must be retrying.
	time.Sleep(20 * time.Millisecond)
	if relay.callCount() < 2 {
		t.Fatalf("link still failing: forwarded %d times, want at least the first pass plus a retry", relay.callCount())
	}

	relay.setStatus(http.StatusOK)
	waitFor(t, 2*time.Second, "the held delivery to be delivered", func() bool { return queue.Len() == 0 })

	// Wait a little past recovery to prove there is no duplicate forward:
	// every attempt forwards the same body, so only the 2xx answer counts as
	// a delivery.
	time.Sleep(50 * time.Millisecond)
	delivered := 0
	relay.mu.Lock()
	for i, b := range relay.bodies {
		if string(b) == string(body) && relay.statuses[i] == http.StatusOK {
			delivered++
		}
	}
	hops := append([]string(nil), relay.hops...)
	relay.mu.Unlock()
	if delivered != 1 {
		t.Errorf("the recovery pass delivered %d times, want exactly 1 (no duplicate forwarding)", delivered)
	}
	for i, hop := range hops {
		if hop != "1" {
			t.Errorf("call %d hop header = %q, want \"1\" on every forward", i, hop)
		}
	}
	if mgr.Pending() != 0 {
		t.Errorf("Pending = %d, want 0 after delivery", mgr.Pending())
	}
}

// TestHoldManagerExhaustionReportsFederationFailedExactlyOnce proves the
// terminal path: the budget expires, the item leaves the queue, and the
// notifier sees one FEDERATION_FAILED report with every correlation field.
func TestHoldManagerExhaustionReportsFederationFailedExactlyOnce(t *testing.T) {
	relay := newFlakyRelay(t, http.StatusInternalServerError)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(25*time.Millisecond))
	client.SetHoldManager(mgr)

	var (
		mu      sync.Mutex
		reports []FailureReport
	)
	mgr.SetNotifier(func(r FailureReport) error {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, r)
		return nil
	})

	_, _, held, err := client.ForwardOrHold(context.Background(), "agent-remote", []byte(`{"payload":{"text":"hi"}}`), HoldMeta{
		MessageID: "msg-9", Sender: "agent-local", RequestID: "req-9", SessionID: "sess-9",
	})
	mustHold(t, held, err)

	mgr.Start()
	defer mgr.Stop()

	waitFor(t, 2*time.Second, "the hold budget to expire and the report to land", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(reports) > 0
	})
	if queue.Len() != 0 {
		t.Errorf("queue Len = %d, want 0 after exhaustion", queue.Len())
	}

	// Extra sweeps must not produce a second report (exactly-once).
	mgr.Sweep(time.Now().Add(time.Hour))
	mgr.Sweep(time.Now().Add(2 * time.Hour))
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(reports) != 1 {
		t.Fatalf("notifications = %d, want exactly 1 (full: %+v)", len(reports), reports)
	}
	r := reports[0]
	if r.Code != CodeFederationFailed {
		t.Errorf("Code = %q, want %q", r.Code, CodeFederationFailed)
	}
	if r.MessageID != "msg-9" || r.Target != "agent-remote" || r.Sender != "agent-local" ||
		r.RequestID != "req-9" || r.SessionID != "sess-9" {
		t.Errorf("report lost correlation context: %+v", r)
	}
	if r.Attempts < 2 {
		t.Errorf("Attempts = %d, want the retries to be counted", r.Attempts)
	}
	if r.Error == "" {
		t.Error("report must carry the last error")
	}
}

// TestHoldManagerSweepOnceDrivesDeterministicExhaustion drives the exported
// sweep directly (no timer) so the exhaustion semantics are pinned without
// any timing dependence.
func TestHoldManagerSweepOnceDrivesDeterministicExhaustion(t *testing.T) {
	relay := newFlakyRelay(t, http.StatusBadGateway)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(time.Minute))
	client.SetHoldManager(mgr)

	var reports []FailureReport
	mgr.SetNotifier(func(r FailureReport) error {
		reports = append(reports, r)
		return nil
	})

	_, _, held, err := client.ForwardOrHold(context.Background(), "agent-remote", []byte(`{"payload":{}}`), HoldMeta{MessageID: "m"})
	mustHold(t, held, err)
	if len(reports) != 0 {
		t.Fatalf("holding must not report a failure yet: %+v", reports)
	}

	// A sweep inside the budget retries and records the failure state.
	mgr.Sweep(time.Now().Add(50 * time.Millisecond))
	stored := queue.List()[0]
	if stored.Attempts < 2 {
		t.Errorf("Attempts = %d, want the retry counted", stored.Attempts)
	}
	if stored.LastStatus != http.StatusBadGateway {
		t.Errorf("LastStatus = %d, want 502 recorded for the report", stored.LastStatus)
	}
	if len(reports) != 0 {
		t.Fatalf("inside the budget nothing may be reported: %+v", reports)
	}

	// A sweep past the deadline is the terminal signal.
	mgr.Sweep(held.Deadline.Add(time.Millisecond))
	if len(reports) != 1 {
		t.Fatalf("reports = %d, want exactly 1 after the deadline", len(reports))
	}
	if reports[0].Status != http.StatusBadGateway {
		t.Errorf("report Status = %d, want the recorded 502", reports[0].Status)
	}
	if queue.Len() != 0 {
		t.Errorf("queue Len = %d, want 0", queue.Len())
	}
}

// TestHoldManagerRetryNotFoundFailsTerminal: if the link comes back but the
// agent exists nowhere, the hold cannot ever succeed — it fails immediately
// instead of burning the rest of the budget.
func TestHoldManagerRetryNotFoundFailsTerminal(t *testing.T) {
	relay := newFlakyRelay(t, http.StatusNotFound)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(time.Minute))
	client.SetHoldManager(mgr)

	var reports []FailureReport
	mgr.SetNotifier(func(r FailureReport) error {
		reports = append(reports, r)
		return nil
	})
	if err := mgr.Enqueue(&HoldItem{ID: "m-404", AgentID: "ghost", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	mgr.Sweep(time.Now().Add(time.Second))

	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1 (all-404 during retry is terminal)", len(reports))
	}
	if reports[0].Status != http.StatusNotFound {
		t.Errorf("report Status = %d, want 404", reports[0].Status)
	}
	if queue.Len() != 0 {
		t.Errorf("queue Len = %d, want 0", queue.Len())
	}
}

// TestHoldManagerRetryDefinitiveRejectionIsTerminal: a 4xx answer (other than
// 404) means the relay saw the delivery and refused it; retrying cannot help,
// so the sender gets the explicit failure with that status.
func TestHoldManagerRetryDefinitiveRejectionIsTerminal(t *testing.T) {
	relay := newFlakyRelay(t, http.StatusForbidden)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(time.Minute))
	client.SetHoldManager(mgr)

	var reports []FailureReport
	mgr.SetNotifier(func(r FailureReport) error {
		reports = append(reports, r)
		return nil
	})
	if err := mgr.Enqueue(&HoldItem{ID: "m-403", AgentID: "remote", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	mgr.Sweep(time.Now().Add(time.Second))

	if len(reports) != 1 {
		t.Fatalf("reports = %d, want 1", len(reports))
	}
	if reports[0].Status != http.StatusForbidden {
		t.Errorf("report Status = %d, want 403 recorded for the sender", reports[0].Status)
	}
}

// TestHoldManagerStartStopIsClean covers the lifecycle requirement: Stop
// joins the worker goroutine, is idempotent, and a stopped manager never
// touches the queue again.
func TestHoldManagerStartStopIsClean(t *testing.T) {
	relay := newFlakyRelay(t, http.StatusServiceUnavailable)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(time.Minute))
	client.SetHoldManager(mgr)

	// Stop before Start must be safe.
	mgr.Stop()
	mgr.Stop()

	// A start/stop cycle with no traffic must leave the goroutine count
	// exactly where it started: the loop is the only thing Start adds.
	before := runtime.NumGoroutine()
	mgr.Start()
	mgr.Start() // idempotent
	mgr.Stop()
	mgr.Stop() // idempotent
	waitFor(t, 2*time.Second, "the worker goroutine to exit", func() bool { return runtime.NumGoroutine() <= before })

	// A stopped manager must never touch the queue again.
	if err := mgr.Enqueue(&HoldItem{ID: "m", AgentID: "remote", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if relay.callCount() != 0 {
		t.Errorf("stopped manager forwarded %d times, want 0", relay.callCount())
	}
	if queue.Len() != 1 {
		t.Errorf("queue Len = %d, want the undelivered item still held", queue.Len())
	}
}

// TestHoldManagerContextCancellationDoesNotLeak: a request context cancel
// during the retry attempt must not leave the worker stuck or leak
// goroutines.
func TestHoldManagerContextCancellationDoesNotLeak(t *testing.T) {
	// A link that accepts but never answers forces the attempt to run until
	// its timeout; Stop must still join promptly.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	client := NewClient([]string{srv.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(time.Minute))
	client.SetHoldManager(mgr)

	if err := mgr.Enqueue(&HoldItem{ID: "m", AgentID: "remote", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	before := runtime.NumGoroutine()
	mgr.Start()
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		mgr.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not join the worker")
	}
	waitFor(t, time.Second, "goroutines to settle", func() bool { return runtime.NumGoroutine() <= before+1 })
}

// TestHoldManagerNotifierFailureIsNotRetried: a failing notifier is logged
// and the delivery is gone — no requeue loop, no infinite reporting.
func TestHoldManagerNotifierFailureIsNotRetried(t *testing.T) {
	relay := newFlakyRelay(t, http.StatusInternalServerError)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, time.Second, "")
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(time.Minute))
	client.SetHoldManager(mgr)

	var calls int
	mgr.SetNotifier(func(FailureReport) error { calls++; return context.DeadlineExceeded })

	// Enqueued with an already-expired budget: the next sweep is terminal.
	now := time.Now()
	if err := mgr.Enqueue(&HoldItem{
		ID: "m", AgentID: "remote", Body: json.RawMessage(`{"payload":{}}`),
		EnqueuedAt: now.Add(-time.Hour), Deadline: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	mgr.Sweep(now)
	mgr.Sweep(now.Add(time.Second))
	if calls != 1 {
		t.Errorf("notifier calls = %d, want 1 (a failed notification is never retried)", calls)
	}
	if queue.Len() != 0 {
		t.Errorf("queue Len = %d, want 0", queue.Len())
	}
}

// TestForwardDeliverRetainsAuthAndHopOnRetry proves the held retry path keeps
// the DF-CRIER-6 link auth and the loop-prevention hop header intact.
func TestForwardDeliverRetainsAuthAndHopOnRetry(t *testing.T) {
	const token = "shared-federation-secret"
	relay := newFlakyRelay(t, http.StatusServiceUnavailable)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	client := NewClient([]string{srv.URL}, time.Second, token)
	queue := NewMemoryHoldQueue()
	mgr := NewHoldManager(client, queue, testHoldConfig(time.Minute))
	client.SetHoldManager(mgr)

	if err := mgr.Enqueue(&HoldItem{ID: "m", AgentID: "remote", Body: json.RawMessage(`{"payload":{}}`)}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	mgr.Sweep(time.Now().Add(time.Second))

	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.auths) == 0 {
		t.Fatal("no forward was made")
	}
	for i, auth := range relay.auths {
		if auth != BearerPrefix+token {
			t.Errorf("call %d Authorization = %q, want the link token", i, auth)
		}
	}
	for i, hop := range relay.hops {
		if hop != "1" {
			t.Errorf("call %d hop header = %q, want \"1\"", i, hop)
		}
	}
}
