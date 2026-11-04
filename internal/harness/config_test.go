package harness_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// envMap returns a lookup over a fixed map, with unset keys empty.
func envMap(m map[string]string) func(string) string {
	return func(key string) string { return m[key] }
}

func TestConfigDefaults(t *testing.T) {
	cfg := harness.LoadConfig(envMap(nil))
	if cfg.Enabled {
		t.Fatal("harness delivery must default to OFF (§8)")
	}
	if cfg.Default != "hermes" {
		t.Fatalf("default adapter: want hermes, got %q", cfg.Default)
	}
	if cfg.Timeout != 900*time.Second {
		t.Fatalf("timeout: want 900s, got %s", cfg.Timeout)
	}
	if cfg.MaxConcurrent != 4 {
		t.Fatalf("max_concurrent: want 4, got %d", cfg.MaxConcurrent)
	}
	for _, name := range []string{"hermes", "opencode", "pi", "codex"} {
		if _, ok := cfg.Adapters[name]; !ok {
			t.Fatalf("adapter block %q must be accepted by default", name)
		}
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	cfg := harness.LoadConfig(envMap(map[string]string{
		"CR_HARNESS_ENABLED":             "true",
		"CR_HARNESS_DEFAULT":             "opencode",
		"CR_HARNESS_TIMEOUT":             "45",
		"CR_HARNESS_MAX_CONCURRENT":      "9",
		"CR_HARNESS_HERMES_BINARY":       "/opt/hermes/bin/hermes",
		"CR_HARNESS_OPENCODE_SERVER_URL": "http://127.0.0.1:4096",
	}))
	if !cfg.Enabled {
		t.Fatal("CR_HARNESS_ENABLED=true must enable delivery")
	}
	if cfg.Default != "opencode" {
		t.Fatalf("default: %q", cfg.Default)
	}
	if cfg.Timeout != 45*time.Second || cfg.MaxConcurrent != 9 {
		t.Fatalf("timeout/max: %s/%d", cfg.Timeout, cfg.MaxConcurrent)
	}
	if got := cfg.Adapters["hermes"].Binary; got != "/opt/hermes/bin/hermes" {
		t.Fatalf("hermes binary: %q", got)
	}
	if got := cfg.Adapters["opencode"].ServerURL; got != "http://127.0.0.1:4096" {
		t.Fatalf("opencode server_url: %q", got)
	}
}

// TestConfigMalformedAdapterBlockDisablesOnlyThatAdapter is the §4.1 rule: the
// daemon starts, one adapter is off, the rest keep working.
func TestConfigMalformedAdapterBlockDisablesOnlyThatAdapter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `harness:
  enabled: true
  default: opencode
  adapters:
    hermes:
      binary: hermes
      this_key_does_not_exist: true
    opencode:
      binary: opencode
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := harness.LoadConfig(envMap(map[string]string{"CR_HARNESS_CONFIG": path}))
	if !cfg.Enabled {
		t.Fatal("enabled must be read from the YAML block")
	}
	if _, ok := cfg.Adapters["hermes"]; ok {
		t.Fatal("a malformed hermes block must not be accepted")
	}
	if reason := cfg.Disabled["hermes"]; !strings.Contains(reason, "malformed adapter block") {
		t.Fatalf("disabled reason must say the block is malformed: %q", reason)
	}
	if _, ok := cfg.Adapters["opencode"]; !ok {
		t.Fatal("a malformed hermes block must not disable opencode")
	}
	found := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "harness adapter disabled") && strings.Contains(warning, "hermes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a disabled adapter must log a named reason, warnings: %v", cfg.Warnings)
	}
}

func TestConfigYAMLTopLevelBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.yaml")
	body := `enabled: true
default: opencode
timeout_seconds: 30
max_concurrent: 3
adapters:
  opencode:
    binary: /usr/local/bin/opencode
    server_url: "http://10.0.0.5:4096"
    username: oc
    password_file: /etc/crier/opencode.pass
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := harness.LoadConfig(envMap(map[string]string{"CR_HARNESS_CONFIG": path}))
	if !cfg.Enabled || cfg.Default != "opencode" {
		t.Fatalf("top-level block not applied: %+v", cfg)
	}
	if cfg.Timeout != 30*time.Second || cfg.MaxConcurrent != 3 {
		t.Fatalf("timeout/max: %s/%d", cfg.Timeout, cfg.MaxConcurrent)
	}
	oc := cfg.Adapters["opencode"]
	if oc.Binary != "/usr/local/bin/opencode" || oc.ServerURL != "http://10.0.0.5:4096" ||
		oc.Username != "oc" || oc.PasswordFile != "/etc/crier/opencode.pass" {
		t.Fatalf("opencode block: %+v", oc)
	}
}

func TestConfigBadNumbersAreWarningsNotFailures(t *testing.T) {
	cfg := harness.LoadConfig(envMap(map[string]string{
		"CR_HARNESS_ENABLED":        "not-a-bool",
		"CR_HARNESS_TIMEOUT":        "soon",
		"CR_HARNESS_MAX_CONCURRENT": "-2",
	}))
	if cfg.Enabled {
		t.Fatal("an unparseable boolean must fail closed, not enable delivery")
	}
	if cfg.Timeout != 900*time.Second || cfg.MaxConcurrent != 4 {
		t.Fatalf("bad values must fall back to defaults: %s/%d", cfg.Timeout, cfg.MaxConcurrent)
	}
	if len(cfg.Warnings) < 3 {
		t.Fatalf("every bad value must be reported: %v", cfg.Warnings)
	}
}

func TestConfigUnknownAdapterDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `harness:
  adapters:
    cursor:
      binary: cursor
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg := harness.LoadConfig(envMap(map[string]string{"CR_HARNESS_CONFIG": path}))
	if _, ok := cfg.Adapters["cursor"]; ok {
		t.Fatal("an unknown adapter block must not be accepted")
	}
	if cfg.Disabled["cursor"] != "unknown adapter" {
		t.Fatalf("disabled reason: %q", cfg.Disabled["cursor"])
	}
}
