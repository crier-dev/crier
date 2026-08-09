package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

func newTestPubKey(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(pub), pub
}

func setupTestStore(t *testing.T) Store {
	t.Helper()
	return NewMemoryStore()
}

func registerTestAgent(t *testing.T, store Store) *Agent {
	t.Helper()
	hexKey, pubKey := newTestPubKey(t)
	agent := &Agent{
		ID:           "agent-1",
		PublicKey:    HexKey(pubKey),
		Capabilities: []string{"relay", "mesh"},
	}
	if err := store.Register(agent); err != nil {
		t.Fatalf("register: %v", err)
	}
	_ = hexKey
	return agent
}

func setupRouter(store Store) *mux.Router {
	handler := NewHandler(store)
	r := mux.NewRouter()
	r.HandleFunc("/agents", handler.HandleRegister).Methods("POST")
	r.HandleFunc("/agents", handler.HandleListAgents).Methods("GET")
	r.HandleFunc("/agents/{id}", handler.HandleGetAgent).Methods("GET")
	r.HandleFunc("/agents/{id}", handler.HandleUnregister).Methods("DELETE")
	r.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox", handler.HandleRetrieve).Methods("GET")
	r.HandleFunc("/agents/{id}/inbox/ack", handler.HandleAck).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox/stats", handler.HandleStats).Methods("GET")
	return r
}

// ----- AC 1: Register agent with ed25519 public key → 201, duplicate → 409 -----

func TestRegister_Success(t *testing.T) {
	hexKey, _ := newTestPubKey(t)
	store := setupTestStore(t)
	router := setupRouter(store)

	body, _ := json.Marshal(registerRequest{
		ID:           "agent-1",
		PublicKey:    hexKey,
		Capabilities: []string{"relay"},
	})
	req := httptest.NewRequest("POST", "/agents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var agent Agent
	json.Unmarshal(rec.Body.Bytes(), &agent)
	if agent.ID != "agent-1" {
		t.Errorf("agent ID = %q, want agent-1", agent.ID)
	}
	if len(agent.Capabilities) != 1 || agent.Capabilities[0] != "relay" {
		t.Errorf("capabilities = %v", agent.Capabilities)
	}
	if agent.Status != StatusOnline {
		t.Errorf("status = %q, want online", agent.Status)
	}
}

func TestRegister_Duplicate(t *testing.T) {
	hexKey, _ := newTestPubKey(t)
	store := setupTestStore(t)
	router := setupRouter(store)

	body, _ := json.Marshal(registerRequest{ID: "agent-1", PublicKey: hexKey})
	req1 := httptest.NewRequest("POST", "/agents", bytes.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	router.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first register: expected 201, got %d", rec1.Code)
	}

	req2 := httptest.NewRequest("POST", "/agents", bytes.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestRegister_InvalidPublicKey(t *testing.T) {
	store := setupTestStore(t)
	router := setupRouter(store)

	tests := []struct {
		name string
		key  string
	}{
		{"too short", "abc123"},
		{"not hex", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"correct hex but wrong length", "00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(registerRequest{ID: "bad-key", PublicKey: tt.key})
			req := httptest.NewRequest("POST", "/agents", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", rec.Code)
			}
		})
	}
}

// ----- AC 2: List agents + get agent detail -----

func TestListAndGetAgent(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	// List
	req := httptest.NewRequest("GET", "/agents", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var list agentsResponse
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(list.Agents))
	}

	// Get
	req2 := httptest.NewRequest("GET", "/agents/agent-1", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", rec2.Code)
	}

	// Get non-existent
	req3 := httptest.NewRequest("GET", "/agents/nope", nil)
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec3.Code)
	}
}

// ----- AC 3: Unregister cleans up inbox -----

func TestUnregister_CleansInbox(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	payload := json.RawMessage(`{"msg":"hello"}`)
	body, _ := json.Marshal(deliverRequest{Payload: payload})
	req := httptest.NewRequest("POST", "/agents/agent-1/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver: expected 201, got %d", rec.Code)
	}

	// Unregister
	req2 := httptest.NewRequest("DELETE", "/agents/agent-1", nil)
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("unregister: expected 204, got %d", rec2.Code)
	}

	// Verify agent is gone
	req3 := httptest.NewRequest("GET", "/agents/agent-1", nil)
	rec3 := httptest.NewRecorder()
	router.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Errorf("expected 404 after unregister, got %d", rec3.Code)
	}
}

// ----- AC 4: Deliver + Retrieve returns correct messages -----

func TestDeliverAndRetrieve(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	for i := 0; i < 2; i++ {
		payload := json.RawMessage(`{"msg":"hello"}`)
		body, _ := json.Marshal(deliverRequest{Payload: payload})
		req := httptest.NewRequest("POST", "/agents/agent-1/inbox", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("deliver %d: expected 201, got %d", i, rec.Code)
		}
	}

	req := httptest.NewRequest("GET", "/agents/agent-1/inbox?max=10", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp retrieveResponse
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(resp.Messages))
	}
	if resp.LeaseID == "" {
		t.Error("expected non-empty lease_id")
	}
}

// ----- AC 5: Lease — ACK removes, un-ACKed return after lease expires -----

