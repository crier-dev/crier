package permissions

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// checkerFor builds a checker over a store with a pinned clock, so grant expiry
// is deterministic.
func checkerFor(t *testing.T, s Store) *Checker {
	t.Helper()
	c := NewChecker(s)
	c.now = func() time.Time { return at(100) }
	return c
}

func mustCheck(t *testing.T, c *Checker, in CheckInput) Result {
	t.Helper()
	res, err := c.MayDeliver(context.Background(), in)
	require.NoError(t, err)
	return res
}

// humanSender is a principal speaking AS an agent — the only shape §6.3 admits.
func humanSender(principal, asAgent string) EffectiveSender {
	return EffectiveSender{Kind: SenderPrincipal, Principal: principal, AsAgent: asAgent}
}

func agentAddr(id string) Subject { return Subject{Type: SubjectAgent, Ref: id} }

// seedBindingAndGrant is the common fixture: an active principal with a speech
// binding onto an agent plus an optional grant.
func seedBindingAndGrant(t *testing.T, s Store, principal *Principal, agent string, actions []Action) {
	t.Helper()
	mustAppend(t, s, principal.Record(at(0)))
	mustAppend(t, s, (&Binding{ID: "bind_" + principal.ID, Principal: principal.ID, Agent: agent, AsAgent: true, CreatedAt: at(1)}).Record(at(1)))
	if actions != nil {
		mustAppend(t, s, (&Grant{
			ID: "grant_" + principal.ID, Principal: principal.ID,
			Subject: agentAddr(agent), Actions: actions, GrantedBy: "prin_owner", GrantedAt: at(2),
		}).Record(at(2)))
	}
}

// ---------------------------------------------------------------------------
// The deployment-level fallback (§6.4 rule 2).
// ---------------------------------------------------------------------------

func TestUnarmedCheckerAllowsEverything(t *testing.T) {
	res, err := NewChecker(nil).MayDeliver(context.Background(), CheckInput{
		Sender: EffectiveSender{Kind: SenderAnonymous}, Action: ActionSend, Subject: agentAddr("atlas"),
	})
	require.NoError(t, err)
	require.True(t, res.Allowed)
	require.Equal(t, ReasonUnarmed, res.Reason)
	require.False(t, (*Checker)(nil).Armed())
}

// TestUnclassedTargetIsLegacyPosture is §8.1's residual, stated as a test: an
// agent with no class record is reachable by anyone who can reach the port.
func TestUnclassedTargetIsLegacyPosture(t *testing.T) {
	c := checkerFor(t, newStore(t))
	res := mustCheck(t, c, CheckInput{
		Sender: EffectiveSender{Kind: SenderAnonymous}, Action: ActionSend, Subject: agentAddr("legacy"),
	})
	require.True(t, res.Allowed)
	require.Equal(t, ReasonUnclassed, res.Reason)
}

// ---------------------------------------------------------------------------
// §3.2 — personal agents: mine vs yours.
// ---------------------------------------------------------------------------

func TestPersonalOwnerAllowed(t *testing.T) {
	s := newStore(t)
	owner := activePrincipal("prin_bane", "owner", "acme")
	seedBindingAndGrant(t, s, owner, "atlas", nil)
	mustAppend(t, s, (&AgentInfo{ID: "atlas", Class: ClassPersonal, Owner: "prin_bane", Namespace: "acme"}).Record(at(3)))

	res := mustCheck(t, checkerFor(t, s), CheckInput{
		Sender: humanSender("prin_bane", "atlas"), Action: ActionSend, Subject: agentAddr("atlas"),
	})
	require.True(t, res.Allowed)
	require.Equal(t, ReasonOwner, res.Reason)
}

