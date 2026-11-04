// auth.go — the read-state permission gate (spec §3.1, §6).
//
// The spec names two permissions in their own vocabulary:
//
//	session.read_state.all        see every participant's row on GET /reads
//	session.read_state.write_all  mark a cursor on behalf of ANOTHER member
//
// MEASURED DEVIATION (reported in the commit body): this tree has no
// evaluation path for an arbitrary permission STRING. internal/permissions
// implements a CLOSED action set — send | read | invite | admin | mint | invoke
// (CHAT-PERMISSIONS §6.1) — and a grant carries those actions on a subject; no
// component resolves a dotted name such as `session.read_state.all`. Inventing
// a second permission system would be a second truth about who may do what.
//
// So the gate is deliberately narrow and says exactly what it consults:
//
//  1. the ACL, when it is ARMED: `admin` on the session subject. §5.2 makes
//     `admin` the action that manages a room — its membership, its grants, its
//     very existence — and read state is accounting about that room's people,
//     so an administrator is the actor the spec's "operator" means here;
//  2. a CONFIG-DRIVEN operator scope (CR_READS_OPERATORS): agent or principal
//     ids that hold both permissions by deployment policy, which is what makes
//     the surface usable in the shipped unarmed posture where no grant can
//     exist at all.
//
// Everything else is a non-privileged caller, and the handler then shows it its
// OWN row only (§3.1: "the rest are omitted, not zeroed").
package reads

import (
	"context"

	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/session"
)

// Authorizer answers the read-state permission questions. The zero value is an
// unarmed deployment with no operators: every caller is non-privileged.
type Authorizer struct {
	// Permissions is the CR-CHAT-003 delivery ACL. Nil (or unarmed) means no
	// ACL is deployed, which is the shipped trust-by-reach posture.
	Permissions *permissions.Checker
	// Operators is the config-driven operator scope: agent/principal ids that
	// hold both read-state permissions. Nil means none configured.
	Operators map[string]bool
}

// Armed reports whether ANY privilege source is configured. A caller may fall
// back to the shipped trust-by-reach posture only when nothing is armed — the
// same rule authorizeRead applies to a realm-visible session.
func (a *Authorizer) Armed() bool {
	if a == nil {
		return false
	}
	if a.Permissions != nil && a.Permissions.Armed() {
		return true
	}
	return len(a.Operators) > 0
}

// Holds reports whether a caller holds one read-state permission on a session.
//
// perm is part of the signature so the two questions stay distinguishable in
// an audit line and so a future ACL can answer them differently; today both
// resolve to the two sources above, which is stated here rather than hidden.
func (a *Authorizer) Holds(ctx context.Context, perm Permission, caller session.AuthorRef, sessionID string) (bool, error) {
	_ = perm
	if a == nil {
		return false, nil
	}
	if a.operator(caller) {
		return true, nil
	}
	if a.Permissions == nil || !a.Permissions.Armed() {
		return false, nil
	}
	res, err := a.Permissions.MayDeliver(ctx, permissions.CheckInput{
		Sender:  senderOf(caller),
		Action:  permissions.ActionAdmin,
		Subject: permissions.Subject{Type: permissions.SubjectSession, Ref: sessionID},
	})
	if err != nil {
		return false, err
	}
	return res.Allowed, nil
}

// operator reports whether the caller is in the config-driven operator scope.
func (a *Authorizer) operator(caller session.AuthorRef) bool {
	if len(a.Operators) == 0 {
		return false
	}
	for _, id := range []string{caller.Agent, caller.AsAgent, caller.Principal} {
		if id != "" && a.Operators[id] {
			return true
		}
	}
	return false
}

// senderOf maps the session author reference onto the ACL's effective sender —
// the same mapping the membership gate uses (internal/session/sender_identity.go),
// kept local because that one is unexported.
func senderOf(caller session.AuthorRef) permissions.EffectiveSender {
	switch {
	case caller.Principal != "":
		return permissions.EffectiveSender{
			Kind:      permissions.SenderPrincipal,
			Principal: caller.Principal,
			AsAgent:   caller.AsAgent,
		}
	case caller.Agent != "":
		return permissions.EffectiveSender{Kind: permissions.SenderAgent, AgentID: caller.Agent}
	case caller.AsAgent != "":
		return permissions.EffectiveSender{Kind: permissions.SenderAgent, AgentID: caller.AsAgent}
	case caller.System != "":
		return permissions.EffectiveSender{Kind: permissions.SenderAgent, AgentID: caller.System}
	}
	return permissions.EffectiveSender{Kind: permissions.SenderAnonymous}
}
