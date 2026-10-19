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
