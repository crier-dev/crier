package federation

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// CR-CHAT-024 — federation peer authentication: signed identity on the
// federation handshake, disk-persisted revocation, config-gated so the
// shared-secret posture is unchanged when CR_FED_AUTH_FILE is unset.
// ---------------------------------------------------------------------------

func writePeerAuthFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peerauth.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write peer auth file: %v", err)
	}
	return path
}

func generatePeerKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

func signHeaders(priv ed25519.PrivateKey, method, path string, now time.Time) (ts, sig string) {
	ts = fmt.Sprintf("%d", now.Unix())
	s := ed25519.Sign(priv, FedAuthPayload(method, path, ts))
	return ts, hex.EncodeToString(s)
}

// AC-a: a peer provisions an identity (keypair) and signs a federation
// request; the signature verifies against the registered public key.
func TestPeerAuthValidSignatureAccepted(t *testing.T) {
	pub, priv := generatePeerKey(t)
	path := writePeerAuthFile(t, fmt.Sprintf(
		`{"peers":[{"peer":"relay-b","public_key":%q}],"revoked":[]}`,
		hex.EncodeToString(pub)))
	pa, err := LoadPeerAuthFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ts, sig := signHeaders(priv, http.MethodPost, "/agents/x/inbox", time.Now())
	if perr := pa.VerifyPeerSignature("relay-b", http.MethodPost, "/agents/x/inbox", ts, sig); perr != nil {
		t.Fatalf("valid signature rejected: %v", perr)
	}
}

// AC-a (wire form): the middleware passes a signed request through to the
// next handler and blocks an unsigned one with 401.
func TestPeerAuthMiddlewareWire(t *testing.T) {
	pub, priv := generatePeerKey(t)
	path := writePeerAuthFile(t, fmt.Sprintf(
		`{"peers":[{"peer":"relay-b","public_key":%q}]}`,
		hex.EncodeToString(pub)))
	pa, err := LoadPeerAuthFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	// No peer announcement: pass through untouched (local agent traffic).
	rec := httptest.NewRecorder()
	pa.Middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents/x/inbox", nil))
	if rec.Code != 200 || !called {
		t.Fatalf("unsigned non-peer request blocked: status %d called=%v", rec.Code, called)
	}

	// Peer announcement without signature headers: 401.
	called = false
	req := httptest.NewRequest(http.MethodPost, "/agents/x/inbox", nil)
	req.Header.Set(PeerHeader, "relay-b")
	rec = httptest.NewRecorder()
	pa.Middleware(next).ServeHTTP(rec, req)
	if rec.Code != 401 || called {
		t.Fatalf("unsigned peer announcement passed: status %d called=%v body=%s", rec.Code, called, rec.Body.String())
	}

	// Signed peer request: passes.
	called = false
	req = httptest.NewRequest(http.MethodPost, "/agents/x/inbox", nil)
	req.Header.Set(PeerHeader, "relay-b")
	ts, sig := signHeaders(priv, http.MethodPost, "/agents/x/inbox", time.Now())
	req.Header.Set(FedAuthTsHeader, ts)
	req.Header.Set(FedAuthSigHeader, sig)
	rec = httptest.NewRecorder()
	pa.Middleware(next).ServeHTTP(rec, req)
	if rec.Code != 200 || !called {
		t.Fatalf("signed peer request blocked: status %d called=%v body=%s", rec.Code, called, rec.Body.String())
	}
}