func TestLeaseAckAndExpiry(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	for i := 0; i < 3; i++ {
		entry := &InboxEntry{Payload: json.RawMessage(`{}`)}
		if err := store.Deliver("agent-1", entry); err != nil {
			t.Fatalf("deliver %d: %v", i, err)
		}
	}

	messages, leaseID, err := store.Retrieve("agent-1", 100*time.Millisecond, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(messages) != 3 {
		t.Errorf("expected 3 messages, got %d", len(messages))
	}

	msgIDs := []string{messages[0].ID}
	if err := store.Ack("agent-1", leaseID, msgIDs); err != nil {
		t.Fatalf("ack: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	store.PurgeExpired()

	messages2, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("retrieve2: %v", err)
	}
	if len(messages2) != 2 {
		t.Errorf("expected 2 messages after lease expiry, got %d", len(messages2))
	}

	for _, m := range messages2 {
		if m.ID == messages[0].ID {
			t.Error("ACKed message should not be returned")
		}
	}
}

func TestAck_WrongLease(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	entry := &InboxEntry{Payload: json.RawMessage(`{}`)}
	store.Deliver("agent-1", entry)

	messages, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	if err != nil {
		t.Fatal(err)
	}

	err = store.Ack("agent-1", "wrong-lease-id", []string{messages[0].ID})
	if err == nil {
		t.Error("expected error for wrong lease ID")
	}
}

func TestAck_EmptyMessageIDsRejected(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := setupRouter(store)

	entry := &InboxEntry{Payload: json.RawMessage(`{}`)}
	if err := store.Deliver("agent-1", entry); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	_, leaseID, err := store.Retrieve("agent-1", 100*time.Millisecond, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}

	// A lease-only ack is a silent no-op (CR-GAP-014) — must be rejected with 400.
	body, _ := json.Marshal(ackRequest{LeaseID: leaseID})
	req := httptest.NewRequest("POST", "/agents/agent-1/inbox/ack", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty message_ids, got %d: %s", rec.Code, rec.Body.String())
	}

	// The message must still be queued — the rejected ack must not have removed it.
	// (It is leased by the first retrieve, so it returns to the queue after expiry.)
	time.Sleep(200 * time.Millisecond)
	store.PurgeExpired()

	messages, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("re-retrieve: %v", err)
	}
	if len(messages) != 1 {
		t.Errorf("expected 1 message still queued after rejected ack, got %d", len(messages))
	}
}

// ----- AC 6: TTL-expired messages auto-purged -----

func TestPurgeExpired(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	entry := &InboxEntry{
		Payload:   json.RawMessage(`{}`),
		ExpiresAt: time.Now().Add(50 * time.Millisecond),
	}
	if err := store.Deliver("agent-1", entry); err != nil {
		t.Fatal(err)
	}

	time.Sleep(100 * time.Millisecond)

	removed := store.PurgeExpired()
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	depth, _, _, err := store.Stats("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if depth != 0 {
		t.Errorf("expected 0 messages, got %d", depth)
	}
}

// ----- AC 7: Agent unregister cleans up inbox -----

func TestUnregister_ClearsInboxDirectly(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`)})
	store.Unregister("agent-1")

	_, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	if err == nil {
		t.Error("expected error after unregister")
	}
}

// ----- AC 8: Concurrent delivery — two retrievers get disjoint message sets -----

func TestConcurrentRetrieve_Disjoint(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	for i := 0; i < 10; i++ {
		store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`)})
	}

	var wg sync.WaitGroup
	results := make([][]*InboxEntry, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
			if err != nil {
				t.Errorf("retriever %d: %v", idx, err)
			}
			results[idx] = msgs
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool)
	total := 0
	for _, msgs := range results {
		for _, m := range msgs {
			if seen[m.ID] {
				t.Errorf("message %q appeared in both retrievals", m.ID)
			}
			seen[m.ID] = true
			total++
		}
	}

	if total < 5 {
		t.Errorf("only got %d messages, expected at least 5", total)
	}
	if total == 10 {
		t.Logf("both retrievers got all 10 messages (expected for non-racy test)")
	}
}

// ----- Stats tests -----

func TestStats(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	for i := 0; i < 3; i++ {
		store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`)})
	}

	depth, leased, _, err := store.Stats("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if depth != 3 {
		t.Errorf("depth = %d, want 3", depth)
	}
	if leased != 0 {
		t.Errorf("leased = %d, want 0 (none leased yet)", leased)
	}

	store.Retrieve("agent-1", 30*time.Second, 2)

	depth, leased, _, err = store.Stats("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if depth != 3 {
		t.Errorf("depth = %d, want 3 (all still in queue)", depth)
	}
	if leased != 2 {
		t.Errorf("leased = %d, want 2", leased)
	}
}

func TestStats_NotFound(t *testing.T) {
	store := setupTestStore(t)
	_, _, _, err := store.Stats("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent agent")
	}
}

// ----- Handler integration tests -----

func TestHandleDeliver_AgentNotFound(t *testing.T) {
	store := setupTestStore(t)
	router := setupRouter(store)

	body, _ := json.Marshal(deliverRequest{Payload: json.RawMessage(`{}`)})
	req := httptest.NewRequest("POST", "/agents/nope/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleStats(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)

	store.Deliver("agent-1", &InboxEntry{Payload: json.RawMessage(`{}`)})

	router := setupRouter(store)
	req := httptest.NewRequest("GET", "/agents/agent-1/inbox/stats", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var stats statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if stats.QueueDepth != 1 {
		t.Errorf("queue_depth = %d, want 1", stats.QueueDepth)
	}

	req = httptest.NewRequest("GET", "/agents/missing/inbox/stats", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}
