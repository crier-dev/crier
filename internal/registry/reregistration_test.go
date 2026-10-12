package registry

// DF-CRIER-302 — re-registration with a NEW public_key.
//
// The row: a live dogfood run re-registered an agent with a different key and
// answered 409 `{"error":"agent already registered"}` — a body that names the
// collision but not its CONSEQUENCE. The operator who pasted the wrong key
// once (or who intended to rotate) is left with an agent whose signed requests
// 401 forever and no hint that the key is immutable or how to recover.
//
// The contract these tests pin:
//
//   - a re-registration supplying DIFFERENT key material is a class of its own
//     (ErrPublicKeyMismatch) and answers 409 with a structured body carrying a
//     machine-readable `error` code plus a human `message` naming the
//     consequence and the remedy;
//   - the same conflict is reported identically by BOTH spellings of the
//     registration endpoint — POST /agents (id in the body) and POST
//     /agents/{id} (id in the path);
//   - a re-registration supplying the SAME key material keeps the historical
//     duplicate class and body, unchanged;
//   - the refused attempt changes NOTHING: the stored key, capabilities and
//     webhook are the ones the FIRST registration wrote;
//   - a relay older than this code (a 409 with no such code in the body) is
//     still read as the plain duplicate, so the new class is never fabricated.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/webhook"
)

// reregistrationConflictBody is the structured 409 body the key conflict
// carries: `error` is the machine-readable code, `message` the human
// description. Both are required — an `error` alone is what the row found
// unhelpful.
type reregistrationConflictBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// decodeConflict decodes a 409 body and returns it, failing loudly when the
// body is not the structured conflict shape.
func decodeConflict(t *testing.T, body string) reregistrationConflictBody {
	t.Helper()
	var out reregistrationConflictBody
	require.NoError(t, json.Unmarshal([]byte(body), &out),
		"the 409 body must be JSON: %s", body)
	return out
}

// assertStructuredKeyConflict asserts the full DF-CRIER-302 409 contract: the
// status, the machine code on the wire, and a message that names the id, the
// immutability of a registered key, that nothing changed, and the remedy.
func assertStructuredKeyConflict(t *testing.T, id string, status int, body string) {
	t.Helper()
	require.Equal(t, http.StatusConflict, status, "body: %s", body)

	conflict := decodeConflict(t, body)
	// The wire literal is asserted, not only the exported constant: renaming
	// the constant must not silently change the published contract.
	require.Equal(t, "public_key_mismatch", conflict.Error,
		"the machine-readable code is the documented contract: %s", body)
	require.Equal(t, CodePublicKeyMismatch, conflict.Error,
		"the emitted code and the exported constant must be the same string: %s", body)

	require.NotEmpty(t, conflict.Message, "the 409 must carry a human message: %s", body)
	require.Contains(t, conflict.Message, id, "the message must name the agent: %s", body)
	require.Contains(t, conflict.Message, "immutable",
		"the message must say a registered key is immutable: %s", body)
	require.Contains(t, conflict.Message, "unchanged",
		"the message must say the registration was left unchanged: %s", body)
	require.Contains(t, conflict.Message, "DELETE /agents/"+id,
		"the message must name the remedy (unregister): %s", body)
}

// TestReRegistration_NewPublicKeyInBodyIsAStructured409 is the headline case on
// the body spelling: POST /agents with an id that is already registered under a
// different key.
func TestReRegistration_NewPublicKeyInBodyIsAStructured409(t *testing.T) {
	firstKey, _ := newTestPubKey(t)
	secondKey, _ := newTestPubKey(t)

	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)

	first, err := json.Marshal(registerRequest{
		ID:           "rotator",
		PublicKey:    firstKey,
		Capabilities: []string{"relay", "mesh"},
	})
	require.NoError(t, err)
	code, body := doJSON(t, router, http.MethodPost, "/agents", string(first))
	require.Equal(t, http.StatusCreated, code, "control: the first registration must succeed: %s", body)

	second, err := json.Marshal(registerRequest{ID: "rotator", PublicKey: secondKey})
	require.NoError(t, err)
	code, body = doJSON(t, router, http.MethodPost, "/agents", string(second))
	assertStructuredKeyConflict(t, "rotator", code, body)

	// The registration the conflict was raised against is intact: the ORIGINAL
	// key (so the agent keeps authenticating) and the original capabilities.
	got, err := store.Get("rotator")
	require.NoError(t, err)
	require.Equal(t, strings.ToLower(firstKey), hex.EncodeToString(got.PublicKey),
		"the refused attempt must not have changed the registered key")
	require.Equal(t, []string{"relay", "mesh"}, got.Capabilities,
		"the refused attempt must not have changed the stored capabilities")
}

