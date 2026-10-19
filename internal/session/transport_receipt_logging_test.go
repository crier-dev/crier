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
	require.Contains(t, logs.String(), "refused delivery", "a real refusal is still named a refusal")

	// DF-CRIER-398: leased/acked/expired are NOT refusals. They must be logged
	// with their own outcome name, never as a refusal, and never with a claim
	// about targets this function does not inspect.
	logs.Reset()
	warnSkippedTargets("probe", []DeliveryOutcome{
		{Target: "lease", Outcome: OutcomeLeased, Reason: "another delivery holds the lease"},
		{Target: "ack", Outcome: OutcomeAcked},
		{Target: "ttl", Outcome: OutcomeExpired},
	}, nil)
	got := logs.String()
	require.NotContains(t, got, "refused", "a lease, ack or expiry is not a refusal")
	require.NotContains(t, got, "delivered to every other target")
	for _, name := range []string{"leased", "acked", "expired"} {
		require.Contains(t, got, name, "the log names the outcome it saw")
	}
}
