//go:build integration

package registry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPostgresA2AReplayContract(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	// Exercise the production policy wrapper and an independent pool, not only
	// the concrete backend's process-local state.
	other, err := NewPostgresStore(t.Context(), testConnString)
	require.NoError(t, err)
	defer other.Close()
	testA2AReplayContract(t, []A2AReplayStore{store, NewAuthorityStore(other, nil, "")})
	var count int
	require.NoError(t, store.pool.QueryRow(t.Context(), `SELECT count(*) FROM a2a_message_replays`).Scan(&count))
	require.LessOrEqual(t, count, 3)
	store.Close()
	reopened, err := NewPostgresStore(t.Context(), testConnString)
	require.NoError(t, err)
	defer reopened.Close()
	owner, raw, err := reopened.AcquireA2AReplay("race", "same-key", time.Date(2026, 10, 19, 12, 0, 0, 123456789, time.UTC), time.Hour)
	require.NoError(t, err)
	require.Empty(t, owner)
	require.Equal(t, "one", string(raw))
}
