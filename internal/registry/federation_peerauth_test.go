package registry

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

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/middleware"
)

func stringReader(s string) *strings.Reader { return strings.NewReader(s) }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func authMiddlewareForTest(token string) func(http.Handler) http.Handler {
	return middleware.Auth(token)
}

// ---------------------------------------------------------------------------
// CR-CHAT-024 — peer auth at the deliver boundary: a request that announces
// a peer identity must PROVE it (signed) before the CR-CHAT-023 policy layer
// acts on the announcement; the policy layer then decides what the proven
// peer may reach (AC-c); revocation (AC-d) and the legacy shared-secret
// posture (AC-e) are covered here too at the handler level.
// ---------------------------------------------------------------------------

func fedAuthPeerKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv
}

// setupPeerAuthHandler wires a deliver route behind the peer-auth middleware
// with the given policy set — the same composition main.go registers
// (Auth → PeerAuth.Middleware → HandleDeliver).
func setupPeerAuthHandler(t *testing.T, policies federation.PeerPolicies) (http.Handler, ed25519.PrivateKey) {
	t.Helper()
	store := setupTestStore(t)
	if err := store.Register(&Agent{ID: "atlas", Namespace: "ns-a"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	handler := NewHandler(store)
	handler.SetPeerPolicies(policies)

	pub, priv := fedAuthPeerKey(t)
	authPath := filepath.Join(t.TempDir(), "peerauth.json")
	doc := fmt.Sprintf(`{"peers":[{"peer":"peer_ok","public_key":%q},{"peer":"peer_revoked","public_key":%q}],"revoked":["peer_revoked"]}`,
		hex.EncodeToString(pub), hex.EncodeToString(pub))
	if err := os.WriteFile(authPath, []byte(doc), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	pa, err := federation.LoadPeerAuthFile(authPath)
	if err != nil {
		t.Fatalf("load peer auth: %v", err)
	}

	r := mux.NewRouter()
	r.Use(pa.Middleware)
	r.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	return r, priv
}

func signedFedRequest(t *testing.T, h http.Handler, priv ed25519.PrivateKey, peer, agentID, body string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agents/"+agentID+"/inbox", stringReader(body))
	req.Header.Set("Content-Type", "application/json")
	if peer != "" {
		req.Header.Set(federation.PeerHeader, peer)
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := ed25519.Sign(priv, federation.FedAuthPayload(http.MethodPost, "/agents/"+agentID+"/inbox", ts))
		req.Header.Set(federation.FedAuthTsHeader, ts)
		req.Header.Set(federation.FedAuthSigHeader, hex.EncodeToString(sig))
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// AC-b (boundary form): a forged peer announcement — signed with a key that
// is not the announced peer's — is refused 401 BEFORE the policy layer runs
// (the deliver handler never executes).
func TestPeerAuthForgedAnnouncementRefusedAtBoundary(t *testing.T) {
	_, wrongPriv := fedAuthPeerKey(t)
	h, _ := setupPeerAuthHandler(t, federation.PeerPolicies{
		"peer_ok": {Peer: "peer_ok", NamespacesAllow: []string{""}},
	})
	rec := signedFedRequest(t, h, wrongPriv, "peer_ok", "atlas", `{"payload":{"a":1}}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged announcement: status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
}

// AC-d (boundary form): a revoked peer with a VALID signature gets 403
// FED_PEER_REVOKED.
func TestPeerAuthRevokedPeerRefusedAtBoundary(t *testing.T) {
	h, priv := setupPeerAuthHandler(t, federation.PeerPolicies{
		"peer_revoked": {Peer: "peer_revoked", NamespacesAllow: []string{""}},
	})
	rec := signedFedRequest(t, h, priv, "peer_revoked", "atlas", `{"payload":{"a":1}}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("revoked peer: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), "FED_PEER_REVOKED") {
		t.Fatalf("refusal must name FED_PEER_REVOKED: %s", rec.Body.String())
	}
}

// AC-c: a peer authorized for action A (deliver into namespace ns-a) but
// not action B (deliver into ns-b) gets 200 on A and 403
// FED_NAMESPACE_NOT_PERMITTED on B — with a PROVEN identity, since the
// signature gate runs first.
func TestPeerAuthAuthorizedActionAllowedUnauthorizedRefused(t *testing.T) {
	h, priv := setupPeerAuthHandler(t, federation.PeerPolicies{
		"peer_ok": {Peer: "peer_ok", NamespacesAllow: []string{"ns-a"}},
	})

	// Action A: deliver into the admitted namespace — accepted.
	rec := signedFedRequest(t, h, priv, "peer_ok", "atlas", `{"payload":{"a":1},"namespace":"ns-a"}`, nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("authorized action: status = %d, want 200/201 (body %s)", rec.Code, rec.Body.String())
	}

	// Action B: deliver into a namespace the policy does not admit — 403.
	rec = signedFedRequest(t, h, priv, "peer_ok", "atlas", `{"payload":{"a":1},"namespace":"ns-b"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unauthorized action: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), "FED_NAMESPACE_NOT_PERMITTED") {
		t.Fatalf("refusal must name FED_NAMESPACE_NOT_PERMITTED: %s", rec.Body.String())
	}
}

// AC-e: legacy shared-secret mode — with NO peer-auth file configured, a
// request announcing a peer identity takes the shipped CR-CHAT-023 path
// unchanged (policy consulted on the bare claim, no signature asked).
func TestLegacySharedSecretModeUnchanged(t *testing.T) {
	store := setupTestStore(t)
	if err := store.Register(&Agent{ID: "atlas"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	handler := NewHandler(store)
	handler.SetPeerPolicies(federation.PeerPolicies{
		"peer_claim": {Peer: "peer_claim", NamespacesAllow: []string{""}},
	})
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")

	// The shipped posture: an announcement with NO signature headers still
	// passes the (nonexistent) signature gate and the policy admits it.
	rec := deliverTo(t, r, "atlas", []byte(`{"payload":{"a":1}}`), map[string]string{
		federation.PeerHeader: "peer_claim",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("legacy mode changed: status = %d (body %s)", rec.Code, rec.Body.String())
	}

	// And the shared-secret Bearer gate itself is untouched: middleware.Auth
	// with a token still rejects a bad token with 401.
	auth := authMiddlewareForTest("sekrit")
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	req := httptest.NewRequest(http.MethodPost, "/agents/atlas/inbox", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	auth(next).ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized || called {
		t.Fatalf("legacy Bearer gate broken: status %d called=%v", rec2.Code, called)
	}
}
