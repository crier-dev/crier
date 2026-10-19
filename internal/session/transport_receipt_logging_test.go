package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReceiptWarningsDoNotClaimOtherTargetsDelivered(t *testing.T) {
	logs := captureFanoutWarnings(t)
	warnSkippedTargets("probe", []DeliveryOutcome{
		{Target: "queue", Outcome: OutcomeAccepted},
		{Target: "hold", Outcome: OutcomeHeld},
	}, nil)
	require.Empty(t, logs.String(), "queue/hold receipts are not delivery refusals")
	warnSkippedTargets("probe", []DeliveryOutcome{{Target: "ghost", Outcome: OutcomeRefused, Reason: "agent not found"}}, nil)
	require.Contains(t, logs.String(), "other targets retain their own outcomes")
	require.NotContains(t, logs.String(), "still delivered to every other target")
}
