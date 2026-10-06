package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/registry"
)

// ---------------------------------------------------------------------------
// CR-CHAT-028 — compile (merge) messages into ONE context bundle.
//
// The three acceptance criteria this file owns:
//
//   - AC1: select messages across two threads/sessions, merge them and tag an
//     agent; the agent receives ONE message whose parts each cite a source id;
//   - AC2: expanding a part resolves back to the ORIGINAL (content + location);
//   - AC3: a source the compiler could NOT read is OMITTED WITH A NAMED GAP (or
//     the merge is REFUSED) — never silently included, never silently dropped.
//
// …plus the design rule they rest on: the compiled message is a NEW message and
// the sources are left exactly as they were.
// ---------------------------------------------------------------------------

// newAPIHarnessWith builds the in-process session API harness with extra
// handler options (the CR-CHAT-028 realm test needs a declared namespace set).
func newAPIHarnessWith(t *testing.T, store Repository, opts HTTPOptions) *apiHarness {
	t.Helper()
	reg := registry.NewMemoryStore()
	for _, id := range []string{fixAgentA, fixAgentB} {
		require.NoError(t, reg.Register(&registry.Agent{ID: id}))
	}
	opts.Deliverer = reg
	opts.Agents = reg
	h := NewHTTPHandler(store, opts)
	r := mux.NewRouter()
	registerSessionRoutes(r, h)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &apiHarness{srv: srv, reg: reg}
}

// postTo composes one message into a session and returns the recorded view.
func (h *apiHarness) postTo(t *testing.T, sessionID string, req postMessageRequest) transcriptMessage {
	t.Helper()
	var msg transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessionID+"/messages", req, &msg, nil),
		"post into %s", sessionID)
	require.NotEmpty(t, msg.ID)
	return msg
}

// transcriptOf reads a session's ordered transcript.
func (h *apiHarness) transcriptOf(t *testing.T, sessionID string) transcriptResponse {
	t.Helper()
	var tr transcriptResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+sessionID+"/messages", nil, &tr, nil))
	return tr
}

// decodeBundle parses a compiled message's payload back into its bundle.
func decodeBundle(t *testing.T, payload json.RawMessage) CompiledBundle {
	t.Helper()
	var b CompiledBundle
	require.NoError(t, json.Unmarshal(payload, &b), "payload: %s", string(payload))
	return b
}

// sameJSON compares two opaque payloads by canonical JSON, so a whitespace or
// key-order difference is not read as a content change.
func sameJSON(t *testing.T, want, got json.RawMessage) {
	t.Helper()
	w, err := CanonicalJSON(want)
	require.NoError(t, err)
	g, err := CanonicalJSON(got)
	require.NoError(t, err)
	require.Equal(t, string(w), string(g))
}

