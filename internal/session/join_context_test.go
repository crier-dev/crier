package session

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Late-join context share — the HTTP surface (§4.6, D10, CR-CHAT-016).
// The record layer's replay/projection battery lives in session_test.go; the
// acceptance criteria these tests verify:
//
//   a. adding a participant records a system event naming the share mode AND
//      the exact boundary (visible on the materialized view and in the log);
//   b. the joined agent's materialized view is retrievable and explainable;
//   c. changing the mode later is permitted and each change is recorded as
//      another system event (a session.member.context record, never a
//      rewrite);
//   d. the default mode is `summary` when the joiner does not choose.
// ---------------------------------------------------------------------------

var bgCtx = context.Background()

// postText posts a plain message into the room through the real API, so every
// test drives the recorded transcript the way a client would.
func postText(t *testing.T, h *apiHarness, id, sender, text string) string {
	t.Helper()
	var msg transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
		postMessageRequest{Payload: json.RawMessage(`{"text":"` + text + `"}`), Sender: sender},
		&msg, nil)
	require.Equal(t, http.StatusCreated, code, "post %q", text)
	return msg.ID
}

// getContextView fetches the member's materialized context view.
func getContextView(t *testing.T, h *apiHarness, id, mt, mid string) memberContextView {
	t.Helper()
	var view memberContextView
	code := h.do(t, http.MethodGet,
		"/sessions/"+id+"/participants/"+mt+"/"+mid+"/context", nil, &view, nil)
	require.Equal(t, http.StatusOK, code)
	return view
}

// TestSessionAPI_ContextShareDefaultSummary is (a)+(d): a join with NO
// context_share in the request is decided `summary`, and the recorded answer
// is visible — both in the membership list and in the joiner's view.
func TestSessionAPI_ContextShareDefaultSummary(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			// In-flight: two messages BEFORE the join.
			postText(t, h, id, fixAgentA, "root message")
			postText(t, h, id, fixAgentB, "reply")

			// The joiner does not choose.
			var fresh participantView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), MemberID: "nimbus-2"},
				&fresh, nil))
			// (d) the DEFAULT answer is recorded, not merely applied.
			require.NotNil(t, fresh.ContextShare, "the default summary answer must be recorded on the join event")
			require.Equal(t, ShareSummary, fresh.ContextShare.Mode)

			// (b) the materialized view explains what the joiner holds.
			view := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.True(t, view.Decided)
			require.NotNil(t, view.Share)
			require.Equal(t, ShareSummary, view.Share.Mode)
			require.Equal(t, 2, view.GivenCount)
			require.NotEmpty(t, view.GeneratedSummary, "summary mode renders a generated digest")
			require.Contains(t, view.Explanation, "GENERATED digest")
		})
	}
}

// TestSessionAPI_ContextShareModes is (a): each explicit mode is recorded
// with its exact boundary, and the materialized view matches the mode.
func TestSessionAPI_ContextShareModes(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			root := postText(t, h, id, fixAgentA, "root")
			postText(t, h, id, fixAgentB, "reply")
			postText(t, h, id, fixAgentA, "second")

			// none: recorded as a first-class answer (§4.6 rule 6), and NOT
			// confused with an undecided join.
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), MemberID: "nimbus-2",
					ContextShare: &ContextShare{Mode: ShareNone}}, &struct{}{}, nil))
			vNone := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.True(t, vNone.Decided)
			require.Equal(t, ShareNone, vNone.Share.Mode)
			require.Equal(t, 0, vNone.GivenCount)
			require.Empty(t, vNone.GeneratedSummary)
			require.Contains(t, vNone.Explanation, "NOTHING")

			// since: the boundary message id is recorded and honoured —
			// messages strictly AFTER the boundary are given.
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), MemberID: "nimbus-3",
					ContextShare: &ContextShare{Mode: ShareSince, BoundaryMessageID: root}}, &struct{}{}, nil))
			vSince := getContextView(t, h, id, string(MemberAgent), "nimbus-3")
			require.Equal(t, ShareSince, vSince.Share.Mode)
			require.Equal(t, root, vSince.Share.BoundaryMessageID, "(a) the exact boundary is named")
			require.Equal(t, 2, vSince.GivenCount)
			require.NotContains(t, vSince.GivenMessageIDs, root, "the boundary itself is withheld")
			require.Contains(t, vSince.Explanation, root)

			// full: everything before the join.
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), MemberID: "nimbus-4",
					ContextShare: &ContextShare{Mode: ShareFull}}, &struct{}{}, nil))
			vFull := getContextView(t, h, id, string(MemberAgent), "nimbus-4")
			require.Equal(t, ShareFull, vFull.Share.Mode)
			require.Equal(t, 3, vFull.GivenCount)
			require.Contains(t, vFull.Explanation, "ENTIRE history")

			// An invalid share is refused with its named error.
			var errBody map[string]string
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), MemberID: "nimbus-5",
					ContextShare: &ContextShare{Mode: "everything"}}, &errBody, nil))
			require.Equal(t, "INVALID_CONTEXT_SHARE", errBody["error"])
		})
	}
}

