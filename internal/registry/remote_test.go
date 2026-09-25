package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/namespace"
)

// newRemoteTestServer spins a real registry HTTP handler (signing disabled)
// over an in-memory store — the same wire the RemoteStore will talk to.
func newRemoteTestServer(t *testing.T) (*httptest.Server, *MemoryStore) {
	return newRemoteTestServerSigning(t, false)
}

// newRemoteTestServerSigning is newRemoteTestServer with the handler's
// CR_REQUIRE_AGENT_SIG toggle exposed, so tests exercise the secure-default
// signed configuration against the real routes rather than a mock.
func newRemoteTestServerSigning(t *testing.T, requireSig bool) (*httptest.Server, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	h := NewHandler(store)
	h.SetRequireAgentSig(requireSig)
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
	r.HandleFunc("/agents", h.HandleListAgents).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}", h.HandleUnregister).Methods(http.MethodDelete)
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox/stats", h.HandleStats).Methods(http.MethodGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store
}

// pkcs8Ed25519PEM encodes priv exactly the way `openssl genpkey -algorithm
// ED25519` writes it: PKCS#8 DER inside a "PRIVATE KEY" PEM block.
func pkcs8Ed25519PEM(t *testing.T, priv ed25519.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestRemoteStore_RegisterGetListUnregister(t *testing.T) {
	srv, _ := newRemoteTestServer(t)
	rs := NewRemoteStore(srv.URL, "remote-client", "")

	// Build the key the way real clients do: hex string -> raw bytes.
	keyBytes, err := hex.DecodeString(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	agent := &Agent{ID: "alice", PublicKey: HexKey(keyBytes), Capabilities: []string{"chat"}}
	if err := rs.Register(agent); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := rs.Get("alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "alice" || hex.EncodeToString([]byte(got.PublicKey)) != strings.Repeat("ab", 32) {
		t.Fatalf("Get = %+v", got)
	}

	list := rs.List()
	if len(list) != 1 || list[0].ID != "alice" {
		t.Fatalf("List = %+v", list)
	}

	if err := rs.Unregister("alice"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if _, err := rs.Get("alice"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Get after unregister: %v (want ErrAgentNotFound)", err)
	}
}

func TestRemoteStore_DeliverRetrieveAck(t *testing.T) {
	srv, _ := newRemoteTestServer(t)
	rs := NewRemoteStore(srv.URL, "bob", "")

	keyBytes, err := hex.DecodeString(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if err := rs.Register(&Agent{ID: "bob", PublicKey: HexKey(keyBytes)}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	payload := json.RawMessage(`{"kind":"question","text":"which plot?"}`)
	if err := rs.Deliver("bob", &InboxEntry{AgentID: "bob", Payload: payload}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	entries, leaseID, err := rs.Retrieve("bob", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(entries) != 1 || leaseID == "" {
		t.Fatalf("Retrieve = %d entries lease=%q", len(entries), leaseID)
	}
	if string(entries[0].Payload) != string(payload) {
		t.Fatalf("payload = %s", entries[0].Payload)
	}

	if err := rs.Ack("bob", leaseID, []string{entries[0].ID}); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	again, _, err := rs.Retrieve("bob", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("Retrieve after ack: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("inbox not empty after ack: %d", len(again))
	}

	depth, leased, _, err := rs.Stats("bob")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if depth != 0 || leased != 0 {
		t.Fatalf("Stats = depth %d leased %d", depth, leased)
	}
}

func TestRemoteStore_Errors(t *testing.T) {
	srv, _ := newRemoteTestServer(t)
	rs := NewRemoteStore(srv.URL+"/", "ghost", "")

	if _, err := rs.Get("ghost"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Get unknown: %v (want ErrAgentNotFound)", err)
	}
	if err := rs.Deliver("ghost", &InboxEntry{Payload: json.RawMessage(`{}`)}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Deliver unknown: %v (want ErrAgentNotFound)", err)
	}
	if _, _, err := rs.Retrieve("ghost", 30*time.Second, 5); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Retrieve unknown: %v (want ErrAgentNotFound)", err)
	}

	// Duplicate register -> ErrAgentExists.
	keyBytes, err := hex.DecodeString(strings.Repeat("ef", 32))
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	rs.Register(&Agent{ID: "dup", PublicKey: HexKey(keyBytes)})
	if err := rs.Register(&Agent{ID: "dup", PublicKey: HexKey(keyBytes)}); !errors.Is(err, ErrAgentExists) {
		t.Fatalf("Register dup: %v (want ErrAgentExists)", err)
	}
}

func TestRemoteStore_SendsAgentIDHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Agent-ID")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"agents":[]}`))
	}))
	t.Cleanup(srv.Close)

	rs := NewRemoteStore(srv.URL, "header-check", "")
	rs.List()
	if gotHeader != "header-check" {
		t.Fatalf("X-Agent-ID = %q", gotHeader)
	}
}

// sigCapture records the per-agent signature headers of every request plus
// the raw signed payload reconstructed exactly the way the server rebuilds
// it (r.Method + "\n" + r.URL.Path + "\n" + ts). The mutex makes concurrent
// retrieve/ack traffic race-free.
type sigCapture struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	sigs    [][]byte
	ts      []string
}

// middleware returns an http.Handler wrapper that records the request, then
// delegates to next (the real registry handler).
func (c *sigCapture) middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ts := r.Header.Get(HeaderAgentTS)
		raw, _ := hex.DecodeString(r.Header.Get(HeaderAgentSig))
		c.mu.Lock()
		c.methods = append(c.methods, r.Method)
		c.paths = append(c.paths, r.URL.Path)
		c.sigs = append(c.sigs, raw)
		c.ts = append(c.ts, ts)
		c.mu.Unlock()
		next(w, r)
	}
}