// TestCompile_MergesAcrossThreadsAndSessionsWithACitationPerPart is AC1 plus the
// design rule: one NEW message whose every part cites its source, delivered to
// the tagged agent through the shipped path, with the sources untouched.
func TestCompile_MergesAcrossThreadsAndSessionsWithACitationPerPart(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	roomA := h.createRoom(t)
	roomB := h.createRoom(t)

	// Session A: a thread (root + reply) and a message from the human.
	msgA1 := h.postTo(t, roomA, postMessageRequest{Payload: json.RawMessage(`{"text":"A root"}`), Sender: fixAgentA})
	msgA2 := h.postTo(t, roomA, postMessageRequest{Payload: json.RawMessage(`{"text":"A reply"}`), Sender: fixAgentB, ParentID: msgA1.ID})
	msgHuman := h.postTo(t, roomA, postMessageRequest{Payload: json.RawMessage(`{"text":"what the human said"}`), PrincipalID: fixHuman})
	// Session B: a second thread.
	msgB1 := h.postTo(t, roomB, postMessageRequest{Payload: json.RawMessage(`{"text":"B root"}`), Sender: fixAgentA})

	beforeA := h.transcriptOf(t, roomA)
	beforeB := h.transcriptOf(t, roomB)

	// The merge: two sessions, two threads, three authors — one human.
	var out compileResponse
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+roomA+"/compile", compileRequest{
		Sources: []compileSourceRef{
			{SessionID: roomA, MessageID: msgA1.ID},
			{SessionID: roomA, MessageID: msgA2.ID},
			// The shorthand form: no session_id means "this session".
			{MessageID: msgHuman.ID},
			// …and a source from the other session.
			{SessionID: roomB, MessageID: msgB1.ID},
		},
		Note:   "gathered context",
		Sender: fixAgentA,
		Targets: []AudienceTarget{
			{Kind: TargetAgent, ID: fixAgentB},
		},
	}, &out, nil), "compile")

	compiled := out.Message
	require.NotEmpty(t, compiled.ID)

	// The compiled message is a NEW message: its id is none of the sources',
	// it opens its own thread, and it is not a reply to anything.
	for _, src := range []string{msgA1.ID, msgA2.ID, msgHuman.ID, msgB1.ID} {
		require.NotEqual(t, src, compiled.ID, "the merge must not reuse a source's message id")
	}
	require.Equal(t, compiled.ID, compiled.ThreadID, "a compile opens its own thread")
	require.Empty(t, compiled.ParentID, "a compile is not a reply — a citation is not reply attribution")
	require.Equal(t, beforeA.Messages[len(beforeA.Messages)-1].Seq+1, compiled.Seq, "the compile is the session's next message")
	// D12: tagging an agent is addressing, never a task.
	require.Equal(t, string(MessageAddressed), compiled.MessageKind)
	require.Equal(t, roomA, beforeA.Session.ID)

	require.Equal(t, 4, out.PartsCount)
	require.Equal(t, 4, out.ResolvedCount)
	require.Equal(t, 0, out.OmittedCount)
	require.Empty(t, out.Gaps)

	// Every part cites its source, inline.
	bundle := decodeBundle(t, compiled.Payload)
	require.Equal(t, CompiledBundleKind, bundle.Kind)
	require.Equal(t, "gathered context", bundle.Text)
	require.Equal(t, 4, bundle.Count)
	require.Equal(t, 0, bundle.OmittedCount)
	require.Len(t, bundle.Parts, 4)

	wantSources := []compileSourceRef{
		{SessionID: roomA, MessageID: msgA1.ID},
		{SessionID: roomA, MessageID: msgA2.ID},
		{SessionID: roomA, MessageID: msgHuman.ID},
		{SessionID: roomB, MessageID: msgB1.ID},
	}
	for i, part := range bundle.Parts {
		require.True(t, part.Cited, "part %d must cite its source", i)
		require.Equal(t, wantSources[i].SessionID, part.SourceSessionID, "part %d source session", i)
		require.Equal(t, wantSources[i].MessageID, part.SourceMessageID, "part %d source message", i)
		require.NotEmpty(t, part.Content, "part %d carries the source content inline", i)
		require.NotNil(t, part.Author, "part %d carries the source author", i)
		require.NotNil(t, part.CreatedAt, "part %d carries the source timestamp", i)
		require.Empty(t, part.Omitted)
	}
	// The inline content IS the source's content, not a copy with a new shape.
	sameJSON(t, msgA1.Payload, bundle.Parts[0].Content)
	sameJSON(t, msgB1.Payload, bundle.Parts[3].Content)
	// "What the humans said" keeps its author: a principal, not an agent.
	require.Equal(t, fixHuman, bundle.Parts[2].Author.Principal)
	require.Empty(t, bundle.Parts[2].Author.Agent)

	// The tagged agent receives ONE message, through the shipped inbox path.
	require.NotEmpty(t, compiled.Outcomes)
	delivered := map[string]string{}
	for _, o := range compiled.Outcomes {
		delivered[o.Target] = o.Outcome
	}
	require.Equal(t, string(OutcomeDelivered), delivered[fixAgentB], "the tagged agent is delivered to")
	_, selfTagged := delivered[fixAgentA]
	require.False(t, selfTagged, "the compiler is not in its own audience")

	entries, _, err := h.reg.Retrieve(fixAgentB, time.Minute, 50)
	require.NoError(t, err)
	var got []string
	for _, e := range entries {
		if e.ID == InboxEntryID(compiled.ID, fixAgentB) {
			got = append(got, string(e.Payload))
		}
	}
	require.Len(t, got, 1, "exactly ONE delivery of the compiled message")
	require.Contains(t, got[0], `"kind":"compiled"`)
	require.Contains(t, got[0], msgB1.ID, "the delivered payload carries the citations")

	// The sources are UNCHANGED: the compile added a message and mutated
	// nothing else.
	afterA := h.transcriptOf(t, roomA)
	require.Equal(t, beforeA.Count+1, afterA.Count, "the compile adds ONE message to the target session")
	for i, before := range beforeA.Messages {
		require.Equal(t, before.ID, afterA.Messages[i].ID)
		require.Equal(t, before.Seq, afterA.Messages[i].Seq)
		require.Equal(t, before.ThreadID, afterA.Messages[i].ThreadID)
		require.Equal(t, before.ParentID, afterA.Messages[i].ParentID)
		sameJSON(t, before.Payload, afterA.Messages[i].Payload)
	}
	afterB := h.transcriptOf(t, roomB)
	require.Equal(t, beforeB.Count, afterB.Count, "a source session is not written to")
	sameJSON(t, beforeB.Messages[0].Payload, afterB.Messages[0].Payload)
}

