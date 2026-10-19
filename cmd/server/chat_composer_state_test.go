package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Exercise the embedded page against two real routers. The only network
// injection delays an actual send response so the in-flight state is observable.
func TestChatClientComposerStateBrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the composer browser regression")
	}
	start := func(signature string) string {
		return startTestServerWithEnv(t, map[string]string{
			"CR_REQUIRE_AGENT_SIG":     signature,
			"CR_REGISTRY_BACKEND":      "memory",
			"CR_SESSION_BACKEND":       "jsonl",
			"CR_SESSION_LOG_ROOT":      t.TempDir(),
			"CR_GROUP_ROOT":            t.TempDir(),
			"CR_RATE_LIMIT_PER_MINUTE": "0",
		})
	}
	open, locked := start("false"), start("true")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "../../scripts/chat-composer-state.cjs", open, locked)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 77 {
		t.Skipf("browser prerequisite unavailable: %s", out)
	}
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("composer browser regression: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS: composer disabled iff visible reason") {
		t.Fatal("composer browser regression did not complete its assertions")
	}
}
