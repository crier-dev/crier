package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

func taskOutputFixture() TaskOutput {
	return TaskOutput{TaskID: "task", ObservedAt: time.Now().UTC().Truncate(time.Microsecond), Artifact: json.RawMessage(`{"artifactId":"answer","parts":[{"text":"deployed"}],"metadata":{"crier":{"publisher_identity_basis":"publisher-identity-verified"}}}`)}
}

func TestTaskOutputMemoryScopeCopiesAndFirstEvidence(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	wrapped := NewAuthorityStore(store, nil, "")
	o := taskOutputFixture()
	require.NoError(t, wrapped.RecordTaskOutput("a", o))
	original := append(json.RawMessage(nil), o.Artifact...)
	o.Artifact[0] = 'x'
	got, found, err := wrapped.TaskOutput("a", o.TaskID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, original, got.Artifact)
	got.Artifact[0] = 'x'
	replacement := taskOutputFixture()
	replacement.ObservedAt = replacement.ObservedAt.Add(time.Hour)
	require.NoError(t, wrapped.RecordTaskOutput("a", replacement))
	got, found, err = wrapped.TaskOutput("a", o.TaskID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, original, got.Artifact)
	require.Equal(t, o.ObservedAt, got.ObservedAt)
	_, found, err = wrapped.TaskOutput("b", o.TaskID)
	require.NoError(t, err)
	require.False(t, found)
	// Output does not create, overwrite, or reverse a lifecycle observation.
	require.NoError(t, wrapped.RecordTaskObservation("a", taskObservationFixture("acknowledged")))
	require.NoError(t, wrapped.RecordTaskObservation("a", taskObservationFixture("unknown")))
	obs, found, err := wrapped.TaskObservation("a", o.TaskID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "acknowledged", obs.Outcome)
}

func TestTaskOutputPostgresReadWrite(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	o := taskOutputFixture()
	mock.ExpectExec("INSERT INTO a2a_task_outputs.*ON CONFLICT .* DO NOTHING").WithArgs("a", o.TaskID, o.ObservedAt, []byte(o.Artifact)).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	require.NoError(t, store.RecordTaskOutput("a", o))
	mock.ExpectQuery("SELECT observed_at, artifact FROM a2a_task_outputs").WithArgs("a", o.TaskID).WillReturnRows(pgxmock.NewRows([]string{"observed_at", "artifact"}).AddRow(o.ObservedAt, []byte(o.Artifact)))
	got, found, err := store.TaskOutput("a", o.TaskID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, o, got)
	mock.ExpectQuery("SELECT observed_at, artifact FROM a2a_task_outputs").WithArgs("other", o.TaskID).WillReturnError(pgx.ErrNoRows)
	_, found, err = store.TaskOutput("other", o.TaskID)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTaskOutputInvalidInput(t *testing.T) {
	t.Parallel()
	for _, change := range []func(*TaskOutput){
		func(o *TaskOutput) { o.TaskID = "" },
		func(o *TaskOutput) { o.ObservedAt = time.Time{} },
		func(o *TaskOutput) { o.Artifact = json.RawMessage(`{}`) },
		func(o *TaskOutput) { o.Artifact = json.RawMessage(`{"artifactId":"a","parts":[]}`) },
		func(o *TaskOutput) { o.Artifact = json.RawMessage(`broken`) },
	} {
		o := taskOutputFixture()
		change(&o)
		require.ErrorIs(t, NewMemoryStore().RecordTaskOutput("a", o), ErrInvalidStoreInput)
	}
}
