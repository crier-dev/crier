package registry

import (
	"encoding/json"
	"fmt"

	"github.com/totalwindupflightsystems/crier/internal/webhook"
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
}

// WebhookFailureSink returns the notifier the webhook driver invokes when a
// queued delivery exhausts its bounded retries (DF-CRIER-8). The
// notification is written DIRECTLY into the originating sender's durable
// inbox via Store.Deliver — it never routes back through webhook delivery,
// so it cannot recurse, and it persists under the Postgres-backed store
// exactly like any other inbox entry.
//
// Best-effort contract: an empty sender never reaches the sink (the driver
// logs and skips); an unregistered sender or a store failure is returned as
// an error for the driver to log — the failed delivery is NOT requeued and
// the notification is NOT retried.
func WebhookFailureSink(store Store) func(webhook.FailureNotification) error {
	return func(n webhook.FailureNotification) error {
		payload, err := json.Marshal(webhookFailurePayload{
			Kind:       "error",
			Code:       webhook.CodeWebhookFailed,
			MessageID:  n.MessageID,
			Target:     n.TargetAgent,
			Retries:    n.Retries,
			StatusCode: n.StatusCode,
			Err:        n.Err,
		})
		if err != nil {
			return fmt.Errorf("webhook failure notification: marshal payload: %w", err)
		}
		if err := store.Deliver(n.Sender, &InboxEntry{Payload: payload}); err != nil {
			return fmt.Errorf("webhook failure notification: deliver to sender %q inbox: %w", n.Sender, err)
		}
		return nil
	}
}
