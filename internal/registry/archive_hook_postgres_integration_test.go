//go:build integration

package registry

// archive_hook_postgres_integration_test.go — the DURABLE half of the
// archive-then-delete contract (specs/DUCKBRAIN-ARCHIVE.md R-D2, "delete only
// after a 2xx archive"), against a real PostgreSQL.
//
// The invariant was already pinned for SQLite and for the httptest DuckBrain
// store (internal/archive/archive_integration_test.go and
// archive_order_integration_test.go). PostgresStore — the backend the docs
// recommend (CR_DATABASE_URL) — had no test in either direction, so it could
// regress to delete-before-archive with nothing going red.
//
// What is measured here, stated honestly:
//
//   - direction (a): an expired dead letter whose archive write SUCCEEDS is
//     purged, the archive write is committed BEFORE the retention DELETE
//     (observed on a SECOND connection while the callback is still running),
//     and the archived copy outlives the hot row;
//   - direction (b): an archive write that really FAILS (a database error, not
//     a stub returning false) deletes nothing — the expired record survives;
//   - the production composition: the same two directions driven through
//     Handler.PurgeExpired over an AuthorityStore, because that is the store the
//     server builds when a service-delivery allowlist is configured.
//
// The archive sink is a table in the same PostgreSQL instance rather than a
// DuckBrain HTTP server: it is a durable write whose commit the database
// decides (autocommit, exactly like the POST's 2xx on the far side) and whose
// failure the database can inject deterministically. The DuckBrain POST itself
// is covered by internal/archive's tests; importing that package from
// package registry is impossible (internal/archive imports internal/registry),
// so this file pins the store's side of the contract, which is the side that
// performs the delete.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// archiveSinkDDL is the stand-in archive destination. Its columns are the
// record's identity and body, so an assertion can prove the archived copy is
// the dead letter that was purged rather than some other row.
const archiveSinkDDL = `
CREATE TABLE test_dead_letter_archive (
	message_id TEXT PRIMARY KEY,
	agent_id   TEXT NOT NULL,
	payload    JSONB NOT NULL,
	archived_at TIMESTAMPTZ NOT NULL
)`

const archiveSinkWrite = `
INSERT INTO test_dead_letter_archive (message_id, agent_id, payload, archived_at)
VALUES ($1, $2, $3::jsonb, $4)`

// postgresArchiveSink creates the sink table and removes it (and any trigger a
// test added to it) when the test ends.
func postgresArchiveSink(t *testing.T, store *PostgresStore) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := store.pool.Exec(ctx, archiveSinkDDL)
	require.NoError(t, err)
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer dropCancel()
		_, dropErr := store.pool.Exec(dropCtx, `DROP TABLE IF EXISTS test_dead_letter_archive CASCADE`)
		require.NoError(t, dropErr)
		_, fnErr := store.pool.Exec(dropCtx, `DROP FUNCTION IF EXISTS reject_test_archive() CASCADE`)
		require.NoError(t, fnErr)
	})
}

