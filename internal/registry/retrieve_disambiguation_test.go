package registry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// retrieve_disambiguation_test.go pins the retrieve disambiguation contract
// (DF-CRIER-177): a 200 whose `messages` array is empty is ambiguous between
// "nothing queued" and "everything queued is leased (held by another
// retriever)". The response body must carry queue_depth and leased_count —
// the same numbers GET /agents/{id}/inbox/stats reports — so the two states
// are distinguishable on the retrieve wire itself, without a second call.
//
// It also pins the spec'd query spellings (DF-CRIER-180 family):
// docs/openapi.yaml documents `limit` and `lease_seconds`; the handler
// historically read only `max` and `lease`. All four must work.

// retrieveBody is the decoded wire shape of GET /agents/{id}/inbox.
type retrieveBody struct {
	Messages    []*InboxEntry `json:"messages"`
	LeaseID     string        `json:"lease_id"`
	QueueDepth  int           `json:"queue_depth"`
	LeasedCount int           `json:"leased_count"`
}

// deliverN delivers n messages to agent-1 through the router's deliver
// handler (the wire path, so the store sees exactly what production sees).
func deliverN(t *testing.T, router http.Handler, id string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		body, err := json.Marshal(deliverRequest{Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
		require.NoError(t, err)
		rec := doInboxRequest(t, router, http.MethodPost, "/agents/"+id+"/inbox", string(body))
		require.Equal(t, http.StatusCreated, rec.Code, "deliver %d: %s", i, rec.Body.String())
	}
}

func getRetrieve(t *testing.T, router http.Handler, target string) (retrieveBody, *httptest.ResponseRecorder) {
	t.Helper()
	rec := doInboxRequest(t, router, http.MethodGet, target, "")
	require.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", target, rec.Body.String())
	var body retrieveBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "body: %s", rec.Body.String())
	return body, rec
}

func getStats(t *testing.T, router http.Handler, target string) statsResponse {
	t.Helper()
	rec := doInboxRequest(t, router, http.MethodGet, target, "")
	require.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", target, rec.Body.String())
	var stats statsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stats), "body: %s", rec.Body.String())
	return stats
}

// (a) A second retrieve while everything is leased must NOT look like a
// genuinely empty inbox: messages is empty in both, but queue_depth and
// leased_count separate HELD from NOTHING-QUEUED.
func TestRetrieveDisambiguation_AllLeasedVsEmptyInbox(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)
	require.NoError(t, store.Register(&Agent{ID: "agent-empty"}))
	deliverN(t, router, "agent-1", 3)

	first, _ := getRetrieve(t, router, "/agents/agent-1/inbox?max=10")
	require.Len(t, first.Messages, 3)
	require.NotEmpty(t, first.LeaseID)

	second, rec2 := getRetrieve(t, router, "/agents/agent-1/inbox?max=10")
	require.Empty(t, second.Messages, "everything is leased: nothing claimable")
	require.Empty(t, second.LeaseID, "no lease is minted when nothing was claimed")
	require.Greater(t, second.LeasedCount, 0, "all-leased second retrieve must report leased_count > 0, body: %s", rec2.Body.String())
	require.Greater(t, second.QueueDepth, 0, "all-leased second retrieve must report queue_depth > 0, body: %s", rec2.Body.String())

	// A never-delivered agent's body must differ: both counters zero.
	empty, emptyRec := getRetrieve(t, router, "/agents/agent-empty/inbox")
	require.Empty(t, empty.Messages)
	require.Empty(t, empty.LeaseID)
	require.Equal(t, 0, empty.QueueDepth, "genuinely empty inbox: queue_depth must be 0, body: %s", emptyRec.Body.String())
	require.Equal(t, 0, empty.LeasedCount, "genuinely empty inbox: leased_count must be 0, body: %s", emptyRec.Body.String())
	require.NotEqual(t, rec2.Body.String(), emptyRec.Body.String(),
		"an all-leased inbox must be wire-distinguishable from an empty one")
}

