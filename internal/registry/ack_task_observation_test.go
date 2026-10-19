package registry

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/stretchr/testify/require"
)

// Both concrete stores run this same contract without installing an ack watch.
func testAckTaskObservation(t *testing.T, store Store) {
	t.Helper()
	require.NoError(t, store.Register(&Agent{ID: "ack-agent"}))
	require.NoError(t, store.Register(&Agent{ID: "other-agent"}))
	backend := store.(TaskObservationStore)
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown-race-%t", race), func(t *testing.T) {
			id := fmt.Sprintf("task-%t", race)
			payload := json.RawMessage(`{"message_id":"original","context_id":"context","role":"ROLE_USER","parts":[{"text":"pre-ack instruction"}]}`)
			entry := &InboxEntry{ID: id, Payload: payload}
			if race {
				never := 0
				entry.TTLSeconds = &never // Also exercise PostgreSQL's infinity scan.
			}
			require.NoError(t, store.Deliver("ack-agent", entry))
			entries, lease, err := store.Retrieve("ack-agent", time.Minute, 1)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			before, err := store.(InboxPeeker).Peek("ack-agent", id)
			require.NoError(t, err)
			unknown := TaskObservation{TaskID: id, ObservedAt: time.Now().UTC(), Outcome: "unknown", Evidence: json.RawMessage(`{}`)}
			if race {
				require.NoError(t, backend.RecordTaskObservation("ack-agent", unknown))
			}
			start := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				<-start
				if race {
					result <- backend.RecordTaskObservation("ack-agent", unknown)
				} else {
					result <- nil
				}
			}()
			close(start)
			require.NoError(t, store.Ack("ack-agent", lease, []string{id}))
			require.NoError(t, <-result)
			o, found, err := backend.TaskObservation("ack-agent", id)
			require.NoError(t, err)
			require.True(t, found, "Ack itself must retain evidence without a watcher")
			require.Equal(t, "acknowledged", o.Outcome)
			var ev a2a.TaskEvidence
			require.NoError(t, json.Unmarshal(o.Evidence, &ev))
			require.NotNil(t, ev.Entry)
			require.Equal(t, id, ev.Entry.ID)
			require.JSONEq(t, string(payload), string(ev.Entry.Payload))
			require.True(t, before.CreatedAt.Equal(ev.Entry.CreatedAt))
			require.True(t, before.ExpiresAt.Equal(ev.Entry.ExpiresAt))
			require.NotNil(t, ev.Entry.LeasedAt)
			require.True(t, before.LeasedAt.Equal(*ev.Entry.LeasedAt))
			require.Equal(t, lease, ev.Entry.LeaseID)
			require.False(t, ev.Entry.ACKed, "snapshot must precede the Ack")
			require.True(t, o.ObservedAt.Equal(ev.AcknowledgedAt))
			state, basis, rpcErr := ev.Resolve("ack-agent", id, time.Now())
			require.Nil(t, rpcErr)
			require.Equal(t, a2a.TaskStateWorking, state, "Ack proves consumption, not an answer")
			require.Equal(t, a2a.StateBasisAcknowledged, basis)
			_, err = store.(InboxPeeker).Peek("ack-agent", id)
			require.ErrorIs(t, err, ErrMessageNotFound)
			require.NoError(t, backend.RecordTaskObservation("ack-agent", unknown))
			after, found, err := backend.TaskObservation("ack-agent", id)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, o, after, "late unknown cannot downgrade or rewrite terminal evidence")
		})
	}

	t.Run("non-task", func(t *testing.T) {
		require.NoError(t, store.Deliver("ack-agent", &InboxEntry{ID: "plain", Payload: []byte(`{"text":"ordinary message"}`)}))
		_, lease, err := store.Retrieve("ack-agent", time.Minute, 1)
		require.NoError(t, err)
		require.NoError(t, store.Ack("ack-agent", lease, []string{"plain"}))
		_, found, err := backend.TaskObservation("ack-agent", "plain")
		require.NoError(t, err)
		require.False(t, found)
	})
	t.Run("invalid-batch-and-foreign-task", func(t *testing.T) {
		payload := []byte(`{"parts":[{"text":"keep me"}],"message_id":"keep"}`)
		require.NoError(t, store.Deliver("ack-agent", &InboxEntry{ID: "keep", Payload: payload}))
		_, lease, err := store.Retrieve("ack-agent", time.Minute, 1)
		require.NoError(t, err)
		require.ErrorIs(t, store.Ack("ack-agent", lease, []string{"keep", "unknown"}), ErrMessageNotFound)
		require.ErrorIs(t, store.Ack("other-agent", lease, []string{"keep"}), ErrMessageNotFound)
		require.ErrorIs(t, store.Ack("ack-agent", "wrong", []string{"keep"}), ErrLeaseConflict)
		for _, agent := range []string{"ack-agent", "other-agent"} {
			for _, id := range []string{"keep", "unknown"} {
				_, found, err := backend.TaskObservation(agent, id)
				require.NoError(t, err)
				require.False(t, found)
			}
		}
		_, err = store.(InboxPeeker).Peek("ack-agent", "keep")
		require.NoError(t, err, "failed batch must not remove a valid entry")
	})
}

func TestAckTaskObservationMemory(t *testing.T) {
	testAckTaskObservation(t, NewMemoryStore())
}