func (c *sigCapture) snapshot() (methods, paths []string, sigs [][]byte, ts []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.methods...),
		append([]string(nil), c.paths...),
		append([][]byte(nil), c.sigs...),
		append([]string(nil), c.ts...)
}

// newCapturingSigServer is newRemoteTestServerSigning with the four
// agent-owned (signature-gated) routes wrapped in a sigCapture middleware, so
// tests can assert the exact server-side signed payload — not just the
// client's idea of it.
func newCapturingSigServer(t *testing.T, requireSig bool) (*httptest.Server, *MemoryStore, *sigCapture) {
	t.Helper()
	store := NewMemoryStore()
	h := NewHandler(store)
	h.SetRequireAgentSig(requireSig)
	cap := &sigCapture{}
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
	r.HandleFunc("/agents", h.HandleListAgents).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}", cap.middleware(h.HandleUnregister)).Methods(http.MethodDelete)
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox", cap.middleware(h.HandleRetrieve)).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox/ack", cap.middleware(h.HandleAck)).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox/stats", cap.middleware(h.HandleStats)).Methods(http.MethodGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store, cap
}

// TestRemoteStore_SignedLifecycle proves the acceptance flow end-to-end with
// the secure default (CR_REQUIRE_AGENT_SIG=true): a remote store configured
// with a PKCS#8 PEM key file can register-time-bind its public key, retrieve,
// read stats, ack, and unregister without 401 signature errors.
func TestRemoteStore_SignedLifecycle(t *testing.T) {
	srv, store, cap := newCapturingSigServer(t, true)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Load the key exactly the way crier-mcp startup does: PKCS#8 PEM file.
	keyFile := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(keyFile, pkcs8Ed25519PEM(t, priv), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	loaded, err := LoadEd25519PrivateKeyFile(keyFile)
	if err != nil {
		t.Fatalf("LoadEd25519PrivateKeyFile: %v", err)
	}
	if !bytes.Equal(loaded, priv) {
		t.Fatal("loaded key differs from generated key")
	}

	// Register the public key for the agent over the (unsigned) register
	// endpoint — the bootstrap step from the README / integration guide.
	regRS := NewRemoteStore(srv.URL, "signed-agent", "")
	if err := regRS.Register(&Agent{ID: "signed-agent", PublicKey: HexKey(pub), Capabilities: []string{"demo"}}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Signing enabled + Bearer token both present: bearer behavior unchanged.
	rs := NewRemoteStore(srv.URL, "signed-agent", "bearer-tok", WithSigningKey(loaded))

	// Deliver to the (unsigned) inbox endpoint so there is something to read.
	if err := regRS.Deliver("signed-agent", &InboxEntry{Payload: json.RawMessage(`{"kind":"ping"}`)}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// Retrieve — agent-owned, signed.
	entries, leaseID, err := rs.Retrieve("signed-agent", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("signed Retrieve: %v", err)
	}
	if len(entries) != 1 || leaseID == "" {
		t.Fatalf("Retrieve = %d entries lease=%q", len(entries), leaseID)
	}

	// Stats — agent-owned, signed. The message is leased (1), not acked yet.
	depth, leased, _, err := rs.Stats("signed-agent")
	if err != nil {
		t.Fatalf("signed Stats: %v", err)
	}
	if depth != 1 || leased != 1 {
		t.Fatalf("Stats = depth %d leased %d, want 1/1", depth, leased)
	}

	// Ack — agent-owned, signed.
	if err := rs.Ack("signed-agent", leaseID, []string{entries[0].ID}); err != nil {
		t.Fatalf("signed Ack: %v", err)
	}

	// Unregister — agent-owned, signed.
	if err := rs.Unregister("signed-agent"); err != nil {
		t.Fatalf("signed Unregister: %v", err)
	}
	if _, err := store.Get("signed-agent"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("agent still present after Unregister: %v", err)
	}

	// Every signed request must have verified on the server: the handler
	// above (secure default) returns 401/403 before touching the store, so
	// reaching this point already proves ed25519.Verify passed. Re-verify the
	// signatures independently anyway, plus the exact payload contract.
	methods, paths, sigs, tss := cap.snapshot()
	if len(methods) != 4 {
		t.Fatalf("captured %d signed requests, want 4 (retrieve, stats, ack, unregister)", len(methods))
	}
	wantMethods := []string{http.MethodGet, http.MethodGet, http.MethodPost, http.MethodDelete}
	wantSuffixes := []string{"/inbox", "/inbox/stats", "/inbox/ack", ""}
	for i, path := range paths {
		if methods[i] != wantMethods[i] || !strings.HasSuffix(path, wantSuffixes[i]) {
			t.Fatalf("request %d = %s %s, want %s …%s", i, methods[i], path, wantMethods[i], wantSuffixes[i])
		}
		// X-Agent-Ts must be a fresh unix-seconds timestamp.
		ts, err := strconv.ParseInt(tss[i], 10, 64)
		if err != nil {
			t.Fatalf("request %d: X-Agent-Ts %q is not unix seconds", i, tss[i])
		}
		if d := time.Since(time.Unix(ts, 0)); d > 30*time.Second || d < -30*time.Second {
			t.Fatalf("request %d: stale X-Agent-Ts %d (delta %s)", i, ts, d)
		}
		if len(sigs[i]) != ed25519.SignatureSize {
			t.Fatalf("request %d: signature size = %d", i, len(sigs[i]))
		}
		if !ed25519.Verify(pub, []byte(methods[i]+"\n"+path+"\n"+tss[i]), sigs[i]) {
			t.Fatalf("request %d: signature does not verify over %q", i, methods[i]+"\n"+path+"\n"+tss[i])
		}
	}
}

// TestRemoteStore_SignedRetrieveExcludesQueryFromPath pins the path part of
// the signing contract: the query string (max/lease on retrieve) must NOT be
// part of the signed payload, and the signature must verify over the decoded
// path exactly as the server's r.URL.Path sees it.
func TestRemoteStore_SignedRetrieveExcludesQueryFromPath(t *testing.T) {
	srv, store, cap := newCapturingSigServer(t, true)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Register(&Agent{ID: "q-agent", PublicKey: HexKey(pub)}); err != nil {
		t.Fatalf("register: %v", err)
	}
	rs := NewRemoteStore(srv.URL, "q-agent", "", WithSigningKey(priv))

	if err := store.Deliver("q-agent", &InboxEntry{Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	entries, _, err := rs.Retrieve("q-agent", 45*time.Second, 3)
	if err != nil {
		t.Fatalf("signed Retrieve: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("Retrieve = %d entries", len(entries))
	}

	_, paths, sigs, tss := cap.snapshot()
	if len(paths) != 1 {
		t.Fatalf("captured %d signed requests, want 1", len(paths))
	}
	// The signed payload must be exactly "GET\n/agents/q-agent/inbox\n<ts>" —
	// no "?lease=45&max=3" suffix.
	payload := paths[0] + "\n" + tss[0]
	if paths[0] != "/agents/q-agent/inbox" {
		t.Fatalf("signed path = %q, want /agents/q-agent/inbox (query excluded)", paths[0])
	}
	if strings.Contains(payload, "max=") || strings.Contains(payload, "lease=") {
		t.Fatalf("signed payload %q leaks the query string", payload)
	}
	if !ed25519.Verify(pub, []byte("GET\n"+payload), sigs[0]) {
		t.Fatalf("signature does not verify over %q", "GET\n"+payload)
	}
}

// TestRemoteStore_SignatureContractPayload pins the escaped/decoded path
// round trip: a request path whose wire form (EscapedPath) differs from what
// the server decodes (r.URL.Path) must be signed over the server-side form,
// or verification fails. A space-containing agent id exercises exactly that
// divergence (%20 on the wire, space in r.URL.Path).
func TestRemoteStore_SignatureContractPayload(t *testing.T) {
	agentID := "agent with spaces"
	srv, store, cap := newCapturingSigServer(t, true)

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Register(&Agent{ID: agentID, PublicKey: HexKey(pub)}); err != nil {
		t.Fatalf("register: %v", err)
	}
	rs := NewRemoteStore(srv.URL, agentID, "", WithSigningKey(priv))

	if _, _, _, err := rs.Stats(agentID); err != nil {
		t.Fatalf("signed Stats with escaped path: %v", err)
	}

	_, paths, sigs, tss := cap.snapshot()
	if len(paths) != 1 {
		t.Fatalf("captured %d signed requests, want 1", len(paths))
	}
	if paths[0] != "/agents/agent with spaces/inbox/stats" {
		t.Fatalf("server saw path %q", paths[0])
	}
	if strings.Contains(paths[0], "%20") {
		t.Fatalf("server path should be decoded, got %q", paths[0])
	}
	if !ed25519.Verify(pub, []byte("GET\n"+paths[0]+"\n"+tss[0]), sigs[0]) {
		t.Fatalf("signature does not verify over the decoded path %q", paths[0])
	}
}

// TestRemoteStore_SignedErrors pins failure modes: a missing key is a safe
// no-op (unsigned store, 401 from the server), and a wrong key yields 401 —
// not a crash or a silent pass.
func TestRemoteStore_SignedErrors(t *testing.T) {
	srv, store := newRemoteTestServerSigning(t, true)

	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	if err := store.Register(&Agent{ID: "gate-agent", PublicKey: HexKey(pub)}); err != nil {
		t.Fatalf("register: %v", err)
	}

	t.Run("no key configured fails safely with 401", func(t *testing.T) {
		rs := NewRemoteStore(srv.URL, "gate-agent", "")
		if _, _, err := rs.Retrieve("gate-agent", 30*time.Second, 5); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("unsigned Retrieve against signed server: %v (want 401 failure)", err)
		}
	})

	t.Run("wrong key is rejected with 401", func(t *testing.T) {
		rs := NewRemoteStore(srv.URL, "gate-agent", "", WithSigningKey(otherPriv))
		if _, _, err := rs.Retrieve("gate-agent", 30*time.Second, 5); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("wrong-key Retrieve: %v (want 401 failure)", err)
		}
	})

	t.Run("nil option is tolerated", func(t *testing.T) {
		// Callers building option slices conditionally may pass nil entries.
		rs := NewRemoteStore(srv.URL, "gate-agent", "", nil)
		if _, _, err := rs.Retrieve("gate-agent", 30*time.Second, 5); err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("nil-option Retrieve: %v (want 401 failure)", err)
		}
	})
}

// TestLoadEd25519PrivateKeyFile covers the crier-mcp startup loader: the
// openssl-shaped PKCS#8 ed25519 key parses; unreadable, malformed,
// non-PKCS#8, and non-ed25519 keys all fail explicitly and no key material
// leaks into error strings.
func TestLoadEd25519PrivateKeyFile(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	goodPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal rsa pkcs8: %v", err)
	}
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER})

	t.Run("valid pkcs8 ed25519", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agent.key")
		if err := os.WriteFile(path, goodPEM, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := LoadEd25519PrivateKeyFile(path)
		if err != nil {
			t.Fatalf("LoadEd25519PrivateKeyFile: %v", err)
		}
		if !bytes.Equal(got, priv) {
			t.Fatal("parsed key differs from original")
		}
	})

	t.Run("unreadable file", func(t *testing.T) {
		if _, err := LoadEd25519PrivateKeyFile(filepath.Join(t.TempDir(), "missing.key")); err == nil {
			t.Fatal("want error for missing file")
		}
	})

	t.Run("empty path", func(t *testing.T) {
		if _, err := LoadEd25519PrivateKeyFile(""); err == nil {
			t.Fatal("want error for empty path")
		}
	})

	t.Run("not pem", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "garbage.key")
		if err := os.WriteFile(path, []byte("definitely not a pem file"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := LoadEd25519PrivateKeyFile(path)
		if err == nil || !strings.Contains(err.Error(), "no PEM data") {
			t.Fatalf("err = %v, want no-PEM failure", err)
		}
		if strings.Contains(err.Error(), "definitely not") {
			t.Fatal("error echoed file contents")
		}
	})

	t.Run("wrong pem block type", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy.key")
		legacy := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{0x01}})
		if err := os.WriteFile(path, legacy, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := LoadEd25519PrivateKeyFile(path); err == nil || !strings.Contains(err.Error(), "EC PARAMETERS") {
			t.Fatalf("err = %v, want wrong-block-type failure", err)
		}
	})

	t.Run("malformed pkcs8 der", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "broken.key")
		broken := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{0x01, 0x02, 0x03}})
		if err := os.WriteFile(path, broken, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := LoadEd25519PrivateKeyFile(path); err == nil || !strings.Contains(err.Error(), "PKCS#8") {
			t.Fatalf("err = %v, want PKCS#8 parse failure", err)
		}
	})

	t.Run("non-ed25519 pkcs8 key (rsa)", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rsa.key")
		if err := os.WriteFile(path, rsaPEM, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, err := LoadEd25519PrivateKeyFile(path)
		if err == nil || !strings.Contains(err.Error(), "ed25519") {
			t.Fatalf("err = %v, want non-ed25519 failure", err)
		}
		if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(rsaKey.N.Bytes()[:8])) {
			t.Fatal("error leaked key material")
		}
	})

	t.Run("no key material in any error", func(t *testing.T) {
		// The raw private bytes must never surface in an error message.
		rawHex := hex.EncodeToString(priv.Seed())
		for _, tc := range []struct {
			name    string
			content []byte
		}{
			{"garbage", []byte(rawHex)},
			{"empty", []byte("")},
		} {
			path := filepath.Join(t.TempDir(), tc.name+".key")
			if err := os.WriteFile(path, tc.content, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := LoadEd25519PrivateKeyFile(path); err != nil && strings.Contains(err.Error(), rawHex[:16]) {
				t.Fatalf("%s: error leaked key material: %v", tc.name, err)
			}
		}
	})
}