// AC-b: a signature produced by a DIFFERENT key (forged identity) is
// rejected with 401.
func TestPeerAuthForgedSignatureRejected(t *testing.T) {
	pub, _ := generatePeerKey(t)
	_, attackerPriv := generatePeerKey(t)
	path := writePeerAuthFile(t, fmt.Sprintf(
		`{"peers":[{"peer":"relay-b","public_key":%q}]}`,
		hex.EncodeToString(pub)))
	pa, err := LoadPeerAuthFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Attacker signs with its own key but claims relay-b's identity.
	ts, sig := signHeaders(attackerPriv, http.MethodPost, "/agents/x/inbox", time.Now())
	perr := pa.VerifyPeerSignature("relay-b", http.MethodPost, "/agents/x/inbox", ts, sig)
	if perr == nil || perr.Status != http.StatusUnauthorized {
		t.Fatalf("forged signature: want 401, got %+v", perr)
	}

	// An unknown peer claiming an identity with no record: 401.
	_, otherPriv := generatePeerKey(t)
	ts, sig = signHeaders(otherPriv, http.MethodPost, "/agents/x/inbox", time.Now())
	perr = pa.VerifyPeerSignature("relay-ghost", http.MethodPost, "/agents/x/inbox", ts, sig)
	if perr == nil || perr.Status != http.StatusUnauthorized {
		t.Fatalf("unknown peer: want 401, got %+v", perr)
	}

	// A valid key signing a DIFFERENT transcript (wrong path / method) fails.
	pub2, priv2 := generatePeerKey(t)
	path2 := writePeerAuthFile(t, fmt.Sprintf(
		`{"peers":[{"peer":"relay-c","public_key":%q}]}`,
		hex.EncodeToString(pub2)))
	pa2, err := LoadPeerAuthFile(path2)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ts, sig = signHeaders(priv2, http.MethodPost, "/agents/OTHER/inbox", time.Now())
	perr = pa2.VerifyPeerSignature("relay-c", http.MethodPost, "/agents/x/inbox", ts, sig)
	if perr == nil || perr.Status != http.StatusUnauthorized {
		t.Fatalf("wrong-transcript signature: want 401, got %+v", perr)
	}

	// Stale timestamp outside the replay window: 401.
	ts, sig = signHeaders(priv2, http.MethodPost, "/agents/x/inbox", time.Now().Add(-10*time.Minute))
	perr = pa2.VerifyPeerSignature("relay-c", http.MethodPost, "/agents/x/inbox", ts, sig)
	if perr == nil || perr.Status != http.StatusUnauthorized {
		t.Fatalf("stale timestamp: want 401, got %+v", perr)
	}
}

// AC-d: a revoked peer is rejected after revocation — by editing the file,
// with no restart and no code change. Revocation wins over a valid signature.
func TestPeerAuthRevocationWithoutRestart(t *testing.T) {
	pub, priv := generatePeerKey(t)
	path := writePeerAuthFile(t, fmt.Sprintf(
		`{"peers":[{"peer":"relay-b","public_key":%q}]}`,
		hex.EncodeToString(pub)))
	pa, err := LoadPeerAuthFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	ts, sig := signHeaders(priv, http.MethodPost, "/agents/x/inbox", time.Now())
	if perr := pa.VerifyPeerSignature("relay-b", http.MethodPost, "/agents/x/inbox", ts, sig); perr != nil {
		t.Fatalf("pre-revocation request rejected: %v", perr)
	}

	// Operator revokes: edit the file (mtime changes), no restart.
	revoked := fmt.Sprintf(`{"peers":[{"peer":"relay-b","public_key":%q}],"revoked":["relay-b"]}`,
		hex.EncodeToString(pub))
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte(revoked), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	perr := pa.VerifyPeerSignature("relay-b", http.MethodPost, "/agents/x/inbox", ts, sig)
	if perr == nil || perr.Status != http.StatusForbidden {
		t.Fatalf("revoked peer: want 403, got %+v", perr)
	}
	if !strings.Contains(perr.Message, "FED_PEER_REVOKED") {
		t.Fatalf("refusal must name FED_PEER_REVOKED, got %q", perr.Message)
	}
}

// A peer removed from `revoked` is admitted again (revocation is reversible
// by the same file edit).
func TestPeerAuthRevocationReversible(t *testing.T) {
	pub, priv := generatePeerKey(t)
	path := writePeerAuthFile(t, fmt.Sprintf(
		`{"peers":[{"peer":"relay-b","public_key":%q}],"revoked":["relay-b"]}`,
		hex.EncodeToString(pub)))
	pa, _ := LoadPeerAuthFile(path)

	ts, sig := signHeaders(priv, http.MethodPost, "/agents/x/inbox", time.Now())
	if perr := pa.VerifyPeerSignature("relay-b", http.MethodPost, "/agents/x/inbox", ts, sig); perr == nil {
		t.Fatalf("revoked peer accepted before un-revocation")
	}

	restored := fmt.Sprintf(`{"peers":[{"peer":"relay-b","public_key":%q}]}`,
		hex.EncodeToString(pub))
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte(restored), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if perr := pa.VerifyPeerSignature("relay-b", http.MethodPost, "/agents/x/inbox", ts, sig); perr != nil {
		t.Fatalf("un-revoked peer still refused: %v", perr)
	}
}

