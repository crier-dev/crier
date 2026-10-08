package registry

// CR-CHAT-007 phase 1 — HUMAN identity: the management surface
// (internal/registry/principals_api.go) and the stored-record provenance
// (principal_id riding the inbox entry, the webhook envelope and the dead
// letter), per specs/CHAT-PERMISSIONS.md §2.1/§2.3/§6.1/§6.3/§6.7.
//
// The delivery-ACL refusals themselves are proven in permissions_test.go
// (CR-CHAT-003); these tests cover what phase 1 ADDS: the admin-gated write
// surface, the suspended-principal rule (§2.2), and the audit answer to
// "which human spoke as which agent" surviving into the stored record.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/permissions"
)

// mgmtTest wires the management routes + the delivery route onto one router,
// with a permissions checker and store over a fresh JSONL log and an armed
// admin token — the shape cmd/server arms (main.go's permsChecker branch).
type mgmtTest struct {
	handler *Handler
	router  *mux.Router
	store   *MemoryStore
	perms   *permissions.JSONLStore
	token   string
}

func newMgmtTest(t *testing.T) *mgmtTest {
	t.Helper()
	store := NewMemoryStore()
	h := NewHandler(store)

	pstore, err := permissions.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pstore.Close() })
	h.SetPermissionsChecker(permissions.NewChecker(pstore))
	h.SetPermissionsStore(pstore)
	const token = "admin-secret"
	h.SetPermissionsAdminToken(token)

	r := mux.NewRouter()
	r.HandleFunc("/principals", h.HandleMintPrincipal).Methods("POST")
	r.HandleFunc("/bindings", h.HandleCreateBinding).Methods("POST")
	r.HandleFunc("/grants", h.HandleCreateGrant).Methods("POST")
	r.HandleFunc("/grants/{grantid}/revoke", h.HandleRevokeGrant).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")

	return &mgmtTest{handler: h, router: r, store: store, perms: pstore, token: token}
}

