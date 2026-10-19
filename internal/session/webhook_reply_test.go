package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebhookReplyText(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"schema extracted", `"answer"`, "answer"},
		{"raw harness", `{"reply":"answer"}`, "answer"},
		{"text payload", `{"text":"answer"}`, "answer"},
		{"preserve whitespace", `{"reply":"\n answer \n"}`, "\n answer \n"},
		{"text precedence", `{"text":"text", "reply":"reply"}`, "text"},
		{"fallback to reply", `{"text":42, "reply":"answer"}`, "answer"},
		{"missing", ``, ""},
		{"null", `null`, ""},
		{"empty object", `{}`, ""},
		{"metadata", `{"ok":true}`, ""},
		{"nontext reply", `{"reply":{"text":"nested data"}}`, ""},
		{"array", `["not a text reply"]`, ""},
		{"number", `42`, ""},
		{"invalid JSON", `{"reply":`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, webhookReplyText(json.RawMessage(tc.raw)))
		})
	}
}

func TestWebhookReplyVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		outcome Outcome
		reason  string
	}{
		{"legacy string", `"answer"`, OutcomeDelivered, ""},
		{"legacy reply", `{"reply":"answer"}`, OutcomeDelivered, ""},
		{"empty", ``, OutcomeDelivered, ""},
		{"null", `null`, OutcomeDelivered, ""},
		{"array", `[]`, OutcomeDelivered, ""},
		{"malformed", `{"reply":`, OutcomeDelivered, ""},
		{"null flags", `{"live":null,"canned":null}`, OutcomeDelivered, ""},
		{"wrong flag types", `{"live":"false","canned":1}`, OutcomeDelivered, ""},
		{"reason alone is not failure", `{"reason":"completed","reply":"answer"}`, OutcomeDelivered, ""},
		{"demo reason", `{"live":false,"reason":"requires live mode"}`, OutcomeCanned, "requires live mode"},
		{"case insensitive failure", `{"status":" failed ","reason":"boom"}`, OutcomeFailed, "boom"},
		{"deduplicated diagnostics", `{"status":"FAILED","reason":"boom","error":"boom"}`, OutcomeFailed, "boom"},
		{"both diagnostics", `{"status":"FAILED","reason":"boom","error":"exit 3"}`, OutcomeFailed, "boom; exit 3"},
		{"error only", `{"error":"exit 3"}`, OutcomeFailed, "exit 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, reason := webhookReplyVerdict(json.RawMessage(tc.raw))
			require.Equal(t, tc.outcome, outcome)
			require.Equal(t, tc.reason, reason)
		})
	}
}
