package session

// PERF-001 acceptance battery for the SQLite journal mode: a file-backed store
// must come up in WAL, and every refusal of WAL must keep the rollback journal
// WITH a logged reason while still opening and serving a create. The fallback is
// the arm a portable store cannot drop (docs/performance.md "Remediation" 1), so
// it is exercised here rather than assumed.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testLogger returns a debug-level logger writing into buf, so a test can read
// back the journal-mode decision instead of taking it on faith. It is passed
// through sqliteOpenOptions rather than slog.SetDefault because the package's
// other tests run in parallel and must not share a logger.
func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// pragmaJournalMode asks the store's own connection which journal mode the
// database is actually in. It is the question the acceptance criterion asks:
// not "did we issue a pragma" but "is the pragma in force".
func pragmaJournalMode(t *testing.T, store *SQLStore) string {
	t.Helper()
	var mode string
	require.NoError(t, store.DB().QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode))
	return strings.ToLower(strings.TrimSpace(mode))
}

// serveOneCreate appends a real session-create record (plus one member) through
// the store and reads the session back, so "the store still serves a create" is
// a durable write followed by a read rather than an open that merely succeeded.
func serveOneCreate(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	recs := buildScenario()
	require.NoError(t, store.Append(ctx, recs[0]))
	require.NoError(t, store.Append(ctx, recs[1]))
	st, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	require.Equal(t, fixSessionID, st.Session.ID)
	require.Len(t, st.Members, 1)
}

// ---------------------------------------------------------------------------
// Arm 1: a file database comes up in WAL
// ---------------------------------------------------------------------------

func TestSQLite_WALArmedOnFileDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "wal.sqlite")

	// Assert the file's DEFAULT first: journal mode is persisted in the
	// database header, so "wal" after the fact would prove nothing about
	// NewSQLiteStore unless the same file was in rollback mode before it.
	raw, err := sql.Open(sqliteDriverName, path)
	require.NoError(t, err)
	var before string
	require.NoError(t, raw.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&before))
	require.NoError(t, raw.Close())
	require.Equal(t, "delete", strings.ToLower(strings.TrimSpace(before)),
		"a fresh SQLite file ships in rollback-journal mode")

	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.Equal(t, "wal", pragmaJournalMode(t, store),
		"NewSQLiteStore must arm journal_mode=WAL on a file database")
	serveOneCreate(t, store)
}

// ---------------------------------------------------------------------------
// Arm 2: refusals keep the rollback journal, log why, and still serve a create
// ---------------------------------------------------------------------------

func TestSQLite_MemoryStoreKeepsRollbackJournal(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	store, err := newSQLiteStore(sqliteInMemoryName, sqliteOpenOptions{logger: testLogger(&logs)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// An in-memory database has no file to put a -wal beside; SQLite answers
	// "memory" and the store keeps it rather than failing the open.
	require.Equal(t, "memory", pragmaJournalMode(t, store))
	serveOneCreate(t, store)
	require.Contains(t, logs.String(), "in-memory database has no file",
		"the fallback is logged, not a silent no-op")
}

func TestSQLite_SidecarRefusalKeepsRollbackJournal(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	path := filepath.Join(t.TempDir(), "readonly.sqlite")

	// A filesystem that cannot host -wal/-shm. The failure is injected at the
	// probe because the real condition cannot be arranged portably: a
	// directory SQLite cannot create files in refuses the rollback journal's
	// own file too, so the store would not open at all. The real probe's
	// refusal is proven separately, in
	// TestSQLite_SidecarProbeRejectsUnwritableDir — here the question is what
	// the open does with a refusal.
	refused := errors.New("injected: filesystem cannot host -wal/-shm")
	store, err := newSQLiteStore(path, sqliteOpenOptions{
		probeSidecar: func(string) error { return refused },
		logger:       testLogger(&logs),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.Equal(t, "delete", pragmaJournalMode(t, store),
		"a refused WAL must leave the shipped rollback journal in place")
	serveOneCreate(t, store)
	logged := logs.String()
	require.Contains(t, logged, "cannot host the -wal/-shm sidecar files")
	require.Contains(t, logged, "level=WARN")
}

func TestSQLite_PragmaErrorIsAFallbackNotAnOpenFailure(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	path := filepath.Join(t.TempDir(), "pragma-error.sqlite")

	// A closed handle makes every query fail the way a driver or filesystem
	// refusal of `PRAGMA journal_mode=WAL` does. The contract under test is
	// that such a refusal is the FALLBACK arm (rollback journal kept, reason
	// logged) and never an error out of the journal-mode decision.
	closed, err := sql.Open(sqliteDriverName, path)
	require.NoError(t, err)
	require.NoError(t, closed.Close())

	mode, err := selectSQLiteJournalMode(context.Background(), closed, path,
		sqliteOpenOptions{logger: testLogger(&logs)})
	require.NoError(t, err, "a refused WAL is a fallback, not an open failure")
	require.Equal(t, sqliteJournalRollback, mode)
	require.Contains(t, logs.String(), "SQLite refused WAL")
}

// ---------------------------------------------------------------------------
// The real probe: the sidecar decision is a filesystem fact
// ---------------------------------------------------------------------------

func TestSQLite_SidecarProbeRejectsUnwritableDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the probe cannot fail")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	require.Error(t, probeSQLiteSidecar(dir),
		"a directory that cannot create a file cannot host -wal/-shm")
	require.NoError(t, probeSQLiteSidecar(t.TempDir()))
}

// TestSQLite_SidecarDirResolvesTheDatabaseDirectory pins the path arithmetic the
// probe depends on: the sidecars land beside the database file, and a "file:"
// DSN's query parameters are not part of that path.
func TestSQLite_SidecarDirResolvesTheDatabaseDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.Equal(t, dir, sqliteSidecarDir(filepath.Join(dir, "sessions.sqlite")))
	require.Equal(t, dir, sqliteSidecarDir("file:"+filepath.Join(dir, "sessions.sqlite")+"?cache=shared"))
	require.Equal(t, ".", sqliteSidecarDir("sessions.sqlite"))
}
