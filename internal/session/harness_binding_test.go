package session

// COURIER-HARNESS §4.3: the harness binding lives on the courier session
// record, so it must survive BOTH persistence shapes — the JSONL append log and
// the SQL view (SQLite here; PostgreSQL shares the projection code).

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func harnessTestSession(id string) *Session {
	return &Session{
		ID:               id,
		Namespace:        "crier",
		Kind:             KindChannel,
		Title:            "harness bound session",
		CreatedAt:        time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC),
		CreatedBy:        AuthorRef{Agent: "alpha"},
		State:            SessionOpen,
		Harness:          "opencode",
		HarnessSessionID: "ses_7f2c",
	}
}

func TestJSONL_HarnessBindingRoundTrips(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	require.NoError(t, err)
	ctx := context.Background()

	sess := harnessTestSession("s_harness_jsonl")
	require.NoError(t, store.CreateSession(ctx, sess, 1))

	// The create line carries the binding fields by name, so an operator can
	// read it in the log without the fold.
	records, err := store.Records(ctx, "s_harness_jsonl")
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "opencode", records[0].Harness)
	require.Equal(t, "ses_7f2c", records[0].HarnessSessionID)

	loaded, err := store.Session(ctx, "s_harness_jsonl")
	require.NoError(t, err)
	require.Equal(t, "opencode", loaded.Harness)
	require.Equal(t, "ses_7f2c", loaded.HarnessSessionID)
}

func TestSQLite_HarnessBindingRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.sqlite")
	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, store.CreateSession(ctx, harnessTestSession("s_harness_sql"), 1))

	loaded, err := store.Session(ctx, "s_harness_sql")
	require.NoError(t, err)
	require.Equal(t, "opencode", loaded.Harness)
	require.Equal(t, "ses_7f2c", loaded.HarnessSessionID)
	require.NoError(t, store.Close())

	// A restart reads the same binding back from the view.
	reopened, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	again, err := reopened.Session(ctx, "s_harness_sql")
	require.NoError(t, err)
	require.Equal(t, "opencode", again.Harness)
	require.Equal(t, "ses_7f2c", again.HarnessSessionID)
}

func TestSQLite_StoreOnlySessionHasEmptyBinding(t *testing.T) {
	store := newTestSQLiteStore(t)
	ctx := context.Background()
	sess := harnessTestSession("s_store_only")
	sess.Harness = ""
	sess.HarnessSessionID = ""
	require.NoError(t, store.CreateSession(ctx, sess, 1))

	loaded, err := store.Session(ctx, "s_store_only")
	require.NoError(t, err)
	require.Empty(t, loaded.Harness, "a store-only session must not acquire a harness")
	require.Empty(t, loaded.HarnessSessionID)
}
