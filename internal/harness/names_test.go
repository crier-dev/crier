package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
)

// writeHarnessConfig drops a harness YAML in a temp dir and returns the env that
// points LoadConfig at it — the same shape the other config tests use.
func writeHarnessConfig(t *testing.T, body string) func(string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return envMap(map[string]string{"CR_HARNESS_CONFIG": path})
}

// TestAliasResolvesToTheSameAdapter is the whole point of CRIER-HARNESS-9: the
// code-factory installers and node seed publish this harness as `pi-agent`
// while crier's adapter id is `pi`, so a config copied from our own docs was
// refused. One implementation, two accepted spellings.
func TestAliasResolvesToTheSameAdapter(t *testing.T) {
	cfg := harness.LoadConfig(writeHarnessConfig(t, `
enabled: true
default: pi-agent
adapters:
  pi-agent:
    binary: /usr/bin/pi
`))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a pi-agent block must load, got: %v", err)
	}
	if cfg.Default != "pi" {
		t.Fatalf("default = %q, want the canonical id %q", cfg.Default, "pi")
	}
	block, ok := cfg.Adapters["pi"]
	if !ok {
		t.Fatalf("the pi-agent block must configure the pi adapter; adapters=%d", len(cfg.Adapters))
	}
	if block.Binary != "/usr/bin/pi" {
		t.Fatalf("the block's values must survive the alias: %+v", block)
	}
	if _, ok := cfg.Adapters["pi-agent"]; ok {
		t.Fatal("the alias must not survive as a second adapter block")
	}
}

// TestAliasReachesTheRegisteredAdapter is the delivery half: a session bound
// `harness: pi-agent` must reach the adapter registered as `pi`, not fail
// HARNESS_UNKNOWN at the first message.
func TestAliasReachesTheRegisteredAdapter(t *testing.T) {
	reg := harness.NewRegistry(harness.RegistryOptions{Enabled: true, Default: "pi", Timeout: 5 * time.Second, MaxConcurrent: 1})
	reg.Register(&stubAdapter{name: "pi"})
	if _, ok := reg.Get("pi-agent"); !ok {
		t.Fatal("Get(pi-agent) must resolve to the pi adapter")
	}
	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		Harness:    "pi-agent",
		SessionRef: harness.SessionRef{Harness: "pi-agent", ID: "hs-1"},
		Message:    harness.Message{Text: "hello"},
	})
	if rc.State != harness.StateDispatched {
		t.Fatalf("delivery through the alias failed: state=%q error=%q", rc.State, rc.Error)
	}
	if rc.Harness != "pi" {
		t.Fatalf("receipt harness = %q, want the canonical id so metrics and bindings agree", rc.Harness)
	}
}

// TestUnknownAdapterIsRefusedAtLoad: a name nothing serves must fail where the
// operator is still looking — at load — and the message must carry the accepted
// set rather than leaving them to guess.
func TestUnknownAdapterIsRefusedAtLoad(t *testing.T) {
	cfg := harness.LoadConfig(writeHarnessConfig(t, `
enabled: true
adapters:
  cursor:
    binary: cursor
`))
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unknown adapter must be refused at load, not warned about")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cursor") {
		t.Fatalf("the error must name the offending id: %s", msg)
	}
	for _, want := range []string{"accepted:", "hermes", "opencode", "pi", "codex", "claude-code", "pi-agent"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the error must carry the accepted set; missing %q in: %s", want, msg)
		}
	}
}

// TestNearMissNamesTheIntendedAdapter: a name that differs only by punctuation
// or case points at what the operator meant instead of just being refused.
func TestNearMissNamesTheIntendedAdapter(t *testing.T) {
	for _, tc := range []struct{ written, want string }{
		{"pi_agent", "pi-agent"},
		{"Pi-Agent", "pi-agent"},
		{"pi agent", "pi-agent"},
		{"CLAUDE_CODE", "claude-code"},
		{"Open-Code", "opencode"},
	} {
		if got := harness.Suggest(tc.written); got != tc.want {
			t.Errorf("Suggest(%q) = %q, want %q", tc.written, got, tc.want)
		}
	}
	if got := harness.Suggest("cursor"); got != "" {
		t.Errorf("Suggest(cursor) = %q, want no suggestion for an unrelated name", got)
	}

	cfg := harness.LoadConfig(writeHarnessConfig(t, `
adapters:
  pi_agent:
    binary: pi
`))
	err := cfg.Validate()
	if err == nil {
		t.Fatal("pi_agent is not accepted as written and must be refused")
	}
	if !strings.Contains(err.Error(), "did you mean") || !strings.Contains(err.Error(), "pi-agent") {
		t.Fatalf("a near-miss must be answered with the intended name: %s", err)
	}
}

// TestAcceptedNamesAreDerivedFromTheAdapters: the advertised set comes from
// KnownAdapters plus the alias table, so adding an adapter cannot leave the
// error message behind.
func TestAcceptedNamesAreDerivedFromTheAdapters(t *testing.T) {
	help := harness.AcceptedNamesHelp()
	for _, name := range harness.KnownAdapters {
		if !strings.Contains(help, name) {
			t.Fatalf("accepted set is missing the adapter %q: %s", name, help)
		}
	}
	if !strings.Contains(help, "pi-agent (alias of pi)") {
		t.Fatalf("an alias must be shown against its adapter: %s", help)
	}
	names := harness.AcceptedNames()
	if len(names) != len(harness.KnownAdapters)+1 {
		t.Fatalf("accepted names = %v, want the adapters plus one alias", names)
	}
}

// TestDefaultConfigStillValidates: the shipped posture (and every existing
// config) must load unchanged — this row only refuses names nothing serves.
func TestDefaultConfigStillValidates(t *testing.T) {
	if err := harness.DefaultConfig().Validate(); err != nil {
		t.Fatalf("the shipped default config must validate: %v", err)
	}
	if got := harness.LoadConfig(envMap(nil)); len(got.Unknown) != 0 {
		t.Fatalf("a config with no adapter blocks has no unknowns: %v", got.Unknown)
	}
}

// TestFormerlySilentNamesAreRefusedAtLoad is the CRIER-HARNESS-12 fixture: a
// harness id that no adapter serves must fail where the operator is looking.
// Two of that row's four names have since become REAL (pi-agent is now an alias
// of pi, claude-code is an adapter), so the live unknowns are the two that
// remain — deepseek and opencode-v2 — and those are what this pins.
func TestFormerlySilentNamesAreRefusedAtLoad(t *testing.T) {
	for _, name := range []string{"deepseek", "opencode-v2"} {
		cfg := harness.LoadConfig(writeHarnessConfig(t, "adapters:\n  "+name+":\n    binary: whatever\n"))
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("%q has no adapter and must be refused at load", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the refusal must name %q: %s", name, err)
		}
		if !strings.Contains(err.Error(), "accepted:") {
			t.Fatalf("the refusal must print the accepted set: %s", err)
		}
	}
}
