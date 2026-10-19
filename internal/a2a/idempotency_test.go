package a2a

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIdempotencyInfoProjectionAndReceiptIsolation(t *testing.T) {
	for _, durable := range []bool{false, true} {
		for _, replayed := range []bool{false, true} {
			info := NewIdempotencyInfo(durable, 123, replayed)
			durability, replay := "process", "new"
			if durable {
				durability = "durable"
			}
			if replayed {
				replay = "replayed"
			}
			require.Equal(t, durability, info.Durability)
			require.Equal(t, replay, info.Replay)
			require.Equal(t, float64(123), info.WindowSeconds)
			require.Contains(t, info.Description, "after expiry the same messageId may create a new task")
			tr, err := Translate(&SendMessageParams{Message: &Message{MessageID: "key", Role: RoleUser, Parts: []Part{TextPart("work")}}}, nil, "", false)
			require.NoError(t, err)
			accept := DeliverAccept{ID: "task", Transport: "inbox", Idempotency: info}
			task, err := TaskFromAccept(tr, accept, time.Now())
			require.NoError(t, err)
			require.Equal(t, info, task.Metadata["crier"].(CrierMeta).Idempotency)
			accept.Reply = json.RawMessage(`{"answer":"done"}`)
			msg, err := MessageFromReply(tr, accept, time.Now())
			require.NoError(t, err)
			require.Equal(t, info, msg.Metadata["crier"].(map[string]any)["idempotency"])
			raw, err := json.Marshal(Replay{Translation: tr, Accept: accept, AcceptedAt: time.Now()})
			require.NoError(t, err)
			require.NotContains(t, string(raw), "window_seconds", "request-local labels must never freeze in a retained receipt")
			decoded, err := DecodeReplay(raw)
			require.NoError(t, err)
			require.Nil(t, decoded.Accept.Idempotency)
		}
	}
}