// TestRemoteStore_RegisterCarriesTheNamespace (CR-FEAT-029) — a proxied
// registration must land the agent in the SAME realm. Without the forwarded
// member the downstream relay would register the agent in ITS default realm,
// which is a silent realm move: the agent would (with per-realm guard settings)
// silently leave the guarded realm, and every later realm check would be
// measured against the wrong row.
func TestRemoteStore_RegisterCarriesTheNamespace(t *testing.T) {
	t.Run("declared realm is forwarded and stored", func(t *testing.T) {
		store := NewMemoryStore()
		h := NewHandler(store)
		h.SetRequireAgentSig(false)
		reg, err := namespace.Parse(`{"namespaces":[{"name":"acme"}]}`, nil)
		if err != nil {
			t.Fatal(err)
		}
		h.SetNamespacePolicies(reg)
		r := mux.NewRouter()
		r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
		srv := httptest.NewServer(r)
		t.Cleanup(srv.Close)

		rs := NewRemoteStore(srv.URL, "remote-client", "")
		keyBytes, err := hex.DecodeString(strings.Repeat("cd", 32))
		if err != nil {
			t.Fatal(err)
		}
		agent := &Agent{ID: "alice", PublicKey: HexKey(keyBytes), Namespace: "acme"}
		if err := rs.Register(agent); err != nil {
			t.Fatalf("Register: %v", err)
		}
		stored, err := store.Get("alice")
		if err != nil {
			t.Fatal(err)
		}
		if stored.Namespace != "acme" {
			t.Fatalf("stored namespace = %q, want acme (the proxy dropped the realm)", stored.Namespace)
		}
	})

	t.Run("default realm sends no namespace member", func(t *testing.T) {
		// The downstream relay declares the realm and refuses an undeclared
		// one, so a proxy that invented a member would be refused here — this
		// proves the default realm's request is the body it always was.
		var got map[string]json.RawMessage
		recorder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"alice"}`))
		}))
		t.Cleanup(recorder.Close)

		rs := NewRemoteStore(recorder.URL, "remote-client", "")
		keyBytes, err := hex.DecodeString(strings.Repeat("ef", 32))
		if err != nil {
			t.Fatal(err)
		}
		if err := rs.Register(&Agent{ID: "alice", PublicKey: HexKey(keyBytes)}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if _, present := got["namespace"]; present {
			t.Fatalf("the default realm must not be sent on the wire: %v", got)
		}
	})
}
