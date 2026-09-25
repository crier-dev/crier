package registry

// CR-FEAT-023 — the durable lane must be able to tap an agent on the shoulder.
//
// Two ADDITIVE surfaces are pinned here, against the contract as it stood
// BEFORE the change:
//
//  1. `?wait_seconds=` on GET /agents/{id}/inbox — the long-poll. It returns
//     early when a message is claimable, returns the SAME body a poll-only read
//     gets when the budget expires, and the poll-only path (absent / 0) is
//     byte-identical to what this endpoint always answered.
//  2. InboxPinger — the new-message ping fired after a delivery lands in the
//     inbox, and only for a delivery that actually STORED something (the
//     webhook-transport and rejected-delivery arms must never ping).
//
// Nothing here re-derives the pre-existing expectations from the code under
// test: the empty-inbox body is pinned as a literal, and every byte comparison
// is between two live responses.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/webhook"
)

// pollOnlyEmptyBody is the EXACT body GET /agents/{id}/inbox has always
// answered for a registered agent with nothing to claim (DF-CRIER-177 added the
// two counters; the key set and field order are the encoder's). Pinned as a
// literal so a change to the poll-only wire cannot ride along unnoticed inside
// the long-poll change.
const pollOnlyEmptyBody = "{\"messages\":[],\"lease_id\":\"\",\"queue_depth\":0,\"leased_count\":0}\n"

// longPollServer boots one handler with the agent-scoped routes over a real
// listener (so a parked read is a real HTTP request and a real context).
func longPollServer(t *testing.T) (*Handler, *httptest.Server, Store) {
	t.Helper()
	store := NewMemoryStore()
	h := NewHandler(store)
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods("GET")
	r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox/stats", h.HandleStats).Methods("GET")
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return h, srv, store
}

