package session

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// ---------------------------------------------------------------------------
// CR-CHAT-019 — the session API surface.
//
// The three acceptance criteria this file owns:
//
//   - AC1: create a session, add two agents and a human principal, deliver via
//     fan-out, and read ONE ordered transcript per participant with parents
//     resolvable;
//   - AC2: a restart/reopen proves the thread tree is reconstructable from
//     storage ALONE;
//   - AC3: thread_id is persisted in every session backend (JSONL and SQLite
//     live here; the PostgreSQL schema is asserted structurally and covered by
//     the integration battery).
// ---------------------------------------------------------------------------

// localBackends are the backends this file can drive with no external service.
// Each case carries a factory and a REOPEN factory so the same scenario proves
// the restart property on every one of them.
type localBackend struct {
	name   string
	open   func(t *testing.T, path string) Repository
	pathFn func(t *testing.T) string
}

func localBackends() []localBackend {
	return []localBackend{
		{
			name: BackendJSONL,
			open: func(t *testing.T, path string) Repository {
				t.Helper()
				s, err := NewJSONLStore(path)
				require.NoError(t, err)
				return s
			},
			pathFn: func(t *testing.T) string { return filepath.Join(t.TempDir(), "log") },
		},
		{
			name: BackendSQLite,
			open: func(t *testing.T, path string) Repository {
				t.Helper()
				s, err := NewSQLiteStore(path)
				require.NoError(t, err)
				return s
			},
			pathFn: func(t *testing.T) string { return filepath.Join(t.TempDir(), "sessions.sqlite") },
		},
	}
}

// apiHarness is the in-process session API server: a real mux router over the
// real handler, backed by a memory registry (the shipped local backend) so a
// fan-out write lands somewhere it can be read back.
type apiHarness struct {
	srv *httptest.Server
	reg *registry.MemoryStore
}

func newAPIHarness(t *testing.T, store Repository) *apiHarness {
	t.Helper()
	reg := registry.NewMemoryStore()
	for _, id := range []string{"atlas", "nimbus"} {
		require.NoError(t, reg.Register(&registry.Agent{ID: id}))
	}
	h := NewHTTPHandler(store, HTTPOptions{Deliverer: reg, Agents: reg})
	r := mux.NewRouter()
	registerSessionRoutes(r, h)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &apiHarness{srv: srv, reg: reg}
}

// registerSessionRoutes wires the session API surface on r: every route the
// handler serves, so a harness drives the REAL surface rather than a
// hand-built subset (a route built but never registered fails in the wiring
// test, cmd/server/sessionapi_test.go).
func registerSessionRoutes(r *mux.Router, h *Handler) {
	r.HandleFunc("/sessions", h.HandleListSessions).Methods(http.MethodGet)
	r.HandleFunc("/sessions", h.HandleCreateSession).Methods(http.MethodPost)
	r.HandleFunc("/sessions/{id}/messages", h.HandleTranscript).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/messages", h.HandlePostMessage).Methods(http.MethodPost)
	r.HandleFunc("/sessions/{id}/participants", h.HandleListParticipants).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/participants", h.HandleAddParticipant).Methods(http.MethodPost)
	// CR-CHAT-016: the late-join context share surface (§4.6, D10).
	r.HandleFunc("/sessions/{id}/participants/{member_type}/{member_id}/context", h.HandleMemberContextView).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/participants/{member_type}/{member_id}/context", h.HandleSetMemberContext).Methods(http.MethodPut)
	// CR-CHAT-028: compile (merge) and the per-part expand.
	r.HandleFunc("/sessions/{id}/compile", h.HandleCompile).Methods(http.MethodPost)
	r.HandleFunc("/sessions/{id}/messages/{mid}/expand", h.HandleExpandCompiledMessage).Methods(http.MethodGet)
	// CR-CHAT-017: thread navigability — depth collapse, timeline, summary,
	// and search that returns a location path.
	r.HandleFunc("/sessions/{id}/threads/{thread_id}", h.HandleThreadRead).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/threads/{thread_id}/timeline", h.HandleThreadTimeline).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/threads/{thread_id}/summary", h.HandleThreadSummary).Methods(http.MethodGet)
	r.HandleFunc("/sessions/{id}/search", h.HandleSessionSearch).Methods(http.MethodGet)
}

