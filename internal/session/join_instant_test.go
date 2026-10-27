package session

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestMaterializedShareBoundedByJoinInstant is the derivation-level arm of
// DF-CRIER-469: the join instant is an OUTER bound of what any share mode could
// have handed this member, and a recorded boundary can only NARROW within it.
// Before the fix a summary/full share carrying a boundary took the boundary
// branch, so the join-instant guard in the else-if was never applied: an
// unresolvable boundary skipped nothing (every message counted as given) and a
// boundary later than the join admitted messages recorded after the join.
func TestMaterializedShareBoundedByJoinInstant(t *testing.T) {
	t.Parallel()
	join := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &State{Messages: []*Message{
		{ID: "pre-1", Seq: 1, CreatedAt: join.Add(-3 * time.Minute)},
		{ID: "pre-2", Seq: 2, CreatedAt: join.Add(-2 * time.Minute)},
		// Recorded AFTER the join: never given to this member, whatever the
		// recorded boundary says.
		{ID: "post-1", Seq: 3, CreatedAt: join.Add(time.Minute)},
	}}
	ids := func(given []*Message) []string {
		out := make([]string, 0, len(given))
		for _, m := range given {
			out = append(out, m.ID)
		}
		return out
	}
	cases := []struct {
		name     string
		mode     ContextShareMode
		boundary string
		want     []string
	}{
		// (AC 1) the join instant clamps every mode.
		{"summary/no boundary", ShareSummary, "", []string{"pre-1", "pre-2"}},
		{"full/no boundary", ShareFull, "", []string{"pre-1", "pre-2"}},
		{"since/no boundary", ShareSince, "", []string{"pre-1", "pre-2"}},
		// (AC 2a) an unresolvable boundary cannot widen the set: it resolves to
		// no edge, so the join instant alone bounds what counts as given.
		{"summary/unresolvable boundary", ShareSummary, "boundary-from-nowhere", []string{"pre-1", "pre-2"}},
		{"full/unresolvable boundary", ShareFull, "boundary-from-nowhere", []string{"pre-1", "pre-2"}},
		// (AC 2b) a boundary recorded AFTER the join cannot widen the set past it.
		{"summary/post-join boundary", ShareSummary, "post-1", []string{"pre-1", "pre-2"}},
		{"full/post-join boundary", ShareFull, "post-1", []string{"pre-1", "pre-2"}},
		{"since/post-join boundary", ShareSince, "post-1", []string{}},
		// The boundary still NARROWS a resolvable, pre-join edge (the positive
		// control: the fix must not leave the boundary unread).
		{"summary/pre-join boundary", ShareSummary, "pre-1", []string{"pre-1"}},
		{"since/pre-join boundary", ShareSince, "pre-1", []string{"pre-2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			given := materializedShare(st, &ContextShare{Mode: tc.mode, BoundaryMessageID: tc.boundary}, join)
			require.Equal(t, tc.want, ids(given))
			for _, g := range given {
				require.False(t, g.CreatedAt.After(join),
					"a message recorded after the join was never given by the share (%s)", g.ID)
			}
		})
	}
}

// TestMaterializedShareKeepsTheCitedEdgeAtATimestampTie pins the side of the
// join instant the clamp must NOT cross: a message stamped at the SAME instant
// as the join is not "recorded after" it, and when the share cites that message
// as its edge it must survive — the join instant narrows a boundary, it must not
// silently drop the very message the digest is anchored to.
func TestMaterializedShareKeepsTheCitedEdgeAtATimestampTie(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &State{Messages: []*Message{
		{ID: "edge", Seq: 2, CreatedAt: at},
		// Credited to the same session with a skewed clock: after the edge by
		// seq, so the boundary — not the wall clock — must drop it.
		{ID: "after", Seq: 3, CreatedAt: at.Add(-time.Hour)},
	}}
	for _, mode := range []ContextShareMode{ShareSummary, ShareFull} {
		given := materializedShare(st, &ContextShare{Mode: mode, BoundaryMessageID: "edge"}, at)
		require.Len(t, given, 1)
		require.Equal(t, "edge", given[0].ID,
			"equal timestamps cannot drop the cited edge; clock skew cannot include a later seq (%s)", mode)
	}
}

