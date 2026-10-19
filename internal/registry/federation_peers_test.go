package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/middleware"
)

type keyedRelay struct {
	id      string
	key     ed25519.PrivateKey
	peers   *federation.PeerStore
	handler *Handler
	router  *mux.Router
	server  *httptest.Server
	store   Store
}

func newKeyedRelay(t *testing.T, id string) *keyedRelay {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peers, err := federation.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	store := setupTestStore(t)
	h := NewHandler(store)
	h.SetPeerRecords(peers)
	hs, err := federation.NewHandshakeService(peers, id, key)
	if err != nil {
		t.Fatal(err)
	}
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
	r.Handle("/fed/peers", federation.OperatorGate("operator", http.HandlerFunc(peers.HandleCreate))).Methods("POST")
	r.Handle("/fed/handshake/start", federation.OperatorGate("operator", http.HandlerFunc(hs.HandleStart))).Methods("POST")
	r.Handle("/fed/handshake/finish", federation.OperatorGate("operator", http.HandlerFunc(hs.HandleFinish))).Methods("POST")
	protected := federation.DeliveryAuth(middleware.Auth("operator"), func() *federation.PeerStore { return peers })(r)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(federation.PeerHeader) != "" && r.Header.Get("Authorization") != "" {
			t.Error("keyed forward leaked a shared token")
		}
		protected.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &keyedRelay{id, key, peers, h, r, srv, store}
}
func fedPost(t *testing.T, base, path string, value any) (int, []byte) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", base+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}
func declarePeer(t *testing.T, local, remote *keyedRelay) {
	t.Helper()
	status, body := fedPost(t, local.server.URL, "/fed/peers", map[string]string{"id": remote.id, "public_key": hex.EncodeToString(remote.key.Public().(ed25519.PublicKey)), "url": remote.server.URL, "direction": "both", "label": remote.id})
	if status != 201 {
		t.Fatalf("declare %s: %d %s", remote.id, status, body)
	}
}
func startProof(t *testing.T, initiator, responder *keyedRelay) federation.HandshakeProof {
	t.Helper()
	n := make([]byte, 32)
	if _, err := rand.Read(n); err != nil {
		t.Fatal(err)
	}
	pub := initiator.key.Public().(ed25519.PublicKey)
	status, body := fedPost(t, responder.server.URL, "/fed/handshake/start", federation.HandshakeStart{InstanceID: initiator.id, PublicKey: hex.EncodeToString(pub), KeyID: federation.KeyID(pub), Nonce: hex.EncodeToString(n), Timestamp: time.Now().Unix()})
	if status != 200 {
		t.Fatalf("start: %d %s", status, body)
	}
	var proof federation.HandshakeProof
	if err := json.Unmarshal(body, &proof); err != nil {
		t.Fatal(err)
	}
	// The initiating side MUST verify the responder's pinned key BEFORE
	// countersigning. This exercises both possession proofs, not just B's.
	sig, err := hex.DecodeString(proof.ResponderSignature)
	if err != nil || !ed25519.Verify(responder.key.Public().(ed25519.PublicKey), proof.Transcript.Payload(), sig) {
		t.Fatal("responder proof failed")
	}
	proof.InitiatorSignature = hex.EncodeToString(ed25519.Sign(initiator.key, proof.Transcript.Payload()))
	return proof
}
func linkRelay(t *testing.T, initiator, responder *keyedRelay) federation.HandshakeProof {
	t.Helper()
	proof := startProof(t, initiator, responder)
	status, body := fedPost(t, responder.server.URL, "/fed/handshake/finish", proof)
	if status != 200 {
		t.Fatalf("finish: %d %s", status, body)
	}
	return proof
}
func forwardClient(local, remote *keyedRelay) *federation.Client {
	c := federation.NewClient([]string{remote.server.URL}, 0, "must-not-cross")
	c.SetSelfKey(local.key)
	c.SetPeerRecords(local.peers, local.id)
	local.handler.SetFederationClient(c)
	return c
}