// TestReRegistration_NewPublicKeyOnIDPathIsAStructured409 is the same case on
// the id-in-path spelling — the spelling the row's acceptance criterion names —
// with a control proving the route itself serves a first registration.
func TestReRegistration_NewPublicKeyOnIDPathIsAStructured409(t *testing.T) {
	firstKey, _ := newTestPubKey(t)
	secondKey, _ := newTestPubKey(t)

	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)

	// Control: the route registers an id that is NOT taken (201), so the 409
	// below cannot be the route refusing the request for some other reason.
	code, body := doJSON(t, router, http.MethodPost, "/agents/path-rotator",
		`{"public_key":"`+firstKey+`","capabilities":["relay"]}`)
	require.Equal(t, http.StatusCreated, code, "control: the route must register a fresh id: %s", body)

	code, body = doJSON(t, router, http.MethodPost, "/agents/path-rotator",
		`{"public_key":"`+secondKey+`"}`)
	assertStructuredKeyConflict(t, "path-rotator", code, body)

	// The registered key is the first one, read back through the ordinary
	// detail route.
	code, body = doJSON(t, router, http.MethodGet, "/agents/path-rotator", "")
	require.Equal(t, http.StatusOK, code, "body: %s", body)
	var stored Agent
	require.NoError(t, json.Unmarshal([]byte(body), &stored))
	require.Equal(t, strings.ToLower(firstKey), hex.EncodeToString(stored.PublicKey),
		"the refused attempt must not have changed the registered key")

	// And the store agrees: the second spelling reached the same registration.
	got, err := store.Get("path-rotator")
	require.NoError(t, err)
	require.Equal(t, []string{"relay"}, got.Capabilities)
}

// TestReRegistration_SamePublicKeyKeepsTheDuplicateClass pins the arm the row
// did NOT change: re-registering with the SAME key is still the historical
// duplicate, with its historical body, so no existing client's handling of a
// duplicate changes.
func TestReRegistration_SamePublicKeyKeepsTheDuplicateClass(t *testing.T) {
	key, _ := newTestPubKey(t)

	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)

	body, err := json.Marshal(registerRequest{ID: "dupe", PublicKey: key})
	require.NoError(t, err)
	code, resp := doJSON(t, router, http.MethodPost, "/agents", string(body))
	require.Equal(t, http.StatusCreated, code, "control: %s", resp)

	code, resp = doJSON(t, router, http.MethodPost, "/agents", string(body))
	require.Equal(t, http.StatusConflict, code, "body: %s", resp)

	conflict := decodeConflict(t, resp)
	require.Contains(t, conflict.Error, "already registered",
		"the same-key duplicate keeps the historical body: %s", resp)
	require.NotEqual(t, CodePublicKeyMismatch, conflict.Error,
		"the same key material is not a key conflict: %s", resp)
	require.Empty(t, conflict.Message,
		"the historical duplicate body is unchanged — no message member is added here: %s", resp)
}

// TestReRegistration_HexCaseIsNotAKeyConflict: the same key spelled in
// uppercase hex is the same key. Comparing the DECODED bytes keeps a
// case-different spelling from reading as a rotation attempt.
func TestReRegistration_HexCaseIsNotAKeyConflict(t *testing.T) {
	key, _ := newTestPubKey(t)

	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)

	code, resp := doJSON(t, router, http.MethodPost, "/agents",
		`{"id":"cased","public_key":"`+key+`"}`)
	require.Equal(t, http.StatusCreated, code, "control: %s", resp)

	code, resp = doJSON(t, router, http.MethodPost, "/agents",
		`{"id":"cased","public_key":"`+strings.ToUpper(key)+`"}`)
	require.Equal(t, http.StatusConflict, code, "body: %s", resp)
	conflict := decodeConflict(t, resp)
	require.NotEqual(t, CodePublicKeyMismatch, conflict.Error,
		"the same key in another hex case is a duplicate, not a rotation: %s", resp)
}

// TestReRegistration_ConflictPreservesTheStoredRegistration drives the case
// where the refused re-registration also carries different capabilities and a
// different webhook: nothing about the stored row may move.
func TestReRegistration_ConflictPreservesTheStoredRegistration(t *testing.T) {
	firstKey, _ := newTestPubKey(t)
	secondKey, _ := newTestPubKey(t)

	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)

	original := &webhook.Config{URL: "http://hook.local/original"}
	first, err := json.Marshal(registerRequest{
		ID:           "keeper",
		PublicKey:    firstKey,
		Capabilities: []string{"relay"},
		Webhook:      original,
	})
	require.NoError(t, err)
	code, body := doJSON(t, router, http.MethodPost, "/agents", string(first))
	require.Equal(t, http.StatusCreated, code, "control: %s", body)

	replacement, err := json.Marshal(registerRequest{
		ID:           "keeper",
		PublicKey:    secondKey,
		Capabilities: []string{"other"},
		Webhook:      &webhook.Config{URL: "http://hook.local/replacement"},
	})
	require.NoError(t, err)
	code, body = doJSON(t, router, http.MethodPost, "/agents", string(replacement))
	assertStructuredKeyConflict(t, "keeper", code, body)

	got, err := store.Get("keeper")
	require.NoError(t, err)
	require.Equal(t, strings.ToLower(firstKey), hex.EncodeToString(got.PublicKey))
	require.Equal(t, []string{"relay"}, got.Capabilities)
	require.NotNil(t, got.Webhook)
	require.Equal(t, original.URL, got.Webhook.URL,
		"a refused re-registration must not repoint the stored webhook")
}

