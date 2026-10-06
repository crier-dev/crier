package registry

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/federation"
)

// ---------------------------------------------------------------------------
// CR-CHAT-023 — the per-peer INBOUND gate on the deliver path: a request that
// arrives announcing a peer identity (federation.PeerHeader) is checked
// against the policy set before anything acts; a request without one takes
// the shipped path unchanged. Verified by tests in the diff, per AC-c.
// ---------------------------------------------------------------------------

func fedPolicyStore(t *testing.T) Store {
	return setupTestStore(t)
}

func registerFedAgent(t *testing.T, store Store, id string) {
	t.Helper()
	if err := store.Register(&Agent{ID: id}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func fedDeliver(t *testing.T, handler *Handler, id, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	router := mux.NewRouter()
	router.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	return deliverTo(t, router, id, []byte(body), headers)
}

func TestInboundPeerPolicyUnknownPeerRefused(t *testing.T) {
	store := fedPolicyStore(t)
	registerFedAgent(t, store, "atlas")
	handler := NewHandler(store)
	handler.SetPeerPolicies(federation.PeerPolicies{
		"peer_acme": {Peer: "peer_acme", NamespacesAllow: []string{""}},
	})

	rec := fedDeliver(t, handler, "atlas", `{"payload":{"a":1}}`, map[string]string{
		federation.PeerHeader: "peer_ghost",
	})
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	var body fedPeerRefusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if body.Error != "FED_PEER_UNTRUSTED" || body.Peer != "peer_ghost" {
		t.Fatalf("refusal = %+v", body)
	}
	if body.Direction != "inbound" {
		t.Fatalf("direction = %q, want inbound", body.Direction)
	}
}

func TestInboundPeerPolicyNamespaceRefusalWholesaleAndPerNamespace(t *testing.T) {
	store := fedPolicyStore(t)
	registerFedAgent(t, store, "atlas")
	registerFedAgent(t, store, "quill")
	// peer_beta may reach namespace beta-lab ONLY — the wholesale shape is
	// the same record with fewer namespaces, so this record is the
	// per-namespace gate and the empty allow list below is the wholesale one.
	handler := NewHandler(store)
	handler.SetPeerPolicies(federation.PeerPolicies{
		"peer_beta": {Peer: "peer_beta", NamespacesAllow: []string{"beta-lab"}},
		"peer_none": {Peer: "peer_none", NamespacesAllow: nil},
	})

	// A peer whose policy admits nothing is refused wholesale.
	rec := fedDeliver(t, handler, "atlas", `{"payload":{}}`, map[string]string{
		federation.PeerHeader: "peer_none",
	})
	if rec.Code != 403 {
		t.Fatalf("wholesale refusal: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	var body fedPeerRefusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "FED_NAMESPACE_NOT_PERMITTED" {
		t.Fatalf("wholesale refusal code = %q", body.Error)
	}

	// A namespace outside the allow list is refused per namespace.
	rec = fedDeliver(t, handler, "atlas", `{"payload":{},"namespace":"other-realm"}`, map[string]string{
		federation.PeerHeader: "peer_beta",
	})
	if rec.Code != 403 {
		t.Fatalf("per-namespace refusal: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	body = fedPeerRefusalBody{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "FED_NAMESPACE_NOT_PERMITTED" {
		t.Fatalf("per-namespace refusal code = %q", body.Error)
	}
}

func TestInboundPeerPolicyAgentGate(t *testing.T) {
	store := fedPolicyStore(t)
	registerFedAgent(t, store, "atlas")
	registerFedAgent(t, store, "deploy-bot")
	registerFedAgent(t, store, "quill")
	handler := NewHandler(store)
	handler.SetPeerPolicies(federation.PeerPolicies{
		// §6.5's worked example: atlas allowed, deploy-bot denied, quill
		// simply not in the allow list — three agents, three answers.
		"peer_acme": {Peer: "peer_acme", NamespacesAllow: []string{""},
			AgentsAllow: []string{"atlas"}, AgentsDeny: []string{"deploy-bot"}},
	})

	// Allowed agent: admitted.
	if rec := fedDeliver(t, handler, "atlas", `{"payload":{}}`, map[string]string{
		federation.PeerHeader: "peer_acme",
	}); rec.Code != 201 {
		t.Fatalf("allowed agent: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	// Denied agent: 403 FED_AGENT_NOT_PERMITTED (deny wins).
	rec := fedDeliver(t, handler, "deploy-bot", `{"payload":{}}`, map[string]string{
		federation.PeerHeader: "peer_acme",
	})
	if rec.Code != 403 {
		t.Fatalf("denied agent: status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	var body fedPeerRefusalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "FED_AGENT_NOT_PERMITTED" {
		t.Fatalf("denied agent code = %q", body.Error)
	}

	// Not in the allow list: same refusal, different reason.
	rec = fedDeliver(t, handler, "quill", `{"payload":{}}`, map[string]string{
		federation.PeerHeader: "peer_acme",
	})
	if rec.Code != 403 {
		t.Fatalf("unlisted agent: status = %d, want 403", rec.Code)
	}
	body = fedPeerRefusalBody{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error != "FED_AGENT_NOT_PERMITTED" {
		t.Fatalf("unlisted agent code = %q", body.Error)
	}
}

func TestInboundNoPeerHeaderUnchanged(t *testing.T) {
	// A request with no peer announcement takes the shipped path — the
	// degraded default posture (§5.3 item 1), byte-for-byte unchanged even
	// when policies ARE configured.
	store := fedPolicyStore(t)
	registerFedAgent(t, store, "atlas")
	handler := NewHandler(store)
	handler.SetPeerPolicies(federation.PeerPolicies{
		"peer_acme": {Peer: "peer_acme", NamespacesAllow: nil},
	})
	if rec := fedDeliver(t, handler, "atlas", `{"payload":{}}`, nil); rec.Code != 201 {
		t.Fatalf("no-header delivery: status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestInboundPolicyRefusalHasNoSideEffect(t *testing.T) {
	// §6.3: a refusal has no side effect — no inbox entry, nothing stored.
	store := fedPolicyStore(t)
	registerFedAgent(t, store, "atlas")
	handler := NewHandler(store)
	handler.SetPeerPolicies(federation.PeerPolicies{
		"peer_none": {Peer: "peer_none", NamespacesAllow: nil},
	})
	if rec := fedDeliver(t, handler, "atlas", `{"payload":{}}`, map[string]string{
		federation.PeerHeader: "peer_none",
	}); rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	entries, _, err := store.Retrieve("atlas", 0, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("inbox entries = %d, want 0: a policy refusal must store nothing", len(entries))
	}
}
