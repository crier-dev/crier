package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/relay"
)

// Deterministically cover both sides of the persisted-output/fanout race.
// The terminal projection must carry the durable artifact before closing even
// when the relay frame has not reached the stream, without duplicating a frame
// already forwarded. No sleeps or ticker scheduling assumptions are involved.
func TestA2APublishedOutputBeforeTerminalStatus(t *testing.T) {
	for _, frameFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "poll-before-fanout", true: "fanout-before-poll"}[frameFirst], func(t *testing.T) {
			base, store := terminalHarness(t)
			task := sentTask(t, rpcResult(t, mustA2AOK(t, http.DefaultClient, base, sendMessageBody("terminal-agent", a2a.MethodSendMessage, "order-361"))))
			id := taskIDOf(t, task)
			_, lease, err := store.Retrieve("terminal-agent", time.Minute, 1)
			require.NoError(t, err)
			require.NoError(t, store.Ack("terminal-agent", lease, []string{id}))
			h := &a2aHandler{store: store, opts: a2aOptions{now: time.Now}}
			ev, rpcErr := h.evidence("terminal-agent", id)
			require.Nil(t, rpcErr)
			typed, err := ev.Task(a2a.TaskStateWorking, a2a.StateBasisAcknowledged, nil, time.Now())
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			s := &a2aStream{h: h, w: rec, id: json.RawMessage("1"), target: "terminal-agent", task: typed, state: a2a.TaskStateWorking}
			topic, ok := a2a.TaskTopic(id)
			require.True(t, ok)
			raw, err := json.Marshal(outputEnvelope(id, "ordered answer"))
			require.NoError(t, err)
			require.NoError(t, h.recordPublishedOutput("", "terminal-agent", topic, raw, false))
			if frameFirst {
				frame, err := json.Marshal(relay.Frame{Topic: topic, Event: raw})
				require.NoError(t, err)
				s.artifact(frame)
			}
			state, done := s.projectAcknowledgement()
			require.True(t, done)
			require.Equal(t, a2a.TaskStateCompleted, state)
			body := rec.Body.String()
			require.Equal(t, 1, strings.Count(body, `"artifactUpdate"`), body)
			require.Contains(t, body, `"artifactId":"`+id+`-answer"`)
			require.Contains(t, body, `"text":"ordered answer"`)
			require.Less(t, strings.Index(body, `"artifactUpdate"`), strings.Index(body, `"statusUpdate"`), body)
		})
	}
}
