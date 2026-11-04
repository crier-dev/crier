package registry

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// NoCapableAgentError is the NAMED refusal of a capability-routed delivery
// nobody on this relay holds (CR-FEAT-026, ErrNoCapableAgent). It exists as an
// error VALUE so a Go caller that is not an http.Handler can tell this refusal
// apart from a transport failure without parsing prose — the HTTP deliver path
// answers the same case with 404 NO_CAPABLE_AGENT.
type NoCapableAgentError struct {
	Capability string
}

func (e *NoCapableAgentError) Error() string {
	return fmt.Sprintf("%s: %s", ErrNoCapableAgent, noCapableAgentDetail(e.Capability))
}

// DeliverComposed delivers ONE already-composed inbox entry, either to a named
// agent or — when capability is non-empty — to the holder the capability
// selector picks, and returns the dispatcher's result.
//
// It is the Go-level twin of POST /agents/{id}/inbox and
// POST /capabilities/{capability}/inbox for a caller that already owns a
// composed message and has no HTTP request to hand: the same guarded Dispatch,
// the same webhook driver, the same durable inbox write, the same
// lease/ack/TTL semantics, the same round-robin capability selection
// (selectCapabilityHolder, the function routeCapability wraps). The HTTP path
// keeps its own pre-processing — body decoding, the guard observation wrapper
// and the sender-side containment gate — none of which applies to an in-process
// caller that is not a remote sender.
//
// The entry is MUTATED the way the deliver path always mutates it: AgentID is
// the resolved recipient, CreatedAt is stamped when absent, and ID is minted
// when the caller supplied none. A caller that needs the stored inbox entry id
// reads it back from entry.ID after this returns.
//
// Errors:
//   - ErrAgentNotFound (wrapped by the store) when the named agent is not
//     registered, or when the request names neither an agent nor a capability;
//   - *NoCapableAgentError when capability is non-empty and nobody holds it;
//   - *DispatchError (or a transport error) when the delivery itself failed.
func (h *Handler) DeliverComposed(ctx context.Context, id, capability string, entry *InboxEntry) (DeliveryResult, error) {
	if entry == nil {
		return DeliveryResult{}, errors.New("nil delivery entry")
	}
	if capability != "" {
		holder, _, found := h.selectCapabilityHolder(capability, time.Now())
		if !found {
			capabilityUnheldTotal.Inc()
			return DeliveryResult{}, &NoCapableAgentError{Capability: capability}
		}
		capabilityRoutedTotal.Inc()
		id = holder.ID
	}
	if id == "" {
		return DeliveryResult{}, fmt.Errorf("%w: no agent id and no capability named", ErrAgentNotFound)
	}
	target, err := h.store.Get(id)
	if err != nil {
		return DeliveryResult{}, err
	}
	// Mirror Handler.Deliver: an entry without an id gets one from the SAME
	// minter the store would use, so the caller can name the row it just wrote.
	if entry.ID == "" {
		messageID, err := newLeaseID()
		if err != nil {
			return DeliveryResult{}, fmt.Errorf("generate message id: %w", err)
		}
		entry.ID = messageID
	}
	entry.AgentID = id
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	return h.Dispatch(ctx, target, DeliveryRequest{Entry: entry, DeliveryMode: "blocking"})
}
