package session

// DF-CRIER-465 — a member-join system event must say WHO added the member.
//
// Measured live on ui-test-3784174: three `session.member.add` events were
// recorded, and the two joins performed afterwards through
// POST /sessions/{id}/participants carried `"actor": {}`. The transcript could
// say "agent scout joined · context summary" but not who put scout there, so an
// operator adding a member to someone else's thread left no trace of itself.
//
// The add path is covered by DF-CRIER-470 (a request that names no actor is
// refused). The remaining hole was the CREATE path: it composed each initial
// member with `creator.AgentID()`, which is EMPTY for a principal creator, so a
// principal-created room recorded `actor: {}` for every member it declared —
// silently, because nothing refused it. Both paths write through
// Handler.appendMember, so both are pinned here.

import (
	"context"
	"net/http"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
)

// memberAddRecords returns the session.member.add records of a session, in seq
// order, whatever backend produced them.
func memberAddRecords(t *testing.T, repo Repository, sessionID string) []*Record {
	t.Helper()
	recs, err := repo.Records(context.Background(), sessionID)
	require.NoError(t, err)
	var out []*Record
	for _, rec := range recs {
		if rec.Type == RecordMemberAdd {
			out = append(out, rec)
		}
	}
	return out
}

// memberAddSystemEvents returns the transcript's join rows for a session.
func memberAddSystemEvents(t *testing.T, h *apiHarness, sessionID string) []LifecycleEvent {
	t.Helper()
	var tr transcriptResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+sessionID+"/messages", nil, &tr, nil))
	var out []LifecycleEvent
	for _, ev := range tr.SystemEvents {
		if ev.Type == RecordMemberAdd {
			out = append(out, ev)
		}
	}
	return out
}

// TestSessionMemberAddActorNamesTheCreator is the create-path arm: the creator
// that declared the members performed those adds, so each session.member.add
// event must name it — including a PRINCIPAL creator, whose AgentID() is empty.
// Before this fix the record carried `actor: {}` and the transcript system row
// rendered an empty actor, which reads like a silent success.
func TestSessionMemberAddActorNamesTheCreator(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			h := newAPIHarness(t, repo)

			var created sessionView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
				Title:     "principal-created room",
				CreatedBy: &AuthorRef{Principal: fixCreator},
				Members: []addParticipantRequest{
					{MemberType: string(MemberAgent), MemberID: fixAgentA, Role: string(RoleOwner)},
					{MemberType: string(MemberPrincipal), MemberID: fixHuman, Role: string(RoleObserver)},
				},
			}, &created, nil), "create session")

			adds := memberAddRecords(t, repo, created.ID)
			require.Len(t, adds, 2, "both declared members are recorded")
			for _, rec := range adds {
				require.NotNil(t, rec.Actor, "join of %s recorded no actor", rec.MemberID)
				require.False(t, rec.Actor.IsZero(), "join of %s recorded an empty actor", rec.MemberID)
				require.Equal(t, AuthorRef{Principal: fixCreator}, *rec.Actor,
					"join of %s must name the principal that performed the add", rec.MemberID)
			}

			// The same fact through the transcript: a system event never
			// renders a join with an empty actor.
			events := memberAddSystemEvents(t, h, created.ID)
			require.Len(t, events, 2)
			for _, ev := range events {
				require.False(t, ev.Actor.IsZero(), "system event for %s has no actor", ev.MemberID)
				require.Equal(t, fixCreator, ev.Actor.Principal,
					"system event for %s must name the principal that performed the add", ev.MemberID)
			}

			// And through the audit read the store serves.
			src, ok := repo.(AuditEventSource)
			require.True(t, ok, "every session store serves the audit read")
			audit, err := src.AuditEvents(context.Background(), created.ID)
			require.NoError(t, err)
			audited := 0
			for _, ev := range audit {
				if ev.Kind != AuditMemberAdded {
					continue
				}
				audited++
				require.False(t, ev.Actor.IsZero(), "audit row for %s has no actor", ev.MemberID)
				require.Equal(t, fixCreator, ev.Actor.Principal,
					"audit row for %s must name the principal that performed the add", ev.MemberID)
			}
			require.Equal(t, 2, audited)
		})
	}
}

// TestSessionMemberAddActorNamesTheAgentCreator is the agent-creator arm: the
// same create path, with an AGENT creator, keeps naming the agent — the shape
// the live measurement showed working and which must not regress.
func TestSessionMemberAddActorNamesTheAgentCreator(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			h := newAPIHarness(t, repo)

			var created sessionView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
				Title:     "agent-created room",
				CreatedBy: &AuthorRef{Agent: fixAgentA},
				Members: []addParticipantRequest{
					{MemberType: string(MemberAgent), MemberID: fixAgentB, Role: string(RoleMember)},
				},
			}, &created, nil), "create session")

			adds := memberAddRecords(t, repo, created.ID)
			require.Len(t, adds, 1)
			require.NotNil(t, adds[0].Actor)
			require.Equal(t, AuthorRef{Agent: fixAgentA}, *adds[0].Actor)
		})
	}
}

