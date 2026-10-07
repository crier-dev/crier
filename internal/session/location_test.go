package session

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// ---------------------------------------------------------------------------
// CR-CHAT-029 — agent situational awareness: deliveries carry a LOCATION and
// agents fetch the permitted context OF that location.
// ---------------------------------------------------------------------------

// TestLocation_FanoutDeliveryCarriesLocation proves part (a) through the live
// flow: a session send fans out through the shipped inbox path, and EVERY
// delivered InboxEntry carries a machine-readable location — instance,
// namespace, channel (the session), thread — populated by the delivery path,
// so the agent can SAY where it is instead of inferring it.
func TestLocation_FanoutDeliveryCarriesLocation(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root message"}),
		Sender:  fixAgentA,
	}, &root, nil))

	// The fan-out wrote one durable delivery per participant. Read them back
	// from the REAL durable inbox the harness wired (the memory registry) —
	// this is the agent's own retrieve surface, not a transcript re-read.
	for _, target := range []string{fixAgentB} {
		entries, _, err := h.reg.Retrieve(target, time.Minute, 10)
		require.NoError(t, err)
		require.NotEmpty(t, entries, "the fan-out must have delivered to %q", target)
		loc := entries[0].Location
		require.NotNil(t, loc, "a fanned-out delivery carries a location (CR-CHAT-029)")
		require.Equal(t, sessID, loc.Channel, "the channel is the session id")
		require.Equal(t, root.ThreadID, loc.ThreadID, "the thread is the message's thread root id")
		require.Equal(t, "", loc.SubThread, "a plain in-thread send names no sub-thread")
	}
}

// TestLocation_DeliverCarriesLocationOnAccept proves the same part (a) on the
// plain deliver path: a POST /agents/{id}/inbox with session/thread context
// records the location WITH the stored entry, and the accept echoes it.
func TestLocation_DeliverCarriesLocationOnAccept(t *testing.T) {
	reg := registry.NewMemoryStore()
	require.NoError(t, reg.Register(&registry.Agent{ID: fixAgentA}))

	msg := &Message{
		ID:        "m-loc-1",
		ThreadID:  "m-loc-1",
		SessionID: "sess-loc-1",
		Author:    AuthorRef{Agent: fixAgentB},
		Payload:   mustJSONRaw(map[string]any{"text": "hello"}),
		CreatedAt: time.Now().UTC(),
	}
	outcomes, err := Fanout(context.Background(), reg, msg, []string{fixAgentA}, nil)
	require.NoError(t, err)
	require.Len(t, outcomes, 1)
	require.Equal(t, OutcomeDelivered, outcomes[0].Outcome)

	entries, _, err := reg.Retrieve(fixAgentA, time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	loc := entries[0].Location
	require.NotNil(t, loc)
	require.Equal(t, "sess-loc-1", loc.Channel)
	require.Equal(t, "m-loc-1", loc.ThreadID)
	// The namespace is left to the deliver path (the target's realm is the
	// authority), and the sub-thread is absent on a non-branched send.
	require.Equal(t, "", loc.Namespace)
	require.Equal(t, "", loc.SubThread)
}

// TestLocation_SubThreadNamedOnSpawnedDelivery proves the sub-thread half of
// the location: when a send spawns a deliberately branched child thread
// (§4.5), the deliveries it fans out carry sub_thread = the child's thread id,
// and thread = the PARENT thread the child branched from.
func TestLocation_SubThreadNamedOnSpawnedDelivery(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root message"}),
		Sender:  fixAgentA,
	}, &root, nil))

	// A reply from fixAgentA addressing a NON-member ("vortex") spawns a
	// sub-thread (CR-CHAT-017); fixAgentB is named alongside it so the
	// spawned send also fans out to a participant whose inbox we can read.
	var child transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload:  mustJSONRaw(map[string]any{"text": "@vortex can you take this side question?"}),
		Sender:   fixAgentA,
		ParentID: root.ID,
		Targets:  []AudienceTarget{{Kind: TargetAgent, ID: "vortex"}, {Kind: TargetAgent, ID: fixAgentB}},
	}, &child, nil))
	require.NotNil(t, child.SpawnedThread, "addressing a non-member must spawn a sub-thread")
	parentThread := child.SpawnedThread.ParentThreadID

	entries, _, err := h.reg.Retrieve(fixAgentB, time.Minute, 10)
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	// The inbox holds BOTH sends' deliveries (the root's, then the spawned
	// child's, FIFO): the spawned send's is the last one.
	loc := entries[len(entries)-1].Location
	require.NotNil(t, loc, "the spawned send's delivery carries a location")
	require.Equal(t, sessID, loc.Channel)
	require.Equal(t, parentThread, loc.ThreadID, "the location's thread is the PARENT thread")
	require.Equal(t, child.SpawnedThread.ThreadID, loc.SubThread, "the sub-thread is the spawned child")
}

