package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The old four-shape delivered table (status/outcome/verdict/bool) encoded
// the defect: vocabulary chosen by the peer was treated as observed agent speech.
// Headers and status envelopes prove no answer crossed the boundary.
func TestTransportReceiptVocabulary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, verdict, inbound string
		want                         Outcome
		code                         string
	}{
		{"empty", "", "", "", OutcomeAccepted, ""},
		{"malformed", `{`, "", "", OutcomeAccepted, ""},
		{"unknown", `{"status":"completed"}`, "", "", OutcomeAccepted, ""},
		{"authorization", `{"verdict":"FED_ALLOWED"}`, "FED_ALLOWED", "FED_ALLOWED", OutcomeAccepted, ""},
		{"status", `{"status":" DELIVERED "}`, "", "", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"outcome", `{"outcome":"delivered"}`, "", "", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"verdict", `{"verdict":"delivered"}`, "", "", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"boolean", `{"delivered":true}`, "", "", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"false", `{"delivered":false}`, "", "", OutcomeAccepted, ""},
		{"wrong_type", `{"delivered":"true"}`, "", "", OutcomeAccepted, ""},
		{"verdict_header", `{}`, "delivered", "", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"inbound_header", `{}`, "", "delivered", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"token_header", `{}`, "FED_TOKEN_ONLY", "", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"token_inbound_header", `{}`, "", "FED_TOKEN_ONLY", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"token_body", `{"verdict":"FED_TOKEN_ONLY"}`, "", "", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"token_inbound_body", `{"inbound_verdict":"FED_TOKEN_ONLY"}`, "", "", OutcomeAccepted, "FED_TOKEN_ONLY"},
		{"token_overrides_delivery", `{"status":"delivered"}`, "", "FED_TOKEN_ONLY", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"hold_overrides_delivery", `{"status":"held","delivered":true}`, "delivered", "", OutcomeHeld, ""},
		{"hold_retains_policy", `{"status":"held"}`, "", "FED_TOKEN_ONLY", OutcomeHeld, "FED_TOKEN_ONLY"},
		{"inbound_body_claim", `{"inbound_verdict":"delivered"}`, "", "", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"reply", `{"reply":"the answer","live":true}`, "", "", OutcomeDelivered, ""},
		{"answer", `{"answer":"the answer"}`, "", "", OutcomeDelivered, ""},
		{"text", `{"text":"the answer"}`, "", "", OutcomeDelivered, ""},
		{"json_string", `"the answer"`, "", "", OutcomeDelivered, ""},
		{"raw_text", `the answer`, "", "", OutcomeDelivered, ""},
		{"artifact", `{"artifact":{"parts":[{"text":"the answer"}]}}`, "", "", OutcomeDelivered, ""},
		{"message", `{"message":{"text":"the answer"}}`, "", "", OutcomeDelivered, ""},
		{"blank_reply_claim", `{"delivered":true,"reply":"  "}`, "", "", OutcomeAccepted, "PEER_CLAIMED_DELIVERY"},
		{"malformed_reply", `{"reply":"the answer"`, "", "", OutcomeAccepted, ""},
		{"arbitrary_object", `{"result":{"answer":"not a supported reply"}}`, "", "", OutcomeAccepted, ""},
		{"array", `["the answer"]`, "", "", OutcomeAccepted, ""},
		{"scalar", `42`, "", "", OutcomeAccepted, ""},
		{"raw_status", `delivered`, "", "", OutcomeAccepted, ""},
		{"diagnostic_message", `{"message":"queued"}`, "", "", OutcomeAccepted, ""},
		{"nontext_part", `{"artifact":{"parts":[{"kind":"data","text":"not speech"}]}}`, "", "", OutcomeAccepted, ""},
		{"canned_reply", `{"reply":"demo","live":false}`, "", "", OutcomeAccepted, ""},
		{"failed_reply", `{"reply":"not an answer","status":"FAILED"}`, "", "", OutcomeAccepted, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, code := transportReceiptOutcome(json.RawMessage(tc.body), tc.verdict, tc.inbound)
			require.Equal(t, tc.want, outcome)
			require.Equal(t, tc.code, code)
		})
	}
}

func TestSummarizeFanoutMixedReceipts(t *testing.T) {
	t.Parallel()
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
