package harness

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The harness block of the relay config (COURIER-HARNESS §4.1). It is read
// from the YAML file named by CR_HARNESS_CONFIG when set, and every value can
// be overridden by an environment variable.
//
// Nothing here can stop the daemon: a malformed adapter block disables THAT
// adapter and records the reason, and a malformed top-level value falls back
// to its documented default. Harness delivery is best-effort by design — a
// configuration mistake must not turn into an outage.
const (
	// EnvConfigFile names the optional YAML document holding the block.
	EnvConfigFile = "CR_HARNESS_CONFIG"

	// Adapter names. The four config blocks are accepted from day one; pi and
	// codex report unavailable until their rows land.
	AdapterHermes   = "hermes"
	AdapterOpencode = "opencode"
	AdapterPi       = "pi"
	AdapterCodex    = "codex"
)

// KnownAdapters is the closed set of adapter names a config block may use.
var KnownAdapters = []string{AdapterHermes, AdapterOpencode, AdapterPi, AdapterCodex}

// AdapterConfig is one adapters.<name> block. The union is deliberate: the
// fields a harness does not use stay empty, and an unknown key inside a block
// is a malformed block (the adapter is disabled, not silently half-applied).
type AdapterConfig struct {
	Binary string `yaml:"binary"`
	// Workdir is the delivery cwd; it must exist at delivery time.
	Workdir string `yaml:"workdir"`
	// Model is passed where the verified surface accepts one; empty means the
	// harness default.
	Model string `yaml:"model"`

	// Hermes gateway (the only verified create path for Hermes).
	GatewayURL   string `yaml:"gateway_url"`
	GatewayToken string `yaml:"gateway_token"`

	// opencode --attach server mode.
	ServerURL    string `yaml:"server_url"`
	Username     string `yaml:"username"`
	PasswordFile string `yaml:"password_file"`

	// pi --session-dir (accepted for CRIER-HARNESS-2).
	SessionDir string `yaml:"session_dir"`

	// codex sandbox mode (accepted for CRIER-HARNESS-3).
	Sandbox string `yaml:"sandbox"`
}

// Config is the harness block.
type Config struct {
	Enabled       bool
	Default       string
	Timeout       time.Duration
	MaxConcurrent int
	// Adapters holds the accepted blocks, by adapter name.
	Adapters map[string]AdapterConfig
	// Disabled maps an adapter name to the reason its block was refused. The
	// adapter is not registered; every other adapter is unaffected.
	Disabled map[string]string
	// Warnings records non-fatal configuration problems worth one log line
	// each at startup.
	Warnings []string
}

// DefaultConfig is the shipped posture: delivery OFF, so every existing code
// path is unchanged until an operator turns it on (§8).
func DefaultConfig() Config {
	return Config{
		Enabled:       false,
		Default:       AdapterHermes,
		Timeout:       DefaultTimeout,
		MaxConcurrent: DefaultMaxConcurrent,
		Adapters: map[string]AdapterConfig{
			AdapterHermes:   {Binary: AdapterHermes},
			AdapterOpencode: {Binary: AdapterOpencode, Username: "opencode"},
			AdapterPi:       {Binary: AdapterPi},
			AdapterCodex:    {Binary: AdapterCodex, Sandbox: "workspace-write"},
		},
		Disabled: map[string]string{},
	}
}

// LoadConfig reads the harness block: shipped defaults, then the optional YAML
// file, then CR_* environment overrides. It never returns an error.
func LoadConfig(getenv func(string) string) Config {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := DefaultConfig()

	if path := strings.TrimSpace(getenv(EnvConfigFile)); path != "" {
		if err := applyYAMLFile(&cfg, path); err != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("harness config %s: %v", path, err))
		}
	}
	applyEnv(&cfg, getenv)
	applyAdapterEnv(&cfg, getenv)
	validate(&cfg)
	return cfg
}

// yamlRoot is the on-disk shape. Pointers distinguish "absent" from "zero", so
// an explicit false/0 is honoured.
type yamlRoot struct {
	Enabled        *bool                `yaml:"enabled"`
	Default        *string              `yaml:"default"`
	TimeoutSeconds *int                 `yaml:"timeout_seconds"`
	MaxConcurrent  *int                 `yaml:"max_concurrent"`
	Adapters       map[string]yaml.Node `yaml:"adapters"`
}