func (m *mgmtTest) post(t *testing.T, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := doMgmtRequest(t, m.router, http.MethodPost, path, token, body)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// doMgmtRequest is doRequest plus the Authorization header the management
// gate reads.
func doMgmtRequest(t *testing.T, router http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// seed appends permission records exactly as permissions_test.go's aclTest
// does, so both suites drive the same JSONL store path.
func (m *mgmtTest) seed(t *testing.T, recs ...*permissions.Record) {
	t.Helper()
	for _, rec := range recs {
		require.NoError(t, m.perms.Append(context.Background(), rec))
	}
}

// seedClassed classes an agent record: personal, owned by the named
// principal — the shape that makes the ACL actually evaluate (§8.1).
func (m *mgmtTest) seedClassed(t *testing.T, agentID, owner string) {
	t.Helper()
	m.seed(t, (&permissions.AgentInfo{ID: agentID, Class: permissions.ClassPersonal, Owner: owner}).Record(time.Now()))
}

// ---------------------------------------------------------------------------
// The admin gate — no store, no token, or the wrong token is refused.
// ---------------------------------------------------------------------------

func TestManagementRefusedWithoutArming(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(store) // no store, no token
	r := mux.NewRouter()
	r.HandleFunc("/principals", h.HandleMintPrincipal).Methods("POST")

	rec := doMgmtRequest(t, r, http.MethodPost, "/principals", "whatever", `{}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "MANAGEMENT_FORBIDDEN")
}

func TestManagementRefusedWithoutAdminToken(t *testing.T) {
	m := newMgmtTest(t)
	m.handler.SetPermissionsAdminToken("") // store wired, gate closed
	rec, body := m.post(t, "/principals", "", `{}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "MANAGEMENT_FORBIDDEN", body["error"])
}

func TestManagementRefusedWithWrongToken(t *testing.T) {
	m := newMgmtTest(t)
	rec, body := m.post(t, "/principals", "not-the-token", `{}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "MANAGEMENT_FORBIDDEN", body["error"])
}

// ---------------------------------------------------------------------------
// Mint → bind → grant → deliver → the principal rides the stored record.
// ---------------------------------------------------------------------------

func TestMintBindGrantDeliverRecordsPrincipal(t *testing.T) {
	m := newMgmtTest(t)
	capabilityHolder(t, m.store, "atlas")

	// Mint.
	rec, principal := m.post(t, "/principals", m.token, `{"display_name":"Bane","namespace":"acme","role":"owner"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	prinID, _ := principal["id"].(string)
	require.True(t, len(prinID) > len("prin_"), "minted id carries the principal prefix")
	require.Equal(t, "principal", principal["kind"])
	require.Equal(t, "active", principal["status"])

	// Bind.
	rec, binding := m.post(t, "/bindings", m.token,
		`{"principal":"`+prinID+`","agent":"atlas"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, true, binding["as_agent"], "as_agent defaults to true (§2.3: the only value this version defines)")

	// The delivery is still REFUSED — a binding is a speech right, not a send
	// (§6.5): the target is classed personal with another owner and no grant.
	m.seedClassed(t, "atlas", "prin_other")
	rec, refusal := m.post(t, "/agents/atlas/inbox", m.token,
		`{"payload":{"text":"hi"},"principal_id":"`+prinID+`","as_agent":"atlas"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, permissions.ErrorDeliveryForbidden, refusal["error"])
	require.Equal(t, permissions.ReasonNoGrant, refusal["reason"])

	// Grant the send.
	rec, grant := m.post(t, "/grants", m.token,
		`{"principal":"`+prinID+`","subject":{"type":"agent","ref":"atlas"},"actions":["send"],"note":"phase-1 test"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	grantID, _ := grant["id"].(string)
	require.True(t, len(grantID) > len("grant_"))

	// Now the delivery lands, and the stored entry records WHICH HUMAN.
	rec, _ = m.post(t, "/agents/atlas/inbox", m.token,
		`{"payload":{"text":"hi"},"principal_id":"`+prinID+`","as_agent":"atlas"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	entries, err := m.store.PeekInbox("atlas")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, prinID, entries[0].PrincipalID,
		"the stored message records the human behind the send (T3 audit)")
}

func TestGrantUnknownActionRefused400(t *testing.T) {
	m := newMgmtTest(t)
	rec, body := m.post(t, "/grants", m.token,
		`{"principal":"prin_x","subject":{"type":"agent","ref":"atlas"},"actions":["execute"]}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, body["error"], "action", "the 400 names the accepted action set (§6.1)")
}

func TestRevokeGrantThroughManagementSurface(t *testing.T) {
	m := newMgmtTest(t)
	capabilityHolder(t, m.store, "atlas")
	m.seedClassed(t, "atlas", "prin_other")
	m.seed(t,
		(&permissions.Principal{ID: "prin_a", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(time.Now()),
		(&permissions.Binding{ID: "b", Principal: "prin_a", Agent: "atlas", AsAgent: true}).Record(time.Now()),
		(&permissions.Grant{
			ID: "grant_live", Principal: "prin_a",
			Subject: permissions.Subject{Type: permissions.SubjectAgent, Ref: "atlas"},
			Actions: []permissions.Action{permissions.ActionSend}, GrantedBy: "admin", GrantedAt: time.Now(),
		}).Record(time.Now()),
	)
	body := `{"payload":{"text":"hi"},"principal_id":"prin_a","as_agent":"atlas"}`
	rec, _ := m.post(t, "/agents/atlas/inbox", m.token, body)
	require.Equal(t, http.StatusCreated, rec.Code)

	rec, out := m.post(t, "/grants/grant_live/revoke", m.token, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "grant_live", out["revoked"])

	rec, refusal := m.post(t, "/agents/atlas/inbox", m.token, body)
	require.Equal(t, http.StatusForbidden, rec.Code, "revocation takes effect on the NEXT delivery (§6.6)")
	require.Equal(t, permissions.ReasonNoGrant, refusal["reason"])

	// An unknown grant id is a 404, not a silent ok.
	rec, _ = m.post(t, "/grants/grant_nope/revoke", m.token, `{}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// §2.2 — a suspended principal's grants are inert.
// ---------------------------------------------------------------------------

func TestSuspendedPrincipalGrantsInert(t *testing.T) {
	m := newMgmtTest(t)
	capabilityHolder(t, m.store, "quill")
	m.seedClassed(t, "quill", "prin_other")
	m.seed(t,
		// Active on delivery 1, suspended before delivery 2: a later record
		// version at the same id wins (keep-LAST, §2.2 lifecycle).
		(&permissions.Principal{ID: "prin_s", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(time.Now()),
		(&permissions.Binding{ID: "b", Principal: "prin_s", Agent: "quill", AsAgent: true}).Record(time.Now()),
		(&permissions.Grant{
			ID: "g", Principal: "prin_s",
			Subject: permissions.Subject{Type: permissions.SubjectAgent, Ref: "quill"},
			Actions: []permissions.Action{permissions.ActionSend}, GrantedBy: "admin", GrantedAt: time.Now(),
		}).Record(time.Now()),
	)
	body := `{"payload":{"text":"hi"},"principal_id":"prin_s","as_agent":"quill"}`
	rec, _ := m.post(t, "/agents/quill/inbox", m.token, body)
	require.Equal(t, http.StatusCreated, rec.Code)

	m.seed(t, (&permissions.Principal{ID: "prin_s", Status: permissions.PrincipalSuspended}).Record(time.Now().Add(time.Second)))

	rec, refusal := m.post(t, "/agents/quill/inbox", m.token, body)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"a suspended principal's grants are inert, not deleted (§2.2)")
	require.Equal(t, permissions.ErrorDeliveryForbidden, refusal["error"])
}

// ---------------------------------------------------------------------------
// Byte-compat: a delivery that names no principal stores no principal key.
// ---------------------------------------------------------------------------

func TestAnonymousDeliveryStoresNoPrincipalField(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(store) // no ACL at all: the shipped posture
	capabilityHolder(t, store, "legacy")
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
	rec := doRequest(t, r, http.MethodPost, "/agents/legacy/inbox", `{"payload":{"text":"hi"}}`)
	require.Equal(t, http.StatusCreated, rec.Code)

	entries, err := store.PeekInbox("legacy")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	wire, err := json.Marshal(entries[0])
	require.NoError(t, err)
	require.NotContains(t, string(wire), "principal_id",
		"a principal-less delivery stays byte-identical to pre-CR-CHAT-007 (omitempty)")
}