// TestSessionAPI_ContextShareLaterChange is (c): a later mode change is
// permitted, appended as a NEW session.member.context event (never a rewrite
// of the add), and the materialized view reflects the latest answer.
func TestSessionAPI_ContextShareLaterChange(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			root := postText(t, h, id, fixAgentA, "root")
			postText(t, h, id, fixAgentB, "reply")

			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), MemberID: "nimbus-2",
					ContextShare: &ContextShare{Mode: ShareNone}}, &struct{}{}, nil))

			// Change to `since root`, then to `full`. Each change is a new
			// recorded event.
			require.Equal(t, http.StatusOK, h.do(t, http.MethodPut,
				"/sessions/"+id+"/participants/agent/nimbus-2/context",
				setMemberContextRequest{Mode: ShareSince, BoundaryMessageID: root, Actor: fixAgentA},
				&struct{}{}, nil))
			v1 := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.Equal(t, ShareSince, v1.Share.Mode)
			require.Equal(t, root, v1.Share.BoundaryMessageID)
			require.Equal(t, 1, v1.GivenCount)
			require.Equal(t, fixAgentA, v1.SetBy)

			require.Equal(t, http.StatusOK, h.do(t, http.MethodPut,
				"/sessions/"+id+"/participants/agent/nimbus-2/context",
				setMemberContextRequest{Mode: ShareFull}, &struct{}{}, nil))
			v2 := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.Equal(t, ShareFull, v2.Share.Mode)
			require.Equal(t, 2, v2.GivenCount)

			// (c) the RECORD is the history: the log holds the add + two
			// context-change events, in that order — nothing was rewritten.
			// The JSONL store reads the log itself; the SQL stores read the
			// projection tables, so the history shape is asserted on the
			// backend that can see it (the latest-state view below is
			// asserted on every backend).
			if jl, isJSONL := store.(*JSONLStore); isJSONL {
				recs, err := jl.Records(bgCtx, id)
				require.NoError(t, err)
				var changeIdx []int
				for i, rec := range recs {
					if rec.Type == RecordMemberContext {
						changeIdx = append(changeIdx, i)
					}
				}
				require.Len(t, changeIdx, 2, "each later change is its own session.member.context event")
				require.Equal(t, RecordMemberAdd, recs[changeIdx[0]-1].Type, "the add event stays intact before the change events")
				require.Equal(t, ShareSince, recs[changeIdx[0]].ContextShare.Mode)
				require.Equal(t, ShareFull, recs[changeIdx[1]].ContextShare.Mode)
			} else {
				st2, err := store.Load(bgCtx, id)
				require.NoError(t, err)
				var shares []*MemberContext
				for _, c := range st2.ContextShares {
					if c.MemberID == "nimbus-2" {
						shares = append(shares, c)
					}
				}
				require.Len(t, shares, 1, "one latest-state row per member")
				require.Equal(t, ShareFull, shares[0].Mode, "the latest change won the projection")
			}
		})
	}
}

// TestSessionAPI_ContextShareRefusals covers the named errors: a `since`
// change naming a message that does not exist, an unknown member, and a
// closed session.
func TestSessionAPI_ContextShareRefusals(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			postText(t, h, id, fixAgentA, "root")
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), MemberID: "nimbus-2"}, &struct{}{}, nil))

			var errBody map[string]string
			// A boundary that does not exist is refused, never silently recorded.
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPut,
				"/sessions/"+id+"/participants/agent/nimbus-2/context",
				setMemberContextRequest{Mode: ShareSince, BoundaryMessageID: "no-such-message"}, &errBody, nil))
			require.Equal(t, "CONTEXT_BOUNDARY_NOT_FOUND", errBody["error"])

			// A member that was never added is a 404, distinct from a share
			// refusal.
			var view memberContextView
			require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet,
				"/sessions/"+id+"/participants/agent/ghost/context", nil, &view, nil))

			// A closed session admits no context change (§1.3).
			require.NoError(t, store.Append(bgCtx, CloseRecord(id, 100, time.Now(), AuthorRef{Agent: fixAgentA}, "done")))
			require.Equal(t, http.StatusConflict, h.do(t, http.MethodPut,
				"/sessions/"+id+"/participants/agent/nimbus-2/context",
				setMemberContextRequest{Mode: ShareFull}, &errBody, nil))
			require.Equal(t, "SESSION_CLOSED", errBody["error"])
		})
	}
}

// TestSessionAPI_ContextShareUndecidedDistinctFromNone pins §4.6 rule 6: a
// member with NO share record is reported `decided: false` — never folded
// into an explicit `none`.
func TestSessionAPI_ContextShareUndecidedDistinctFromNone(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	id := h.createRoom(t)

	// A member whose join predates the share record and was never given one:
	// appended out of band through the store, bypassing the HTTP add path.
	require.NoError(t, store.AddMember(bgCtx,
		&Member{SessionID: id, MemberType: MemberAgent, MemberID: "pre-legacy", Role: RoleMember, AddedAt: time.Now().UTC()},
		50, nil))

	view := getContextView(t, h, id, string(MemberAgent), "pre-legacy")
	require.False(t, view.Decided)
	require.Nil(t, view.Share)
	require.Contains(t, view.Explanation, "never decided")
}
