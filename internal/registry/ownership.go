package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/namespace"
)

// This file is the HTTP and sweep side of task ownership (CR-FEAT-025): the
// expiry sweep that turns a silent purge into a dead letter plus one receipt,
// the dead-letter read path, and the transfer/reassign surface an operator uses
// to rebalance a stuck lease.
//
// The lease model already answers "who owns this message right now" — a
// retrieve claims it, an ack releases it, a lapse returns it. What it did not
// answer was what happens when nobody ever comes back for it, and what an
// operator can do about a consumer that is gone without waiting out a lease and
// racing every other consumer. These three surfaces answer that, inside the
// lease model rather than beside it: a dead letter is a message the lease model
// gave up on, and a transfer moves a message between two inboxes under the same
// lease rules (with one explicit, named override).

// Metrics owned by the ownership surfaces (DF-CRIER-142's series-ownership
// rule: the code that increments them registers them).
var (
	expiredMessagesTotal = metrics.Default.NewCounter("expired_messages_total",
		"Messages removed from an inbox by the TTL expiry sweep.")
	deadLetteredMessagesTotal = metrics.Default.NewCounter("dead_lettered_messages_total",
		"Expired messages recorded in a dead-letter destination.")
	expiryReceiptsTotal = metrics.Default.NewCounter("expiry_receipts_total",
		"MESSAGE_EXPIRED receipts written into a sender's inbox.")
	idempotentReplaysTotal = metrics.Default.NewCounter("idempotent_replays_total",
		"Deliveries answered from an idempotency key's recorded accept instead of storing a message.")
	transfersTotal = metrics.Default.NewCounter("transfers_total",
		"Messages moved between inboxes by a transfer/reassign.")
)

// transferRequest is the JSON body for POST /agents/{id}/inbox/transfer.
type transferRequest struct {
	// TargetAgentID is the destination inbox (required).
	TargetAgentID string `json:"target_agent_id"`
	// LeaseID is the lease the moved messages must currently be held under.
	// Required unless Force is set.
	LeaseID string `json:"lease_id,omitempty"`
	// MessageIDs are the messages to move. All of them move, or none does.
	MessageIDs []string `json:"message_ids"`
	// Force overrides the lease check — the operator escape hatch for a holder
	// that is gone. It must be asked for explicitly.
	Force bool `json:"force,omitempty"`
}

// transferResponse is the 200 body for a completed transfer.
type transferResponse struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Moved      int      `json:"moved"`
	MessageIDs []string `json:"message_ids"`
	Forced     bool     `json:"forced,omitempty"`
}

// deadLettersResponse is the 200 body of GET /agents/{id}/inbox/dead-letters.
type deadLettersResponse struct {
	AgentID     string        `json:"agent_id"`
	DeadLetters []*DeadLetter `json:"dead_letters"`
	// Count is the number of records in THIS response (the page), never a
	// total: the dead-letter destination has no meaningful global count for a
	// caller that asked for one agent.
	Count int `json:"count"`
}

// PurgeExpired runs the store's expiry sweep and turns each removed message
// into (a) a dead-letter record and (b) exactly one expiry receipt to its
// sender (CR-FEAT-025). It is what the server's periodic sweep calls; the
// store's own PurgeExpired remains the count-only contract.
//
// Exactly-once: the sweep hands each removed message to the report callback
// exactly once, and a record already present in the dead-letter destination
// suppresses the receipt for it — so a store that reports the same removal
// twice (or a sweep racing another sweep) cannot produce two receipts.
func (h *Handler) PurgeExpired() int {
	rep, ok := h.store.(PurgeReporter)
	if !ok {
		// A store that cannot report what it removed: the count is all anyone
		// gets, and the messages leave with no dead letter and no receipt.
		// Both shipped backends implement PurgeReporter, so this is the
		// contract for an embedding store, stated rather than hidden.
		return h.store.PurgeExpired()
	}
	return rep.PurgeExpiredReport(h.expireMessage)
}

