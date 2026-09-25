package registry

import (
	"encoding/json"
	"fmt"
	"time"
)

// This file is the ownership-failure half of the lease model (CR-FEAT-025):
// a message that expires unacknowledged is not silently dropped. It becomes a
// DEAD LETTER — retrievable from a dead-letter path — and its sender receives
// exactly one durable receipt, in the same shape the house already uses for
// the other terminal outcomes (WEBHOOK_FAILED, DF-CRIER-8; FEDERATION_FAILED,
// DF-CRIER-7): an error envelope written straight into the sender's own inbox
// through Store.Deliver, never routed back through webhook/federation
// delivery, so it cannot recurse and it persists exactly like any other inbox
// entry.
//
// Before this, `PurgeExpired` was the whole story: it returned a count and the
// message was gone — no record of the body, no way for the sender to learn its
// work had died. "Failure is a message, not a mystery."

// CodeMessageExpired is the machine-readable error code carried by the expiry
// receipt (the house `kind: "error"` inbox notification). It is the counterpart
// of webhook.CodeWebhookFailed and federation.CodeFederationFailed for the
// inbox/TTL lane: the message was accepted (201), stored durably, and then
// expired while it was still unacknowledged.
const CodeMessageExpired = "MESSAGE_EXPIRED"

// DeadLetterReasonExpired is the reason recorded on a dead letter whose TTL
// elapsed while it was still unacknowledged. It is the only reason the inbox
// lane produces today; the field exists so a later reason (an exhausted
// redelivery budget, an operator purge) does not have to change the record
// shape.
const DeadLetterReasonExpired = "ttl_expired_unacked"

// DefaultDeadLetterRetention is how long a dead letter is kept before the
// expiry sweep also removes the DEAD LETTER rows it produced. Retention is what
// keeps a dead-letter destination from becoming an unbounded archive: a message
// nobody retrieves within this window is gone for good, and the sweep that
// removed it is the sweep that forgets it. Backends that cannot sweep (the
// in-memory queue is bounded instead) document their own bound.
const DefaultDeadLetterRetention = 7 * 24 * time.Hour

// DeadLetter is one message that expired unacknowledged in an agent's inbox,
// preserved with everything a consumer needs to act on it: the payload the
// sender originally delivered, its ids, and when it died.
//
// It is deliberately NOT foreign-keyed to the agent row. A dead letter exists
// precisely for the case where the receiving side stopped consuming — an agent
// that was unregistered, renamed or crashed must not take the record of its
// undelivered work with it.
type DeadLetter struct {
	// MessageID is the id the message was delivered under. It is the primary
	// key of the dead-letter destination: a message can be dead-lettered
	// exactly once, so a store that is told about the same removal twice does
	// not create a second record (and the receipt is sent once).
	MessageID string `json:"message_id"`
	// AgentID is the inbox the message died in — the TARGET of the delivery,
	// not its sender.
	AgentID string `json:"agent_id"`
	// Sender is the originating agent id, empty when the delivery named none.
	// It is the address the expiry receipt is sent to.
	Sender string `json:"sender,omitempty"`
	// Payload is the stored message body, byte-for-byte what was delivered
	// (after the guard's verdict was applied, exactly like the inbox copy).
	Payload []byte `json:"payload"`
	// CreatedAt is when the message was delivered.
	CreatedAt time.Time `json:"created_at"`
	// ExpiredAt is the expiry instant that elapsed unacknowledged.
	ExpiredAt time.Time `json:"expires_at"`
	// DeadLetteredAt is when the expiry sweep removed it from the inbox.
	DeadLetteredAt time.Time `json:"dead_lettered_at"`
	// Reason is why it was dead-lettered (DeadLetterReasonExpired today).
	Reason string `json:"reason"`
	// IdempotencyKey is the sender-supplied key the delivery carried, when it
	// carried one (CR-FEAT-025). It is provenance only: deduplication happens
	// at deliver time, and a dead letter records which key produced this
	// message so a re-delivery under the same key can be reasoned about.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Namespace is the realm the message belonged to when it expired
	// (CR-FEAT-029), copied from the inbox entry at sweep time. A dead letter
	// outlives its agent's row (the destination is deliberately not
	// foreign-keyed), so the realm has to be recorded WITH the record or the
	// operator loses the one fact that says whose retention policy applied.
	// Empty = the default namespace, and `omitempty` keeps every
	// pre-CR-FEAT-029 record byte-identical.
	Namespace string `json:"namespace,omitempty"`
}

