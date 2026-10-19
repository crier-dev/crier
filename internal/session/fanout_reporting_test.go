package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSummarizeFanoutExactOutcome(t *testing.T) {
	for _, outcome := range []Outcome{OutcomeDelivered, OutcomeLeased, OutcomeAcked, OutcomeExpired, OutcomeRefused, OutcomeFailed, OutcomeCanned, OutcomeBudgetCut, OutcomeUnreadableReply, "future"} {
		t.Run(string(outcome), func(t *testing.T) {
			var view transcriptMessage
			summarizeFanout(&view, []DeliveryOutcome{{Target: "agent", Outcome: outcome}})
			if outcome == OutcomeDelivered {
				require.Equal(t, 1, view.Delivered)
			} else {
				require.Zero(t, view.Delivered, "%s is not delivered", outcome)
			}
			if outcome == OutcomeRefused {
				require.Equal(t, 1, view.Skipped)
				require.Equal(t, []string{"agent"}, view.SkippedAgents)
			} else {
				require.Zero(t, view.Skipped)
			}
		})
	}
}
