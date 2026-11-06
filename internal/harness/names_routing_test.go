package harness_test

// CRIER-HARNESS-12: prove the load-time refusal of an unknown harness, and
// exercise the positive half — the names that were formerly silent and are now
// real (pi-agent as the documented alias of pi, claude-code as an adapter) must
// be routed through as WORKING adapters by a fixture, not merely accepted by the
// name table.
//
// Every fixture here is hermetic: the "harness binary" is a shell script in
// t.TempDir(), so nothing reaches the network and no model is spent. The
// routing fixtures build the registry the server/CLI builds (build.Registry)
// and assert on the adapter the registry actually holds, so "pi-agent works"
// cannot be satisfied by a stub adapter wearing the alias.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/build"
	"github.com/crier-dev/crier/internal/harness/claudecode"
	"github.com/crier-dev/crier/internal/harness/pi"
)

// ---------------------------------------------------------------------------
// The hermetic stub harness
// ---------------------------------------------------------------------------

// stubHarness is a stand-in harness binary. It answers --version (so probe
// paths never try to run a model), records the argv NUL separated and the stdin
// body of a delivery, and then prints whatever the test wrote to its answer
// file. It consults no network and starts no model: the JSON it prints is the
// fixture's input, which is exactly what makes the delivery path testable.
type stubHarness struct {
	bin string
	dir string
}

func newStubHarness(t *testing.T) *stubHarness {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "stub-harness")
	body := "#!/bin/bash\n" +
		"set -u\n" +
		"dir=" + shellQuote(dir) + "\n" +
		"if [ \"${1:-}\" = \"--version\" ]; then\n" +
		"  printf '%s\\n' 'stub 1.2.3'\n" +
		"  exit 0\n" +
		"fi\n" +
		"printf '%s\\0' \"$@\" > \"$dir/argv\"\n" +
		"cat > \"$dir/stdin\" 2>/dev/null || true\n" +
		"if [ -f \"$dir/answer\" ]; then\n" +
		"  cat \"$dir/answer\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub harness: %v", err)
	}
	return &stubHarness{bin: bin, dir: dir}
}

// answer is the JSON the harness prints for a delivery.
func (s *stubHarness) answer(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "answer"), []byte(body+"\n"), 0o600); err != nil {
		t.Fatalf("write stub answer: %v", err)
	}
}

// argv returns the argument vector of the single delivery this stub served.
func (s *stubHarness) argv(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.dir, "argv"))
	if err != nil {
		t.Fatalf("the stub harness recorded no invocation: %v", err)
	}
	parts := strings.Split(string(raw), "\x00")
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}