// expireMessage is the sweep's report sink: one removed message becomes a dead
// letter (when the backend keeps one) and at most one receipt.
func (h *Handler) expireMessage(agentID string, entry *InboxEntry) {
	if entry == nil {
		return
	}
	expiredMessagesTotal.Inc()
	dl := newDeadLetter(agentID, entry, time.Now().UTC())

	recorded, archive := h.recordDeadLetter(dl)
	switch {
	case archive && !recorded:
		// This message id is already in the dead-letter destination: it was
		// reported before, so its receipt already went out. Sending another
		// would be a duplicate, and the contract is exactly one.
		slog.Warn("expiry sweep: message already dead-lettered, skipping receipt",
			"message_id", dl.MessageID, "target", dl.AgentID)
		return
	case !archive:
		// No dead-letter destination on this backend: nothing can deduplicate
		// repeated reports, so the receipt is sent on the sweep's own
		// exactly-once guarantee (one report per removal).
		slog.Warn("expiry sweep: store keeps no dead-letter destination; receipt sent without a retrievable record",
			"message_id", dl.MessageID, "target", dl.AgentID)
	}
	h.notifyExpiry(dl)
}

// recordDeadLetter stores dl when the backend keeps dead letters, reporting
// whether it was ADDED and whether the backend keeps an archive at all.
func (h *Handler) recordDeadLetter(dl *DeadLetter) (added, supported bool) {
	store, ok := h.store.(DeadLetterStore)
	if !ok {
		return false, false
	}
	added, err := store.AppendDeadLetter(dl)
	if err != nil {
		slog.Error("expiry sweep: record dead letter", "error", err,
			"message_id", dl.MessageID, "target", dl.AgentID)
		return false, true
	}
	if added {
		deadLetteredMessagesTotal.Inc()
	}
	return added, true
}

// notifyExpiry writes the MESSAGE_EXPIRED receipt into the sender's own inbox —
// the same direct store write the WEBHOOK_FAILED and FEDERATION_FAILED
// notifications use, so it cannot recurse through webhook or federation
// delivery and it persists under a durable backend exactly like any other inbox
// entry.
//
// Best-effort, like those sinks: an unregistered sender or a store failure is
// logged, never retried. A delivery that named no sender produces no receipt —
// there is no inbox to address it to — and the dead letter is the whole record.
// The receipt itself carries no sender, so a receipt that expires unacknowledged
// cannot produce a receipt of its own.
func (h *Handler) notifyExpiry(dl *DeadLetter) {
	if dl.Sender == "" {
		return
	}
	payload, err := expiryReceiptPayloadBytes(dl)
	if err != nil {
		slog.Error("expiry receipt: build payload", "error", err, "message_id", dl.MessageID)
		return
	}
	if err := h.store.Deliver(dl.Sender, &InboxEntry{Payload: payload}); err != nil {
		slog.Warn("expiry receipt: deliver to sender inbox", "error", err,
			"sender", dl.Sender, "message_id", dl.MessageID, "target", dl.AgentID)
		return
	}
	expiryReceiptsTotal.Inc()
}

// parseDeadLetterLimit reads the optional `limit` page size of a dead-letter
// read. Absent is the documented default; a present value is honored or
// REJECTED (never silently clamped), so a caller asking for 500 learns the
// bound instead of quietly receiving 100.
func parseDeadLetterLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return DefaultDeadLetterListLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("limit must be an integer")
	}
	if n < 1 || n > MaxDeadLetterListLimit {
		return 0, fmt.Errorf("limit must be 1..%d", MaxDeadLetterListLimit)
	}
	return n, nil
}

// HandleDeadLetters handles GET /agents/{id}/inbox/dead-letters — the messages
// that expired unacknowledged in this agent's inbox, newest first.
//
// Agent-owned, exactly like the other inbox routes: the caller must be the
// agent (or hold the relay token). The agent ROW need not still exist — a dead
// letter outlives the registration it was addressed to, which is precisely when
// it matters — and answering from the dead-letter destination alone keeps that
// true.
func (h *Handler) HandleDeadLetters(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	limit, err := parseDeadLetterLimit(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	store, ok := h.store.(DeadLetterStore)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "registry store does not keep a dead-letter destination",
		})
		return
	}
	records, err := store.ListDeadLetters(id, limit)
	if err != nil {
		if errors.Is(err, ErrInvalidStoreInput) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeStoreError(w, err)
		return
	}
	if records == nil {
		records = []*DeadLetter{}
	}
	writeJSON(w, http.StatusOK, deadLettersResponse{
		AgentID:     id,
		DeadLetters: records,
		Count:       len(records),
	})
}

