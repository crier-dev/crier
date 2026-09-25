package registry

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/webhook"
)

// CR-FEAT-025 — sender idempotency keys.
//
// The lease answers "who owns this message now"; it says nothing about a
// SENDER's retry. A sender that delivered and then lost the connection has no
// way to ask whether its message landed, so it re-delivers — and before this,
// the target got two copies of the same work. An optional `idempotency_key`
// closes that gap: the duplicate is answered with the ORIGINAL accept (same
// id, same status) and stores nothing.
//
// These tests pin the property at the wire (the accept body a client reads) and
// in the store (how many messages actually exist), because a fix that answered
// a replay with the right id but still stored a second message would pass the
// first and fail the second.

// registerAgentAt registers a second/third agent in the same store.
func registerAgentAt(t *testing.T, store Store, id string) {
	t.Helper()
	_, pub := newTestPubKey(t)
	require.NoError(t, store.Register(&Agent{
		ID:           id,
		PublicKey:    HexKey(pub),
		Capabilities: []string{"work"},
	}))
}

// deliverWithKey posts a delivery carrying an idempotency key and returns the
// recorder.
func deliverWithKey(t *testing.T, router http.Handler, agentID, payload, key string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"payload": json.RawMessage(payload), "sender": "foreman"}
	if key != "" {
		body["idempotency_key"] = key
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	return doInboxRequest(t, router, http.MethodPost, "/agents/"+agentID+"/inbox", string(raw))
}

// inboxCount is the number of messages actually stored for an agent — the
// property these tests are really about. It reads queue_depth alone: a leased
// message is counted in queue_depth AND in leased_count, so summing the two
// double-counts it.
func inboxCount(t *testing.T, store Store, agentID string) int {
	t.Helper()
	depth, _, _, err := store.Stats(agentID)
	require.NoError(t, err)
	return depth
}

// ---------------------------------------------------------------------------
// One key, one message
// ---------------------------------------------------------------------------

func TestIdempotency_RepeatedDeliverStoresOneMessage(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	first := deliverWithKey(t, router, "agent-1", `{"task":1}`, "work-1")
	require.Equal(t, http.StatusCreated, first.Code, "body: %s", first.Body.String())
	var firstBody deliverResponseBody
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &firstBody))

	second := deliverWithKey(t, router, "agent-1", `{"task":1}`, "work-1")
	require.Equal(t, http.StatusCreated, second.Code,
		"a replay answers with the ORIGINAL status — body: %s", second.Body.String())
	var secondBody struct {
		ID               string `json:"id"`
		Transport        string `json:"transport"`
		IdempotentReplay bool   `json:"idempotent_replay"`
	}
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &secondBody))

	require.Equal(t, firstBody.ID, secondBody.ID, "a replay names the message the first delivery stored")
	require.True(t, secondBody.IdempotentReplay, "the replay is marked: %s", second.Body.String())
	require.Equal(t, "inbox", secondBody.Transport)
	require.Equal(t, 1, inboxCount(t, store, "agent-1"),
		"the duplicate must not store a second message")
}

func TestIdempotency_DifferentKeysStoreTwoMessages(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-1", `{"n":1}`, "k-1").Code)
	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-1", `{"n":2}`, "k-2").Code)
	require.Equal(t, 2, inboxCount(t, store, "agent-1"))
}

func TestIdempotency_AbsentKeyDeliversEveryTime(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	// No key at all is the pre-CR-FEAT-025 behaviour, unchanged: two deliveries
	// are two messages.
	first := deliverWithKey(t, router, "agent-1", `{"n":1}`, "")
	require.Equal(t, http.StatusCreated, first.Code)
	require.NotContains(t, first.Body.String(), "idempotent_replay")
	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-1", `{"n":1}`, "").Code)
	require.Equal(t, 2, inboxCount(t, store, "agent-1"))
}

