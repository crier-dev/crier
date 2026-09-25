package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// CR-FEAT-025 — a TTL expiry is an outcome, not a mystery.
//
// Before this, PurgeExpired returned a count and the message was gone: no
// record of the body, no way for the sender to learn its work had died. Now
// every message the sweep removes is (a) recorded in a dead-letter destination
// with the payload that was delivered and (b) reported to its sender as exactly
// one MESSAGE_EXPIRED receipt, in the same shape the house uses for
// WEBHOOK_FAILED and FEDERATION_FAILED. The exactly-once rule is load-bearing:
// an expiry must not produce a receipt storm.

// ownershipStore is the narrowest store shape the handler needs to be wired
// with a fake backend, so a test can prove what happens when the backend keeps
// no dead-letter destination. Embedding the Store interface means the wrapper
// exposes ONLY the Store methods — the optional capabilities (PurgeReporter,
// DeadLetterStore, Transferrer) are invisible, exactly like a third-party
// backend that never implemented them.
type ownershipStore struct {
	Store
}

// expiredEntry builds a message that is already past its expiry. An explicit
// ExpiresAt is honoured by both backends (resolveMessageExpiry lets a supplied
// instant win), and the creation time precedes it, which is the ordering every
// backend requires.
func expiredEntry(t *testing.T, store Store, agentID, messageID, payload, sender string) *InboxEntry {
	t.Helper()
	entry := &InboxEntry{
		ID:        messageID,
		Payload:   []byte(payload),
		Sender:    sender,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	require.NoError(t, store.Deliver(agentID, entry))
	return entry
}

// receiptBody decodes the MESSAGE_EXPIRED payload from a sender's inbox.
type receiptBody struct {
	Kind           string `json:"kind"`
	Code           string `json:"code"`
	MessageID      string `json:"message_id"`
	Target         string `json:"target"`
	Sender         string `json:"sender"`
	DeadLettered   bool   `json:"dead_lettered"`
	DeadLetterPath string `json:"dead_letter_path"`
	Reason         string `json:"reason"`
	ExpiresAt      string `json:"expires_at"`
}

// senderInbox returns the payloads sitting in an agent's inbox, without
// claiming a lease on them for long (the lease is irrelevant here).
func senderInbox(t *testing.T, store Store, agentID string) []receiptBody {
	t.Helper()
	msgs, _, err := store.Retrieve(agentID, 30*time.Second, 100)
	require.NoError(t, err)
	out := make([]receiptBody, 0, len(msgs))
	for _, m := range msgs {
		var body receiptBody
		require.NoError(t, json.Unmarshal(m.Payload, &body), "payload: %s", m.Payload)
		out = append(out, body)
	}
	return out
}

// deadLetters fetches the destination over HTTP — the path an operator uses —
// so a change that recorded the record but broke the route fails here.
func deadLetters(t *testing.T, router http.Handler, agentID string) []*DeadLetter {
	t.Helper()
	rec := doInboxRequest(t, router, http.MethodGet, "/agents/"+agentID+"/inbox/dead-letters", "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp struct {
		DeadLetters []*DeadLetter `json:"dead_letters"`
		Count       int           `json:"count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	require.Equal(t, len(resp.DeadLetters), resp.Count, "count is this page's size")
	return resp.DeadLetters
}

// ---------------------------------------------------------------------------
// An expired message is dead-lettered AND receipted
// ---------------------------------------------------------------------------

func TestPurgeExpired_ExpiredMessageIsDeadLetteredAndReceipted(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "foreman")
	handler, router := setupRouterWithHandler(store)

	expiredEntry(t, store, "agent-1", "msg-dead", `{"task":"build"}`, "foreman")

	require.Equal(t, 1, handler.PurgeExpired(), "the sweep removes the expired message")

	// The target's inbox no longer holds it...
	msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Empty(t, msgs, "an expired message is not retrievable")

	// ...the dead-letter destination does, with the delivered payload...
	records := deadLetters(t, router, "agent-1")
	require.Len(t, records, 1)
	require.Equal(t, "msg-dead", records[0].MessageID)
	require.Equal(t, "agent-1", records[0].AgentID, "the record belongs to the inbox it died in")
	require.Equal(t, "foreman", records[0].Sender)
	require.JSONEq(t, `{"task":"build"}`, string(records[0].Payload))
	require.Equal(t, DeadLetterReasonExpired, records[0].Reason)
	require.False(t, records[0].ExpiredAt.IsZero())
	require.False(t, records[0].DeadLetteredAt.IsZero())

	// ...and the sender was told, exactly once.
	receipts := senderInbox(t, store, "foreman")
	require.Len(t, receipts, 1)
	require.Equal(t, "error", receipts[0].Kind)
	require.Equal(t, CodeMessageExpired, receipts[0].Code)
	require.Equal(t, "msg-dead", receipts[0].MessageID)
	require.Equal(t, "agent-1", receipts[0].Target)
	require.True(t, receipts[0].DeadLettered, "the receipt says the body is retrievable")
	require.Equal(t, DeadLettersPath("agent-1"), receipts[0].DeadLetterPath)
	require.NotEmpty(t, receipts[0].ExpiresAt)
}

func TestPurgeExpired_IsExactlyOnceEvenIfTheStoreReportsTwice(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "foreman")
	handler, router := setupRouterWithHandler(store)

	entry := expiredEntry(t, store, "agent-1", "msg-once", `{"n":1}`, "foreman")

	// The sweep reports each removal once, but the exactly-once rule must not
	// depend on that: the dead-letter destination is keyed by message id, so a
	// second report of the same message cannot produce a second receipt.
	handler.expireMessage("agent-1", entry)
	handler.expireMessage("agent-1", entry)

	require.Len(t, deadLetters(t, router, "agent-1"), 1, "one message, one record")
	require.Len(t, senderInbox(t, store, "foreman"), 1, "one message, one receipt")
}

func TestPurgeExpired_SecondSweepFindsNothingToReport(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "foreman")
	handler, router := setupRouterWithHandler(store)

	expiredEntry(t, store, "agent-1", "msg-dead", `{"n":1}`, "foreman")

	require.Equal(t, 1, handler.PurgeExpired())
	require.Equal(t, 0, handler.PurgeExpired(), "the message is gone, so the second sweep counts nothing")
	require.Len(t, deadLetters(t, router, "agent-1"), 1)
	require.Len(t, senderInbox(t, store, "foreman"), 1)
}

func TestPurgeExpired_SenderlessMessageIsStillDeadLettered(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	handler, router := setupRouterWithHandler(store)

	expiredEntry(t, store, "agent-1", "msg-anon", `{"n":1}`, "")

	require.Equal(t, 1, handler.PurgeExpired())
	records := deadLetters(t, router, "agent-1")
	require.Len(t, records, 1, "a senderless message is still preserved")
	require.Empty(t, records[0].Sender)
}

func TestPurgeExpired_UnknownSenderIsStillDeadLettered(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	handler, router := setupRouterWithHandler(store)

	// The sender was never registered (or has since gone). The receipt cannot
	// be delivered, but that must not cost the record.
	expiredEntry(t, store, "agent-1", "msg-orphan", `{"n":1}`, "ghost")

	require.Equal(t, 1, handler.PurgeExpired())
	require.Len(t, deadLetters(t, router, "agent-1"), 1)
}

func TestPurgeExpired_AckedMessageIsNotDeadLettered(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "foreman")
	handler, router := setupRouterWithHandler(store)

	// A message that was taken and ACKED — the work was done. ACK removes it,
	// so there is nothing for the sweep to expire and nothing to report: a
	// receipt here would be a false alarm about completed work.
	entry := &InboxEntry{
		ID:        "msg-acked",
		Payload:   []byte(`{"n":1}`),
		Sender:    "foreman",
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	require.NoError(t, store.Deliver("agent-1", entry))
	// An already-expired message cannot be claimed, so the ack path is
	// exercised on a live one and the expiry is applied afterwards.
	live := &InboxEntry{
		ID:        "msg-live",
		Payload:   []byte(`{"n":2}`),
		Sender:    "foreman",
		CreatedAt: time.Now().Add(-time.Minute),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, store.Deliver("agent-1", live))
	msgs, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "msg-live", msgs[0].ID)
	require.NoError(t, store.Ack("agent-1", leaseID, []string{"msg-live"}))

	// Only the expired one is swept...
	require.Equal(t, 1, handler.PurgeExpired())
	records := deadLetters(t, router, "agent-1")
	require.Len(t, records, 1)
	require.Equal(t, "msg-acked", records[0].MessageID,
		"the acked message never reaches the destination")
	require.Len(t, senderInbox(t, store, "foreman"), 1, "one expiry, one receipt")
}

func TestPurgeExpired_NeverExpiringMessageSurvives(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "foreman")
	handler, router := setupRouterWithHandler(store)

	never := 0
	entry := &InboxEntry{
		ID:         "msg-forever",
		Payload:    []byte(`{"n":1}`),
		Sender:     "foreman",
		CreatedAt:  time.Now().Add(-100 * time.Hour),
		TTLSeconds: &never,
	}
	require.NoError(t, store.Deliver("agent-1", entry))
	require.True(t, entry.ExpiresAt.IsZero(), "ttl_seconds=0 means never expires")

	require.Equal(t, 0, handler.PurgeExpired())
	msgs, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1, "a never-expiring message outlives every sweep")
	require.Empty(t, deadLetters(t, router, "agent-1"))
}

func TestPurgeExpired_ReceiptCannotRecurse(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "foreman")
	handler, router := setupRouterWithHandler(store)

	expiredEntry(t, store, "agent-1", "msg-dead", `{"n":1}`, "foreman")
	require.Equal(t, 1, handler.PurgeExpired())

	// The receipt itself expires. It has no sender recorded, so its own expiry
	// dead-letters it but cannot produce a receipt of a receipt.
	receipts, _, err := store.Retrieve("foreman", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, receipts, 1)
	receipts[0].ExpiresAt = time.Now().Add(-time.Minute)
	receipts[0].CreatedAt = time.Now().Add(-2 * time.Minute)

	require.Equal(t, 1, handler.PurgeExpired())
	require.Len(t, deadLetters(t, router, "foreman"), 1)
	require.Empty(t, senderInbox(t, store, "foreman"), "a receipt cannot produce a receipt")
}

// ---------------------------------------------------------------------------
// The dead-letter read path
// ---------------------------------------------------------------------------

func TestDeadLetters_NewestFirstAndBounded(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	handler, router := setupRouterWithHandler(store)

	for i, id := range []string{"m-old", "m-mid", "m-new"} {
		entry := &InboxEntry{
			ID:        id,
			Payload:   []byte(`{"n":1}`),
			CreatedAt: time.Now().Add(-2 * time.Hour),
			ExpiresAt: time.Now().Add(-time.Hour),
		}
		require.NoError(t, store.Deliver("agent-1", entry))
		// Distinct dead-lettered instants so the ORDER is deterministic.
		handler.expireMessage("agent-1", entry)
		_ = i
	}

	records := deadLetters(t, router, "agent-1")
	require.Len(t, records, 3)
	require.True(t, !records[0].DeadLetteredAt.Before(records[1].DeadLetteredAt) &&
		!records[1].DeadLetteredAt.Before(records[2].DeadLetteredAt),
		"newest first: %v", []time.Time{records[0].DeadLetteredAt, records[1].DeadLetteredAt, records[2].DeadLetteredAt})

	rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox/dead-letters?limit=1", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		DeadLetters []*DeadLetter `json:"dead_letters"`
		Count       int           `json:"count"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.DeadLetters, 1)
	require.Equal(t, 1, resp.Count)
}

func TestDeadLetters_EmptyIsAnArrayNotNull(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	_, router := setupRouterWithHandler(store)

	rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox/dead-letters", "")
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"dead_letters":[]`,
		"an empty destination is an empty array, never null: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"count":0`)
}

