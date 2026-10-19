package federation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func handshakeFixture(t *testing.T) (*HandshakeService, ed25519.PrivateKey, HandshakeStart) {
	t.Helper()
	_, self, _ := ed25519.GenerateKey(rand.Reader)
	pub, peer, _ := ed25519.GenerateKey(rand.Reader)
	s, err := OpenPeerStore(filepath.Join(t.TempDir(), "federation", "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(PeerRecord{ID: "a", PublicKey: hex.EncodeToString(pub), URL: "https://a.example", Direction: "both"}); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandshakeService(s, "b", self)
	if err != nil {
		t.Fatal(err)
	}
	return h, peer, HandshakeStart{InstanceID: "a", PublicKey: hex.EncodeToString(pub), KeyID: KeyID(pub), Nonce: strings.Repeat("ab", 32), Timestamp: time.Now().Unix()}
}
func countersign(key ed25519.PrivateKey, p HandshakeProof) HandshakeProof {
	p.InitiatorSignature = hex.EncodeToString(ed25519.Sign(key, p.Transcript.Payload()))
	return p
}
func requireHandshakeError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Fatalf("want %s, got %v", want, err)
	}
	t.Logf("measured refusal=%s", err)
}

func TestFederation363HandshakeRefusalMatrix(t *testing.T) {
	for _, test := range []struct {
		name, want string
		edit       func(*HandshakeProof)
	}{
		{"out_of_order", "FED_HANDSHAKE_OUT_OF_ORDER", func(p *HandshakeProof) { p.Transcript.ResponderNonce = strings.Repeat("cd", 32) }},
		{"wrong_nonce", "FED_HANDSHAKE_NONCE_MISMATCH", func(p *HandshakeProof) { p.Transcript.InitiatorNonce = strings.Repeat("cd", 32) }},
		{"wrong_key_id", "FED_HANDSHAKE_NONCE_MISMATCH", func(p *HandshakeProof) { p.Transcript.InitiatorKeyID = "wrong" }},
		{"wrong_path", "FED_HANDSHAKE_NONCE_MISMATCH", func(p *HandshakeProof) { p.Transcript.Path = "/agents/a/inbox" }},
		{"wrong_responder", "FED_HANDSHAKE_NONCE_MISMATCH", func(p *HandshakeProof) { p.Transcript.ResponderID = "other" }},
		{"wrong_signature", string(FedBadSignature), func(p *HandshakeProof) { p.InitiatorSignature = strings.Repeat("00", 64) }},
		{"wrong_responder_signature", string(FedBadSignature), func(p *HandshakeProof) { p.ResponderSignature = strings.Repeat("00", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, key, req := handshakeFixture(t)
			p, err := h.Start(req)
			if err != nil {
				t.Fatal(err)
			}
			p = countersign(key, p)
			test.edit(&p)
			_, err = h.Finish(p)
			requireHandshakeError(t, err, test.want)
			peer, _ := h.Store.Get("a")
			if peer.State != "pending" {
				t.Fatal(peer)
			}
		})
	}
	t.Run("presented key differs from pinned record", func(t *testing.T) {
		h, _, req := handshakeFixture(t)
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		req.PublicKey = hex.EncodeToString(pub)
		req.KeyID = KeyID(pub)
		_, err := h.Start(req)
		requireHandshakeError(t, err, string(FedBadSignature))
	})
	t.Run("expired challenge", func(t *testing.T) {
		h, key, req := handshakeFixture(t)
		p, err := h.Start(req)
		if err != nil {
			t.Fatal(err)
		}
		h.now = func() time.Time { return time.Now().Add(time.Minute) }
		_, err = h.Finish(countersign(key, p))
		requireHandshakeError(t, err, "FED_HANDSHAKE_EXPIRED")
	})
	t.Run("stale start", func(t *testing.T) {
		h, _, req := handshakeFixture(t)
		req.Timestamp -= 60
		_, err := h.Start(req)
		requireHandshakeError(t, err, "FED_HANDSHAKE_EXPIRED")
	})
	t.Run("replayed start", func(t *testing.T) {
		h, _, req := handshakeFixture(t)
		if _, err := h.Start(req); err != nil {
			t.Fatal(err)
		}
		_, err := h.Start(req)
		requireHandshakeError(t, err, "FED_HANDSHAKE_REPLAY")
	})
	t.Run("revoked during handshake", func(t *testing.T) {
		h, key, req := handshakeFixture(t)
		p, err := h.Start(req)
		if err != nil {
			t.Fatal(err)
		}
		state := "revoked"
		if _, err = h.Store.Patch("a", PeerPatch{State: &state}); err != nil {
			t.Fatal(err)
		}
		_, err = h.Finish(countersign(key, p))
		requireHandshakeError(t, err, string(FedPeerRevoked))
	})
}

func TestFederation363HandshakeConcurrentReplay(t *testing.T) {
	h, key, req := handshakeFixture(t)
	p, err := h.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	p = countersign(key, p)
	var wg sync.WaitGroup
	outcomes := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := h.Finish(p); outcomes <- err }()
	}
	wg.Wait()
	close(outcomes)
	accepted, replayed := 0, 0
	for err := range outcomes {
		if err == nil {
			accepted++
		} else if err.Error() == "FED_HANDSHAKE_REPLAY" {
			replayed++
		} else {
			t.Fatal(err)
		}
	}
	if accepted != 1 || replayed != 1 || len(h.Store.List()) != 1 {
		t.Fatalf("accepted %d replayed %d", accepted, replayed)
	}
}
func TestFederation363PeerStoreDurabilityAndImmutableKey(t *testing.T) {
	h, key, req := handshakeFixture(t)
	proof, err := h.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Finish(countersign(key, proof)); err != nil {
		t.Fatal(err)
	}
	label := "new label"
	direction := "inbound"
	state := "revoked"
	if _, err = h.Store.Patch("a", PeerPatch{Label: &label, Direction: &direction, State: &state}); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPeerStore(h.Store.path)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reopened.Get("a")
	if !ok || p.State != "revoked" || p.Direction != direction || p.Label != label || p.LinkedAt == nil || p.RevokedAt == nil {
		t.Fatalf("lost record: %+v", p)
	}
	st, err := os.Stat(h.Store.path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st, err)
	}
	st, err = os.Stat(filepath.Dir(h.Store.path))
	if err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("dir mode %v %v", st, err)
	}
	for _, body := range []string{`{"public_key":"replacement"}`, `{"id":"other"}`, `{"url":"https://other.example"}`, `{"key_id":"other"}`} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("PATCH", "/fed/peers/a", strings.NewReader(body))
		reopened.HandlePatch("a", w, r)
		if w.Code != 400 {
			t.Fatalf("immutable field accepted %s", body)
		}
	}
	if _, err = reopened.Create(p); err == nil {
		t.Fatal("duplicate identity accepted")
	}
	pending := "pending"
	if _, err = reopened.Patch("a", PeerPatch{State: &pending}); err != nil {
		t.Fatal(err)
	}
	linked := "linked"
	if _, err = reopened.Patch("a", PeerPatch{State: &linked}); err == nil {
		t.Fatal("PATCH bypasses handshake")
	}
	if err = os.WriteFile(h.Store.path, []byte("torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenPeerStore(h.Store.path); err == nil {
		t.Fatal("corrupt auth state failed open")
	}
}
func TestFederation363PeerStoreWriteFailureDoesNotPublish(t *testing.T) {
	h, _, _ := handshakeFixture(t)
	previous, _ := h.Store.Get("a")
	// A parent regular file forces the persistence error without root-mode assumptions.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Store.path = filepath.Join(blocker, "peers.json")
	state := "revoked"
	if _, err := h.Store.Patch("a", PeerPatch{State: &state}); err == nil {
		t.Fatal("expected write failure")
	}
	after, _ := h.Store.Get("a")
	beforeJSON, _ := json.Marshal(previous)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("failed mutation published: %s", afterJSON)
	}
}
func TestFederation363OperatorGateFailClosed(t *testing.T) {
	for _, token := range []string{"", "operator"} {
		for _, presented := range []string{"", "wrong", "operator"} {
			hit := false
			h := OperatorGate(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
			r := httptest.NewRequest("POST", "/fed/peers", nil)
			r.Header.Set("Authorization", BearerPrefix+presented)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if hit != (token != "" && token == presented) {
				t.Fatalf("token set %t presented %q hit %t", token != "", presented, hit)
			}
		}
	}
}

func TestFederation363DeliveryBodyAndIdentityBound(t *testing.T) {
	h, key, req := handshakeFixture(t)
	proof, err := h.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Finish(countersign(key, proof)); err != nil {
		t.Fatal(err)
	}
	c := NewClient([]string{"https://a.example"}, 0, "")
	c.SetSelfKey(key)
	c.SetPeerRecords(h.Store, "a")
	for _, test := range []struct {
		name   string
		mutate func(*http.Request, []byte)
	}{
		{"valid", func(r *http.Request, b []byte) {}},
		{"body_tampered", func(r *http.Request, b []byte) {
			r.Body = io.NopCloser(strings.NewReader(`{"payload":{"text":"tampered"}}`))
		}},
		{"key_id_tampered", func(r *http.Request, b []byte) { r.Header.Set("X-Fed-Key-Id", "other") }},
		{"unknown_identity", func(r *http.Request, b []byte) { r.Header.Set(PeerHeader, "unknown") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"payload":{"text":"original"}}`)
			r := httptest.NewRequest("POST", "https://a.example/agents/test/inbox", bytes.NewReader(body))
			SignFedRequest(r, key, time.Now())
			if err := c.preparePeerForward(r, Link{URL: "https://a.example"}, body); err != nil {
				t.Fatal(err)
			}
			test.mutate(r, body)
			got := h.Store.VerifyDelivery(r)
			want := FedBadSignature
			if test.name == "valid" {
				want = FedAllowed
			}
			if test.name == "unknown_identity" {
				want = FedNoLink
			}
			if got != want {
				t.Fatalf("verdict %s want %s", got, want)
			}
		})
	}
}

type peerRoundTripper func(*http.Request) (*http.Response, error)

func (f peerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFederation363RetryReceiptUsesCurrentRemoteVerdict(t *testing.T) {
	h, key, req := handshakeFixture(t)
	proof, err := h.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Finish(countersign(key, proof)); err != nil {
		t.Fatal(err)
	}
	c := NewClient([]string{"https://a.example"}, 0, "")
	c.SetSelfKey(key)
	c.SetPeerRecords(h.Store, "b")
	revoked := false
	c.http.Transport = peerRoundTripper(func(r *http.Request) (*http.Response, error) {
		status := 503
		headers := make(http.Header)
		if revoked {
			status = 403
			headers.Set(VerdictHeader, string(FedPeerRevoked))
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(`{"error":"remote refusal"}`))}, nil
	})
	queue := NewMemoryHoldQueue()
	holds := NewHoldManager(c, queue, HoldConfig{MaxHold: time.Minute})
	c.SetHoldManager(holds)
	reports := []FailureReport{}
	holds.SetNotifier(func(r FailureReport) error { reports = append(reports, r); return nil })
	_, _, held, v, err := c.ForwardOrHoldVerdict(t.Context(), "target", []byte(`{"payload":{}}`), HoldMeta{MessageID: "m-363", Sender: "sender"})
	if err == nil || held == nil || v != FedAllowed {
		t.Fatalf("held=%v verdict=%s err=%v", held, v, err)
	}
	revoked = true
	holds.Sweep(time.Now().Add(3 * time.Second))
	if len(reports) != 1 || reports[0].Verdict != FedPeerRevoked || reports[0].Status != 403 || queue.Len() != 0 {
		t.Fatalf("reports=%+v queued=%d", reports, queue.Len())
	}
}

func TestFederation363KeyCredentialIsInboxOnlyAndCannotDowngrade(t *testing.T) {
	h, key, req := handshakeFixture(t)
	proof, err := h.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Finish(countersign(key, proof)); err != nil {
		t.Fatal(err)
	}
	c := NewClient([]string{"https://a.example"}, 0, "")
	c.SetSelfKey(key)
	c.SetPeerRecords(h.Store, "a")
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/agents/test/inbox", 200}, {"POST", "/capabilities/test/inbox", 200},
		{"GET", "/agents/test/inbox", 401}, {"POST", "/agents/test/inbox/ack", 401},
		{"GET", "/fed/peers", 401}, {"POST", "/fed/peers", 401},
		{"PATCH", "/fed/peers/a", 401}, {"POST", "/fed/handshake/start", 401},
		{"POST", "/agents", 401}, {"POST", "/agents/test/class", 401},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			body := []byte(`{"payload":{}}`)
			r := httptest.NewRequest(tc.method, tc.path, bytes.NewReader(body))
			SignFedRequest(r, key, time.Now())
			if err := c.preparePeerForward(r, Link{URL: "https://a.example"}, body); err != nil {
				t.Fatal(err)
			}
			hit := false
			legacy := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer deployment" {
						w.WriteHeader(401)
						return
					}
					next.ServeHTTP(w, r)
				})
			}
			gate := DeliveryAuth(legacy, func() *PeerStore { return h.Store })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, r)
			if w.Code != tc.want || hit != (tc.want == 200) {
				t.Fatalf("got %d hit=%v want %d", w.Code, hit, tc.want)
			}
			if tc.want == 200 {
				// A correct deployment bearer must not substitute for the invalid key.
				r.Body = io.NopCloser(bytes.NewReader(body))
				r.Header.Set("Authorization", "Bearer deployment")
				r.Header.Set(FedAuthSigHeader, "invalid")
				w = httptest.NewRecorder()
				gate.ServeHTTP(w, r)
				if w.Code != 401 || w.Header().Get(VerdictHeader) != string(FedBadSignature) {
					t.Fatalf("invalid key fell back to token: %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
