package main

import (
	"os"
	"testing"
)

func TestTracingConfigurationRefusesStartup(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"CR_TRACING_SAMPLE_RATIO", "2"},
		{"CR_TRACING_EXPORTER", "grpc"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			getenv := func(key string) string {
				if key == tc.key {
					return tc.value
				}
				return ""
			}
			if code := runWithEnv(nil, make(chan os.Signal), getenv); code != 1 {
				t.Fatalf("runWithEnv exit code = %d, want startup refusal 1", code)
			}
		})
	}
}
