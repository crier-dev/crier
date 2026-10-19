package session

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
)

type budgetDeliverer struct {
	calls       atomic.Int32
	budget      int
	wrongBudget atomic.Bool
}

func (d *budgetDeliverer) Deliver(string, *registry.InboxEntry) error {
	panic("fan-out must use the context-aware dispatcher")
}

func (d *budgetDeliverer) DispatchDelivery(_ context.Context, _ string, req registry.DeliveryRequest) (registry.DeliveryResult, error) {
	d.calls.Add(1)
	if req.TimeoutMs != d.budget {
		d.wrongBudget.Store(true)
	}
	return registry.DeliveryResult{}, nil
}

func TestFanoutPassesBudgetAndPreservesPriorOutcomes(t *testing.T) {
	for _, budget := range []int{0, 250} {
		t.Run(strconv.Itoa(budget), func(t *testing.T) {
			wantBudget := budget
			if wantBudget == 0 {
				wantBudget = registry.DefaultSessionDeliveryTimeoutMs
			}
			d := &budgetDeliverer{budget: wantBudget}
			prior := DeliveryOutcome{Target: "b", Outcome: OutcomeRefused, Reason: "original refusal"}
			msg := &Message{ID: "message", SessionID: "session", ThreadID: "thread", Payload: []byte(`{}`), Outcomes: []DeliveryOutcome{prior}}
			outcomes, err := fanoutWithTimeout(context.Background(), d, msg, []string{"c", "b", "a", "c"}, nil, budget)
			require.NoError(t, err)
			require.EqualValues(t, 2, d.calls.Load(), "prior outcome and duplicate must not dispatch")
			require.False(t, d.wrongBudget.Load(), "the configured/default budget must reach DeliveryRequest")
			require.Len(t, outcomes, 3)
			require.Equal(t, "a", outcomes[0].Target)
			require.Equal(t, prior, outcomes[1])
			require.Equal(t, "c", outcomes[2].Target)
			require.Equal(t, outcomes, msg.Outcomes)
		})
	}
}