// TestSessionMemberAddActorOnOperatorJoin is the post-create arm reached with
// the OPERATOR credential: the caller names itself with the body `actor` (or
// X-Agent-ID) because the deployment token identifies a reach level, not a
// principal — and that name, with the assurance it earned, is what the record
// keeps. An add that names NOTHING is refused and writes no line
// (TestSessionMembershipRefusesMissingActorDurably pins the refusal).
func TestSessionMemberAddActorOnOperatorJoin(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			h := newAPIHarness(t, repo)
			room := h.createRoom(t)

			// (a) the actor named in the body.
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/participants",
				map[string]any{"member_type": "agent", "member_id": "nimbus-3", "actor": fixAgentA},
				&struct{}{}, nil), "body-actor join")
			// (b) the actor named by the X-Agent-ID header — the fallback the
			// operator's browser/CLI uses.
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/participants",
				map[string]any{"member_type": "agent", "member_id": "nimbus-4"},
				&struct{}{}, map[string]string{registry.HeaderAgentID: fixAgentA}),
				"header-actor join")

			want := map[string]string{"nimbus-3": fixAgentA, "nimbus-4": fixAgentA}
			seen := 0
			for _, rec := range memberAddRecords(t, repo, room) {
				agent, ok := want[rec.MemberID]
				if !ok {
					continue
				}
				seen++
				require.NotNil(t, rec.Actor, "operator join of %s recorded no actor", rec.MemberID)
				require.Equal(t, AuthorRef{Agent: agent}, *rec.Actor,
					"operator join of %s must name the caller that performed it", rec.MemberID)
				require.Equal(t, SenderIdentityClaimed, rec.SenderIdentityBasis,
					"an operator-token caller's identity is CLAIMED, and the record says so")
			}
			require.Equal(t, 2, seen)

			events := memberAddSystemEvents(t, h, room)
			named := 0
			for _, ev := range events {
				if _, ok := want[ev.MemberID]; !ok {
					continue
				}
				named++
				require.Equal(t, AuthorRef{Agent: fixAgentA}, ev.Actor)
			}
			require.Equal(t, 2, named)
		})
	}
}

// TestSessionMemberAddActorOnVerifiedJoin is the signed-agent arm: a caller
// that presents a proof is recorded as the VERIFIED agent, and the event names
// it. It reuses the DF-CRIER-470 ingress fixture so the signed path is exercised
// the way the deployment wires it, not with a stubbed basis.
func TestSessionMemberAddActorOnVerifiedJoin(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			harness := newAPIHarness(t, repo)
			room := harness.createRoom(t)
			handler := signedMembershipHandler(t, repo, harness.reg)

			w := membershipCall(t, handler, http.MethodPost, "/sessions/"+room+"/participants",
				`{"member_type":"agent","member_id":"nimbus-3","actor":"`+fixAgentA+`"}`,
				map[string]string{
					registry.HeaderAgentID:  fixAgentA,
					registry.HeaderAgentSig: "key-of-" + fixAgentA,
				})
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

			for _, rec := range memberAddRecords(t, repo, room) {
				if rec.MemberID != "nimbus-3" {
					continue
				}
				require.NotNil(t, rec.Actor)
				require.Equal(t, AuthorRef{Agent: fixAgentA}, *rec.Actor)
				require.Equal(t, SenderIdentityVerified, rec.SenderIdentityBasis)
				return
			}
			t.Fatal("the verified join was not recorded")
		})
	}
}

// TestSessionMemberAddRefusesAnUnknownActor pins the choke point every
// membership write passes through: an add whose actor could not be established
// is REFUSED before a line is written, never stored as an empty object that
// reads like a silent success.
func TestSessionMemberAddRefusesAnUnknownActor(t *testing.T) {
	t.Parallel()
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			repo := b.open(t, b.pathFn(t))
			t.Cleanup(func() { _ = repo.Close() })
			h := newAPIHarness(t, repo)
			room := h.createRoom(t)
			before := len(memberAddRecords(t, repo, room))

			st, err := repo.Load(context.Background(), room)
			require.NoError(t, err)
			writer := NewHTTPHandler(repo, HTTPOptions{})
			err = writer.appendMember(context.Background(), st.Session, &memberPlan{
				Member: &Member{
					SessionID:  room,
					MemberType: MemberAgent,
					MemberID:   "nimbus-3",
					Role:       RoleMember,
					AddedAt:    writer.now(),
				},
				Share: &ContextShare{Mode: ShareSummary},
			})
			require.Error(t, err, "an add with no actor must be refused")
			require.Equal(t, "AUTHOR_REQUIRED", codeForMemberError(err))
			require.Len(t, memberAddRecords(t, repo, room), before, "a refused add writes nothing")
		})
	}
}
