package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// priority_test.go — CR-FEAT-035 at the registry edge: the `priority` field
// orders an inbox, the global ingest budget sheds with a named 429, and the
// store-wide queue depth is reportable. The three properties this file exists to
// pin, in the order the row states them:
//
//  1. under queue pressure a HIGHER-priority message is retrieved before a
//     lower-priority one (and equal priorities keep arrival order, and a
//     delivery that names no priority is the FIFO message it always was);
//  2. the global budget sheds with 429 + Retry-After + a NAMED error, and does
//     not exist at all when unconfigured;
//  3. queue depth is measurable from the serving store.

// ---- priority: retrieval order ---------------------------------------------

// deliverPriority stores one message with the given priority directly through
// the store (the HTTP edge is covered separately below). A nil priority means
// "the delivery named none" — the CR-FEAT-035 default path.
func deliverPriority(t *testing.T, store Store, agentID, id string, priority *int) {
	t.Helper()
	entry := &InboxEntry{ID: id, Payload: json.RawMessage(`{"n":1}`)}
	if priority != nil {
		entry.Priority = *priority
	}
	require.NoError(t, store.Deliver(agentID, entry))
}

// idsOf returns the message ids of a retrieval batch, in the order returned.
func idsOf(entries []*InboxEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

func intPtr(v int) *int { return &v }

// queueDepthOf reads the store-wide depth through the OPTIONAL capability, the
// way GET /status and the metrics gauges do: a store that does not implement it
// is "cannot report", not "zero".
func queueDepthOf(t *testing.T, store Store) QueueDepth {
	t.Helper()
	reporter, ok := store.(DepthReporter)
	require.True(t, ok, "MemoryStore must implement DepthReporter (CR-FEAT-035)")
	depth, err := reporter.QueueDepth()
	require.NoError(t, err)
	return depth
}

func TestMemoryStore_RetrieveTakesTheHighestPriorityFirst(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	// A long low-value backlog, then one urgent message behind it. This is the
	// starvation the review named: without priority, the urgent message waits
	// for every message in front of it.
	for i := 0; i < 5; i++ {
		deliverPriority(t, store, "agent-1", "low-"+strconv.Itoa(i), intPtr(0))
	}
	deliverPriority(t, store, "agent-1", "urgent", intPtr(MaxMessagePriority))

	// max=1: the batch is CUT AFTER ordering, which is the whole point — a
	// priority that only reordered an already-chosen batch would still hand
	// back low-0 here.
	first, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 1)
	require.NoError(t, err)
	require.NotEmpty(t, leaseID)
	require.Equal(t, []string{"urgent"}, idsOf(first),
		"the highest-priority message must be retrieved first, whatever its arrival position")

	// The rest still comes back in arrival order, at their own priority.
	rest, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"low-0", "low-1", "low-2", "low-3", "low-4"}, idsOf(rest),
		"equal-priority messages keep their arrival order")
}

func TestMemoryStore_RetrieveEqualPriorityKeepsArrivalOrder(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	for i, id := range []string{"a", "b", "c"} {
		deliverPriority(t, store, "agent-1", id, intPtr(5))
		_ = i
	}
	msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, idsOf(msgs))
}

// TestMemoryStore_RetrieveWithoutPriorityIsUnchangedFIFO is the "MUST NOT change
// existing behaviour when unused" half of the row, measured: a queue of
// deliveries that name no priority (and of deliveries that name an explicit
// default, which resolves to the same stored value) reads back exactly FIFO,
// across a batch edge.
func TestMemoryStore_RetrieveWithoutPriorityIsUnchangedFIFO(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	deliverPriority(t, store, "agent-1", "m1", nil)
	deliverPriority(t, store, "agent-1", "m2", intPtr(MinMessagePriority))
	deliverPriority(t, store, "agent-1", "m3", nil)
	deliverPriority(t, store, "agent-1", "m4", nil)

	first, lease, err := store.Retrieve("agent-1", 30*time.Second, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"m1", "m2"}, idsOf(first))
	require.NoError(t, store.Ack("agent-1", lease, []string{"m1", "m2"}))

	second, _, err := store.Retrieve("agent-1", 30*time.Second, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"m3", "m4"}, idsOf(second))
}

// ---- priority: the HTTP edge ------------------------------------------------

// priorityRouter mounts the three inbox routes this file drives, exactly as
// cmd/server registers them.
func priorityRouter(h *Handler) *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)
	return r
}

