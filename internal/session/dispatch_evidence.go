package session

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// DispatchEvidence journals one fan-out pass, independently of the outcome
// write-back. StartedAt proves only entry to the dispatch boundary; CompletedAt
// and Receipts prove returned results, never remote execution. See §3.2's table.
type DispatchEvidence struct {
	Attempt          int               `json:"attempt"`
	StartedAt        time.Time         `json:"started_at"`
	CompletedAt      *time.Time        `json:"completed_at,omitempty"`
	Targets          []string          `json:"targets"`
	Receipts         []DeliveryOutcome `json:"receipts"`
	OutcomesRecorded bool              `json:"outcomes_recorded"`
}

// addReceipts keeps one result per target: the local fan-out also returns the
// remote results it reused, which must not become a second receipt or attempt.
func (e *DispatchEvidence) addReceipts(receipts []DeliveryOutcome) {
	byTarget := make(map[string]DeliveryOutcome, len(e.Receipts)+len(receipts))
	for _, receipt := range e.Receipts {
		byTarget[receipt.Target] = receipt
	}
	for _, receipt := range receipts {
		byTarget[receipt.Target] = receipt
	}
	e.Receipts = make([]DeliveryOutcome, 0, len(byTarget))
	for _, receipt := range byTarget {
		e.Receipts = append(e.Receipts, receipt)
	}
	sort.Slice(e.Receipts, func(i, j int) bool { return e.Receipts[i].Target < e.Receipts[j].Target })
}

func messageOutcomesIncomplete(m *Message) bool {
	if n := len(m.DispatchEvidence); n != 0 {
		e := m.DispatchEvidence[n-1]
		if !e.OutcomesRecorded {
			return true
		}
		// Even a completion marker cannot substitute for missing outcomes.
		// The sole empty-set exception is a proven zero-target pass.
		return len(m.Outcomes) == 0 && (e.CompletedAt == nil || len(e.Targets) != 0 || len(e.Receipts) != 0)
	}
	// Legacy intent-only records cannot prove whether dispatch ran. An empty
	// audience alone cannot prove it: older group rosters may have changed.
	return m.IdempotencyKey != "" && len(m.Outcomes) == 0
}

func replayAction(m *Message) string {
	if !messageOutcomesIncomplete(m) {
		return "replayed"
	}
	if n := len(m.DispatchEvidence); n != 0 {
		e := m.DispatchEvidence[n-1]
		if e.CompletedAt != nil {
			return "replayed_outcome_missing"
		}
	}
	return "replayed_outcome_unknown"
}

// appendDispatchEvidence uses the existing durable append primitive, not the
// final PostMessage write-back. Keep the original outcomes until that write
// succeeds; otherwise an interrupted negative retry could look retryable again.
// A disconnected caller must not cancel the receipt journal after dispatch.
func (h *Handler) appendDispatchEvidence(ctx context.Context, msg *Message, prior []DeliveryOutcome) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	rec := msg.Record()
	rec.Outcomes = prior
	if err := h.store.Append(ctx, rec); err != nil {
		return fmt.Errorf("message %q dispatch evidence unavailable; completed transport receipts cannot be guaranteed durable: %w", msg.ID, err)
	}
	return nil
}
