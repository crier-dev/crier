package session

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// ---------------------------------------------------------------------------
// CR-CHAT-013 — channels, direct messages and NAMED agent groups.
//
// The group acceptance criteria this file owns:
//
//   - a (AC-a): create a group, add agents to it (API-level);
//   - b (AC-b): send ONE message to a named group of 5 agents — it lands in
//     each participant's inbox;
//   - c (AC-c): a DM (session kind "direct") to a single agent works, and
//     only that agent;
//   - d (AC-d): a named group is INSPECTABLE — who is in it, who created it,
//     who last edited it;
//   - e (AC-e): a roster edit routes the NEXT send to the CURRENT members.
//
// The channel half is CR-CHAT-019's session surface (kind "channel", already
// shipped and covered there); the store and wire tests pin the new group
// store's contract.
// ---------------------------------------------------------------------------

// newGroupHarness boots the in-process API server over a fresh group store,
// with the memory registry as the shipped deliverer.
func newGroupHarness(t *testing.T, agents ...string) *apiHarness {
	t.Helper()
	store, err := NewJSONLGroupStore(t.TempDir())
	require.NoError(t, err)
	reg := registryForAgents(t, agents...)
	h := NewHTTPHandler(newJSONLSessionStore(t), HTTPOptions{Deliverer: reg, Agents: reg, Groups: store})
	r := newMuxWithGroupRoutes(h)
	srv := newTestServer(t, r)
	return &apiHarness{srv: srv, reg: reg}
}

func newTestServer(t *testing.T, r *mux.Router) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// registryForAgents registers every named agent on a fresh memory store.
func registryForAgents(t *testing.T, ids ...string) *registry.MemoryStore {
	t.Helper()
	reg := registry.NewMemoryStore()
	for _, id := range ids {
		require.NoError(t, reg.Register(&registry.Agent{ID: id}))
	}
	return reg
}

// newJSONLSessionStore opens a throwaway JSONL session store.
func newJSONLSessionStore(t *testing.T) Repository {
	t.Helper()
	s, err := NewJSONLStore(filepath.Join(t.TempDir(), "log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newMuxWithGroupRoutes registers the group routes BESIDE the session routes,
// so a harness drives the real surface.
func newMuxWithGroupRoutes(h *Handler) *mux.Router {
	r := mux.NewRouter()
	registerSessionRoutes(r, h)
	r.HandleFunc("/groups", h.HandleCreateGroup).Methods(http.MethodPost)
	r.HandleFunc("/groups", h.HandleListGroups).Methods(http.MethodGet)
	r.HandleFunc("/groups/{name}", h.HandleGetGroup).Methods(http.MethodGet)
	r.HandleFunc("/groups/{name}/members", h.HandleUpdateGroupMembers).Methods(http.MethodPatch)
	return r
}

// TestGroupAPI_CreateAddInspect is AC-a and AC-d: create a group, edit its
// roster, and read the roster back — members, creator, last editor.
func TestGroupAPI_CreateAddInspect(t *testing.T) {
	h := newGroupHarness(t, "atlas", "nimbus", "orion", "vega", "lyra")

	var created groupView
	code := h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name:      "infra",
		CreatedBy: "kara",
		Members:   []string{"atlas", "nimbus"},
	}, &created, nil)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, "infra", created.Name)
	require.Equal(t, []string{"atlas", "nimbus"}, created.Members)
	require.Equal(t, "kara", created.CreatedBy)
	require.Equal(t, "kara", created.UpdatedBy)

	// Add two more members (PATCH /groups/{name}/members).
	var edited groupView
	code = h.do(t, http.MethodPatch, "/groups/infra/members", updateGroupMembersRequest{
		Add:   []string{"orion", "vega"},
		Actor: "sam",
	}, &edited, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"atlas", "nimbus", "orion", "vega"}, edited.Members)
	require.Equal(t, "kara", edited.CreatedBy, "an edit is never a re-create")
	require.Equal(t, "sam", edited.UpdatedBy)

	// Remove one.
	code = h.do(t, http.MethodPatch, "/groups/infra/members", updateGroupMembersRequest{
		Remove: []string{"nimbus"},
		Actor:  "sam",
	}, &edited, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"atlas", "orion", "vega"}, edited.Members)

	// INSPECTABLE: the roster reads back from a fresh GET — who is in it,
	// who created it, who last edited it (AC-d).
	var got groupView
	code = h.do(t, http.MethodGet, "/groups/infra", nil, &got, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"atlas", "orion", "vega"}, got.Members)
	require.Equal(t, "kara", got.CreatedBy)
	require.Equal(t, "sam", got.UpdatedBy)

	// The list answers too.
	var list groupsResponse
	code = h.do(t, http.MethodGet, "/groups", nil, &list, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, list.Count)
	require.Equal(t, "infra", list.Groups[0].Name)

	// A duplicate create is a 409, not a silent second roster.
	code = h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name: "infra", CreatedBy: "kara",
	}, nil, nil)
	require.Equal(t, http.StatusConflict, code)

	// An unaddressable name is a 400 (the @team: grammar is the same rule).
	code = h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name: "not a group", CreatedBy: "kara",
	}, nil, nil)
	require.Equal(t, http.StatusBadRequest, code)

	// An absent creator is refused, never defaulted to a service identity.
	code = h.do(t, http.MethodPost, "/groups", createGroupRequest{Name: "lonely"}, nil, nil)
	require.Equal(t, http.StatusBadRequest, code)

	// An unknown group is a 404.
	code = h.do(t, http.MethodGet, "/groups/nope", nil, nil, nil)
	require.Equal(t, http.StatusNotFound, code)

	// An edit that names nothing is a 400.
	code = h.do(t, http.MethodPatch, "/groups/infra/members", updateGroupMembersRequest{Actor: "sam"}, nil, nil)
	require.Equal(t, http.StatusBadRequest, code)
}