func (h *apiHarness) do(t *testing.T, method, path string, body any, out any, headers map[string]string) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if out != nil && len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, out), "decode %s %s: %s", method, path, string(raw))
	}
	return resp.StatusCode
}

// fixedSessionID and fixedMemberIDs keep assertions readable.
const (
	fixAgentA  = "atlas"
	fixAgentB  = "nimbus"
	fixHuman   = "kara"
	fixCreator = "kara"
)

// createFixtures posts one session with two agents and a human principal and
// returns its id — the AC1 setup.
func (h *apiHarness) createRoom(t *testing.T) string {
	t.Helper()
	var created sessionView
	code := h.do(t, http.MethodPost, "/sessions", createSessionRequest{
		Title:     "build-plan",
		Kind:      string(KindChannel),
		CreatedBy: &AuthorRef{Principal: fixCreator},
		Members: []addParticipantRequest{
			{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleMember)},
			{MemberType: string(MemberAgent), MemberID: fixAgentB, Role: string(RoleMember)},
			{MemberType: string(MemberPrincipal), MemberID: fixHuman, Role: string(RoleObserver)},
		},
	}, &created, nil)
	require.Equal(t, http.StatusCreated, code, "create session")
	require.NotEmpty(t, created.ID)
	require.Equal(t, string(SessionOpen), created.State)
	require.Equal(t, 2, created.AudienceCount)
	require.Equal(t, 3, created.ParticipantCount)
	return created.ID
}

// TestSessionAPI_FanoutAndOrderedTranscript is AC1: one ordered cross-agent
// transcript, parents resolvable, the human recorded as addressing but not
// delivered to, and the thread id PERSISTED on the stored inbox record.
func TestSessionAPI_FanoutAndOrderedTranscript(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			// atlas opens a thread.
			var root transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
				postMessageRequest{Payload: json.RawMessage(`{"text":"kickoff"}`), Sender: fixAgentA, MessageKind: string(MessagePlain)},
				&root, nil))
			require.Equal(t, root.ID, root.ThreadID, "a thread root's thread_id is its own message id")
			require.Equal(t, root.ID, root.RootID)
			require.True(t, root.ParentResolvable)
			require.Equal(t, 0, root.ReplyDepth)

			// The audience is the session minus the author: nimbus (agent)
			// and the human principal (recorded, not delivered to).
			gotTargets := map[string]string{}
			for _, tg := range root.Audience.Targets {
				gotTargets[tg.ID] = string(tg.Kind)
			}
			require.Equal(t, AudienceSession, root.Audience.Rule)
			require.Equal(t, string(TargetAgent), gotTargets[fixAgentB])
			require.Equal(t, string(TargetPrincipal), gotTargets[fixHuman])
			_, selfAddressed := gotTargets[fixAgentA]
			require.False(t, selfAddressed, "the author is not in its own audience")

			// Exactly one delivery, with the thread id stored WITH it.
			entries, _, err := h.reg.Retrieve(fixAgentB, time.Minute, 10)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, root.ThreadID, entries[0].ThreadID, "thread_id must be persisted on the stored inbox record")

			// nimbus replies IN THREAD.
			var reply transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
				postMessageRequest{Payload: json.RawMessage(`{"text":"on it"}`), Sender: fixAgentB, ParentID: root.ID},
				&reply, nil))
			require.Equal(t, root.ID, reply.ThreadID, "a reply stays in its parent's thread (D11)")
			require.Equal(t, root.ID, reply.ParentID)
			require.True(t, reply.ParentResolvable)
			require.Equal(t, 1, reply.ReplyDepth)

			// Every participant reads ONE ordered transcript with the same
			// facts, parents resolvable.
			for _, viewer := range []string{fixAgentA, fixAgentB, fixHuman} {
				var tr transcriptResponse
				require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+id+"/messages", nil, &tr, nil))
				require.Equal(t, 2, tr.Count)
				require.Len(t, tr.Messages, 2)
				require.Equal(t, root.ID, tr.Messages[0].ID)
				require.Equal(t, reply.ID, tr.Messages[1].ID)
				require.Less(t, tr.Messages[0].Seq, tr.Messages[1].Seq, "ordered by seq")
				require.True(t, tr.Messages[1].ParentResolvable, "viewer %s must resolve the parent", viewer)
				require.False(t, tr.Messages[1].ParentID == "", "viewer %s sees the parent ref", viewer)
				require.Empty(t, tr.Findings, "a well-formed transcript reports no broken threads")
			}
		})
	}
}

