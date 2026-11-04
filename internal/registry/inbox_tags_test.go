package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// inbox_tags_test.go — CRIER-TAGS §2.4 (CRIER-TAGS-1) at the inbox edge: the
// tags a message was CLASSIFIED with ride its route record, so a reader can ask
// "the build lane's messages" and get only those — and the two filter
// spellings cannot both be supplied.

// tagsRouter mounts the two inbox routes this file drives, exactly as
// cmd/server registers them.
func tagsRouter(h *Handler) *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
	return r
}

// deliverTagged stores one message with tags directly through the store.
func deliverTagged(t *testing.T, store Store, agentID, id string, tags ...string) {
	t.Helper()
	entry := &InboxEntry{ID: id, Payload: json.RawMessage(`{"n":1}`), Tags: tags}
	require.NoError(t, store.Deliver(agentID, entry))
}

// retrieveTags calls the inbox read with a raw query string.
func retrieveTags(t *testing.T, r *mux.Router, agentID, query string) (*httptest.ResponseRecorder, retrieveResponse) {
	t.Helper()
	path := "/agents/" + agentID + "/inbox"
	if query != "" {
		path += "?" + query
	}
	rec := priorityCall(t, r, http.MethodGet, path, "")
	var body retrieveResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	}
	return rec, body
}

// TestInboxTagFilter is §7.9: an entry created with tags is filtered by them,
// and an untagged message is never returned by a tag filter.
func TestInboxTagFilter(t *testing.T) {
	t.Parallel()
	store := setupTestStore(t)
	registerTestAgent(t, store)
	// Two agents, because Retrieve LEASES what it returns: a second filtered
	// read of the SAME inbox would be looking at messages the first call
	// already claimed (handler.go documents that cost).
	require.NoError(t, store.Register(&Agent{ID: "agent-2"}))
	r := tagsRouter(NewHandler(store))

	deliverTagged(t, store, "agent-1", "build-1", "build")
	deliverTagged(t, store, "agent-1", "urgent-1", "urgent")
	deliverTagged(t, store, "agent-1", "both-1", "build", "urgent")
	deliverTagged(t, store, "agent-1", "plain-1")

	rec, body := retrieveTags(t, r, "agent-1", "tag=build&max=10")
	require.Equal(t, http.StatusOK, rec.Code)
	ids := idsOf(body.Messages)
	require.ElementsMatch(t, []string{"build-1", "both-1"}, ids)
	for _, entry := range body.Messages {
		require.Contains(t, entry.Tags, "build", "the tags ride the entry the lease hands back")
	}

	// The OR form returns every message carrying any of the terms.
	deliverTagged(t, store, "agent-2", "build-2", "build")
	deliverTagged(t, store, "agent-2", "urgent-2", "urgent")
	deliverTagged(t, store, "agent-2", "both-2", "build", "urgent")
	deliverTagged(t, store, "agent-2", "plain-2")
	rec, body = retrieveTags(t, r, "agent-2", "tags_any=build,urgent")
	require.Equal(t, http.StatusOK, rec.Code)
	require.ElementsMatch(t, []string{"build-2", "urgent-2", "both-2"}, idsOf(body.Messages))
}

// TestInboxUnknownTagFilterIsEmpty pins §6: polling a category nothing carries
// is an empty 200, never a 404 and never an error.
func TestInboxUnknownTagFilterIsEmpty(t *testing.T) {
	t.Parallel()
	store := setupTestStore(t)
	registerTestAgent(t, store)
	r := tagsRouter(NewHandler(store))

	deliverTagged(t, store, "agent-1", "build-1", "build")

	rec, body := retrieveTags(t, r, "agent-1", "tag=nothing-yet")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, body.Messages)
	require.NotNil(t, body.Messages, "an empty result is an empty list, not null")
}

// TestInboxTagFilterAndAnyConflict is §5's TAGS_FILTER_CONFLICT at the inbox
// edge — and it is refused BEFORE anything is leased, so a bad filter cannot
// consume a message.
func TestInboxTagFilterAndAnyConflict(t *testing.T) {
	t.Parallel()
	store := setupTestStore(t)
	registerTestAgent(t, store)
	r := tagsRouter(NewHandler(store))

	deliverTagged(t, store, "agent-1", "build-1", "build")

	rec, _ := retrieveTags(t, r, "agent-1", "tag=build&tags_any=urgent")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var failure map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &failure))
	require.Equal(t, "TAGS_FILTER_CONFLICT", failure["error"])

	// Nothing was claimed by the refused request: the message is still
	// retrievable, with no lease minted on its behalf.
	entries, lease, err := store.Retrieve("agent-1", 30*1e9, 10)
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	require.NotEmpty(t, lease)
}

// TestInboxTagFilterRejectsBadGrammar pins that a malformed term is named, not
// silently ignored: a filter that skipped a bad term would answer a DIFFERENT
// question than the one asked.
func TestInboxTagFilterRejectsBadGrammar(t *testing.T) {
	t.Parallel()
	store := setupTestStore(t)
	registerTestAgent(t, store)
	r := tagsRouter(NewHandler(store))

	rec, _ := retrieveTags(t, r, "agent-1", "tag=Build")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	var failure map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &failure))
	require.Equal(t, "TAGS_INVALID", failure["error"])
	require.Contains(t, failure["detail"], "Build")
}

// TestInboxEntriesCarryTagsThroughSQLite proves the tags are PERSISTED on the
// route record, not only held in memory: the SQLite backend reads them back.
func TestInboxEntriesCarryTagsThroughSQLite(t *testing.T) {
	t.Parallel()
	store := newTestSQLiteStore(t)
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))
	handler := NewHandler(store)
	r := tagsRouter(handler)

	deliverTagged(t, store, "agent-1", "build-1", "build", "ci")
	deliverTagged(t, store, "agent-1", "plain-1")

	rec, body := retrieveTags(t, r, "agent-1", "tag=ci")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, body.Messages, 1)
	require.Equal(t, "build-1", body.Messages[0].ID)
	require.Equal(t, []string{"build", "ci"}, body.Messages[0].Tags)
}