// (b) The counters on the retrieve response agree with the stats endpoint
// taken immediately after, for both the empty and the all-leased state.
func TestRetrieveDisambiguation_CountersMatchStats(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)
	deliverN(t, router, "agent-1", 2)

	body, _ := getRetrieve(t, router, "/agents/agent-1/inbox?max=10")
	require.Len(t, body.Messages, 2)

	stats := getStats(t, router, "/agents/agent-1/inbox/stats")
	require.Equal(t, stats.QueueDepth, body.QueueDepth,
		"retrieve queue_depth must equal stats queue_depth for the same agent")
	require.Equal(t, stats.LeasedCount, body.LeasedCount,
		"retrieve leased_count must equal stats leased_count for the same agent")
	require.Equal(t, 2, stats.QueueDepth)
	require.Equal(t, 2, stats.LeasedCount)

	// Same agreement after a second retrieve that claims nothing.
	second, _ := getRetrieve(t, router, "/agents/agent-1/inbox?max=10")
	require.Empty(t, second.Messages)
	stats2 := getStats(t, router, "/agents/agent-1/inbox/stats")
	require.Equal(t, stats2.QueueDepth, second.QueueDepth)
	require.Equal(t, stats2.LeasedCount, second.LeasedCount)
	require.Equal(t, 2, second.QueueDepth)
	require.Equal(t, 2, second.LeasedCount)
}

// (c) The documented spellings are honored: ?limit=N caps the batch and
// ?lease_seconds=N mints a lease that expires ~N seconds later (the message
// is claimable again by a later retrieve).
func TestRetrieveDisambiguation_DocumentedParamSpellings(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)
	deliverN(t, router, "agent-1", 3)

	// limit=1 -> exactly 1 message, a lease, 1 leased + 3 queued.
	body, _ := getRetrieve(t, router, "/agents/agent-1/inbox?limit=1")
	require.Len(t, body.Messages, 1, "?limit=1 must cap the batch at 1")
	require.NotEmpty(t, body.LeaseID)
	require.Equal(t, 3, body.QueueDepth)
	require.Equal(t, 1, body.LeasedCount)

	// lease_seconds=1 on the remaining two: claim both, wait past the
	// expiry, then a later retrieve must see them again.
	body2, _ := getRetrieve(t, router, "/agents/agent-1/inbox?lease_seconds=1")
	require.Len(t, body2.Messages, 2, "the 2 unleased messages are claimable")
	require.NotEmpty(t, body2.LeaseID)

	time.Sleep(1100 * time.Millisecond)

	body3, _ := getRetrieve(t, router, "/agents/agent-1/inbox?max=10")
	require.GreaterOrEqual(t, len(body3.Messages), 2,
		"expired lease_seconds=1 leases must release the messages for a later retrieve")
}

// (d) The historical spellings still behave as before (regression guard).
func TestRetrieveDisambiguation_LegacySpellingsStillWork(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)
	deliverN(t, router, "agent-1", 3)

	body, _ := getRetrieve(t, router, "/agents/agent-1/inbox?max=1")
	require.Len(t, body.Messages, 1, "?max=1 must cap the batch at 1")
	require.NotEmpty(t, body.LeaseID)

	// ?lease=1 -> 1-second lease; the message returns after expiry.
	body2, _ := getRetrieve(t, router, "/agents/agent-1/inbox?lease=1")
	require.Len(t, body2.Messages, 2)
	require.NotEmpty(t, body2.LeaseID)

	time.Sleep(1100 * time.Millisecond)

	body3, _ := getRetrieve(t, router, "/agents/agent-1/inbox?max=10")
	require.GreaterOrEqual(t, len(body3.Messages), 2, "?lease=1 leases must expire like lease_seconds")
}

// (e) The documented bound is enforced for BOTH spellings: >100 -> 400.
func TestRetrieveDisambiguation_LimitOverBoundIs400(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	for _, q := range []string{"?limit=101", "?max=101"} {
		rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox"+q, "")
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: body: %s", q, rec.Body.String())
		var errResp struct {
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		require.Equal(t, "max must be <= 100", errResp.Error, "%s: %s", q, rec.Body.String())
	}

	// Boundary values stay valid for both spellings.
	for _, q := range []string{"?limit=100", "?max=100"} {
		rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox"+q, "")
		require.Equal(t, http.StatusOK, rec.Code, "%s: body: %s", q, rec.Body.String())
	}
}