func TestDeadLetters_LimitIsHonoredOrRejected(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	_, router := setupRouterWithHandler(store)

	for _, bad := range []string{"0", "-1", "101", "abc", "1.5"} {
		rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox/dead-letters?limit="+bad, "")
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"limit=%s must be rejected, not clamped — body: %s", bad, rec.Body.String())
	}
	for _, ok := range []string{"1", "20", "100"} {
		rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox/dead-letters?limit="+ok, "")
		require.Equal(t, http.StatusOK, rec.Code, "limit=%s — body: %s", ok, rec.Body.String())
	}
}

func TestDeadLetters_OutliveTheAgentRow(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	handler, router := setupRouterWithHandler(store)

	expiredEntry(t, store, "agent-1", "msg-dead", `{"n":1}`, "")
	require.Equal(t, 1, handler.PurgeExpired())

	// The consumer is gone — which is precisely the case a dead letter exists
	// for, so unregistering must not take the record with it.
	require.NoError(t, store.Unregister("agent-1"))

	records := deadLetters(t, router, "agent-1")
	require.Len(t, records, 1, "a dead letter outlives the registration it was addressed to")
	require.Equal(t, "msg-dead", records[0].MessageID)
}

func TestDeadLetters_StoreWithoutADestinationAnswers501(t *testing.T) {
	base := NewMemoryStore()
	fake := &ownershipStore{Store: base}
	registerTestAgent(t, fake)
	handler, router := setupRouterWithHandler(fake)

	// The receipt still goes out (a sender must not lose the notification
	// because the backend keeps no archive), and the read path says so instead
	// of answering an empty array as if nothing had happened.
	expiredEntry(t, fake, "agent-1", "msg-dead", `{"n":1}`, "foreman")
	require.Equal(t, 1, handler.PurgeExpired())

	rec := doInboxRequest(t, router, http.MethodGet, "/agents/agent-1/inbox/dead-letters", "")
	require.Equal(t, http.StatusNotImplemented, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "dead-letter")
}

