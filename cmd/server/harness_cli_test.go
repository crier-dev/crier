package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cliFakeHarness = `#!/bin/bash
if [ "${1:-}" = "--version" ]; then
  printf '%s\n' "fakeharness 4.5.6"
  exit 0
fi
exit 0
`

// writeCLIFakeHarness writes an executable fake harness and returns its path.
func writeCLIFakeHarness(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-harness")
	if err := os.WriteFile(path, []byte(cliFakeHarness), 0o755); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	return path
}

func harnessEnv(fakePath string) func(string) string {
	env := map[string]string{
		"CR_HARNESS_HERMES_BINARY":   fakePath,
		"CR_HARNESS_OPENCODE_BINARY": "/nonexistent/opencode-binary",
		"CR_HARNESS_PI_BINARY":       fakePath,
		"CR_HARNESS_CODEX_BINARY":    fakePath,
	}
	return func(key string) string { return env[key] }
}

func TestHarnessListReportsEveryAdapter(t *testing.T) {
	fake := writeCLIFakeHarness(t)
	var out, errOut bytes.Buffer
	if code := runHarness([]string{"list"}, &out, &errOut, harnessEnv(fake)); code != 0 {
		t.Fatalf("list exit: want 0, got %d (%s)", code, errOut.String())
	}
	got := out.String()
	// All four adapters are real in this build: every one of them is probed and
	// reports the fake binary's version, and none reports a later row.
	for _, want := range []string{"NAME", "hermes", "opencode", "pi", "codex", "true", "false", "4.5.6"} {
		if !strings.Contains(got, want) {
			t.Fatalf("list output missing %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"CRIER-HARNESS-2", "CRIER-HARNESS-3"} {
		if strings.Contains(got, gone) {
			t.Fatalf("pi and codex are implemented; %q must not appear:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "binary not found: /nonexistent/opencode-binary") {
		t.Fatalf("an unavailable adapter must carry a reason naming the binary:\n%s", got)
	}
}

func TestHarnessProbeExitsOneWhenUnavailable(t *testing.T) {
	fake := writeCLIFakeHarness(t)
	var out, errOut bytes.Buffer

	if code := runHarness([]string{"probe", "hermes"}, &out, &errOut, harnessEnv(fake)); code != 0 {
		t.Fatalf("available probe exit: want 0, got %d (%s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"available":true`) {
		t.Fatalf("probe must print the capability JSON:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runHarness([]string{"probe", "opencode"}, &out, &errOut, harnessEnv(fake)); code != 1 {
		t.Fatalf("unavailable probe exit: want 1, got %d", code)
	}
	if !strings.Contains(out.String(), `"available":false`) {
		t.Fatalf("probe must print the unavailable capability:\n%s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runHarness([]string{"probe", "nope"}, &out, &errOut, harnessEnv(fake)); code != 1 {
		t.Fatalf("unknown adapter exit: want 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "not a registered adapter") {
		t.Fatalf("unknown adapter must explain itself: %s", errOut.String())
	}
}

func TestHarnessUsageAndUnknownSubcommand(t *testing.T) {
	fake := writeCLIFakeHarness(t)
	var out, errOut bytes.Buffer
	if code := runHarness(nil, &out, &errOut, harnessEnv(fake)); code != 2 {
		t.Fatalf("no subcommand: want 2, got %d", code)
	}
	if !strings.Contains(errOut.String(), "crier harness list") {
		t.Fatalf("usage must name the subcommands: %s", errOut.String())
	}
	errOut.Reset()
	if code := runHarness([]string{"deliver"}, &out, &errOut, harnessEnv(fake)); code != 2 {
		t.Fatalf("unknown subcommand: want 2, got %d", code)
	}
}
