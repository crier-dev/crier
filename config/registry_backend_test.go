package config_test

import (
	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRegistryBackendResolution(t *testing.T) {
	for _, tc := range []struct{ name, backend, path, url, want, session, err string }{
		{name: "path alone keeps default", path: "registry.db", want: "memory", session: "jsonl"},
		{name: "explicit postgres", backend: "postgres", url: "postgres://localhost/crier", want: "postgres", session: "postgres"},
		{name: "normalized sqlite", backend: " SQLite ", path: "registry.db", want: "sqlite", session: "jsonl"},
		{name: "no env", want: "memory", session: "jsonl"},
		{name: "URL auto", url: "postgres://localhost/crier", want: "postgres", session: "postgres"},
		{name: "explicit memory overrides URL", backend: "memory", url: "postgres://localhost/crier", want: "memory", session: "postgres"},
		{name: "sqlite", backend: "sqlite", path: "registry.db", want: "sqlite", session: "jsonl"},
		{name: "sqlite overrides URL", backend: "sqlite", path: "registry.db", url: "postgres://localhost/crier", want: "sqlite", session: "postgres"},
		{name: "sqlite missing path", backend: "sqlite", err: "CR_REGISTRY_SQLITE_PATH"},
		{name: "postgres missing URL", backend: "postgres", err: "CR_REGISTRY_BACKEND=postgres requires"},
		{name: "unknown", backend: "jsonl", err: "want memory, sqlite or postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("CR_REGISTRY_BACKEND", tc.backend)
			t.Setenv("CR_REGISTRY_SQLITE_PATH", tc.path)
			t.Setenv("CR_DATABASE_URL", tc.url)
			cfg, err := config.Load()
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.ResolvedRegistryBackend())
			require.Equal(t, tc.session, cfg.ResolvedSessionBackend())
			t.Logf("backend=%q path=%q db_url_set=%t -> registry=%s session=%s", tc.backend, tc.path, tc.url != "", cfg.ResolvedRegistryBackend(), cfg.ResolvedSessionBackend())
		})
	}
}