// TestReRegistration_KeylessOverKeyedIsAConflict: with signing disabled (the
// documented dev shortcut) an omitted key is legal, and registering keyless
// over an id that HAS a key would strip the key the agent signs with. That is
// the same class of conflict, not a silent downgrade.
func TestReRegistration_KeylessOverKeyedIsAConflict(t *testing.T) {
	key, _ := newTestPubKey(t)

	store := setupTestStore(t)
	handler, router := setupRouterWithHandler(store)
	// The README dev shortcut (CR_REQUIRE_AGENT_SIG=false) — the only
	// configuration in which a keyless registration is accepted at all.
	handler.SetRequireAgentSig(false)

	code, body := doJSON(t, router, http.MethodPost, "/agents",
		`{"id":"keyed","public_key":"`+key+`"}`)
	require.Equal(t, http.StatusCreated, code, "control: %s", body)

	code, body = doJSON(t, router, http.MethodPost, "/agents", `{"id":"keyed"}`)
	assertStructuredKeyConflict(t, "keyed", code, body)

	got, err := store.Get("keyed")
	require.NoError(t, err)
	require.Equal(t, strings.ToLower(key), hex.EncodeToString(got.PublicKey),
		"the key must survive a keyless re-registration attempt")
}

// TestReRegistration_PathAndBodyIDMustAgree: POST /agents/{id} with a body that
// names a DIFFERENT id is refused (400) and registers neither id, rather than
// resolving the disagreement silently in either direction.
func TestReRegistration_PathAndBodyIDMustAgree(t *testing.T) {
	key, _ := newTestPubKey(t)

	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)

	code, body := doJSON(t, router, http.MethodPost, "/agents/path-agent",
		`{"id":"body-agent","public_key":"`+key+`"}`)
	require.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	conflict := decodeConflict(t, body)
	require.Contains(t, conflict.Error, "path-agent")
	require.Contains(t, conflict.Error, "body-agent")

	for _, id := range []string{"path-agent", "body-agent"} {
		if _, err := store.Get(id); !errors.Is(err, ErrAgentNotFound) {
			t.Fatalf("agent %q must not have been registered by a mismatched request (err=%v)", id, err)
		}
	}
}

// TestReRegistration_StoreReportsTheConflictClass pins the store-level contract
// the two HTTP spellings are built on, including the two classes being
// DISTINCT sentinels (the bridge and the handlers switch on them).
func TestReRegistration_StoreReportsTheConflictClass(t *testing.T) {
	_, firstKey := newTestPubKey(t)
	_, secondKey := newTestPubKey(t)

	store := setupTestStore(t)
	require.NoError(t, store.Register(&Agent{ID: "stored", PublicKey: HexKey(firstKey)}))

	// Same key material → the historical duplicate class.
	err := store.Register(&Agent{ID: "stored", PublicKey: HexKey(firstKey)})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAgentExists), "got %v", err)
	require.False(t, errors.Is(err, ErrPublicKeyMismatch), "got %v", err)

	// Different key material → the conflict class, and NOT the duplicate one.
	err = store.Register(&Agent{ID: "stored", PublicKey: HexKey(secondKey)})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPublicKeyMismatch), "got %v", err)
	require.False(t, errors.Is(err, ErrAgentExists),
		"the two conflict classes must stay distinguishable: %v", err)
	require.NotEqual(t, ErrAgentExists.Error(), ErrPublicKeyMismatch.Error())

	// A keyless attempt over a keyed row differs too (it would remove the key).
	err = store.Register(&Agent{ID: "stored", PublicKey: HexKey(nil)})
	require.True(t, errors.Is(err, ErrPublicKeyMismatch), "got %v", err)

	// The stored row never moved.
	got, err := store.Get("stored")
	require.NoError(t, err)
	require.Equal(t, []byte(firstKey), []byte(got.PublicKey))
}