func TestIdempotency_KeyIsScopedToTheTargetAgent(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "agent-2")
	router := setupRouter(store)

	// The same key aimed at two agents is two pieces of work, not one.
	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-1", `{"n":1}`, "shared").Code)
	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-2", `{"n":1}`, "shared").Code)
	require.Equal(t, 1, inboxCount(t, store, "agent-1"))
	require.Equal(t, 1, inboxCount(t, store, "agent-2"))
}

func TestIdempotency_PayloadIsNotReStoredOnReplay(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-1", `{"task":"first"}`, "k").Code)
	// A different body under the SAME key is still a duplicate: the key is the
	// sender's statement that this is the same delivery.
	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-1", `{"task":"second"}`, "k").Code)

	msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.JSONEq(t, `{"task":"first"}`, string(msgs[0].Payload))
}

func TestIdempotency_OverlongKeyRejected(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	long := make([]byte, maxIdempotencyKeyLen+1)
	for i := range long {
		long[i] = 'k'
	}
	rec := deliverWithKey(t, router, "agent-1", `{"n":1}`, string(long))
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "idempotency_key must be at most")
	require.Equal(t, 0, inboxCount(t, store, "agent-1"), "nothing may be stored for a rejected request")
}

// ---------------------------------------------------------------------------
// The window
// ---------------------------------------------------------------------------

func TestIdempotency_WindowExpiryDeliversAgain(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	handler, router := setupRouterWithHandler(store)
	// The window is the documented one for production; a test needs a window it
	// can outlive. A shorter window is a legal configuration (the handler
	// resolves any non-positive value to the default, never to "deduplicate
	// nothing").
	handler.SetIdempotencyWindow(20 * time.Millisecond)

	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-1", `{"n":1}`, "k").Code)
	require.Equal(t, 1, inboxCount(t, store, "agent-1"))

	time.Sleep(40 * time.Millisecond)

	after := deliverWithKey(t, router, "agent-1", `{"n":1}`, "k")
	require.Equal(t, http.StatusCreated, after.Code)
	require.NotContains(t, after.Body.String(), "idempotent_replay",
		"past the window the key is a fresh delivery: %s", after.Body.String())
	require.Equal(t, 2, inboxCount(t, store, "agent-1"))
}

func TestIdempotency_NonPositiveWindowFallsBackToTheDocumentedDefault(t *testing.T) {
	// A window of 0 must never mean "deduplicate nothing": that is the feature
	// silently absent while the API still documents it.
	handler := NewHandler(NewMemoryStore())
	handler.SetIdempotencyWindow(0)
	require.Equal(t, DefaultIdempotencyWindow, handler.idempotency.Window())

	handler.SetIdempotencyWindow(-time.Second)
	require.Equal(t, DefaultIdempotencyWindow, handler.idempotency.Window())

	handler.SetIdempotencyWindow(90 * time.Second)
	require.Equal(t, 90*time.Second, handler.idempotency.Window())
}

// ---------------------------------------------------------------------------
// Only an ACCEPT is replayed
// ---------------------------------------------------------------------------

func TestIdempotency_RejectedDeliveryIsNotRecorded(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	// A delivery to an unknown agent is a 404. It produced no work, so the key
	// must stay unclaimed: the corrected retry has to be DELIVERED, not
	// answered with a replay of the rejection.
	rejected := deliverWithKey(t, router, "ghost", `{"n":1}`, "k")
	require.Equal(t, http.StatusNotFound, rejected.Code, "body: %s", rejected.Body.String())

	retry := deliverWithKey(t, router, "agent-1", `{"n":1}`, "k")
	require.Equal(t, http.StatusCreated, retry.Code, "body: %s", retry.Body.String())
	require.Equal(t, 1, inboxCount(t, store, "agent-1"))
}