func TestFederation363MutualHandshakeAndVerdicts(t *testing.T) {
	a, b := newKeyedRelay(t, "relay-a"), newKeyedRelay(t, "relay-b")
	declarePeer(t, a, b)
	declarePeer(t, b, a)
	if err := b.store.Register(&Agent{ID: "remote", Capabilities: []string{"test"}}); err != nil {
		t.Fatal(err)
	}
	forwardClient(a, b)
	body := []byte(`{"payload":{"text":"keyed hello"},"sender":"sender","request_id":"req-363"}`)
	// Pending is not linked: nothing goes over the link.
	r := deliverTo(t, a.router, "remote", body, nil)
	assertFedVerdict(t, r, 403, federation.FedNoLink)
	if fedQueueDepth(t, b.store, "remote") != 0 {
		t.Fatal("pending link forwarded")
	}
	p := linkRelay(t, a, b)
	linkRelay(t, b, a)
	r = deliverTo(t, a.router, "remote", body, nil)
	assertFedVerdict(t, r, 201, federation.FedAllowed)
	stats := fedQueueDepth(t, b.store, "remote")
	if stats != 1 {
		t.Fatalf("delivered count: %+v", stats)
	}
	t.Logf("mutual transcript=%s", p.Transcript.Payload())
	peer, _ := a.peers.Get(b.id)
	raw, _ := json.Marshal(peer)
	t.Logf("peer record=%s", raw)
	// Replaying the exact proof cannot create/relink a second record.
	status, replay := fedPost(t, b.server.URL, "/fed/handshake/finish", p)
	if status != 403 || !strings.Contains(string(replay), "FED_HANDSHAKE_REPLAY") || len(b.peers.List()) != 1 {
		t.Fatalf("replay: %d %s", status, replay)
	}
	// Wrong signing key while retaining the announced, pinned identity.
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	c := forwardClient(a, b)
	c.SetSelfKey(wrong)
	r = deliverTo(t, a.router, "remote", body, nil)
	assertFedVerdict(t, r, 401, federation.FedBadSignature)
	if fedQueueDepth(t, b.store, "remote") != 1 {
		t.Fatal("wrong key delivered")
	}
	forwardClient(a, b)
	state := "revoked"
	if _, err := a.peers.Patch(b.id, federation.PeerPatch{State: &state}); err != nil {
		t.Fatal(err)
	}
	r = deliverTo(t, a.router, "remote", body, nil)
	assertFedVerdict(t, r, 403, federation.FedPeerRevoked)
	if fedQueueDepth(t, b.store, "remote") != 1 {
		t.Fatal("revoked peer delivered")
	}
	var failure map[string]any
	if err := json.Unmarshal(r.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"error", "message_id", "target", "sender", "request_id", "attempts", "detail", "verdict"} {
		if _, ok := failure[k]; !ok {
			t.Errorf("receipt lost %s: %s", k, r.Body.String())
		}
	}
	if failure["error"] != "FEDERATION_FAILED" {
		t.Fatal(failure)
	}
}
func assertFedVerdict(t *testing.T, r *httptest.ResponseRecorder, status int, v federation.Verdict) {
	t.Helper()
	if r.Code != status || r.Header().Get(federation.VerdictHeader) != string(v) {
		t.Fatalf("want %d %s; got %d %s body=%s", status, v, r.Code, r.Header().Get(federation.VerdictHeader), r.Body.String())
	}
	t.Logf("measured %s: HTTP %d body=%s", v, r.Code, r.Body.String())
}
func TestFederation363TokenOnlyByteParity(t *testing.T) {
	want := `{"id":"original","reply":{"text":"legacy pong"},"request_id":"req-legacy"}`
	var gotBody []byte
	remote := httptest.NewServer(middleware.Auth("legacy-secret")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(federation.HopHeader) != "1" || r.Header.Get(federation.PeerHeader) != "" || r.Header.Get(federation.FedAuthSigHeader) != "" {
			t.Error("legacy headers changed")
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(want))
	})))
	defer remote.Close()
	router, h := setupFedRouter(setupTestStore(t), nil)
	c := federation.NewClient([]string{remote.URL}, 0, "legacy-secret")
	empty, err := federation.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	c.SetPeerRecords(empty, "relay-a")
	h.SetFederationClient(c)
	r := deliverTo(t, router, "remote", []byte(`{"payload":{"text":"hello"},"sender":"local","request_id":"req-legacy"}`), nil)
	assertFedVerdict(t, r, 200, federation.FedTokenOnly)
	if r.Body.String() != want {
		t.Fatalf("legacy response bytes changed: %q", r.Body.String())
	}
	var decoded deliverRequest
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatal(err)
	}
	legacyBytes := []byte(`{"payload":{"text":"hello"},"sender":"local","request_id":"req-legacy"}`)
	if !bytes.Equal(gotBody, legacyBytes) {
		t.Fatalf("legacy request bytes changed: %s", gotBody)
	}
	t.Logf("token-only: response byte-identical (%d bytes); request equals shipped deliverRequest marshal (%d bytes)", len(want), len(gotBody))
	// A wrong deployment bearer remains the shipped exact refusal.
	bad := federation.NewClient([]string{remote.URL}, 0, "wrong")
	s, b, _, v, e := bad.ForwardOrHoldVerdict(t.Context(), "remote", legacyBytes, federation.HoldMeta{})
	if s != 401 || string(b) != `{"error":"invalid token"}` || e != nil || v != federation.FedTokenOnly {
		t.Fatal(fmt.Sprint(s, string(b), v, e))
	}
}

func fedQueueDepth(t *testing.T, s Store, id string) int {
	t.Helper()
	depth, _, _, err := s.Stats(id)
	if err != nil {
		t.Fatal(err)
	}
	return depth
}
