package registry

// CR-CHAT-003 acceptance — the DELIVERY ACL, through the REAL handler.
//
// The row's deliverable is the delivery ACL of specs/CHAT-PERMISSIONS.md §6:
// "a principal without a delivery grant is refused with a NAMED error when
// addressing an agent", the four roles demonstrated per §5.2, revocation
// semantics (§6.6), and a default-deny that covers the delivery surfaces §6.4
// names.
//
// Every assertion below goes through the real HTTP surface — a gorilla mux
// router wired to the same handlers cmd/server registers — so nothing here
// re-implements a code path. The ACL is armed exactly as cmd/server arms it:
// buildPermissions (§6.4 rule 2's opt-in deployment), and a JSONL store seeded
// with principals, bindings, grants and agent-class records.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/permissions"
)

// aclTest wires the two delivery surfaces the spec's §6.4 names onto the same
// route set the server registers, with a permissions checker over a fresh JSONL
// store.
type aclTest struct {
	handler *Handler
	router  *mux.Router
	store   *MemoryStore
	perms   *permissions.JSONLStore
	checker *permissions.Checker
}

func newACLTest(t *testing.T) *aclTest {
	t.Helper()
	store := NewMemoryStore()
	h := NewHandler(store)

	pstore, err := permissions.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pstore.Close() })
	checker := permissions.NewChecker(pstore)
	h.SetPermissionsChecker(checker)

	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
	r.HandleFunc("/capabilities/{capability}/inbox", h.HandleDeliverByCapability).Methods("POST")

	return &aclTest{handler: h, router: r, store: store, perms: pstore, checker: checker}
}

func (a *aclTest) seed(t *testing.T, recs ...*permissions.Record) {
	t.Helper()
	for _, rec := range recs {
		require.NoError(t, a.perms.Append(context.Background(), rec))
	}
}

func (a *aclTest) deliver(t *testing.T, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := doRequest(t, a.router, http.MethodPost, path, body)
	return rec, decodeJSONBody(t, rec)
}

func (a *aclTest) register(t *testing.T, id string, caps ...string) {
	t.Helper()
	capabilityHolder(t, a.store, id, caps...)
}

const principalGrantAgentBody = `{"payload":{"text":"hi"},"principal_id":"prin_viewer","as_agent":"quill"}`

// ---------------------------------------------------------------------------
// AC1 — a principal without a delivery grant is refused with a NAMED error.
// ---------------------------------------------------------------------------

// TestDeliverRefusedWithoutGrantNamedError is the acceptance criterion, end to
// end: the SPEC §6.5 worked refusal, through POST /agents/{id}/inbox. The
// principal holds a binding and a READ grant — a speech right and a read right
// — and the send is still refused, with a machine-readable body.
func TestDeliverRefusedWithoutGrantNamedError(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "quill")

	a.seed(t,
		(&permissions.Principal{
			ID: "prin_viewer", DisplayName: "Viewer", Namespace: "acme",
			Role: permissions.RoleViewer, Status: permissions.PrincipalActive,
		}).Record(time.Now()),
		(&permissions.Binding{ID: "bind_v", Principal: "prin_viewer", Agent: "quill", AsAgent: true}).Record(time.Now()),
		(&permissions.Grant{
			ID: "grant_v", Principal: "prin_viewer",
			Subject: permissions.Subject{Type: permissions.SubjectAgent, Ref: "quill"},
			Actions: []permissions.Action{permissions.ActionRead}, GrantedBy: "prin_owner", GrantedAt: time.Now(),
		}).Record(time.Now()),
		(&permissions.AgentInfo{ID: "quill", Class: permissions.ClassPersonal, Owner: "prin_other", Namespace: "acme"}).Record(time.Now()),
	)

	rec, body := a.deliver(t, "/agents/quill/inbox", principalGrantAgentBody)

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, permissions.ErrorDeliveryForbidden, body["error"], "the error must be NAMED and machine-readable")
	require.Equal(t, permissions.ReasonNoGrant, body["reason"])
	require.Equal(t, "prin_viewer", body["principal"])
	require.Equal(t, "quill", body["as_agent"])
	require.Equal(t, string(permissions.ActionSend), body["action"])

	// The refusal has NO store side effect: nothing landed in the inbox.
	depth, _, _, err := a.store.Stats("quill")
	require.NoError(t, err)
	require.Zero(t, depth, "a refused delivery must store nothing")
}

