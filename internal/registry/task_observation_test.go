package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

func taskObservationFixture(outcome string) TaskObservation {
	return TaskObservation{TaskID: "task", ObservedAt: time.Now().UTC(), Outcome: outcome, Evidence: json.RawMessage(`{"Entry":{"ID":"task"}}`)}
}

func TestTaskObservation_MemoryScopeCopiesAndMonotonicity(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	wrapped := NewAuthorityStore(store, nil, "")
	unknown := taskObservationFixture("unknown")
	require.NoError(t, wrapped.RecordTaskObservation("a", unknown))
	ack := taskObservationFixture("acknowledged")
	require.NoError(t, wrapped.RecordTaskObservation("a", ack))
	require.NoError(t, wrapped.RecordTaskObservation("a", taskObservationFixture("canceled")))
	require.NoError(t, wrapped.RecordTaskObservation("a", unknown))
	unknown.Evidence[0] = 'x'
	got, found, err := wrapped.TaskObservation("a", "task")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ack, got, "terminal evidence and original timestamp are immutable")
	got.Evidence[0] = 'x'
	list, err := wrapped.TaskObservations("a")
	require.NoError(t, err)
	require.Equal(t, []TaskObservation{ack}, list)
	list[0].Evidence[0] = 'x'
	got, found, err = wrapped.TaskObservation("b", "task")
	require.NoError(t, err)
	require.False(t, found)
	require.Empty(t, got.Evidence)
	got, found, err = wrapped.TaskObservation("a", "task")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ack, got)
	fresh := NewMemoryStore()
	_, found, err = fresh.TaskObservation("a", "task")
	require.NoError(t, err)
	require.False(t, found, "a new memory backend honestly has no prior evidence")
}

func TestTaskObservation_MemoryCloseAtomic(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	require.NoError(t, store.Register(&Agent{ID: "a"}))
	o := taskObservationFixture("canceled")
	require.ErrorIs(t, store.CloseObservedTask("a", o), ErrMessageNotFound)
	_, found, err := store.TaskObservation("a", o.TaskID)
	require.NoError(t, err)
	require.False(t, found, "a failed close is not cancellation evidence")
	require.NoError(t, store.Deliver("a", &InboxEntry{ID: o.TaskID, Payload: json.RawMessage(`{}`)}))
	require.NoError(t, store.CloseObservedTask("a", o))
	_, err = store.Peek("a", o.TaskID)
	require.ErrorIs(t, err, ErrMessageNotFound)
	got, found, err := store.TaskObservation("a", o.TaskID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, o, got)
}

func TestTaskObservation_PostgresCloseRollback(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	o := taskObservationFixture("canceled")
	mock.ExpectBegin()
	mock.ExpectQuery("DELETE FROM inbox_entries").WithArgs("a", o.TaskID).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(o.TaskID))
	mock.ExpectExec("INSERT INTO a2a_task_observations").WithArgs("a", o.TaskID, o.ObservedAt, o.Outcome, []byte(o.Evidence)).WillReturnError(pgx.ErrTxClosed)
	mock.ExpectRollback()
	require.ErrorIs(t, store.CloseObservedTask("a", o), pgx.ErrTxClosed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTaskObservation_PostgresReadAndWrite(t *testing.T) {
	t.Parallel()
	store, mock := newMockStore(t)
	o := taskObservationFixture("acknowledged")
	mock.ExpectExec("INSERT INTO a2a_task_observations").WithArgs("a", o.TaskID, o.ObservedAt, o.Outcome, []byte(o.Evidence)).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	require.NoError(t, store.RecordTaskObservation("a", o))
	mock.ExpectQuery("SELECT observed_at, outcome, evidence").WithArgs("a", o.TaskID).WillReturnRows(pgxmock.NewRows([]string{"observed_at", "outcome", "evidence"}).AddRow(o.ObservedAt, o.Outcome, []byte(o.Evidence)))
	got, found, err := store.TaskObservation("a", o.TaskID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, o, got)
	mock.ExpectQuery("SELECT observed_at, outcome, evidence").WithArgs("other", o.TaskID).WillReturnError(pgx.ErrNoRows)
	_, found, err = store.TaskObservation("other", o.TaskID)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTaskObservation_InvalidInput(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	for _, change := range []func(*TaskObservation){
		func(o *TaskObservation) { o.TaskID = "" },
		func(o *TaskObservation) { o.ObservedAt = time.Time{} },
		func(o *TaskObservation) { o.Outcome = "completed-by-absence" },
		func(o *TaskObservation) { o.Evidence = json.RawMessage(`invalid`) },
	} {
		o := taskObservationFixture("acknowledged")
		change(&o)
		require.ErrorIs(t, store.RecordTaskObservation("a", o), ErrInvalidStoreInput)
	}
	require.ErrorIs(t, store.CloseObservedTask("a", taskObservationFixture("acknowledged")), ErrInvalidStoreInput)
}
