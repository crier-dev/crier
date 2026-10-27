package session

// readd_participant_test.go — DF-CRIER-482: POST /sessions/{id}/participants
// for a member that is ALREADY an active participant used to answer 201 and
// silently replace that member's stored context share with the JOIN DEFAULT
// (`summary` + the newest boundary), because the add path composed a fresh
// member plan and appended a second `session.member.add` — which re-applies
// whatever share the plan carries, default included.
//
// Reported by the dogfood finding (5/5 re-adds each mutated state): a member
// joined with `since`+B1, and a plain retry with NO `context_share` rewrote the
// join-time grant. That is a silent mutation of a grant the request never asked
// to change, and the audit trail showed a SECOND member.added for the same
// member, indistinguishable from a first join.
//
// The contract these tests pin (specs/CHAT-SESSIONS.md §2.3 consequence 2 — "a
// duplicate add for the same member and role is a no-op, not a second row";
// §2.3's `session.member.context` — "a LATER change to a member's
// context_share ... a new event, never a rewrite"):
//
//   - a re-add of an ACTIVE member that names no share is a NO-OP: nothing is
//     appended, the stored share is untouched, and the response says so
//     (`changed: false`, `action: "unchanged"`, 200);
//   - a re-add that DOES name a share records a `session.member.context` event
//     (audit kind `member.context`, never a second `member.added`) and states
//     the previous share in the response;
//   - a genuinely new participant is unchanged: 201, `action: "added"`, the
//     default share still recorded (§4.6 D10);
//   - re-adding a REMOVED member is still a NEW event (§2.4 rule 5).
//
// Backend scope: the RECORD-level counts come from the append log, which only
// the JSONL backend can read back as history — the SQL stores project LATEST
// STATE and, as audit.go states, retain neither a re-add nor a
// `member.context` history. The view-level facts (stored share, audit read,
// response body) are asserted on every local backend.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// readdParticipantResult is the WIRE shape these tests assert: the recorded
// participant (participantView) plus what THIS request did to it. It is
// declared here, not taken from the handler's own struct, so the test pins the
// JSON contract rather than a Go type that could drift with it.
type readdParticipantResult struct {
	participantView
	Changed              bool          `json:"changed"`
	Action               string        `json:"action"`
	ContextSharePrevious *ContextShare `json:"context_share_previous"`
}

// auditCountsFor counts the audit events the AUDIT READ SURFACE serves for one
// member id, keyed by kind. The read surface is the operator's view, so it is
// the right oracle for "can an operator tell a re-add from a join".
func auditCountsFor(t *testing.T, h *apiHarness, id, memberID string) map[AuditEventKind]int {
	t.Helper()
	var trail AuditTrail
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+id+"/audit", nil, &trail, nil))
	counts := map[AuditEventKind]int{}
	for _, ev := range trail.Events {
		if ev.MemberID == memberID {
			counts[ev.Kind]++
		}
	}
	return counts
}

// logRecordCounts counts the session.member.add and session.member.context
// RECORDS the append log holds for one member. ok is false on a backend that
// cannot answer it: the SQL stores project latest state and keep no such
// history (see the file comment), so asserting through them would be a
// vacuous green rather than a fact about the log.
func logRecordCounts(t *testing.T, store Repository, id, memberID string) (adds, contexts int, ok bool) {
	t.Helper()
	jl, isJSONL := store.(*JSONLStore)
	if !isJSONL {
		return 0, 0, false
	}
	recs, err := jl.Records(context.Background(), id)
	require.NoError(t, err)
	for _, rec := range recs {
		if rec.MemberID != memberID {
			continue
		}
		switch rec.Type {
		case RecordMemberAdd:
			adds++
		case RecordMemberContext:
			contexts++
		}
	}
	return adds, contexts, true
}

