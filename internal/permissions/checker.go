package permissions

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ErrorDeliveryForbidden is the ONE authorization code (§6.7). It is
// machine-readable and a client branches on it without parsing prose. The
// shipped server's other 403s (NAMESPACE_MISMATCH, GUARD_BLOCKED,
// AGENT_QUARANTINED) are never re-spelled.
const ErrorDeliveryForbidden = "DELIVERY_FORBIDDEN"

// Result reasons. ReasonNoGrant is the §6.7 reason a denial names when no live
// grant, ownership rule or scope reach set yielded allow; ReasonNoBinding is
// §6.7's `"reason":"NO_BINDING"`. The others describe the rule that ALLOWED a
// delivery and are carried in the audit line, not in a refusal.
const (
	ReasonAllow     = "ALLOWED"
	ReasonUnarmed   = "UNARMED"
	ReasonUnclassed = "UNCLASSED"
	ReasonOwner     = "OWNER"
	ReasonGrant     = "GRANT"
	ReasonScope     = "SCOPE"
	ReasonRole      = "ROLE"
	ReasonAdmin     = "ADMIN"

	ReasonNoGrant   = "NO_GRANT"
	ReasonNoBinding = "NO_BINDING"
	ReasonAnonymous = "ANONYMOUS"
)

// SenderKind distinguishes the four ways §6.3 resolves an effective sender.
type SenderKind string

const (
	// SenderPrincipal is a human through the UI/API: the request carried
	// (principal_id, as_agent) and the ACL requires a live binding.
	SenderPrincipal SenderKind = "principal"
	// SenderAgent is an agent's own signed call (X-Agent-ID). §6.3 maps it to
	// the agent's OWNER for a `personal` agent, else "the namespace".
	SenderAgent SenderKind = "agent"
	// SenderAnonymous is a request that named no identity. It is allowed on an
	// unclassed target (the shipped posture) and refused on a classed one.
	SenderAnonymous SenderKind = "anonymous"
	// SenderRemote is a remote participant represented LOCALLY as a shadow
	// principal (§2.5, D14). No remote grants cross the boundary.
	SenderRemote SenderKind = "remote"
)

// EffectiveSender is the sender the ACL evaluates, computed ONCE before the
// check (§6.3). It is deliberately not a bus identity: a delivery made under a
// binding is a delivery from the agent, and the principal rides alongside it.
type EffectiveSender struct {
	Kind SenderKind
	// Principal is the principal id for SenderPrincipal/SenderRemote.
	Principal string
	// AsAgent is the agent a principal speaks AS — the binding target.
	AsAgent string
	// AgentID is the calling agent's id for SenderAgent.
	AgentID string
}

// CheckInput is one authorization question: may this sender perform this
// action on this subject?
type CheckInput struct {
	Sender  EffectiveSender
	Action  Action
	Subject Subject
	// Agent, when non-nil, supplies the target agent's class/owner/scope. When
	// nil the Checker looks the class record up from its store; an absent
	// record means UNCLASSED (§8.1), so the ACL has nothing to say.
	Agent *AgentInfo
}

// Result is the outcome of one authorization question.
type Result struct {
	Allowed bool
	// Reason is the rule that decided it: an ALLOW rule name, or a refusal
	// reason (NO_GRANT / NO_BINDING / ANONYMOUS).
	Reason string
	// Detail explains the decision to a human without parsing prose. §6.7's
	// refusal body carries it verbatim.
	Detail string
}

// Refusal is the §6.7 machine-readable refusal body. Field order is the order
// the spec prints, and encoding/json preserves it.
type Refusal struct {
	Error     string  `json:"error"`
	Reason    string  `json:"reason,omitempty"`
	Principal string  `json:"principal,omitempty"`
	AsAgent   string  `json:"as_agent,omitempty"`
	Target    Subject `json:"target"`
	Action    Action  `json:"action"`
	Detail    string  `json:"detail"`
}

