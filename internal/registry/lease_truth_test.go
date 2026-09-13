package registry

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

// lease_truth_test.go pins the lease contract at the observable edge
// (DF-CRIER-32):
//
//  1. a retrieval that claims zero messages returns a non-nil empty slice and
//     an EMPTY lease ID — no lease is minted, so a client can never hold a
//     "fresh-looking" lease that covers nothing;
//  2. a message ID that does not exist in the inbox is reported as
//     ErrMessageNotFound (HTTP 404) and is distinguishable from a message
//     that exists under a different lease (ErrLeaseConflict, HTTP 409).

// ---- MemoryStore -----------------------------------------------------------

func TestMemoryStore_RetrieveEmptyInboxMintsNoLease(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.Register(&Agent{ID: "agent-1"}))

	msgs, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.NotNil(t, msgs, "an empty retrieval must return a non-nil empty slice")
	require.Empty(t, msgs)
	require.Empty(t, leaseID, "an empty inbox must not mint a lease")
}

func TestMemoryStore_RetrieveAllLeasedMintsNoLease(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{"n":1}`)}))

	first, firstLease, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.NotEmpty(t, firstLease, "a non-empty retrieval keeps returning a lease")
	require.Equal(t, firstLease, first[0].LeaseID)

	second, secondLease, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.NotNil(t, second, "a fully leased inbox must return a non-nil empty slice")
	require.Empty(t, second)
	require.Empty(t, secondLease, "a retrieval that claims nothing must not mint a lease")

	// The second retrieve must not have disturbed the first lease: the caller
	// holding it can still ack its message.
	require.NoError(t, store.Ack("agent-1", firstLease, []string{first[0].ID}))
}

func TestMemoryStore_AckMissingIDReportsMessageNotFound(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`)}))
	msgs, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	err = store.Ack("agent-1", leaseID, []string{"does-not-exist"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMessageNotFound), "want ErrMessageNotFound, got %v", err)
	require.False(t, errors.Is(err, ErrLeaseConflict), "a missing ID is not a lease conflict: %v", err)
	require.Contains(t, err.Error(), "does-not-exist")

	// The real message is still there and still ackable under its own lease.
	require.NoError(t, store.Ack("agent-1", leaseID, []string{msgs[0].ID}))
}

func TestMemoryStore_AckWrongLeaseReportsLeaseConflict(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`)}))
	msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	err = store.Ack("agent-1", "not-the-real-lease", []string{msgs[0].ID})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrLeaseConflict), "want ErrLeaseConflict, got %v", err)
	require.False(t, errors.Is(err, ErrMessageNotFound), "an existing message is not 'not found': %v", err)
}

func TestMemoryStore_AckMissingIDWinsOverWrongLease(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`)}))
	_, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)

	// Neither the lease nor the ID exist: the missing ID is the actionable
	// answer (the client cannot fix a lease for an ID that was never delivered).
	err = store.Ack("agent-1", "bogus-lease", []string{"bogus-id"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMessageNotFound), "want ErrMessageNotFound, got %v", err)
}

// ---- HTTP wire -------------------------------------------------------------

func doInboxRequest(t *testing.T, router http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// decodeRetrieve asserts the 200 wire shape of a retrieve: `messages` is
// always a JSON array (never null) and lease_id is always present.
func decodeRetrieve(t *testing.T, rec *httptest.ResponseRecorder) ([]*InboxEntry, string) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"messages":`)
	require.NotContains(t, rec.Body.String(), `"messages":null`, "messages must serialize as [] not null")

	var resp struct {
		Messages []*InboxEntry `json:"messages"`
		LeaseID  string        `json:"lease_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Messages, resp.LeaseID
}

func TestHTTPRetrieve_EmptyInboxReturnsEmptyMessagesAndLease(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox", "")
	msgs, leaseID := decodeRetrieve(t, rec)
	require.Empty(t, msgs)
	require.Empty(t, leaseID, "no messages claimed means no lease on the wire")
	require.Contains(t, rec.Body.String(), `"messages":[],"lease_id":""`,
		"empty retrieval wire shape: %s", rec.Body.String())
}

