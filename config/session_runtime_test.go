package config_test

import (
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

func TestSessionRuntimeBudgetInvariant(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, session string
		wantErr                bool
	}{
		{"defaults", "", "", false},
		{"max_runtime", "25000", "30000", false},
		{"explicit_short", "100", "400", false},
		{"runtime_exceeds_session", "20000", "10000", true},
		{"equal_no_overhead", "20000", "20000", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("RUNTIME_TIMEOUT_MS", tc.runtime)
			t.Setenv("CR_SESSION_DELIVERY_TIMEOUT_MS", tc.session)
			_, err := config.Load()
			if tc.wantErr {
				require.ErrorContains(t, err, "RUNTIME_TIMEOUT_MS="+tc.runtime)
				require.ErrorContains(t, err, "CR_SESSION_DELIVERY_TIMEOUT_MS="+tc.session)
				t.Logf("startup rejected: %v", err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
