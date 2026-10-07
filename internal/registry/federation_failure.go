package registry

import (
	"encoding/json"
	"fmt"

	"github.com/crier-dev/crier/internal/federation"
)

// fedPeerRefusalBody is the machine-readable 403 body of a per-peer policy
// refusal (CR-CHAT-023, spec §6.4): it names the decision inputs so an
// operator can explain a refusal without parsing prose.
type fedPeerRefusalBody struct {
	Error string `json:"error"`
	Peer  string `json:"peer"`
	// Direction is always inbound on this surface: the refusal was decided
	// by THIS instance's policy on a request that arrived over a link.
	Direction string            `json:"direction"`
	Target    fedPeerRefusalTgt `json:"target"`
	Detail    string            `json:"detail"`
}

type fedPeerRefusalTgt struct {
	Type string `json:"type"`
	Ref  string `json:"ref"`
}

// fedPeerRefusal builds the §6.4 refusal body for an inbound delivery the
// peer policy declined. code is one of the FED_* refusal codes; peer is the
// announced peer id; target is the (possibly unresolved) recipient id.
func fedPeerRefusal(code, peer, target, detail string) fedPeerRefusalBody {
	return fedPeerRefusalBody{
		Error:     code,
		Peer:      peer,
		Direction: "inbound",
		Target:    fedPeerRefusalTgt{Type: "agent", Ref: target},
		Detail:    detail,
	}
}

// federationFailurePayload is the machine-readable entry written into the
// sender's inbox when a held federated delivery exhausts its hold budget
// (DF-CRIER-7). Shape mirrors the webhook-failure payload (DF-CRIER-8):
// kind=error plus the FEDERATION_FAILED code and the correlation context of
// the original delivery. The message body is never echoed.
type federationFailurePayload struct {
	Kind      string `json:"kind"` // always "error"
	Code      string `json:"code"` // always federation.CodeFederationFailed
	MessageID string `json:"message_id"`
	Target    string `json:"target"`
	Sender    string `json:"sender,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Attempts  int    `json:"attempts"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// FederationFailureSink returns the notifier the federation hold manager
// invokes when a held delivery exhausts CR_FED_MAX_HOLD_S (DF-CRIER-7,
// spec §8). The FEDERATION_FAILED notification is written DIRECTLY into the
// originating sender's durable inbox via Store.Deliver — it never routes
// back through webhook or federation delivery, so it cannot recurse or
// requeue, and it persists under the Postgres-backed store exactly like any
// other inbox entry.
//
// Exactly-once contract: the hold manager removes the delivery from its
// queue BEFORE invoking this notifier and never requeues it, so at most one
// notification exists per held delivery. The call itself is best-effort: an
// unregistered/absent sender or a store failure is returned as an error for
// the manager to log — the notification is NOT retried.
func FederationFailureSink(store Store) func(federation.FailureReport) error {
	return func(r federation.FailureReport) error {
		payload, err := json.Marshal(federationFailurePayload{
			Kind:      "error",
			Code:      federation.CodeFederationFailed,
			MessageID: r.MessageID,
			Target:    r.Target,
			Sender:    r.Sender,
			RequestID: r.RequestID,
			SessionID: r.SessionID,
			Attempts:  r.Attempts,
			Status:    r.Status,
			Error:     r.Error,
		})
		if err != nil {
			return fmt.Errorf("federation failure notification: marshal payload: %w", err)
		}
		if r.Sender == "" {
			return fmt.Errorf("federation failure notification: no sender on message %q (target %q)", r.MessageID, r.Target)
		}
		if err := store.Deliver(r.Sender, &InboxEntry{Payload: payload}); err != nil {
			return fmt.Errorf("federation failure notification: deliver to sender %q inbox: %w", r.Sender, err)
		}
		return nil
	}
}
