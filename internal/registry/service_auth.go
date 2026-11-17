package registry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// ValidateServiceIdentities prevents policy activation from turning ordinary
// registration into service-identity provisioning.
func ValidateServiceIdentities(store Store, policy ServiceDeliveryPolicy) error {
	seen := map[string]struct{}{}
	for recipient, senders := range policy {
		seen[recipient] = struct{}{}
		for sender := range senders {
			seen[sender] = struct{}{}
		}
	}
	for id := range seen {
		agent, err := store.Get(id)
		if err != nil {
			return fmt.Errorf("service identity %q is not pre-provisioned: %w", id, err)
		}
		if len(agent.PublicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("service identity %q has no valid Ed25519 public key", id)
		}
	}
	return nil
}

// SeedIdentity is one identity the OPERATOR declares at boot
// (CR_REGISTRY_SEED_IDENTITIES) so an empty store starts with it. It carries
// only public material: the private half stays with the service that holds the
// identity, exactly as it does when the same identity is registered over
// POST /agents.
type SeedIdentity struct {
	ID        string
	PublicKey HexKey
}

// ParseSeedIdentities validates CR_REGISTRY_SEED_IDENTITIES. Empty disables
// seeding; nonempty input must consist entirely of id=<hex ed25519 public key>
// entries. Surrounding whitespace is ignored, identity bytes/case are not
// changed. A declaration is explicit per identity — there is deliberately no
// wildcard, no "seed everything on the allowlist", and no default.
func ParseSeedIdentities(raw string) ([]SeedIdentity, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := map[string]struct{}{}
	ids := make([]SeedIdentity, 0, strings.Count(raw, ",")+1)
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.Split(strings.TrimSpace(entry), "=")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid CR_REGISTRY_SEED_IDENTITIES entry %q: want exactly id=<hex ed25519 public key>", entry)
		}
		id, rawKey := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if !serviceAgentID.MatchString(id) {
			return nil, fmt.Errorf("invalid CR_REGISTRY_SEED_IDENTITIES entry %q: agent IDs must match [A-Za-z0-9][A-Za-z0-9._:-]*; no wildcards", entry)
		}
		key, err := hex.DecodeString(rawKey)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid CR_REGISTRY_SEED_IDENTITIES entry %q: public key must be %d hex-encoded bytes", entry, ed25519.PublicKeySize)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("invalid CR_REGISTRY_SEED_IDENTITIES entry %q: identity %q is declared twice", entry, id)
		}
		seen[id] = struct{}{}
		ids = append(ids, SeedIdentity{ID: id, PublicKey: HexKey(key)})
	}
	return ids, nil
}

// ApplySeedIdentities provisions the operator-declared identities into a store
// at boot, BEFORE any service-delivery policy is validated. Seeding is legal
// only on the in-memory backend: it starts empty by definition, so the guard's
// "the identity already exists" requirement has no other way to be satisfied
// without the operator also landing at the registration API inside the same
// process lifetime. A durable store already keeps its own agents across
// restarts; re-seeding it at every boot would silently resurrect rows an
// operator deleted, so that is refused rather than performed.
func ApplySeedIdentities(store Store, ids []SeedIdentity) error {
	if len(ids) == 0 {
		return nil
	}
	if _, ok := store.(*MemoryStore); !ok {
		return fmt.Errorf("CR_REGISTRY_SEED_IDENTITIES applies only to the in-memory registry backend (got %T): a durable store keeps its own agents, so provision new identities with POST /agents", store)
	}
	for _, id := range ids {
		if err := store.Register(&Agent{ID: id.ID, PublicKey: id.PublicKey, Capabilities: []string{}}); err != nil {
			return fmt.Errorf("seed identity %q: %w", id.ID, err)
		}
	}
	return nil
}

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

// SetServiceDelivery configures the complete protected-delivery posture.  A
// non-empty policy requires a relay audience and a policy-enforcing store.
func (h *Handler) SetServiceDelivery(raw, audience string) error {
	policy, err := ParseServiceDeliveryAllowlist(raw)
	if err != nil {
		return err
	}
	if len(policy) > 0 && strings.TrimSpace(audience) == "" {
		return fmt.Errorf("CR_SERVICE_DELIVERY_AUDIENCE is required when CR_SERVICE_DELIVERY_ALLOWLIST is nonempty")
	}
	h.serviceDeliveries = policy
	h.serviceAudience = audience
	if len(policy) > 0 {
		if wrapped, ok := h.store.(*AuthorityStore); ok {
			wrapped.policy, wrapped.audience = policy, audience
		} else {
			h.store = NewAuthorityStore(h.store, policy, audience)
		}
	}
	return nil
}

type serviceDeliveryContextKey struct{}

// authorizeServiceDelivery runs BEFORE budgets, replay registration, holder
// rotation or transports. Detection only records the response; it cannot admit
// a protected recipient. A protected recipient is reached ONLY via its
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
	if h.serviceAudience != "" && req.IdempotencyKey == "" {
		_ = h.agentSigFail(w, http.StatusBadRequest, "protected service delivery requires nonempty idempotency_key")
		return false
	}
	audience := r.Header.Get(HeaderServiceAudience)
	if h.serviceAudience != "" && (audience == "" || audience != h.serviceAudience) {
		_ = h.agentSigFail(w, http.StatusUnauthorized, "service delivery audience missing or mismatched")
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
	a := &ServiceAuthority{sender: sender, recipient: id, idempotencyKey: req.IdempotencyKey,
		bodyDigest: r.Header.Get(HeaderAgentBodySHA256), audience: audience, rawBody: append([]byte(nil), body...)}
	if secured, ok := h.store.(*AuthorityStore); ok {
		receipt, err := secured.lookupAuthorized(a)
		if errors.Is(err, ErrServiceIdempotencyConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return false
		}
		if err != nil {
			writeStoreError(w, err)
			return false
		}
		if receipt != nil {
			writeServiceReceipt(w, receipt)
			return false
		}
	}
	*r = *r.WithContext(context.WithValue(r.Context(), serviceDeliveryContextKey{}, a))
	return true
}

func writeServiceReceipt(w http.ResponseWriter, receipt *ServiceDeliveryReceipt) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(receipt.Status)
	_, _ = w.Write(receipt.Response)
}