func TestPurgeExpired_StoreWithoutAReporterFallsBackToTheCount(t *testing.T) {
	base := NewMemoryStore()
	fake := &ownershipStore{Store: base}
	registerTestAgent(t, fake)
	handler := NewHandler(fake)

	expiredEntry(t, fake, "agent-1", "msg-dead", `{"n":1}`, "foreman")
	require.Equal(t, 1, handler.PurgeExpired(),
		"a store that cannot report still purges: the count is the whole contract for it")
}

// ---------------------------------------------------------------------------
// Transfer / reassign — the stuck-lease rebalance
// ---------------------------------------------------------------------------

func transferBody(target, lease string, ids []string, force bool) string {
	body := map[string]any{"target_agent_id": target, "message_ids": ids}
	if lease != "" {
		body["lease_id"] = lease
	}
	if force {
		body["force"] = true
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func postTransfer(t *testing.T, router http.Handler, from, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doInboxRequest(t, router, http.MethodPost, "/agents/"+from+"/inbox/transfer", body)
}

func TestTransfer_MovesAStuckLeaseToAnotherInbox(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "worker-b")
	_, router := setupRouterWithHandler(store)

	entry := &InboxEntry{ID: "msg-stuck", Payload: []byte(`{"task":"build"}`), Sender: "foreman",
		CreatedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour), IdempotencyKey: "k-1"}
	require.NoError(t, store.Deliver("agent-1", entry))

	// agent-1 takes the work and never acks it: the lease is stuck.
	_, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)
	require.NotEmpty(t, leaseID)

	rec := postTransfer(t, router, "agent-1", transferBody("worker-b", leaseID, []string{"msg-stuck"}, false))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp transferResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "agent-1", resp.From)
	require.Equal(t, "worker-b", resp.To)
	require.Equal(t, 1, resp.Moved)
	require.Equal(t, []string{"msg-stuck"}, resp.MessageIDs)

	// The source is empty...
	depth, _, _, err := store.Stats("agent-1")
	require.NoError(t, err)
	require.Zero(t, depth, "the message left the stuck inbox")

	// ...and the destination holds it UNLEASED, claimable right away, with its
	// identity and payload intact.
	msgs, newLease, err := store.Retrieve("worker-b", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotEmpty(t, newLease, "the moved message is claimable by the destination")
	require.Equal(t, "msg-stuck", msgs[0].ID, "the id is unchanged, so a lease or receipt naming it still resolves")
	require.Equal(t, "worker-b", msgs[0].AgentID)
	require.JSONEq(t, `{"task":"build"}`, string(msgs[0].Payload))
	require.Equal(t, "foreman", msgs[0].Sender, "the sender travels with the message")
	require.Equal(t, "k-1", msgs[0].IdempotencyKey)
	require.Equal(t, entry.ExpiresAt.UTC(), msgs[0].ExpiresAt.UTC(), "the expiry is unchanged")

	// The old holder can no longer ack work it no longer holds.
	err = store.Ack("agent-1", leaseID, []string{"msg-stuck"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrMessageNotFound)
}

func TestTransfer_WrongLeaseMovesNothing(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "worker-b")
	_, router := setupRouterWithHandler(store)

	entry := &InboxEntry{ID: "msg-1", Payload: []byte(`{"n":1}`), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, store.Deliver("agent-1", entry))
	_, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)

	rec := postTransfer(t, router, "agent-1", transferBody("worker-b", "not-the-lease", []string{"msg-1"}, false))
	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), leaseID, "the body reports the lease that actually holds it")

	depth, _, _, err := store.Stats("agent-1")
	require.NoError(t, err)
	require.Equal(t, 1, depth, "a rejected transfer moves nothing")
	otherDepth, _, _, err := store.Stats("worker-b")
	require.NoError(t, err)
	require.Zero(t, otherDepth)
}

