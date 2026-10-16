package registry

import (
	"encoding/json"
	"fmt"

	"github.com/crier-dev/crier/internal/webhook"
)

// webhookFailurePayload is the durable, machine-readable inbox payload for a
// WEBHOOK_FAILED notification (DF-CRIER-8, spec §4): the originating sender
// learns that an async delivery to a target agent exhausted its retries.
type webhookFailurePayload struct {
	Kind       string `json:"kind"` // always "error"
	Code       string `json:"code"` // always webhook.CodeWebhookFailed
	MessageID  string `json:"message_id"`
	Target     string `json:"target"`
	Retries    int    `json:"retries"`
	StatusCode int    `json:"status_code,omitempty"`
	Err        string `json:"error,omitempty"`
	// NoSender marks a DF-CRIER-320 target-inbox fallback notification: the
	// exhausted delivery carried no Crier.Sender, so this entry was written
	// to the TARGET agent's own inbox rather than a sender's. It is omitted
	// whenever a sender was present, so a sender notification keeps exactly
	// the pre-DF-CRIER-320 payload shape.
	NoSender bool `json:"no_sender,omitempty"`
}

// WebhookFailureSink returns the notifier the webhook driver invokes when a
// queued delivery exhausts its bounded retries (DF-CRIER-8). The
// notification is written DIRECTLY into a durable inbox via Store.Deliver —
// it never routes back through webhook delivery, so it cannot recurse, and
// it persists under the Postgres-backed store exactly like any other inbox
// entry.
//
// ADDRESSING IS THE SINK'S JOB (DF-CRIER-320):
//
//   - Sender set → the originating sender's inbox, unchanged.
//   - Sender empty (the documented default for fire-and-forget callers) →
//     NOT a skip: the entry goes to the TARGET agent's own inbox, because
//     the operator whose endpoint dropped the delivery is the one who must
//     learn about it. The payload then carries no_sender:true so it is
//     distinguishable from a sender notification.
//
// Best-effort contract: only a FAILED write is best-effort — an unregistered
// destination (sender, or target when there is no sender) or a store failure
// is returned as an error for the driver to log; the failed delivery is NOT
// requeued and the notification is NOT retried.
func WebhookFailureSink(store Store) func(webhook.FailureNotification) error {
	return func(n webhook.FailureNotification) error {
		dest := n.Sender
		noSender := n.Sender == ""
		if noSender {
			// No sender to attribute the failure to: fall back to the agent
			// whose endpoint failed, so the drop is never silent.
			dest = n.TargetAgent
		}
		payload, err := json.Marshal(webhookFailurePayload{
			Kind:       "error",
			Code:       webhook.CodeWebhookFailed,
			MessageID:  n.MessageID,
			Target:     n.TargetAgent,
			Retries:    n.Retries,
			StatusCode: n.StatusCode,
			Err:        n.Err,
			NoSender:   noSender,
		})
		if err != nil {
			return fmt.Errorf("webhook failure notification: marshal payload: %w", err)
		}
		if err := store.Deliver(dest, &InboxEntry{Payload: payload}); err != nil {
			if noSender {
				return fmt.Errorf("webhook failure notification: no sender; deliver to target %q inbox: %w", dest, err)
			}
			return fmt.Errorf("webhook failure notification: deliver to sender %q inbox: %w", n.Sender, err)
		}
		return nil
	}
}
