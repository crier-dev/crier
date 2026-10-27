package session

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// DF-CRIER-471 — the invalid-member-CONFIG arm of "400 but durable".
//
// The row's premise was that POST /sessions pre-checked only checkMemberAgent,
// then wrote the session and the creator, then added members one by one, so a
// LATER member failing validation returned 400 while the session and the
// earlier members stayed durable. That write order is gone: the create path now
// composes and validates the WHOLE membership plan (composeMember: member type,
// member id, context-share SHAPE, agent admission, remote policy — plus
// checkMemberBoundary against the empty transcript) before its first record
// (DF-CRIER-481, http.go:652-680). A refused create therefore leaves no session
// and no members, and a retry with the same id creates exactly one room instead
// of colliding with a durable ghost of the failed attempt.
//
// The boundary arm is pinned by create_boundary_atomicity_test.go. This file
// pins the arm this row's acceptance criteria name: the SECOND member carries an
// invalid CONTEXT SHARE (a mode outside the §4.6 vocabulary), which is rejected
// by a validator the old pre-check never ran at all.
// ---------------------------------------------------------------------------

// TestCreateSessionSecondMemberInvalidSharePersistsNothing is DF-CRIER-471's
// acceptance probe: a create whose first member is entirely valid and whose
// second member carries an invalid context share must store nothing at all.
func TestCreateSessionSecondMemberInvalidSharePersistsNothing(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)

			const room = "df471-badshare"
			probe := createSessionRequest{
				ID:        room,
				CreatedBy: &AuthorRef{Principal: fixCreator},
				Members: []addParticipantRequest{
					// Valid in every respect: this one used to be persisted
					// before the second member was ever looked at.
					{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleMember)},
					// Invalid CONFIG: "everything" is not a §4.6 share mode.
					{MemberType: string(MemberAgent), MemberID: fixAgentB,
						ContextShare: &ContextShare{Mode: "everything"}},
				},
			}

			var body map[string]any
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions", probe, &body, nil),
				"a member with an invalid context share is a 400")
			require.Equal(t, "INVALID_CONTEXT_SHARE", body["error"])
			detail, _ := body["detail"].(string)
			require.Contains(t, detail, "everything",
				"the 400 must name the share mode it could not accept")
			require.NotContains(t, body, "session_id",
				"a request that persisted nothing must not report a surviving session")
			require.NotContains(t, body, "partial",
				"nothing was partially created, so no partial envelope applies")

			// (a) NOTHING is stored — not the session, not the valid first member.
			_, err := store.Load(context.Background(), room)
			require.ErrorIs(t, err, ErrSessionNotFound, "a 400 must leave no durable session behind")
			ids, err := store.Sessions(context.Background())
			require.NoError(t, err)
			require.NotContains(t, ids, room)
			require.Empty(t, ids, "the rejected create wrote no record at all")

			// The retry with the SAME id creates exactly ONE room with both
			// members — it does not collide with a ghost of the failed attempt.
			retry := probe
			retry.Members = []addParticipantRequest{
				{MemberType: string(MemberAgent), MemberID: fixAgentA},
				{MemberType: string(MemberAgent), MemberID: fixAgentB,
					ContextShare: &ContextShare{Mode: ShareSummary}},
			}
			var created sessionView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", retry, &created, nil),
				"the retry of a rejected create must be able to create the room")
			require.Equal(t, room, created.ID)
			require.Equal(t, 2, created.ParticipantCount)

			ids, err = store.Sessions(context.Background())
			require.NoError(t, err)
			require.Equal(t, []string{room}, ids, "exactly one room, not two")
			st, err := store.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Members, 2, "both members of the accepted create are recorded")
		})
	}
}