func TestTransfer_ForceOverridesAStuckLeaseWithoutItsID(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "worker-b")
	_, router := setupRouterWithHandler(store)

	entry := &InboxEntry{ID: "msg-1", Payload: []byte(`{"n":1}`), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, store.Deliver("agent-1", entry))
	_, _, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)

	// The holder is GONE: the operator has no lease ID, and says so explicitly.
	rec := postTransfer(t, router, "agent-1", transferBody("worker-b", "", []string{"msg-1"}, true))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var resp transferResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.True(t, resp.Forced)

	msgs, _, err := store.Retrieve("worker-b", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "msg-1", msgs[0].ID)
}

func TestTransfer_UnleasedMessageNeedsNoLeaseOrForce(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "worker-b")
	_, router := setupRouterWithHandler(store)

	// Nobody ever retrieved it: it is claimable by any consumer, so moving it
	// displaces nobody and needs no override.
	entry := &InboxEntry{ID: "msg-free", Payload: []byte(`{"n":1}`), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, store.Deliver("agent-1", entry))

	rec := postTransfer(t, router, "agent-1", transferBody("worker-b", "", []string{"msg-free"}, false))
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	msgs, _, err := store.Retrieve("worker-b", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "msg-free", msgs[0].ID)
}

func TestTransfer_UnknownMessageMovesNothing(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "worker-b")
	_, router := setupRouterWithHandler(store)

	entry := &InboxEntry{ID: "msg-1", Payload: []byte(`{"n":1}`), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, store.Deliver("agent-1", entry))
	_, leaseID, err := store.Retrieve("agent-1", 30*time.Second, 10)
	require.NoError(t, err)

	// One known and one unknown id: the batch moves as a unit, so nothing does.
	rec := postTransfer(t, router, "agent-1", transferBody("worker-b", leaseID, []string{"msg-1", "ghost"}, false))
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "ghost")

	msgs, _, err := store.Retrieve("worker-b", 30*time.Second, 10)
	require.NoError(t, err)
	require.Empty(t, msgs)
}

