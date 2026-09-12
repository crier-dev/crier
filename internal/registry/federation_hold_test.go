package registry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/totalwindupflightsystems/crier/internal/federation"
)

// holdRelay is a linked relay whose status the test controls at runtime, so
// a hold can be recovered (or left failing) deterministically.
type holdRelay struct {
	t        *testing.T
	mu       sync.Mutex
	status   atomic.Int64
	calls    int
	okCalls  int
	bodies   [][]byte
	hops     []string
	auths    []string
	response string
}

func newHoldRelay(t *testing.T, status int, response string) *holdRelay {
	r := &holdRelay{t: t, response: response}
	r.status.Store(int64(status))
	return r
}

func (r *holdRelay) setStatus(status int) { r.status.Store(int64(status)) }

func (r *holdRelay) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		buf := new(bytes.Buffer)
		buf.ReadFrom(req.Body)
		status := int(r.status.Load())
		r.mu.Lock()
		r.calls++
		if status >= 200 && status < 300 {
			r.okCalls++
		}
		r.bodies = append(r.bodies, buf.Bytes())
		r.hops = append(r.hops, req.Header.Get(federation.HopHeader))
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if r.response != "" {
			w.Write([]byte(r.response))
		}
	}
}

func (r *holdRelay) snapshot() (calls, okCalls int, bodies [][]byte, hops []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.okCalls, append([][]byte(nil), r.bodies...), append([]string(nil), r.hops...)
}

// holdRouter wires the registry routes with a federation client whose hold
// manager uses millisecond timings (no test waits real seconds). Returned
// values: the router, the store-backed handler, the hold manager, the queue.
func holdRouter(t *testing.T, store Store, links []string, maxHold time.Duration) (*mux.Router, *Handler, *federation.HoldManager, federation.HoldQueue) {
	t.Helper()
	handler := NewHandler(store)
	client := federation.NewClient(links, 2*time.Second, "")
	queue := federation.NewMemoryHoldQueue()
	mgr := federation.NewHoldManager(client, queue, federation.HoldConfig{
		MaxHold:          maxHold,
		RetryEvery:       time.Millisecond,
		MaxRetryInterval: 3 * time.Millisecond,
	})
	mgr.SetNotifier(FederationFailureSink(store))
	client.SetHoldManager(mgr)
	handler.SetFederationClient(client)

	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	return r, handler, mgr, queue
}

func waitForHold(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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

func inboxMessages(t *testing.T, store Store, agentID string) []*InboxEntry {
	t.Helper()
	msgs, _, err := store.Retrieve(agentID, time.Minute, 50)
	if err != nil {
		t.Fatalf("retrieve %s inbox: %v", agentID, err)
	}
	return msgs
}

// TestHandleDeliverFederationTransientOutageHeld: a delivery to an unknown
// agent while every link is unreachable is held at the source (202) instead
// of being reported as agent-not-found (DF-CRIER-7).
func TestHandleDeliverFederationTransientOutageHeld(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close() // unreachable link

	store := setupTestStore(t)
	registerTestAgent(t, store)
	router, _, mgr, queue := holdRouter(t, store, []string{dead.URL}, time.Minute)
	defer mgr.Stop()

	reqBody := []byte(`{"payload":{"text":"ping"},"sender":"agent-1","delivery_mode":"blocking","session_id":"sess-1","request_id":"req-1"}`)
	rec := deliverTo(t, router, "agent-remote", reqBody, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (held, not 404) — body: %s", rec.Code, rec.Body.String())
	}
	var held federationHeldResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &held); err != nil {
		t.Fatalf("held response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if held.Status != "held" {
		t.Errorf("status field = %q, want \"held\"", held.Status)
	}
	if held.ID == "" {
		t.Error("held response must carry the assigned message id")
	}
	if held.Target != "agent-remote" {
		t.Errorf("target = %q, want agent-remote", held.Target)
	}
	if held.MaxHoldS != 60 {
		t.Errorf("max_hold_s = %d, want 60 (the configured CR_FED_MAX_HOLD_S)", held.MaxHoldS)
	}
	if queue.Len() != 1 {
		t.Fatalf("queue Len = %d, want 1 held delivery", queue.Len())
	}
	item := queue.List()[0]
	if item.ID != held.ID || item.AgentID != "agent-remote" || item.Sender != "agent-1" ||
		item.RequestID != "req-1" || item.SessionID != "sess-1" {
		t.Errorf("held item lost correlation context: %+v", item)
	}
	// The held body is the handler's re-encoded deliver request: the same
	// JSON object the first pass forwarded (key order is not significant).
	var gotBody, wantBody map[string]any
	if err := json.Unmarshal(item.Body, &gotBody); err != nil {
		t.Fatalf("held body is not JSON: %v (%s)", err, item.Body)
	}
	if err := json.Unmarshal(reqBody, &wantBody); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotBody, wantBody) {
		t.Errorf("held body = %s, want the original deliver request %s", item.Body, reqBody)
	}
	// Nothing was delivered, and no failure was reported yet.
	if msgs := inboxMessages(t, store, "agent-1"); len(msgs) != 0 {
		t.Errorf("sender inbox = %d messages, want 0 while the hold is still running", len(msgs))
	}
}

