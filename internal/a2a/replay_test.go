package a2a

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReplayRoundTripPreservesOptionalAcceptFields(t *testing.T) {
	for _, expiry := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage(`"2026-10-20T00:00:00Z"`)} {
		tr, err := Translate(&SendMessageParams{Message: &Message{MessageID: "key", Role: RoleUser, Parts: []Part{TextPart("work")}}}, nil, "", false)
		require.NoError(t, err)
		tr.SentMessage.Metadata = map[string]any{"large_id": json.Number("9007199254740993")}
		original := Replay{Translation: tr, Accept: DeliverAccept{ID: "task", Transport: "inbox", ExpiresAt: expiry}, AcceptedAt: time.Now().UTC()}
		raw, err := json.Marshal(original)
		require.NoError(t, err)
		restored, err := DecodeReplay(raw)
		require.NoError(t, err)
		require.Equal(t, json.Number("9007199254740993"), restored.Translation.SentMessage.Metadata["large_id"])
		require.Nil(t, restored.Accept.Guard, "absent guard must not become explicit JSON null")
		require.Nil(t, restored.Accept.Reply)
		require.Equal(t, expiry, restored.Accept.ExpiresAt, "never-expiry null and missing expiry have different semantics")
		before, err := TaskFromAccept(original.Translation, original.Accept, original.AcceptedAt)
		require.NoError(t, err)
		after, err := TaskFromAccept(restored.Translation, restored.Accept, restored.AcceptedAt)
		require.NoError(t, err)
		first, err := json.Marshal(before)
		require.NoError(t, err)
		second, err := json.Marshal(after)
		require.NoError(t, err)
		require.Equal(t, string(first), string(second))
	}
}

func TestDecodeReplayRejectsIncompleteReceipts(t *testing.T) {
	for _, raw := range []string{"{", "null", "{}", `{"translation":{"Request":null},"accept":{"id":"task"}}`, `{} trailing`} {
		_, err := DecodeReplay([]byte(raw))
		require.Error(t, err, raw)
	}
}
