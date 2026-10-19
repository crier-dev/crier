package webhook

import (
	"fmt"
	"time"
)

// BlockingDeliveryError retains evidence from the existing per-target wait.
// BudgetExhausted alone is NOT a cut: a target repeatedly answering 503 refused
// the delivery, even if backoff consumed the budget. Cut denotes an incomplete
// timed-out exchange (including an upstream 408/504), not an endpoint refusal.
// Unwrap preserves ErrPermanent and context deadline/cancellation classification.
type BlockingDeliveryError struct {
	Attempts        int
	Budget          time.Duration
	BudgetExhausted bool
	Cut             bool
	Cause           error
}

func (e *BlockingDeliveryError) Unwrap() error { return e.Cause }

func (e *BlockingDeliveryError) Error() string {
	unit := "attempts"
	if e.Attempts == 1 {
		unit = "attempt"
	}
	wait := fmt.Sprintf("%d %s, %s delivery budget", e.Attempts, unit, e.Budget)
	if e.BudgetExhausted {
		wait += " exhausted"
	}
	if e.Cut {
		return fmt.Sprintf("answer did not arrive inside the budget: %s; answer cut, partial or late answer discarded. Raise RUNTIME_TIMEOUT_MS / CR_SESSION_DELIVERY_TIMEOUT_MS (runtime must remain strictly less than session) or retry. %v", wait, e.Cause)
	}
	return fmt.Sprintf("%s: %v", wait, e.Cause)
}
