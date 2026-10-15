package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServiceDeliveryAllowlistLoad(t *testing.T) {
	for _, raw := range []string{"scheduler>router,router>scheduler", "", "scheduler>router"} {
		t.Run("valid/"+raw, func(t *testing.T) {
			t.Setenv("CR_SERVICE_DELIVERY_ALLOWLIST", raw)
			cfg, err := Load()
			require.NoError(t, err)
			require.Equal(t, raw, cfg.ServiceDeliveryAllowlist)
		})
	}
	for _, raw := range []string{"* >router", "scheduler>*", "scheduler", "scheduler>router>other", ">router", "scheduler>", "scheduler>router,", "scheduler>router,,router>scheduler", "scheduler name>router", "scheduler>/router"} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			t.Setenv("CR_SERVICE_DELIVERY_ALLOWLIST", raw)
			_, err := Load()
			require.Error(t, err)
			if err != nil {
				require.Contains(t, err.Error(), "CR_SERVICE_DELIVERY_ALLOWLIST")
			}
		})
	}
}