func TestTransfer_UnknownTargetAgentIs404(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	_, router := setupRouterWithHandler(store)

	entry := &InboxEntry{ID: "msg-1", Payload: []byte(`{"n":1}`), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, store.Deliver("agent-1", entry))

	rec := postTransfer(t, router, "agent-1", transferBody("nobody", "", []string{"msg-1"}, false))
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())

	depth, _, _, err := store.Stats("agent-1")
	require.NoError(t, err)
	require.Equal(t, 1, depth, "a transfer to nowhere leaves the message where it was")
}

func TestTransfer_RejectsInvalidRequests(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	registerAgentAt(t, store, "worker-b")
	_, router := setupRouterWithHandler(store)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing target", `{"message_ids":["m"]}`, "target_agent_id is required"},
		{"target is the source", transferBody("agent-1", "", []string{"m"}, true), "must differ from the source"},
		{"no messages", transferBody("worker-b", "", []string{}, false), "message_ids is required"},
		{"not json", `{`, "invalid json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postTransfer(t, router, "agent-1", tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.want)
		})
	}
}

func TestTransfer_StoreWithoutTheCapabilityAnswers501(t *testing.T) {
	store := &ownershipStore{Store: NewMemoryStore()}
	registerTestAgent(t, store)
	_, router := setupRouterWithHandler(store)

	rec := postTransfer(t, router, "agent-1", transferBody("worker-b", "", []string{"m"}, true))
	require.Equal(t, http.StatusNotImplemented, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "transfer")
}

