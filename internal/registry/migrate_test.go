//go:build integration

package registry

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepostgres "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunMigrations_CreatesTables verifies that RunMigrations applies the
// embedded SQL migrations and creates the expected tables (agents, inbox_entries).
func TestRunMigrations_CreatesTables(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(ctx, t)
	defer db.Close()
	clearSchema(ctx, t, db)

	require.NoError(t, RunMigrations(ctx, testConnString))

	var agentsExist bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_name = 'agents'
		)
	`).Scan(&agentsExist)
	require.NoError(t, err)
	assert.True(t, agentsExist, "agents table should exist after migration")

	var inboxExist bool
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_name = 'inbox_entries'
		)
	`).Scan(&inboxExist)
	require.NoError(t, err)
	assert.True(t, inboxExist, "inbox_entries table should exist after migration")
}

// TestRunMigrations_SchemaMigrationsCreated verifies that the golang-migrate
// bookkeeping table (schema_migrations) was created with the correct version
// and is not marked dirty.
func TestRunMigrations_SchemaMigrationsCreated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(ctx, t)
	defer db.Close()
	clearSchema(ctx, t, db)

	require.NoError(t, RunMigrations(ctx, testConnString))

	var (
		version int
		dirty   bool
	)
	err := db.QueryRowContext(ctx, `
		SELECT version, dirty FROM schema_migrations
	`).Scan(&version, &dirty)
	require.NoError(t, err, "schema_migrations table should exist after migration")
	assert.Equal(t, 7, version, "should reflect the seven embedded migration files (006 = CR-FEAT-025 task ownership, 007 = CR-FEAT-035 message priority)")
	assert.False(t, dirty, "migrations should not be marked dirty")
}

// TestRunMigrations_AgentConfigColumns verifies the nullable optional-config
// columns on agents: webhook + guard (003, DF-CRIER-151) and a2a (005,
// INT-A2A-001). A migration applied by a different name would be a no-op on an
// existing database, so the columns — not the file — are what this asserts.
func TestRunMigrations_AgentConfigColumns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(ctx, t)
	defer db.Close()
	clearSchema(ctx, t, db)

	require.NoError(t, RunMigrations(ctx, testConnString))

	for _, col := range []string{"webhook", "guard", "a2a"} {
		var exists bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT FROM information_schema.columns
				WHERE table_name = 'agents' AND column_name = $1
			)
		`, col).Scan(&exists)
		require.NoError(t, err)
		assert.True(t, exists, "agents.%s should exist after migration", col)
	}
}

// TestRunMigrations_KeylessAgentsConstraint verifies 004
// (DF-CRIER-192): after migration the agents.public_key column accepts NULL
// (a keyless agent) while still rejecting a non-null value that is not 32
// bytes, and down-migrating restores the NOT NULL form.
func TestRunMigrations_KeylessAgentsConstraint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(ctx, t)
	defer db.Close()
	clearSchema(ctx, t, db)

	require.NoError(t, RunMigrations(ctx, testConnString))

	// NULL key is storable (keyless agent); empty bytea is NOT (004 keeps
	// the 32-byte rule for non-null values).
	now := time.Now().UTC()
	ins := `INSERT INTO agents (id, public_key, capabilities, status, registered_at, last_seen)
		VALUES ($1, $2, '[]'::jsonb, 'online', $3, $3);`
	_, err := db.ExecContext(ctx, ins, "keyless", nil, now)
	require.NoError(t, err, "NULL public_key (keyless agent) must be storable after 004")
	_, err = db.ExecContext(ctx, ins, "empty", []byte{}, now)
	require.Error(t, err, "an empty (non-null) bytea must still violate the 32-byte CHECK")
	_, err = db.ExecContext(ctx, `DELETE FROM agents WHERE id = 'keyless';`)
	require.NoError(t, err)

	// Down restores NOT NULL: a NULL insert is rejected again. (Re-dropping
	// schema_migrations tables by hand would fight golang-migrate's
	// bookkeeping, so this drives a fresh migrate.NewWithInstance over the
	// same embedded source, down to 3, then back up to 4.)
	source, err := iofs.New(migrationFS, "migrations")
	require.NoError(t, err)
	drv, err := migratepostgres.WithInstance(db, &migratepostgres.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("iofs", source, "postgres", drv)
	require.NoError(t, err)
	defer m.Close()
	require.NoError(t, m.Migrate(3))
	_, err = db.ExecContext(ctx, ins, "null-rejected", nil, now)
	require.Error(t, err, "after 004-down, public_key is NOT NULL again")
	require.NoError(t, m.Migrate(4))
	_, err = db.ExecContext(ctx, ins, "null-ok", nil, now)
	require.NoError(t, err, "after re-running 004 up, NULL is storable again")
}

// TestRunMigrations_Idempotent verifies that running migrations twice succeeds.
// The first call applies the migrations; the second call hits the
// ErrNoChange path inside RunMigrations, which is absorbed and returns nil.
func TestRunMigrations_Idempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db := openTestDB(ctx, t)
	defer db.Close()
	clearSchema(ctx, t, db)

	require.NoError(t, RunMigrations(ctx, testConnString), "first migration run should succeed")
	require.NoError(t, RunMigrations(ctx, testConnString), "second (idempotent) run should also succeed")
}

// TestRunMigrations_EmptyConnString verifies that passing an empty connection
// string returns an ErrInvalidStoreInput.
func TestRunMigrations_EmptyConnString(t *testing.T) {
	err := RunMigrations(context.Background(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidStoreInput)
}

// TestRunMigrations_InvalidConnString verifies that connecting to an
// unreachable database returns an error (not ErrInvalidStoreInput).
func TestRunMigrations_InvalidConnString(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := RunMigrations(ctx,
		"postgres://crier:crier@127.0.0.1:1/crier?sslmode=disable&connect_timeout=1")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidStoreInput)
}

// --- helpers -------------------------------------------------------------------

// openTestDB opens a pgx-backed *sql.DB against the shared test connection
// string. The caller must close the returned DB.
func openTestDB(ctx context.Context, t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", testConnString)
	require.NoError(t, err)
	return db
}

// clearSchema drops every table created by the embedded migrations (plus the
// golang-migrate bookkeeping table) so each test starts from a clean schema.
func clearSchema(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		DROP TABLE IF EXISTS dead_letters, inbox_entries, agents, schema_migrations CASCADE;
	`)
	require.NoError(t, err)
}
