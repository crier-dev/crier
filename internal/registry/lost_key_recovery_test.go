package registry

// DF-CRIER-515 — the lost-key recovery.
//
// The row: on a durable backend (the compose postgres volume), an id whose
// registered key file no longer exists is a permanent tombstone. The 409
// public_key_mismatch message prescribed "delete the agent first (DELETE
// /agents/{id})", but DELETE requires the signature of the REGISTERED key —
// exactly the key that was lost — so the remedy was unexecutable in the only
// situation the error occurs, and the documented happy path failed on first
// contact for anyone reusing a shared backend (live hit: dogfood tick
// crier-dogfood-2026-11-24-19-47-57, ids alice/bob registered 2026-09-30).
//
// The fix (direction (a) of the row: an operator-scoped registry delete):
// DELETE /agents/{id} also accepts the OPERATOR admin token
// (CR_PERMISSIONS_ADMIN_TOKEN) as its Bearer, independent of the delivery ACL,
// and the 409 message + README name that recovery.
//
// The contract these tests pin:
//
//   - a fresh-key registration over a taken id is STILL a structured 409 and
//     changes nothing — key immutability by design is intact (DF-CRIER-302);
//   - the 409 message names the operator remedy by its credential
//     (CR_PERMISSIONS_ADMIN_TOKEN) so following ONLY the message resolves it;
//   - signing DELETE with the NEW (unregistered) key is still the 401 dead
//     end it always was — no signature can substitute for a lost key;
//   - the documented recovery works end to end: admin Bearer → 204 → the id
//     re-registers with the fresh key (201), and GET answers 404 in between;
//   - every OTHER credential is not the operator credential: an unarmed
//     deployment keeps its tombstone, and a Bearer that is not the armed
//     admin token cannot delete either.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// operatorToken is the armed CR_PERMISSIONS_ADMIN_TOKEN of these tests. The
// value is arbitrary; what is pinned is that it is DISTINCT from any message
// token and that only this exact Bearer opens the operator delete.
const operatorToken = "operator-secret"

// newTestKeypair generates a full keypair: the hex public key a registration
// carries and the private half that signs agent-owned requests. Unlike
// newTestPubKey it keeps the private half, because the lost-key tests sign
// DELETEs with what the caller holds (and must be able to lose one on purpose).
func newTestKeypair(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(pub), priv
}

