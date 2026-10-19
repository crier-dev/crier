package a2a

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskPublisherIdentityAssurance(t *testing.T) {
	for _, tc := range []struct {
		name, stamp, want string
	}{
		{"legacy output", "", "publisher-identity-claimed"},
		{"explicit claim", "publisher-identity-claimed", "publisher-identity-claimed"},
		{"verified output", "publisher-identity-verified", "publisher-identity-verified"},
		{"unknown assurance", "verified-by-payload", "publisher-identity-claimed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := &Artifact{ArtifactID: "answer", Parts: []Part{TextPart("answer")}}
			if tc.stamp != "" {
				artifact.Metadata = map[string]any{"crier": map[string]any{"publisher_identity_basis": tc.stamp}}
			}
			ev := TaskEvidence{Entry: &InboxView{ID: "task"}, AcknowledgedAt: lifecycleNow, AnswerObservedAt: lifecycleNow, Answer: artifact}
			state, basis, rpcErr := ev.Resolve("owner", "task", lifecycleNow)
			require.Nil(t, rpcErr)
			task, err := ev.Task(state, basis, nil, lifecycleNow)
			require.NoError(t, err)
			raw, err := json.Marshal(task)
			require.NoError(t, err)
			var wire map[string]any
			require.NoError(t, json.Unmarshal(raw, &wire))
			require.Equal(t, tc.want, wire["metadata"].(map[string]any)["crier"].(map[string]any)["publisher_identity_basis"])
			require.Contains(t, *task.Status.Message.Parts[0].Text, tc.want)
		})
	}
}
