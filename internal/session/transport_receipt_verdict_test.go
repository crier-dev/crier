package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransportReceiptVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name, body, verdict, inbound string
		want                         Outcome
		code                         string
	}{
		{"empty", "", "", "", OutcomeAccepted, ""},
		{"malformed", `{`, "", "", OutcomeAccepted, ""},
		{"unknown", `{"status":"completed"}`, "", "", OutcomeAccepted, ""},
		{"authorization", `{"verdict":"FED_ALLOWED"}`, "FED_ALLOWED", "FED_ALLOWED", OutcomeAccepted, ""},
		{"status", `{"status":" DELIVERED "}`, "", "", OutcomeDelivered, ""},
		{"outcome", `{"outcome":"delivered"}`, "", "", OutcomeDelivered, ""},
		{"verdict", `{"verdict":"delivered"}`, "", "", OutcomeDelivered, ""},
		{"boolean", `{"delivered":true}`, "", "", OutcomeDelivered, ""},
		{"false", `{"delivered":false}`, "", "", OutcomeAccepted, ""},
		{"wrong_type", `{"delivered":"true"}`, "", "", OutcomeAccepted, ""},
		{"verdict_header", `{}`, "delivered", "", OutcomeDelivered, ""},
		{"inbound_header", `{}`, "", "delivered", OutcomeDelivered, ""},
		{"token_header", `{}`, "FED_TOKEN_ONLY", "", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"token_inbound_header", `{}`, "", "FED_TOKEN_ONLY", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"token_body", `{"verdict":"FED_TOKEN_ONLY"}`, "", "", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"token_inbound_body", `{"inbound_verdict":"FED_TOKEN_ONLY"}`, "", "", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"token_overrides_delivery", `{"status":"delivered"}`, "", "FED_TOKEN_ONLY", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"hold_overrides_delivery", `{"status":"held","delivered":true}`, "delivered", "", OutcomeHeld, ""},
		{"hold_retains_policy", `{"status":"held"}`, "", "FED_TOKEN_ONLY", OutcomeHeld, "FED_TOKEN_ONLY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, code := transportReceiptOutcome(json.RawMessage(tc.body), tc.verdict, tc.inbound)
			require.Equal(t, tc.want, outcome)
			require.Equal(t, tc.code, code)
		})
	}
}

func TestSummarizeFanoutMixedReceipts(t *testing.T) {
	var view transcriptMessage
	summarizeFanout(&view, []DeliveryOutcome{
		{Target: "answer", Outcome: OutcomeDelivered},
		{Target: "queue", Outcome: OutcomeAccepted},
		{Target: "token", Outcome: OutcomeAccepted, Code: "FED_TOKEN_ONLY"},
		{Target: "hold", Outcome: OutcomeHeld},
		{Target: "missing", Outcome: OutcomeRefused},
		{Target: "pending", Outcome: OutcomeInProgress},
	})
	require.Equal(t, 1, view.Delivered)
	require.Equal(t, 2, view.Accepted)
	require.Equal(t, 1, view.Held)
	require.Equal(t, 1, view.Skipped)
	require.Equal(t, []string{"missing"}, view.SkippedAgents)
	require.Equal(t, map[Outcome]int{OutcomeDelivered: 1, OutcomeAccepted: 2, OutcomeHeld: 1, OutcomeRefused: 1, OutcomeInProgress: 1}, view.OutcomeCounts)
}
