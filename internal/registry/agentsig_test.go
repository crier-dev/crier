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

// --- DF-CRIER-236: a PRESENT but unusable X-Agent-Sig must be reported as
// itself, not as a missing header. The three client mistakes below used to
// share one message ("missing agent signature headers …"), which sent the
// reader to debug headers that were all present and correct. The pre-fix
// shape was a client whose signing helper produced NO output: `openssl pkeyutl
// -sign -rawin` is a one-shot operation that needs a SEEKABLE payload, so a
// piped or redirected payload makes it fail with "unable to determine file
// size for oneshot operation" and emit zero bytes — which the helper's
// `2>/dev/null` hid, so the client sent `X-Agent-Sig:` empty.

// sigHeaderRequest builds a request with valid X-Agent-ID and X-Agent-Ts but a
// verbatim X-Agent-Sig value — including "" (header present, no value) and
// whitespace-only, the exact shape a broken signing helper produces.
func sigHeaderRequest(t *testing.T, agentID, sig string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/agents/"+agentID+"/inbox", nil)
	req.Header.Set(HeaderAgentID, agentID)
	req.Header.Set(HeaderAgentTS, fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set(HeaderAgentSig, sig) // Set stores the value verbatim, "" included
	return req
}

// deniedMessage drives authorizeAgent against req (expected to be rejected) and
// returns the status, the DECODED error string, and the raw body. Assertions
// read the decoded string on purpose: the raw wire body is JSON with the
// default HTML escaping, so "<file>" arrives as "\u003cfile\u003e" and a quoted
// value as \"-escaped — a raw-body Contains on those shapes fails on a correct
// response.
func deniedMessage(t *testing.T, h *Handler, req *http.Request, targetID string) (int, string, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, targetID); err == nil {
		t.Fatal("expected the request to be rejected")
	}
	raw := rec.Body.String()
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body is not the {\"error\": \"…\"} shape: %v (raw body %q)", err, raw)
	}
	return rec.Code, resp["error"], raw
}

// TestAuthorizeAgent_EmptySignatureNamesTheCause pins the empty (and
// whitespace-only) X-Agent-Sig branch: 401, NOT the missing-headers message,
// and a body that names the empty signature and the non-seekable-payload cause.
func TestAuthorizeAgent_EmptySignatureNamesTheCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  string
	}{
		{"empty value", ""},
		{"whitespace only", "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			h := newSigHandler(store)
			_, _ = testAgentKeypair(t, store, "agent-1")

			status, msg, raw := deniedMessage(t, h, sigHeaderRequest(t, "agent-1", tc.sig), "agent-1")
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", status)
			}
			if strings.Contains(raw, "missing agent signature headers") {
				t.Fatalf("body = %q: a PRESENT but empty signature must not be reported as a missing header", raw)
			}
			for _, want := range []string{
				"X-Agent-Sig is present but empty",
				"seekable",
				"-in <file>",
				"zero-byte signature",
			} {
				if !strings.Contains(msg, want) {
					t.Fatalf("error = %q, want it to name the cause (missing %q)", msg, want)
				}
			}
		})
	}
}

