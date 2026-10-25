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
	"fmt"
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

// appendCount is the number of record-versions the store holds. It is the
// direct evidence a refused management write appended NOTHING — the assertion
// is a before/after count on the store the ACL folds, not an inference from
// the response body.
func (m *mgmtTest) appendCount(t *testing.T) int {
	t.Helper()
	recs, err := m.perms.Records(context.Background())
	require.NoError(t, err)
	return len(recs)
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
	t.Parallel()
	store := NewMemoryStore()
	h := NewHandler(store) // no store, no token
	r := mux.NewRouter()
	r.HandleFunc("/principals", h.HandleMintPrincipal).Methods("POST")

	rec := doMgmtRequest(t, r, http.MethodPost, "/principals", "whatever", `{}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "MANAGEMENT_FORBIDDEN")
}

func TestManagementRefusedWithoutAdminToken(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	m.handler.SetPermissionsAdminToken("") // store wired, gate closed
	rec, body := m.post(t, "/principals", "", `{}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "MANAGEMENT_FORBIDDEN", body["error"])
}

func TestManagementRefusedWithWrongToken(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	rec, body := m.post(t, "/principals", "not-the-token", `{}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "MANAGEMENT_FORBIDDEN", body["error"])
}

// ---------------------------------------------------------------------------
// Mint → bind → grant → deliver → the principal rides the stored record.
// ---------------------------------------------------------------------------

func TestMintBindGrantDeliverRecordsPrincipal(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	capabilityHolder(t, m.store, "atlas")

	// DF-CRIER-300: the minted/bound JSON body must carry a REAL created_at,
	// and it must be the same instant the persisted record carries.
	createdAtOf := func(t *testing.T, body map[string]any, what string) time.Time {
		t.Helper()
		raw, _ := body["created_at"].(string)
		require.NotEmpty(t, raw, "%s response carries a created_at (DF-CRIER-300)", what)
		ts, err := time.Parse(time.RFC3339Nano, raw)
		require.NoError(t, err, "%s created_at parses as RFC3339Nano", what)
		require.False(t, ts.IsZero(), "%s created_at is not the zero time", what)
		return ts
	}
	beforeMint := time.Now().UTC()

	// Mint.
	rec, principal := m.post(t, "/principals", m.token, `{"display_name":"Bane","namespace":"acme","role":"owner"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	prinID, _ := principal["id"].(string)
	require.True(t, len(prinID) > len("prin_"), "minted id carries the principal prefix")
	require.Equal(t, "principal", principal["kind"])
	require.Equal(t, "active", principal["status"])
	principalCreatedAt := createdAtOf(t, principal, "principal")
	require.False(t, principalCreatedAt.Before(beforeMint), "created_at is at/after the request")
	require.False(t, principalCreatedAt.After(time.Now().UTC().Add(5*time.Second)), "created_at is near the request time")

	// Bind.
	beforeBind := time.Now().UTC()
	rec, binding := m.post(t, "/bindings", m.token,
		`{"principal":"`+prinID+`","agent":"atlas"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, true, binding["as_agent"], "as_agent defaults to true (§2.3: the only value this version defines)")
	bindingCreatedAt := createdAtOf(t, binding, "binding")
	require.False(t, bindingCreatedAt.Before(beforeBind), "created_at is at/after the request")
	require.False(t, bindingCreatedAt.After(time.Now().UTC().Add(5*time.Second)), "created_at is near the request time")

	// The response created_at IS the persisted record's instant (DF-CRIER-300).
	snap, err := m.perms.Snapshot(context.Background())
	require.NoError(t, err)
	require.NotNil(t, snap.Principal(prinID))
	require.True(t, principalCreatedAt.Equal(snap.Principal(prinID).CreatedAt),
		"response created_at %v matches the stored principal's %v", principalCreatedAt, snap.Principal(prinID).CreatedAt)
	require.NotNil(t, snap.Binding(prinID, "atlas"))
	require.True(t, bindingCreatedAt.Equal(snap.Binding(prinID, "atlas").CreatedAt),
		"response created_at %v matches the stored binding's %v", bindingCreatedAt, snap.Binding(prinID, "atlas").CreatedAt)

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
	t.Parallel()
	m := newMgmtTest(t)
	rec, body := m.post(t, "/grants", m.token,
		`{"principal":"prin_x","subject":{"type":"agent","ref":"atlas"},"actions":["execute"]}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, body["error"], "action", "the 400 names the accepted action set (§6.1)")
}

func TestRevokeGrantThroughManagementSurface(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// ---------------------------------------------------------------------------
// DF-CRIER-301 — input validation is UNEVEN across the management surface.
//
// The premise, measured live before the fix: POST /principals with an unknown
// role answered 500 {"error":"store failure"} (the store's own record check
// refused the record and the handler reported the caller's input error as an
// internal failure), and POST /bindings accepted a principal that never
// existed — and an AGENT id in the principal field — appending a record the
// ACL could never resolve.
//
// The contract this fixes, stated once: an invalid field is the CALLER's
// error, so it is a 400 written BEFORE the append, it names the accepted set
// (the shape the sibling POST /grants and POST /agents/{agentid}/class
// already follow), and it leaves the store untouched. Valid flows are
// unchanged.
// ---------------------------------------------------------------------------

// TestMintPrincipalUnknownRoleRefused400 is the mint half of the premise: the
// role is validated by the handler, so an unknown one is a 400 naming the four
// roles — never a 500 produced by the store's record check.
func TestMintPrincipalUnknownRoleRefused400(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	before := m.appendCount(t)

	// Premise (holds before AND after the fix): the RECORD layer already
	// refuses an unknown role — which is exactly why the unvalidated handler
	// answered 500: the store's MarshalLine failed and the handler called that
	// "store failure". Asserted so this test keeps meaning if the record check
	// ever loosens.
	require.Error(t,
		(&permissions.Principal{ID: "prin_probe", Role: "superadmin", Status: permissions.PrincipalActive}).
			Record(time.Now()).Validate(),
		"premise: an unknown role is refused by the record layer")

	rec, body := m.post(t, "/principals", m.token, `{"display_name":"Bane","role":"superadmin"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an unknown role is the caller's input error, not a store failure (body: %s)", rec.Body.String())
	require.Equal(t, "INVALID_ROLE", body["error"])
	detail, _ := body["detail"].(string)
	require.NotEmpty(t, detail)
	for _, role := range permissions.Roles {
		require.Contains(t, detail, string(role), "the refusal names the accepted role set (§5.1)")
	}
	require.Equal(t, before, m.appendCount(t), "a refused mint appends no record")
}

// TestMintPrincipalUnknownStatusRefused400 is the same field class on the same
// handler: status carried the identical 500.
func TestMintPrincipalUnknownStatusRefused400(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	before := m.appendCount(t)

	rec, body := m.post(t, "/principals", m.token, `{"display_name":"Bane","status":"pending"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an unknown lifecycle state is the caller's input error (body: %s)", rec.Body.String())
	require.Equal(t, "INVALID_STATUS", body["error"])
	detail, _ := body["detail"].(string)
	for _, status := range permissions.Statuses {
		require.Contains(t, detail, string(status), "the refusal names the accepted lifecycle set (§2.2)")
	}
	require.Equal(t, before, m.appendCount(t), "a refused mint appends no record")
}

// TestMintPrincipalAcceptsEveryDocumentedRole keeps the valid flows green: all
// four roles still mint, the record carries the role, and the principal is in
// the store the ACL reads.
func TestMintPrincipalAcceptsEveryDocumentedRole(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	snapBefore, err := m.perms.Snapshot(context.Background())
	require.NoError(t, err)

	for _, role := range permissions.Roles {
		rec, body := m.post(t, "/principals", m.token,
			fmt.Sprintf(`{"display_name":%q,"namespace":"acme","role":%q}`, string(role), string(role)))
		require.Equal(t, http.StatusCreated, rec.Code, "role %q must still mint (body: %s)", role, rec.Body.String())
		require.Equal(t, string(role), body["role"])
		require.Equal(t, "active", body["status"], "absent status still defaults to active")

		id, _ := body["id"].(string)
		require.True(t, permissions.ValidPrincipalID(id), "minted id %q carries the principal grammar (§2.1)", id)
		snap, err := m.perms.Snapshot(context.Background())
		require.NoError(t, err)
		require.NotNil(t, snap.Principal(id), "the minted principal is in the store the ACL reads")
		require.Nil(t, snapBefore.Principal(id))
	}

	// A documented lifecycle state other than the default is still accepted.
	rec, body := m.post(t, "/principals", m.token, `{"display_name":"Invitee","status":"invited"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, "invited", body["status"])
}

// TestCreateBindingRejectsAgentIDInPrincipalField: §2.3/§6.1 — the principal
// field is a principal id, and an agent id is not one.
func TestCreateBindingRejectsAgentIDInPrincipalField(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	capabilityHolder(t, m.store, "atlas")
	before := m.appendCount(t)

	rec, body := m.post(t, "/bindings", m.token, `{"principal":"atlas","agent":"atlas"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an agent id in the principal field is refused (body: %s)", rec.Body.String())
	require.Equal(t, "INVALID_PRINCIPAL", body["error"])
	require.Contains(t, body["detail"], "atlas", "the refusal names the offending value")
	require.Contains(t, body["detail"], permissions.PrincipalIDPrefix,
		"the refusal names the principal-id grammar (§2.1)")
	require.Equal(t, before, m.appendCount(t), "a refused binding appends no record")
}

// TestCreateBindingRejectsUnknownPrincipal: a well-formed `prin_` id nobody
// minted is a binding the ACL could never resolve, so it is refused before the
// append rather than stored as dead weight.
func TestCreateBindingRejectsUnknownPrincipal(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	before := m.appendCount(t)
	unknown := permissions.NewPrincipalID() // well-formed, never minted here

	rec, body := m.post(t, "/bindings", m.token, `{"principal":"`+unknown+`","agent":"atlas"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an unknown principal is refused (body: %s)", rec.Body.String())
	require.Equal(t, "UNKNOWN_PRINCIPAL", body["error"])
	require.Contains(t, body["detail"], unknown, "the refusal names the unknown id")
	require.Equal(t, before, m.appendCount(t), "a refused binding appends no record")

	// Premise: the id really was well-formed — the refusal is about existence,
	// not grammar (so a grammar fix alone would still store this binding).
	require.True(t, permissions.ValidPrincipalID(unknown))
	snap, err := m.perms.Snapshot(context.Background())
	require.NoError(t, err)
	require.Nil(t, snap.Principal(unknown))
}

// TestCreateBindingRejectsPrincipalIDInAgentField is the reverse direction of
// the same invariant: the two id vocabularies are not interchangeable in
// either field.
func TestCreateBindingRejectsPrincipalIDInAgentField(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)
	before := m.appendCount(t)

	rec, principal := m.post(t, "/principals", m.token, `{"display_name":"Bane"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	prinID, _ := principal["id"].(string)

	rec, body := m.post(t, "/bindings", m.token, `{"principal":"`+prinID+`","agent":"`+prinID+`"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a principal id in the agent field is refused (body: %s)", rec.Body.String())
	require.Equal(t, "INVALID_AGENT", body["error"])
	require.Contains(t, body["detail"], prinID)
	require.Equal(t, before+1, m.appendCount(t), "the mint appended one record; the refused binding appended none")
}

// TestCreateBindingValidFlowsRemainGreen pins what is deliberately NOT
// validated, so the new refusals cannot quietly tighten a valid flow: a
// registered principal may be bound to an agent that has no registry row yet
// (§2.3 requires the binding target, not the agent's existence), and the empty
// field case keeps its shipped 400.
func TestCreateBindingValidFlowsRemainGreen(t *testing.T) {
	t.Parallel()
	m := newMgmtTest(t)

	rec, principal := m.post(t, "/principals", m.token, `{"display_name":"Bane","namespace":"acme","role":"member"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	prinID, _ := principal["id"].(string)

	// No agent row exists for "unregistered-agent" — still a 201.
	rec, binding := m.post(t, "/bindings", m.token, `{"principal":"`+prinID+`","agent":"unregistered-agent"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, prinID, binding["principal"])
	require.Equal(t, "unregistered-agent", binding["agent"])
	require.Equal(t, true, binding["as_agent"])

	snap, err := m.perms.Snapshot(context.Background())
	require.NoError(t, err)
	require.NotNil(t, snap.Binding(prinID, "unregistered-agent"))

	// The pre-existing empty-field refusal is untouched.
	rec, body := m.post(t, "/bindings", m.token, `{"principal":"","agent":"atlas"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "principal and agent are required", body["error"])
}