// TestPersonalStrangerRefusedNoGrant is THE RULE (§3.2) and the acceptance
// criterion for CR-CHAT-003, at the checker level: a principal with a binding
// but no send grant on someone else's personal agent is refused NO_GRANT.
func TestPersonalStrangerRefusedNoGrant(t *testing.T) {
	s := newStore(t)
	viewer := activePrincipal("prin_viewer", "viewer", "acme")
	// A read grant and a binding — deliberately NOT a send grant (§6.5).
	seedBindingAndGrant(t, s, viewer, "quill", []Action{ActionRead})
	mustAppend(t, s, (&AgentInfo{ID: "quill", Class: ClassPersonal, Owner: "prin_other", Namespace: "acme"}).Record(at(3)))

	c := checkerFor(t, s)
	res := mustCheck(t, c, CheckInput{
		Sender: humanSender("prin_viewer", "quill"), Action: ActionSend, Subject: agentAddr("quill"),
	})
	require.False(t, res.Allowed)
	require.Equal(t, ReasonNoGrant, res.Reason)

	// The refusal is NAMED and machine-readable.
	body := RefusalFor(CheckInput{
		Sender: humanSender("prin_viewer", "quill"), Action: ActionSend, Subject: agentAddr("quill"),
	}, res)
	require.Equal(t, ErrorDeliveryForbidden, body.Error)
	require.Equal(t, ReasonNoGrant, body.Reason)
	require.Equal(t, "prin_viewer", body.Principal)
	require.Equal(t, "quill", body.AsAgent)

	// …and the SAME principal CAN read (the read grant is live, §6.5).
	readRes := mustCheck(t, c, CheckInput{
		Sender: humanSender("prin_viewer", "quill"), Action: ActionRead, Subject: agentAddr("quill"),
	})
	require.True(t, readRes.Allowed, "§6.5: the viewer may read")
	require.Equal(t, ReasonGrant, readRes.Reason)
}

func TestPersonalGrantedSenderAllowed(t *testing.T) {
	s := newStore(t)
	p := activePrincipal("prin_ana", "member", "acme")
	seedBindingAndGrant(t, s, p, "quill", []Action{ActionSend})
	mustAppend(t, s, (&AgentInfo{ID: "quill", Class: ClassPersonal, Owner: "prin_other"}).Record(at(3)))

	res := mustCheck(t, checkerFor(t, s), CheckInput{
		Sender: humanSender("prin_ana", "quill"), Action: ActionSend, Subject: agentAddr("quill"),
	})
	require.True(t, res.Allowed)
	require.Equal(t, ReasonGrant, res.Reason)
}

// ---------------------------------------------------------------------------
// §6.6 — revocation and expiry.
// ---------------------------------------------------------------------------

func TestRevokedGrantStopsWorking(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	p := activePrincipal("prin_ana", "member", "acme")
	seedBindingAndGrant(t, s, p, "quill", []Action{ActionSend})
	mustAppend(t, s, (&AgentInfo{ID: "quill", Class: ClassPersonal, Owner: "prin_other"}).Record(at(3)))

	c := checkerFor(t, s)
	in := CheckInput{Sender: humanSender("prin_ana", "quill"), Action: ActionSend, Subject: agentAddr("quill")}
	require.True(t, mustCheck(t, c, in).Allowed)

	require.NoError(t, RevokeGrant(ctx, s, "grant_prin_ana", "prin_owner", at(50)))

	res := mustCheck(t, c, in)
	require.False(t, res.Allowed, "revocation takes effect on the next delivery (§6.6)")
	require.Equal(t, ReasonNoGrant, res.Reason)
}