// TestReAddActiveParticipantIsNoOp is the DF-CRIER-482 regression: a member
// joined with `since`+B1, then re-added with NO context_share, keeps B1.
func TestReAddActiveParticipantIsNoOp(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			b1 := postText(t, h, id, fixAgentA, "boundary one")
			postText(t, h, id, fixAgentB, "a later message the default would cite")

			const peer = "nimbus-3"
			var joined readdParticipantResult
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				map[string]any{
					"member_type": "agent",
					"member_id":   peer,
					"actor":       fixAgentA,
					"context_share": map[string]any{
						"mode":                string(ShareSince),
						"boundary_message_id": b1,
					},
				}, &joined, nil))
			require.NotNil(t, joined.ContextShare)
			require.Equal(t, ShareSince, joined.ContextShare.Mode)
			require.Equal(t, b1, joined.ContextShare.BoundaryMessageID)
			joinedAt := joined.AddedAt

			// The natural idempotent retry a client sends: same member, no
			// share named. The finding measured 5/5 such calls each mutating
			// state, so all five are exercised here. The SHARE is asserted
			// first: it is the grant this defect silently replaced.
			for i := 0; i < 5; i++ {
				var again readdParticipantResult
				code := h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
					map[string]any{"member_type": "agent", "member_id": peer, "actor": fixAgentB},
					&again, nil)
				require.NotNil(t, again.ContextShare,
					"re-add %d: the response is the recorded membership, so it carries the share that is actually stored", i+1)
				require.Equal(t, ShareSince, again.ContextShare.Mode,
					"re-add %d must not replace the join-time grant with the join default", i+1)
				require.Equal(t, b1, again.ContextShare.BoundaryMessageID,
					"re-add %d must not move the boundary to the newest message", i+1)
				require.True(t, joinedAt.Equal(again.AddedAt),
					"re-add %d must not move added_at: got %s want %s", i+1, again.AddedAt, joinedAt)
				require.Equal(t, http.StatusOK, code, "re-add %d is an acknowledged no-op", i+1)
				require.False(t, again.Changed, "re-add %d changed nothing", i+1)
				require.Equal(t, "unchanged", again.Action)
			}

			// A re-add does not change the role in place: §2.3 makes a role
			// change a remove+add pair, so the recorded role is reported back
			// rather than silently rewritten (and the grant is still B1).
			var withRole readdParticipantResult
			roleCode := h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				map[string]any{"member_type": "agent", "member_id": peer, "role": string(RoleObserver), "actor": fixAgentB},
				&withRole, nil)
			require.Equal(t, http.StatusOK, roleCode)
			require.False(t, withRole.Changed)
			require.Equal(t, string(RoleMember), withRole.Role,
				"the RECORDED role is reported: a re-add is not an in-place role edit")
			require.NotNil(t, withRole.ContextShare)
			require.Equal(t, b1, withRole.ContextShare.BoundaryMessageID)

			// The STORED share — what the member's context view serves — still
			// includes B1.
			st, err := store.Load(context.Background(), id)
			require.NoError(t, err)
			mc := contextShareFor(st, MemberAgent, peer)
			require.NotNil(t, mc)
			require.Equal(t, ShareSince, mc.Mode, "the stored share must survive the re-adds")
			require.Equal(t, b1, mc.BoundaryMessageID)

			// The record: ONE membership event for this member and no
			// share-change event — a no-op leaves no second join in the log.
			adds, contexts, ok := logRecordCounts(t, store, id, peer)
			if ok {
				require.Equal(t, 1, adds, "a no-op re-add must not append a second session.member.add")
				require.Zero(t, contexts, "a no-op re-add must not record a share change either")
			}

			// The same fact through the AUDIT READ: one member.added, no
			// member.context, so an operator does not see the repeated add.
			counts := auditCountsFor(t, h, id, peer)
			require.Equal(t, 1, counts[AuditMemberAdded],
				"the audit trail must not carry a second member.added for a no-op re-add")
			require.Zero(t, counts[AuditMemberContext],
				"a no-op re-add must not show up as a grant change")

			// The FIRST join is a change and says so; asserted last so a
			// failure above reports the share defect rather than the shape.
			require.Equal(t, "added", joined.Action)
			require.True(t, joined.Changed, "a first join is a change")
		})
	}
}