// TestCompile_ExpandResolvesEveryPartToItsOriginal is AC2: a citation resolves
// back to the source message's content AND its location.
func TestCompile_ExpandResolvesEveryPartToItsOriginal(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	roomA := h.createRoom(t)
	roomB := h.createRoom(t)

	msgA := h.postTo(t, roomA, postMessageRequest{Payload: json.RawMessage(`{"text":"originating A"}`), Sender: fixAgentA})
	msgB := h.postTo(t, roomB, postMessageRequest{Payload: json.RawMessage(`{"text":"originating B"}`), Sender: fixAgentB})

	var out compileResponse
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+roomA+"/compile", compileRequest{
		Sources: []compileSourceRef{
			{SessionID: roomA, MessageID: msgA.ID},
			{SessionID: roomB, MessageID: msgB.ID},
		},
		Sender:  fixAgentA,
		Targets: []AudienceTarget{{Kind: TargetAgent, ID: fixAgentB}},
	}, &out, nil))

	var exp expandResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet,
		"/sessions/"+roomA+"/messages/"+out.Message.ID+"/expand", nil, &exp, nil), "expand")

	require.Equal(t, out.Message.ID, exp.MessageID)
	require.Equal(t, roomA, exp.SessionID)
	require.Equal(t, 2, exp.Count)
	require.Equal(t, 2, exp.ResolvedCount)
	require.Equal(t, 0, exp.OmittedCount)

	for i, want := range []struct {
		session string
		msg     transcriptMessage
	}{{roomA, msgA}, {roomB, msgB}} {
		part := exp.Parts[i]
		require.True(t, part.Resolved, "part %d resolves", i)
		require.True(t, part.Cited)
		require.Equal(t, want.session, part.SourceSessionID, "part %d location: session", i)
		require.Equal(t, want.msg.ID, part.SourceMessageID, "part %d location: message", i)
		require.Equal(t, want.msg.Seq, part.Seq, "part %d location: seq", i)
		require.Equal(t, want.msg.ThreadID, part.ThreadID, "part %d location: thread", i)
		require.Equal(t, want.msg.Author.Agent, part.Author.Agent, "part %d author", i)
		require.NotNil(t, part.CreatedAt)
		require.WithinDuration(t, want.msg.CreatedAt, *part.CreatedAt, time.Second)
		sameJSON(t, want.msg.Payload, part.Content)
	}

	// A message that is not a compile cannot be expanded, and an unknown
	// message is a 404 — neither is an empty success.
	var errBody map[string]string
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodGet,
		"/sessions/"+roomA+"/messages/"+msgA.ID+"/expand", nil, &errBody, nil))
	require.Equal(t, "NOT_COMPILED", errBody["error"])
	require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet,
		"/sessions/"+roomA+"/messages/no-such-message/expand", nil, &errBody, nil))
	require.Equal(t, "MESSAGE_NOT_FOUND", errBody["error"])
}

