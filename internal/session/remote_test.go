package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/registry"
)

// ---------------------------------------------------------------------------
// CR-CHAT-023 — federated rooms end to end, over TWO in-process server
// instances (the acceptance criteria's own shape):
//
//	a. instance A and instance B are federated; a member of B participates
//	   in a session on A (a remote participant joins through the peer
//	   policy);
//	b. a message crosses and lands in B's member's inbox through the
//	   SHIPPED federation path (federation.Client.ForwardDeliver — the same
//	   POST /agents/{id}/inbox the link path uses; no new transport);
//	c. A's per-peer policy refuses a peer wholesale and per-namespace;
//	d. A's participant view names the remote participant as REMOTE.
// ---------------------------------------------------------------------------

// fedPeerInstance is one crier instance in the test: its own registry, its
// own session store and handler, its own mux router over the real handlers.
type fedPeerInstance struct {
	t       *testing.T
	srv     *httptest.Server
	reg     *registry.MemoryStore
	handler *Handler
}

func newInstance(t *testing.T) *fedPeerInstance {
	t.Helper()
	store, err := NewJSONLStore(t.TempDir() + "/log")
	require.NoError(t, err)
	reg := registry.NewMemoryStore()
	h := NewHTTPHandler(store, HTTPOptions{Deliverer: reg, Agents: reg})
	r := mux.NewRouter()
	registerSessionRoutes(r, h)
	r.HandleFunc("/agents/{id}/inbox", func(w http.ResponseWriter, req *http.Request) {
		h := registry.NewHandler(reg)
		h.HandleDeliver(w, req)
	}).Methods(http.MethodPost)
	inst := &fedPeerInstance{t: t, srv: httptest.NewServer(r), reg: reg, handler: h}
	t.Cleanup(inst.srv.Close)
	return inst
}

func (in *fedPeerInstance) do(method, path, body string, headers map[string]string) (int, string) {
	in.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, in.srv.URL+path, reader)
	require.NoError(in.t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := in.srv.Client().Do(req)
	require.NoError(in.t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(in.t, err)
	return resp.StatusCode, string(raw)
}

// armPeerPolicy configures instance A's side of the boundary: the per-peer
// policy (which namespaces may cross, what A announces as) and the shipped
// federation client that carries the forward.
func (in *fedPeerInstance) armPeerPolicy(pp federation.PeerPolicies) {
	in.t.Helper()
	client := federation.NewClient(nil, 0, "")
	client.SetPeerPolicies(pp)
	in.handler.SetPeerPolicies(pp)
	in.handler.SetFedClient(client)
}

func createFederatedSession(t *testing.T, in *fedPeerInstance) string {
	t.Helper()
	status, body := in.do("POST", "/sessions", `{"id":"shared-room","created_by":{"agent":"atlas-a"}}`, nil)
	require.Equal(t, http.StatusCreated, status, "create session: %s", body)
	return "shared-room"
}

func TestFederatedRoomRemoteParticipantCrosses(t *testing.T) {
	a := newInstance(t)
	b := newInstance(t)

	// B's member, registered on B only — A has no local row for it.
	require.NoError(t, b.reg.Register(&registry.Agent{ID: "atlas-b"}))

	// A admits peer_b for the default realm and announces itself as peer_a.
	a.armPeerPolicy(federation.PeerPolicies{
		"peer_b": {Peer: "peer_b", URL: b.srv.URL, SelfAs: "peer_a", NamespacesAllow: []string{""}},
	})

	// (a) A member of B joins a session on A as a REMOTE participant.
	sessID := createFederatedSession(t, a)
	status, body := a.do("POST", "/sessions/"+sessID+"/participants",
		`{"member_type":"agent","member_id":"remote:peer_b:atlas-b","role":"member"}`, nil)
	require.Equal(t, http.StatusCreated, status, "add remote participant: %s", body)

	// (d) The participant view names the remote participant as REMOTE.
	status, body = a.do("GET", "/sessions/"+sessID+"/participants", "", nil)
	require.Equal(t, http.StatusOK, status)
	var participants struct {
		Participants []participantView `json:"participants"`
		Count        int               `json:"count"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &participants))
	require.Len(t, participants.Participants, 1)
	remote := participants.Participants[0]
	require.Equal(t, "remote:peer_b:atlas-b", remote.MemberID)
	require.True(t, remote.Remote, "the remote participant must be marked remote")
	require.True(t, remote.Active)

	// (b) A message crosses and lands in B's member's inbox through the
	// SHIPPED federation path.
	status, body = a.do("POST", "/sessions/"+sessID+"/messages",
		`{"payload":{"text":"hello across the boundary"},"sender":"atlas-a"}`, nil)
	require.Equal(t, http.StatusCreated, status, "post message: %s", body)
	var sent struct {
		Outcomes []DeliveryOutcome `json:"outcomes"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &sent))
	require.Len(t, sent.Outcomes, 1)
	require.Equal(t, "remote:peer_b:atlas-b", sent.Outcomes[0].Target)
	require.Equal(t, OutcomeDelivered, sent.Outcomes[0].Outcome, "outcome: %+v", sent.Outcomes[0])

	// The message is IN B's member's durable inbox on B.
	entries, _, err := b.reg.Retrieve("atlas-b", 0, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1, "B's member must have received the message in its inbox")
	require.Contains(t, string(entries[0].Payload), "hello across the boundary")
	require.Equal(t, "atlas-a", entries[0].Sender, "the crossing records its author")

	// The transcript names the remote participant in its remote-qualified
	// identity (§8.3) — never as a local one.
	status, body = a.do("GET", "/sessions/"+sessID+"/messages", "", nil)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "remote:peer_b:atlas-b")
}