// An invalid document update keeps the last known-good state (fail-safe,
// not fail-open to an empty peer set).
func TestPeerAuthInvalidUpdateKeepsLastGoodState(t *testing.T) {
	pub, priv := generatePeerKey(t)
	path := writePeerAuthFile(t, fmt.Sprintf(
		`{"peers":[{"peer":"relay-b","public_key":%q}]}`,
		hex.EncodeToString(pub)))
	pa, _ := LoadPeerAuthFile(path)

	broken := `{"peers":[{"peer":"relay-b","public_key":"zz-not-hex"}]}`
	future := time.Now().Add(2 * time.Second)
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	ts, sig := signHeaders(priv, http.MethodPost, "/agents/x/inbox", time.Now())
	if perr := pa.VerifyPeerSignature("relay-b", http.MethodPost, "/agents/x/inbox", ts, sig); perr != nil {
		t.Fatalf("valid peer refused after invalid file update: %v", perr)
	}
}

// A malformed INITIAL document is a load error, never a silently-empty gate.
func TestPeerAuthInvalidInitialDocument(t *testing.T) {
	for name, content := range map[string]string{
		"bad json":     `{"peers":[`,
		"no peer id":   `{"peers":[{"public_key":"aa"}]}`,
		"duplicate":    `{"peers":[{"peer":"x","public_key":"` + strings.Repeat("ab", 32) + `"},{"peer":"x","public_key":"` + strings.Repeat("ab", 32) + `"}]}`,
		"short key":    `{"peers":[{"peer":"x","public_key":"aabb"}]}`,
		"non-hex key":  `{"peers":[{"peer":"x","public_key":"zz"}]}`,
		"missing file": "",
	} {
		if content == "" {
			if _, err := LoadPeerAuthFile(filepath.Join(t.TempDir(), "absent.json")); err == nil {
				t.Fatalf("%s: missing file loaded without error", name)
			}
			continue
		}
		if _, err := LoadPeerAuthFile(writePeerAuthFile(t, content)); err == nil {
			t.Fatalf("%s: invalid document loaded without error", name)
		}
	}
}

// Mutual auth end-to-end at the wire level: relay A signs its outbound
// forward with its self key; relay B's peer-auth middleware verifies the
// signature against A's registered public key.
func TestMutualAuthOutboundSignVerifyRoundTrip(t *testing.T) {
	pubA, privA := generatePeerKey(t)
	authPath := writePeerAuthFile(t, fmt.Sprintf(
		`{"peers":[{"peer":"relay-a","public_key":%q}]}`,
		hex.EncodeToString(pubA)))
	paB, err := LoadPeerAuthFile(authPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var gotPeer, gotTs, gotSig string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPeer = r.Header.Get(PeerHeader)
		gotTs = r.Header.Get(FedAuthTsHeader)
		gotSig = r.Header.Get(FedAuthSigHeader)
		w.WriteHeader(200)
	}))
	defer destination.Close()

	client := NewClient([]string{destination.URL}, 0, "")
	client.SetSelfKey(privA)
	// Stamp at NOW (real clock) so the destination's replay window accepts.
	status, _, err := client.ForwardToURL(t.Context(), destination.URL, "agent-x", []byte(`{}`))
	if err != nil || status != 200 {
		t.Fatalf("forward: status=%d err=%v", status, err)
	}
	if gotPeer != "" {
		t.Fatalf("no policy SelfAs configured: peer header should be absent, got %q", gotPeer)
	}
	if perr := paB.VerifyPeerSignature("relay-a", http.MethodPost, "/agents/agent-x/inbox", gotTs, gotSig); perr != nil {
		t.Fatalf("destination could not verify relay A's signature: %v", perr)
	}
}

// Without a self key, no signature headers are stamped (shipped posture).
func TestClientUnsignedWithoutSelfKey(t *testing.T) {
	var gotTs, gotSig string
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTs = r.Header.Get(FedAuthTsHeader)
		gotSig = r.Header.Get(FedAuthSigHeader)
		w.WriteHeader(200)
	}))
	defer destination.Close()

	client := NewClient([]string{destination.URL}, 0, "")
	if _, _, err := client.ForwardToURL(t.Context(), destination.URL, "agent-x", []byte(`{}`)); err != nil {
		t.Fatalf("forward: %v", err)
	}
	if gotTs != "" || gotSig != "" {
		t.Fatalf("unsigned client stamped signature headers: ts=%q sig=%q", gotTs, gotSig)
	}
}
