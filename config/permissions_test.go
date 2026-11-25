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

// ---------------------------------------------------------------------------
// DF-CRIER-297 — the admin token is a SECOND secret, and Load refuses to boot
// with the two roles collapsed onto one value.
// ---------------------------------------------------------------------------

func TestPermissionsAdminTokenEqualsAuthTokenFailsBoot(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_PERMISSIONS_ENABLED", "true")
	t.Setenv("CR_AUTH_TOKEN", "one-secret-for-both")
	t.Setenv("CR_PERMISSIONS_ADMIN_TOKEN", "one-secret-for-both")

	_, err := config.Load()
	require.Error(t, err, "an enabled ACL whose admin token IS the message token must not boot")
	require.Contains(t, err.Error(), "CR_PERMISSIONS_ADMIN_TOKEN")
	require.Contains(t, err.Error(), "CR_AUTH_TOKEN")
	require.Contains(t, err.Error(), "DF-CRIER-297")
}

func TestPermissionsAdminTokenDistinctFromAuthTokenLoads(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_PERMISSIONS_ENABLED", "true")
	t.Setenv("CR_AUTH_TOKEN", "message-secret")
	t.Setenv("CR_PERMISSIONS_ADMIN_TOKEN", "admin-secret")

	cfg, err := config.Load()
	require.NoError(t, err)
	require.True(t, cfg.Permissions.Enabled)
	require.Equal(t, "message-secret", cfg.AuthToken)
	require.Equal(t, "admin-secret", cfg.Permissions.AdminToken)
}

// DF-CRIER-515 flipped the ACL-off case: the admin token now arms the
// registry's operator delete (DELETE /agents/{id} with the admin Bearer) even
// when CR_PERMISSIONS_ENABLED is off, so the old "the admin token arms nothing
// with the ACL disabled" premise no longer holds and the collision is a boot
// refusal in every configuration (DF-CRIER-297's rule, unconditionally).
func TestPermissionsEqualTokensWithAclDisabledFailsBoot(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_AUTH_TOKEN", "one-secret-for-both")
	t.Setenv("CR_PERMISSIONS_ADMIN_TOKEN", "one-secret-for-both") // no CR_PERMISSIONS_ENABLED: still a refusal (DF-CRIER-515)

	_, err := config.Load()
	require.Error(t, err, "the admin token arms the registry's operator delete even with the ACL off, so the collision is not inert")
	require.Contains(t, err.Error(), "CR_PERMISSIONS_ADMIN_TOKEN")
	require.Contains(t, err.Error(), "CR_AUTH_TOKEN")
}

func TestPermissionsAdminTokenWithoutAuthTokenLoads(t *testing.T) {
	unsetAll(t)
	t.Setenv("CR_PERMISSIONS_ENABLED", "true")
	t.Setenv("CR_PERMISSIONS_ADMIN_TOKEN", "admin-secret")
	// CR_AUTH_TOKEN unset: documented development posture (no reach gate).
	// The management writes are still gated by the admin token at the handler.

	cfg, err := config.Load()
	require.NoError(t, err)
	require.Empty(t, cfg.AuthToken)
	require.Equal(t, "admin-secret", cfg.Permissions.AdminToken)
}
