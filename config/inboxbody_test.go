package config_test

import (
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_InboxMaxBodyBytes(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		unsetAll(t)

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, 1<<20, cfg.InboxMaxBodyBytes)
	})

	t.Run("configured", func(t *testing.T) {
		unsetAll(t)
		t.Setenv("CR_INBOX_MAX_BODY_BYTES", "4096")

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, 4096, cfg.InboxMaxBodyBytes)
	})

	for _, value := range []string{"0", "-1", "abc", "4.5"} {
		t.Run("invalid "+value, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("CR_INBOX_MAX_BODY_BYTES", value)

			cfg, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "CR_INBOX_MAX_BODY_BYTES")
			assert.Equal(t, 1<<20, cfg.InboxMaxBodyBytes)
		})
	}
}
