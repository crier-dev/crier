package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The real server positive control pins task.task_id; the browser then exercises
// absent/failed/delayed task reads with explicitly intercepted fixture responses.
func TestChatClientTaskMessageLinkBrowser(t *testing.T) {
	parallelBoot(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the task-link browser regression")
	}
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG":     "false",
		"CR_REGISTRY_BACKEND":      "memory",
		"CR_SESSION_BACKEND":       "jsonl",
		"CR_SESSION_LOG_ROOT":      t.TempDir(),
		"CR_GROUP_ROOT":            t.TempDir(),
		"CR_RATE_LIMIT_PER_MINUTE": "0",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "../../scripts/chat-task-link.cjs", base)
	cmd.Env = append(os.Environ(), "CHAT_EVIDENCE="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 77 {
		t.Skipf("browser prerequisite unavailable: %s", out)
	}
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("task-link browser regression: %v\n%s", err, out)
	}
	for _, marker := range []string{"LIVE PASS: stock Go server", "PASS DF-CRIER-325:"} {
		if !strings.Contains(string(out), marker) {
			t.Fatalf("task-link browser regression missing %q", marker)
		}
	}
}
