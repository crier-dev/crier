package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChatLateJoinTruth(t *testing.T) {
	node, err := exec.LookPath("node")
	require.NoError(t, err, "node is required to exercise the shipped late-join client")
	out, err := exec.Command(node, filepath.Join("..", "..", "scripts", "chat-late-join-truth.cjs")).CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "RESULT 12/12 passed")
	t.Log(string(out))
}

func TestChatLateJoinBrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the late-join browser regression")
	}
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG":     "false",
		"CR_REGISTRY_BACKEND":      "memory",
		"CR_SESSION_BACKEND":       "jsonl",
		"CR_SESSION_LOG_ROOT":      t.TempDir(),
		"CR_GROUP_ROOT":            t.TempDir(),
		"CR_RATE_LIMIT_PER_MINUTE": "0",
	})
	evidence := os.Getenv("CHAT_LATE_JOIN_EVIDENCE")
	if evidence == "" {
		evidence = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "../../scripts/chat-late-join-browser.cjs", base, evidence).CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 77 {
		t.Skipf("browser prerequisite unavailable: %s", out)
	}
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "LIVE PASS: stock Go API + real Chromium")
	t.Log(string(out))
}
