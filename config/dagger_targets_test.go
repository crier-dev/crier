package config_test

import (
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDaggerTargetEnvParsing (CR-CHAT-035): named remote execution targets are
// read from CR_DAGGER_TARGET_<NAME>=<url> variables, lowercased, with
// non-target variables, empty names and empty-name entries ignored.
func TestDaggerTargetEnvParsing(t *testing.T) {
	t.Setenv("CR_DAGGER_URL", "http://127.0.0.1:9001")
	t.Setenv("CR_DAGGER_TARGET_BUNKER_1", "http://bunker.local:8765")
	t.Setenv("CR_DAGGER_TARGET_DEDI-2", "http://dedi2.local:8765") // lowercased
	t.Setenv("CR_DAGGER_TARGET_", "http://ignored.example")        // empty NAME
	t.Setenv("CR_OTHER_VAR", "http://not-a-target.example")        // not a target

	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Dagger.Targets)
	assert.Equal(t, "http://bunker.local:8765", cfg.Dagger.Targets["bunker_1"])
	assert.Equal(t, "http://dedi2.local:8765", cfg.Dagger.Targets["dedi-2"])
	assert.NotContains(t, cfg.Dagger.Targets, "")
	assert.NotContains(t, cfg.Dagger.Targets, "other_var")
}

// TestDaggerTargetsWithoutURLIsolated (CR-CHAT-035): target entries may be
// present while the dagger surface is off (CR_DAGGER_URL unset) — the map is
// still populated (wiring refuses to start the surface without a local bridge
// anyway) and parsing must not crash on it.
func TestDaggerTargetsWithoutURLIsolated(t *testing.T) {
	t.Setenv("CR_DAGGER_TARGET_LONELY", "http://alone.example:8765")

	cfg, err := config.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Dagger.Targets)
	assert.Equal(t, "http://alone.example:8765", cfg.Dagger.Targets["lonely"])
}