// shellQuote makes a path safe to embed in the stub script, so a temp dir
// containing shell metacharacters cannot turn the fixture into a different
// program.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// adapterYAML renders one adapter block, so a fixture's config is the config an
// operator would write rather than a hand-built Config struct.
func adapterYAML(extra map[string]string, indent string) string {
	out := ""
	for _, key := range sortedKeys(extra) {
		out += indent + key + ": \"" + extra[key] + "\"\n"
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Acceptance criterion 1: an unknown harness fails at load, named, with the
// derived accepted set. This is the CRIER-HARNESS-9 behaviour kept as a
// regression assertion.
// ---------------------------------------------------------------------------

// TestRefusedHarnessNamesFailOneLoad: a config naming harnesses no adapter
// serves must fail where the operator is still looking — at load — and the
// failure must name every offending id and print the accepted set. Unknown is
// what makes the CLI exit non-zero (`crier harness ...` exits 2 on this error),
// so a regression that demoted this to a warning would show up here.
func TestRefusedHarnessNamesFailOneLoad(t *testing.T) {
	cfg := harness.LoadConfig(writeHarnessConfig(t, `
enabled: true
adapters:
  deepseek:
    binary: deepseek
  opencode-v2:
    binary: opencode
`))
	err := cfg.Validate()
	if err == nil {
		t.Fatal("deepseek and opencode-v2 have no adapter and must be refused at load")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown harness adapters:") {
		t.Fatalf("the refusal must name the class and both ids: %s", msg)
	}
	for _, name := range []string{"deepseek", "opencode-v2"} {
		if !strings.Contains(msg, `"`+name+`"`) {
			t.Fatalf("the refusal must name %q: %s", name, msg)
		}
		if !slices.Contains(cfg.Unknown, name) {
			t.Fatalf("%q must be recorded in Unknown so the load is a hard failure: %v", name, cfg.Unknown)
		}
		if cfg.Disabled[name] != "unknown adapter" {
			t.Fatalf("%q disabled reason = %q, want %q", name, cfg.Disabled[name], "unknown adapter")
		}
		if _, ok := cfg.Adapters[name]; ok {
			t.Fatalf("%q must not survive as an adapter block", name)
		}
	}
	// The accepted set is printed from KnownAdapters + the alias table, so every
	// served name must appear — including the ones this row turned from silent
	// names into real adapters.
	for _, name := range harness.AcceptedNames() {
		if !strings.Contains(msg, name) {
			t.Fatalf("the refusal must print the accepted set; missing %q in: %s", name, msg)
		}
	}
	if strings.Contains(msg, "warn") || strings.Contains(msg, "warned") {
		t.Fatalf("the refusal is a failure, not a warning: %s", msg)
	}
}

// TestEveryKnownAdapterBlockStillLoads is acceptance criterion 4: a
// configuration that names only served adapters loads unchanged — no refusals,
// no disabled blocks — and the registry the server builds registers all five.
func TestEveryKnownAdapterBlockStillLoads(t *testing.T) {
	cfg := harness.LoadConfig(writeHarnessConfig(t, `
enabled: true
default: opencode
adapters:
  hermes:
    binary: hermes
  opencode:
    binary: opencode
  pi:
    binary: pi
  codex:
    binary: codex
  claude-code:
    binary: claude
`))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config naming only served adapters must load: %v", err)
	}
	if len(cfg.Unknown) != 0 {
		t.Fatalf("no adapter was unknown, got %v", cfg.Unknown)
	}
	if len(cfg.Disabled) != 0 {
		t.Fatalf("nothing was malformed, so nothing may be disabled: %v", cfg.Disabled)
	}
	if !cfg.Enabled || cfg.Default != harness.AdapterOpencode {
		t.Fatalf("enabled/default must be read from the YAML: %t/%q", cfg.Enabled, cfg.Default)
	}
	for _, name := range harness.KnownAdapters {
		block, ok := cfg.Adapters[name]
		if !ok {
			t.Fatalf("adapter block %q must be accepted", name)
		}
		if strings.TrimSpace(block.Binary) == "" {
			t.Fatalf("adapter block %q lost its binary: %+v", name, block)
		}
	}
	registered := build.Registry(cfg).Names()
	sort.Strings(registered)
	known := slices.Clone(harness.KnownAdapters)
	sort.Strings(known)
	if !slices.Equal(registered, known) {
		t.Fatalf("the builders register %v but the known set is %v", registered, known)
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 3: the accepted set is derived from the constructors,
// not a second hand-written list.
// ---------------------------------------------------------------------------

// TestAcceptedNamesAreServedByTheConstructors closes the loop between the name
// table and the adapters the server actually builds: every advertised name must
// resolve to a registered adapter, and every registered adapter must be
// advertised. A new adapter therefore cannot ship without appearing in the
// accepted set, and a name cannot be advertised that nothing serves.
func TestAcceptedNamesAreServedByTheConstructors(t *testing.T) {
	names := harness.AcceptedNames()
	if len(names) == 0 {
		t.Fatal("the accepted set is empty: an operator could not name any adapter")
	}
	reg := build.Registry(harness.DefaultConfig())
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Fatalf("the accepted set repeats %q", name)
		}
		seen[name] = true
		adapter, ok := reg.Get(name)
		if !ok {
			t.Fatalf("accepted name %q has no adapter: the list has drifted from the constructors (registered: %v)", name, reg.Names())
		}
		if !slices.Contains(harness.KnownAdapters, adapter.Name()) {
			t.Fatalf("accepted name %q resolves to %q, which is not a known adapter", name, adapter.Name())
		}
	}
	registered := reg.Names()
	sort.Strings(registered)
	known := slices.Clone(harness.KnownAdapters)
	sort.Strings(known)
	if !slices.Equal(registered, known) {
		t.Fatalf("the constructors register %v but KnownAdapters says %v", registered, known)
	}
}

// TestFormerlySilentNamesAreRefusedOrAccepted is the CRIER-HARNESS-12 split
// fixture. The row's four formerly-silent names each belong to exactly ONE of
// the two cases, and the case asserts the behaviour it claims:
//
//	refused  — deepseek, opencode-v2   (nothing serves these names)
//	accepted — pi-agent, claude-code   (served, and exercised end to end below)
func TestFormerlySilentNamesAreRefusedOrAccepted(t *testing.T) {
	refused := []string{"deepseek", "opencode-v2"}
	accepted := []string{"pi-agent", "claude-code"}

	count := make(map[string]int, len(refused)+len(accepted))
	for _, name := range slices.Concat(refused, accepted) {
		count[name]++
	}
	if len(count) != 4 {
		t.Fatalf("the fixtures cover %d names, want the row's four: %v", len(count), count)
	}
	for name, n := range count {
		if n != 1 {
			t.Fatalf("%q appears in %d fixtures; each name must sit in exactly one (refused or accepted)", name, n)
		}
	}

	for _, name := range refused {
		if canonical, ok := harness.CanonicalName(name); ok {
			t.Fatalf("%q is in the refused fixture but resolves to %q", name, canonical)
		}
		cfg := harness.LoadConfig(writeHarnessConfig(t, "adapters:\n  "+name+":\n    binary: whatever\n"))
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("%q is in the refused fixture but loaded", name)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("the refusal must name %q: %s", name, err)
		}
	}

	for _, name := range accepted {
		canonical, ok := harness.CanonicalName(name)
		if !ok {
			t.Fatalf("%q is in the accepted fixture but nothing serves it", name)
		}
		cfg := harness.LoadConfig(writeHarnessConfig(t, "adapters:\n  "+name+":\n    binary: whatever\n"))
		if err := cfg.Validate(); err != nil {
			t.Fatalf("%q is in the accepted fixture but load refused it: %v", name, err)
		}
		if _, ok := cfg.Adapters[canonical]; !ok {
			t.Fatalf("%q loaded without configuring its adapter %q", name, canonical)
		}
	}
}

