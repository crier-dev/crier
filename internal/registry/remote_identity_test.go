package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// identityTestServer is a real registry handler (signatures ON, the secure
// default) plus counters for the two ungated bootstrap routes
// EnsureRegistered uses, so tests can assert the exact wire behaviour
// (a GET that finds the agent must NOT be followed by a POST).
type identityTestServer struct {
	srv       *httptest.Server
	store     *MemoryStore
	gets      int32
	posts     int32
	conflicts int32
}

func (s *identityTestServer) getCount() int  { return int(atomic.LoadInt32(&s.gets)) }
func (s *identityTestServer) postCount() int { return int(atomic.LoadInt32(&s.posts)) }

// newIdentityTestServer wires the same routes the server binary registers.
// postOverride, when non-nil, replaces the POST /agents handler (used to
// simulate a concurrent registrar forcing a 409).
func newIdentityTestServer(t *testing.T, postOverride http.HandlerFunc) *identityTestServer {
	t.Helper()
	its := &identityTestServer{store: NewMemoryStore()}
	h := NewHandler(its.store)
	h.SetRequireAgentSig(true)

	post := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&its.posts, 1)
		if postOverride != nil {
			postOverride(w, r)
			return
		}
		h.HandleRegister(w, r)
	}
	get := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&its.gets, 1)
		h.HandleGetAgent(w, r)
	}

	r := mux.NewRouter()
	r.HandleFunc("/agents", post).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}", get).Methods(http.MethodGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	its.srv = srv
	return its
}

// identityKey returns a deterministic-by-generation ed25519 keypair.
func identityKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

// storeKey inserts an agent straight into the store (simulating a
// registration that happened before this process started).
func storeKey(t *testing.T, store *MemoryStore, id string, pub ed25519.PublicKey) {
	t.Helper()
	if err := store.Register(&Agent{ID: id, PublicKey: HexKey(pub), Capabilities: []string{}}); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
}

