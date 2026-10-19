package config_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
)

func TestInbound371RequirePeerConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    bool
		invalid bool
	}{
		{"", false, false}, {"false", false, false}, {"true", true, false}, {"not-a-boolean", false, true},
	} {
		t.Run("value_"+tc.value, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("CR_FED_REQUIRE_PEER", tc.value)
			cfg, err := config.Load()
			if tc.invalid {
				require.ErrorContains(t, err, "CR_FED_REQUIRE_PEER")
				return
			}
			require.NoError(t, err)
			raw, err := json.Marshal(cfg.Federation)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(raw, &fields))
			require.Equal(t, tc.want, fields["RequirePeer"], "the named inbound posture must be explicit even by default")
		})
	}
}
