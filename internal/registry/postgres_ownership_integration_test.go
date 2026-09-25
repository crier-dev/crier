//go:build integration

package registry

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CR-FEAT-025 against a real PostgreSQL: the migration, the dead-letter
// destination, the reporting purge and transfer/reassign. The unit tests use
// pgxmock and prove the SHAPE of the SQL; these prove it RUNS — the columns
// exist, the predicates match the rows they claim to, and the CHECK/UNIQUE
// constraints behave as the contracts above them assume.

// TestPostgresOwnership_MigrationAddsOwnershipSchema asserts the COLUMNS and
// TABLE, not the migration file: a migration applied under another name, or one
// that only half-ran, must fail here.
func TestPostgresOwnership_MigrationAddsOwnershipSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(ctx, t)
	defer db.Close()
	clearSchema(ctx, t, db)

	require.NoError(t, RunMigrations(ctx, testConnString))

	for _, col := range []string{"sender", "idempotency_key"} {
		var exists bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT FROM information_schema.columns
				WHERE table_name = 'inbox_entries' AND column_name = $1
			)`, col).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "inbox_entries.%s should exist after migration 006", col)
	}

	var tableExists bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT FROM information_schema.tables WHERE table_name = 'dead_letters'
		)`).Scan(&tableExists))
	assert.True(t, tableExists, "dead_letters should exist after migration 006")

	// The dead-letter destination must NOT be foreign-keyed to agents: a dead
	// letter exists for the case where the consumer is gone.
	var fkCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM information_schema.table_constraints
		WHERE table_name = 'dead_letters' AND constraint_type = 'FOREIGN KEY'`).Scan(&fkCount))
	assert.Zero(t, fkCount, "dead_letters must not reference agents")
}

func TestPostgresOwnership_DeliverPersistsSenderAndKey(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "owner")
	entry := &InboxEntry{
		Payload:        []byte(`{"task":1}`),
		Sender:         "foreman",
		IdempotencyKey: "key-1",
		TTLSeconds:     ptrInt(3600),
	}
	require.NoError(t, store.Deliver(agent.ID, entry))

	msgs, _, err := store.Retrieve(agent.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "foreman", msgs[0].Sender, "the sender is durable WITH the message")
	require.Equal(t, "key-1", msgs[0].IdempotencyKey)
}

func TestPostgresOwnership_DeliverStoresNullForAbsentProvenance(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "owner")
	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: []byte(`{}`)}))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db := openTestDB(ctx, t)
	defer db.Close()

	var (
		sender sql.NullString
		key    sql.NullString
	)
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT sender, idempotency_key FROM inbox_entries WHERE agent_id = $1`, agent.ID).
		Scan(&sender, &key))
	require.False(t, sender.Valid, "an absent sender is SQL NULL, never an empty string")
	require.False(t, key.Valid, "an absent idempotency key is SQL NULL")
}

func TestPostgresOwnership_PurgeReportsWhatItRemoved(t *testing.T) {
	store := newTestStore(t)
	target := newTestAgent(t, store, "target")
	other := newTestAgent(t, store, "other")

	expired := func(id string) *InboxEntry {
		return &InboxEntry{
			ID:             id,
			Payload:        []byte(`{"task":"x"}`),
			Sender:         "foreman",
			IdempotencyKey: "k-" + id,
			CreatedAt:      time.Now().Add(-2 * time.Hour),
			ExpiresAt:      time.Now().Add(-time.Hour),
		}
	}
	require.NoError(t, store.Deliver(target.ID, expired("m-1")))
	require.NoError(t, store.Deliver(target.ID, expired("m-2")))

	// A live message must survive the sweep and never be reported.
	live := &InboxEntry{
		ID: "m-live", Payload: []byte(`{"task":"live"}`), Sender: "foreman",
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, store.Deliver(target.ID, live))
	// A never-expiring message (infinity) must also survive.
	never := 0
	require.NoError(t, store.Deliver(other.ID, &InboxEntry{
		ID: "m-never", Payload: []byte(`{}`), TTLSeconds: &never,
	}))

	reported := map[string]*InboxEntry{}
	removed := store.PurgeExpiredReport(func(agentID string, entry *InboxEntry) {
		require.Equal(t, target.ID, agentID)
		reported[entry.ID] = entry
	})

	require.Equal(t, 2, removed)
	require.Len(t, reported, 2)
	require.Equal(t, "foreman", reported["m-1"].Sender, "the report carries the sender a receipt needs")
	require.Equal(t, "k-m-1", reported["m-1"].IdempotencyKey)
	require.JSONEq(t, `{"task":"x"}`, string(reported["m-1"].Payload),
		"the report carries the payload the dead letter preserves")

	// The inbox now holds only the live message.
	msgs, _, err := store.Retrieve(target.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "m-live", msgs[0].ID)
}