// ---------------------------------------------------------------------------
// Acceptance criterion 2: pi-agent and claude-code are WORKING adapters — a
// message routes through each one, built from a config file, with no live
// model.
// ---------------------------------------------------------------------------

// TestPiAgentAliasIsAWorkingAdapter: the documented alias `pi-agent` configures
// the pi adapter (not a second one) and a message delivered through that name
// reaches the real pi adapter, runs the stub binary, and comes back dispatched
// with the id the harness minted. TestPiAgentAliasIsAWorkingAdapter is the test
// this row names for pi-agent.
func TestPiAgentAliasIsAWorkingAdapter(t *testing.T) {
	stub := newStubHarness(t)
	// pi reports its session id in its JSON answer; the adapter must read it
	// back, which is only possible if the real pi adapter ran.
	stub.answer(t, `{"sessionID":"stub-pi-minted"}`)
	workdir := t.TempDir()

	cfg := harness.LoadConfig(writeHarnessConfig(t,
		"enabled: true\n"+
			"default: pi-agent\n"+
			"adapters:\n"+
			"  pi-agent:\n"+
			adapterYAML(map[string]string{"binary": stub.bin, "workdir": workdir}, "    ")))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("pi-agent is the documented alias of pi and must load: %v", err)
	}
	if len(cfg.Unknown) != 0 {
		t.Fatalf("pi-agent must not be an unknown adapter: %v", cfg.Unknown)
	}
	if cfg.Default != harness.AdapterPi {
		t.Fatalf("default = %q, want the canonical id %q", cfg.Default, harness.AdapterPi)
	}
	block, ok := cfg.Adapters[harness.AdapterPi]
	if !ok {
		t.Fatalf("the pi-agent block must configure pi; adapters=%v", cfg.Adapters)
	}
	if block.Binary != stub.bin || block.Workdir != workdir {
		t.Fatalf("the alias block's values must survive: %+v", block)
	}
	if _, second := cfg.Adapters["pi-agent"]; second {
		t.Fatal("the alias must not survive as a second adapter block")
	}

	reg := build.Registry(cfg)
	adapter, ok := reg.Get("pi-agent")
	if !ok {
		t.Fatal("pi-agent must resolve in the registry built from this config")
	}
	if _, isPi := adapter.(*pi.Adapter); !isPi {
		t.Fatalf("pi-agent must reach the real pi adapter, got %T", adapter)
	}

	const text = "hello from the pi-agent alias"
	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		Harness:    "pi-agent",
		SessionRef: harness.SessionRef{Harness: "pi-agent"},
		Message:    harness.Message{ID: "m_pi_alias", SessionID: "s_pi_alias", Author: "ci", Text: text},
	})
	if rc.State != harness.StateDispatched {
		t.Fatalf("delivery through pi-agent failed: state=%q error=%q tail=%q", rc.State, rc.Error, rc.Tail)
	}
	if rc.Harness != harness.AdapterPi {
		t.Fatalf("receipt harness = %q, want the canonical id %q", rc.Harness, harness.AdapterPi)
	}
	if rc.SessionID != "stub-pi-minted" {
		t.Fatalf("receipt session id = %q, want the id the harness minted (%q)", rc.SessionID, "stub-pi-minted")
	}
	if !rc.Created {
		t.Fatalf("a delivery with no session id is pi's create path: %+v", rc)
	}

	argv := stub.argv(t)
	for _, want := range []string{"-p", "--mode", "json", "--session-id", "crier-s_pi_alias", "--", text} {
		if !containsElement(argv, want) {
			t.Fatalf("pi argv %q missing %q", argv, want)
		}
	}
	if containsElement(argv, "--model") {
		t.Fatalf("no model is configured, so none may be forced: %v", argv)
	}
}