// TestLocation_FetchThreadMessagesByLocation proves part (b): a member agent
// takes its delivery's location and fetches EXACTLY the messages of that
// thread — the permitted set, in seq order — and the response echoes the
// location it asked for.
func TestLocation_FetchThreadMessagesByLocation(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root message"}),
		Sender:  fixAgentA,
	}, &root, nil))
	var reply transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload:  mustJSONRaw(map[string]any{"text": "a reply"}),
		Sender:   fixAgentB,
		ParentID: root.ID,
	}, &reply, nil))

	// fixAgentB is a member: its fetch returns exactly its thread's messages.
	var got threadMessagesResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet,
		"/sessions/"+sessID+"/threads/"+root.ThreadID+"/messages", nil, &got,
		map[string]string{registry.HeaderAgentID: fixAgentB}))
	require.Equal(t, 2, got.Count, "exactly the thread's messages, permitted set")
	require.Len(t, got.Messages, 2)
	require.Equal(t, root.ID, got.Messages[0].ID)
	require.Equal(t, reply.ID, got.Messages[1].ID)
	require.Equal(t, root.ThreadID, got.Location.Thread)
	require.Equal(t, sessID, got.Location.Channel)
}

// TestLocation_FetchSubThreadMessages proves the sub-thread path of the
// location-addressed fetch: ?sub_thread_id= returns ONLY the child thread's
// messages, never the parent's.
func TestLocation_FetchSubThreadMessages(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root message"}),
		Sender:  fixAgentA,
	}, &root, nil))
	child := branchOff(t, h, sessID, root.ID)

	var got threadMessagesResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet,
		"/sessions/"+sessID+"/threads/"+root.ThreadID+"/messages?sub_thread_id="+child.ThreadID,
		nil, &got, map[string]string{registry.HeaderAgentID: fixAgentA}))
	require.Equal(t, 1, got.Count, "only the sub-thread's own messages")
	require.Equal(t, child.ID, got.Messages[0].ID)
	require.Equal(t, root.ThreadID, got.Location.Thread)
	require.Equal(t, child.ThreadID, got.Location.SubThread)
}

// TestLocation_UnpermittedFetchIsNamedRefusal proves acceptance 3: a caller
// the session rules refuse gets 403 PERMISSION_FORBIDDEN — a NAMED error —
// and NEVER an empty list that would read as "the thread is quiet".
func TestLocation_UnpermittedFetchIsNamedRefusal(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)

	// A PRIVATE session with fixAgentA as its only member.
	var created sessionView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
		Title:      "secret",
		Visibility: VisibilityPrivate,
		CreatedBy:  &AuthorRef{Principal: fixCreator},
		Members:    []addParticipantRequest{{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleMember)}},
	}, &created, nil))

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+created.ID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "hidden root"}),
		Sender:  fixAgentA,
	}, &root, nil))

	// fixAgentB (registered in the harness but NOT a member) fetches the
	// same location its delivery would have named: named refusal.
	var errBody map[string]string
	require.Equal(t, http.StatusForbidden, h.do(t, http.MethodGet,
		"/sessions/"+created.ID+"/threads/"+root.ThreadID+"/messages", nil, &errBody,
		map[string]string{registry.HeaderAgentID: fixAgentB}))
	require.Equal(t, "PERMISSION_FORBIDDEN", errBody["error"])

	// An anonymous caller is refused the same way — the location is never
	// answered with an empty list.
	errBody = nil
	require.Equal(t, http.StatusForbidden, h.do(t, http.MethodGet,
		"/sessions/"+created.ID+"/threads/"+root.ThreadID+"/messages", nil, &errBody, nil))
	require.Equal(t, "PERMISSION_FORBIDDEN", errBody["error"])
}

// TestLocation_FetchUnknownThreadIs404Not403: an unknown thread is a miss on
// the transcript, not a permission problem — 404, a DIFFERENT answer from the
// refusal, so a caller can tell a typo from a permission wall.
func TestLocation_FetchUnknownThreadIs404Not403(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var errBody map[string]string
	require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet,
		"/sessions/"+sessID+"/threads/no-such-thread/messages", nil, &errBody,
		map[string]string{registry.HeaderAgentID: fixAgentA}))
	require.Equal(t, "THREAD_NOT_FOUND", errBody["error"])
}

// TestLocation_FetchWrongSubThreadParentIs404: a sub_thread_id that is not a
// child of the named thread is a location that does not exist as addressed.
func TestLocation_FetchWrongSubThreadParentIs404(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root message"}),
		Sender:  fixAgentA,
	}, &root, nil))

	var errBody map[string]string
	require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet,
		"/sessions/"+sessID+"/threads/"+root.ThreadID+"/messages?sub_thread_id=not-a-child",
		nil, &errBody, map[string]string{registry.HeaderAgentID: fixAgentA}))
	require.Equal(t, "THREAD_NOT_FOUND", errBody["error"])
}