func TestPostgresOwnership_DeadLetterAppendIsIdempotentAndNewestFirst(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "dead")

	base := time.Now().UTC().Truncate(time.Millisecond)
	record := func(id string, at time.Time) *DeadLetter {
		return &DeadLetter{
			MessageID:      id,
			AgentID:        agent.ID,
			Sender:         "foreman",
			Payload:        []byte(`{"n":1}`),
			CreatedAt:      at.Add(-2 * time.Hour),
			ExpiredAt:      at.Add(-time.Hour),
			DeadLetteredAt: at,
			Reason:         DeadLetterReasonExpired,
			IdempotencyKey: "k-" + id,
		}
	}

	added, err := store.AppendDeadLetter(record("m-1", base))
	require.NoError(t, err)
	require.True(t, added)

	added, err = store.AppendDeadLetter(record("m-1", base))
	require.NoError(t, err)
	require.False(t, added, "the primary key makes a redelivered report a no-op")

	added, err = store.AppendDeadLetter(record("m-2", base.Add(time.Second)))
	require.NoError(t, err)
	require.True(t, added)

	list, err := store.ListDeadLetters(agent.ID, 10)
	require.NoError(t, err)
	require.Len(t, list, 2, "the duplicate was not recorded twice")
	require.Equal(t, "m-2", list[0].MessageID, "newest first")
	require.Equal(t, "m-1", list[1].MessageID)
	require.Equal(t, "foreman", list[0].Sender)
	require.Equal(t, "k-m-2", list[0].IdempotencyKey)
	require.True(t, list[0].ExpiredAt.Equal(record("m-2", base.Add(time.Second)).ExpiredAt))
	require.JSONEq(t, `{"n":1}`, string(list[0].Payload))

	// An id that has no records answers an empty page, not an error.
	empty, err := store.ListDeadLetters("nobody", 10)
	require.NoError(t, err)
	require.Empty(t, empty)

	// The destination is not keyed to a live agent row: unregistering the target
	// leaves its records readable.
	require.NoError(t, store.Unregister(agent.ID))
	after, err := store.ListDeadLetters(agent.ID, 10)
	require.NoError(t, err)
	require.Len(t, after, 2)
}

func TestPostgresOwnership_RetentionSweepsOldDeadLetters(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "retention")

	old := time.Now().UTC().Add(-DefaultDeadLetterRetention - time.Hour)
	fresh := time.Now().UTC()
	for _, rec := range []*DeadLetter{
		{MessageID: "m-old", AgentID: agent.ID, Payload: []byte(`{}`),
			CreatedAt: old.Add(-time.Hour), ExpiredAt: old, DeadLetteredAt: old,
			Reason: DeadLetterReasonExpired},
		{MessageID: "m-fresh", AgentID: agent.ID, Payload: []byte(`{}`),
			CreatedAt: fresh.Add(-time.Hour), ExpiredAt: fresh, DeadLetteredAt: fresh,
			Reason: DeadLetterReasonExpired},
	} {
		added, err := store.AppendDeadLetter(rec)
		require.NoError(t, err)
		require.True(t, added)
	}

	require.Zero(t, store.PurgeExpired(), "nothing in the inboxes to purge")

	list, err := store.ListDeadLetters(agent.ID, 10)
	require.NoError(t, err)
	require.Len(t, list, 1, "the retention window bounds the destination by time")
	require.Equal(t, "m-fresh", list[0].MessageID)
}