// TestCompile_UnreadableSourceIsANamedGapNeverSilent is AC3: a source the
// compiler could not read is reported by name, and a merge with NOTHING
// readable is refused.
func TestCompile_UnreadableSourceIsANamedGapNeverSilent(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	room := h.createRoom(t)
	good := h.postTo(t, room, postMessageRequest{Payload: json.RawMessage(`{"text":"readable"}`), Sender: fixAgentA})

	var out compileResponse
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources: []compileSourceRef{
			{MessageID: good.ID},
			{MessageID: "no-such-message"},
			{SessionID: "no-such-session", MessageID: "whatever"},
		},
		Sender:  fixAgentA,
		Targets: []AudienceTarget{{Kind: TargetAgent, ID: fixAgentB}},
	}, &out, nil))

	require.Equal(t, 3, out.PartsCount)
	require.Equal(t, 1, out.ResolvedCount)
	require.Equal(t, 2, out.OmittedCount)
	require.Len(t, out.Gaps, 2)
	require.Contains(t, out.Gaps[0], "no-such-message")
	require.Contains(t, out.Gaps[1], "no-such-session")

	// The unreadable sources are PRESENT in the bundle — cited, and named.
	bundle := decodeBundle(t, out.Message.Payload)
	require.Len(t, bundle.Parts, 3, "an unreadable source is never dropped from the merge")
	require.True(t, bundle.Parts[0].Cited)
	require.Empty(t, bundle.Parts[0].Omitted)
	for i := 1; i < 3; i++ {
		require.False(t, bundle.Parts[i].Cited, "part %d does not claim to cite content it could not read", i)
		require.Empty(t, bundle.Parts[i].Content)
		require.NotEmpty(t, bundle.Parts[i].Omitted, "part %d carries a NAMED gap", i)
	}
	require.Contains(t, bundle.Parts[1].Omitted, "no-such-message")
	require.Contains(t, bundle.Parts[2].Omitted, "no-such-session")
	require.Equal(t, 2, bundle.OmittedCount)

	// Nothing readable at all: the merge is REFUSED, with the gaps — never an
	// empty shell, and never a message.
	before := h.transcriptOf(t, room)
	var errBody map[string]string
	require.Equal(t, http.StatusUnprocessableEntity, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources: []compileSourceRef{{MessageID: "nope-1"}, {MessageID: "nope-2"}},
		Sender:  fixAgentA,
	}, &errBody, nil))
	require.Equal(t, "COMPILE_NO_READABLE_SOURCE", errBody["error"])
	require.Contains(t, errBody["detail"], "nope-1")
	require.Contains(t, errBody["detail"], "nope-2")
	require.Equal(t, before.Count, h.transcriptOf(t, room).Count, "a refused merge writes no message")
}