// TestSessionAPI_ContextViewNeverClaimsPostJoinMessages is (AC 3): a member
// whose RECORDED share is summary, with (a) a boundary that does not exist and
// (b) a boundary later than the join, must report a given set that holds no
// post-join message — and must not render one into the generated digest the
// view presents as history.
//
// The share is seeded straight through the store: both write paths now REFUSE an
// unresolvable boundary (DF-CRIER-472), so a record carrying one can only reach
// the read view from a log written before that refusal — exactly the record the
// defect was measured on. Seeding it here keeps the READ path honest
// independently of the WRITE path's validation.
func TestSessionAPI_ContextViewNeverClaimsPostJoinMessages(t *testing.T) {
	t.Parallel()
	const bogus = "boundary-from-nowhere"
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			pre1 := postText(t, h, id, fixAgentA, "root recorded before the join")
			pre2 := postText(t, h, id, fixAgentB, "reply recorded before the join")
			// The join itself: the default answer pins its edge to pre2.
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost,
				"/sessions/"+id+"/participants",
				addParticipantRequest{MemberType: string(MemberAgent), Actor: fixAgentA, MemberID: "nimbus-2"},
				&struct{}{}, nil))
			post := postText(t, h, id, fixAgentA, "recorded AFTER the join")

			// Baseline: the digest the join itself recorded is already honest.
			base := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.Equal(t, 2, base.GivenCount)
			require.NotContains(t, base.GivenMessageIDs, post)

			seed := func(mode ContextShareMode, boundary string) {
				t.Helper()
				setter, ok := store.(interface {
					SetMemberContext(context.Context, *MemberContext, int64) error
				})
				require.True(t, ok, "the backend records a context change")
				seq, err := store.NextSeq(bgCtx, id)
				require.NoError(t, err)
				require.NoError(t, setter.SetMemberContext(bgCtx, &MemberContext{
					SessionID: id, MemberType: MemberAgent, MemberID: "nimbus-2",
					Mode: mode, BoundaryMessageID: boundary,
					SetBy: fixAgentA, SetAt: time.Now().UTC(),
				}, seq))
			}

			// (a) a boundary that does not exist: it resolves to no edge, so it
			// cannot widen the set — and the view must SAY it could not resolve
			// the boundary it was asked to honour.
			seed(ShareSummary, bogus)
			view := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.Equal(t, 2, view.GivenCount,
				"an unresolvable boundary must not turn every message in the session into given history")
			require.ElementsMatch(t, []string{pre1, pre2}, view.GivenMessageIDs)
			require.NotContains(t, view.GivenMessageIDs, post)
			require.NotContains(t, view.GeneratedSummary, post,
				"the generated digest must never name a message recorded after the join")
			require.Equal(t, bogus, view.BoundaryUnresolved,
				"the view reports the boundary it could not resolve")
			require.Contains(t, view.Explanation, bogus,
				"the explanation names the boundary it could not honour")

			// (b) a boundary LATER than the join: it resolves, but it can only
			// narrow — it cannot admit a message the member did not exist for.
			seed(ShareSummary, post)
			view = getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.Equal(t, 2, view.GivenCount,
				"a post-join boundary cannot widen the given set past the join instant")
			require.ElementsMatch(t, []string{pre1, pre2}, view.GivenMessageIDs)
			require.NotContains(t, view.GivenMessageIDs, post)
			require.NotContains(t, view.GeneratedSummary, post)
			require.Empty(t, view.BoundaryUnresolved, "this boundary does resolve")

			// `full` under the same post-join boundary cannot widen either.
			seed(ShareFull, post)
			full := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.Equal(t, 2, full.GivenCount)
			require.NotContains(t, full.GivenMessageIDs, post)

			// A `since` boundary recorded after the join gives nothing at all.
			seed(ShareSince, post)
			since := getContextView(t, h, id, string(MemberAgent), "nimbus-2")
			require.Equal(t, 0, since.GivenCount)
			require.NotContains(t, since.GivenMessageIDs, post)
		})
	}
}