// HandleTransfer handles POST /agents/{id}/inbox/transfer — moving messages
// from this inbox into another agent's (CR-FEAT-025).
//
// This is the rebalance surface for a stuck lease: a foreman whose worker took
// a message and never acked it can move the work to a live worker instead of
// waiting for the lease to lapse. Called on the CURRENT holder's inbox, so the
// agent-owned signature gate applies to the holder — an operator acting on its
// behalf uses the shared relay token (the same authorization the other inbox
// routes accept).
//
// Contract:
//   - `target_agent_id` (required) names the destination inbox; it must be a
//     registered agent and must differ from this one;
//   - `message_ids` (required, non-empty) are moved as a unit — an unknown id
//     moves nothing and answers 404;
//   - `lease_id` must match the lease a LEASED message is held under (409
//     otherwise), which keeps the lease a real lock. An unleased message needs
//     no lease: nobody owns it, and it is claimable by any consumer anyway, so
//     moving it displaces nobody;
//   - `force: true` is the explicit operator override for a holder that is
//     gone: it moves the messages whatever their lease state. It is opt-in on
//     every single call, never a default;
//   - moved messages keep their id, payload, creation and expiry and land
//     UNLEASED, so the destination can claim them immediately.
func (h *Handler) HandleTransfer(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	// Validate before touching the store: the answer must not depend on the
	// lease's current state.
	switch {
	case req.TargetAgentID == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_agent_id is required"})
		return
	case req.TargetAgentID == id:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "target_agent_id must differ from the source inbox"})
		return
	case len(req.MessageIDs) == 0:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message_ids is required (a transfer with no messages moves nothing)"})
		return
	}

	// Realms (CR-FEAT-029): a transfer is a MOVE of live messages between two
	// inboxes, and moving one into another realm's inbox would be exactly the
	// implicit crossing this feature forbids — the message would be readable by
	// an agent whose realm never agreed to hold it. Both rows are read first;
	// an unknown agent is left to the store's own error reporting (the same 404
	// every other transfer shape gets), and only a pair of KNOWN agents in
	// DIFFERENT realms is refused here.
	if source, srcErr := h.store.Get(id); srcErr == nil {
		if dest, dstErr := h.store.Get(req.TargetAgentID); dstErr == nil {
			if from, to := namespaceOfAgent(source), namespaceOfAgent(dest); from != to {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "NAMESPACE_MISMATCH",
					"detail": fmt.Sprintf("inbox transfer cannot cross namespaces: source is in %q, target is in %q",
						namespace.Display(from), namespace.Display(to)),
				})
				return
			}
		}
	}

	store, ok := h.store.(Transferrer)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "registry store does not support transfers",
		})
		return
	}

	moved, err := store.Transfer(id, req.LeaseID, req.MessageIDs, req.TargetAgentID, req.Force)
	if err != nil {
		switch {
		case errors.Is(err, ErrAgentNotFound), errors.Is(err, ErrMessageNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrLeaseConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrInvalidStoreInput):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		default:
			writeStoreError(w, err)
		}
		return
	}

	transfersTotal.Add(float64(moved))
	slog.Info("inbox transfer",
		"from", id,
		"to", req.TargetAgentID,
		"moved", moved,
		"forced", req.Force,
	)
	// The moved messages are durable in the destination, so this is the same
	// fire-and-forget wake the delivery path fires (CR-FEAT-023): reads parked
	// on either inbox are released — the source's because its queue changed,
	// the destination's because it just gained work.
	h.wakeLongPolls(id)
	h.wakeLongPolls(req.TargetAgentID)
	h.pingInbox(req.TargetAgentID, req.MessageIDs[0], "")

	writeJSON(w, http.StatusOK, transferResponse{
		From:       id,
		To:         req.TargetAgentID,
		Moved:      moved,
		MessageIDs: req.MessageIDs,
		Forced:     req.Force,
	})
}