// TestCompile_PrivateSourceIsANamedGapForACallerWhoMayNotReadIt is AC3's
// permission arm, and AC2's counterpart: a source resolves for a caller who may
// read it and is a NAMED GAP for one who may not.
func TestCompile_PrivateSourceIsANamedGapForACallerWhoMayNotReadIt(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)

	// A private session the compiler (atlas) is a member of, and nimbus is not.
	var private sessionView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
		Title:      "private-source",
		Visibility: VisibilityPrivate,
		CreatedBy:  &AuthorRef{Principal: fixHuman},
		Members:    []addParticipantRequest{{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleMember)}},
	}, &private, nil))
	secret := h.postTo(t, private.ID, postMessageRequest{Payload: json.RawMessage(`{"text":"private note"}`), Sender: fixAgentA})

	room := h.createRoom(t)
	public := h.postTo(t, room, postMessageRequest{Payload: json.RawMessage(`{"text":"public note"}`), Sender: fixAgentA})

	expand := func(t *testing.T, compiledID string, headers map[string]string) expandResponse {
		t.Helper()
		var exp expandResponse
		require.Equal(t, http.StatusOK, h.do(t, http.MethodGet,
			"/sessions/"+room+"/messages/"+compiledID+"/expand", nil, &exp, headers))
		return exp
	}

	// (a) The compiler IS a member: the private source resolves.
	var asMember compileResponse
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources: []compileSourceRef{{MessageID: public.ID}, {SessionID: private.ID, MessageID: secret.ID}},
		Sender:  fixAgentA,
		Targets: []AudienceTarget{{Kind: TargetAgent, ID: fixAgentB}},
	}, &asMember, nil))
	require.Equal(t, 2, asMember.ResolvedCount)
	require.Equal(t, 0, asMember.OmittedCount)

	// (b) A compiler who is NOT a member: the same citation is a named gap,
	// and the readable source still merges.
	var asStranger compileResponse
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources: []compileSourceRef{{MessageID: public.ID}, {SessionID: private.ID, MessageID: secret.ID}},
		Sender:  fixAgentB,
		Targets: []AudienceTarget{{Kind: TargetAgent, ID: fixAgentA}},
	}, &asStranger, nil))
	require.Equal(t, 1, asStranger.ResolvedCount)
	require.Equal(t, 1, asStranger.OmittedCount)
	require.Len(t, asStranger.Gaps, 1)
	require.Contains(t, asStranger.Gaps[0], "private")
	require.NotContains(t, string(asStranger.Message.Payload), "private note",
		"an unreadable source's content must not leak into the bundle")

	// (c) Expansion runs as the CALLER: a reader who may not read the source
	// gets a named gap, a reader who may gets the original.
	strangerView := expand(t, asMember.Message.ID, nil)
	require.Equal(t, 1, strangerView.OmittedCount, "an anonymous reader may not read the private source")
	require.Equal(t, 1, strangerView.ResolvedCount)
	require.Contains(t, strangerView.Parts[1].Omitted, "private")
	memberView := expand(t, asMember.Message.ID, map[string]string{registry.HeaderAgentID: fixAgentA})
	require.Equal(t, 2, memberView.ResolvedCount, "a member of the source session resolves it")
	require.Equal(t, 0, memberView.OmittedCount)
	sameJSON(t, secret.Payload, memberView.Parts[1].Content)
}

// TestCompile_SourceInAnotherRealmIsANamedGap covers the §4 row 38 realm wall at
// the source read: the compiler cannot reach across it.
func TestCompile_SourceInAnotherRealmIsANamedGap(t *testing.T) {
	nsReg, err := namespace.NewRegistry([]namespace.Policy{
		{Name: namespace.DefaultName, Auth: namespace.AuthShared},
		{Name: "other", Auth: namespace.AuthShared},
	}, nil)
	require.NoError(t, err)

	store := NewJSONLStoreForTest(t)
	h := newAPIHarnessWith(t, store, HTTPOptions{Namespaces: nsReg})
	otherRealm := map[string]string{namespace.HeaderNamespace: "other"}

	// A room and a message in the OTHER realm.
	var otherRoom sessionView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions",
		createSessionRequest{Title: "other-realm", CreatedBy: &AuthorRef{Principal: fixHuman}}, &otherRoom, otherRealm))
	require.Equal(t, "other", namespace.Display(otherRoom.Namespace))
	var otherMsg transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+otherRoom.ID+"/messages",
		postMessageRequest{Payload: json.RawMessage(`{"text":"across the wall"}`), Sender: fixAgentA}, &otherMsg, otherRealm))

	// The target room lives in the default realm.
	room := h.createRoom(t)
	local := h.postTo(t, room, postMessageRequest{Payload: json.RawMessage(`{"text":"local"}`), Sender: fixAgentA})

	var out compileResponse
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources: []compileSourceRef{
			{MessageID: local.ID},
			{SessionID: otherRoom.ID, MessageID: otherMsg.ID},
		},
		Sender:  fixAgentA,
		Targets: []AudienceTarget{{Kind: TargetAgent, ID: fixAgentB}},
	}, &out, nil))

	require.Equal(t, 1, out.ResolvedCount)
	require.Equal(t, 1, out.OmittedCount)
	require.Len(t, out.Gaps, 1)
	require.Contains(t, out.Gaps[0], "realm")
	require.NotContains(t, string(out.Message.Payload), "across the wall",
		"a source outside the realm must not be gathered")
}

