package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSummarizeFanoutExactOutcome(t *testing.T) {
	t.Parallel()
	for _, outcome := range []Outcome{OutcomeDelivered, OutcomeAccepted, OutcomeHeld, OutcomeLeased, OutcomeAcked, OutcomeExpired, OutcomeRefused, OutcomeFailed, OutcomeCanned, OutcomeBudgetCut, OutcomeUnreadableReply, "future"} {
		t.Run(string(outcome), func(t *testing.T) {
			var view transcriptMessage
			summarizeFanout(&view, []DeliveryOutcome{{Target: "agent", Outcome: outcome}})
			require.Equal(t, map[Outcome]int{outcome: 1}, view.OutcomeCounts)
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