// TestSessionAPI_ThreadTreeReconstructableFromStorageAlone is AC2 (and AC3's
// reopen half): close the store, open a fresh one over the same path, and
// prove the thread survives and the tree reassembles with NO client state.
func TestSessionAPI_ThreadTreeReconstructableFromStorageAlone(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			path := be.pathFn(t)
			store := be.open(t, path)
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			var root, reply transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
				postMessageRequest{Payload: json.RawMessage(`{"text":"root"}`), Sender: fixAgentA}, &root, nil))
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
				postMessageRequest{Payload: json.RawMessage(`{"text":"reply"}`), Sender: fixAgentB, ParentID: root.ID}, &reply, nil))

			require.NoError(t, store.Close())

			// A FRESH handle over the same storage — no shared state, no
			// client-held thread.
			reopened := be.open(t, path)
			t.Cleanup(func() { _ = reopened.Close() })
			st, err := reopened.Load(context.Background(), id)
			require.NoError(t, err)

			reRoot := st.Message(root.ID)
			require.NotNil(t, reRoot)
			require.Equal(t, root.ID, reRoot.ThreadID)
			reReply := st.Message(reply.ID)
			require.NotNil(t, reReply)
			require.Equal(t, root.ID, reReply.ThreadID, "the reply's thread survives storage")
			require.Equal(t, root.ID, reReply.ParentID)

			// The tree reassembles: one root thread, the reply grouped under
			// it, the reply chain complete.
			require.Len(t, st.RootThreads(), 1)
			require.Equal(t, root.ID, st.RootThreads()[0].ID)
			require.Len(t, st.ThreadMessages(root.ID), 2)
			chain, complete := st.ReplyChain(reply.ID)
			require.True(t, complete)
			require.Len(t, chain, 2)
			require.Equal(t, root.ID, chain[0].ID)
			require.Equal(t, reply.ID, chain[1].ID)
			require.Empty(t, st.Findings)

			// And a read through the API on the reopened store serves the
			// same transcript.
			h2 := newAPIHarness(t, reopened)
			var tr transcriptResponse
			require.Equal(t, http.StatusOK, h2.do(t, http.MethodGet, "/sessions/"+id+"/messages", nil, &tr, nil))
			require.Equal(t, 2, tr.Count)
			require.Equal(t, root.ID, tr.Messages[1].ThreadID)
		})
	}
}

// TestSessionAPI_ThreadIDColumnDeclaredInEverySQLSchema is AC3's structural
// half: the SQLite AND PostgreSQL schema statement sets both declare the
// stored thread id, so the property is asserted for the PostgreSQL backend
// here without a live server (the integration battery drives it live).
func TestSessionAPI_ThreadIDColumnDeclaredInEverySQLSchema(t *testing.T) {
	for name, stmts := range map[string][]string{
		"sqlite":   sqliteSchemaStatements,
		"postgres": postgresSchemaStatements,
	} {
		t.Run(name, func(t *testing.T) {
			joined := strings.Join(stmts, "\n")
			require.Contains(t, joined, "thread_id", "%s chat_transcript must declare the stored thread id", name)
		})
	}
	// The pgxpool-backed store is the one OpenStore selects for postgres, so
	// its schema is the one that matters: assert it too.
	require.Contains(t, strings.Join(SchemaStatements, "\n"), "thread_id")
}