func TestIdempotency_ConcurrentDuplicatesProduceOneMessage(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	const racers = 8
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		codes  []int
		bodies []string
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := deliverWithKey(t, router, "agent-1", `{"n":1}`, "same-key")
			mu.Lock()
			codes = append(codes, rec.Code)
			bodies = append(bodies, rec.Body.String())
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, 1, inboxCount(t, store, "agent-1"),
		"concurrent duplicates must still store exactly one message; responses: %v", bodies)

	// Every response is an accept carrying the SAME id: the losers waited for
	// the winner's receipt and replayed it (a duplicate that cannot wait is
	// answered 409 and stores nothing — also one message, never two).
	ids := map[string]int{}
	for i, code := range codes {
		require.Contains(t, []int{http.StatusCreated, http.StatusConflict}, code,
			"response %d: %s", i, bodies[i])
		if code != http.StatusCreated {
			continue
		}
		var body struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(bodies[i]), &body))
		ids[body.ID]++
	}
	require.Len(t, ids, 1, "all accepts must name the one stored message: %v", ids)
}

// ---------------------------------------------------------------------------
// The webhook lane: no duplicate endpoint call
// ---------------------------------------------------------------------------

func TestIdempotency_WebhookRetryCallsTheEndpointOnce(t *testing.T) {
	var calls atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"echo":true}`))
	}))
	defer endpoint.Close()

	// A blocking delivery is request/response: if the key did not deduplicate
	// it, the retry would call the agent's endpoint a second time.
	h, _ := deliverHarness(t, "agent-block", &webhook.Config{
		URL:          endpoint.URL,
		DeliveryMode: "blocking",
		TimeoutMs:    2000,
	})

	body := `{"payload":{"n":1},"idempotency_key":"hook-1","delivery_mode":"blocking","sender":"foreman"}`
	first := doInboxRequest(t, h, http.MethodPost, "/agents/agent-block/inbox", body)
	require.Equal(t, http.StatusOK, first.Code, "body: %s", first.Body.String())

	second := doInboxRequest(t, h, http.MethodPost, "/agents/agent-block/inbox", body)
	require.Equal(t, http.StatusOK, second.Code, "body: %s", second.Body.String())
	require.Contains(t, second.Body.String(), `"idempotent_replay":true`,
		"a blocking replay is marked and carries the recorded reply: %s", second.Body.String())
	require.Contains(t, second.Body.String(), `"echo":true`,
		"the replayed reply is the one the single endpoint call produced: %s", second.Body.String())

	require.Equal(t, int32(1), calls.Load(), "the endpoint must be called once for one key")
}

func TestIdempotency_WebhookAsyncRetryQueuesOnce(t *testing.T) {
	var posts atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer endpoint.Close()

	h, _ := deliverHarness(t, "agent-async", &webhook.Config{
		URL:          endpoint.URL,
		DeliveryMode: "async",
		Retries:      1,
		TimeoutMs:    2000,
	})

	body := `{"payload":{"n":1},"idempotency_key":"async-1","delivery_mode":"async","sender":"foreman"}`
	first := doInboxRequest(t, h, http.MethodPost, "/agents/agent-async/inbox", body)
	require.Equal(t, http.StatusAccepted, first.Code, "body: %s", first.Body.String())

	second := doInboxRequest(t, h, http.MethodPost, "/agents/agent-async/inbox", body)
	require.Equal(t, http.StatusAccepted, second.Code, "body: %s", second.Body.String())
	require.Contains(t, second.Body.String(), `"idempotent_replay":true`, "body: %s", second.Body.String())

	// The queued POST is the work; a duplicate accept must not have queued a
	// second one. Give the driver time to flush what it was given.
	waitForEndpointPosts(t, &posts, 1, "async idempotent delivery")
	time.Sleep(150 * time.Millisecond)
	require.Equal(t, int32(1), posts.Load(), "one key means one queued POST")
}

// ---------------------------------------------------------------------------
// The registry unit: the claim/wait state machine, without HTTP in the way
// ---------------------------------------------------------------------------

func TestIdempotencyRegistry_ClaimThenRecordThenReplay(t *testing.T) {
	reg := newIdempotencyRegistry(time.Minute)
	now := time.Now()

	rec, outcome, attempt := reg.Acquire("agent", "k", now)
	require.Equal(t, idempotencyProceed, outcome)
	require.Empty(t, rec.ID, "a claimed key has no receipt yet")
	require.NotNil(t, attempt)

	// A second attempt while the first is in flight waits for it. It is released
	// by the first one's Finish, and then replays.
	done := make(chan idempotencyOutcome, 1)
	go func() {
		_, second, _ := reg.Acquire("agent", "k", time.Now())
		done <- second
	}()
	time.Sleep(20 * time.Millisecond)
	attempt.Finish(idempotentReceipt{Status: http.StatusCreated, ID: "m-1", Transport: "inbox"})

	select {
	case second := <-done:
		require.Equal(t, idempotencyReplay, second)
	case <-time.After(2 * time.Second):
		t.Fatal("a concurrent duplicate never resolved")
	}

	replayed, outcome, _ := reg.Acquire("agent", "k", time.Now())
	require.Equal(t, idempotencyReplay, outcome)
	require.Equal(t, "m-1", replayed.ID)
	require.Equal(t, http.StatusCreated, replayed.Status)
}

func TestIdempotencyRegistry_AbandonReleasesTheKey(t *testing.T) {
	reg := newIdempotencyRegistry(time.Minute)

	_, outcome, attempt := reg.Acquire("agent", "k", time.Now())
	require.Equal(t, idempotencyProceed, outcome)
	attempt.Abandon()

	_, next, _ := reg.Acquire("agent", "k", time.Now())
	require.Equal(t, idempotencyProceed, next,
		"an abandoned key must be claimable again, or a corrected retry could never be delivered")
}

func TestIdempotencyRegistry_WindowedRecordExpires(t *testing.T) {
	reg := newIdempotencyRegistry(time.Minute)
	now := time.Now()

	_, outcome, attempt := reg.Acquire("agent", "k", now)
	require.Equal(t, idempotencyProceed, outcome)
	attempt.Finish(idempotentReceipt{Status: http.StatusCreated, ID: "m-1"})

	_, replayed, _ := reg.Acquire("agent", "k", now.Add(30*time.Second))
	require.Equal(t, idempotencyReplay, replayed)

	_, fresh, _ := reg.Acquire("agent", "k", now.Add(2*time.Minute))
	require.Equal(t, idempotencyProceed, fresh,
		"past the window the record is gone and the key delivers again")
}

func TestIdempotencyRegistry_KeyCannotImpersonateAnotherPair(t *testing.T) {
	reg := newIdempotencyRegistry(time.Minute)
	now := time.Now()

	_, outcome, attempt := reg.Acquire("agent-a", "x", now)
	require.Equal(t, idempotencyProceed, outcome)
	attempt.Finish(idempotentReceipt{Status: http.StatusCreated, ID: "m-1"})

	// A pair built to collide under naive string concatenation must not reach
	// the other pair's record.
	_, other, _ := reg.Acquire("agent", "a\x00x", now)
	require.Equal(t, idempotencyProceed, other)
}

func TestValidateIdempotencyKey_Bounds(t *testing.T) {
	require.NoError(t, validateIdempotencyKey("k"))
	require.NoError(t, validateIdempotencyKey(string(make([]byte, maxIdempotencyKeyLen))))
	err := validateIdempotencyKey(string(make([]byte, maxIdempotencyKeyLen+1)))
	require.Error(t, err)
	require.Contains(t, err.Error(), "at most")
}

// ---------------------------------------------------------------------------
// Store provenance
// ---------------------------------------------------------------------------

func TestDeliver_RecordsSenderAndKeyOnTheStoredMessage(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	require.Equal(t, http.StatusCreated, deliverWithKey(t, router, "agent-1", `{"n":1}`, "k-9").Code)
	msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "foreman", msgs[0].Sender,
		"the sender is stored WITH the message: it is the address an expiry receipt goes to")
	require.Equal(t, "k-9", msgs[0].IdempotencyKey)
}

// errIsAnyOf keeps multi-class assertions readable.
func errIsAnyOf(t *testing.T, err error, wants ...error) {
	t.Helper()
	require.Error(t, err)
	for _, want := range wants {
		if errors.Is(err, want) {
			return
		}
	}
	t.Fatalf("error %v matches none of %v", err, wants)
}
