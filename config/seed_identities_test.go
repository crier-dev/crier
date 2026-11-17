package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRegistrySeedIdentitiesLoad pins the env grammar at the CONFIG layer: a
// malformed declaration must fail Load (boot), never be silently dropped, and
// the raw value must survive unchanged for the boot path to parse again.
func TestRegistrySeedIdentitiesLoad(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key := hex.EncodeToString(pub)
	pub2, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	key2 := hex.EncodeToString(pub2)

	for _, raw := range []string{
		"",
		"agent-a=" + key,
		" agent-a=" + key + " ",
		"agent-a=" + key + ",agent-b=" + key2,
		"agent-a=" + key + " , agent-b=" + key2,
	} {
		t.Run("valid/"+raw, func(t *testing.T) {
			t.Setenv("CR_REGISTRY_SEED_IDENTITIES", raw)
			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, raw, cfg.RegistrySeedIdentities)
		})
	}

	// Unset is the default and must stay "seed nothing".
	t.Run("unset", func(t *testing.T) {
		cfg, err := Load()
		require.NoError(t, err)
		require.Empty(t, cfg.RegistrySeedIdentities)
	})

	for _, raw := range []string{
		"agent-a",
		"agent-a=",
		"=" + key,
		"agent a=" + key,
		"agent-a=nothex",
		"agent-a=" + key[:len(key)-2],
		"agent-a=" + key + ",agent-a=" + key2,
		"agent-a=" + key + ",",
	} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			t.Setenv("CR_REGISTRY_SEED_IDENTITIES", raw)
			_, err := Load()
			require.Error(t, err)
			if err != nil {
				require.Contains(t, err.Error(), "CR_REGISTRY_SEED_IDENTITIES")
			}
		})
	}
}

// The declaration does not relax the allowlist: an audience is still required
// for a nonempty allowlist, and an empty allowlist still loads with no seed.
func TestRegistrySeedIdentitiesDoesNotRelaxDeliveryPolicy(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	t.Setenv("CR_SERVICE_DELIVERY_ALLOWLIST", "agent-a>agent-b")
	t.Setenv("CR_REGISTRY_SEED_IDENTITIES", "agent-a="+hex.EncodeToString(pub))
	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, "agent-a>agent-b", cfg.ServiceDeliveryAllowlist)
	// CR_SERVICE_DELIVERY_AUDIENCE unset: the server refuses this combination
	// at composition time regardless of seeded identities.
	require.Empty(t, cfg.ServiceDeliveryAudience)
}