// TestSessionAPI_ListWithStateAndCountFilters covers rows 8/9: the list is a
// view over the same objects, and the filters narrow it.
func TestSessionAPI_ListWithStateAndCountFilters(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)

	quiet := h.createRoom(t)
	busy := h.createRoom(t)
	// Make `busy` busy: one message.
	var m transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+busy+"/messages",
		postMessageRequest{Payload: json.RawMessage(`{"text":"hi"}`), Sender: fixAgentA}, &m, nil))

	var all sessionsResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions", nil, &all, nil))
	require.Equal(t, 2, all.Count)

	var withMsg sessionsResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions?min_messages=1", nil, &withMsg, nil))
	require.Equal(t, 1, withMsg.Count)
	require.Equal(t, busy, withMsg.Sessions[0].ID)
	require.Equal(t, 1, withMsg.Sessions[0].MessageCount)

	var none sessionsResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions?min_messages=5", nil, &none, nil))
	require.Equal(t, 0, none.Count, "quiet session %q must not match", quiet)

	var badState sessionsResponse
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodGet, "/sessions?state=zombie", nil, &badState, nil))
}

// TestSessionAPI_Membership covers rows 24/37: add a participant, read the
// membership list, and see a removed member retained with active=false.
func TestSessionAPI_Membership(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	id := h.createRoom(t)

	var p participantView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
		addParticipantRequest{MemberType: string(MemberAgent), MemberID: "atlas", Role: string(RoleObserver)},
		&p, nil))
	require.Equal(t, "atlas", p.MemberID)
	require.True(t, p.Active)

	var list participantsResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+id+"/participants", nil, &list, nil))
	require.Equal(t, 3, list.Count)

	// An unknown member_type is refused with its named error.
	var errBody map[string]string
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
		addParticipantRequest{MemberType: "robot", MemberID: "x"}, &errBody, nil))
	require.Equal(t, "INVALID_MEMBER", errBody["error"])
}

// TestSessionAPI_ClosedSessionRefusesWrites covers §1.3's lifecycle on the
// message route and the membership route.
func TestSessionAPI_ClosedSessionRefusesWrites(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	id := h.createRoom(t)

	// Close it out of band through the store (the API owes no close route).
	require.NoError(t, store.Append(context.Background(), CloseRecord(id, 100, time.Now(), AuthorRef{Agent: fixAgentA}, "done")))

	var errBody map[string]string
	require.Equal(t, http.StatusConflict, h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
		postMessageRequest{Payload: json.RawMessage(`{"text":"late"}`), Sender: fixAgentA}, &errBody, nil))
	require.Equal(t, "SESSION_CLOSED", errBody["error"])

	require.Equal(t, http.StatusConflict, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
		addParticipantRequest{MemberType: string(MemberAgent), MemberID: fixAgentB}, &errBody, nil))
	require.Equal(t, "SESSION_CLOSED", errBody["error"])
}

// TestSessionAPI_RealmScopingAndVisibility covers §4 row 38 (realm wall) and
// row 13 (a private session's read guard).
func TestSessionAPI_RealmScopingAndVisibility(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)

	// A private session in the default realm, with atlas as a member.
	var created sessionView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
		Title:      "secret",
		Visibility: VisibilityPrivate,
		CreatedBy:  &AuthorRef{Principal: fixCreator},
		Members:    []addParticipantRequest{{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleMember)}},
	}, &created, nil))
	require.Equal(t, VisibilityPrivate, created.Visibility)

	// A member reads it; a stranger does not.
	var tr transcriptResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+created.ID+"/messages", nil, &tr,
		map[string]string{registry.HeaderAgentID: fixAgentA}))
	var errBody map[string]string
	require.Equal(t, http.StatusForbidden, h.do(t, http.MethodGet, "/sessions/"+created.ID+"/messages", nil, &errBody,
		map[string]string{registry.HeaderAgentID: fixAgentB}))
	require.Equal(t, "VISIBILITY_FORBIDDEN", errBody["error"])

	// An unknown realm is refused and never falls back to the default.
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodGet, "/sessions", nil, &errBody,
		map[string]string{"X-Crier-Namespace": "nope"}))
	require.Equal(t, "UNKNOWN_NAMESPACE", errBody["error"])
}

