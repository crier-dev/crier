package session

import (
	"context"
	"database/sql"
	"fmt"
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

// NewSQLiteStore opens (creating when absent) a SQLite-backed session view and
// applies the schema. It is the SQLite adapter over the shared SQLStore
// (CR-CHAT-006): no PostgreSQL URL, no service, and no CGO are required.
//
// path is a database file (a relative path is resolved against the process
// working directory) or ":memory:" for an ephemeral view. The caller owns the
// returned store and must Close it.
func NewSQLiteStore(path string) (*SQLStore, error) {
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

	store := newSQLStore(db, sqliteDialect{}, true)
	if err := store.ApplySchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}
