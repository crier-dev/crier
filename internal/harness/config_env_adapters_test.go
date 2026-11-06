package harness_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/harness"
)

// envKeyFor renders the CR_HARNESS_* key for an adapter field the way an
// operator has to write it: the adapter id upper-cased with any hyphen turned
// into an underscore, because an environment variable name cannot contain one.
func envKeyFor(adapter, suffix string) string {
	return "CR_HARNESS_" + strings.ReplaceAll(strings.ToUpper(adapter), "-", "_") + "_" + suffix
}

// yamlTag is the config-file name of an AdapterConfig field, which is also the
// tail of its environment override.
func yamlTag(f reflect.StructField) string {
	return strings.Split(f.Tag.Get("yaml"), ",")[0]
}

// TestAdapterEnvOverridesEveryAdapterConfigField holds adapterEnvFields to the
// claim its comment makes: every field a config file can set on an adapter
// block can also be set from the environment. The key set is derived from
// AdapterConfig's own yaml tags rather than a hand-written list, so a field
// added to the struct without an env entry fails here instead of quietly
// becoming YAML-only.
func TestAdapterEnvOverridesEveryAdapterConfigField(t *testing.T) {
	const adapter = "claude-code"
	typ := reflect.TypeOf(harness.AdapterConfig{})

	env := map[string]string{}
	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := yamlTag(f)
		if tag == "" || tag == "-" {
			t.Fatalf("AdapterConfig.%s carries no yaml tag; the env key is derived from it", f.Name)
		}
		if f.Type.Kind() != reflect.String {
			t.Logf("AdapterConfig.%s is %s; an env override would need its own converter, so this table-driven check skips it", f.Name, f.Type)
			continue
		}
		env[envKeyFor(adapter, strings.ToUpper(tag))] = "env-" + tag
		checked++
	}
	if checked == 0 {
		t.Fatal("no string AdapterConfig fields found; the check would be vacuous")
	}

	cfg := harness.LoadConfig(envMap(env))
	block, ok := cfg.Adapters[adapter]
	if !ok {
		t.Fatalf("adapter %q must have a block after loading env overrides (%v)", adapter, cfg.Disabled)
	}
	value := reflect.ValueOf(block)
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.String {
			continue
		}
		tag := yamlTag(f)
		want := "env-" + tag
		if got := value.Field(i).String(); got != want {
			t.Errorf("%s must be settable from %s: got %q, want %q",
				f.Name, envKeyFor(adapter, strings.ToUpper(tag)), got, want)
		}
	}
}

// TestAdapterEnvPermissionModeKeySurvivesAdapterHyphen is the key-spelling
// contract: the adapter id contains a hyphen, the environment key does not, and
// the hyphenated spelling is not consulted at all.
func TestAdapterEnvPermissionModeKeySurvivesAdapterHyphen(t *testing.T) {
	const (
		underscore = "CR_HARNESS_CLAUDE_CODE_PERMISSION_MODE"
		hyphen     = "CR_HARNESS_CLAUDE-CODE_PERMISSION_MODE"
	)
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "underscore spelling sets the mode",
			env:  map[string]string{underscore: "acceptEdits"},
			want: "acceptEdits",
		},
		{
			name: "hyphen spelling is not an env key and leaves the mode untouched",
			env:  map[string]string{hyphen: "bypassPermissions"},
			want: "",
		},
		{
			name: "both spellings set: the underscore key is the one that wins",
			env: map[string]string{
				underscore: "acceptEdits",
				hyphen:     "bypassPermissions",
			},
			want: "acceptEdits",
		},
		{
			name: "unset leaves the operator's posture untouched",
			env:  nil,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := harness.LoadConfig(envMap(tc.env))
			if got := cfg.Adapters["claude-code"].PermissionMode; got != tc.want {
				t.Fatalf("claude-code permission mode: got %q, want %q (env %v)", got, tc.want, tc.env)
			}
		})
	}
}

// TestAdapterEnvExistingKeysUnchanged pins the keys that were already
// documented: the hyphen mapping must not rename any adapter id that has no
// hyphen, so the pre-existing overrides still resolve for their own adapter and
// no other.
func TestAdapterEnvExistingKeysUnchanged(t *testing.T) {
	cfg := harness.LoadConfig(envMap(map[string]string{
		"CR_HARNESS_HERMES_BINARY":       "/opt/hermes/bin/hermes",
		"CR_HARNESS_OPENCODE_SERVER_URL": "http://127.0.0.1:4096",
		"CR_HARNESS_CODEX_SANDBOX":       "read-only",
		"CR_HARNESS_PI_SESSION_DIR":      "/var/tmp/pi-sessions",
	}))
	if got, want := cfg.Adapters["hermes"].Binary, "/opt/hermes/bin/hermes"; got != want {
		t.Errorf("CR_HARNESS_HERMES_BINARY: got %q, want %q", got, want)
	}
	if got, want := cfg.Adapters["opencode"].ServerURL, "http://127.0.0.1:4096"; got != want {
		t.Errorf("CR_HARNESS_OPENCODE_SERVER_URL: got %q, want %q", got, want)
	}
	if got, want := cfg.Adapters["codex"].Sandbox, "read-only"; got != want {
		t.Errorf("CR_HARNESS_CODEX_SANDBOX: got %q, want %q", got, want)
	}
	if got, want := cfg.Adapters["pi"].SessionDir, "/var/tmp/pi-sessions"; got != want {
		t.Errorf("CR_HARNESS_PI_SESSION_DIR: got %q, want %q", got, want)
	}
	// A key for one adapter must not leak into another.
	if got := cfg.Adapters["claude-code"].Binary; got != "claude" {
		t.Errorf("claude-code binary must keep its shipped default: got %q", got)
	}
}
