package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransportReplyTextShapes(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"raw-whitespace-preserved", "\n the answer \n", "\n the answer \n"},
		{"string-whitespace-preserved", `"\n the answer \n"`, "\n the answer \n"},
		{"text-precedes-reply-and-answer", `{"text":"text","reply":"reply","answer":"answer"}`, "text"},
		{"wrong-type-falls-back", `{"text":42,"reply":"reply"}`, "reply"},
		{"blank-falls-back", `{"text":" ","answer":"answer"}`, "answer"},
		{"artifact-precedes-message", `{"artifact":{"answer":"artifact"},"message":{"text":"message"}}`, "artifact"},
		{"part-order-and-whitespace", `{"artifact":{"parts":[{"text":" first "},{"type":"data","text":"metadata"},{"kind":"text","text":"second"},{"text":" "}]}}`, " first \nsecond"},
		{"nontext-type-excluded", `{"message":{"parts":[{"type":"file","text":"file name"}]}}`, ""},
		{"file-data-parts-excluded", `{"artifact":{"parts":[{"url":"https://example.invalid/file"},{"data":{"text":"metadata"}}]}}`, ""},
		{"malformed-parts", `{"message":{"parts":{"text":"not an array"}}}`, ""},
		{"bad-part-text", `{"artifact":{"parts":[{"text":true}]}}`, ""},
		{"no-arbitrary-recursion", `{"artifact":{"result":{"text":"not supported"}}}`, ""},
		{"message-diagnostic-not-answer", `{"message":"accepted for processing"}`, ""},
		{"no-array-stringification", `["text"]`, ""},
		{"no-bool-stringification", `true`, ""},
		{"no-null", `null`, ""},
		{"no-number", `42`, ""},
		{"no-malformed-object", `{"text":"answer"`, ""},
		{"no-malformed-array", `["answer"`, ""},
		{"no-malformed-string", `"answer`, ""},
		{"no-markup", `<html>accepted</html>`, ""},
		{"no-blank", " \n ", ""},
		{"no-status-string", `" DELIVERED "`, ""},
		{"no-federation-word", `FED_ALLOWED`, ""},
		{"no-canned", `{"answer":"demo","canned":true}`, ""},
		{"no-failure-error", `{"answer":"failure","error":"execution failed"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, transportReplyText(json.RawMessage(tc.body)))
		})
	}
}
