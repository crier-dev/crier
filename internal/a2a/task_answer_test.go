package a2a

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskAnswerProjection(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		ack, output, canceled, unknown bool
		want                           TaskState
	}{
		{"consumed without answer", true, false, false, false, TaskStateWorking},
		{"answered and acknowledged", true, true, false, false, TaskStateCompleted},
		{"output alone is not acknowledgement", false, true, false, false, TaskStateSubmitted},
		{"cancel unchanged", true, true, true, false, TaskStateCanceled},
		{"unknown unchanged", false, true, false, true, TaskStateUnspecified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := TaskEvidence{Entry: &InboxView{ID: "task-355", CreatedAt: lifecycleNow.Add(-time.Minute)}}
			if tc.ack {
				ev.AcknowledgedAt = lifecycleNow
			}
			if tc.output {
				ev.Answer = &Artifact{ArtifactID: "answer-355", Parts: []Part{TextPart("canary deployed")}}
				ev.AnswerObservedAt = lifecycleNow.Add(time.Second)
			}
			if tc.canceled {
				ev.CanceledAt = lifecycleNow
			}
			if tc.unknown {
				ev.RemovedAt = lifecycleNow
			}
			state, basis, rpcErr := ev.Resolve("agent-355", "task-355", lifecycleNow.Add(time.Minute))
			require.Nil(t, rpcErr)
			require.Equal(t, tc.want, state)
			task, err := ev.Task(state, basis, nil, lifecycleNow.Add(time.Minute))
			require.NoError(t, err)
			raw, err := json.Marshal(task)
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(raw, &wire))
			meta := wire["metadata"].(map[string]any)["crier"].(map[string]any)
			require.Equal(t, tc.output, meta["answered"])
			if tc.output {
				require.Equal(t, Timestamp(ev.AnswerObservedAt), meta["answer_observed_at"])
				require.Len(t, task.Artifacts, 1)
				require.Len(t, *task.ForListing(true).Artifacts, 1)
			} else {
				require.NotContains(t, meta, "answer_observed_at")
			}
			if tc.ack && !tc.output {
				require.Contains(t, *task.Status.Message.Parts[0].Text, "no answer observed yet")
			}
		})
	}
}

func TestTaskEmptyAnswerIsNotOutputEvidence(t *testing.T) {
	ev := TaskEvidence{Entry: &InboxView{ID: "task-355"}, AcknowledgedAt: lifecycleNow, Answer: &Artifact{}, AnswerObservedAt: lifecycleNow}
	state, _, rpcErr := ev.Resolve("agent-355", "task-355", lifecycleNow)
	require.Nil(t, rpcErr)
	require.Equal(t, TaskStateWorking, state)
}

func TestTaskOutputPartsMustCarryContent(t *testing.T) {
	for _, parts := range [][]Part{nil, {}, {TextPart("")}, {TextPart(" ")}, {{Data: json.RawMessage("null")}}, {{Data: json.RawMessage("{}")}}, {{}}} {
		require.False(t, HasOutputParts(parts), "empty parts are not answer evidence: %#v", parts)
	}
	require.True(t, HasOutputParts([]Part{TextPart("canary deployed")}))
}
