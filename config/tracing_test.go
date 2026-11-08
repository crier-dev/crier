package config_test

import (
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

func TestLoad_TracingDefaultsAndValues(t *testing.T) {
	unsetAll(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.TracingConfig{Exporter: "otlp", OTLPEndpoint: "http://localhost:4318", SampleRatio: 1, ServiceName: "crier"}, cfg.Tracing)

	t.Setenv("CR_TRACING_ENABLED", "yes")
	t.Setenv("CR_TRACING_EXPORTER", "stdout")
	t.Setenv("CR_TRACING_OTLP_ENDPOINT", "http://collector:4318")
	t.Setenv("CR_TRACING_SAMPLE_RATIO", "0.25")
	t.Setenv("CR_TRACING_SERVICE_NAME", "worker")
	cfg, err = config.Load()
	require.NoError(t, err)
	require.Equal(t, config.TracingConfig{Enabled: true, Exporter: "stdout", OTLPEndpoint: "http://collector:4318", SampleRatio: 0.25, ServiceName: "worker"}, cfg.Tracing)
}

func TestLoad_TracingRejectsInvalidValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"CR_TRACING_SAMPLE_RATIO", "0"},
		{"CR_TRACING_SAMPLE_RATIO", "2"},
		{"CR_TRACING_SAMPLE_RATIO", "NaN"},
		{"CR_TRACING_SAMPLE_RATIO", "invalid"},
		{"CR_TRACING_EXPORTER", "grpc"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			unsetAll(t)
			t.Setenv(tc.key, tc.value)
			_, err := config.Load()
			require.ErrorContains(t, err, tc.key)
		})
	}
}