// TestDeliverRefusedNoBindingNamedError covers §6.7's NO_BINDING row: a
// principal that names no live binding is refused with the reason a client
// branches on.
func TestDeliverRefusedNoBindingNamedError(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "quill")
	a.seed(t,
		(&permissions.Principal{ID: "prin_v", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(time.Now()),
		(&permissions.AgentInfo{ID: "quill", Class: permissions.ClassPersonal, Owner: "prin_other"}).Record(time.Now()),
	)
	rec, body := a.deliver(t, "/agents/quill/inbox",
		`{"payload":{"text":"hi"},"principal_id":"prin_v","as_agent":"quill"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, permissions.ErrorDeliveryForbidden, body["error"])
	require.Equal(t, permissions.ReasonNoBinding, body["reason"])
}

// TestDeliverAnonymousOnClassedTargetRefused covers §6.7's anonymous row.
func TestDeliverAnonymousOnClassedTargetRefused(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "quill")
	a.seed(t, (&permissions.AgentInfo{ID: "quill", Class: permissions.ClassPersonal, Owner: "prin_other"}).Record(time.Now()))

	rec, body := a.deliver(t, "/agents/quill/inbox", `{"payload":{"text":"hi"}}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, permissions.ErrorDeliveryForbidden, body["error"])
	require.Equal(t, "anonymous", body["principal"])
}

// TestDeliverAllowedWithSendGrant is the positive half: the grant that admits a
// send does so, and the message lands.
func TestDeliverAllowedWithSendGrant(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "quill")
	a.seed(t,
		(&permissions.Principal{ID: "prin_ana", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(time.Now()),
		(&permissions.Binding{ID: "b", Principal: "prin_ana", Agent: "quill", AsAgent: true}).Record(time.Now()),
		(&permissions.Grant{
			ID: "g", Principal: "prin_ana",
			Subject: permissions.Subject{Type: permissions.SubjectAgent, Ref: "quill"},
			Actions: []permissions.Action{permissions.ActionSend}, GrantedBy: "prin_owner", GrantedAt: time.Now(),
		}).Record(time.Now()),
		(&permissions.AgentInfo{ID: "quill", Class: permissions.ClassPersonal, Owner: "prin_other"}).Record(time.Now()),
	)
	rec, _ := a.deliver(t, "/agents/quill/inbox",
		`{"payload":{"text":"hi"},"principal_id":"prin_ana","as_agent":"quill"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	depth, _, _, err := a.store.Stats("quill")
	require.NoError(t, err)
	require.Equal(t, 1, depth)
}

// TestOwnerMayReachOwnAgent is §3.2's ownership rule through the wire.
func TestOwnerMayReachOwnAgent(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "atlas")
	a.seed(t,
		(&permissions.Principal{ID: "prin_bane", Role: permissions.RoleOwner, Status: permissions.PrincipalActive, Namespace: "acme"}).Record(time.Now()),
		(&permissions.Binding{ID: "b", Principal: "prin_bane", Agent: "atlas", AsAgent: true}).Record(time.Now()),
		(&permissions.AgentInfo{ID: "atlas", Class: permissions.ClassPersonal, Owner: "prin_bane", Namespace: "acme"}).Record(time.Now()),
	)
	rec, _ := a.deliver(t, "/agents/atlas/inbox",
		`{"payload":{"text":"hi"},"principal_id":"prin_bane","as_agent":"atlas"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// §6.6 — revocation takes effect on the NEXT delivery.
// ---------------------------------------------------------------------------

func TestRevokedGrantRefusedThroughHandler(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "quill")
	a.seed(t,
		(&permissions.Principal{ID: "prin_ana", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(time.Now()),
		(&permissions.Binding{ID: "b", Principal: "prin_ana", Agent: "quill", AsAgent: true}).Record(time.Now()),
		(&permissions.Grant{
			ID: "g", Principal: "prin_ana", Subject: permissions.Subject{Type: permissions.SubjectAgent, Ref: "quill"},
			Actions: []permissions.Action{permissions.ActionSend}, GrantedBy: "prin_owner", GrantedAt: time.Now(),
		}).Record(time.Now()),
		(&permissions.AgentInfo{ID: "quill", Class: permissions.ClassPersonal, Owner: "prin_other"}).Record(time.Now()),
	)
	body := `{"payload":{"text":"hi"},"principal_id":"prin_ana","as_agent":"quill"}`

	rec, _ := a.deliver(t, "/agents/quill/inbox", body)
	require.Equal(t, http.StatusCreated, rec.Code)

	require.NoError(t, permissions.RevokeGrant(context.Background(), a.perms, "g", "prin_owner", time.Now()))

	rec, refusal := a.deliver(t, "/agents/quill/inbox", body)
	require.Equal(t, http.StatusForbidden, rec.Code, "a revoked grant must stop working on the next delivery (§6.6)")
	require.Equal(t, permissions.ErrorDeliveryForbidden, refusal["error"])
	require.Equal(t, permissions.ReasonNoGrant, refusal["reason"])
}

// ---------------------------------------------------------------------------
// §6.4 rule 2 / §8.1 — the deployment-level fallback.
// ---------------------------------------------------------------------------

// TestUnclassedTargetKeepsLegacyPosture proves the ACL does not break an
// existing fleet: with the ACL ARMED, an agent with no class record is still
// reachable exactly as it shipped (§8.1's residual, stated rather than hidden).
func TestUnclassedTargetKeepsLegacyPosture(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "legacy")
	rec, _ := a.deliver(t, "/agents/legacy/inbox", `{"payload":{"text":"hi"}}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// TestNoCheckerIsByteIdenticalLegacy proves the switch itself: with NO checker
// wired, the SAME classed-agent request that the ACL refuses above is accepted,
// which is the pre-CR-CHAT-003 posture (§6.4 rule 2).
func TestNoCheckerIsByteIdenticalLegacy(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(store) // no SetPermissionsChecker
	capabilityHolder(t, store, "quill")
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")

	rec := doRequest(t, r, http.MethodPost, "/agents/quill/inbox", principalGrantAgentBody)
	require.Equal(t, http.StatusCreated, rec.Code,
		"with no ACL deployment the delivery is authorized exactly as before CR-CHAT-003")
}

// ---------------------------------------------------------------------------
// §6.4 rule 1 — the pool address is checked BEFORE holder selection, so a
// refused @cap: delivery consumes no rotation turn.
// ---------------------------------------------------------------------------

func TestCapabilityRefusalConsumesNoRotationTurn(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "worker-a", capabilityProbe)
	a.register(t, "worker-b", capabilityProbe)
	// A viewer holds no `invoke` (§5.2) and there is no grant on the pool. The
	// body's as_agent is the agent the human speaks AS (a binding is required
	// for any principal write, §6.3); the ADDRESS under check is the pool.
	a.seed(t,
		(&permissions.Principal{ID: "prin_viewer", Role: permissions.RoleViewer, Status: permissions.PrincipalActive}).Record(time.Now()),
		(&permissions.Binding{ID: "b", Principal: "prin_viewer", Agent: "worker-a", AsAgent: true}).Record(time.Now()),
	)

	rec, body := a.deliver(t, "/capabilities/"+capabilityProbe+"/inbox",
		`{"payload":{"text":"hi"},"principal_id":"prin_viewer","as_agent":"worker-a"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, permissions.ErrorDeliveryForbidden, body["error"])
	require.Equal(t, permissions.ReasonNoGrant, body["reason"], "the refusal is the invoke denial, not a missing binding")
	require.Equal(t, string(permissions.ActionInvoke), body["action"])
	require.Empty(t, a.handler.capCursors, "a refused pool delivery must consume no rotation turn (§6.4 rule 1)")

	// A member+ holds `invoke` in the role bundle (§5.2) and IS allowed; that
	// delivery does advance the cursor.
	a.seed(t,
		(&permissions.Principal{ID: "prin_member", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(time.Now()),
		(&permissions.Binding{ID: "b2", Principal: "prin_member", Agent: "worker-b", AsAgent: true}).Record(time.Now()),
	)
	rec, _ = a.deliver(t, "/capabilities/"+capabilityProbe+"/inbox",
		`{"payload":{"text":"hi"},"principal_id":"prin_member","as_agent":"worker-b"}`)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.NotEmpty(t, a.handler.capCursors, "an allowed pool delivery advances the cursor")
}

// TestCapabilityViewerRefusedInvoke is §6.5's third worked line, through the
// capability surface.
func TestCapabilityViewerRefusedInvoke(t *testing.T) {
	a := newACLTest(t)
	a.register(t, "worker-a", capabilityProbe)
	a.seed(t,
		(&permissions.Principal{ID: "prin_viewer", Role: permissions.RoleViewer, Status: permissions.PrincipalActive}).Record(time.Now()),
		(&permissions.Binding{ID: "b", Principal: "prin_viewer", Agent: "worker-a", AsAgent: true}).Record(time.Now()),
	)
	rec, body := a.deliver(t, "/capabilities/"+capabilityProbe+"/inbox",
		`{"payload":{"text":"hi"},"principal_id":"prin_viewer","as_agent":"worker-a"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, permissions.ReasonNoGrant, body["reason"])
}
