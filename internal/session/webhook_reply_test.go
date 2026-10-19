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
		{"result without text", `{"result":{"ok":true}}`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: result"},
		{"messages without text", `{"messages":[{"role":"assistant"}]}`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: messages"},
		{"sorted keys", `{"result":{},"messages":[],"data":{}}`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: data, messages, result"},
		{"empty", ``, OutcomeUnreadableReply, "reply body had no displayable text field; keys: (none)"},
		{"null", `null`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: (none)"},
		{"array", `[]`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: (none)"},
		{"malformed", `{"reply":`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: (none)"},
		{"empty object", `{}`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: (none)"},
		{"blank string", `"  "`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: (none)"},
		{"nontext reply", `{"reply":{"text":"nested"}}`, OutcomeUnreadableReply, "reply body had no displayable text field; keys: reply"},
		{"null flags", `{"live":null,"canned":null,"reply":"answer"}`, OutcomeDelivered, ""},
		{"wrong flag types", `{"live":"false","canned":1,"reply":"answer"}`, OutcomeDelivered, ""},
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
