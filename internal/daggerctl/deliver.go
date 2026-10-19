package daggerctl

import (
	"fmt"
	"strings"

	"github.com/crier-dev/crier/internal/registry"
)

// InboxStore is the delivery capability this package needs. Production injects
// the live registry.Handler, so notifications and waits pass its guarded
// dispatcher. Tests may inject a raw store; the adapter itself adds no policy.
type InboxStore interface {
	Deliver(agentID string, entry *registry.InboxEntry) error
}

// inboxDeliverer adapts the injected delivery capability without bypassing it.
// In production it is registry.Handler.Deliver (guard + transport); raw stores
// are appropriate only for isolated store-level tests.
type inboxDeliverer struct{ store InboxStore }

// InboxDeliverer adapts the injected delivery capability into a Service notifier.
func InboxDeliverer(store InboxStore) Deliverer { return inboxDeliverer{store: store} }

// DeliverToInbox sends payload through the injected capability. An absent agent
// is refused (ErrAgentNotFound), and the Service records any dispatch refusal as
// the run's notify_error rather than swallowing it.
func (d inboxDeliverer) DeliverToInbox(agentID string, payload []byte) error {
	if d.store == nil {
		return fmt.Errorf("dagger notification: no inbox store is wired")
	}
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("dagger notification: requesting agent is empty")
	}
	return d.store.Deliver(agentID, &registry.InboxEntry{Payload: payload})
}