func TestExpiredGrantStopsWorking(t *testing.T) {
	s := newStore(t)
	expiry := at(50)
	mustAppend(t, s, activePrincipal("prin_ana", "member", "acme").Record(at(0)))
	mustAppend(t, s, (&Binding{ID: "bind", Principal: "prin_ana", Agent: "quill", AsAgent: true, CreatedAt: at(1)}).Record(at(1)))
	mustAppend(t, s, (&Grant{
		ID: "grant_exp", Principal: "prin_ana", Subject: agentAddr("quill"),
		Actions: []Action{ActionSend}, GrantedBy: "prin_owner", GrantedAt: at(2), ExpiresAt: &expiry,
	}).Record(at(2)))
	mustAppend(t, s, (&AgentInfo{ID: "quill", Class: ClassPersonal, Owner: "prin_other"}).Record(at(3)))

	c := checkerFor(t, s) // now == at(100), after expiry
	in := CheckInput{Sender: humanSender("prin_ana", "quill"), Action: ActionSend, Subject: agentAddr("quill")}
	res := mustCheck(t, c, in)
	require.False(t, res.Allowed)
	require.Equal(t, ReasonNoGrant, res.Reason)

	// A clock before expiry still allows — the grant is inert only AFTER it.
	c.now = func() time.Time { return at(10) }
	require.True(t, mustCheck(t, c, in).Allowed)
}

// ---------------------------------------------------------------------------
// §6.3 — effective sender resolution.
// ---------------------------------------------------------------------------

func TestPrincipalWithoutBindingRefusedNoBinding(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, activePrincipal("prin_nobody", "member", "acme").Record(at(0)))
	mustAppend(t, s, (&AgentInfo{ID: "atlas", Class: ClassPersonal, Owner: "prin_nobody"}).Record(at(1)))

	res := mustCheck(t, checkerFor(t, s), CheckInput{
		Sender: humanSender("prin_nobody", "atlas"), Action: ActionSend, Subject: agentAddr("atlas"),
	})
	require.False(t, res.Allowed)
	require.Equal(t, ReasonNoBinding, res.Reason)
}

func TestAnonymousOnClassedTargetRefused(t *testing.T) {
	s := newStore(t)
	mustAppend(t, s, (&AgentInfo{ID: "atlas", Class: ClassPersonal, Owner: "prin_bane"}).Record(at(0)))

	c := checkerFor(t, s)
	res := mustCheck(t, c, CheckInput{
		Sender: EffectiveSender{Kind: SenderAnonymous}, Action: ActionSend, Subject: agentAddr("atlas"),
	})
	require.False(t, res.Allowed)
	require.Equal(t, ReasonAnonymous, res.Reason)
	body := RefusalFor(CheckInput{Sender: EffectiveSender{Kind: SenderAnonymous}, Action: ActionSend, Subject: agentAddr("atlas")}, res)
	require.Equal(t, "anonymous", body.Principal)
}

func TestSuspendedPrincipalRefused(t *testing.T) {
	s := newStore(t)
	p := activePrincipal("prin_ana", "member", "acme")
	p.Status = PrincipalSuspended
	mustAppend(t, s, p.Record(at(0)))
	mustAppend(t, s, (&Binding{ID: "bind", Principal: "prin_ana", Agent: "quill", AsAgent: true, CreatedAt: at(1)}).Record(at(1)))
	mustAppend(t, s, (&Grant{ID: "g", Principal: "prin_ana", Subject: agentAddr("quill"), Actions: []Action{ActionSend}, GrantedBy: "o", GrantedAt: at(2)}).Record(at(2)))
	mustAppend(t, s, (&AgentInfo{ID: "quill", Class: ClassPersonal, Owner: "prin_other"}).Record(at(3)))

	res := mustCheck(t, checkerFor(t, s), CheckInput{
		Sender: humanSender("prin_ana", "quill"), Action: ActionSend, Subject: agentAddr("quill"),
	})
	require.False(t, res.Allowed)
	require.Contains(t, res.Detail, "suspended")
}