// TestClaudeCodeIsAWorkingAdapter: claude-code is an adapter in its own right
// (it was one of the formerly silent names) and a message delivered through it
// reaches the real adapter, runs the stub binary with the configured permission
// mode, and comes back dispatched and bound to the id the CLI minted.
// TestClaudeCodeIsAWorkingAdapter is the test this row names for claude-code.
func TestClaudeCodeIsAWorkingAdapter(t *testing.T) {
	stub := newStubHarness(t)
	stub.answer(t, `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"stub-cc-minted"}`)
	workdir := t.TempDir()

	cfg := harness.LoadConfig(writeHarnessConfig(t,
		"enabled: true\n"+
			"default: claude-code\n"+
			"adapters:\n"+
			"  claude-code:\n"+
			adapterYAML(map[string]string{
				"binary":          stub.bin,
				"workdir":         workdir,
				"permission_mode": "acceptEdits",
			}, "    ")))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("claude-code is a served adapter and must load: %v", err)
	}
	if len(cfg.Unknown) != 0 {
		t.Fatalf("claude-code must not be an unknown adapter: %v", cfg.Unknown)
	}
	block, ok := cfg.Adapters[harness.AdapterClaudeCode]
	if !ok {
		t.Fatalf("the claude-code block must be accepted; adapters=%v", cfg.Adapters)
	}
	if block.Binary != stub.bin || block.PermissionMode != "acceptEdits" {
		t.Fatalf("the claude-code block's values must survive: %+v", block)
	}

	reg := build.Registry(cfg)
	adapter, ok := reg.Get("claude-code")
	if !ok {
		t.Fatal("claude-code must resolve in the registry built from this config")
	}
	if _, isClaude := adapter.(*claudecode.Adapter); !isClaude {
		t.Fatalf("claude-code must reach the real claude-code adapter, got %T", adapter)
	}

	const text = "hello from the claude-code fixture"
	rc := reg.Deliver(context.Background(), harness.DispatchRequest{
		Harness:    "claude-code",
		SessionRef: harness.SessionRef{Harness: "claude-code"},
		Message:    harness.Message{ID: "m_cc", SessionID: "s_cc", Author: "ci", Text: text},
	})
	if rc.State != harness.StateDispatched {
		t.Fatalf("delivery through claude-code failed: state=%q error=%q tail=%q", rc.State, rc.Error, rc.Tail)
	}
	if rc.Harness != harness.AdapterClaudeCode {
		t.Fatalf("receipt harness = %q, want %q", rc.Harness, harness.AdapterClaudeCode)
	}
	if !rc.Created {
		t.Fatalf("a delivery with no session id is the create path: %+v", rc)
	}
	if rc.SessionID != "stub-cc-minted" {
		t.Fatalf("receipt session id = %q, want the id the CLI minted (%q)", rc.SessionID, "stub-cc-minted")
	}
	if rc.Error != "" {
		t.Fatalf("a successful turn must carry no error code: %+v", rc)
	}

	argv := stub.argv(t)
	for _, want := range []string{"-p", "--output-format", "json", "--permission-mode", "acceptEdits", text} {
		if !containsElement(argv, want) {
			t.Fatalf("claude-code argv %q missing %q", argv, want)
		}
	}
	if containsElement(argv, "--resume") {
		t.Fatalf("the create path must not carry --resume: %v", argv)
	}
}