// TestHandleDeliverFederationTransientWithoutHoldQueueReturns502: with no
// hold queue the transient failure is surfaced synchronously as a bounded
// FEDERATION_FAILED 502 — never the old misleading 404.
func TestHandleDeliverFederationTransientWithoutHoldQueueReturns502(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dead.Close()

	store := setupTestStore(t)
	registerTestAgent(t, store)
	handler := NewHandler(store)
	handler.SetFederationClient(federation.NewClient([]string{dead.URL}, 2*time.Second, ""))
	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")

	reqBody := []byte(`{"payload":{"text":"ping"},"sender":"agent-1","request_id":"req-9","session_id":"sess-9"}`)
	rec := deliverTo(t, router, "agent-remote", reqBody, nil)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("a link outage must never be reported as 404 (body: %s)", rec.Body.String())
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 FEDERATION_FAILED — body: %s", rec.Code, rec.Body.String())
	}
	var failure federationFailureResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &failure); err != nil {
		t.Fatalf("failure response is not JSON: %v (%s)", err, rec.Body.String())
	}
	if failure.Error != federation.CodeFederationFailed {
		t.Errorf("error code = %q, want %q", failure.Error, federation.CodeFederationFailed)
	}
	if failure.MessageID == "" {
		t.Error("failure body must carry the assigned message id")
	}
	if failure.Target != "agent-remote" || failure.Sender != "agent-1" ||
		failure.RequestID != "req-9" || failure.SessionID != "sess-9" {
		t.Errorf("failure body lost correlation context: %+v", failure)
	}
	if failure.Attempts < 1 {
		t.Errorf("attempts = %d, want at least 1", failure.Attempts)
	}
	if failure.Detail == "" {
		t.Error("failure body must explain the outage")
	}
}

// TestHandleDeliverFederationRetryable5xxIsHeldNotRelayed locks the
// classification: a retryable remote 5xx means "the link is unhealthy", not
// "here is the answer" — it is held rather than relayed to the sender.
func TestHandleDeliverFederationRetryable5xxIsHeldNotRelayed(t *testing.T) {
	relay := newHoldRelay(t, http.StatusServiceUnavailable, `{"error":"relay overloaded"}`)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	store := setupTestStore(t)
	registerTestAgent(t, store)
	router, _, mgr, queue := holdRouter(t, store, []string{srv.URL}, time.Minute)
	defer mgr.Stop()

	rec := deliverTo(t, router, "agent-remote", []byte(`{"payload":{},"sender":"agent-1"}`), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (503 is retryable, not an answer) — body: %s", rec.Code, rec.Body.String())
	}
	if queue.Len() != 1 {
		t.Fatalf("queue Len = %d, want 1", queue.Len())
	}
	if item := queue.List()[0]; item.LastStatus != http.StatusServiceUnavailable {
		t.Errorf("held item LastStatus = %d, want 503 recorded", item.LastStatus)
	}
}

