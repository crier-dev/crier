package registry

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// INT-018: ordinary inbox delivery must authenticate the sender it stores.
//
// Before this gate, POST /agents/{id}/inbox with CR_REQUIRE_AGENT_SIG=true
// answered 201 for a request carrying no X-Agent-* headers and stored whatever
// `sender` the body claimed. The gate reuses the shipped agent-signature
// verifier (verifyAgentSignature) — no new header set, no new crypto.

// signedDeliverPost sends body to target's inbox as senderID, signing the
// delivered request with priv. bindBody selects the opt-in body-binding form
// (CR-CHAT-027) that RemoteStore uses for POSTs; otherwise the legacy 3-line
// transcript is signed.
func signedDeliverPost(t *testing.T, h *cr36Harness, senderID string, priv ed25519.PrivateKey, target, body string, bindBody bool) (int, string) {
	t.Helper()
	path := "/agents/" + target + "/inbox"
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	digest := ""
	if bindBody {
		digest = sha256Hex([]byte(body))
		req.Header.Set(HeaderAgentBodySHA256, digest)
	}
	sig := ed25519.Sign(priv, agentSigPayload(http.MethodPost, path, ts, digest))
	req.Header.Set(HeaderAgentID, senderID)
	req.Header.Set(HeaderAgentTS, ts)
	req.Header.Set(HeaderAgentSig, hex.EncodeToString(sig))

	resp, err := h.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

// unsignedDeliverPost sends body with no signature headers at all.
func unsignedDeliverPost(t *testing.T, h *cr36Harness, target, body string) (int, string) {
	t.Helper()
	return h.send(t, http.MethodPost, "/agents/"+target+"/inbox", body, nil)
}

// requireNothingStored fails when the target's inbox holds a message: a
// rejected delivery must not leak the claimed sender (or the payload) into the
// store.
func requireNothingStored(t *testing.T, h *cr36Harness, target string) {
	t.Helper()
	entries, _, err := h.store.Retrieve(target, 30*time.Second, 10)
	require.NoError(t, err)
	require.Empty(t, entries, "a rejected delivery must store nothing")
}

// TestDeliverUnsignedIsRejectedWhenSigningEnforced is the regression proof of
// this row: the exact 201 -> non-2xx flip for an unsigned ordinary deliver. It
// fails on the base commit (which answered 201 and stored the claimed sender).
func TestDeliverUnsignedIsRejectedWhenSigningEnforced(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.handler.SetRequireAgentSig(true)
	h.register(t, "recipient")

	status, body := unsignedDeliverPost(t, h, "recipient", `{"sender":"intruder","payload":{"task":"take over"}}`)
	require.Equal(t, http.StatusUnauthorized, status,
		"an unsigned ordinary deliver must be refused when signing is enforced; got %d: %s", status, body)
	require.Contains(t, body, "missing agent signature headers",
		"the refusal is the shipped signature-failure shape: %s", body)
	requireNothingStored(t, h, "recipient")
}

// TestDeliverSignedIsAcceptedAndSenderVerified proves the positive direction:
// a properly signed delivery is accepted and the sender STORED is the sender
// that proved the key — not an unchecked claim.
func TestDeliverSignedIsAcceptedAndSenderVerified(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.handler.SetRequireAgentSig(true)
	_, senderKey := testAgentKeypair(t, h.store, "sender-agent")
	h.register(t, "recipient")

	// Body binding is the form RemoteStore uses for POSTs; the gate verifies it
	// against the delivered bytes.
	status, body := signedDeliverPost(t, h, "sender-agent", senderKey, "recipient",
		`{"sender":"sender-agent","payload":{"task":"signed"}}`, true)
	require.Equal(t, http.StatusCreated, status, "a correctly signed deliver must be accepted: %s", body)

	entries, _, err := h.store.Retrieve("recipient", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "sender-agent", entries[0].Sender, "the stored sender is the VERIFIED sender")
}

// TestDeliverSignedWithoutSenderClaimIsAccepted pins the compatibility rule: a
// signed delivery that names no sender (the shape RemoteStore.Deliver produces
// when InboxEntry.Sender is empty) makes no claim to forge and stays accepted.
func TestDeliverSignedWithoutSenderClaimIsAccepted(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.handler.SetRequireAgentSig(true)
	_, senderKey := testAgentKeypair(t, h.store, "sender-agent")
	h.register(t, "recipient")

	status, body := signedDeliverPost(t, h, "sender-agent", senderKey, "recipient",
		`{"payload":{"task":"no sender claim"}}`, true)
	require.Equal(t, http.StatusCreated, status, "a signed deliver with no sender claim must be accepted: %s", body)
}

// TestDeliverForgedSignatureIsRejected proves a signature that verifies against
// the WRONG key (a forged or replayed identity) is refused before any store
// write.
func TestDeliverForgedSignatureIsRejected(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.handler.SetRequireAgentSig(true)
	testAgentKeypair(t, h.store, "victim-agent")
	_, forgerKey := testAgentKeypair(t, h.store, "forger-agent")
	h.register(t, "recipient")

	// The request claims to be victim-agent but signs with the forger's key.
	status, body := signedDeliverPost(t, h, "victim-agent", forgerKey, "recipient",
		`{"sender":"victim-agent","payload":{"task":"forged"}}`, true)
	require.Equal(t, http.StatusUnauthorized, status,
		"a signature from a different key must be refused; got %d: %s", status, body)
	require.Contains(t, body, "signature verification failed", "shipped signature-failure body: %s", body)
	requireNothingStored(t, h, "recipient")
}

// TestDeliverSignedButMismatchedSenderIsRejected proves the body cannot claim a
// sender other than the one whose key just signed: same 403 the protected
// service-delivery path uses for this mismatch.
func TestDeliverSignedButMismatchedSenderIsRejected(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.handler.SetRequireAgentSig(true)
	_, honestKey := testAgentKeypair(t, h.store, "honest-agent")
	h.register(t, "recipient")

	status, body := signedDeliverPost(t, h, "honest-agent", honestKey, "recipient",
		`{"sender":"somebody-else","payload":{"task":"impersonation"}}`, true)
	require.Equal(t, http.StatusForbidden, status,
		"a signed request may not store a different sender; got %d: %s", status, body)
	require.Contains(t, body, "delivery body sender must equal the signed X-Agent-ID", "named refusal: %s", body)
	requireNothingStored(t, h, "recipient")
}

// TestDeliverUnsignedAcceptedWhenSigningOff pins the gate-off direction:
// behaviour is exactly what it was before this row.
func TestDeliverUnsignedAcceptedWhenSigningOff(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.register(t, "recipient")

	status, body := unsignedDeliverPost(t, h, "recipient", `{"sender":"anyone","payload":{"task":"legacy"}}`)
	require.Equal(t, http.StatusCreated, status, "gate off keeps the legacy unsigned deliver: %s", body)

	entries, _, err := h.store.Retrieve("recipient", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "anyone", entries[0].Sender, "gate off stores the claim exactly as before")
}

// TestDeliverInProcessBindingSkipsSenderGate pins the ONE exemption: an
// in-process protocol translation (cmd/server's A2A JSON-RPC binding) re-issues
// the delivery with no X-Agent-* headers by construction, so it marks the
// request context instead. A context value cannot arrive over the wire; every
// HTTP delivery still has to prove its sender.
func TestDeliverInProcessBindingSkipsSenderGate(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	require.NoError(t, store.Register(&Agent{ID: "recipient"}))
	h := NewHandler(store)
	h.SetRequireAgentSig(true)

	req := httptest.NewRequest(http.MethodPost, "/agents/recipient/inbox",
		strings.NewReader(`{"sender":"binding-sender","payload":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req = mux.SetURLVars(req, map[string]string{"id": "recipient"})
	req = req.WithContext(InProcessDelivery(req.Context()))
	rec := httptest.NewRecorder()
	h.HandleDeliver(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code,
		"an in-process binding delivery keeps its pre-INT-018 behaviour: %s", rec.Body.String())
}

// TestDeliverUnsignedRejectionCarriesShippedShape pins the exact response shape
// a client branches on: a JSON object with a single `error` string, 401.
func TestDeliverUnsignedRejectionCarriesShippedShape(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.handler.SetRequireAgentSig(true)
	h.register(t, "recipient")

	status, body := unsignedDeliverPost(t, h, "recipient", `{"sender":"x","payload":{}}`)
	require.Equal(t, http.StatusUnauthorized, status)
	require.True(t, strings.HasPrefix(body, "{"), "JSON object body: %s", body)
	require.Contains(t, body, fmt.Sprintf(`%q:`, "error"), "the single documented `error` member: %s", body)
}
