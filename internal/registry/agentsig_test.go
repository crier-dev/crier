package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// testAgentKeypair generates a keypair and registers an agent with it.
func testAgentKeypair(t *testing.T, store Store, id string) (pubHex string, priv ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubHex = hex.EncodeToString(pub)
	if err := store.Register(&Agent{ID: id, PublicKey: HexKey(pub), Capabilities: []string{}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return pubHex, priv
}

// signedRequest builds an HTTP request with valid agent signature headers.
func signedRequest(t *testing.T, priv ed25519.PrivateKey, agentID, method, path string) *http.Request {
	t.Helper()
	ts := fmt.Sprintf("%d", time.Now().Unix())
	payload := []byte(method + "\n" + path + "\n" + ts)
	sig := ed25519.Sign(priv, payload)

	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(HeaderAgentID, agentID)
	req.Header.Set(HeaderAgentTS, ts)
	req.Header.Set(HeaderAgentSig, hex.EncodeToString(sig))
	return req
}

// withURLVar sets the gorilla/mux {id} variable on a request, mirroring what
// the router does before invoking a handler.
func withURLVar(req *http.Request, id string) *http.Request {
	return mux.SetURLVars(req, map[string]string{"id": id})
}

func newSigHandler(store Store) *Handler {
	h := NewHandler(store)
	h.SetRequireAgentSig(true)
	return h
}

func TestAuthorizeAgent_ValidSignature(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, priv := testAgentKeypair(t, store, "agent-1")

	req := signedRequest(t, priv, "agent-1", http.MethodGet, "/agents/agent-1/inbox")
	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, "agent-1"); err != nil {
		t.Fatalf("expected authorization, got %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAuthorizeAgent_MissingHeaders(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, _ = testAgentKeypair(t, store, "agent-1")

	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/inbox", nil)
	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, "agent-1"); err == nil {
		t.Fatal("expected error for missing headers")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "missing agent signature headers") {
		t.Fatalf("body = %q, want missing headers message", rec.Body.String())
	}
}

func TestAuthorizeAgent_CrossAgentRejected(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, privA := testAgentKeypair(t, store, "agent-a")
	_, _ = testAgentKeypair(t, store, "agent-b")

	// agent-a tries to read agent-b's inbox → 403
	req := signedRequest(t, privA, "agent-a", http.MethodGet, "/agents/agent-b/inbox")
	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, "agent-b"); err == nil {
		t.Fatal("expected cross-agent access to be rejected")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "own resources") {
		t.Fatalf("body = %q, want own-resources message", rec.Body.String())
	}
}

func TestAuthorizeAgent_WrongKeyRejected(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, _ = testAgentKeypair(t, store, "agent-1")
	_, attackerPriv, _ := ed25519.GenerateKey(rand.Reader)

	// attacker signs as agent-1 with an unrelated key → 401
	req := signedRequest(t, attackerPriv, "agent-1", http.MethodGet, "/agents/agent-1/inbox")
	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, "agent-1"); err == nil {
		t.Fatal("expected wrong-key signature to be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAuthorizeAgent_StaleTimestampRejected(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, priv := testAgentKeypair(t, store, "agent-1")

	// Sign with a timestamp 10 minutes in the past → replay → 401
	ts := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	payload := []byte(http.MethodGet + "\n/agents/agent-1/inbox\n" + ts)
	sig := ed25519.Sign(priv, payload)

	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/inbox", nil)
	req.Header.Set(HeaderAgentID, "agent-1")
	req.Header.Set(HeaderAgentTS, ts)
	req.Header.Set(HeaderAgentSig, hex.EncodeToString(sig))

	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, "agent-1"); err == nil {
		t.Fatal("expected stale timestamp to be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "timestamp") {
		t.Fatalf("body = %q, want timestamp message", rec.Body.String())
	}
}

func TestAuthorizeAgent_UnknownAgentNotFound(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, priv := testAgentKeypair(t, store, "agent-1")

	// agent-1 tries to access an unregistered agent → 404 (no existence leak)
	req := signedRequest(t, priv, "agent-1", http.MethodGet, "/agents/ghost/inbox")
	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, "ghost"); err == nil {
		t.Fatal("expected unknown agent to be rejected")
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandleRetrieve_RequiresSignature(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, _ = testAgentKeypair(t, store, "agent-1")

	// Deliver a message, then attempt unsigned retrieve → 401
	if err := store.Deliver("agent-1", &InboxEntry{ID: "m1", Payload: json.RawMessage(`{"x":1}`)}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/inbox?max=10&lease=30", nil)
	req = withURLVar(req, "agent-1")
	rec := httptest.NewRecorder()
	h.HandleRetrieve(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleRetrieve_SignedSucceeds(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, priv := testAgentKeypair(t, store, "agent-1")

	if err := store.Deliver("agent-1", &InboxEntry{ID: "m1", Payload: json.RawMessage(`{"x":1}`)}); err != nil {
		t.Fatal(err)
	}

	req := signedRequest(t, priv, "agent-1", http.MethodGet, "/agents/agent-1/inbox")
	req = withURLVar(req, "agent-1")
	q := req.URL.Query()
	q.Set("max", "10")
	q.Set("lease", "30")
	req.URL.RawQuery = q.Encode()

	rec := httptest.NewRecorder()
	h.HandleRetrieve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp retrieveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(resp.Messages))
	}
}

func TestHandleAck_SignedSucceeds_UnsignedFails(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, priv := testAgentKeypair(t, store, "agent-1")

	if err := store.Deliver("agent-1", &InboxEntry{ID: "m1", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	_, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 10)
	if err != nil {
		t.Fatal(err)
	}

	// Unsigned ack → 401
	body := strings.NewReader(fmt.Sprintf(`{"lease_id":%q,"message_ids":["m1"]}`, leaseID))
	req := httptest.NewRequest(http.MethodPost, "/agents/agent-1/inbox/ack", body)
	req = withURLVar(req, "agent-1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleAck(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned ack status = %d, want 401", rec.Code)
	}

	// Signed ack → 204
	body2 := strings.NewReader(fmt.Sprintf(`{"lease_id":%q,"message_ids":["m1"]}`, leaseID))
	req2 := signedRequest(t, priv, "agent-1", http.MethodPost, "/agents/agent-1/inbox/ack")
	req2 = withURLVar(req2, "agent-1")
	req2 = httptest.NewRequest(http.MethodPost, "/agents/agent-1/inbox/ack", body2)
	req2 = withURLVar(req2, "agent-1")
	req2.Header.Set(HeaderAgentID, "agent-1")
	req2.Header.Set(HeaderAgentTS, fmt.Sprintf("%d", time.Now().Unix()))
	sig := ed25519.Sign(priv, []byte(http.MethodPost+"\n/agents/agent-1/inbox/ack\n"+req2.Header.Get(HeaderAgentTS)))
	req2.Header.Set(HeaderAgentSig, hex.EncodeToString(sig))

	rec2 := httptest.NewRecorder()
	h.HandleAck(rec2, req2)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("signed ack status = %d, want 204 (body: %s)", rec2.Code, rec2.Body.String())
	}
}

func TestHandleStats_RequiresSignature(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, _ = testAgentKeypair(t, store, "agent-1")

	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/inbox/stats", nil)
	req = withURLVar(req, "agent-1")
	rec := httptest.NewRecorder()
	h.HandleStats(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleUnregister_RequiresSignature(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, _ = testAgentKeypair(t, store, "agent-1")

	req := httptest.NewRequest(http.MethodDelete, "/agents/agent-1", nil)
	req = withURLVar(req, "agent-1")
	rec := httptest.NewRecorder()
	h.HandleUnregister(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandleUnregister_SignedSucceeds(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, priv := testAgentKeypair(t, store, "agent-1")

	req := signedRequest(t, priv, "agent-1", http.MethodDelete, "/agents/agent-1")
	req = withURLVar(req, "agent-1")
	rec := httptest.NewRecorder()
	h.HandleUnregister(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, err := store.Get("agent-1"); err == nil {
		t.Fatal("agent should be unregistered")
	}
}

func TestRequireAgent_DisabledByDefault(t *testing.T) {
	// Legacy behavior: Handler without SetRequireAgentSig(true) allows access.
	store := NewMemoryStore()
	h := NewHandler(store)
	_, _ = testAgentKeypair(t, store, "agent-1")

	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/inbox?max=10&lease=30", nil)
	req = withURLVar(req, "agent-1")
	rec := httptest.NewRecorder()
	h.HandleRetrieve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (legacy default)", rec.Code)
	}
}
