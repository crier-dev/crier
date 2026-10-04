package registry

import (
	"log/slog"
	"net/http"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/httperr"
	"github.com/crier-dev/crier/internal/permissions"
)

// This file is the delivery-ACL half of CR-CHAT-003
// (specs/CHAT-PERMISSIONS.md §6): the two insertion points §6.8 fixes for the
// ONE deliver() path, and the machine-readable refusal.
//
// Why TWO points and not one (§6.8): an address-level check is a statement
// about a POOL (`may this sender invoke @cap:y?`) and can be made before any
// holder exists — and it must be, because holder selection is where a rotation
// turn is spent and a refused delivery must consume none (§6.4 rule 1). A
// target-level check is a statement about a specific agent and cannot be made
// before the row that carries its class and owner has been read. Both run the
// SAME §3.2 predicate against the SAME effective sender: two positions, one
// rule.
//
// The whole file is inert while no checker is wired: a nil handler.permissions
// is the pre-CR-CHAT-003 deployment, byte-identical (§6.4 rule 2).

// effectiveSenderOf computes the effective sender of a delivery request ONCE,
// before the ACL runs (§6.3). The order of the branches is the spec's:
//
//   - a request that arrived over a federation link names a LOCAL shadow
//     principal (§2.5, D14) — a remote actor never resolves to a local agent's
//     own reach and carries no remote grants;
//   - a request carrying principal_id is a human through the UI/API; it
//     requires a live binding, which the Checker enforces;
//   - a request carrying X-Agent-ID is an agent's own signed call, which §6.3
//     maps to its owner (for a personal agent);
//   - anything else is anonymous — allowed on an unclassed target (the shipped
//     posture) and refused on a classed one.
func effectiveSenderOf(r *http.Request, req *deliverRequest) permissions.EffectiveSender {
	if r.Header.Get(federation.HopHeader) != "" {
		return permissions.EffectiveSender{
			Kind:      permissions.SenderRemote,
			Principal: req.PrincipalID,
			AsAgent:   req.AsAgent,
		}
	}
	if req.PrincipalID != "" {
		return permissions.EffectiveSender{
			Kind:      permissions.SenderPrincipal,
			Principal: req.PrincipalID,
			AsAgent:   req.AsAgent,
		}
	}
	if id := r.Header.Get(HeaderAgentID); id != "" {
		return permissions.EffectiveSender{Kind: permissions.SenderAgent, AgentID: id}
	}
	return permissions.EffectiveSender{Kind: permissions.SenderAnonymous}
}

// authorizeCapabilityAddress runs §6.8 step 4 — the ADDRESS-level check for a
// pool/roster address (`@cap:y`), before the holder is chosen. It returns true
// when it has written a refusal (the caller must return).
//
// Only `capability` deliveries exist on today's wire; `@team:x` is a grant
// contract with no route yet (§8.1 item 11), so it is not reachable here.
func (h *Handler) authorizeCapabilityAddress(w http.ResponseWriter, r *http.Request, req *deliverRequest, capability string) bool {
	if h.permissions == nil || capability == "" {
		return false
	}
	return h.refuseIfDenied(w, r, permissions.CheckInput{
		Sender:  effectiveSenderOf(r, req),
		Action:  permissions.ActionInvoke,
		Subject: permissions.Subject{Type: permissions.SubjectCapability, Ref: capability},
	})
}

// authorizeAgentTarget runs §6.8 step 7 — the TARGET-level check for an agent
// address, after the target row and its realm have been read. It returns true
// when it has written a refusal.
func (h *Handler) authorizeAgentTarget(w http.ResponseWriter, r *http.Request, req *deliverRequest, target *Agent) bool {
	if h.permissions == nil || target == nil {
		return false
	}
	return h.refuseIfDenied(w, r, permissions.CheckInput{
		Sender:  effectiveSenderOf(r, req),
		Action:  permissions.ActionSend,
		Subject: permissions.Subject{Type: permissions.SubjectAgent, Ref: target.ID},
	})
}

// refuseIfDenied evaluates one ACL question and writes the §6.7 refusal when
// the answer is no. A store read failure is a 500, never a silent allow: an
// authorization decision that cannot be made must not be skipped (§6.4 — no
// fall-through).
func (h *Handler) refuseIfDenied(w http.ResponseWriter, r *http.Request, in permissions.CheckInput) bool {
	res, err := h.permissions.MayDeliver(r.Context(), in)
	if err != nil {
		slog.Error("delivery acl unavailable",
			"error", err,
			"target", in.Subject.Ref,
			"target_type", string(in.Subject.Type),
			"action", string(in.Action),
			"principal", in.Sender.Principal,
		)
		httperr.WriteJSONError(w, http.StatusInternalServerError, "permission store unavailable")
		return true
	}
	if res.Allowed {
		return false
	}
	httperr.WriteJSON(w, http.StatusForbidden, permissions.RefusalFor(in, res))
	return true
}