// expiryReceiptPayload is the durable, machine-readable inbox payload of an
// expiry receipt (the MESSAGE_EXPIRED notification). It mirrors the
// webhookFailurePayload / federationFailurePayload shape: kind=error, the code,
// and the correlation context of the original delivery. The message body is
// never echoed — the body itself lives in the dead-letter record, which is what
// the `dead_letter` pointer names.
type expiryReceiptPayload struct {
	Kind      string `json:"kind"` // always "error"
	Code      string `json:"code"` // always CodeMessageExpired
	MessageID string `json:"message_id"`
	Target    string `json:"target"`
	Sender    string `json:"sender,omitempty"`
	// ExpiresAt is the expiry instant that elapsed unacknowledged, RFC 3339.
	ExpiresAt time.Time `json:"expires_at"`
	// DeadLetteredAt is when the sweep removed the message.
	DeadLetteredAt time.Time `json:"dead_lettered_at"`
	// DeadLettered is always true today: an expiry receipt is only produced by
	// a sweep that recorded the message first. It is stated explicitly so a
	// sender needs no out-of-band knowledge to know the body is retrievable.
	DeadLettered bool `json:"dead_lettered"`
	// DeadLetterPath is the path the receiving agent (or an operator holding
	// the relay token) fetches the record from.
	DeadLetterPath string `json:"dead_letter_path"`
	Reason         string `json:"reason"`
	// IdempotencyKey echoes the delivery's key when it carried one.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// expiryReceiptPayloadBytes renders the receipt body for the sender's inbox.
func expiryReceiptPayloadBytes(dl *DeadLetter) ([]byte, error) {
	payload, err := json.Marshal(expiryReceiptPayload{
		Kind:           "error",
		Code:           CodeMessageExpired,
		MessageID:      dl.MessageID,
		Target:         dl.AgentID,
		Sender:         dl.Sender,
		ExpiresAt:      dl.ExpiredAt,
		DeadLetteredAt: dl.DeadLetteredAt,
		DeadLettered:   true,
		DeadLetterPath: DeadLettersPath(dl.AgentID),
		Reason:         dl.Reason,
		IdempotencyKey: dl.IdempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("expiry receipt: marshal payload: %w", err)
	}
	return payload, nil
}

// DeadLettersPath is the retrieval path of one agent's dead-letter destination
// — GET on it lists the messages that expired unacknowledged in that agent's
// inbox. It is a function rather than a literal so the receipt a sender
// receives and the route the server registers cannot drift apart.
func DeadLettersPath(agentID string) string {
	return "/agents/" + agentID + "/inbox/dead-letters"
}

// newDeadLetter builds the record for a message the expiry sweep removed.
func newDeadLetter(agentID string, entry *InboxEntry, now time.Time) *DeadLetter {
	return &DeadLetter{
		MessageID:      entry.ID,
		AgentID:        agentID,
		Sender:         entry.Sender,
		Payload:        entry.Payload,
		CreatedAt:      entry.CreatedAt,
		ExpiredAt:      entry.ExpiresAt,
		DeadLetteredAt: now,
		Reason:         DeadLetterReasonExpired,
		IdempotencyKey: entry.IdempotencyKey,
		// The realm travels with the record (CR-FEAT-029): the dead letter
		// outlives the agent row, so this is the only place the realm of the
		// retention policy that expired it can be read from afterwards.
		Namespace: entry.Namespace,
	}
}
