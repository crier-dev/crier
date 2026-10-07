package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// ---------------------------------------------------------------------------
// CR-CHAT-031 — the session-level dual output read:
// GET /sessions/{id}/output?mode=trace (default) | mode=summary.
//
// House invariant under test: the trace is the RECORD (the same ordered,
// timestamped payload HandleTranscript serves); the summary is a GENERATED
// index (marked generated, `covers` naming what it covers, a `trace` link to
// the record) and is NEVER served when it does not exist — 404
// SUMMARY_UNAVAILABLE, never an empty or synthesised summary.
// ---------------------------------------------------------------------------

// postToOutput posts one text message as a participant of sid and asserts 201.
func postToOutput(t *testing.T, h *apiHarness, sid, actor, text string) {
	t.Helper()
	var resp map[string]any
	code := h.do(t, http.MethodPost, "/sessions/"+sid+"/messages", postMessageRequest{
		Payload: json.RawMessage(fmt.Sprintf(`{"text":%q}`, text)),
		Sender:  actor,
	}, &resp, nil)
	require.Equal(t, http.StatusCreated, code, "post message to %s", sid)
}

// outputFixture builds one session with two agents, posts messages into two
// threads and returns the session id.
func outputFixture(t *testing.T, h *apiHarness) string {
	t.Helper()
	sid := h.createRoom(t)
	postToOutput(t, h, sid, fixAgentA, "hello trace record 1")
	postToOutput(t, h, sid, fixAgentB, "hello trace record 2")
	return sid
}

func TestSessionOutput_TraceMode(t *testing.T) {
	for _, tc := range localBackends() {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t, tc.pathFn(t))
			h := newAPIHarness(t, store)
			sid := outputFixture(t, h)

			// mode=trace and no mode must be the SAME payload the
			// transcript read serves — the trace is the record, one path.
			var viaOutput, viaTranscript transcriptResponse
			code := h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=trace", nil, &viaOutput, nil)
			require.Equal(t, http.StatusOK, code)
			code = h.do(t, http.MethodGet, "/sessions/"+sid+"/messages", nil, &viaTranscript, nil)
			require.Equal(t, http.StatusOK, code)
			require.Equal(t, viaTranscript, viaOutput, "trace mode must delegate to HandleTranscript, not fork it")

			require.Equal(t, 2, viaOutput.Count)
			require.NotEmpty(t, viaOutput.Messages[0].ID)
			// Timestamps present and ordered — the trace carries its times.
			require.False(t, viaOutput.Messages[0].CreatedAt.IsZero())
			require.False(t, viaOutput.Messages[1].CreatedAt.IsZero())
			require.False(t, viaOutput.Messages[1].CreatedAt.Before(viaOutput.Messages[0].CreatedAt),
				"messages must come back in seq order with timestamps")
		})
	}
}

func TestSessionOutput_TraceIsDefaultMode(t *testing.T) {
	store := localBackends()[0].open(t, localBackends()[0].pathFn(t))
	h := newAPIHarness(t, store)
	sid := outputFixture(t, h)

	var bare, traced transcriptResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+sid+"/output", nil, &bare, nil))
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=trace", nil, &traced, nil))
	require.Equal(t, traced, bare, "no query param defaults to the trace")
}

func TestSessionOutput_SummaryMode(t *testing.T) {
	store := localBackends()[0].open(t, localBackends()[0].pathFn(t))
	h := newAPIHarness(t, store)
	sid := outputFixture(t, h)

	var sum sessionSummaryResponse
	code := h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=summary", nil, &sum, nil)
	require.Equal(t, http.StatusOK, code)

	// Generated, visibly: the card is an index, never the record.
	require.True(t, sum.Generated)
	require.Equal(t, sid, sum.SessionID)

	// covers names what it covers: session id, every thread id, total count.
	require.Equal(t, sid, sum.Covers.SessionID)
	require.NotEmpty(t, sum.Covers.ThreadIDs)
	require.Equal(t, 2, sum.Covers.MessageCount, "covers must count every message across the threads")

	// The trace link points at the record one read underneath.
	require.Equal(t, fmt.Sprintf("/sessions/%s/output?mode=trace", sid), sum.Trace)

	// Per-thread cards are the generated summary cards, each marked generated.
	require.Len(t, sum.Threads, len(sum.Covers.ThreadIDs))
	for _, card := range sum.Threads {
		require.True(t, card.Generated)
		require.NotEmpty(t, card.ThreadID)
	}

	// Cross-check: the per-thread summary read serves the same card.
	var threadSum threadSummaryResponse
	code = h.do(t, http.MethodGet, "/sessions/"+sid+"/threads/"+sum.Threads[0].ThreadID+"/summary", nil, &threadSum, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, threadSum.Summary, sum.Threads[0])

	// The record stays reachable through the link: a trace read behind the
	// summary still returns every message.
	var trace transcriptResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, sum.Trace, nil, &trace, nil))
	require.Equal(t, 2, trace.Count)
}

func TestSessionOutput_SummaryUnavailable(t *testing.T) {
	store := localBackends()[0].open(t, localBackends()[0].pathFn(t))
	h := newAPIHarness(t, store)
	sid := h.createRoom(t) // no messages, no threads — nothing to summarise

	code := h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=summary", nil, nil, nil)
	require.Equal(t, http.StatusNotFound, code, "an unavailable summary is a 404, never an empty or synthesised one")
}

func TestSessionOutput_InvalidMode(t *testing.T) {
	store := localBackends()[0].open(t, localBackends()[0].pathFn(t))
	h := newAPIHarness(t, store)
	sid := outputFixture(t, h)

	var resp map[string]any
	code := h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=nonsense", nil, &resp, nil)
	require.Equal(t, http.StatusBadRequest, code)
	require.Equal(t, "OUTPUT_MODE_INVALID", resp["error"])
}

// outputPrivateFixture builds one PRIVATE session with atlas as its only
// member and posts one message into it.
func outputPrivateFixture(t *testing.T, h *apiHarness) string {
	t.Helper()
	var created sessionView
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
		Title:      "secret",
		Visibility: VisibilityPrivate,
		CreatedBy:  &AuthorRef{Principal: fixCreator},
		Members:    []addParticipantRequest{{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleMember)}},
	}, &created, nil))
	require.Equal(t, VisibilityPrivate, created.Visibility)
	postToOutput(t, h, created.ID, fixAgentA, "private trace record")
	return created.ID
}

func TestSessionOutput_UnauthorizedRead(t *testing.T) {
	h := newAPIHarness(t, NewJSONLStoreForTest(t))
	sid := outputPrivateFixture(t, h)

	// A stranger (nimbus is NOT a member) is refused with 403
	// VISIBILITY_FORBIDDEN for BOTH modes — the summary must not leak
	// through a mode toggle either.
	var errBody map[string]string
	stranger := map[string]string{registry.HeaderAgentID: fixAgentB}
	require.Equal(t, http.StatusForbidden, h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=trace", nil, &errBody, stranger))
	require.Equal(t, "VISIBILITY_FORBIDDEN", errBody["error"])
	require.Equal(t, http.StatusForbidden, h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=summary", nil, &errBody, stranger))
	require.Equal(t, "VISIBILITY_FORBIDDEN", errBody["error"])

	// A member still reads both modes.
	member := map[string]string{registry.HeaderAgentID: fixAgentA}
	var trace transcriptResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=trace", nil, &trace, member))
	require.Equal(t, 1, trace.Count)
	var sum sessionSummaryResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+sid+"/output?mode=summary", nil, &sum, member))
	require.True(t, sum.Generated)
}
