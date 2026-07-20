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
	assert.Equal(t, 2, version, "should reflect the two embedded migration files")
	assert.False(t, dirty, "migrations should not be marked dirty")
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
		DROP TABLE IF EXISTS inbox_entries, agents, schema_migrations CASCADE;
	`)
	require.NoError(t, err)
}