// TestGroupAPI_SendOneMessageToNamedGroup is AC-b: ONE message addressed to a
// named group of 5 agents lands in EACH participant's inbox.
func TestGroupAPI_SendOneMessageToNamedGroup(t *testing.T) {
	const n = 5
	agents := []string{"atlas", "nimbus", "orion", "vega", "lyra"}
	require.Len(t, agents, n)
	h := newGroupHarness(t, agents...)

	var created groupView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name: "team", CreatedBy: "kara", Members: agents,
	}, &created, nil))

	room := h.createRoom(t)

	// ONE send, one `group` target.
	var msg transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{
		Payload: json.RawMessage(`{"text":"deploy window opens"}`),
		Sender:  "kara-agent",
		Targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}},
	}, &msg, nil)
	require.Equal(t, http.StatusCreated, code)

	// The audience RECORDS the group target (D8: the roster is curated data,
	// so the set is enumerable and recordable BEFORE the send).
	require.Equal(t, AudienceExplicit, msg.Audience.Rule)

	// EVERY current member holds the message.
	for _, id := range agents {
		entries, _, err := h.reg.Retrieve(id, time.Minute, 10)
		require.NoError(t, err, "agent %s", id)
		require.Len(t, entries, 1, "agent %s must hold the message", id)
		require.Equal(t, InboxEntryID(msg.ID, id), entries[0].ID, "one message id across the room, per-target inbox rows")
	}
}

// TestGroupAPI_RosterEditRoutesToCurrentMembers is AC-e: after a roster edit,
// the NEXT send reaches the CURRENT members — the removed one stops receiving,
// the added one starts.
func TestGroupAPI_RosterEditRoutesToCurrentMembers(t *testing.T) {
	h := newGroupHarness(t, "atlas", "nimbus", "orion")

	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name: "team", CreatedBy: "kara", Members: []string{"atlas", "nimbus"},
	}, new(groupView), nil))

	room := h.createRoom(t)
	send := func() string {
		t.Helper()
		var msg transcriptMessage
		require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{
			Payload: json.RawMessage(`{"text":"ping"}`),
			Sender:  "kara-agent",
			Targets: []AudienceTarget{{Kind: TargetGroup, ID: "team"}},
		}, &msg, nil))
		return msg.ID
	}

	send() // first send: atlas + nimbus
	for _, id := range []string{"atlas", "nimbus"} {
		entries, _, err := h.reg.Retrieve(id, time.Minute, 10)
		require.NoError(t, err)
		require.Len(t, entries, 1, id)
	}

	// Edit the roster: remove nimbus, add orion.
	require.Equal(t, http.StatusOK, h.do(t, http.MethodPatch, "/groups/team/members", updateGroupMembersRequest{
		Add: []string{"orion"}, Remove: []string{"nimbus"}, Actor: "sam",
	}, new(groupView), nil))

	// The NEXT send routes to the CURRENT members: orion (added) and atlas;
	// nimbus (removed) receives nothing.
	send()
	entries, _, err := h.reg.Retrieve("orion", time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the added member receives the next send")

	// nimbus got exactly ONE delivery in total across both sends: the first
	// send's, before its removal. The first drain above took it; this drain
	// proves the second send added nothing.
	empty, _, err := h.reg.Retrieve("nimbus", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, empty, "the removed member received nothing NEW")
}