// TestAuthorizeAgent_ShortSignatureNamesTheLength pins the wrong-length branch:
// 6 hex chars is not hex rubbish, so the message must report the expected
// 128-char length AND the actual value/length rather than a generic line.
func TestAuthorizeAgent_ShortSignatureNamesTheLength(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, _ = testAgentKeypair(t, store, "agent-1")

	status, msg, raw := deniedMessage(t, h, sigHeaderRequest(t, "agent-1", "abcdef"), "agent-1")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	for _, want := range []string{"expected 128 hex chars", "got 6 character(s)", `"abcdef"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want it to contain %q", msg, want)
		}
	}
	for _, unwanted := range []string{"missing agent signature headers", "not hex"} {
		if strings.Contains(msg, unwanted) {
			t.Fatalf("error = %q, want the wrong-length message (unexpected %q)", msg, unwanted)
		}
	}
	if strings.Contains(raw, "missing agent signature headers") {
		t.Fatalf("body = %q, want the malformed-signature message", raw)
	}
}

// TestAuthorizeAgent_NonHexSignatureSaysNotHex pins the non-hex branch: the
// message must say the value is not hex, not merely report a length.
func TestAuthorizeAgent_NonHexSignatureSaysNotHex(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, _ = testAgentKeypair(t, store, "agent-1")

	status, msg, raw := deniedMessage(t, h, sigHeaderRequest(t, "agent-1", "zzzz"), "agent-1")
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	for _, want := range []string{"not hex", `"zzzz"`} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error = %q, want it to contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "character(s)") {
		t.Fatalf("error = %q: a non-hex value must be reported as not-hex, not as a wrong length", msg)
	}
	if strings.Contains(raw, "missing agent signature headers") {
		t.Fatalf("body = %q, want the malformed-signature message", raw)
	}
}

// TestAuthorizeAgent_MissingSignatureHeaderMessageUnchanged guards the other
// side of the split: with X-Agent-ID (and X-Agent-Ts) truly absent the original
// message must survive verbatim — a caller with no signature at all must not be
// told to debug an empty signing step.
func TestAuthorizeAgent_MissingHeaderMessageUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setHeaders func(*http.Request)
	}{
		{"no headers at all", func(*http.Request) {}},
		{"X-Agent-Ts absent", func(r *http.Request) { r.Header.Set(HeaderAgentID, "agent-1") }},
		{"X-Agent-ID absent", func(r *http.Request) { r.Header.Set(HeaderAgentTS, "1700000000") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			h := newSigHandler(store)
			_, _ = testAgentKeypair(t, store, "agent-1")

			req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/inbox", nil)
			tc.setHeaders(req)
			rec := httptest.NewRecorder()
			if err := h.authorizeAgent(rec, req, "agent-1"); err == nil {
				t.Fatal("expected missing headers to be rejected")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if body := rec.Body.String(); !strings.Contains(body, "missing agent signature headers") {
				t.Fatalf("body = %q, want the original missing-headers message", body)
			}
		})
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

// DF-CRIER-294 — the other half of the ±30 s contract, and the reason the
// docs (docs/mesh-protocol.md § "Replay protection on the signed lanes") state
// that the window IS the whole replay defence on these routes: the lane keeps
// no nonce or signature cache, so a byte-identical signed request presented
// again INSIDE the window is ACCEPTED. This test pins the DOCUMENTED trade from
// the executable side. If a nonce/signature cache is ever added, invert this
// test TOGETHER with that doc paragraph — the pair is the contract, and a
// silent change to either half is the drift this test exists to catch.
func TestAuthorizeAgent_InWindowReplayIsAccepted_DocumentedTrade(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, priv := testAgentKeypair(t, store, "agent-1")

	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := hex.EncodeToString(ed25519.Sign(priv, []byte(http.MethodGet+"\n/agents/agent-1/inbox\n"+ts)))

	// ONE signed request, presented twice with the SAME timestamp + signature.
	for i := 1; i <= 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/inbox", nil)
		req.Header.Set(HeaderAgentID, "agent-1")
		req.Header.Set(HeaderAgentTS, ts)
		req.Header.Set(HeaderAgentSig, sig)
		rec := httptest.NewRecorder()
		if err := h.authorizeAgent(rec, req, "agent-1"); err != nil {
			t.Errorf("presentation %d: authorizeAgent = %v, want the in-window replay ACCEPTED (documented trade: ±%s is the whole defence)", i, err, sigWindow)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("presentation %d: status = %d, want 200", i, rec.Code)
		}
	}

	// Outside the window the same lane refuses 401 — that refusal is what makes
	// the window the defence at all.
	stale := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	staleSig := hex.EncodeToString(ed25519.Sign(priv, []byte(http.MethodGet+"\n/agents/agent-1/inbox\n"+stale)))
	req := httptest.NewRequest(http.MethodGet, "/agents/agent-1/inbox", nil)
	req.Header.Set(HeaderAgentID, "agent-1")
	req.Header.Set(HeaderAgentTS, stale)
	req.Header.Set(HeaderAgentSig, staleSig)
	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, "agent-1"); err == nil {
		t.Errorf("out-of-window timestamp accepted: the ±%s window is the whole replay defence", sigWindow)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
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

// TestAuthorizeAgent_KeylessAgentFailsClosed401 pins the DF-CRIER-192
// fail-closed contract: an agent stored WITHOUT a public key (registered
// while enforcement was off) that presents signature headers on a server
// with enforcement ON gets 401 naming the real reason — never a 500 (the
// pre-fix "invalid stored public key" Internal Server Error), never a 200.
func TestAuthorizeAgent_KeylessAgentFailsClosed401(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	// Keyless agent: registered with an empty key (enforcement off at the
	// time), like the README dev shortcut leaves behind.
	if err := store.Register(&Agent{ID: "keyless", PublicKey: HexKey(nil), Capabilities: []string{}}); err != nil {
		t.Fatal(err)
	}
	_, somePriv, _ := ed25519.GenerateKey(rand.Reader)

	req := signedRequest(t, somePriv, "keyless", http.MethodGet, "/agents/keyless/inbox")
	rec := httptest.NewRecorder()
	if err := h.authorizeAgent(rec, req, "keyless"); err == nil {
		t.Fatal("expected keyless agent authorization to fail")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (never 500, never 200)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no registered public key") {
		t.Fatalf("body = %q, want it to name the missing key", rec.Body.String())
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