// TestRemoteShadowResolvesLocally is §2.5/D14: a remote actor is a LOCAL shadow
// principal, so a local grant authorizes it and a missing shadow does not.
func TestRemoteShadowResolvesLocally(t *testing.T) {
	s := newStore(t)
	shadow := activePrincipal("prin_fed_peer_ada", "member", "acme")
	shadow.Remote = true
	shadow.RemoteInstance = "peer"
	shadow.RemoteRef = "ada"
	mustAppend(t, s, shadow.Record(at(0)))
	mustAppend(t, s, (&Binding{ID: "bind", Principal: shadow.ID, Agent: "atlas", AsAgent: true, CreatedAt: at(1)}).Record(at(1)))
	mustAppend(t, s, (&Grant{ID: "g", Principal: shadow.ID, Subject: agentAddr("atlas"), Actions: []Action{ActionSend}, GrantedBy: "o", GrantedAt: at(2)}).Record(at(2)))
	mustAppend(t, s, (&AgentInfo{ID: "atlas", Class: ClassPersonal, Owner: "prin_bane"}).Record(at(3)))

	c := checkerFor(t, s)
	res := mustCheck(t, c, CheckInput{
		Sender: EffectiveSender{Kind: SenderRemote, Principal: shadow.ID, AsAgent: "atlas"},
		Action: ActionSend, Subject: agentAddr("atlas"),
	})
	require.True(t, res.Allowed, "the local shadow's local grant authorizes it")

	// A remote actor with no local shadow is refused, principal:"remote".
	res = mustCheck(t, c, CheckInput{
		Sender: EffectiveSender{Kind: SenderRemote, Principal: "prin_fed_ghost"},
		Action: ActionSend, Subject: agentAddr("atlas"),
	})
	require.False(t, res.Allowed)
	body := RefusalFor(CheckInput{Sender: EffectiveSender{Kind: SenderRemote, Principal: "prin_fed_ghost"}}, res)
	require.Equal(t, "remote", body.Principal)
}

// ---------------------------------------------------------------------------
// §5.2 roles on pool rosters (§6.2 invoke) — the four roles demonstrated.
// ---------------------------------------------------------------------------

func TestCapabilityRolesAndGrants(t *testing.T) {
	cases := []struct {
		name   string
		role   string
		grant  []Action
		allow  bool
		reason string
	}{
		{"owner role", "owner", nil, true, ReasonRole},
		{"admin role", "admin", nil, true, ReasonRole},
		{"member role", "member", nil, true, ReasonRole},
		{"viewer role refused", "viewer", nil, false, ReasonNoGrant},
		{"viewer with explicit invoke grant", "viewer", []Action{ActionInvoke}, true, ReasonGrant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			p := activePrincipal("prin_x", tc.role, "acme")
			seedBindingAndGrant(t, s, p, "atlas", nil)
			if tc.grant != nil {
				// The grant is on the CAPABILITY subject — the pool address
				// the delivery names (§6.2 invoke × cap:y).
				mustAppend(t, s, (&Grant{
					ID: "grant_cap", Principal: p.ID,
					Subject: Subject{Type: SubjectCapability, Ref: "research"},
					Actions: tc.grant, GrantedBy: "prin_owner", GrantedAt: at(3),
				}).Record(at(3)))
			}
			res := mustCheck(t, checkerFor(t, s), CheckInput{
				Sender: humanSender("prin_x", "atlas"), Action: ActionInvoke,
				Subject: Subject{Type: SubjectCapability, Ref: "research"},
			})
			require.Equal(t, tc.allow, res.Allowed, "reason=%s detail=%s", res.Reason, res.Detail)
			if !res.Allowed {
				require.Equal(t, tc.reason, res.Reason)
			}
		})
	}
}

// TestPoolRejectsSendAction is §6.2: a team/capability is addressed with
// `invoke`, never `send`.
func TestPoolRejectsSendAction(t *testing.T) {
	s := newStore(t)
	p := activePrincipal("prin_x", "owner", "acme")
	seedBindingAndGrant(t, s, p, "atlas", nil)
	res := mustCheck(t, checkerFor(t, s), CheckInput{
		Sender: humanSender("prin_x", "atlas"), Action: ActionSend,
		Subject: Subject{Type: SubjectCapability, Ref: "research"},
	})
	require.False(t, res.Allowed)
	require.Contains(t, res.Detail, "invoke")
}

// ---------------------------------------------------------------------------
// §3.2 — service agents: scope reach, then admin/owner.
// ---------------------------------------------------------------------------