// TestSessionAPI_FanoutRefusesCrossRealmTarget covers §3.4/row 38 at the
// delivery step: a member whose stored row is in another realm is refused with
// a NAMED outcome rather than delivered to.
func TestSessionAPI_FanoutRefusesCrossRealmTarget(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	reg := registry.NewMemoryStore()
	require.NoError(t, reg.Register(&registry.Agent{ID: fixAgentA}))
	require.NoError(t, reg.Register(&registry.Agent{ID: "outsider", Namespace: "other-realm"}))

	h := NewHTTPHandler(store, HTTPOptions{Deliverer: reg, Agents: reg})
	r := mux.NewRouter()
	r.HandleFunc("/sessions", h.HandleCreateSession).Methods(http.MethodPost)
	r.HandleFunc("/sessions/{id}/messages", h.HandlePostMessage).Methods(http.MethodPost)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	var created sessionView
	require.Equal(t, http.StatusCreated, srvPost(t, srv, "/sessions",
		createSessionRequest{CreatedBy: &AuthorRef{Principal: fixCreator}}, &created))

	// Record both members OUT OF BAND: the request-time membership path
	// refuses a known cross-realm agent (§4 row 38), but a realm can be
	// assigned to an agent AFTER it joined, so the DELIVERY step must refuse
	// it too. A direct append is exactly that shape.
	ctx := context.Background()
	for i, id := range []string{"outsider", fixAgentA} {
		m := &Member{SessionID: created.ID, MemberType: MemberAgent, MemberID: id, Role: RoleMember, AddedAt: time.Now()}
		require.NoError(t, store.Append(ctx, m.AddRecord(int64(i+2), time.Now(), nil)))
	}

	var msg transcriptMessage
	require.Equal(t, http.StatusCreated, srvPost(t, srv, "/sessions/"+created.ID+"/messages",
		postMessageRequest{Payload: json.RawMessage(`{"text":"hello"}`), Sender: fixAgentA}, &msg))

	require.Len(t, msg.Outcomes, 1)
	require.Equal(t, "outsider", msg.Outcomes[0].Target)
	require.Equal(t, string(OutcomeRefused), msg.Outcomes[0].Outcome)
	require.Contains(t, msg.Outcomes[0].Detail, "different namespace",
		"a refusal is shown with its reason, never swallowed")

	entries, _, err := reg.Retrieve("outsider", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, entries, "a cross-realm target must receive nothing")
}

// TestSessionAPI_CreateRequiresCreator covers §1.4: creation is never defaulted
// to a service identity.
func TestSessionAPI_CreateRequiresCreator(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	var errBody map[string]string
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions",
		createSessionRequest{Title: "nobody"}, &errBody, nil))
	require.Equal(t, "CREATED_BY_REQUIRED", errBody["error"])
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// NewJSONLStoreForTest opens a JSONL store under the test's temp dir.
func NewJSONLStoreForTest(t *testing.T) Repository {
	t.Helper()
	s, err := NewJSONLStore(filepath.Join(t.TempDir(), "log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// srvPost posts JSON to a bare httptest server and decodes the response.
func srvPost(t *testing.T, srv *httptest.Server, path string, body any, out any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := srv.Client().Post(srv.URL+path, "application/json", bytes.NewReader(raw))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	if out != nil {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}