func TestPostgresOwnership_TransferMovesAStuckLease(t *testing.T) {
	store := newTestStore(t)
	holder := newTestAgent(t, store, "holder")
	relief := newTestAgent(t, store, "relief")

	entry := &InboxEntry{
		ID: "m-stuck", Payload: []byte(`{"task":"build"}`), Sender: "foreman",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
		TTLSeconds: ptrInt(3600),
	}
	require.NoError(t, store.Deliver(holder.ID, entry))

	_, leaseID, err := store.Retrieve(holder.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.NotEmpty(t, leaseID)

	moved, err := store.Transfer(holder.ID, leaseID, []string{"m-stuck"}, relief.ID, false)
	require.NoError(t, err)
	require.Equal(t, 1, moved)

	// The destination holds it, unleased and claimable, with identity intact.
	msgs, newLease, err := store.Retrieve(relief.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotEmpty(t, newLease)
	require.Equal(t, "m-stuck", msgs[0].ID)
	require.Equal(t, relief.ID, msgs[0].AgentID)
	require.Equal(t, "foreman", msgs[0].Sender)
	require.JSONEq(t, `{"task":"build"}`, string(msgs[0].Payload))

	// The old holder can no longer ack it.
	err = store.Ack(holder.ID, leaseID, []string{"m-stuck"})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrMessageNotFound)
}

func TestPostgresOwnership_TransferRefusesForeignAndUnknown(t *testing.T) {
	store := newTestStore(t)
	holder := newTestAgent(t, store, "holder")
	relief := newTestAgent(t, store, "relief")

	live := &InboxEntry{
		ID: "m-1", Payload: []byte(`{"n":1}`), CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour), TTLSeconds: ptrInt(3600),
	}
	require.NoError(t, store.Deliver(holder.ID, live))
	_, leaseID, err := store.Retrieve(holder.ID, 30*time.Second, 10)
	require.NoError(t, err)

	// A foreign lease (including none at all) is refused without force...
	_, err = store.Transfer(holder.ID, "not-the-lease", []string{"m-1"}, relief.ID, false)
	require.ErrorIs(t, err, ErrLeaseConflict)
	// ...an unknown message is refused...
	_, err = store.Transfer(holder.ID, leaseID, []string{"ghost"}, relief.ID, false)
	require.ErrorIs(t, err, ErrMessageNotFound)
	// ...a partial batch moves NOTHING (the known message must still be the
	// holder's)...
	_, err = store.Transfer(holder.ID, leaseID, []string{"m-1", "ghost"}, relief.ID, false)
	require.ErrorIs(t, err, ErrMessageNotFound)
	// ...an unknown target is refused...
	_, err = store.Transfer(holder.ID, leaseID, []string{"m-1"}, "nobody", false)
	require.ErrorIs(t, err, ErrAgentNotFound)
	// ...and the message is still exactly where it was. (It is still LEASED by
	// the retrieve above, so a second retrieve of the same inbox correctly
	// returns nothing — the queue depth is what proves it never left.)
	depth, _, _, err := store.Stats(holder.ID)
	require.NoError(t, err)
	require.Equal(t, 1, depth, "a refused transfer leaves the message in the holder's inbox")
	reliefDepth, _, _, err := store.Stats(relief.ID)
	require.NoError(t, err)
	require.Zero(t, reliefDepth, "nothing reached the destination")

	// A forced move needs no lease and clears it.
	moved, err := store.Transfer(holder.ID, "", []string{"m-1"}, relief.ID, true)
	require.NoError(t, err)
	require.Equal(t, 1, moved)
	relieved, _, err := store.Retrieve(relief.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, relieved, 1)
	require.Equal(t, "m-1", relieved[0].ID)
}

func TestPostgresOwnership_TransferMovesUnleasedWithoutForce(t *testing.T) {
	store := newTestStore(t)
	holder := newTestAgent(t, store, "holder")
	relief := newTestAgent(t, store, "relief")

	// Never retrieved: claimable by anyone, so moving it displaces nobody.
	require.NoError(t, store.Deliver(holder.ID, &InboxEntry{
		ID: "m-free", Payload: []byte(`{}`), CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(time.Hour), TTLSeconds: ptrInt(3600),
	}))

	moved, err := store.Transfer(holder.ID, "", []string{"m-free"}, relief.ID, false)
	require.NoError(t, err)
	require.Equal(t, 1, moved)

	msgs, _, err := store.Retrieve(relief.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "m-free", msgs[0].ID)
}

// ptrInt is the TTLSeconds helper for these tests.
func ptrInt(n int) *int { return &n }