// deadLetterCount reads a count on the pool it is given; the ordering assertion
// below passes the OBSERVER pool so the claim is a second connection's view.
func deadLetterCount(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) (int, error) {
	var n int
	err := pool.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

// storeCount reads a count through the store's own pool, for assertions made
// after the sweep (no ordering claim, so the sweep's connection is enough).
func storeCount(ctx context.Context, store *PostgresStore, query string, args ...any) (int, error) {
	var n int
	err := store.pool.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

// expiredDeadLetter builds a record past the retention window (and a fresh one
// that must never be touched) for the given agent.
func expiredDeadLetter(messageID, agentID string) *DeadLetter {
	old := time.Now().Add(-DefaultDeadLetterRetention - time.Hour).UTC()
	return &DeadLetter{
		MessageID: messageID, AgentID: agentID, Payload: []byte(`{"k":"v"}`),
		CreatedAt: old, ExpiredAt: old, DeadLetteredAt: old,
		Reason: DeadLetterReasonExpired,
	}
}

func freshDeadLetter(messageID, agentID string) *DeadLetter {
	now := time.Now().UTC()
	return &DeadLetter{
		MessageID: messageID, AgentID: agentID, Payload: []byte(`{"k":"fresh"}`),
		CreatedAt: now, ExpiredAt: now, DeadLetteredAt: now,
		Reason: DeadLetterReasonExpired,
	}
}

// TestPostgresArchiveHook_ArchiveCommittedBeforeRetentionDelete is direction
// (a). The archive callback reads the dead-letter table through a SEPARATE
// pool while it runs: "the record still exists at the instant it is archived"
// is measured there, not inferred from a final count, and the archive row is
// read back on that same second connection before the callback returns — so
// the write the delete depends on is committed, not pending.
func TestPostgresArchiveHook_ArchiveCommittedBeforeRetentionDelete(t *testing.T) {
	store := newTestStore(t)
	postgresArchiveSink(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	observer, err := pgxpool.New(ctx, testConnString)
	require.NoError(t, err)
	defer observer.Close()

	for _, dl := range []*DeadLetter{
		expiredDeadLetter("expired", "archive-agent"),
		freshDeadLetter("fresh", "archive-agent"),
	} {
		added, appendErr := store.AppendDeadLetter(dl)
		require.NoError(t, appendErr)
		require.True(t, added, "fixture dead letter %s", dl.MessageID)
	}

	var presentAtArchive, archivedAtArchive int
	removed := store.PurgeExpiredDeadLetters(func(dl *DeadLetter) bool {
		var qErr error
		presentAtArchive, qErr = deadLetterCount(ctx, observer,
			`SELECT count(*) FROM dead_letters WHERE message_id = $1`, dl.MessageID)
		if qErr != nil {
			t.Logf("observer read failed while archiving %s: %v", dl.MessageID, qErr)
			return false
		}
		// The archive write: committed by the database when this returns nil.
		if _, execErr := observer.Exec(ctx, archiveSinkWrite,
			dl.MessageID, dl.AgentID, dl.Payload, time.Now().UTC()); execErr != nil {
			t.Logf("archive write failed for %s: %v", dl.MessageID, execErr)
			return false
		}
		// Read the archived row back on the same independent connection BEFORE
		// the retention delete can run: archive-committed is observable here.
		archivedAtArchive, qErr = deadLetterCount(ctx, observer,
			`SELECT count(*) FROM test_dead_letter_archive WHERE message_id = $1`, dl.MessageID)
		return qErr == nil && archivedAtArchive == 1
	})

	require.Equal(t, 1, removed, "the archived expired record is the only one removed")
	require.Equal(t, 1, presentAtArchive,
		"ORDERING: the dead letter must still exist WHILE it is being archived (archive precedes the retention DELETE)")
	require.Equal(t, 1, archivedAtArchive,
		"the archive write must be committed by the time the callback returns")

	remaining, err := store.ListDeadLetters("archive-agent", 10)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "only the retention-expired record is swept")
	require.Equal(t, "fresh", remaining[0].MessageID,
		"a dead letter inside its retention window must never be archived or deleted")

	archived, err := deadLetterCount(ctx, observer,
		`SELECT count(*) FROM test_dead_letter_archive WHERE message_id = $1`, "expired")
	require.NoError(t, err)
	require.Equal(t, 1, archived, "the archived copy survives the deletion of the hot row")
	hot, err := deadLetterCount(ctx, observer,
		`SELECT count(*) FROM dead_letters WHERE message_id = $1`, "expired")
	require.NoError(t, err)
	require.Equal(t, 0, hot, "after the 2xx-equivalent archive the hot row is gone")
}

// TestPostgresArchiveHook_FailedArchiveRetainsDeadLetter is direction (b), with
// a REAL failure: a trigger raises on the archive insert, the callback returns
// the write's error upstream as `false`, and nothing may be deleted. A purge
// that deleted first, or that ignored the callback's verdict, reds here.
func TestPostgresArchiveHook_FailedArchiveRetainsDeadLetter(t *testing.T) {
	store := newTestStore(t)
	postgresArchiveSink(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := store.pool.Exec(ctx, `
CREATE FUNCTION reject_test_archive() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	RAISE EXCEPTION 'injected archive failure';
END;
$$;
CREATE TRIGGER reject_test_archive BEFORE INSERT ON test_dead_letter_archive
	FOR EACH ROW EXECUTE FUNCTION reject_test_archive();`)
	require.NoError(t, err)

	added, err := store.AppendDeadLetter(expiredDeadLetter("retain-me", "archive-agent"))
	require.NoError(t, err)
	require.True(t, added)

	var archiveErr error
	removed := store.PurgeExpiredDeadLetters(func(dl *DeadLetter) bool {
		_, archiveErr = store.pool.Exec(ctx, archiveSinkWrite,
			dl.MessageID, dl.AgentID, dl.Payload, time.Now().UTC())
		return archiveErr == nil
	})

	require.Error(t, archiveErr, "the injected archive failure must be a real database error, not a stub")
	require.Zero(t, removed, "nothing is deleted when the archive write fails")

	remaining, err := store.ListDeadLetters("archive-agent", 10)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "the retention-expired dead letter must survive a failed archive")
	require.Equal(t, "retain-me", remaining[0].MessageID)
	require.JSONEq(t, `{"k":"v"}`, string(remaining[0].Payload),
		"the surviving record is the original body")

	archived, err := storeCount(ctx, store, `SELECT count(*) FROM test_dead_letter_archive`)
	require.NoError(t, err)
	require.Zero(t, archived, "a failed archive leaves nothing in the archive sink")
}

// TestPostgresArchiveHook_HandlerSweepConsultsHookThroughAuthorityStore drives
// the production composition — Handler over AuthorityStore over PostgresStore,
// which is what composeRegistryDelivery builds when a service-delivery allowlist
// is configured — in both directions. The wrapper must keep the archive
// capability visible; without it the sweep never runs at all and the record is
// retained forever with no archive, which is the silent half of the same
// defect.
func TestPostgresArchiveHook_HandlerSweepConsultsHookThroughAuthorityStore(t *testing.T) {
	store := newTestStore(t)
	postgresArchiveSink(t, store)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	added, err := store.AppendDeadLetter(expiredDeadLetter("swept", "archive-agent"))
	require.NoError(t, err)
	require.True(t, added)

	wrapped := NewAuthorityStore(store, nil, "")
	require.Implements(t, (*ArchiveHook)(nil), wrapped,
		"the policy wrapper must not hide the retention sweep from the handler")
	handler := NewHandler(wrapped)

	var seen []string
	// Refusal: the archive write does not happen (and reports failure), so the
	// sweep must keep the record.
	handler.SetArchiveHook(func(*DeadLetter) bool {
		seen = append(seen, "refused")
		return false
	})
	handler.PurgeExpired()
	require.Equal(t, []string{"refused"}, seen, "the sweep must consult the hook exactly once per expired record")
	retained, err := store.ListDeadLetters("archive-agent", 10)
	require.NoError(t, err)
	require.Len(t, retained, 1, "a refused archive retains the record on the durable backend")

	// Acceptance: the archive write is committed, then the row goes.
	handler.SetArchiveHook(func(dl *DeadLetter) bool {
		seen = append(seen, dl.MessageID)
		if _, execErr := store.pool.Exec(ctx, archiveSinkWrite,
			dl.MessageID, dl.AgentID, dl.Payload, time.Now().UTC()); execErr != nil {
			t.Logf("archive write failed for %s: %v", dl.MessageID, execErr)
			return false
		}
		return true
	})
	handler.PurgeExpired()
	require.Equal(t, []string{"refused", "swept"}, seen)

	purged, err := store.ListDeadLetters("archive-agent", 10)
	require.NoError(t, err)
	require.Empty(t, purged, "an accepted archive purges the expired record through the production sweep")

	archived, err := storeCount(ctx, store, `SELECT count(*) FROM test_dead_letter_archive`)
	require.NoError(t, err)
	require.Equal(t, 1, archived, "exactly one archived copy of the swept record")
}
