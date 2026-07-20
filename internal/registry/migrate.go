package registry

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // registers database/sql driver "pgx"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// RunMigrations applies all embedded SQL migrations against the PostgreSQL
// database identified by connString. It uses golang-migrate with an iofs
// source to read migrations from the embedded migrations/ directory.
// Returns nil on success or ErrNoChange when migrations are already current.
func RunMigrations(ctx context.Context, connString string) error {
	if connString == "" {
		return fmt.Errorf("migrations: %w: database URL is empty", ErrInvalidStoreInput)
	}

	db, err := sql.Open("pgx", connString)
	if err != nil {
		return fmt.Errorf("open migration database: %w", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping migration database: %w", err)
	}

	source, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	driver, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
	if err != nil {
		return fmt.Errorf("create migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
