package config_test

// CR-CHAT-003 config acceptance: the delivery ACL is OPT-IN, its store
// directory resolves to a documented default when enabled, and a malformed
// switch fails the boot rather than being silently ignored.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
)

func TestPermissionsDisabledByDefault(t *testing.T) {
	unsetAll(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.False(t, cfg.Permissions.Enabled, "the delivery ACL is opt-in")
	require.Empty(t, cfg.Permissions.StoreDir, "no store is resolved when the ACL is off")
}

func TestPermissionsEnabledResolvesDefaultStoreDir(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_PERMISSIONS_ENABLED", "true")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.True(t, cfg.Permissions.Enabled)
	require.Equal(t, "/var/lib/crier/permissions", cfg.Permissions.StoreDir)
}

func TestPermissionsStoreDirOverride(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_PERMISSIONS_ENABLED", "1")
	t.Setenv("CR_PERMISSIONS_DIR", "/tmp/crier-acl")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.True(t, cfg.Permissions.Enabled)
	require.Equal(t, "/tmp/crier-acl", cfg.Permissions.StoreDir)
}

func TestPermissionsInvalidSwitchFailsBoot(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_PERMISSIONS_ENABLED", "maybe")
	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "CR_PERMISSIONS_ENABLED")
}
