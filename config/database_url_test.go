package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

func TestDatabaseURLSQLiteRouting(t *testing.T) {
	for _, backend := range []string{"registry", "session", "auto"} {
		t.Run(backend, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("CR_REGISTRY_BACKEND", "memory")
			t.Setenv("CR_REGISTRY_SQLITE_PATH", "")
			t.Setenv("CR_SESSION_BACKEND", "jsonl")
			path := filepath.Join(t.TempDir(), "url.sqlite")
			t.Setenv("CR_DATABASE_URL", "sqlite://"+path)
			switch backend {
			case "registry":
				t.Setenv("CR_REGISTRY_BACKEND", "sqlite")
			case "session":
				t.Setenv("CR_SESSION_BACKEND", "sqlite")
			case "auto":
				t.Setenv("CR_REGISTRY_BACKEND", "")
				t.Setenv("CR_SESSION_BACKEND", "")
			}
			cfg, err := config.Load()
			require.NoError(t, err)
			if backend != "session" {
				require.Equal(t, "sqlite", cfg.ResolvedRegistryBackend())
				require.Equal(t, path, cfg.Registry.SQLitePath)
				s, err := registry.NewSQLiteStore(cfg.Registry.SQLitePath)
				require.NoError(t, err)
				require.NoError(t, s.Close())
			}
			if backend != "registry" {
				require.Equal(t, "sqlite", cfg.ResolvedSessionBackend())
				require.Equal(t, path, cfg.Session.SQLitePath)
				s, err := session.OpenStore(context.Background(), cfg.ResolvedSessionBackend(), session.StoreOptions{SQLitePath: cfg.Session.SQLitePath})
				require.NoError(t, err)
				require.NoError(t, s.Close())
			}
			_, err = os.Stat(path)
			require.NoError(t, err, "the real store must open the URL-derived file")
		})
	}
}

func TestDatabaseURLSQLitePrecedence(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_REGISTRY_BACKEND", "sqlite")
	t.Setenv("CR_SESSION_BACKEND", "sqlite")
	t.Setenv("CR_DATABASE_URL", "sqlite://relative.db")
	t.Setenv("DATABASE_URL", "mysql://ignored")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, "relative.db", cfg.Registry.SQLitePath)
	require.Equal(t, "relative.db", cfg.Session.SQLitePath)
	t.Setenv("CR_REGISTRY_SQLITE_PATH", "explicit-registry.db")
	t.Setenv("CR_SQLITE_PATH", "explicit-session.db")
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, "explicit-registry.db", cfg.Registry.SQLitePath)
	require.Equal(t, "explicit-session.db", cfg.Session.SQLitePath)
}

func TestDatabaseURLBackendMismatch(t *testing.T) {
	for _, env := range []string{"CR_REGISTRY_BACKEND", "CR_SESSION_BACKEND"} {
		for _, tc := range []struct{ url, backend, scheme, wanted string }{
			{"sqlite:///tmp/x.db", "postgres", "sqlite", "postgres or postgresql"},
			{"postgresql://localhost/crier", "sqlite", "postgresql", "sqlite"},
		} {
			t.Run(env+"/"+tc.backend, func(t *testing.T) {
				unsetAll(t)
				t.Setenv("CR_REGISTRY_BACKEND", "memory")
				t.Setenv("CR_SESSION_BACKEND", "jsonl")
				t.Setenv("CR_REGISTRY_SQLITE_PATH", "override.db")
				t.Setenv("CR_SQLITE_PATH", "override.db")
				t.Setenv(env, tc.backend)
				t.Setenv("CR_DATABASE_URL", tc.url)
				_, err := config.Load()
				require.Error(t, err)
				require.Contains(t, err.Error(), "DATABASE_BACKEND_MISMATCH")
				require.Contains(t, err.Error(), "scheme \""+tc.scheme+"\"")
				require.Contains(t, err.Error(), env+"="+tc.backend)
				require.Contains(t, err.Error(), "requires "+tc.wanted)
				require.Contains(t, err.Error(), "set CR_DATABASE_URL")
				require.Contains(t, err.Error(), "CR_REGISTRY_SQLITE_PATH and CR_SQLITE_PATH override URL-derived paths, not the URL scheme")
				t.Log(err)
			})
		}
	}
}

func TestDatabaseURLUnknownSchemeNamesSource(t *testing.T) {
	for _, env := range []string{"CR_DATABASE_URL", "DATABASE_URL", "CRIER_DATABASE_URL"} {
		t.Run(env, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("CR_REGISTRY_BACKEND", "memory")
			t.Setenv(env, "mysql://user:private@host/db")
			_, err := config.Load()
			require.EqualError(t, err, "DATABASE_URL_SCHEME: "+env+" has scheme \"mysql\" (accepted: postgres, postgresql, sqlite); set "+env+" to an accepted database URL")
			require.NotContains(t, err.Error(), "private")
			t.Log(err)
		})
	}
}
