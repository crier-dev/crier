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
	t.Parallel()
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
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2"},
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
	t.Parallel()
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
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2",
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
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-3",
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
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-4",
					ContextShare: &ContextShare{Mode: ShareFull}}, &struct{}{}, nil))
			vFull := getContextView(t, h, id, string(MemberAgent), "nimbus-4")
			require.Equal(t, ShareFull, vFull.Share.Mode)
			require.Equal(t, 3, vFull.GivenCount)
			require.Contains(t, vFull.Explanation, "ENTIRE history")

			// An invalid share is refused with its named error.
			var errBody map[string]string
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-5",
					ContextShare: &ContextShare{Mode: "everything"}}, &errBody, nil))
			require.Equal(t, "INVALID_CONTEXT_SHARE", errBody["error"])
		})
	}
}

// TestSessionAPI_ContextShareLaterChange is (c): a later mode change is
// permitted, appended as a NEW session.member.context event (never a rewrite
// of the add), and the materialized view reflects the latest answer.
func TestSessionAPI_ContextShareLaterChange(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			root := postText(t, h, id, fixAgentA, "root")
			postText(t, h, id, fixAgentB, "reply")

			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2",
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
				setMemberContextRequest{Mode: ShareFull, Actor: fixAgentA}, &struct{}{}, nil))
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
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			postText(t, h, id, fixAgentA, "root")
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2"}, &struct{}{}, nil))

			var errBody map[string]string
			// A boundary that does not exist is refused, never silently recorded.
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPut,
				"/sessions/"+id+"/participants/agent/nimbus-2/context",
				setMemberContextRequest{Mode: ShareSince, BoundaryMessageID: "no-such-message"}, &errBody, nil))
			require.Equal(t, "CONTEXT_BOUNDARY_NOT_FOUND", errBody["error"])

			// A member that was never added is a 404, distinct from a share
			// refusal — and it is answered in the SAME envelope as every other
			// session 404 (a named code plus a detail naming the member), not
			// with a bare status and not with the router's "not found"
			// (DF-CRIER-480: an integrator must be able to tell a wrong
			// member from a wrong session from an unset share).
			require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet,
				"/sessions/"+id+"/participants/agent/ghost/context", nil, &errBody, nil))
			require.Equal(t, "MEMBER_NOT_FOUND", errBody["error"])
			require.Contains(t, errBody["detail"], "ghost")
			require.Contains(t, errBody["detail"], id,
				"the refusal names the session the member was looked for in")

			// …and an unknown SESSION is a DIFFERENT named 404, so the two
			// states are distinguishable from the body alone.
			require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet,
				"/sessions/no-such-room/participants/agent/nimbus-2/context", nil, &errBody, nil))
			require.Equal(t, "SESSION_NOT_FOUND", errBody["error"])
			require.Contains(t, errBody["detail"], "no-such-room")

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
	t.Parallel()
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

// TestSessionAPI_ContextShareBoundaryValidatedAllModes pins DF-CRIER-472: a
// boundary is validated on EVERY write path that can carry one, in EVERY mode.
// Before the fix the PUT path validated only `since`, so
// mode=summary boundary=boundary-from-nowhere answered 200 and the
// session.member.context event recorded the id as fact ("context summary ·
// boundary message boundary-from-nowhere") while the add path refused the very
// same id.
//
// The negative arms are PAIRED with a positive arm (a boundary that IS a
// message of the session is still accepted under summary and full), so a
// blanket refusal of the capability cannot pass this test — the fix must
// validate, not forbid.
func TestSessionAPI_ContextShareBoundaryValidatedAllModes(t *testing.T) {
	t.Parallel()
	const bogus = "boundary-from-nowhere"
	putPath := func(id string) string {
		return "/sessions/" + id + "/participants/agent/nimbus-2/context"
	}
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			postText(t, h, id, fixAgentA, "root")
			postText(t, h, id, fixAgentB, "reply")

			// The add path (http.go) is the template: capture the exact refusal
			// it returns for an unknown boundary so every PUT arm is held to the
			// SAME error, not merely the same status (AC 1).
			var addErr map[string]string
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2",
					ContextShare: &ContextShare{Mode: ShareSummary, BoundaryMessageID: bogus}},
				&addErr, nil), "the add path already refuses an unknown boundary")
			require.Equal(t, "CONTEXT_BOUNDARY_NOT_FOUND", addErr["error"])
			require.NotEmpty(t, addErr["detail"])

			// The refused add recorded nothing; add the member for real with the
			// default answer so the PUT arms have a decided member to change.
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2"},
				&struct{}{}, nil))

			// (AC 3) PUT with the unknown boundary under `summary` and `full`:
			// refused with the add path's own error.
			for _, mode := range []ContextShareMode{ShareSummary, ShareFull} {
				var putErr map[string]string
				require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPut, putPath(id),
					setMemberContextRequest{Mode: mode, BoundaryMessageID: bogus},
					&putErr, nil),
					"PUT mode=%s must refuse a boundary that is not a message of the session", mode)
				require.Equal(t, addErr["error"], putErr["error"],
					"PUT mode=%s must return the add path's error code", mode)
				require.Equal(t, addErr["detail"], putErr["detail"],
					"PUT mode=%s must return the add path's error — the two write paths of the same field agree", mode)
			}

			// (AC 2) Nothing was recorded as fact: neither the read view nor the
			// lifecycle event text (the surface the defect was PROVEN on) names
			// an unverified boundary, and the recorded answer is untouched.
			view := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.NotNil(t, view.Share)
			require.Equal(t, ShareSummary, view.Share.Mode, "a refused write must not change the recorded answer")
			require.NotEqual(t, bogus, view.Share.BoundaryMessageID)
			require.NotContains(t, view.Explanation, bogus)

			st, err := store.Load(bgCtx, id)
			require.NoError(t, err)
			for _, ev := range systemEvents(st) {
				require.NotContains(t, ev.Text, bogus,
					"lifecycle event %q must not assert an unverified boundary", ev.Text)
				if ev.ContextShare != nil {
					require.NotEqual(t, bogus, ev.ContextShare.BoundaryMessageID,
						"recorded event %q carries an unverified boundary", ev.Type)
				}
			}
		})
	}
}