// registerLongPollAgent registers a keyless agent (signature enforcement is off
// on this handler — the routes are exercised unsigned).
func registerLongPollAgent(t *testing.T, store Store, id string) {
	t.Helper()
	if err := store.Register(&Agent{ID: id, Capabilities: []string{}}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

// deliverInbox posts one delivery and returns the accepted message id. It
// returns an error instead of failing the test so a parking test can deliver
// from a background goroutine (t.Fatalf is illegal there).
func deliverInbox(base, agentID, sender string) (string, error) {
	body := fmt.Sprintf(`{"payload":{"n":1},"sender":%q}`, sender)
	resp, err := http.Post(base+"/agents/"+agentID+"/inbox", "application/json", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("deliver: status %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func mustDeliver(t *testing.T, base, agentID, sender string) string {
	t.Helper()
	id, err := deliverInbox(base, agentID, sender)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	return id
}

// getInboxRaw performs one retrieve and returns its status and RAW body, plus
// how long the server held the request.
func getInboxRaw(t *testing.T, base, agentID, query string) (int, string, time.Duration) {
	t.Helper()
	// No client timeout: the long-poll cases are bounded by the server's own
	// budget, which is what this file measures.
	start := time.Now()
	resp, err := http.Get(base + "/agents/" + agentID + "/inbox" + query)
	if err != nil {
		t.Fatalf("GET inbox%s: %v", query, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read inbox body: %v", err)
	}
	return resp.StatusCode, string(raw), time.Since(start)
}

// decodePollBody decodes a retrieve body with the counters the endpoint
// documents (the package's own decodeRetrieve helper is recorder-shaped; this
// file works from raw bodies over a real listener).
type decodedPollBody struct {
	Messages []struct {
		ID      string `json:"id"`
		AgentID string `json:"agent_id"`
	} `json:"messages"`
	LeaseID     string `json:"lease_id"`
	QueueDepth  int    `json:"queue_depth"`
	LeasedCount int    `json:"leased_count"`
}

func decodePollBody(t *testing.T, body string) decodedPollBody {
	t.Helper()
	var out decodedPollBody
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode retrieve body %q: %v", body, err)
	}
	return out
}

// volatileRetrieveValues masks the values a retrieve response generates per
// call (message id, lease id, timestamps — including leased_at, which the lease
// sets to the moment of the claim) so two live responses can be compared for
// structural identity.
var volatileRetrieveValues = regexp.MustCompile(`"(id|lease_id|created_at|expires_at|leased_at)":"[^"]*"`)

func maskVolatile(t *testing.T, body string) string {
	t.Helper()
	return volatileRetrieveValues.ReplaceAllString(body, `"$1":"<masked>"`)
}

// ----- AC 1: the long-poll returns EARLY on delivery -----

func TestInboxLongPollReturnsEarlyOnDelivery(t *testing.T) {
	_, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")

	// Deliver 250ms into a 5s budget: an early return is the whole point, so
	// the assertion is on ELAPSED time, not just on the body.
	delivered := make(chan string, 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		id, err := deliverInbox(srv.URL, "agent-1", "agent-2")
		if err != nil {
			delivered <- ""
			return
		}
		delivered <- id
	}()

	status, body, elapsed := getInboxRaw(t, srv.URL, "agent-1", "?wait_seconds=5")
	if status != http.StatusOK {
		t.Fatalf("long-poll: status %d, want 200 (body %s)", status, body)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("long-poll returned after %s — it must answer as soon as the delivery lands, well inside the 5s budget", elapsed)
	}
	got := decodePollBody(t, body)
	if len(got.Messages) != 1 {
		t.Fatalf("long-poll: messages = %d, want 1 (body %s)", len(got.Messages), body)
	}
	if got.LeaseID == "" {
		t.Errorf("long-poll: lease_id is empty — a batch of 1 message must carry a lease (body %s)", body)
	}
	if got.Messages[0].AgentID != "agent-1" {
		t.Errorf("long-poll: message agent_id = %q, want agent-1", got.Messages[0].AgentID)
	}
	// queue_depth counts what is still undelivered-to-the-client (unacked and
	// unexpired, leased or not) while leased_count counts the claim this very
	// response made — so one claimed message reads 1/1, not 0/1.
	if got.QueueDepth != 1 || got.LeasedCount != 1 {
		t.Errorf("long-poll: queue_depth/leased_count = %d/%d, want 1/1 (claimed but not yet acked)",
			got.QueueDepth, got.LeasedCount)
	}
	id := <-delivered
	if id == "" {
		t.Fatal("background delivery failed")
	}
	if got.Messages[0].ID != id {
		t.Errorf("long-poll: message id = %q, want the delivered id %q", got.Messages[0].ID, id)
	}
}

// ----- AC 2: the long-poll times out EMPTY, with the poll-only body -----

func TestInboxLongPollTimesOutEmptyWithThePollOnlyBody(t *testing.T) {
	_, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "waiter")
	registerLongPollAgent(t, store, "poll-only")

	status, body, elapsed := getInboxRaw(t, srv.URL, "waiter", "?wait_seconds=1")
	if status != http.StatusOK {
		t.Fatalf("timed-out long-poll: status %d, want 200 (body %s)", status, body)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("timed-out long-poll returned after %s — it must hold the read for the requested 1s budget", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timed-out long-poll returned after %s — the 1s budget must not overrun", elapsed)
	}
	// A timed-out long-poll is the SAME empty read a poll-only caller gets —
	// byte-for-byte, so a client can treat both identically and simply re-poll.
	if body != pollOnlyEmptyBody {
		t.Errorf("timed-out long-poll body = %q, want the poll-only empty body %q", body, pollOnlyEmptyBody)
	}

	// The poll-only control, measured on an untouched agent in the same run.
	_, pollBody, pollElapsed := getInboxRaw(t, srv.URL, "poll-only", "")
	if pollBody != pollOnlyEmptyBody {
		t.Errorf("poll-only empty body = %q, want %q", pollBody, pollOnlyEmptyBody)
	}
	if pollElapsed >= 300*time.Millisecond {
		t.Errorf("poll-only read took %s — it must return immediately, never park", pollElapsed)
	}
	if pollBody != body {
		t.Errorf("poll-only %q and timed-out long-poll %q bodies differ", pollBody, body)
	}
}

// ----- AC 3: the poll-only path is unchanged (absent and 0) -----

func TestInboxPollOnlyPathIsUnchanged(t *testing.T) {
	_, srv, store := longPollServer(t)
	for _, id := range []string{"poll-a", "poll-b", "wait-a"} {
		registerLongPollAgent(t, store, id)
	}

	// (a) Empty inboxes: the frozen literal, and identical with wait_seconds=0.
	_, emptyNoParam, elapsedNoParam := getInboxRaw(t, srv.URL, "poll-a", "")
	_, emptyWaitZero, elapsedWaitZero := getInboxRaw(t, srv.URL, "wait-a", "?wait_seconds=0")
	if emptyNoParam != pollOnlyEmptyBody {
		t.Errorf("no-param empty body = %q, want the frozen poll-only body %q", emptyNoParam, pollOnlyEmptyBody)
	}
	if emptyWaitZero != pollOnlyEmptyBody {
		t.Errorf("wait_seconds=0 empty body = %q, want the frozen poll-only body %q", emptyWaitZero, pollOnlyEmptyBody)
	}
	if emptyNoParam != emptyWaitZero {
		t.Errorf("absent wait (%q) and wait_seconds=0 (%q) must answer identically", emptyNoParam, emptyWaitZero)
	}

	// (b) The SAME agent, read poll-only and then with wait_seconds=0 after an
	// ack, so the only differences left are the ids/timestamps each response
	// generates. Everything else — key set, field order, counters, payload
	// encoding — must match.
	mustDeliver(t, srv.URL, "poll-b", "sender-1")
	_, populatedNoParam, _ := getInboxRaw(t, srv.URL, "poll-b", "")
	first := decodePollBody(t, populatedNoParam)
	if len(first.Messages) != 1 {
		t.Fatalf("poll-only read claimed %d message(s), want 1 (body %s)", len(first.Messages), populatedNoParam)
	}
	if err := store.Ack("poll-b", first.LeaseID, []string{first.Messages[0].ID}); err != nil {
		t.Fatalf("ack the poll-only claim: %v", err)
	}
	mustDeliver(t, srv.URL, "poll-b", "sender-1")
	_, populatedWaitZero, elapsedPopulatedWaitZero := getInboxRaw(t, srv.URL, "poll-b", "?wait_seconds=0")
	if maskedNoParam, maskedWaitZero := maskVolatile(t, populatedNoParam), maskVolatile(t, populatedWaitZero); maskedNoParam != maskedWaitZero {
		t.Errorf("poll-only body and wait_seconds=0 body differ beyond generated ids/timestamps:\n  absent: %s\n  zero:   %s",
			maskedNoParam, maskedWaitZero)
	}
	if !strings.Contains(populatedNoParam, `"messages":[{"id":`) {
		t.Errorf("poll-only body does not carry the claimed message: %s", populatedNoParam)
	}

	// (c) Neither poll-only spelling may park: both must return immediately.
	if elapsedNoParam >= 300*time.Millisecond || elapsedWaitZero >= 300*time.Millisecond {
		t.Errorf("poll-only reads took %s / %s — an absent or 0 budget must never wait", elapsedNoParam, elapsedWaitZero)
	}
	if elapsedPopulatedWaitZero >= 300*time.Millisecond {
		t.Errorf("populated wait_seconds=0 read took %s — a claimable message is returned, not waited for", elapsedPopulatedWaitZero)
	}
}

// ----- the budget is honored or rejected, never silently ignored -----

func TestInboxWaitSecondsValidation(t *testing.T) {
	_, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")

	cases := []struct {
		name   string
		query  string
		status int
	}{
		{"non-integer", "?wait_seconds=abc", http.StatusBadRequest},
		{"float", "?wait_seconds=1.5", http.StatusBadRequest},
		{"negative", "?wait_seconds=-1", http.StatusBadRequest},
		{"above the ceiling", "?wait_seconds=121", http.StatusBadRequest},
		{"absurd", "?wait_seconds=99999999999999999999", http.StatusBadRequest},
		{"empty is absent", "?wait_seconds=", http.StatusOK},
		{"explicit zero", "?wait_seconds=0", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, elapsed := getInboxRaw(t, srv.URL, "agent-1", tc.query)
			if status != tc.status {
				t.Fatalf("status %d, want %d (body %s)", status, tc.status, body)
			}
			if tc.status == http.StatusBadRequest {
				if !strings.Contains(body, "wait_seconds") {
					t.Errorf("400 body %q must name the parameter", body)
				}
				if elapsed >= 300*time.Millisecond {
					t.Errorf("rejected budget took %s — a 400 must not park", elapsed)
				}
			}
		})
	}
	// The accepted edge is asserted on the constant, not by parking a request
	// for two minutes: 121 is rejected above, so 120 is the ceiling.
	if maxWaitSeconds != 120 {
		t.Errorf("maxWaitSeconds = %d, want 120 (the documented ceiling; the case table rejects 121)", maxWaitSeconds)
	}
}

// TestInboxWaitSecondsAcceptanceBoundary drives the ceiling as a VALUE (not as
// a parked request): a budget below it must be ACCEPTED, and the read must
// still return immediately when the agent already has a claimable message.
func TestInboxWaitSecondsAcceptanceBoundary(t *testing.T) {
	_, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")
	mustDeliver(t, srv.URL, "agent-1", "sender-1")

	status, body, elapsed := getInboxRaw(t, srv.URL, "agent-1", "?wait_seconds=120")
	if status != http.StatusOK {
		t.Fatalf("wait_seconds=120: status %d, want 200 (body %s)", status, body)
	}
	if elapsed >= time.Second {
		t.Errorf("wait_seconds=120 with a claimable message took %s — a claimable message is returned at once", elapsed)
	}
	if got := decodePollBody(t, body); len(got.Messages) != 1 {
		t.Fatalf("wait_seconds=120 returned no message: %s", body)
	}
}

// ----- errors and parameter rejections are never waited out -----

func TestInboxLongPollDoesNotWaitOutErrors(t *testing.T) {
	_, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")

	status, body, elapsed := getInboxRaw(t, srv.URL, "ghost-agent", "?wait_seconds=5")
	if status != http.StatusNotFound {
		t.Fatalf("unregistered agent: status %d, want 404 (body %s)", status, body)
	}
	if elapsed >= time.Second {
		t.Errorf("404 took %s — a store error must not be held for the budget", elapsed)
	}

	// A rejected `limit` is answered before any wait for the same reason.
	status, body, elapsed = getInboxRaw(t, srv.URL, "agent-1", "?limit=101&wait_seconds=5")
	if status != http.StatusBadRequest {
		t.Fatalf("limit>100: status %d, want 400 (body %s)", status, body)
	}
	if elapsed >= time.Second {
		t.Errorf("rejected limit took %s — validation must precede the wait", elapsed)
	}
}

// ----- out-of-band inbox writes are covered by the fallback re-read -----

func TestInboxLongPollWakesOnOutOfBandStoreWrite(t *testing.T) {
	_, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")

	// A DIRECT store write, bypassing HandleDeliver: this is the shape of the
	// federation hold queue, a webhook-failure notice, or a second relay
	// process sharing the store. No wake-up edge exists for it, so the bounded
	// fallback re-read is the only thing that can surface it — and it must,
	// well inside the budget.
	go func() {
		time.Sleep(250 * time.Millisecond)
		_ = store.Deliver("agent-1", &InboxEntry{ID: "out-of-band", Payload: []byte(`{"n":1}`)})
	}()

	status, body, elapsed := getInboxRaw(t, srv.URL, "agent-1", "?wait_seconds=4")
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", status, body)
	}
	got := decodePollBody(t, body)
	if len(got.Messages) != 1 || got.Messages[0].ID != "out-of-band" {
		t.Fatalf("out-of-band write was not surfaced: %s", body)
	}
	if elapsed > 3*time.Second {
		t.Errorf("out-of-band write surfaced only after %s — the fallback re-read (waitFallbackInterval=%s) bounds this",
			elapsed, waitFallbackInterval)
	}
}

// ----- the notifier itself: wake-up edges, and nil safety -----

func TestInboxNotifierWakesWaitersAndToleratesNil(t *testing.T) {
	// A nil notifier (a zero-value Handler that NewHandler never built) must
	// degrade to no-wake-up, never panic on the delivery path.
	var nilNotifier *inboxNotifier
	nilNotifier.notify("agent-1")
	if w := nilNotifier.subscribe("agent-1"); w != nil {
		t.Fatalf("nil notifier subscribe returned %v, want nil", w)
	}
	nilNotifier.unsubscribe("agent-1", nil)

	n := newInboxNotifier()
	first := n.subscribe("agent-1")
	second := n.subscribe("agent-1")
	other := n.subscribe("agent-2")

	n.notify("agent-1")
	for i, w := range []*inboxWaiter{first, second} {
		select {
		case <-w.ready:
		case <-time.After(time.Second):
			t.Fatalf("waiter %d was not woken", i)
		}
	}
	select {
	case <-other.ready:
		t.Fatal("a waiter parked on another agent was woken")
	case <-time.After(50 * time.Millisecond):
	}

	// Re-signalling an already-signalled waiter must not block the delivery
	// path (the signal is a non-blocking send into a buffered channel).
	done := make(chan struct{})
	go func() {
		n.notify("agent-1")
		n.notify("agent-1")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notify blocked on an already-signalled waiter")
	}

	n.unsubscribe("agent-1", first)
	n.unsubscribe("agent-1", second)
	n.unsubscribe("agent-2", other)
	n.waitersMu(t)
}

// waitersMu asserts the notifier's key set is empty — no waiter bookkeeping may
// outlive the reads that created it.
func (n *inboxNotifier) waitersMu(t *testing.T) {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.waiters) != 0 {
		t.Fatalf("notifier still tracks %d agent(s) after every unsubscribe: %v", len(n.waiters), n.waiters)
	}
}

// ----- a cancelled request ends the park instead of holding it -----

func TestInboxLongPollCancelledContextEndsThePark(t *testing.T) {
	h, _, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	messages, leaseID, err := h.retrieveWithWait(ctx, "agent-1", 30*time.Second, 10, 30*time.Second)
	elapsed := time.Since(start)
	if !errorsIsContext(err) {
		t.Fatalf("cancelled wait returned err = %v, want a context error", err)
	}
	if messages != nil || leaseID != "" {
		t.Errorf("cancelled wait returned messages=%v lease=%q, want none", messages, leaseID)
	}
	if elapsed >= time.Second {
		t.Errorf("cancelled wait took %s — the client is gone, the park must end at once", elapsed)
	}
}

func errorsIsContext(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

// ----- the new-message ping (CR-FEAT-023, deliverable 2) -----

// recordingPinger is a stand-in for the mesh tick: it records what the delivery
// path asked it to ping.
type recordingPinger struct {
	mu    sync.Mutex
	calls []pingCall
	reply bool
}

type pingCall struct {
	agentID   string
	messageID string
	sender    string
}

func (p *recordingPinger) PingInbox(agentID, messageID, sender string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, pingCall{agentID: agentID, messageID: messageID, sender: sender})
	return p.reply
}

func (p *recordingPinger) snapshot() []pingCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]pingCall(nil), p.calls...)
}

