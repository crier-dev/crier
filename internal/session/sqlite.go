package session

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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

// The bounds on the SQLite read pool for a FILE-backed database.
//
// Width 8 is the bound rather than "as many as database/sql will open" because
// the cost this fixes is a QUEUE, not throughput: at 100 concurrent requests a
// one-connection store makes the 100th caller wait ~100 service times, and past
// a small multiple of the CPU count extra connections buy nothing but page-cache
// memory and WAL reader lag. Eight admitted readers took the measured conc100
// p99 from 302ms to ~40ms (docs/performance.md, PERF-004), which is the target
// with headroom; a deployment that wants a different width sets
// CR_SESSION_SQLITE_POOL (a value below 1 or an unparseable one is logged and
// ignored in favour of the default, never silently applied as-is).
const (
	sqlitePoolConns    = 8
	sqlitePoolMaxConns = 64
	sqlitePoolEnv      = "CR_SESSION_SQLITE_POOL"
)

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
//
// A file-backed store also runs a WIDENED read pool (sqlitePoolConns, overridable
// with CR_SESSION_SQLITE_POOL). That is only sound because every setting the
// store depends on rides the driver DSN (sqliteDSN), so a connection the pool
// opens later is armed exactly like the first one; with the pragmas applied once
// at open, a second connection would silently lose foreign-key enforcement and
// the busy timeout. ":memory:" stays at ONE connection: each connection to an
// in-memory database is a DIFFERENT database, so a pool there would hand out
// empty databases.
func NewSQLiteStore(path string) (*SQLStore, error) {
	return newSQLiteStore(path, sqliteOpenOptions{})
}

// newSQLiteStore is NewSQLiteStore with the test seams spelled out.
func newSQLiteStore(path string, opts sqliteOpenOptions) (*SQLStore, error) {
	return newSQLiteStorePooled(path, sqliteRequestedPool(opts), opts)
}

// sqliteRequestedPool reads the configured pool width: the CR_SESSION_SQLITE_POOL
// override when it parses, the package default otherwise. An unusable override is
// logged and ignored rather than applied — a typo must not quietly become a
// one-connection store (the defect this change fixes) or an unbounded one.
func sqliteRequestedPool(opts sqliteOpenOptions) int {
	raw := strings.TrimSpace(os.Getenv(sqlitePoolEnv))
	if raw == "" {
		return sqlitePoolConns
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		sqliteLogger(opts).Warn("session sqlite store: ignoring unusable pool width",
			"env", sqlitePoolEnv, "value", raw, "default", sqlitePoolConns)
		return sqlitePoolConns
	}
	return n
}

// newSQLiteStorePooled is NewSQLiteStore with an explicit pool width, which is
// the seam the equivalence test and the pool1/pool8 benchmark arms use.
func newSQLiteStorePooled(path string, pool int, opts sqliteOpenOptions) (*SQLStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("session sqlite store: %w: sqlite path is empty", ErrInvalidRecord)
	}
	db, err := sql.Open(sqliteDriverName, sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("session sqlite store: open: %w", err)
	}
	// The connection pragmas are on the DSN, so every connection in this pool
	// is armed the moment it is opened — see sqliteDSN. That is what allows a
	// width above 1: SQLite admits one WRITER at a time, but under WAL it
	// admits many concurrent READERS, and reads are what the p99 budget
	// measured (docs/performance.md "Analysis" §2). Writers still serialize
	// inside SQLite, bounded by busy_timeout instead of by this pool.
	conns := sqlitePoolWidth(path, pool, opts)
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)

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

// sqlitePoolWidth is the width actually handed to database/sql. An in-memory
// database is pinned to one connection no matter what was requested (a pooled
// ":memory:" is a different database per connection), and a requested width
// outside [1, sqlitePoolMaxConns] is clamped with a logged reason.
func sqlitePoolWidth(path string, requested int, opts sqliteOpenOptions) int {
	if sqliteInMemory(path) {
		if requested > 1 {
			sqliteLogger(opts).Info("session sqlite store: in-memory database pinned to one connection",
				"path", path, "requested_pool", requested)
		}
		return 1
	}
	if requested < 1 {
		return 1
	}
	if requested > sqlitePoolMaxConns {
		sqliteLogger(opts).Warn("session sqlite store: pool width clamped",
			"requested_pool", requested, "max_pool", sqlitePoolMaxConns)
		return sqlitePoolMaxConns
	}
	return requested
}

// sqliteLogger resolves the logger an open uses: the test seam when set, slog's
// default otherwise.
func sqliteLogger(opts sqliteOpenOptions) *slog.Logger {
	if opts.logger != nil {
		return opts.logger
	}
	return slog.Default()
}

// selectSQLiteJournalMode arms WAL on a file-backed database and reports the
// journal mode the database is actually in. It returns an error only when the
// journal-mode decision could not be made at all; every REFUSAL of WAL is a
// fallback (rollback journal kept) carrying a logged reason.
//
// journal_mode cannot ride sqliteDialect.InitStatements: the pragma returns a
// row, and InitStatements is a list of row-less statements. It is therefore a
// QueryRow at open, before the pool can be used by anything else.
func selectSQLiteJournalMode(ctx context.Context, db *sql.DB, path string, opts sqliteOpenOptions) (string, error) {
	logger := sqliteLogger(opts)

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