// TestRemoteStore_EnsureRegistered_MissingRegisters is the headline case:
// a 404 on GET means the identity is absent, so EnsureRegistered POSTs the
// hex public key and reports Created.
func TestRemoteStore_EnsureRegistered_MissingRegisters(t *testing.T) {
	its := newIdentityTestServer(t, nil)
	// A signing key is configured: the bootstrap routes are ungated, but the
	// store still signs every request (as production does).
	_, priv := identityKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	rs := NewRemoteStore(its.srv.URL, "bridge-1", "", WithSigningKey(priv))

	res, err := rs.EnsureRegistered("bridge-1", pub, []string{"mcp"})
	if err != nil {
		t.Fatalf("EnsureRegistered: %v", err)
	}
	if !res.Created || res.Existed {
		t.Fatalf("result = %+v, want Created=true Existed=false", res)
	}
	if !res.KeyMatches {
		t.Fatalf("result = %+v, want KeyMatches=true on create", res)
	}
	if len(res.RegisteredPublicKey) != ed25519.PublicKeySize {
		t.Fatalf("RegisteredPublicKey len = %d", len(res.RegisteredPublicKey))
	}
	if its.getCount() != 1 || its.postCount() != 1 {
		t.Fatalf("wire = %d GET / %d POST, want 1/1", its.getCount(), its.postCount())
	}

	// The stored agent really carries the hex public key and the caps.
	got, err := its.store.Get("bridge-1")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if hex.EncodeToString(got.PublicKey) != hex.EncodeToString(pub) {
		t.Fatalf("stored key = %s, want %s", hex.EncodeToString(got.PublicKey), hex.EncodeToString(pub))
	}
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "mcp" {
		t.Fatalf("stored capabilities = %v", got.Capabilities)
	}

	// The public key on the wire is a hex STRING (HexKey marshalling), not a
	// base64 byte array — the exact regression the typed Register guards.
	raw, err := json.Marshal(&Agent{ID: "bridge-1", PublicKey: HexKey(pub)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"public_key":"`+hex.EncodeToString(pub)+`"`) {
		t.Fatalf("wire public_key is not the hex form: %s", raw)
	}
}

// TestRemoteStore_EnsureRegistered_ExistingSameKey: an identity already on
// the server with the same key is idempotent — no POST at all.
func TestRemoteStore_EnsureRegistered_ExistingSameKey(t *testing.T) {
	its := newIdentityTestServer(t, nil)
	pub, priv := identityKey(t)
	storeKey(t, its.store, "bridge-2", pub)
	rs := NewRemoteStore(its.srv.URL, "bridge-2", "", WithSigningKey(priv))

	res, err := rs.EnsureRegistered("bridge-2", pub, []string{"mcp"})
	if err != nil {
		t.Fatalf("EnsureRegistered: %v", err)
	}
	if res.Created || !res.Existed {
		t.Fatalf("result = %+v, want Created=false Existed=true", res)
	}
	if !res.KeyMatches {
		t.Fatalf("result = %+v, want KeyMatches=true", res)
	}
	if its.postCount() != 0 {
		t.Fatalf("POST count = %d, want 0 (no write for an already-registered identity)", its.postCount())
	}
	if hex.EncodeToString(res.RegisteredPublicKey) != hex.EncodeToString(pub) {
		t.Fatalf("RegisteredPublicKey = %s, want %s", hex.EncodeToString(res.RegisteredPublicKey), hex.EncodeToString(pub))
	}
}

// TestRemoteStore_EnsureRegistered_ExistingDifferentKey: the stale-key case.
// It must NOT error and must NOT overwrite the server's registration — the
// caller decides (and logs the remedy).
func TestRemoteStore_EnsureRegistered_ExistingDifferentKey(t *testing.T) {
	its := newIdentityTestServer(t, nil)
	stalePub, _ := identityKey(t)
	localPub, localPriv := identityKey(t)
	storeKey(t, its.store, "bridge-3", stalePub)
	rs := NewRemoteStore(its.srv.URL, "bridge-3", "", WithSigningKey(localPriv))

	res, err := rs.EnsureRegistered("bridge-3", localPub, []string{"mcp"})
	if err != nil {
		t.Fatalf("EnsureRegistered returned an error for a key mismatch: %v (want a report, not an error)", err)
	}
	if res.Created || !res.Existed {
		t.Fatalf("result = %+v, want Created=false Existed=true", res)
	}
	if res.KeyMatches {
		t.Fatalf("result = %+v, want KeyMatches=false", res)
	}
	if its.postCount() != 0 {
		t.Fatalf("POST count = %d, want 0 (must not overwrite a mismatched registration)", its.postCount())
	}
	if hex.EncodeToString(res.RegisteredPublicKey) != hex.EncodeToString(stalePub) {
		t.Fatalf("RegisteredPublicKey = %s, want the server's stale key %s",
			hex.EncodeToString(res.RegisteredPublicKey), hex.EncodeToString(stalePub))
	}
	// The server's registration is untouched.
	got, err := its.store.Get("bridge-3")
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if hex.EncodeToString(got.PublicKey) != hex.EncodeToString(stalePub) {
		t.Fatalf("stored key changed to %s", hex.EncodeToString(got.PublicKey))
	}
}

// TestRemoteStore_EnsureRegistered_Race409: the GET missed and the POST lost
// a race (409). EnsureRegistered re-GETs and reports what is actually there
// — Existed, no error.
func TestRemoteStore_EnsureRegistered_Race409(t *testing.T) {
	pub, priv := identityKey(t)
	var its *identityTestServer
	var raced int32
	its = newIdentityTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Simulate the concurrent registrar winning between our GET and our
		// POST: the identity appears server-side, and our POST loses with 409.
		atomic.AddInt32(&raced, 1)
		if err := its.store.Register(&Agent{ID: "bridge-4", PublicKey: HexKey(pub), Capabilities: []string{}}); err != nil {
			t.Errorf("seed racing registration: %v", err)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"agent already exists"}`))
	})
	rs := NewRemoteStore(its.srv.URL, "bridge-4", "", WithSigningKey(priv))

	res, err := rs.EnsureRegistered("bridge-4", pub, []string{"mcp"})
	if err != nil {
		t.Fatalf("EnsureRegistered after 409: %v", err)
	}
	if res.Created {
		t.Fatalf("result = %+v, want Created=false (we lost the race)", res)
	}
	if !res.Existed {
		t.Fatalf("result = %+v, want Existed=true from the re-GET", res)
	}
	if !res.KeyMatches {
		t.Fatalf("result = %+v, want KeyMatches=true (the winner registered our key)", res)
	}
	if atomic.LoadInt32(&raced) != 1 {
		t.Fatalf("409 injections = %d, want 1", raced)
	}
	if its.postCount() != 1 {
		t.Fatalf("POST count = %d, want 1 (no retry loop)", its.postCount())
	}
	if its.getCount() != 2 {
		t.Fatalf("GET count = %d, want 2 (initial 404 + re-GET after 409)", its.getCount())
	}
}

