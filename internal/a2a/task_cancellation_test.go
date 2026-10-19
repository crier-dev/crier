package a2a

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskEvidenceCancellationPrecedesSnapshotExpiry(t *testing.T) {
	now := time.Now().UTC()
	canceledAt := now.Add(-time.Hour)
	evidence := TaskEvidence{
		Entry:      &InboxView{ID: "canceled-task", LeaseID: "old-lease", ExpiresAt: now.Add(-time.Minute)},
		CanceledAt: canceledAt,
	}
	state, basis, rpcErr := evidence.Resolve("agent", "canceled-task", now)
	require.Nil(t, rpcErr)
	require.Equal(t, TaskStateCanceled, state)
	require.Equal(t, StateBasisCanceled, basis)
	task, err := evidence.Task(state, basis, nil, now)
	require.NoError(t, err)
	require.Equal(t, "canceled-task", task.ID)
	require.Equal(t, Timestamp(canceledAt), task.Status.Timestamp)
	require.Contains(t, *task.Status.Message.Parts[0].Text, "canceled by request")
}
