package registry

import (
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

func TestInboxAcknowledgementWatchScopeAndCleanup(t *testing.T) {
	var observer inboxAcknowledgements
	first, stopFirst := observer.WatchAcknowledgement("a", "m")
	second, stopSecond := observer.WatchAcknowledgement("a", "m")
	foreign, stopForeign := observer.WatchAcknowledgement("b", "m")
	require.False(t, first())
	observer.recordAcknowledgement("a", []string{"m"})
	require.True(t, first())
	require.True(t, second())
	require.False(t, foreign(), "another inbox's same message id is not acknowledged")
	stopFirst()
	stopFirst() // idempotent cleanup must not remove another observer
	require.Len(t, observer.watches[inboxAckKey{"a", "m"}], 1)
	stopSecond()
	stopForeign()
	require.Empty(t, observer.watches)
	late, stopLate := observer.WatchAcknowledgement("a", "m")
	defer stopLate()
	require.False(t, late(), "a new watch cannot invent evidence about a past ack")
}

func TestInboxAcknowledgementMemoryOnlySuccessfulAck(t *testing.T) {
	store := NewMemoryStore()
	require.NoError(t, store.Register(&Agent{ID: "a"}))
	require.NoError(t, store.Deliver("a", &InboxEntry{ID: "m", Payload: []byte(`{}`)}))
	observed, stop := store.WatchAcknowledgement("a", "m")
	defer stop()
	_, lease, err := store.Retrieve("a", 1, 60)
	require.NoError(t, err)
	require.ErrorIs(t, store.Ack("a", "wrong-lease", []string{"m"}), ErrLeaseConflict)
	require.False(t, observed())
	require.NoError(t, store.Ack("a", lease, []string{"m"}))
	require.True(t, observed())
	_, err = store.Peek("a", "m")
	require.ErrorIs(t, err, ErrMessageNotFound, "ack still hard-deletes the inbox entry")
}

func TestInboxAcknowledgementPostgresRequiresCommit(t *testing.T) {
	for _, commitFailure := range []bool{false, true} {
		name := "committed"
		if commitFailure {
			name = "commit-failed"
		}
		t.Run(name, func(t *testing.T) {
			store, mock := newMockStore(t)
			observed, stop := store.WatchAcknowledgement("a", "m")
			defer stop()
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT 1 FROM agents`).WithArgs("a").
				WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(1))
			mock.ExpectQuery(`SELECT id, COALESCE`).WithArgs("a", []string{"m"}).
				WillReturnRows(pgxmock.NewRows([]string{"id", "lease_id"}).AddRow("m", "lease"))
			mock.ExpectQuery(`DELETE FROM inbox_entries`).WithArgs("a", "lease", []string{"m"}).
				WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("m"))
			commit := mock.ExpectCommit()
			if commitFailure {
				commit.WillReturnError(errors.New("commit failed"))
			}
			err := store.Ack("a", "lease", []string{"m"})
			if commitFailure {
				require.ErrorContains(t, err, "ack commit")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, !commitFailure, observed())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