// TestReRegistration_RemoteStoreDistinguishesTheConflict drives the REAL
// handler through RemoteStore — the transport the crier-mcp bridge registers
// agents with — and requires the bridge to see the conflict CLASS rather than a
// flat "already registered".
func TestReRegistration_RemoteStoreDistinguishesTheConflict(t *testing.T) {
	srv, _ := newRemoteTestServerSigning(t, false)
	rs := NewRemoteStore(srv.URL, "probe-client", "")

	_, key := newTestPubKey(t)
	_, otherKey := newTestPubKey(t)

	require.NoError(t, rs.Register(&Agent{ID: "remote-dup", PublicKey: HexKey(key)}))

	// Same key → the historical duplicate mapping (unchanged).
	err := rs.Register(&Agent{ID: "remote-dup", PublicKey: HexKey(key)})
	require.True(t, errors.Is(err, ErrAgentExists), "got %v", err)
	require.False(t, errors.Is(err, ErrPublicKeyMismatch), "got %v", err)

	// Different key → the conflict class.
	err = rs.Register(&Agent{ID: "remote-dup", PublicKey: HexKey(otherKey)})
	require.True(t, errors.Is(err, ErrPublicKeyMismatch), "got %v", err)
	require.False(t, errors.Is(err, ErrAgentExists), "got %v", err)
}

// TestReRegistration_LegacyRelayIsNotMisreported: a relay that predates this
// change answers 409 with its old body (or with no body at all). The remote
// decoder must keep reading that as the plain duplicate — the mismatch class is
// only ever claimed on an OBSERVED code.
func TestReRegistration_LegacyRelayIsNotMisreported(t *testing.T) {
	_, key := newTestPubKey(t)

	for _, tt := range []struct {
		name string
		body string
	}{
		{"legacy message body", `{"error":"agent already registered: \"legacy-agent\""}`},
		{"no body", ``},
		{"unparseable body", `<<not json>>`},
		{"other code", `{"error":"some_other_conflict","message":"x"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				if tt.body != "" {
					_, _ = w.Write([]byte(tt.body))
				}
			}))
			defer srv.Close()

			rs := NewRemoteStore(srv.URL, "probe-client", "")
			err := rs.Register(&Agent{ID: "legacy-agent", PublicKey: HexKey(key)})
			require.True(t, errors.Is(err, ErrAgentExists), "got %v", err)
			require.False(t, errors.Is(err, ErrPublicKeyMismatch),
				"a relay that does not emit the code must not be reported as a key conflict: %v", err)
		})
	}
}

// TestReRegistration_BothSpellingsShareOneImplementation is a structural check:
// the same request must produce the same answer on both routes, which is what
// makes the two spellings one endpoint rather than two behaviours.
func TestReRegistration_BothSpellingsShareOneImplementation(t *testing.T) {
	key, _ := newTestPubKey(t)
	otherKey, _ := newTestPubKey(t)

	store := setupTestStore(t)
	_, router := setupRouterWithHandler(store)

	// Register the same identity under each spelling on its OWN router, so the
	// comparison is not polluted by the first registration.
	for _, tt := range []struct {
		name string
		path string
		body string
		next string
	}{
		{"body spelling", "/agents",
			`{"id":"shared","public_key":"` + key + `"}`,
			`{"id":"shared","public_key":"` + otherKey + `"}`},
		{"path spelling", "/agents/shared",
			`{"public_key":"` + key + `"}`,
			`{"public_key":"` + otherKey + `"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			freshStore := setupTestStore(t)
			_, freshRouter := setupRouterWithHandler(freshStore)

			code, resp := doJSON(t, freshRouter, http.MethodPost, tt.path, tt.body)
			require.Equal(t, http.StatusCreated, code, "control: %s", resp)

			code, resp = doJSON(t, freshRouter, http.MethodPost, tt.path, tt.next)
			assertStructuredKeyConflict(t, "shared", code, resp)
		})
	}

	// On ONE router, the conflict body is identical across the two spellings
	// (the same formatter builds it), so a client cannot get a different
	// answer depending on which URL it happened to use.
	code, bodyAnswer := doJSON(t, router, http.MethodPost, "/agents",
		`{"id":"echo","public_key":"`+key+`"}`)
	require.Equal(t, http.StatusCreated, code, "control: %s", bodyAnswer)
	code, bodyAnswer = doJSON(t, router, http.MethodPost, "/agents",
		`{"id":"echo","public_key":"`+otherKey+`"}`)
	require.Equal(t, http.StatusConflict, code, "body: %s", bodyAnswer)

	code, pathAnswer := doJSON(t, router, http.MethodPost, "/agents/echo",
		`{"public_key":"`+otherKey+`"}`)
	require.Equal(t, http.StatusConflict, code, "body: %s", pathAnswer)

	require.Equal(t, decodeConflict(t, bodyAnswer), decodeConflict(t, pathAnswer),
		"the two registration spellings must answer the same conflict body\nbody spelling: %s\npath spelling: %s",
		bodyAnswer, pathAnswer)
}