// TestReAddWithExplicitShareChangeIsRecorded is the other half of AC2: an
// EXPLICIT share change on a re-add is still applied, but it is recorded as a
// `session.member.context` event — never as a second `member.added` — and the
// response names the share it replaced.
func TestReAddWithExplicitShareChangeIsRecorded(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			b1 := postText(t, h, id, fixAgentA, "boundary one")
			postText(t, h, id, fixAgentB, "second")

			const peer = "nimbus-4"
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				map[string]any{"member_type": "agent", "member_id": peer, "actor": fixAgentA,
					"context_share": map[string]any{"mode": string(ShareSince), "boundary_message_id": b1}},
				&struct{}{}, nil))

			var changed readdParticipantResult
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				map[string]any{"member_type": "agent", "member_id": peer, "actor": fixAgentB,
					"context_share": string(ShareFull)}, &changed, nil))

			// The explicit change IS applied (behaviour preserved)...
			st, err := store.Load(context.Background(), id)
			require.NoError(t, err)
			mc := contextShareFor(st, MemberAgent, peer)
			require.NotNil(t, mc)
			require.Equal(t, ShareFull, mc.Mode)

			// ...and it is distinguishable in the record: the membership event
			// stays ONE session.member.add, and the share change is its own
			// session.member.context record — never a second join.
			adds, contexts, ok := logRecordCounts(t, store, id, peer)
			if ok {
				require.Equal(t, 1, adds, "a re-add is not a second join: the log must not carry two member.add records")
				require.Equal(t, 1, contexts, "the explicit change is recorded as a session.member.context event")
				var reason string
				recs, err := store.(*JSONLStore).Records(context.Background(), id)
				require.NoError(t, err)
				for _, rec := range recs {
					if rec.Type == RecordMemberContext && rec.MemberID == peer {
						reason = rec.Reason
					}
				}
				require.Contains(t, reason, "re-add",
					"the audit row says a re-add replaced the grant, not that a member joined")
				require.Contains(t, reason, string(ShareSince),
					"the audit row names the mode it replaced")
			}
			require.Equal(t, 1, auditCountsFor(t, h, id, peer)[AuditMemberAdded],
				"the audit read must not show two joins for one member")

			// The response names what it did and what it replaced, so the
			// overwrite is never silent.
			require.True(t, changed.Changed)
			require.Equal(t, "context_share_updated", changed.Action)
			require.NotNil(t, changed.ContextShare)
			require.Equal(t, ShareFull, changed.ContextShare.Mode)
			require.NotNil(t, changed.ContextSharePrevious,
				"an applied re-add must name the share it replaced")
			require.Equal(t, ShareSince, changed.ContextSharePrevious.Mode)
			require.Equal(t, b1, changed.ContextSharePrevious.BoundaryMessageID)

			// Repeating the SAME explicit share is idempotent: nothing to
			// record a second time.
			var same readdParticipantResult
			code := h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				map[string]any{"member_type": "agent", "member_id": peer, "actor": fixAgentA,
					"context_share": string(ShareFull)}, &same, nil)
			if againAdds, againContexts, ok := logRecordCounts(t, store, id, peer); ok {
				require.Equal(t, adds, againAdds)
				require.Equal(t, contexts, againContexts,
					"an unchanged explicit share records nothing")
			}
			require.Equal(t, http.StatusOK, code)
			require.False(t, same.Changed)
			require.Equal(t, "unchanged", same.Action)
			require.NotNil(t, same.ContextShare)
			require.Equal(t, ShareFull, same.ContextShare.Mode)
		})
	}
}

// TestAddParticipantNewAndRemovedStayNewEvents pins the behaviour that must NOT
// change: a genuinely new participant is a 201 join with the §4.6 D10 default,
// and re-adding a REMOVED member is a NEW event (§2.4 rule 5).
func TestAddParticipantNewAndRemovedStayNewEvents(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)

			edge := postText(t, h, id, fixAgentA, "the newest message")

			// A genuinely new participant: still 201, still "added", and the
			// default share is still recorded (§4.6 D10).
			const peer = "nimbus-2"
			var added readdParticipantResult
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				map[string]any{"member_type": "agent", "member_id": peer, "actor": fixAgentA},
				&added, nil))
			require.NotNil(t, added.ContextShare)
			require.Equal(t, ShareSummary, added.ContextShare.Mode)
			require.Equal(t, edge, added.ContextShare.BoundaryMessageID)
			require.Equal(t, "added", added.Action)
			require.True(t, added.Changed)

			// Remove it, then re-add: §2.4 rule 5 makes the re-add a NEW
			// membership event, not a no-op.
			seq, err := store.NextSeq(context.Background(), id)
			require.NoError(t, err)
			require.NoError(t, store.Append(context.Background(), &Record{
				V:          RecordFormatVersion,
				Type:       RecordMemberRemove,
				SessionID:  id,
				Seq:        seq,
				TS:         time.Now().UTC(),
				MemberType: MemberAgent,
				MemberID:   peer,
				Actor:      &AuthorRef{Agent: fixAgentA},
				Reason:     "left the room",
			}))

			var rejoined readdParticipantResult
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				map[string]any{"member_type": "agent", "member_id": peer, "actor": fixAgentA},
				&rejoined, nil))
			require.True(t, rejoined.Active)
			require.Equal(t, "added", rejoined.Action)
			require.True(t, rejoined.Changed)

			if adds, _, ok := logRecordCounts(t, store, id, peer); ok {
				require.Equal(t, 2, adds,
					"a join/leave/join sequence keeps both joins in the log")
			}
			require.GreaterOrEqual(t, auditCountsFor(t, h, id, peer)[AuditMemberAdded], 1,
				"the rejoined member is in the audit trail")
		})
	}
}