func TestFederatedRoomPerPeerRefusal(t *testing.T) {
	a := newInstance(t)
	b := newInstance(t)
	require.NoError(t, b.reg.Register(&registry.Agent{ID: "atlas-b"}))
	sessID := createFederatedSession(t, a)

	// (c.i) WHOLESALE: the peer's policy admits NO namespace, so its member
	// cannot even join, and (via the outbound wall) nothing can cross.
	a.armPeerPolicy(federation.PeerPolicies{
		"peer_b": {Peer: "peer_b", URL: b.srv.URL, SelfAs: "peer_a", NamespacesAllow: nil},
	})
	status, body := a.do("POST", "/sessions/"+sessID+"/participants",
		`{"member_type":"agent","member_id":"remote:peer_b:atlas-b"}`, nil)
	require.Equal(t, http.StatusForbidden, status, "wholesale refusal: %s", body)
	require.Contains(t, body, "FED_NAMESPACE_NOT_PERMITTED")

	// (c.ii) PER-NAMESPACE: two peers, one admitted to the session's realm,
	// one not — the difference is a policy record, not a different secret.
	a2 := newInstance(t)
	a2s := createFederatedSession(t, a2)
	a2.armPeerPolicy(federation.PeerPolicies{
		"peer_b":     {Peer: "peer_b", URL: b.srv.URL, NamespacesAllow: []string{""}},
		"peer_ghost": {Peer: "peer_ghost", URL: b.srv.URL, NamespacesAllow: []string{"other-realm"}},
	})
	// The admitted peer joins and receives.
	status, body = a2.do("POST", "/sessions/"+a2s+"/participants",
		`{"member_type":"agent","member_id":"remote:peer_b:atlas-b"}`, nil)
	require.Equal(t, http.StatusCreated, status, "admitted peer joins: %s", body)
	status, _ = a2.do("POST", "/sessions/"+a2s+"/messages",
		`{"payload":{"text":"for the admitted peer"},"sender":"atlas-a"}`, nil)
	require.Equal(t, http.StatusCreated, status)
	entries, _, err := b.reg.Retrieve("atlas-b", 0, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// A remote member of a peer whose policy does not admit this session's
	// namespace is refused AT THE WALL — per-namespace, named.
	status, body = a2.do("POST", "/sessions/"+a2s+"/participants",
		`{"member_type":"agent","member_id":"remote:peer_ghost:atlas-b"}`, nil)
	require.Equal(t, http.StatusForbidden, status, "per-namespace refusal: %s", body)
	require.Contains(t, body, "FED_NAMESPACE_NOT_PERMITTED")

	// An UNKNOWN peer is refused with FED_PEER_UNTRUSTED — never silently
	// admitted, never a 404.
	status, body = a2.do("POST", "/sessions/"+a2s+"/participants",
		`{"member_type":"agent","member_id":"remote:peer_unknown:atlas-b"}`, nil)
	require.Equal(t, http.StatusForbidden, status)
	require.Contains(t, body, "FED_PEER_UNTRUSTED")
}

func TestFederatedRoomNoPolicyNoRemoteJoin(t *testing.T) {
	// With NO peer policies configured (the shipped posture), a
	// remote-shaped member id is refused: nothing is admitted by default
	// and every existing behavior is unchanged.
	a := newInstance(t)
	sessID := createFederatedSession(t, a)
	status, body := a.do("POST", "/sessions/"+sessID+"/participants",
		`{"member_type":"agent","member_id":"remote:peer_b:atlas-b"}`, nil)
	require.Equal(t, http.StatusForbidden, status, "no-policy posture: %s", body)
	require.Contains(t, body, "FED_PEER_UNTRUSTED")
}

func TestRemoteMemberIDRoundTrip(t *testing.T) {
	id := RemoteMemberID("peer_acme", "atlas")
	require.Equal(t, "remote:peer_acme:atlas", id)
	peer, subject, ok := ParseRemoteMemberID(id)
	require.True(t, ok)
	require.Equal(t, "peer_acme", peer)
	require.Equal(t, "atlas", subject)
	require.False(t, IsRemoteMember("atlas"), "a local id is not remote")
	for _, bad := range []string{"remote:", "remote:peer", "remote:peer:", "remote::atlas"} {
		_, _, ok := ParseRemoteMemberID(bad)
		require.False(t, ok, "%q must not parse as a remote identity", bad)
	}
}

func TestDeliverRemoteWithoutFedClientRefusesNamed(t *testing.T) {
	// A remote participant joined while the federation client is not wired:
	// the send records a REFUSED outcome with the named reason — shown,
	// never swallowed (§3.2).
	a := newInstance(t)
	b := newInstance(t)
	require.NoError(t, b.reg.Register(&registry.Agent{ID: "atlas-b"}))
	a.armPeerPolicy(federation.PeerPolicies{
		"peer_b": {Peer: "peer_b", NamespacesAllow: []string{""}}, // no Fed client
	})
	a.handler.SetFedClient(nil) // the case under test: the remote leg is unwired
	sessID := createFederatedSession(t, a)
	status, body := a.do("POST", "/sessions/"+sessID+"/participants",
		`{"member_type":"agent","member_id":"remote:peer_b:atlas-b"}`, nil)
	require.Equal(t, http.StatusCreated, status, "%s", body)
	status, body = a.do("POST", "/sessions/"+sessID+"/messages",
		`{"payload":{"text":"unwired"},"sender":"atlas-a"}`, nil)
	require.Equal(t, http.StatusCreated, status)
	var sent struct {
		Outcomes []outcomeView `json:"outcomes"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &sent))
	require.Len(t, sent.Outcomes, 1)
	require.Equal(t, "refused", sent.Outcomes[0].Outcome)
	require.Contains(t, sent.Outcomes[0].Detail, "FEDERATION_UNCONFIGURED", "outcomes: %+v", sent.Outcomes)
}

// guard: keep fmt imported for the harness' potential use in future cases.
var _ = fmt.Sprintf
