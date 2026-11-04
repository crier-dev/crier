// ingressroutes.go — wiring for the push-ingress surface (CRIER-INGRESS-1,
// specs/COURIER-INGRESS.md).
//
// The handler itself lives in internal/ingress. This file supplies the three
// things it needs from the process, and nothing else:
//
//   - the SESSION repository, the one CR_SESSION_BACKEND selected, so a
//     session-mode or new_session push is written through exactly the store and
//     the record shapes the /sessions routes use;
//   - the INBOX delivery path, adapted onto internal/registry's guarded
//     dispatcher, so mode=inbox produces the same entry id and the same
//     lease/ack semantics POST /agents/{id}/inbox produces;
//   - the route log and the idempotency ledger (§3).
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/ingress"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/permissions"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/session"
)

// registryInbox adapts the registry's delivery core to ingress.InboxWriter.
//
// It calls Handler.DeliverComposed, which is the Go-level twin of
// POST /agents/{id}/inbox and POST /capabilities/{capability}/inbox: the same
// guarded Dispatch, webhook driver, durable inbox write and capability
// selection. The entry it composes is the entry those routes compose — payload,
// sender, tags, idempotency key, created-at — so an inbox entry written by
// ingress is indistinguishable from one written over HTTP, which is exactly
// what COURIER-INGRESS §4.2 and test 7 require.
type registryInbox struct {
	handler *registry.Handler
}

// Deliver implements ingress.InboxWriter.
func (r registryInbox) Deliver(ctx context.Context, agentID, capability string, msg ingress.InboxMessage) ([]session.DeliveryOutcome, error) {
	if r.handler == nil {
		return nil, fmt.Errorf("no registry handler is wired")
	}
	entry := &registry.InboxEntry{
		Payload:        msg.Payload,
		Sender:         msg.Author.AgentID(),
		IdempotencyKey: msg.IdempotencyKey,
		CreatedAt:      time.Now().UTC(),
		Tags:           msg.Tags,
		PrincipalID:    msg.Author.Principal,
	}
	result, err := r.handler.DeliverComposed(ctx, agentID, capability, entry)
	if err != nil {
		var noCapable *registry.NoCapableAgentError
		switch {
		case errors.As(err, &noCapable):
			return nil, fmt.Errorf("%w: %v", ingress.ErrAgentUnknown, err)
		case errors.Is(err, registry.ErrAgentNotFound):
			return nil, fmt.Errorf("%w: agent %q is not registered%s", ingress.ErrAgentUnknown, agentID, capabilityNote(capability))
		default:
			return nil, err
		}
	}
	outcome := session.OutcomeAccepted
	if result.StatusCode >= 400 && result.StatusCode < 500 {
		outcome = session.OutcomeRefused
	} else if result.StatusCode >= 500 {
		outcome = session.OutcomeFailed
	}
	now := time.Now().UTC()
	return []session.DeliveryOutcome{{
		Target:       entry.AgentID,
		Outcome:      outcome,
		InboxEntryID: entry.ID,
		UpdatedAt:    now,
		Reason:       deliveryOutcomeReason(result),
	}}, nil
}

// capabilityNote names the pool a delivery was aimed at when the caller used the
// capability form, so a 404 says which of the two addresses failed.
func capabilityNote(capability string) string {
	if capability == "" {
		return ""
	}
	return fmt.Sprintf(" (capability-routed: %q)", capability)
}

// deliveryOutcomeReason carries the dispatch's machine status on a non-accept so
// the ingress response never reports a bare refusal.
func deliveryOutcomeReason(result registry.DeliveryResult) string {
	if result.StatusCode >= 400 {
		return fmt.Sprintf("delivery path answered %d", result.StatusCode)
	}
	return ""
}

// openIngress builds the push-ingress handler over the already-open session
// store and registry.
//
// A route log that cannot be opened is a boot error: the surface writes a
// durable receipt for every inbox push, and a process that cannot write it must
// not start claiming pushes it cannot account for. The idempotency ledger is
// process-scoped (see internal/ingress/idempotency.go for the stated scope).
func openIngress(cfg config.Config, repo session.Repository, dispatcher *registry.Handler, namespaces *namespace.Registry, perms *permissions.Checker) (*ingress.Handler, error) {
	root := strings.TrimSpace(cfg.Session.LogRoot)
	if root == "" {
		root = config.DefaultSessionLogRoot
	}
	routes, err := ingress.NewJSONLRouteStore(root)
	if err != nil {
		return nil, fmt.Errorf("ingress route log: %w", err)
	}
	h := &ingress.Handler{
		Repo:         repo,
		Inbox:        registryInbox{handler: dispatcher},
		Route:        routes,
		Idem:         ingress.NewMemoryIdempotencyStore(),
		Namespaces:   namespaces,
		Permissions:  perms,
		AllowedModes: ingressModes(cfg.Ingress.AllowedModes),
		MaxBodyBytes: cfg.Ingress.MaxBodyBytes,
	}
	// The dispatch hook is wired to the harness registry (COURIER-HARNESS §4,
	// via harnesswiring.go). A session with no harness binding still answers
	// `{"mode":"store","state":"stored"}`, which is the honest report and not a
	// weaker success: the courier record IS the delivery for a store-only
	// session.
	if reg := buildHarnessRegistry(cfg); reg != nil {
		h.Dispatch = &harnessDispatch{reg: reg, repo: repo}
	}
	return h, nil
}

// ingressModes converts the configured mode strings into the handler's typed
// set. An empty configuration means "all three" and is passed through as nil.
func ingressModes(configured []string) []ingress.Mode {
	if len(configured) == 0 {
		return nil
	}
	out := make([]ingress.Mode, 0, len(configured))
	for _, m := range configured {
		out = append(out, ingress.Mode(strings.ToLower(strings.TrimSpace(m))))
	}
	return out
}
