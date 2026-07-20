//go:build integration

package registry

import (
	"context"
	"database/sql"
	"io/fs"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migrationTestDBURL = "postgres://crier:crier@localhost:5437/crier?sslmode=disable"

func TestRunMigrations_Success(t *testing.T) {
	startMigrationPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	require.NoError(t, RunMigrations(ctx, migrationTestDBURL))

	db, err := sql.Open("pgx", migrationTestDBURL)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	var (
		version int
		dirty   bool
	)
	err = db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)
	require.NoError(t, err)
	assert.Equal(t, 2, version)
	assert.False(t, dirty)

	var rowCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&rowCount))
	assert.Equal(t, 1, rowCount, "golang-migrate stores only the current schema version")

	require.NoError(t, RunMigrations(ctx, migrationTestDBURL), "running current migrations must be a no-op")
}

func TestRunMigrations_EmptyConnString(t *testing.T) {
	err := RunMigrations(context.Background(), "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidStoreInput)
}

func TestRunMigrations_InvalidConnString(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := RunMigrations(ctx, "postgres://crier:crier@127.0.0.1:1/crier?sslmode=disable&connect_timeout=1")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidStoreInput)
}

func TestEmbeddedMigrations_Exist(t *testing.T) {
	for _, name := range []string{
		"001_create_agents.up.sql",
		"001_create_agents.down.sql",
		"002_create_inbox_entries.up.sql",
		"002_create_inbox_entries.down.sql",
	} {
		t.Run(name, func(t *testing.T) {
			info, err := fs.Stat(migrationFS, "migrations/"+name)
			require.NoError(t, err)
			assert.False(t, info.IsDir())
		})
	}
}

func startMigrationPostgres(t *testing.T) {
	t.Helper()

	compose := composeCommand(t)
	composeFile, err := filepath.Abs(filepath.Join("..", "..", "docker-compose.yml"))
	require.NoError(t, err)

	psOutput, _ := composeOutput(compose, composeFile, "ps", "-q", "postgres")
	wasRunning := len(psOutput) > 0
	if output, err := composeOutput(compose, composeFile, "up", "-d", "postgres"); err != nil {
		t.Skipf("docker compose could not start the PostgreSQL test database: %v\n%s", err, output)
	}

	t.Cleanup(func() {
		if wasRunning {
			return
		}
		output, err := composeOutput(compose, composeFile, "down")
		assert.NoError(t, err, "docker compose down failed: %s", output)
	})

	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		db, openErr := sql.Open("pgx", migrationTestDBURL)
		if openErr == nil {
			lastErr = db.PingContext(ctx)
			_ = db.Close()
		} else {
			lastErr = openErr
		}
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.NoError(t, lastErr, "PostgreSQL did not become ready")
}

func composeCommand(t *testing.T) []string {
	t.Helper()

	if docker, err := exec.LookPath("docker"); err == nil {
		cmd := exec.Command(docker, "compose", "version")
		if err := cmd.Run(); err == nil {
			return []string{docker, "compose"}
		}
	}
	if dockerCompose, err := exec.LookPath("docker-compose"); err == nil {
		return []string{dockerCompose}
	}
	t.Skip("docker compose is not available")
	return nil
}

func composeOutput(compose []string, composeFile string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	commandArgs := append([]string{}, compose[1:]...)
	commandArgs = append(commandArgs, "-f", composeFile)
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(ctx, compose[0], commandArgs...)
	return cmd.CombinedOutput()
}