// TestRemoteStore_EnsureRegistered_Validation: local input errors are
// explicit and never touch the wire.
func TestRemoteStore_EnsureRegistered_Validation(t *testing.T) {
	its := newIdentityTestServer(t, nil)
	rs := NewRemoteStore(its.srv.URL, "bridge-5", "")

	if _, err := rs.EnsureRegistered("", make(ed25519.PublicKey, ed25519.PublicKeySize), nil); !errors.Is(err, ErrInvalidStoreInput) {
		t.Fatalf("empty id: err = %v, want ErrInvalidStoreInput", err)
	}
	if _, err := rs.EnsureRegistered("bridge-5", ed25519.PublicKey("short"), nil); !errors.Is(err, ErrInvalidStoreInput) {
		t.Fatalf("bad key: err = %v, want ErrInvalidStoreInput", err)
	}
	if its.getCount() != 0 || its.postCount() != 0 {
		t.Fatalf("wire = %d GET / %d POST, want 0/0 for local validation failures", its.getCount(), its.postCount())
	}
}

// TestRemoteStore_EnsureRegistered_TransportError: a server-side failure is
// returned as-is (not swallowed, not reported as a mismatch) so the caller
// can log the cause and keep starting up.
func TestRemoteStore_EnsureRegistered_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)

	_, priv := identityKey(t)
	rs := NewRemoteStore(srv.URL, "bridge-6", "", WithSigningKey(priv))
	_, err := rs.EnsureRegistered("bridge-6", priv.Public().(ed25519.PublicKey), nil)
	if err == nil {
		t.Fatal("EnsureRegistered: err = nil, want the 500 surfaced")
	}
	if errors.Is(err, ErrAgentNotFound) || errors.Is(err, ErrAgentExists) {
		t.Fatalf("err = %v, want a plain transport/status error", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the status in the message", err)
	}
}

// TestRemoteStore_EnsureRegistered_SignedServerRoundTrip is the end-to-end
// proof that self-registration actually unblocks the agent-owned leg: after
// EnsureRegistered on a signature-requiring server, a signed Retrieve of the
// bridge's own inbox succeeds. Before registration the same call 404s.
func TestRemoteStore_EnsureRegistered_SignedServerRoundTrip(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(store)
	h.SetRequireAgentSig(true)
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	_, priv := identityKey(t)
	pub := priv.Public().(ed25519.PublicKey)
	rs := NewRemoteStore(srv.URL, "bridge-e2e", "", WithSigningKey(priv))

	// Before registration: the bridge's own inbox is unknown -> 404.
	if _, _, err := rs.Retrieve("bridge-e2e", 30*time.Second, 5); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("pre-registration Retrieve: err = %v, want ErrAgentNotFound", err)
	}

	res, err := rs.EnsureRegistered("bridge-e2e", pub, []string{"mcp"})
	if err != nil || !res.Created {
		t.Fatalf("EnsureRegistered = %+v, %v", res, err)
	}

	// After registration the same signed call works end-to-end.
	if err := rs.Deliver("bridge-e2e", &InboxEntry{Payload: json.RawMessage(`{"kind":"ping"}`)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	entries, leaseID, err := rs.Retrieve("bridge-e2e", 30*time.Second, 5)
	if err != nil {
		t.Fatalf("post-registration Retrieve: %v", err)
	}
	if len(entries) != 1 || leaseID == "" {
		t.Fatalf("Retrieve = %d entries lease=%q", len(entries), leaseID)
	}
}
