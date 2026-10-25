//go:build integration

package registry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAckTaskObservationPostgres(t *testing.T) {
	t.Parallel()
	testAckTaskObservation(t, newTestStore(t))
}

func TestAckTaskObservationPostgresRollback(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	require.NoError(t, store.Register(testAgent(t)))
	ctx := context.Background()
	_, err := store.pool.Exec(ctx, `CREATE FUNCTION reject_ack_observation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected ack observation failure'; END; $$;
CREATE TRIGGER reject_ack_observation BEFORE INSERT ON a2a_task_observations FOR EACH ROW EXECUTE FUNCTION reject_ack_observation();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := store.pool.Exec(ctx, `DROP FUNCTION reject_ack_observation() CASCADE`)
		require.NoError(t, err)
	})
	payload := []byte(`{"parts":[{"text":"rollback evidence"}],"message_id":"rollback"}`)
	require.NoError(t, store.Deliver("test-agent", &InboxEntry{ID: "rollback", Payload: payload}))
	_, lease, err := store.Retrieve("test-agent", time.Minute, 1)
	require.NoError(t, err)
	observed, stop := store.WatchAcknowledgement("test-agent", "rollback")
	defer stop()
	require.ErrorContains(t, store.Ack("test-agent", lease, []string{"rollback"}), "injected ack observation failure")
	require.False(t, observed(), "rolled-back ack must not notify watchers")
	entry, err := store.Peek("test-agent", "rollback")
	require.NoError(t, err)
	require.JSONEq(t, string(payload), string(entry.Payload))
	require.Equal(t, lease, entry.LeaseID)
	_, found, err := store.TaskObservation("test-agent", "rollback")
	require.NoError(t, err)
	require.False(t, found)
}