// serveRequest runs one prepared request through the router (the doJSON
// shape for requests that carry their own headers — the operator Bearer, or
// the agent signature trio a signedDelete builds).
func serveRequest(t *testing.T, r *mux.Router, req *http.Request) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// bearerReq builds a request with (only) an Authorization: Bearer header.
func bearerReq(method, path, token string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

// signedDelete builds a DELETE /agents/{id} signed with the given private key
// — the agent-owned shape, and the NEW-key arm the bug report followed.
func signedDelete(t *testing.T, priv ed25519.PrivateKey, id string) *http.Request {
	t.Helper()
	req := signedRequest(t, priv, id, http.MethodDelete, "/agents/"+id)
	req.Header.Del("Content-Type")
	return req
}

// registerWithKey registers id with a fresh public key over the router and
// asserts the 201, so every test below starts from a known-clean registration.
func registerWithKey(t *testing.T, r *mux.Router, id, pubHex string) {
	t.Helper()
	code, body := doJSON(t, r, http.MethodPost, "/agents",
		`{"id":"`+id+`","public_key":"`+pubHex+`"}`)
	require.Equal(t, http.StatusCreated, code, "control: the first registration must succeed: %s", body)
}

// TestLostKeyRecovery is the DF-CRIER-515 end-to-end gate: register fresh →
// 409 on the fresh key → the documented recovery (operator Bearer on DELETE)
// → re-register the same id with the fresh key → 201. Every step follows the
// 409 message and the README's registry section alone.
func TestLostKeyRecovery(t *testing.T) {
	t.Parallel()
	lostKey, lostPriv := newTestKeypair(t) // the key that "no longer exists" on disk
	freshKey, freshPriv := newTestKeypair(t)

	store := setupTestStore(t)
	handler, router := setupRouterWithHandler(store)
	handler.SetRequireAgentSig(true)                // the default posture (CR_REQUIRE_AGENT_SIG)
	handler.SetPermissionsAdminToken(operatorToken) // the operator credential is armed

	// 1. The id registers fresh (the backend had never seen it — the
	//    pre-tombstone world).
	registerWithKey(t, router, "alice", lostKey)

	// 2. A later registration with a DIFFERENT key (the lost one replaced by
	//    crier keygen) is refused with the structured 409, and the message
	//    names the operator remedy by its credential — following ONLY the
	//    message must resolve the situation.
	code, body := doJSON(t, router, http.MethodPost, "/agents",
		`{"id":"alice","public_key":"`+freshKey+`"}`)
	assertStructuredKeyConflict(t, "alice", code, body)
	conflict := decodeConflict(t, body)
	require.Contains(t, conflict.Message, "CR_PERMISSIONS_ADMIN_TOKEN",
		"the message must name the operator credential of the recovery: %s", body)

	// 3. The dead end the row documented: DELETE signed with the key you HOLD
	//    (the fresh one) is still 401 — the registered key is what must sign,
	//    and that key is gone. Nothing about the fix loosens this.
	code, body = serveRequest(t, router, signedDelete(t, freshPriv, "alice"))
	require.Equal(t, http.StatusUnauthorized, code,
		"a fresh-key signature must not delete the tombstoned identity: %s", body)

	// 4. The documented recovery: the same DELETE with the operator admin
	//    token as Bearer — no signature, because none could exist — deletes
	//    the tombstoned identity.
	code, body = serveRequest(t, router, bearerReq(http.MethodDelete, "/agents/alice", operatorToken))
	require.Equal(t, http.StatusNoContent, code,
		"the operator recovery must delete the identity: %s", body)

	// 5. The row is really gone…
	code, body = doJSON(t, router, http.MethodGet, "/agents/alice", "")
	require.Equal(t, http.StatusNotFound, code, "body: %s", body)

	// 6. …and the documented happy path now completes: the SAME fresh key
	//    registers, and the stored key is the fresh one.
	registerWithKey(t, router, "alice", freshKey)
	got, err := store.Get("alice")
	require.NoError(t, err)
	require.Equal(t, strings.ToLower(freshKey), hex.EncodeToString(got.PublicKey),
		"the recovered identity must carry the fresh key")
	_ = lostPriv // the lost key never signs anything again — that is the point
}

// TestLostKeyRecovery_UnarmedDeploymentKeepsTheTombstone pins AC arm 3 of the
// row: WITHOUT the operator credential armed, nothing changed — the fresh-key
// registration is still a 409 and even an admin-LOOKING Bearer cannot delete,
// because the gate is closed. Key immutability by design is intact.
func TestLostKeyRecovery_UnarmedDeploymentKeepsTheTombstone(t *testing.T) {
	t.Parallel()
	firstKey, _ := newTestKeypair(t)
	freshKey, _ := newTestKeypair(t)

	store := setupTestStore(t)
	handler, router := setupRouterWithHandler(store)
	handler.SetRequireAgentSig(true)
	// NO SetPermissionsAdminToken: the deployment armed no operator credential.

	registerWithKey(t, router, "bob", firstKey)

	code, body := doJSON(t, router, http.MethodPost, "/agents",
		`{"id":"bob","public_key":"`+freshKey+`"}`)
	assertStructuredKeyConflict(t, "bob", code, body)

	code, body = serveRequest(t, router, bearerReq(http.MethodDelete, "/agents/bob", "some-secret"))
	require.Equal(t, http.StatusUnauthorized, code,
		"with no admin token armed the delete must stay signature-gated: %s", body)
	if _, err := store.Get("bob"); err != nil {
		t.Fatal("the refused operator delete must leave the registration intact")
	}

	// …and the tombstone stands: the fresh key is still refused 409.
	code, body = doJSON(t, router, http.MethodPost, "/agents",
		`{"id":"bob","public_key":"`+freshKey+`"}`)
	assertStructuredKeyConflict(t, "bob", code, body)
}

// TestLostKeyRecovery_WrongBearerIsNotTheOperatorCredential: a deployment
// that armed the admin token keeps every OTHER credential out — a Bearer that
// is not the armed admin token (a leaked message token, a wrong secret, or no
// Bearer at all) cannot delete an identity it cannot sign for.
func TestLostKeyRecovery_WrongBearerIsNotTheOperatorCredential(t *testing.T) {
	t.Parallel()
	for name, token := range map[string]string{
		"message-token bearer": "message-secret",
		"wrong secret":         "operator-secret-wrong",
		"no bearer":            "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			key, _ := newTestKeypair(t)

			store := setupTestStore(t)
			handler, router := setupRouterWithHandler(store)
			handler.SetRequireAgentSig(true)
			handler.SetPermissionsAdminToken(operatorToken)

			registerWithKey(t, router, "carol", key)

			code, body := serveRequest(t, router, bearerReq(http.MethodDelete, "/agents/carol", token))
			require.Equal(t, http.StatusUnauthorized, code,
				"a non-admin Bearer must not reach the operator delete: %s", body)
			if _, err := store.Get("carol"); err != nil {
				t.Fatalf("the refused delete must leave the registration intact (bearer %q)", token)
			}
		})
	}
}

// TestLostKeyRecovery_AgentOwnedDeleteStillWorks pins the arm the fix must not
// disturb: the agent's OWN registered key still deletes itself over the same
// route (the historical TestHandleUnregister_SignedSucceeds contract, walked
// through the router).
func TestLostKeyRecovery_AgentOwnedDeleteStillWorks(t *testing.T) {
	t.Parallel()
	key, priv := newTestKeypair(t)

	store := setupTestStore(t)
	handler, router := setupRouterWithHandler(store)
	handler.SetRequireAgentSig(true)
	handler.SetPermissionsAdminToken(operatorToken)

	registerWithKey(t, router, "dave", key)

	code, body := serveRequest(t, router, signedDelete(t, priv, "dave"))
	require.Equal(t, http.StatusNoContent, code,
		"the agent-owned signed delete is unchanged: %s", body)
	if _, err := store.Get("dave"); err == nil {
		t.Fatal("the signed delete must have removed the agent")
	}
}
