package config_test

import (
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

func TestSessionSQLitePathAlias(t *testing.T) {
	for _, tc := range []struct{ name, alias, legacy, url, want string }{
		{"alias", " /data/sessions.db ", "", "", "/data/sessions.db"},
		{"alias wins over legacy", "sessions.db", "legacy.db", "", "sessions.db"},
		{"alias wins over URL", "sessions.db", "", "sqlite://url.db", "sessions.db"},
		{"legacy", "", "legacy.db", "", "legacy.db"},
		{"blank alias uses legacy", "  ", "legacy.db", "", "legacy.db"},
		{"URL", "", "", "sqlite://url.db", "url.db"},
		{"default", "", "", "", config.DefaultSQLitePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("CR_SESSION_BACKEND", "sqlite")
			t.Setenv("CR_SESSION_SQLITE_PATH", tc.alias)
			t.Setenv("CR_SQLITE_PATH", tc.legacy)
			t.Setenv("CR_DATABASE_URL", tc.url)
			cfg, err := config.Load()
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Session.SQLitePath, "the operator's session path must not silently default")
			if tc.url != "" {
				require.Equal(t, "url.db", cfg.Registry.SQLitePath, "session alias must not redirect registry storage")
			}
		})
	}
}