func TestHTTPRetrieve_AllLeasedReturnsEmptyMessagesAndLease(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{"n":1}`)}))

	rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox?max=10", "")
	msgs, leaseID := decodeRetrieve(t, rec)
	require.Len(t, msgs, 1)
	require.NotEmpty(t, leaseID)

	// Everything is leased: the second caller gets nothing and — crucially —
	// no lease it could mistake for a usable one.
	rec2 := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox?max=10", "")
	msgs2, leaseID2 := decodeRetrieve(t, rec2)
	require.Empty(t, msgs2)
	require.Empty(t, leaseID2)
	require.Contains(t, rec2.Body.String(), `"messages":[],"lease_id":""`,
		"fully leased wire shape: %s", rec2.Body.String())
}

func TestHTTPAck_MissingIDIs404AndWrongLeaseIs409(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)
	require.NoError(t, store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`)}))
	msgs, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotEmpty(t, leaseID)
	realID := msgs[0].ID

	// (a) An ID that was never delivered, with the real lease → 404.
	body, err := json.Marshal(ackRequest{LeaseID: leaseID, MessageIDs: []string{"no-such-message"}})
	require.NoError(t, err)
	rec := doInboxRequest(t, router, http.MethodPost, "/agents/agent-1/inbox/ack", string(body))
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "no-such-message")

	// (b) A real ID under the wrong lease → 409.
	wrongBody, err := json.Marshal(ackRequest{LeaseID: "not-the-real-lease", MessageIDs: []string{realID}})
	require.NoError(t, err)
	rec2 := doInboxRequest(t, router, http.MethodPost, "/agents/agent-1/inbox/ack", string(wrongBody))
	require.Equal(t, http.StatusConflict, rec2.Code, "body: %s", rec2.Body.String())

	// (c) Neither the lease nor the ID exist → still 404 (the missing ID is
	// the actionable answer), never a lease conflict.
	bogusBody, err := json.Marshal(ackRequest{LeaseID: "bogus-lease", MessageIDs: []string{"bogus-id"}})
	require.NoError(t, err)
	rec3 := doInboxRequest(t, router, http.MethodPost, "/agents/agent-1/inbox/ack", string(bogusBody))
	require.Equal(t, http.StatusNotFound, rec3.Code, "body: %s", rec3.Body.String())

	// The message survived every rejected ack and is still ackable.
	require.NoError(t, store.Ack("agent-1", leaseID, []string{realID}))
}

// ---- RemoteStore (real server, real wire) ----------------------------------

func newRemoteAgentStore(t *testing.T, agentID string) *RemoteStore {
	t.Helper()
	srv, _ := newRemoteTestServer(t)
	rs := NewRemoteStore(srv.URL, agentID, "")
	_, pub := newTestPubKey(t)
	require.NoError(t, rs.Register(&Agent{ID: agentID, PublicKey: HexKey(pub)}))
	return rs
}

func TestRemoteStore_RetrieveEmptyInboxEmptyLease(t *testing.T) {
	rs := newRemoteAgentStore(t, "bob")

	msgs, leaseID, err := rs.Retrieve("bob", 30*time.Second, 10)
	require.NoError(t, err)
	require.NotNil(t, msgs)
	require.Empty(t, msgs)
	require.Empty(t, leaseID)
}

func TestRemoteStore_AckMissingIDDecodesMessageNotFound(t *testing.T) {
	rs := newRemoteAgentStore(t, "bob")

	err := rs.Ack("bob", "any-lease", []string{"never-delivered"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMessageNotFound), "want ErrMessageNotFound, got %v", err)
	require.False(t, errors.Is(err, ErrLeaseConflict), "404 must not decode as a lease conflict: %v", err)
}

func TestRemoteStore_AckWrongLeaseDecodesLeaseConflict(t *testing.T) {
	rs := newRemoteAgentStore(t, "bob")
	require.NoError(t, rs.Deliver("bob", &InboxEntry{Payload: json.RawMessage(`{"kind":"ping"}`)}))

	msgs, leaseID, err := rs.Retrieve("bob", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotEmpty(t, leaseID)

	err = rs.Ack("bob", "not-the-real-lease", []string{msgs[0].ID})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrLeaseConflict), "want ErrLeaseConflict, got %v", err)

	require.NoError(t, rs.Ack("bob", leaseID, []string{msgs[0].ID}))
}

// ---- PostgresStore (pgxmock) ----------------------------------------------

func TestPostgresStoreUnit_Retrieve_NoClaimMintsNoLease(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))
	// Nothing claimable: the locking select returns an empty result set.
	mock.ExpectQuery(`FOR UPDATE SKIP LOCKED`).
		WithArgs("agent", pgxmock.AnyArg(), 10).
		WillReturnRows(pgxmock.NewRows([]string{"id", "agent_id", "payload", "created_at", "expires_at"}))
	// Nothing was leased, so no UPDATE runs and the transaction is rolled back.
	mock.ExpectRollback()

	msgs, leaseID, err := s.Retrieve("agent", 30*time.Second, 10)
	require.NoError(t, err)
	require.NotNil(t, msgs)
	require.Empty(t, msgs)
	require.Empty(t, leaseID, "no claimable messages means no lease")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Ack_MissingIDMessageNotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))
	// The lookup finds none of the requested IDs.
	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("agent", []string{"gone"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "lease_id"}))
	mock.ExpectRollback()

	err := s.Ack("agent", "lease", []string{"gone"})
	require.True(t, errors.Is(err, ErrMessageNotFound), "want ErrMessageNotFound, got %v", err)
	require.False(t, errors.Is(err, ErrLeaseConflict), "want no lease-conflict sentinel: %v", err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Ack_WrongLeaseConflict(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))
	// The message exists but is leased under a different lease.
	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("agent", []string{"msg-1"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "lease_id"}).AddRow("msg-1", "other-lease"))
	mock.ExpectRollback()

	err := s.Ack("agent", "lease", []string{"msg-1"})
	require.True(t, errors.Is(err, ErrLeaseConflict), "want ErrLeaseConflict, got %v", err)
	require.False(t, errors.Is(err, ErrMessageNotFound), "an existing message is not 'not found': %v", err)
	require.NoError(t, mock.ExpectationsWereMet())
}