func priorityCall(t *testing.T, r *mux.Router, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestHandleDeliver_PriorityOutOfRangeIsRejectedNotClamped pins the documented
// range (docs/openapi.yaml: minimum 0, maximum 9) at the boundary: a value
// outside it is a 400 naming the range, and nothing is stored — a clamp would
// silently retrieve the message at a priority the caller never asked for.
func TestHandleDeliver_PriorityOutOfRangeIsRejectedNotClamped(t *testing.T) {
	store := setupTestStore(t)
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))
	h := NewHandler(store)
	r := priorityRouter(h)

	for _, tc := range []struct {
		name     string
		priority string
	}{
		{"negative", "-1"},
		{"above the maximum", "10"},
		{"far above the maximum", "1000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := priorityCall(t, r, http.MethodPost, "/agents/agent-1/inbox",
				`{"payload":{"n":1},"priority":`+tc.priority+`}`)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "priority must be 0..9")

			// Nothing was stored: the refusal is a refusal, not a partial accept.
			msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
			require.NoError(t, err)
			require.Empty(t, msgs)
		})
	}

	// Both ends of the documented range are honored, not rejected.
	for _, inRange := range []int{MinMessagePriority, MaxMessagePriority} {
		rec := priorityCall(t, r, http.MethodPost, "/agents/agent-1/inbox",
			`{"payload":{"n":1},"priority":`+strconv.Itoa(inRange)+`}`)
		require.Equal(t, http.StatusCreated, rec.Code, "priority %d is inside 0..9: %s", inRange, rec.Body.String())
	}
}

// TestHandleRetrieve_PrioritylessMessageKeepsTheOldWireShape is the wire half of
// "absent priority = today's behaviour": a delivery that names no priority
// serialises without a `priority` key at all, so a pre-CR-FEAT-035 client parses
// the same bytes it always did; a delivery that names one carries it.
func TestHandleRetrieve_PrioritylessMessageKeepsTheOldWireShape(t *testing.T) {
	store := setupTestStore(t)
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))
	h := NewHandler(store)
	r := priorityRouter(h)

	require.Equal(t, http.StatusCreated,
		priorityCall(t, r, http.MethodPost, "/agents/agent-1/inbox", `{"payload":{"plain":true}}`).Code)

	rec := priorityCall(t, r, http.MethodGet, "/agents/agent-1/inbox", "")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.NotContains(t, body, `"priority"`,
		"a priority-less message must not grow a priority key: %s", body)

	var decoded struct {
		Messages []map[string]any `json:"messages"`
		LeaseID  string           `json:"lease_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &decoded))
	require.Len(t, decoded.Messages, 1)
	require.NotContains(t, decoded.Messages[0], "priority")

	// Now the same inbox with an explicit priority: it IS on the wire, so a
	// consumer can see the ordering it is being handed.
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`), Priority: 7}))
	rec = priorityCall(t, r, http.MethodGet, "/agents/agent-1/inbox?max=10", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"priority":7`)
}

// ---- the global ingest budget ----------------------------------------------

const (
	// shedErrorCode is the NAMED error a shed delivery must carry. It is
	// asserted as a literal (not read from the package constant) so a rename
	// that skips the documented contract fails here.
	shedErrorCode = "RATE_LIMITED_GLOBAL"
)

// deliverOne posts one well-formed delivery and returns the recorder.
func deliverOne(t *testing.T, r *mux.Router, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	return priorityCall(t, r, http.MethodPost, "/agents/"+agentID+"/inbox", `{"payload":{"n":1}}`)
}

// TestGlobalRateLimitDisabledByDefaultFreesEveryDelivery pins the default: with
// no budget configured, nothing on the delivery path is shed, however many
// deliveries arrive — the pre-CR-FEAT-035 behaviour, measured rather than
// asserted in prose.
func TestGlobalRateLimitDisabledByDefaultFreesEveryDelivery(t *testing.T) {
	store := setupTestStore(t)
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))
	h := NewHandler(store)
	require.Zero(t, h.GlobalRateLimit(), "the default budget is 0 = none")
	r := priorityRouter(h)

	for i := 0; i < 50; i++ {
		require.Equal(t, http.StatusCreated, deliverOne(t, r, "agent-1").Code,
			"delivery %d must not be shed with no budget configured", i)
	}
	depth := queueDepthOf(t, store)
	require.Equal(t, 50, depth.Pending)
}

// TestGlobalRateLimitShedsWithNamedErrorAndRetryAfter is the row's acceptance
// half: once the budget is spent, the next delivery is refused 429 with a
// NAMED error body, a `Retry-After` header, and it reaches no store at all.
func TestGlobalRateLimitShedsWithNamedErrorAndRetryAfter(t *testing.T) {
	store := setupTestStore(t)
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))
	h := NewHandler(store)
	h.SetGlobalRateLimit(2)
	require.Equal(t, 2, h.GlobalRateLimit())
	r := priorityRouter(h)

	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusCreated, deliverOne(t, r, "agent-1").Code)
	}

	rec := deliverOne(t, r, "agent-1")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	// The header is what the review found missing on the existing 429.
	retryAfter := rec.Header().Get("Retry-After")
	require.NotEmpty(t, retryAfter, "a 429 must tell the caller how long to wait")
	secs, err := strconv.Atoi(retryAfter)
	require.NoError(t, err, "Retry-After must be an integer number of seconds, got %q", retryAfter)
	require.GreaterOrEqual(t, secs, 1, "Retry-After of 0 would read as 'retry immediately'")

	var body struct {
		Error          string `json:"error"`
		Scope          string `json:"scope"`
		LimitPerMinute int    `json:"limit_per_minute"`
		RetryAfterS    int    `json:"retry_after_s"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, shedErrorCode, body.Error, "the shed must be NAMED, not prose")
	require.Equal(t, "global", body.Scope)
	require.Equal(t, 2, body.LimitPerMinute)
	require.Equal(t, secs, body.RetryAfterS,
		"the body's retry_after_s and the header must report the same wait")

	// The shed happened BEFORE the store write: a refused delivery left no
	// trace in the queue it exists to protect.
	depth := queueDepthOf(t, store)
	require.Equal(t, 2, depth.Pending, "only the two accepted deliveries are queued")
}

