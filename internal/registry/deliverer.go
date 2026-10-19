package registry

import (
	"context"
	"fmt"
)

// Deliver adapts non-HTTP producers to the guarded dispatch core. Unlike queue
// admission, success here confirms the webhook answered (or the inbox write
// completed). It uses the existing HTTP blocking budget, not the session budget.
// Callers with session context use SessionDeliverer instead.
func (h *Handler) Deliver(id string, entry *InboxEntry) error {
	if entry == nil {
		return fmt.Errorf("nil delivery entry")
	}
	entry.AgentID = id
	if entry.ID == "" {
		messageID, err := newLeaseID()
		if err != nil {
			return fmt.Errorf("generate message id: %w", err)
		}
		entry.ID = messageID
	}
	target, err := h.store.Get(id)
	if err != nil {
		return fmt.Errorf("resolve delivery target %q: %w", id, err)
	}
	_, err = h.Dispatch(context.Background(), target, DeliveryRequest{Entry: entry, DeliveryMode: "blocking"})
	return err
}
