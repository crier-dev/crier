package registry

import (
	"context"
	"fmt"
)

// SessionDeliverer adapts fan-out to the handler's guarded dispatcher, never
// directly to Store.Deliver. It shares the live handler (including its guard,
// webhook driver, realm policies and idempotency window) with HTTP ingress.
type SessionDeliverer struct{ handler *Handler }

func NewSessionDeliverer(h *Handler) *SessionDeliverer { return &SessionDeliverer{handler: h} }

// Deliver retains the InboxDeliverer contract for callers without a context.
// Session fan-out uses DispatchDelivery below to preserve all conversation data.
func (d *SessionDeliverer) Deliver(id string, entry *InboxEntry) error {
	req := DeliveryRequest{Entry: entry}
	if entry != nil && entry.Location != nil {
		req.SessionID = entry.Location.Channel
	}
	_, err := d.DispatchDelivery(context.Background(), id, req)
	return err
}

// DispatchDelivery waits for a webhook response: a synchronous transcript
// outcome must describe delivery, not background queue admission. HTTP ingress
// retains all three webhook modes unchanged. The inbox branch still writes via
// Dispatch's single Store.Deliver call and is never written as a webhook fallback.
func (d *SessionDeliverer) DispatchDelivery(ctx context.Context, id string, req DeliveryRequest) (DeliveryResult, error) {
	target, err := d.handler.store.Get(id)
	if err != nil {
		return DeliveryResult{}, fmt.Errorf("resolve delivery target %q: %w", id, err)
	}
	req.DeliveryMode = "blocking"
	return d.handler.Dispatch(ctx, target, req)
}