// TestCompile_RefusesTaskKindAndMalformedSources covers the compile's own guards:
// a compile never creates work (D12), a named-but-empty source is refused by
// name rather than skipped, and a closed session admits no compile.
func TestCompile_RefusesTaskKindAndMalformedSources(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	room := h.createRoom(t)
	msg := h.postTo(t, room, postMessageRequest{Payload: json.RawMessage(`{"text":"hi"}`), Sender: fixAgentA})

	var errBody map[string]string
	// D12: a compile is addressing, never an instruction.
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources:     []compileSourceRef{{MessageID: msg.ID}},
		Sender:      fixAgentA,
		MessageKind: string(MessageTask),
	}, &errBody, nil))
	require.Equal(t, "INVALID_MESSAGE_KIND", errBody["error"])
	require.Contains(t, errBody["detail"], "D12")

	// A source named with an empty message id is refused, not dropped.
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources: []compileSourceRef{{SessionID: room, MessageID: "   "}},
		Sender:  fixAgentA,
	}, &errBody, nil))
	require.Equal(t, "INVALID_REQUEST", errBody["error"])

	// No source at all is not a merge.
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sender: fixAgentA,
	}, &errBody, nil))
	require.Equal(t, "INVALID_REQUEST", errBody["error"])

	// An author is never defaulted here either.
	require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources: []compileSourceRef{{MessageID: msg.ID}},
	}, &errBody, nil))
	require.Equal(t, "AUTHOR_REQUIRED", errBody["error"])

	// A closed session admits no compile (§1.3).
	require.NoError(t, store.Append(t.Context(), CloseRecord(room, 100, time.Now(), AuthorRef{Agent: fixAgentA}, "done")))
	require.Equal(t, http.StatusConflict, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		Sources: []compileSourceRef{{MessageID: msg.ID}},
		Sender:  fixAgentA,
	}, &errBody, nil))
	require.Equal(t, "SESSION_CLOSED", errBody["error"])
}

// TestCompile_DedupesTheSameSourceNamedTwice pins the de-duplication: a source
// named in both forms is gathered once, so a merge cannot cite it twice.
func TestCompile_DedupesTheSameSourceNamedTwice(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	room := h.createRoom(t)
	msg := h.postTo(t, room, postMessageRequest{Payload: json.RawMessage(`{"text":"once"}`), Sender: fixAgentA})

	var out compileResponse
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/compile", compileRequest{
		MessageIDs: []string{msg.ID},
		Sources:    []compileSourceRef{{SessionID: room, MessageID: msg.ID}},
		Sender:     fixAgentA,
		Targets:    []AudienceTarget{{Kind: TargetAgent, ID: fixAgentB}},
	}, &out, nil))
	require.Equal(t, 1, out.PartsCount)

	bundle := decodeBundle(t, out.Message.Payload)
	require.Len(t, bundle.Parts, 1)
	require.Equal(t, msg.ID, bundle.Parts[0].SourceMessageID)

	// The thread a compile opens is not confused with a source's thread.
	require.Equal(t, out.Message.ID, out.Message.ThreadID)
	require.NotContains(t, strings.TrimSpace(out.Message.ParentID), msg.ID)
}
