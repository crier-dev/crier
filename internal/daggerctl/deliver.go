package daggerctl

import (
	"fmt"
	"strings"

	"github.com/crier-dev/crier/internal/registry"
)

// InboxStore is the slice of registry.Store this package needs: the direct
// durable-inbox write every terminal notification in crier already uses.
type InboxStore interface {
	Deliver(agentID string, entry *registry.InboxEntry) error
}

// inboxDeliverer is the shipped Deliverer: it writes a notification into an
// agent's durable inbox with the SAME call a MESSAGE_EXPIRED receipt and a
// WEBHOOK_FAILED notification use (registry.Store.Deliver). There is no second
// delivery path, which is the whole point — a dagger outcome must be visible in
// the thread it was started from, not on a side channel.
type inboxDeliverer struct{ store InboxStore }

// InboxDeliverer adapts a registry store into the Deliverer a Service uses.
func InboxDeliverer(store InboxStore) Deliverer { return inboxDeliverer{store: store} }

// DeliverToInbox writes payload into agentID's durable inbox. An absent agent
// is refused by the store (ErrAgentNotFound), which the Service records as the
// run's notify_error rather than swallowing.
func (d inboxDeliverer) DeliverToInbox(agentID string, payload []byte) error {
	if d.store == nil {
		return fmt.Errorf("dagger notification: no inbox store is wired")
	}
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("dagger notification: requesting agent is empty")
	}
	return d.store.Deliver(agentID, &registry.InboxEntry{Payload: payload})
}
