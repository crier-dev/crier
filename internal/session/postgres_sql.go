package session

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	// The pgx database/sql adapter. The server already links pgx through its
	// pgxpool registry store, so this adds no new dependency — it is the
	// second engine driven through the SAME database/sql repository.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// postgresDriverName is the driver name pgx/v5/stdlib registers with
// database/sql.
const postgresDriverName = "pgx"

// NewPostgresSQLStore opens a server-backed session view over the shared
// SQLStore (CR-CHAT-006) and applies the schema. It is the PostgreSQL adapter
// that proves the "one repository, two engines" requirement: the projection,
// the keep-LAST upsert and the State assembly are the exact code SQLite runs —
// only the placeholder syntax and the DDL differ.
//
// It is NOT a rewrite of the shipped pgxpool-backed PostgresStore, which stays
// in place for the registry's shared pool path. Both target the same §5.2 view
// names, so a deployment selects ONE engine per database. The caller owns the
// returned store and must Close it.
func NewPostgresSQLStore(ctx context.Context, connString string) (*SQLStore, error) {
	if strings.TrimSpace(connString) == "" {
		return nil, fmt.Errorf("session postgres store: %w: connection string is empty", ErrInvalidRecord)
	}
	db, err := sql.Open(postgresDriverName, connString)
	if err != nil {
		return nil, fmt.Errorf("session postgres store: open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("session postgres store: ping: %w", err)
	}
	store := newSQLStore(db, postgresDialect{}, true)
	if err := store.ApplySchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}
