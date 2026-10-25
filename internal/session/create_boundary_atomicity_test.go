package session

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// DF-CRIER-481 — the context-boundary arm of "400 but durable".
//
// DF-CRIER-471 closed the ghost-AGENT arm of POST /sessions by pre-checking the
// members' agent resolution before the room was written. The boundary arm was
// still open: addMember ran per member AFTER CreateSession, so a member whose
// context_boundary_message_id cannot resolve in the new room failed the whole
// POST while the session row stayed. The caller read 400 ("nothing happened"),
// GET /sessions showed the room with participant_count=0, and the retry was
// 409 SESSION_EXISTS for a room it had never successfully created.
//
// The record log has no delete/tombstone record — session.create,
// session.member.add, message, thread.branch, session.close, session.reopen —
// and CreateSession is Append(CreateRecord) on every backend, so a rollback is
// not expressible. The fix validates the WHOLE membership plan against the
// empty transcript of the room-not-yet-written BEFORE the first record, so the
// 400 leaves nothing behind and the retry creates exactly one room.
// ---------------------------------------------------------------------------

// TestCreateSessionRejectedBoundaryPersistsNothing reproduces the dogfood probe
// verbatim: one valid member carrying a context boundary that does not exist in
// the session being created.
func TestCreateSessionRejectedBoundaryPersistsNothing(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)

			const room = "df23-badctx"
			probe := createSessionRequest{
				ID:        room,
				CreatedBy: &AuthorRef{Principal: fixCreator},
				Members: []addParticipantRequest{{
					MemberType:               string(MemberAgent),
					MemberID:                 fixAgentB,
					ContextShare:             &ContextShare{Mode: ShareFull},
					ContextBoundaryMessageID: "nonexistent-msg-999",
				}},
			}

			var body map[string]any
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions", probe, &body, nil),
				"an unresolvable boundary is still a 400 CONTEXT_BOUNDARY_NOT_FOUND")
			require.Equal(t, "CONTEXT_BOUNDARY_NOT_FOUND", body["error"])
			require.Contains(t, body["detail"], "nonexistent-msg-999", "the 400 must name the boundary it could not resolve")
			require.NotContains(t, body, "session_id",
				"a request that persisted nothing must not report a surviving session")

			// (a) NOTHING is stored — not the session, not the member.
			_, err := store.Load(context.Background(), room)
			require.ErrorIs(t, err, ErrSessionNotFound, "a 400 must leave no durable session behind")
			ids, err := store.Sessions(context.Background())
			require.NoError(t, err)
			require.NotContains(t, ids, room)
			require.Empty(t, ids, "the rejected create wrote no record at all")

			// The retry with the SAME id creates exactly ONE room — it does not
			// collide with a ghost of the failed attempt (the 409 half of the bug).
			retry := probe
			retry.Members = []addParticipantRequest{{MemberType: string(MemberAgent), MemberID: fixAgentB}}
			var created sessionView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", retry, &created, nil),
				"the retry of a rejected create must be able to create the room")
			require.Equal(t, room, created.ID)
			require.Equal(t, 1, created.ParticipantCount)

			ids, err = store.Sessions(context.Background())
			require.NoError(t, err)
			require.Equal(t, []string{room}, ids, "exactly one room, not two")
			st, err := store.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Members, 1)
			require.Equal(t, fixAgentB, st.Members[0].MemberID)
		})
	}
}

// TestCreateSessionRejectedBoundaryLeavesNoEarlierMember pins the ordering half
// of the same defect: the probe's boundary member was ADMITTED first and the
// failure came from a later member, so a per-member write leaves a partially
// membered room. The whole plan is validated before the first record, so the
// good member of a refused create is not recorded either.
func TestCreateSessionRejectedBoundaryLeavesNoEarlierMember(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)

			const room = "df23-halfmembered"
			var body map[string]any
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
				ID:        room,
				CreatedBy: &AuthorRef{Principal: fixCreator},
				Members: []addParticipantRequest{
					// This one is entirely valid and would have been written first.
					{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleMember)},
					{MemberType: string(MemberAgent), MemberID: fixAgentB, ContextBoundaryMessageID: "nonexistent-msg-999"},
				},
			}, &body, nil))
			require.Equal(t, "CONTEXT_BOUNDARY_NOT_FOUND", body["error"])

			_, err := store.Load(context.Background(), room)
			require.ErrorIs(t, err, ErrSessionNotFound,
				"the valid member of a refused create must not survive as a memberless room")
			ids, err := store.Sessions(context.Background())
			require.NoError(t, err)
			require.Empty(t, ids)
		})
	}
}