// RefusalFor builds the §6.7 refusal body for a denied Result.
func RefusalFor(in CheckInput, res Result) Refusal {
	return Refusal{
		Error:     ErrorDeliveryForbidden,
		Reason:    res.Reason,
		Principal: senderLabel(in.Sender),
		AsAgent:   in.Sender.AsAgent,
		Target:    in.Subject,
		Action:    in.Action,
		Detail:    res.Detail,
	}
}

func senderLabel(s EffectiveSender) string {
	switch s.Kind {
	case SenderAnonymous:
		return "anonymous"
	case SenderRemote:
		return "remote"
	case SenderAgent:
		return s.AgentID
	default:
		return s.Principal
	}
}

// Checker evaluates may_deliver against a Store. A nil *Checker is UNARMED:
// every question is allowed, which is the shipped trust-by-reach posture and
// is the deployment-level fallback §6.4 rule 2 fixes.
type Checker struct {
	store Store
	now   func() time.Time
}

// NewChecker builds a Checker over a store. A nil store yields an unarmed
// checker.
func NewChecker(store Store) *Checker {
	return &Checker{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// Armed reports whether the checker will evaluate anything. A nil checker or
// one with no store is not armed.
func (c *Checker) Armed() bool { return c != nil && c.store != nil }

// MayDeliver answers one authorization question. It reads a FRESH snapshot per
// call, which is §6.6's per-request evaluation: a grant revoked a moment ago
// is absent on the next delivery, never cached.
func (c *Checker) MayDeliver(ctx context.Context, in CheckInput) (Result, error) {
	if !c.Armed() {
		return Result{Allowed: true, Reason: ReasonUnarmed, Detail: "no permission store is wired; the ACL is not deployed"}, nil
	}
	snap, err := c.store.Snapshot(ctx)
	if err != nil {
		return Result{}, err
	}
	return c.evaluate(snap, in, c.now()), nil
}

// evaluate runs the §3.2 rule against a snapshot. It never returns an error:
// every store read has already happened.
func (c *Checker) evaluate(snap *Snapshot, in CheckInput, now time.Time) Result {
	// §6.3 — an anonymous sender is allowed on an unclassed target and refused
	// on a classed one. The unclassed exception is evaluated inside the agent
	// branch below (it needs the row); for every other subject type an
	// anonymous sender reaches nothing.
	if in.Subject.Type == SubjectAgent {
		ag := in.Agent
		if ag == nil {
			ag = snap.Agent(in.Subject.Ref)
		}
		if ag == nil || ag.Class == "" {
			// §8.1 residual: an unclassed row keeps the shipped posture.
			return Result{Allowed: true, Reason: ReasonUnclassed,
				Detail: fmt.Sprintf("agent %q is unclassed; the ACL has no class to evaluate (§8.1)", in.Subject.Ref)}
		}
		return c.evaluateAgent(snap, in, ag, now)
	}

	p, bindRes, ok := c.resolvePrincipal(snap, in)
	if !ok {
		return bindRes
	}

	switch in.Subject.Type {
	case SubjectCapability, SubjectGroup:
		// §6.2: a pool/roster is addressed with `invoke`, never `send`.
		if in.Action != ActionInvoke {
			return Result{Reason: ReasonNoGrant,
				Detail: fmt.Sprintf("subject %s %q requires the %s action, not %s (§6.2)", in.Subject.Type, in.Subject.Ref, ActionInvoke, in.Action)}
		}
		if p != nil && RoleAllows(p.Role, ActionInvoke) {
			return Result{Allowed: true, Reason: ReasonRole,
				Detail: fmt.Sprintf("principal %s holds role %s, whose bundle includes %s (§5.2)", p.ID, p.Role, ActionInvoke)}
		}
		if p != nil {
			if g := snap.LiveGrantFor(p.ID, in.Subject, in.Action, now); g != nil {
				return Result{Allowed: true, Reason: ReasonGrant,
					Detail: fmt.Sprintf("live grant %s confers %s on %s %q", g.ID, in.Action, in.Subject.Type, in.Subject.Ref)}
			}
		}
		return Result{Reason: ReasonNoGrant,
			Detail: fmt.Sprintf("no live grant for principal %s on %s %q with action %s", senderLabel(in.Sender), in.Subject.Type, in.Subject.Ref, in.Action)}
	case SubjectSession:
		if p != nil && RoleAllows(p.Role, in.Action) {
			return Result{Allowed: true, Reason: ReasonRole,
				Detail: fmt.Sprintf("principal %s holds role %s, whose bundle includes %s (§5.2)", p.ID, p.Role, in.Action)}
		}
		if p != nil {
			if g := snap.LiveGrantFor(p.ID, in.Subject, in.Action, now); g != nil {
				return Result{Allowed: true, Reason: ReasonGrant,
					Detail: fmt.Sprintf("live grant %s confers %s on session %q", g.ID, in.Action, in.Subject.Ref)}
			}
		}
		return Result{Reason: ReasonNoGrant,
			Detail: fmt.Sprintf("no live grant for principal %s on session %q with action %s", senderLabel(in.Sender), in.Subject.Ref, in.Action)}
	case SubjectNamespace:
		if in.Action != ActionRead {
			return Result{Reason: ReasonNoGrant,
				Detail: fmt.Sprintf("a namespace subject admits %s only (§6.2)", ActionRead)}
		}
		if p != nil && RoleAllows(p.Role, ActionRead) && sameRealm(p.Namespace, in.Subject.Ref) {
			return Result{Allowed: true, Reason: ReasonRole,
				Detail: fmt.Sprintf("principal %s holds %s in realm %q", p.ID, ActionRead, in.Subject.Ref)}
		}
		return Result{Reason: ReasonNoGrant,
			Detail: fmt.Sprintf("no %s right for principal %s in realm %q", ActionRead, senderLabel(in.Sender), in.Subject.Ref)}
	}
	return Result{Reason: ReasonNoGrant,
		Detail: fmt.Sprintf("unknown subject type %q", in.Subject.Type)}
}

// resolvePrincipal validates the sender's identity for a non-agent subject and
// for the agent branch's principal-side rules. It returns (principal, refusal,
// ok): ok=false means the question is already answered by the refusal.
func (c *Checker) resolvePrincipal(snap *Snapshot, in CheckInput) (*Principal, Result, bool) {
	s := in.Sender
	switch s.Kind {
	case SenderAnonymous:
		return nil, Result{Reason: ReasonAnonymous,
			Detail: "an anonymous sender carries no identity; delivery is refused on any subject the ACL is armed for (§2.4, §6.7)"}, false
	case SenderRemote:
		// §2.5: a remote participant resolves to its LOCAL shadow principal.
		// Grants stay local; a missing shadow is refused with principal:"remote".
		p := snap.Principal(s.Principal)
		if p == nil || !p.Active() {
			return nil, Result{Reason: ReasonNoGrant,
				Detail: fmt.Sprintf("no local shadow principal %q for this remote actor (§2.5)", s.Principal)}, false
		}
		return p, Result{}, true
	case SenderAgent:
		// §6.3/§8.2 Q2 PROPOSED-DEFAULT: an agent's own call maps to its
		// OWNER for a `personal` agent. A service or unclassed sender has no
		// principal identity and is treated like an anonymous caller.
		info := snap.Agent(s.AgentID)
		if info != nil && info.Class == ClassPersonal && info.Owner != "" {
			p := snap.Principal(info.Owner)
			if p != nil && p.Active() {
				return p, Result{}, true
			}
		}
		return nil, Result{Reason: ReasonNoGrant,
			Detail: fmt.Sprintf("calling agent %q has no owner principal to act as (§6.3, §8.2 Q2)", s.AgentID)}, false
	case SenderPrincipal:
		if strings.TrimSpace(s.Principal) == "" {
			return nil, Result{Reason: ReasonAnonymous,
				Detail: "principal_id was empty; the request carries no identity"}, false
		}
		p := snap.Principal(s.Principal)
		if p == nil {
			return nil, Result{Reason: ReasonNoGrant,
				Detail: fmt.Sprintf("principal %q is not known to this deployment", s.Principal)}, false
		}
		if !p.Active() {
			return nil, Result{Reason: ReasonNoGrant,
				Detail: fmt.Sprintf("principal %q is %s; grants do not apply and delivery is refused (§2.2)", p.ID, p.Status)}, false
		}
		if strings.TrimSpace(s.AsAgent) == "" {
			return nil, Result{Reason: ReasonNoBinding,
				Detail: fmt.Sprintf("principal %q carried no as_agent; a write is principal_id + as_agent or neither (§2.4)", p.ID)}, false
		}
		b := snap.Binding(p.ID, s.AsAgent)
		if b == nil || !b.AsAgent {
			return nil, Result{Reason: ReasonNoBinding,
				Detail: fmt.Sprintf("principal %q holds no live binding to agent %q (§6.3, §6.7 NO_BINDING)", p.ID, s.AsAgent)}, false
		}
		return p, Result{}, true
	}
	return nil, Result{Reason: ReasonAnonymous, Detail: "unrecognised sender kind"}, false
}

// evaluateAgent is §3.2's rule for a CLASSED agent target.
func (c *Checker) evaluateAgent(snap *Snapshot, in CheckInput, ag *AgentInfo, now time.Time) Result {
	p, bindRes, ok := c.resolvePrincipal(snap, in)
	if !ok {
		return bindRes
	}
	principalID := ""
	if p != nil {
		principalID = p.ID
	}

	switch ag.Class {
	case ClassPersonal:
		if principalID != "" && principalID == ag.Owner {
			return Result{Allowed: true, Reason: ReasonOwner,
				Detail: fmt.Sprintf("principal %s owns personal agent %s (§3.2)", principalID, ag.ID)}
		}
		if principalID != "" {
			if g := snap.LiveGrantFor(principalID, in.Subject, in.Action, now); g != nil {
				return Result{Allowed: true, Reason: ReasonGrant,
					Detail: fmt.Sprintf("live grant %s confers %s on agent %s", g.ID, in.Action, ag.ID)}
			}
		}
		return Result{Reason: ReasonNoGrant,
			Detail: fmt.Sprintf("no live grant for principal %s on agent %s (class personal, owner %s)",
				senderLabel(in.Sender), ag.ID, ag.Owner)}
	case ClassService:
		if principalID != "" {
			if g := snap.LiveGrantFor(principalID, in.Subject, in.Action, now); g != nil {
				return Result{Allowed: true, Reason: ReasonGrant,
					Detail: fmt.Sprintf("live grant %s confers %s on service agent %s", g.ID, in.Action, ag.ID)}
			}
			if ag.reachableByPrincipal(principalID) {
				return Result{Allowed: true, Reason: ReasonScope,
					Detail: fmt.Sprintf("principal %s is in a scope reach set of service agent %s (§3.2, §4.1)", principalID, ag.ID)}
			}
		}
		// A service agent with no declared scope and no grant is reachable by
		// admin/owner only (§3.2 consequence 2). The role is a namespace role:
		// it grants no reach outside its own realm (T2 closed).
		if p != nil && (p.Role == RoleAdmin || p.Role == RoleOwner) && sameRealm(p.Namespace, ag.Namespace) {
			return Result{Allowed: true, Reason: ReasonAdmin,
				Detail: fmt.Sprintf("principal %s holds the %s role in realm %q and administers service agent %s (§3.2)", p.ID, p.Role, p.Namespace, ag.ID)}
		}
		return Result{Reason: ReasonNoGrant,
			Detail: fmt.Sprintf("no live grant and no scope reach for principal %s on service agent %s (no scope, no grant: admin/owner only)",
				senderLabel(in.Sender), ag.ID)}
	}
	// A validated AgentInfo cannot reach here, but an unvalidated one could.
	return Result{Reason: ReasonNoGrant,
		Detail: fmt.Sprintf("agent %q has an unknown class %q", ag.ID, ag.Class)}
}
