// namespace.go — the registry lane's half of the realm dimension (CR-FEAT-029).
//
// The registry is where a namespace is DECIDED for practical purposes: an
// agent's row carries its namespace, and every delivery into that agent
// inherits it. That is what makes "a message cannot cross namespaces
// implicitly" true rather than aspirational — the effective namespace of a
// message is never taken from the caller's claim, it is taken from the
// TARGET's stored row, and a caller that states a different namespace is
// refused (NAMESPACE_MISMATCH) instead of being believed.
package registry

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/crier-dev/crier/internal/namespace"
)

// SetNamespacePolicies wires the namespace registry onto the handler
// (CR-FEAT-029). Nil — the default, and the ONLY state a deployment that
// declares no namespaces is in — means one implicit namespace: delivery
// resolves every target to the default namespace, the guard and retention
// resolve exactly as they did before this feature existed, and the namespace
// member is absent from every row and every response body.
//
// Wiring a registry with NO declared namespaces (the default namespace only) is
// behaviourally identical to nil: the difference between the two states is
// nothing a client can observe, which is the point.
func (h *Handler) SetNamespacePolicies(r *namespace.Registry) {
	h.namespaces = r
}

// Namespaces exposes the wired registry (nil when none is configured) so the
// server serves the same policy set it enforces — one source, no copy.
func (h *Handler) Namespaces() *namespace.Registry {
	return h.namespaces
}

// resolveRegistrationNamespace validates the namespace a registration asks for.
//
//   - absent/""/"default" → the default namespace ("", the canonical spelling
//     every pre-CR-FEAT-029 registration implicitly got);
//   - a name this server does not declare → ErrUnknownNamespace. It is NEVER
//     mapped back to the default: a client that misspells its realm would
//     otherwise silently join a different one — and, with per-namespace guard
//     settings, silently leave the guarded realm;
//   - a declared name → its canonical form, once the namespace's auth posture
//     is satisfied by the supplied credential.
func (h *Handler) resolveRegistrationNamespace(name, token string) (string, error) {
	canonical := namespace.Canonical(name)
	if canonical == "" {
		// The default namespace may still carry an auth posture (an operator
		// can declare it), so it is checked rather than waved through.
		if err := h.namespaces.CheckAuth("", token); err != nil {
			return "", err
		}
		return "", nil
	}
	if _, ok := h.namespaces.Lookup(canonical); !ok {
		return "", namespace.ErrUnknownNamespace
	}
	if err := h.namespaces.CheckAuth(canonical, token); err != nil {
		return "", err
	}
	return canonical, nil
}

// writeNamespaceError renders the response for a request whose namespace name
// or namespace credential this server cannot honor.
//
// The two failures are kept apart on purpose: a wrong/missing namespace
// credential is 401 and the body says NAMESPACE_UNAUTHORIZED (the caller must
// authenticate again), while an undeclared name is a 400 naming the problem.
// Neither is ever answered with the default namespace.
func writeNamespaceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, namespace.ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "NAMESPACE_UNAUTHORIZED", "detail": err.Error()})
	case errors.Is(err, namespace.ErrUnknownNamespace):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "UNKNOWN_NAMESPACE", "detail": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

// namespacesResponse is the GET /namespaces body.
type namespacesResponse struct {
	// Default names the namespace every agent gets when none is declared. It
	// is always "default" and is served so a client never has to hard-code the
	// spelling of the implicit realm.
	Default    string               `json:"default"`
	Count      int                  `json:"count"`
	Namespaces []namespaceListEntry `json:"namespaces"`
}

// namespaceListEntry is one namespace as GET /namespaces reports it: the policy
// an operator can act on, and NOTHING that could disclose a secret — Auth is
// the posture, TokenRef is an environment REFERENCE (never the value it
// resolves to), and no resolved token is ever rendered.
type namespaceListEntry struct {
	Name               string `json:"name"`
	Auth               string `json:"auth"`
	TokenRef           string `json:"token_ref,omitempty"`
	Configured         bool   `json:"configured"`
	RateLimitPerMinute *int   `json:"rate_limit_per_minute,omitempty"`
	GuardEnabled       *bool  `json:"guard_enabled,omitempty"`
	HasGuardPolicy     bool   `json:"has_guard_policy"`
	RetentionSeconds   *int64 `json:"retention_seconds,omitempty"`
	Agents             int    `json:"agents"`
}

// HandleListNamespaces serves GET /namespaces: the policy set this server is
// actually enforcing, counted against the live registry so an operator can see
// which realms are in use. It reads no body and takes no parameters.
//
// The agent census is per namespace and derived from the SAME rows the delivery
// path routes on, so the count cannot disagree with the routing.
func (h *Handler) HandleListNamespaces(w http.ResponseWriter, r *http.Request) {
	entries := make([]namespaceListEntry, 0)
	census := map[string]int{}
	for _, a := range h.store.List() {
		census[namespace.Canonical(a.Namespace)]++
	}
	for _, p := range h.namespaces.Policies() {
		entries = append(entries, namespaceListEntry{
			Name:               p.Display,
			Auth:               string(p.Auth),
			TokenRef:           p.TokenRef,
			Configured:         p.RateLimitPerMinute != nil || p.GuardEnabled != nil || p.GuardPolicy != nil || p.RetentionSeconds != nil || p.TokenRef != "",
			RateLimitPerMinute: p.RateLimitPerMinute,
			GuardEnabled:       p.GuardEnabled,
			HasGuardPolicy:     p.GuardPolicy != nil,
			RetentionSeconds:   p.RetentionSeconds,
			Agents:             census[p.Name],
		})
	}
	writeJSON(w, http.StatusOK, namespacesResponse{
		Default:    namespace.DefaultName,
		Count:      len(entries),
		Namespaces: entries,
	})
}

// namespaceOfAgent is the effective namespace of a stored agent row, which is
// the authority for every delivery to it.
func namespaceOfAgent(a *Agent) string {
	if a == nil {
		return ""
	}
	return namespace.Canonical(a.Namespace)
}

// retentionFor returns the default message lifetime for a delivery into the
// given namespace: the namespace's declared retention, else the store's
// DefaultMessageTTL — which is what every delivery got before this feature.
func retentionFor(pol namespace.Policy) time.Duration {
	return pol.Retention(DefaultMessageTTL)
}

// logNamespaceDeliver records the realm a delivery resolved to, at debug: it is
// per-message evidence for the isolation property, and it carries no payload, no
// token and no resolved secret.
func logNamespaceDeliver(target, nsName, sender string) {
	if nsName == "" {
		return
	}
	slog.Debug("inbox deliver scoped to namespace",
		"target", target, "namespace", namespace.Display(nsName), "sender", sender)
}