func applyYAMLFile(cfg *Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// The file may be the whole relay document (harness: {...}) or just the
	// harness block. Accept both: a "harness" key wins, else the top level.
	var wrapper struct {
		Harness yaml.Node `yaml:"harness"`
	}
	if err := yaml.Unmarshal(raw, &wrapper); err != nil {
		return err
	}
	root := raw
	if wrapper.Harness.Kind != 0 {
		encoded, err := yaml.Marshal(&wrapper.Harness)
		if err != nil {
			return err
		}
		root = encoded
	}

	var parsed yamlRoot
	dec := yaml.NewDecoder(bytes.NewReader(root))
	dec.KnownFields(true)
	if err := dec.Decode(&parsed); err != nil {
		return err
	}
	if parsed.Enabled != nil {
		cfg.Enabled = *parsed.Enabled
	}
	if parsed.Default != nil {
		cfg.Default = strings.TrimSpace(*parsed.Default)
	}
	if parsed.TimeoutSeconds != nil {
		if *parsed.TimeoutSeconds <= 0 {
			cfg.Warnings = append(cfg.Warnings, "harness.timeout_seconds must be positive; keeping the default")
		} else {
			cfg.Timeout = time.Duration(*parsed.TimeoutSeconds) * time.Second
		}
	}
	if parsed.MaxConcurrent != nil {
		if *parsed.MaxConcurrent <= 0 {
			cfg.Warnings = append(cfg.Warnings, "harness.max_concurrent must be positive; keeping the default")
		} else {
			cfg.MaxConcurrent = *parsed.MaxConcurrent
		}
	}
	for name, node := range parsed.Adapters {
		ac, err := decodeAdapterBlock(node)
		if err != nil {
			cfg.Disabled[name] = "malformed adapter block: " + err.Error()
			continue
		}
		if !isKnownAdapter(name) {
			cfg.Disabled[name] = "unknown adapter"
			continue
		}
		cfg.Adapters[name] = ac
		delete(cfg.Disabled, name)
	}
	return nil
}

// decodeAdapterBlock applies KnownFields per block, so a typo in one adapter
// cannot leak into another — and a bad block only disables itself.
func decodeAdapterBlock(node yaml.Node) (AdapterConfig, error) {
	encoded, err := yaml.Marshal(&node)
	if err != nil {
		return AdapterConfig{}, err
	}
	var ac AdapterConfig
	dec := yaml.NewDecoder(bytes.NewReader(encoded))
	dec.KnownFields(true)
	if err := dec.Decode(&ac); err != nil {
		return AdapterConfig{}, err
	}
	return ac, nil
}

func applyEnv(cfg *Config, getenv func(string) string) {
	if v := strings.TrimSpace(getenv("CR_HARNESS_ENABLED")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("CR_HARNESS_ENABLED=%q is not a boolean; harness delivery stays off", v))
			cfg.Enabled = false
		} else {
			cfg.Enabled = b
		}
	}
	if v := strings.TrimSpace(getenv("CR_HARNESS_DEFAULT")); v != "" {
		cfg.Default = v
	}
	if v := strings.TrimSpace(getenv("CR_HARNESS_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n <= 0 {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("CR_HARNESS_TIMEOUT=%q is not a positive integer; keeping the default", v))
		} else {
			cfg.Timeout = time.Duration(n) * time.Second
		}
	}
	if v := strings.TrimSpace(getenv("CR_HARNESS_MAX_CONCURRENT")); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n <= 0 {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("CR_HARNESS_MAX_CONCURRENT=%q is not a positive integer; keeping the default", v))
		} else {
			cfg.MaxConcurrent = n
		}
	}
}

// adapterEnvFields names the env override for every configurable adapter field.
var adapterEnvFields = []struct {
	suffix string
	set    func(*AdapterConfig, string)
}{
	{"BINARY", func(a *AdapterConfig, v string) { a.Binary = v }},
	{"WORKDIR", func(a *AdapterConfig, v string) { a.Workdir = v }},
	{"MODEL", func(a *AdapterConfig, v string) { a.Model = v }},
	{"GATEWAY_URL", func(a *AdapterConfig, v string) { a.GatewayURL = v }},
	{"GATEWAY_TOKEN", func(a *AdapterConfig, v string) { a.GatewayToken = v }},
	{"SERVER_URL", func(a *AdapterConfig, v string) { a.ServerURL = v }},
	{"USERNAME", func(a *AdapterConfig, v string) { a.Username = v }},
	{"PASSWORD_FILE", func(a *AdapterConfig, v string) { a.PasswordFile = v }},
	{"SESSION_DIR", func(a *AdapterConfig, v string) { a.SessionDir = v }},
	{"SANDBOX", func(a *AdapterConfig, v string) { a.Sandbox = v }},
}

func applyAdapterEnv(cfg *Config, getenv func(string) string) {
	for _, name := range KnownAdapters {
		ac := cfg.Adapters[name]
		for _, f := range adapterEnvFields {
			key := "CR_HARNESS_" + strings.ToUpper(name) + "_" + f.suffix
			if v := strings.TrimSpace(getenv(key)); v != "" {
				f.set(&ac, v)
			}
		}
		cfg.Adapters[name] = ac
	}
}

// validate records configuration problems without removing an adapter whose
// block was structurally fine. Values the adapter cannot act on are warnings;
// a name outside the known set is a disabled block.
func validate(cfg *Config) {
	if cfg.Default != "" && !isKnownAdapter(cfg.Default) {
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("harness.default %q is not a known adapter; requests naming no harness will fail HARNESS_UNKNOWN", cfg.Default))
	}
	for name := range cfg.Adapters {
		if !isKnownAdapter(name) {
			cfg.Disabled[name] = "unknown adapter"
			delete(cfg.Adapters, name)
		}
	}
	for name, reason := range cfg.Disabled {
		delete(cfg.Adapters, name)
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("harness adapter disabled: %s: %s", name, reason))
	}
}

func isKnownAdapter(name string) bool {
	for _, known := range KnownAdapters {
		if name == known {
			return true
		}
	}
	return false
}
