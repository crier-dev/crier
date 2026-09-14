//go:build integration

package registry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// DF-CRIER-37 against the real Postgres backend: the requested `ttl_seconds`
// must reach the stored row, and ttl_seconds=0 ("never expires") must be both
// storable (the column is NOT NULL with CHECK expires_at > created_at) and
// non-expiring under the real claim/purge predicates — which are SQL, so an
// in-memory-only fix cannot prove them.

func TestPostgresStore_TTL_NeverExpiresRoundTrip(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "ttl-never")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	zero := 0
	entry := &InboxEntry{Payload: []byte(`{"n":1}`), TTLSeconds: &zero}
	require.NoError(t, store.Deliver(agent.ID, entry))
	require.True(t, entry.ExpiresAt.IsZero(), "a never-expiring delivery keeps the zero ExpiresAt")

	// The row must be the native timestamptz `infinity`, not a far-future
	// sentinel pretending to be one.
	var stored string
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT expires_at::text FROM inbox_entries WHERE id = $1`, entry.ID).Scan(&stored))
	require.Equal(t, "infinity", stored)

	// Purge (expires_at <= now) and stats (expires_at > now) must both treat
	// it as alive.
	require.Equal(t, 0, store.PurgeExpired(), "infinity is never TTL-purged")

	depth, _, _, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 1, depth)

	// And it must decode back to the documented never-expires representation
	// rather than blowing up the scan with "cannot scan Infinity into *time.Time".
	msgs, leaseID, err := store.Retrieve(agent.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.NotEmpty(t, leaseID)
	require.Len(t, msgs, 1, "a never-expiring message stays claimable")
	require.True(t, msgs[0].ExpiresAt.IsZero(), "infinity must read back as the zero time, got %s", msgs[0].ExpiresAt)

	require.NoError(t, store.Ack(agent.ID, leaseID, []string{entry.ID}))
}

func TestPostgresStore_TTL_RequestedLifetimeAndDefaultAreStored(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "ttl-hour")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	hour := 3600
	entry := &InboxEntry{Payload: []byte(`{"n":1}`), TTLSeconds: &hour}
	require.NoError(t, store.Deliver(agent.ID, entry))
	require.InDelta(t, 3600, entry.ExpiresAt.Sub(entry.CreatedAt).Seconds(), 5,
		"stored lifetime = %s, want 1h", entry.ExpiresAt.Sub(entry.CreatedAt))

	// The database agrees with the store's own view of the expiry.
	var seconds float64
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (expires_at - created_at)) FROM inbox_entries WHERE id = $1`,
		entry.ID).Scan(&seconds))
	require.InDelta(t, 3600, seconds, 5)

	// Absent ttl_seconds keeps the 24h default.
	def := &InboxEntry{Payload: []byte(`{"n":2}`)}
	require.NoError(t, store.Deliver(agent.ID, def))
	require.InDelta(t, (24 * time.Hour).Seconds(), def.ExpiresAt.Sub(def.CreatedAt).Seconds(), 5)

	// Neither is purged; a genuinely expired sibling still is, so the TTL path
	// did not disable expiration wholesale.
	expired := &InboxEntry{
		Payload:   []byte(`{"n":3}`),
		CreatedAt: time.Now().UTC().Add(-2 * time.Hour),
		ExpiresAt: time.Now().UTC().Add(-time.Hour),
	}
	require.NoError(t, store.Deliver(agent.ID, expired))
	require.Equal(t, 1, store.PurgeExpired(), "only the genuinely expired message is purged")

	depth, _, _, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 2, depth)
}
