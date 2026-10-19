package config_test

import (
	"path/filepath"
	"testing"

	"github.com/crier-dev/crier/config"
)

func TestFederation363RecordFileResolution(t *testing.T) {
	for _, tc := range []struct{ name, key, queue, explicit, want string }{
		{"legacy_off", "", "", "", ""},
		{"legacy_signing_only", "/keys/self.pem", "/state/holds.json", "", ""},
		{"hold_adjacent", "/keys/self.pem", "/state/holds.json", "auto", "/state/federation-peers.json"},
		{"key_adjacent", "/keys/self.pem", "", "auto", "/keys/federation-peers.json"},
		{"explicit", "/keys/self.pem", "/state/holds.json", "/custom/records.json", "/custom/records.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetAll(t)
			t.Setenv("CR_FED_SELF_KEY_FILE", tc.key)
			t.Setenv("CR_FED_QUEUE_FILE", tc.queue)
			t.Setenv("CR_FED_RECORDS_FILE", tc.explicit)
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want != "" {
				want = filepath.Clean(want)
			}
			if cfg.Federation.RecordsFile != want {
				t.Fatalf("path %q want %q", cfg.Federation.RecordsFile, want)
			}
		})
	}
}