// TestGlobalRateLimitShedsAcrossEveryProducer pins "global": the budget is not
// per agent, so a second producer arriving at a spent budget is refused too —
// which is exactly what a per-agent cap alone could never do.
func TestGlobalRateLimitShedsAcrossEveryProducer(t *testing.T) {
	store := setupTestStore(t)
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))
	require.NoError(t, store.Register(&Agent{ID: "agent-2"}))
	h := NewHandler(store)
	h.SetGlobalRateLimit(1)
	r := priorityRouter(h)

	require.Equal(t, http.StatusCreated, deliverOne(t, r, "agent-1").Code)
	require.Equal(t, http.StatusTooManyRequests, deliverOne(t, r, "agent-2").Code,
		"a spent GLOBAL budget refuses a different agent too")
}

// ---- queue depth -----------------------------------------------------------

// TestMemoryStore_QueueDepthCountsPendingAndLeased pins the store-wide
// measurement against the per-agent counters it must agree with.
func TestMemoryStore_QueueDepthCountsPendingAndLeased(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))
	require.NoError(t, store.Register(&Agent{ID: "agent-2"}))

	empty := queueDepthOf(t, store)
	require.Equal(t, QueueDepth{}, empty, "no messages anywhere is a zero depth, not an absence")

	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{"n":1}`)}))
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{"n":2}`)}))
	require.NoError(t, store.Deliver("agent-2", &InboxEntry{Payload: json.RawMessage(`{"n":3}`)}))

	depth := queueDepthOf(t, store)
	require.Equal(t, 3, depth.Pending, "depth is store-wide: both inboxes count")
	require.Equal(t, 0, depth.Leased)
	require.Positive(t, depth.OldestAge)

	// A leased message is still queued — it is held, not delivered.
	batch, lease, err := store.Retrieve("agent-1", 30*time.Second, 1)
	require.NoError(t, err)
	require.Len(t, batch, 1)
	require.NotEmpty(t, lease)

	depth = queueDepthOf(t, store)
	require.Equal(t, 3, depth.Pending)
	require.Equal(t, 1, depth.Leased)

	// Acknowledging one removes it from both counts.
	require.NoError(t, store.Ack("agent-1", lease, []string{batch[0].ID}))
	depth = queueDepthOf(t, store)
	require.Equal(t, 2, depth.Pending)
	require.Equal(t, 0, depth.Leased)
}

// TestMemoryStore_QueueDepthSkipsExpiredButCountsNeverExpiring pins the two
// expiry rules the per-agent Stats already applies, so the store-wide number is
// the SUM of the per-agent ones rather than a second definition of "queued".
func TestMemoryStore_QueueDepthSkipsExpiredButCountsNeverExpiring(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))

	never := 0
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{
		Payload: json.RawMessage(`{"n":1}`), TTLSeconds: &never,
	}))
	// Already expired on arrival: CreatedAt in the past with a 1s lifetime.
	past := time.Now().Add(-time.Hour)
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{
		ID: "expired", Payload: json.RawMessage(`{"n":2}`), CreatedAt: past, ExpiresAt: past.Add(time.Second),
	}))

	depth := queueDepthOf(t, store)
	require.Equal(t, 1, depth.Pending, "an expired message is not queued; a never-expiring one is")

	statsDepth, _, _, err := store.Stats("agent-1")
	require.NoError(t, err)
	require.Equal(t, statsDepth, depth.Pending, "store-wide depth is the sum of the per-agent depth")
}
