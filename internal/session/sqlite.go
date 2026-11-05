package session

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	// modernc.org/sqlite is a PURE-GO SQLite implementation (no CGO, no
	// cgo toolchain, no server): the driver that makes "a user does not have
	// to run PostgreSQL" true. Imported for its database/sql driver
	// registration only.
	_ "modernc.org/sqlite"
)

// sqliteDriverName is the driver name modernc.org/sqlite registers with
// database/sql.
const sqliteDriverName = "sqlite"

// The two journal modes this store runs in. WAL is the applied one; rollback is
// the shipped default the store keeps when WAL cannot be armed (see
// selectSQLiteJournalMode).
const (
	sqliteJournalWAL      = "wal"
	sqliteJournalRollback = "delete"
)

// sqliteOpenOptions carries the seams a test needs to redirect without touching
// process-wide state. The zero value is the production behaviour: probe the real
// filesystem, log to slog's default logger.
type sqliteOpenOptions struct {
	// probeSidecar reports whether dir can host the -wal/-shm sidecar files.
	// nil means probeSQLiteSidecar, the real filesystem probe.
	probeSidecar func(dir string) error
	// logger receives the journal-mode decision. nil means slog.Default().
	logger *slog.Logger
}

// NewSQLiteStore opens (creating when absent) a SQLite-backed session view and
// applies the schema. It is the SQLite adapter over the shared SQLStore
// (CR-CHAT-006): no PostgreSQL URL, no service, and no CGO are required.
//
// path is a database file (a relative path is resolved against the process
// working directory) or ":memory:" for an ephemeral view. The caller owns the
// returned store and must Close it.
//
// A file-backed database is put in WAL journal mode before the schema is
// applied: the shipped rollback journal fsyncs a journal create + delete per
// commit, and ONE POST /sessions performs 5-6 of them (docs/performance.md
// "Analysis" §1). The mode is chosen by selectSQLiteJournalMode, which keeps the
// rollback journal — with a logged reason, never silently — when WAL is refused.
func NewSQLiteStore(path string) (*SQLStore, error) {
	return newSQLiteStore(path, sqliteOpenOptions{})
}

// newSQLiteStore is NewSQLiteStore with the test seams spelled out.
func newSQLiteStore(path string, opts sqliteOpenOptions) (*SQLStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("session sqlite store: %w: sqlite path is empty", ErrInvalidRecord)
	}
	db, err := sql.Open(sqliteDriverName, path)
	if err != nil {
		return nil, fmt.Errorf("session sqlite store: open: %w", err)
	}
	// ONE connection: SQLite admits a single writer, and a single pooled
	// connection keeps the per-connection pragmas (foreign_keys,
	// busy_timeout, synchronous) in force for every statement instead of
	// being reset by a fresh connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// Before the schema: the DDL below runs in one transaction, and under WAL
	// that transaction is a few appends to the -wal file instead of a journal
	// create + fsync + unlink.
	if _, err := selectSQLiteJournalMode(context.Background(), db, path, opts); err != nil {
		_ = db.Close()
		return nil, err
	}

	store := newSQLStore(db, sqliteDialect{}, true)
	if err := store.ApplySchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// selectSQLiteJournalMode arms WAL on a file-backed database and reports the
// journal mode the database is actually in. It returns an error only when the
// journal-mode decision could not be made at all; every REFUSAL of WAL is a
// fallback (rollback journal kept) carrying a logged reason.
//
// journal_mode cannot ride sqliteDialect.InitStatements: the pragma returns a
// row, and InitStatements is a list of row-less statements. It is therefore a
// QueryRow at open, on the same single connection every later statement uses.
func selectSQLiteJournalMode(ctx context.Context, db *sql.DB, path string, opts sqliteOpenOptions) (string, error) {
	logger := opts.logger
	if logger == nil {
		logger = slog.Default()
	}

	// :memory: has no file, so there is nowhere to put -wal/-shm; SQLite keeps
	// its in-memory "memory" journal. Asking is harmless but the reason would
	// be recorded as "refused" rather than "not applicable".
	if sqliteInMemory(path) {
		logger.Info("session sqlite store: rollback journal kept",
			"reason", "in-memory database has no file for the -wal/-shm sidecars",
			"path", path, "journal_mode", "memory")
		return "memory", nil
	}

	// A directory that cannot create a file cannot host the sidecars either.
	// Probing up front beats discovering it on the first commit: WAL would be
	// armed (or refused) and the failure would surface later as a write error
	// on a store that opened fine.
	probe := opts.probeSidecar
	if probe == nil {
		probe = probeSQLiteSidecar
	}
	if err := probe(sqliteSidecarDir(path)); err != nil {
		logger.Warn("session sqlite store: rollback journal kept",
			"reason", "database directory cannot host the -wal/-shm sidecar files",
			"path", path, "journal_mode", sqliteJournalRollback, "error", err)
		return sqliteJournalRollback, nil
	}

	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=WAL`).Scan(&mode); err != nil {
		// On a filesystem that does not support WAL the pragma reports the
		// refusal as an error, with the database still in rollback mode.
		logger.Warn("session sqlite store: rollback journal kept",
			"reason", "SQLite refused WAL", "path", path,
			"journal_mode", sqliteJournalRollback, "error", err)
		return sqliteJournalRollback, nil
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != sqliteJournalWAL {
		logger.Warn("session sqlite store: rollback journal kept",
			"reason", "SQLite refused WAL", "path", path, "journal_mode", mode)
		return mode, nil
	}
	logger.Info("session sqlite store: WAL journal mode armed",
		"path", path, "journal_mode", mode)
	return mode, nil
}

// probeSQLiteSidecar proves dir accepts the files WAL creates beside the
// database: a -wal and a -shm on DDL/commit, and its own probe here. Create and
// remove are the two operations SQLite needs, so one probe file covers both.
func probeSQLiteSidecar(dir string) error {
	f, err := os.CreateTemp(dir, ".crier-sqlite-wal-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// sqliteSidecarDir is the directory SQLite places the -wal/-shm files in: the
// database file's own directory. A "file:" DSN keeps its parameters after "?",
// which is not part of the path.
func sqliteSidecarDir(path string) string {
	p := strings.TrimPrefix(path, "file:")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if p == "" || p == sqliteInMemoryName {
		return "."
	}
	return filepath.Dir(p)
}

// sqliteInMemoryName is SQLite's in-memory database spelling.
const sqliteInMemoryName = ":memory:"

// sqliteInMemory reports whether path names an in-memory database — either the
// ":memory:" spelling or a "file:" DSN asking for mode=memory.
func sqliteInMemory(path string) bool {
	if strings.TrimSpace(path) == sqliteInMemoryName {
		return true
	}
	if strings.HasPrefix(path, "file:") {
		return strings.HasPrefix(path, "file:"+sqliteInMemoryName) ||
			strings.Contains(path, "mode=memory")
	}
	return false
}
