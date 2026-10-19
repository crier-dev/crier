package registry

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/federation"
)

func TestFederation363InboundRevocation(t *testing.T) {
	a, b := newKeyedRelay(t, "relay-a"), newKeyedRelay(t, "relay-b")
	declarePeer(t, a, b)
	declarePeer(t, b, a)
	linkRelay(t, a, b)
	linkRelay(t, b, a)
	if err := b.store.Register(&Agent{ID: "remote"}); err != nil {
		t.Fatal(err)
	}
	forwardClient(a, b)
	// Remote revocation is resolved at ingress even when the source's half
	// remains linked and the request carries valid signatures.
	state := "revoked"
	if _, err := b.peers.Patch(a.id, federation.PeerPatch{State: &state}); err != nil {
		t.Fatal(err)
	}
	r := deliverTo(t, a.router, "remote", []byte(`{"payload":{"text":"refused"}}`), nil)
	assertFedVerdict(t, r, 403, federation.FedPeerRevoked)
	if fedQueueDepth(t, b.store, "remote") != 0 {
		t.Fatal("remote revoked peer delivered")
	}
}

func TestFederation363RevokingOnePeerDoesNotDisableAnother(t *testing.T) {
	a, b := newKeyedRelay(t, "a"), newKeyedRelay(t, "b")
	declarePeer(t, a, b)
	declarePeer(t, b, a)
	linkRelay(t, a, b)
	linkRelay(t, b, a)
	if err := b.store.Register(&Agent{ID: "remote"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(201) }))
	defer dead.Close()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := a.peers.Create(federation.PeerRecord{ID: "revoked", PublicKey: hex.EncodeToString(pub), URL: dead.URL, Direction: "both"}); err != nil {
		t.Fatal(err)
	}
	state := "revoked"
	if _, err := a.peers.Patch("revoked", federation.PeerPatch{State: &state}); err != nil {
		t.Fatal(err)
	}
	c := federation.NewClient([]string{dead.URL, b.server.URL}, time.Second, "")
	c.SetSelfKey(a.key)
	c.SetPeerRecords(a.peers, a.id)
	a.handler.SetFederationClient(c)
	r := deliverTo(t, a.router, "remote", []byte(`{"payload":{"text":"other peer survives"}}`), nil)
	assertFedVerdict(t, r, 201, federation.FedAllowed)
	if calls != 0 || fedQueueDepth(t, b.store, "remote") != 1 {
		t.Fatal("revocation affected wrong peer")
	}
}

func TestFederation363UnknownPeerCannotUseEmptyStore(t *testing.T) {
	b := newKeyedRelay(t, "b")
	if err := b.store.Register(&Agent{ID: "remote"}); err != nil {
		t.Fatal(err)
	}
	r := deliverTo(t, b.router, "remote", []byte(`{"payload":{}}`), map[string]string{federation.PeerHeader: "unapproved", federation.HopHeader: "1"})
	assertFedVerdict(t, r, 403, federation.FedNoLink)
	if fedQueueDepth(t, b.store, "remote") != 0 {
		t.Fatal("unknown peer delivered")
	}
}

func TestFederation363AsyncReceiptRetainsFieldsAndAddsVerdict(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer remote.Close()
	store := setupTestStore(t)
	if err := store.Register(&Agent{ID: "sender"}); err != nil {
		t.Fatal(err)
	}
	router, h := setupFedRouter(store, nil)
	c := federation.NewClient([]string{remote.URL}, 0, "token")
	queue := federation.NewMemoryHoldQueue()
	holds := federation.NewHoldManager(c, queue, federation.HoldConfig{MaxHold: time.Second})
	holds.SetNotifier(FederationFailureSink(store))
	c.SetHoldManager(holds)
	h.SetFederationClient(c)
	rec := deliverTo(t, router, "remote", []byte(`{"payload":{"text":"held"},"sender":"sender","request_id":"req-363","session_id":"session-363"}`), nil)
	assertFedVerdict(t, rec, 202, federation.FedTokenOnly)
	holds.Sweep(time.Now().Add(2 * time.Second))
	entries, _, err := store.Retrieve("sender", time.Minute, 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("receipt %v %+v", err, entries)
	}
	var payload map[string]any
	if err = json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"kind", "code", "message_id", "target", "sender", "request_id", "session_id", "attempts", "status", "error", "verdict"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("lost %s: %s", field, entries[0].Payload)
		}
	}
	if payload["code"] != "FEDERATION_FAILED" || payload["verdict"] != string(federation.FedTokenOnly) || payload["kind"] != "error" {
		t.Fatal(payload)
	}
	t.Logf("asynchronous receipt: %s", entries[0].Payload)
}