// TestSessionAPI_ContextShareBoundaryUnchangedForSince is (AC 4): the `since`
// path's existing refusal is unchanged by the DF-CRIER-472 fix — still 400
// CONTEXT_BOUNDARY_NOT_FOUND, still naming the boundary and the session. The
// fix routes `since` through the same helper as the other modes, so its DETAIL
// text becomes the add path's wording ("context boundary message …"); what the
// refusal IS (status + code) is pinned here as unchanged.
func TestSessionAPI_ContextShareBoundaryUnchangedForSince(t *testing.T) {
	t.Parallel()
	const bogus = "boundary-from-nowhere"
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			postText(t, h, id, fixAgentA, "root")
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2"},
				&struct{}{}, nil))

			var sinceErr map[string]string
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPut,
				"/sessions/"+id+"/participants/agent/nimbus-2/context",
				setMemberContextRequest{Mode: ShareSince, BoundaryMessageID: bogus, Actor: fixAgentA},
				&sinceErr, nil), "the since refusal is unchanged")
			require.Equal(t, "CONTEXT_BOUNDARY_NOT_FOUND", sinceErr["error"])
			require.Contains(t, sinceErr["detail"], bogus,
				"the refusal names the boundary that was not found")
			require.Contains(t, sinceErr["detail"], id,
				"the refusal names the session it was looked for in")

			// The refusal records nothing: the member keeps its summary answer.
			view := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.Equal(t, ShareSummary, view.Share.Mode)
			require.NotEqual(t, bogus, view.Share.BoundaryMessageID)
		})
	}
}

// TestSessionAPI_ContextShareBoundaryVerifiedAccepted is the positive arm
// paired with the refusals above: a boundary that IS a message of the session
// is still accepted under summary and full and is recorded. Without this arm a
// fix that blanket-refused every boundary for those modes would go green.
func TestSessionAPI_ContextShareBoundaryVerifiedAccepted(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			root := postText(t, h, id, fixAgentA, "root")
			postText(t, h, id, fixAgentB, "reply")
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2"},
				&struct{}{}, nil))

			for _, mode := range []ContextShareMode{ShareSummary, ShareFull} {
				require.Equal(t, http.StatusOK, h.do(t, http.MethodPut,
					"/sessions/"+id+"/participants/agent/nimbus-2/context",
					setMemberContextRequest{Mode: mode, BoundaryMessageID: root, Actor: fixAgentA},
					&struct{}{}, nil), "a verified boundary must still be accepted under %s", mode)
				view := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
				require.Equal(t, mode, view.Share.Mode)
				require.Equal(t, root, view.Share.BoundaryMessageID, "the verified boundary is recorded")
				require.Equal(t, 1, view.GivenCount, "the boundary is honoured as the upper edge")
			}
		})
	}
}

// TestJoinContextGETBodies is DF-CRIER-480 at the handler level: the three
// answers this GET can give are three BODIES, and the difference between "no
// such session", "no such member" and "an empty view" is readable from the
// body alone. The wire-level twin (cmd/server/context_view_wire_test.go) proves
// the same three on the served route table; this one keeps the contract in the
// package that writes it, so a body change is caught without booting a server.
//
// The third body is the one the row was filed over: a live member whose share
// gave it nothing is a 200 carrying `given_count: 0` — never a 404 — which is
// exactly the "0 rows on the same request" the dogfood recorded.
func TestJoinContextGETBodies(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			// (1) Unknown session: a NAMED 404, and the detail names the
			// session — never a bare status or the router's "not found".
			var sessionErr map[string]string
			require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet,
				"/sessions/no-such-room/participants/agent/"+fixAgentA+"/context",
				nil, &sessionErr, nil))
			require.Equal(t, "SESSION_NOT_FOUND", sessionErr["error"])
			require.Contains(t, sessionErr["detail"], "no-such-room")

			// (2) Known session, unknown member: a DIFFERENT named 404 whose
			// detail names the member and the session it was looked for in.
			var memberErr map[string]string
			require.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet,
				"/sessions/"+id+"/participants/agent/ghost/context",
				nil, &memberErr, nil))
			require.Equal(t, "MEMBER_NOT_FOUND", memberErr["error"])
			require.Contains(t, memberErr["detail"], "ghost")
			require.Contains(t, memberErr["detail"], id)
			require.NotEqual(t, sessionErr["error"], memberErr["error"],
				"the two 404s must be distinguishable from their codes")

			// (3) Known member: 200 with the documented view. The room held no
			// messages when it was created, so every member's share is the
			// recorded `none` and the given set is empty — an EMPTY VIEW, which
			// is a 200.
			var view memberContextView
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet,
				"/sessions/"+id+"/participants/agent/"+fixAgentA+"/context",
				nil, &view, nil))
			require.True(t, view.Decided)
			require.NotNil(t, view.Share)
			require.Equal(t, ShareNone, view.Share.Mode)
			require.Equal(t, 0, view.GivenCount)
			require.NotEmpty(t, view.Explanation)
		})
	}
}