// ---------------------------------------------------------------------------
// Store-level contracts (the handler is only one caller)
// ---------------------------------------------------------------------------

func TestMemoryStore_DeadLetterAppendIsIdempotent(t *testing.T) {
	store := NewMemoryStore()
	registerTestAgent(t, store)

	rec := &DeadLetter{
		MessageID:      "m-1",
		AgentID:        "agent-1",
		Payload:        []byte(`{"n":1}`),
		CreatedAt:      time.Now().Add(-time.Hour),
		ExpiredAt:      time.Now(),
		DeadLetteredAt: time.Now(),
		Reason:         DeadLetterReasonExpired,
	}
	added, err := store.AppendDeadLetter(rec)
	require.NoError(t, err)
	require.True(t, added)

	added, err = store.AppendDeadLetter(rec)
	require.NoError(t, err)
	require.False(t, added, "a message id is recorded once")

	list, err := store.ListDeadLetters("agent-1", 0)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestMemoryStore_DeadLetterValidationAndBounds(t *testing.T) {
	store := NewMemoryStore()
	registerTestAgent(t, store)

	_, err := store.AppendDeadLetter(&DeadLetter{AgentID: "agent-1", Reason: "x", DeadLetteredAt: time.Now()})
	require.ErrorIs(t, err, ErrInvalidStoreInput, "a record without a message id is refused")

	_, err = store.ListDeadLetters("", 0)
	require.ErrorIs(t, err, ErrInvalidStoreInput)

	_, err = store.ListDeadLetters("agent-1", MaxDeadLetterListLimit+1)
	require.ErrorIs(t, err, ErrInvalidStoreInput, "an over-large page is refused, never clamped")
}

func TestMemoryStore_DeadLetterCapacityEvictsOldest(t *testing.T) {
	store := NewMemoryStore()
	store.deadLetters = newMemoryDeadLetters(2)

	for _, id := range []string{"m-1", "m-2", "m-3"} {
		added, err := store.AppendDeadLetter(&DeadLetter{
			MessageID: id, AgentID: "agent-1", Payload: []byte(`{}`),
			CreatedAt: time.Now(), ExpiredAt: time.Now(), DeadLetteredAt: time.Now(),
			Reason: DeadLetterReasonExpired,
		})
		require.NoError(t, err)
		require.True(t, added)
	}

	list, err := store.ListDeadLetters("agent-1", 10)
	require.NoError(t, err)
	require.Len(t, list, 2, "the in-memory destination is bounded")
	require.Equal(t, "m-3", list[0].MessageID, "newest first")
	require.Equal(t, "m-2", list[1].MessageID, "the oldest record was evicted")
}

func TestMemoryStore_TransferValidation(t *testing.T) {
	store := NewMemoryStore()
	registerTestAgent(t, store)
	registerAgentAt(t, store, "worker-b")

	cases := []struct {
		name            string
		from, to, lease string
		ids             []string
		force           bool
		want            error
	}{
		{"blank source", "", "worker-b", "", []string{"m"}, true, ErrInvalidStoreInput},
		{"blank target", "agent-1", "", "", []string{"m"}, true, ErrInvalidStoreInput},
		{"same inbox", "agent-1", "agent-1", "", []string{"m"}, true, ErrInvalidStoreInput},
		{"no ids", "agent-1", "worker-b", "", nil, true, ErrInvalidStoreInput},
		{"duplicate ids", "agent-1", "worker-b", "", []string{"m", "m"}, true, ErrInvalidStoreInput},
		{"unknown source", "ghost", "worker-b", "", []string{"m"}, true, ErrAgentNotFound},
		{"unknown target", "agent-1", "ghost", "", []string{"m"}, true, ErrAgentNotFound},
		{"unknown message", "agent-1", "worker-b", "", []string{"m"}, true, ErrMessageNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Transfer(tc.from, tc.lease, tc.ids, tc.to, tc.force)
			require.Error(t, err)
			require.ErrorIs(t, err, tc.want, "got %v", err)
		})
	}
}
