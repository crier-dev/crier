package config_test

// CR-CHAT-006 config acceptance: the chat-session backend is selected by
// CR_SESSION_BACKEND, the SQLite path resolves to a documented default so a
// clean install needs ONE switch, no PostgreSQL URL is required for the SQLite
// path, and a malformed or unsatisfiable selection fails the boot rather than
// being silently ignored.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
)

func TestSessionBackendAutoResolvesWithoutChangingAnything(t *testing.T) {
	unsetAll(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Empty(t, cfg.Session.Backend, "the selection is unset by default")
	require.Equal(t, config.SessionBackendJSONL, cfg.ResolvedSessionBackend(),
		"with no database URL the default is the JSONL log — no PostgreSQL service")
	require.Equal(t, config.DefaultSessionLogRoot, cfg.Session.LogRoot)
	require.Empty(t, cfg.Session.SQLitePath, "no SQLite path is resolved when sqlite is not selected")
}

func TestSessionBackendAutoWithDatabaseURLIsPostgres(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_DATABASE_URL", "postgres://user:pass@localhost:5432/crier?sslmode=disable")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.SessionBackendPostgres, cfg.ResolvedSessionBackend(),
		"a configured database URL keeps the historical postgres default")
}

func TestSessionBackendSQLiteNeedsNoDatabaseURL(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_SESSION_BACKEND", "sqlite")
	cfg, err := config.Load()
	require.NoError(t, err, "the SQLite backend requires no PostgreSQL URL")
	require.Equal(t, config.SessionBackendSQLite, cfg.ResolvedSessionBackend())
	require.Equal(t, config.DefaultSQLitePath, cfg.Session.SQLitePath,
		"one switch is enough: CR_SESSION_BACKEND=sqlite resolves the default database file")
}

func TestSessionBackendSQLitePathOverride(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_SESSION_BACKEND", "sqlite")
	t.Setenv("CR_SQLITE_PATH", "var/lib/crier/sessions.sqlite")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "var/lib/crier/sessions.sqlite", cfg.Session.SQLitePath)
}

func TestSessionBackendJSONLResolvesLogRootDefault(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_SESSION_BACKEND", "jsonl")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.SessionBackendJSONL, cfg.ResolvedSessionBackend())
	require.Equal(t, config.DefaultSessionLogRoot, cfg.Session.LogRoot)

	t.Setenv("CR_SESSION_LOG_ROOT", "var/lib/crier/session-log")
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, "var/lib/crier/session-log", cfg.Session.LogRoot)
}

func TestSessionBackendIsCaseInsensitive(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_SESSION_BACKEND", "  SQLite  ")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.SessionBackendSQLite, cfg.ResolvedSessionBackend())
}

func TestSessionBackendUnknownValueFailsBoot(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_SESSION_BACKEND", "mysql")
	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid CR_SESSION_BACKEND")
	require.Contains(t, err.Error(), "mysql")
}

func TestSessionBackendPostgresWithoutURLFailsBoot(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_SESSION_BACKEND", "postgres")
	_, err := config.Load()
	require.Error(t, err, "an explicit postgres selection with no URL is a boot error, not a fallback")
	require.Contains(t, err.Error(), "CR_DATABASE_URL")
}

func TestSessionBackendPostgresWithURLBoots(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_SESSION_BACKEND", "postgres")
	t.Setenv("CR_DATABASE_URL", "postgres://user:pass@localhost:5432/crier?sslmode=disable")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.SessionBackendPostgres, cfg.ResolvedSessionBackend())
}
