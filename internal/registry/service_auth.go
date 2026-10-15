package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// ServiceDeliveryPolicy is an exact, directed recipient -> sender set. It is
// immutable after startup; no wildcard or human-principal identity is admitted.
type ServiceDeliveryPolicy map[string]map[string]struct{}

var serviceAgentID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// ParseServiceDeliveryAllowlist validates CR_SERVICE_DELIVERY_ALLOWLIST. Empty
// disables the gate; nonempty input must consist entirely of sender>recipient
// pairs. Surrounding whitespace is ignored, identity bytes/case are not changed.
func ParseServiceDeliveryAllowlist(raw string) (ServiceDeliveryPolicy, error) {
	policy := ServiceDeliveryPolicy{}
	if strings.TrimSpace(raw) == "" {
		return policy, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.Split(strings.TrimSpace(entry), ">")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid CR_SERVICE_DELIVERY_ALLOWLIST entry %q: want exact sender>recipient", entry)
		}
		sender, target := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if !serviceAgentID.MatchString(sender) || !serviceAgentID.MatchString(target) {
			return nil, fmt.Errorf("invalid CR_SERVICE_DELIVERY_ALLOWLIST entry %q: agent IDs must match [A-Za-z0-9][A-Za-z0-9._:-]*; no wildcards", entry)
		}
		if policy[target] == nil {
			policy[target] = map[string]struct{}{}
		}
		policy[target][sender] = struct{}{}
	}
	return policy, nil
}

// SetServiceDeliveryAllowlist is startup-only, like the other handler setters.
// On error the previous policy is preserved, never silently disabled.
func (h *Handler) SetServiceDeliveryAllowlist(raw string) error {
	policy, err := ParseServiceDeliveryAllowlist(raw)
	if err != nil {
		return err
	}
	h.serviceDeliveries = policy
	return nil
}

type serviceDeliveryContextKey struct{}

// authorizeServiceDelivery runs BEFORE detection, budgets, replay registration,
// holder rotation or transports. A protected recipient is reached ONLY via its
// explicit URL, not a pool, human binding, reply_to, or shared bearer identity.
// The authenticated recipient in context is the sole bypass of the human ACL;
// the untrusted JSON/header fields themselves never provide that bypass.
func (h *Handler) authorizeServiceDelivery(w http.ResponseWriter, r *http.Request, id, capability string) bool {
	if len(h.serviceDeliveries) == 0 {
		return true
	}
	if capability != "" {
		holders := h.capabilityHolders(capability)
		if reporter, ok := h.store.(ListErrorReporter); ok && reporter.ListError() != nil {
			writeStoreError(w, reporter.ListError())
			return false
		}
		for _, a := range holders {
			if _, protected := h.serviceDeliveries[a.ID]; protected {
				_ = h.agentSigFail(w, http.StatusForbidden, "service delivery requires an explicit recipient URL; capability routing cannot authorize a service recipient")
				return false
			}
		}
		return true
	}
	allowed, protected := h.serviceDeliveries[id]
	if !protected {
		return true
	}
	// Verify possession of the SENDER key, not the recipient key. Unlike the
	// existing self-only read/ack wrapper this path REQUIRES the 4-line form.
	sender := r.Header.Get(HeaderAgentID)
	if h.verifyAgentSignature(w, r, sender, true) != nil {
		return false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		_ = h.agentSigFail(w, http.StatusUnauthorized, agentSigBodyUnreadableError)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var req deliverRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return false
	}
	if req.Sender == "" || req.Sender != sender {
		_ = h.agentSigFail(w, http.StatusForbidden, "service delivery body sender must equal signed X-Agent-ID")
		return false
	}
	if req.PrincipalID != "" || req.AsAgent != "" {
		_ = h.agentSigFail(w, http.StatusForbidden, "principal_id/as_agent are human identities, not service delivery authorization")
		return false
	}
	if _, ok := allowed[sender]; !ok {
		_ = h.agentSigFail(w, http.StatusForbidden, "service delivery sender>recipient pair is not allowlisted")
		return false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), serviceDeliveryContextKey{}, id))
	return true
}
