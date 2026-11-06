package registry

// CR-GAP-074 regression pins: reply_to is ENVELOPE metadata, never payload data.
//
// The correlation a reply answers travels on the delivery envelope — the
// top-level `reply_to` member of the POST body (deliverRequest.ReplyTo), which
// is what the handler copies onto InboxEntry.ReplyTo. It is never read out of
// the message payload: a `reply_to` inside the payload is client data, confers
// no authority (CR-FEAT-036 AC2) and must not forge a correlation onto the
// stored entry either.
//
// These arms exist because the row was filed claiming the opposite data flow.
// They are the executable answer: if a future change starts reading the
// correlation out of the payload (or drops the envelope member), they fail.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// replyToEnvelopeCorrelation is the correlation the envelope-carrying arms use.
// It is deliberately not a plausible agent id: a reply_to is provenance, and the
// tests must not be able to pass by treating it as an address.
const replyToEnvelopeCorrelation = "dispatch-6be0dd4d2b8e"

// replyToPeek reads one stored entry through the read-only capability, so the
// assertion is made against STORAGE rather than against the wire shape alone.
func replyToPeek(t *testing.T, store Store, agentID, messageID string) *InboxEntry {
	t.Helper()
	peeker, ok := store.(InboxPeeker)
	require.True(t, ok, "the store must answer a Peek to assert the stored entry")
	entry, err := peeker.Peek(agentID, messageID)
	require.NoError(t, err)
	require.NotNil(t, entry)
	return entry
}

// TestEnvelopeReplyToPopulatesInboxEntry is brief AC1: a message delivered with
// `reply_to` on the envelope — and nothing of the sort in its payload — yields an
// InboxEntry whose ReplyTo is that value, and the same value is handed back on
// every read of the message.
func TestEnvelopeReplyToPopulatesInboxEntry(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.register(t, cr36Router)

	// The envelope carries the correlation; the payload is ordinary work.
	h.deliver(t, cr36Router,
		`{"payload":{"result":"done"},"reply_to":"`+replyToEnvelopeCorrelation+`"}`,
		http.StatusCreated)

	polled := h.poll(t, cr36Router, 30)
	require.Len(t, polled.Messages, 1)

	// AC1 on the wire: the polled message carries the correlation it was sent
	// with, as a top-level member (not nested in the payload).
	wireReplyTo, present := cr36Message(polled.Messages[0], "reply_to")
	require.True(t, present, "the polled message must carry the envelope reply_to")
	require.JSONEq(t, `"`+replyToEnvelopeCorrelation+`"`, wireReplyTo)

	// The payload is rendered as base64 on the wire (`[]byte`), so it is
	// decoded before asserting on its content.
	rawPayload, err := base64.StdEncoding.DecodeString(
		strings.Trim(string(polled.Messages[0]["payload"]), `"`))
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(rawPayload, &payload))
	_, payloadHas := payload["reply_to"]
	require.False(t, payloadHas, "crier must not copy the correlation into the payload")

	// AC1 in storage: the entry behind the wire holds it too — and the payload
	// is stored byte-for-byte as sent.
	entry := replyToPeek(t, h.store, cr36Router, cr36ID(t, polled.Messages[0]))
	require.Equal(t, replyToEnvelopeCorrelation, entry.ReplyTo,
		"the envelope reply_to must land on InboxEntry.ReplyTo")
	require.JSONEq(t, `{"result":"done"}`, string(entry.Payload),
		"the stored payload must be exactly what the sender wrote")
}

// TestReplyToIsNotReadFromThePayload is the negative half of the flow the brief
// describes: a correlation that appears ONLY inside the payload is payload data.
// It must not populate the entry, must not appear as envelope metadata on the
// read, and — when the envelope also carries one — the envelope value is the
// one that is stored, in both directions.
func TestReplyToIsNotReadFromThePayload(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.register(t, cr36Router)

	// 1. Payload-only: silently stored as ordinary message content, entry empty.
	h.deliver(t, cr36Router,
		`{"payload":{"result":"done","reply_to":"payload-only","crier_reply_to":"payload-only"}}`,
		http.StatusCreated)

	payloadOnly := h.poll(t, cr36Router, 30)
	require.Len(t, payloadOnly.Messages, 1)
	_, present := cr36Message(payloadOnly.Messages[0], "reply_to")
	require.False(t, present,
		"a payload-only reply_to must not surface as envelope metadata")
	entry := replyToPeek(t, h.store, cr36Router, cr36ID(t, payloadOnly.Messages[0]))
	require.Empty(t, entry.ReplyTo,
		"a payload-only reply_to must not be read into InboxEntry.ReplyTo")

	// 2. Both present: the envelope wins. The payload copy stays what it is —
	// client content — and never overrides the correlation the caller declared.
	h.deliver(t, cr36Router,
		`{"payload":{"reply_to":"payload-decoy"},"reply_to":"envelope-authority"}`,
		http.StatusCreated)

	both := h.poll(t, cr36Router, 30)
	require.Len(t, both.Messages, 1)
	wireReplyTo, present := cr36Message(both.Messages[0], "reply_to")
	require.True(t, present)
	require.JSONEq(t, `"envelope-authority"`, wireReplyTo,
		"the envelope correlation must not be displaced by a payload copy")
	entry = replyToPeek(t, h.store, cr36Router, cr36ID(t, both.Messages[0]))
	require.Equal(t, "envelope-authority", entry.ReplyTo)
}

// TestReplyCanBeFoundByItsEnvelopeReplyTo is brief AC2: the reply round trip.
// A dispatch is delivered to the router, the router answers the SCHEDULER with
// `reply_to` = the dispatch's Crier message id, and the scheduler recovers the
// answer from its own inbox alone by matching that field — no payload parsing,
// no shared worksheet.
func TestReplyCanBeFoundByItsEnvelopeReplyTo(t *testing.T) {
	t.Parallel()
	h := newCR36Harness(t, nil)
	h.register(t, cr36Scheduler)
	h.register(t, cr36Router)

	// The scheduler dispatches work.
	h.deliver(t, cr36Router,
		`{"payload":{"task":"summarize"},"sender":"`+cr36Scheduler+`"}`,
		http.StatusCreated)
	dispatched := h.poll(t, cr36Router, 30)
	require.Len(t, dispatched.Messages, 1)
	dispatchID := cr36ID(t, dispatched.Messages[0])

	// The router answers the scheduler, correlating by the dispatch id.
	h.deliver(t, cr36Scheduler,
		`{"payload":{"result":"summarized"},"sender":"`+cr36Router+`","reply_to":"`+dispatchID+`"}`,
		http.StatusCreated)

	// The scheduler's own inbox is enough to route the answer: find the message
	// whose reply_to is the dispatch it sent.
	answers := h.poll(t, cr36Scheduler, 30)
	require.Len(t, answers.Messages, 1)

	found := ""
	for _, msg := range answers.Messages {
		if raw, ok := cr36Message(msg, "reply_to"); ok {
			var correlation string
			require.NoError(t, json.Unmarshal([]byte(raw), &correlation))
			if correlation == dispatchID {
				found = cr36ID(t, msg)
			}
		}
	}
	require.NotEmpty(t, found,
		"the answer to dispatch %s must be findable by its reply_to field", dispatchID)
	require.Equal(t, dispatchID, replyToPeek(t, h.store, cr36Scheduler, found).ReplyTo,
		"the answer the scheduler found carries the dispatch id it answers")
}