func TestServiceScopeReachAllowed(t *testing.T) {
	s := newStore(t)
	p := activePrincipal("prin_leo", "member", "acme")
	mustAppend(t, s, p.Record(at(0)))
	mustAppend(t, s, (&Binding{ID: "bind", Principal: p.ID, Agent: "deploy-bot", AsAgent: true, CreatedAt: at(1)}).Record(at(1)))
	mustAppend(t, s, (&AgentInfo{
		ID: "deploy-bot", Class: ClassService, Namespace: "acme",
		Scopes: []Scope{{ID: "scope_repo_crier", Reach: Reach{Principals: []string{"prin_leo"}}}},
	}).Record(at(2)))

	res := mustCheck(t, checkerFor(t, s), CheckInput{
		Sender: humanSender("prin_leo", "deploy-bot"), Action: ActionSend, Subject: agentAddr("deploy-bot"),
	})
	require.True(t, res.Allowed)
	require.Equal(t, ReasonScope, res.Reason)
}

func TestServiceNoScopeIsAdminOnlyInRealm(t *testing.T) {
	s := newStore(t)
	admin := activePrincipal("prin_admin", "admin", "acme")
	foreign := activePrincipal("prin_ext", "admin", "beta")
	other := activePrincipal("prin_member", "member", "acme")
	mustAppend(t, s, admin.Record(at(0)))
	mustAppend(t, s, foreign.Record(at(0)))
	mustAppend(t, s, other.Record(at(0)))
	for _, id := range []string{admin.ID, foreign.ID, other.ID} {
		mustAppend(t, s, (&Binding{ID: "b_" + id, Principal: id, Agent: "deploy-bot", AsAgent: true, CreatedAt: at(1)}).Record(at(1)))
	}
	mustAppend(t, s, (&AgentInfo{ID: "deploy-bot", Class: ClassService, Namespace: "acme"}).Record(at(2)))

	c := checkerFor(t, s)
	// admin of the SAME realm may reach a scopeless service agent (§3.2).
	res := mustCheck(t, c, CheckInput{Sender: humanSender("prin_admin", "deploy-bot"), Action: ActionSend, Subject: agentAddr("deploy-bot")})
	require.True(t, res.Allowed)
	require.Equal(t, ReasonAdmin, res.Reason)

	// An admin of another realm may NOT (T2: a role is not a reach badge).
	res = mustCheck(t, c, CheckInput{Sender: humanSender("prin_ext", "deploy-bot"), Action: ActionSend, Subject: agentAddr("deploy-bot")})
	require.False(t, res.Allowed)
	require.Equal(t, ReasonNoGrant, res.Reason)

	// A member with no scope and no grant may not.
	res = mustCheck(t, c, CheckInput{Sender: humanSender("prin_member", "deploy-bot"), Action: ActionSend, Subject: agentAddr("deploy-bot")})
	require.False(t, res.Allowed)
}

// TestAdminCannotReachAnotherOwnersPersonalAgent is T2 closed: `admin` is a
// role, not a reach badge.
func TestAdminCannotReachAnotherOwnersPersonalAgent(t *testing.T) {
	s := newStore(t)
	admin := activePrincipal("prin_admin", "admin", "acme")
	mustAppend(t, s, admin.Record(at(0)))
	mustAppend(t, s, (&Binding{ID: "b", Principal: admin.ID, Agent: "atlas", AsAgent: true, CreatedAt: at(1)}).Record(at(1)))
	mustAppend(t, s, (&AgentInfo{ID: "atlas", Class: ClassPersonal, Owner: "prin_other", Namespace: "acme"}).Record(at(2)))

	res := mustCheck(t, checkerFor(t, s), CheckInput{
		Sender: humanSender("prin_admin", "atlas"), Action: ActionSend, Subject: agentAddr("atlas"),
	})
	require.False(t, res.Allowed, "an admin may administer the realm, not reach a personal agent it does not own")
	require.Equal(t, ReasonNoGrant, res.Reason)
}