// TestHandleDeliverFederationRecoveryDeliversOnceAndReportsNothing: the link
// comes back inside the hold budget; the delivery is forwarded exactly once
// with the original bytes and no failure notification is written.
func TestHandleDeliverFederationRecoveryDeliversOnceAndReportsNothing(t *testing.T) {
	relay := newHoldRelay(t, http.StatusServiceUnavailable, `{"id":"remote-1","reply":{"text":"pong"}}`)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	store := setupTestStore(t)
	registerTestAgent(t, store)
	router, _, mgr, queue := holdRouter(t, store, []string{srv.URL}, 5*time.Second)
	defer mgr.Stop()

	reqBody := []byte(`{"payload":{"text":"ping"},"sender":"agent-1","delivery_mode":"blocking","request_id":"req-1"}`)
	rec := deliverTo(t, router, "agent-remote", reqBody, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 while the link is down — body: %s", rec.Code, rec.Body.String())
	}

	mgr.Start()
	relay.setStatus(http.StatusOK)
	waitForHold(t, 3*time.Second, "the held delivery to be delivered", func() bool { return queue.Len() == 0 })
	time.Sleep(40 * time.Millisecond) // a little past recovery: prove no duplicate

	calls, okCalls, bodies, hops := relay.snapshot()
	if okCalls != 1 {
		t.Errorf("successful forwards = %d (of %d), want exactly 1 (no duplicate delivery)", okCalls, calls)
	}
	if len(bodies) == 0 || string(bodies[okCalls-1]) != string(reqBody) {
		t.Errorf("forwarded body = %s, want the original deliver JSON", bodies)
	}
	for i, hop := range hops {
		if hop != "1" {
			t.Errorf("call %d hop header = %q, want \"1\"", i, hop)
		}
	}
	if msgs := inboxMessages(t, store, "agent-1"); len(msgs) != 0 {
		t.Errorf("sender inbox = %d messages, want none after a successful recovery", len(msgs))
	}
}

// TestHandleDeliverFederationExhaustionNotifiesSenderExactlyOnce is the
// terminal contract: the hold budget expires and the sender gets exactly one
// durable FEDERATION_FAILED entry carrying the correlation context.
func TestHandleDeliverFederationExhaustionNotifiesSenderExactlyOnce(t *testing.T) {
	relay := newHoldRelay(t, http.StatusServiceUnavailable, "")
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	store := setupTestStore(t)
	registerTestAgent(t, store)
	router, _, mgr, queue := holdRouter(t, store, []string{srv.URL}, 20*time.Millisecond)
	defer mgr.Stop()

	reqBody := []byte(`{"payload":{"text":"ping"},"sender":"agent-1","request_id":"req-42","session_id":"sess-42"}`)
	rec := deliverTo(t, router, "agent-remote", reqBody, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 — body: %s", rec.Code, rec.Body.String())
	}
	var held federationHeldResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &held); err != nil {
		t.Fatalf("held response: %v", err)
	}

	mgr.Start()
	// Poll with Stats: Retrieve leases messages, so polling it would consume
	// the notification before the assertions run.
	waitForHold(t, 3*time.Second, "the hold budget to expire and the notification to land", func() bool {
		depth, leased, _, err := store.Stats("agent-1")
		return err == nil && depth+leased > 0
	})
	waitForHold(t, 2*time.Second, "the queue to drain", func() bool { return queue.Len() == 0 })

	// Extra sweeps must not produce a second notification.
	mgr.Sweep(time.Now().Add(time.Hour))
	mgr.Sweep(time.Now().Add(2 * time.Hour))
	time.Sleep(20 * time.Millisecond)

	depth, leased, _, err := store.Stats("agent-1")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if depth+leased != 1 {
		t.Fatalf("sender inbox holds %d notifications (queued %d, leased %d), want exactly 1", depth+leased, depth, leased)
	}

	msgs := inboxMessages(t, store, "agent-1")
	if len(msgs) != 1 {
		t.Fatalf("sender inbox = %d notifications, want exactly 1", len(msgs))
	}
	entry := msgs[0]
	if entry.AgentID != "agent-1" {
		t.Errorf("notification landed on %q, want the sender's own inbox", entry.AgentID)
	}
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("notification payload is not machine-readable JSON: %v (%s)", err, entry.Payload)
	}
	checks := map[string]any{
		"kind":       "error",
		"code":       federation.CodeFederationFailed,
		"message_id": held.ID,
		"target":     "agent-remote",
		"sender":     "agent-1",
		"request_id": "req-42",
		"session_id": "sess-42",
	}
	for k, want := range checks {
		if got := payload[k]; got != want {
			t.Errorf("payload[%q] = %v, want %v (payload: %s)", k, got, want, entry.Payload)
		}
	}
	if got, ok := payload["attempts"].(float64); !ok || got < 2 {
		t.Errorf("payload[attempts] = %v, want the retry count", payload["attempts"])
	}
	if errText, _ := payload["error"].(string); errText == "" {
		t.Error("payload must carry the last error")
	}
}

