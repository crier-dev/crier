package config_test

import (
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

func TestSessionDeliveryTimeoutConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"", 30000}, {"2", 2}, {"400", 400}, {"10000", 10000}, {"30000", 30000},
		{"0", 0}, {"-1", 0}, {"30001", 0}, {"oops", 0}, {"1.5", 0},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("RUNTIME_TIMEOUT_MS", "1")
			t.Setenv("CR_SESSION_DELIVERY_TIMEOUT_MS", tc.value)
			cfg, err := config.Load()
			if tc.want == 0 {
				require.ErrorContains(t, err, "CR_SESSION_DELIVERY_TIMEOUT_MS")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Session.DeliveryTimeoutMs)
		})
	}
}