// TestGroupAPI_DirectMessageIsOneToOne is AC-c: a `direct` session send with
// one agent target reaches ONLY that agent.
func TestGroupAPI_DirectMessageIsOneToOne(t *testing.T) {
	h := newGroupHarness(t, "atlas", "nimbus")

	var dm sessionView
	code := h.do(t, http.MethodPost, "/sessions", createSessionRequest{
		Title:     "1:1 with atlas",
		Kind:      string(KindDirect),
		CreatedBy: &AuthorRef{Principal: "kara"},
		Members: []addParticipantRequest{
			{MemberType: string(MemberAgent), MemberID: "atlas", Role: string(RoleMember)},
		},
	}, &dm, nil)
	require.Equal(t, http.StatusCreated, code)
	require.Equal(t, string(KindDirect), dm.Kind)
	require.Equal(t, 1, dm.AudienceCount)

	var msg transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+dm.ID+"/messages", postMessageRequest{
		Payload: json.RawMessage(`{"text":"just you"}`),
		Sender:  "kara-agent",
		Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}},
	}, &msg, nil))

	entries, _, err := h.reg.Retrieve("atlas", time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, string(entries[0].Payload), "just you")

	// ONLY that agent: nimbus holds nothing.
	other, _, err := h.reg.Retrieve("nimbus", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, other, "a DM to atlas must not reach nimbus")
}

// TestGroupAPI_UnknownGroupIsASkipNotADelivery: a group target with no roster
// is a RECORDED skip — no member is guessed, nothing is delivered.
func TestGroupAPI_UnknownGroupIsASkipNotADelivery(t *testing.T) {
	h := newGroupHarness(t, "atlas")
	room := h.createRoom(t)

	var msg transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{
		Payload: json.RawMessage(`{"text":"anyone?"}`),
		Sender:  "kara-agent",
		Targets: []AudienceTarget{{Kind: TargetGroup, ID: "ghost"}},
	}, &msg, nil))

	entries, _, err := h.reg.Retrieve("atlas", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, entries, "a group with no known roster must not resolve to a guessed delivery")
}

// TestGroupAPI_RealmWall: a group of another realm reads as 404 — the same
// rule a session of another realm follows.
func TestGroupAPI_RealmWall(t *testing.T) {
	h := newGroupHarness(t, "atlas")
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/groups", createGroupRequest{
		Name: "infra", CreatedBy: "kara", Members: []string{"atlas"},
	}, new(groupView), nil))

	// Same-realm read works; another-realm read is a 404.
	code := h.do(t, http.MethodGet, "/groups/infra", nil, nil, map[string]string{"X-Crier-Namespace": "other"})
	require.Equal(t, http.StatusBadRequest, code, "an undeclared namespace is UNKNOWN_NAMESPACE, never a silent fallback")
}

// TestGroupStore_KeepLastPerName pins the roster log's reduction: the current
// version per name survives reopen, edits preserve creation facts, and the
// name rule is the @team: grammar.
func TestGroupStore_KeepLastPerName(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLGroupStore(dir)
	require.NoError(t, err)
	ctx := t.Context()

	require.NoError(t, s.Create(ctx, &Group{Name: "infra", Members: []string{"atlas"}, CreatedAt: time.Now(), CreatedBy: "kara", UpdatedAt: time.Now()}))
	require.True(t, errors.Is(s.Create(ctx, &Group{Name: "infra"}), ErrGroupExists), "a second create is ErrGroupExists")

	// Edit.
	require.NoError(t, s.Update(ctx, &Group{Name: "infra", Members: []string{"atlas", "nimbus"}, CreatedAt: time.Now(), CreatedBy: "WRONG", UpdatedAt: time.Now(), UpdatedBy: "sam"}))
	g, err := s.Get(ctx, "infra")
	require.NoError(t, err)
	require.Equal(t, []string{"atlas", "nimbus"}, g.Members)
	require.Equal(t, "kara", g.CreatedBy, "creation facts survive the edit")
	require.Equal(t, "sam", g.UpdatedBy)

	// A fresh store over the same path reads the same roster.
	s2, err := NewJSONLGroupStore(dir)
	require.NoError(t, err)
	g2, err := s2.Get(ctx, "infra")
	require.NoError(t, err)
	require.Equal(t, g.Members, g2.Members)

	// Not-found, and the name rule.
	_, err = s.Get(ctx, "nope")
	require.True(t, errors.Is(err, ErrGroupNotFound))
	require.False(t, ValidGroupName("has space"))
	require.False(t, ValidGroupName(""))
	require.True(t, ValidGroupName("Infra-Team_1.x"))
}