func (p *recordingPinger) await(t *testing.T, want int) []pingCall {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls := p.snapshot()
		if len(calls) >= want {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("pings = %d (%+v), want %d", len(calls), calls, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeliverFiresInboxPingOnStoredDelivery(t *testing.T) {
	h, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")
	pinger := &recordingPinger{reply: true}
	h.SetInboxPinger(pinger)

	id := mustDeliver(t, srv.URL, "agent-1", "agent-2")
	calls := pinger.await(t, 1)
	if calls[0].agentID != "agent-1" || calls[0].messageID != id || calls[0].sender != "agent-2" {
		t.Errorf("ping = %+v, want {agent-1 %s agent-2}", calls[0], id)
	}
}

func TestDeliverDoesNotPingOutsideTheInboxLane(t *testing.T) {
	// (a) a delivery the relay REJECTS must not ping: nothing was stored.
	h, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")
	pinger := &recordingPinger{reply: true}
	h.SetInboxPinger(pinger)

	resp, err := http.Post(srv.URL+"/agents/ghost-agent/inbox", "application/json", strings.NewReader(`{"payload":{"n":1}}`))
	if err != nil {
		t.Fatalf("deliver to ghost: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deliver to ghost: status %d, want 404", resp.StatusCode)
	}
	resp, err = http.Post(srv.URL+"/agents/agent-1/inbox", "application/json", strings.NewReader(`{"payloads":{"n":1}}`))
	if err != nil {
		t.Fatalf("deliver with a bad payload key: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid payload: status %d, want 400", resp.StatusCode)
	}
	time.Sleep(200 * time.Millisecond)
	if calls := pinger.snapshot(); len(calls) != 0 {
		t.Errorf("rejected deliveries pinged the agent: %+v", calls)
	}

	// (b) a WEBHOOK-transport delivery bypasses the inbox by design (201→202
	// on the async path), so there is no inbox message to ping about.
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()

	webhookStore := NewMemoryStore()
	if err := webhookStore.Register(&Agent{
		ID: "agent-w",
		Webhook: &webhook.Config{
			URL:          hook.URL,
			DeliveryMode: "async",
			TimeoutMs:    5000,
		},
	}); err != nil {
		t.Fatalf("register webhook agent: %v", err)
	}
	webhookHandler := NewHandler(webhookStore)
	driver := webhook.NewDriver(webhook.NewClient(2*time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{
		MaxRetries:     1,
		RedeliverEvery: 200 * time.Millisecond,
		ProbeEvery:     200 * time.Millisecond,
	})
	driver.Start()
	defer driver.Stop()
	driver.SetConfigResolver(func(agentID string) (*webhook.Config, error) {
		agent, err := webhookStore.Get(agentID)
		if err != nil {
			return nil, err
		}
		if agent.Webhook == nil {
			return nil, fmt.Errorf("agent %s has no webhook configured", agentID)
		}
		return agent.Webhook, nil
	})
	webhookHandler.SetWebhookDriver(driver)
	webhookPinger := &recordingPinger{reply: true}
	webhookHandler.SetInboxPinger(webhookPinger)

	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", webhookHandler.HandleDeliver).Methods("POST")
	router.HandleFunc("/agents/{id}/inbox", webhookHandler.HandleRetrieve).Methods("GET")
	webhookSrv := httptest.NewServer(router)
	defer webhookSrv.Close()

	resp, err = http.Post(webhookSrv.URL+"/agents/agent-w/inbox", "application/json", strings.NewReader(`{"payload":{"n":1}}`))
	if err != nil {
		t.Fatalf("webhook deliver: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook deliver: status %d, want 202", resp.StatusCode)
	}
	time.Sleep(300 * time.Millisecond)
	if calls := webhookPinger.snapshot(); len(calls) != 0 {
		t.Errorf("a webhook-transport delivery pinged the inbox lane: %+v", calls)
	}
	// The retrieve that follows is the empty read: the webhook lane bypassed
	// the inbox, which is exactly why there is nothing to ping about.
	_, body, _ := getInboxRaw(t, webhookSrv.URL, "agent-w", "")
	if body != pollOnlyEmptyBody {
		t.Errorf("webhook-transport inbox read = %q, want the empty body %q", body, pollOnlyEmptyBody)
	}
}

// TestInboxPingFailureIsInvisibleToTheDelivery pins the fire-and-forget
// contract: a pinger that reports failure (or panics-free false) must not change
// the accept the sender sees.
func TestInboxPingFailureIsInvisibleToTheDelivery(t *testing.T) {
	h, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")
	pinger := &recordingPinger{reply: false}
	h.SetInboxPinger(pinger)

	id := mustDeliver(t, srv.URL, "agent-1", "")
	if id == "" {
		t.Fatal("deliver returned no message id")
	}
	calls := pinger.await(t, 1)
	if calls[0].agentID != "agent-1" || calls[0].messageID != id {
		t.Errorf("ping = %+v, want the stored delivery {agent-1 %s}", calls[0], id)
	}
	// The message is durable and retrievable regardless of the ping outcome.
	_, body, _ := getInboxRaw(t, srv.URL, "agent-1", "")
	if !strings.Contains(body, `"id":"`+id+`"`) {
		t.Errorf("the stored delivery is not retrievable: %s", body)
	}
}

// TestInboxLongPollWithoutPingerStillWorks is the wiring-independence check:
// the long-poll is internal to the handler, so it works with no pinger at all
// (every deployment before CR-FEAT-023, and the default in tests).
func TestInboxLongPollWithoutPingerStillWorks(t *testing.T) {
	h, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")
	if h.inboxPing != nil {
		t.Fatal("handler has a pinger wired; this test is about the unwired default")
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = deliverInbox(srv.URL, "agent-1", "agent-2")
	}()
	status, body, elapsed := getInboxRaw(t, srv.URL, "agent-1", "?wait_seconds=5")
	if status != http.StatusOK || elapsed >= 2*time.Second {
		t.Fatalf("long-poll without a pinger: status %d after %s (body %s)", status, elapsed, body)
	}
	if got := decodePollBody(t, body); len(got.Messages) != 1 {
		t.Fatalf("long-poll without a pinger returned no message: %s", body)
	}
}

// TestInboxLongPollConcurrentWaitersShareTheBatch pins that the wake-up is a
// notification, not a private hand-off: two parked reads on one agent, one
// delivery each, every message claimed exactly once under the normal lease
// rules.
func TestInboxLongPollConcurrentWaitersShareTheBatch(t *testing.T) {
	_, srv, store := longPollServer(t)
	registerLongPollAgent(t, store, "agent-1")

	type result struct {
		body    string
		elapsed time.Duration
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			start := time.Now()
			resp, err := http.Get(srv.URL + "/agents/agent-1/inbox?wait_seconds=3")
			if err != nil {
				results <- result{body: "ERR " + err.Error()}
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			results <- result{body: string(raw), elapsed: time.Since(start)}
		}()
	}
	time.Sleep(250 * time.Millisecond) // both readers are parked
	mustDeliver(t, srv.URL, "agent-1", "sender-1")
	mustDeliver(t, srv.URL, "agent-1", "sender-1")

	seen := map[string]bool{}
	total := 0
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.elapsed >= 5*time.Second {
				t.Fatalf("a parked reader overran its 3s budget: %s after %s", r.body, r.elapsed)
			}
			got := decodePollBody(t, r.body)
			for _, m := range got.Messages {
				if seen[m.ID] {
					t.Fatalf("message %s was handed to two readers: %s", m.ID, r.body)
				}
				seen[m.ID] = true
			}
			total += len(got.Messages)
		case <-time.After(6 * time.Second):
			t.Fatal("a parked reader never answered")
		}
	}
	if total != 2 {
		t.Errorf("readers claimed %d message(s), want 2", total)
	}
}