// TestHandleDeliverFederationAllLinks404StaysNotFoundWithHoldQueue proves the
// distinction survives the hold machinery: a definitive all-404 is answered
// immediately and never queued.
func TestHandleDeliverFederationAllLinks404StaysNotFoundWithHoldQueue(t *testing.T) {
	relay := newHoldRelay(t, http.StatusNotFound, `{"error":"agent not found"}`)
	srv := httptest.NewServer(relay.handler())
	defer srv.Close()

	store := setupTestStore(t)
	registerTestAgent(t, store)
	router, _, mgr, queue := holdRouter(t, store, []string{srv.URL}, time.Minute)
	defer mgr.Stop()

	rec := deliverTo(t, router, "ghost", []byte(`{"payload":{},"sender":"agent-1"}`), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a definitive all-links-404 — body: %s", rec.Code, rec.Body.String())
	}
	if queue.Len() != 0 {
		t.Errorf("queue Len = %d, want 0 (a 404 waits for nothing)", queue.Len())
	}
	_, okCalls, _, _ := relay.snapshot()
	_ = okCalls
	if msgs := inboxMessages(t, store, "agent-1"); len(msgs) != 0 {
		t.Errorf("sender inbox = %d, want 0 (no failure notification for a 404)", len(msgs))
	}
}

// TestFederationFailureSinkWritesDurableNotification covers the sink itself:
// one machine-readable FEDERATION_FAILED entry in the sender's inbox, and an
// error (logged, never requeued) when the sender cannot receive it.
func TestFederationFailureSinkWritesDurableNotification(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	sink := FederationFailureSink(store)

	report := federation.FailureReport{
		Code: federation.CodeFederationFailed, MessageID: "msg-1", Target: "agent-remote",
		Sender: "agent-1", RequestID: "req-1", SessionID: "sess-1",
		Attempts: 4, Status: 503, Error: "link down",
	}
	if err := sink(report); err != nil {
		t.Fatalf("sink: %v", err)
	}
	msgs := inboxMessages(t, store, "agent-1")
	if len(msgs) != 1 {
		t.Fatalf("inbox = %d, want exactly 1 durable notification", len(msgs))
	}
	var payload map[string]any
	if err := json.Unmarshal(msgs[0].Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload["code"] != federation.CodeFederationFailed || payload["status_code"] != nil {
		t.Errorf("unexpected payload fields: %s", msgs[0].Payload)
	}
	for k, want := range map[string]any{"kind": "error", "message_id": "msg-1", "target": "agent-remote", "status": float64(503), "attempts": float64(4), "error": "link down"} {
		if got := payload[k]; got != want {
			t.Errorf("payload[%q] = %v, want %v (%s)", k, got, want, msgs[0].Payload)
		}
	}

	// An absent sender is reported, never silently accepted.
	if err := sink(federation.FailureReport{Code: federation.CodeFederationFailed, MessageID: "m", Target: "t"}); err == nil {
		t.Error("a report without a sender must return an error for the manager to log")
	}
	// An unregistered sender cannot receive the notification.
	if err := sink(federation.FailureReport{Code: federation.CodeFederationFailed, MessageID: "m", Target: "t", Sender: "ghost-sender"}); err == nil {
		t.Error("an unregistered sender must return an error")
	}
}
