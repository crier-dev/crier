package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CRIER-UI-1: the Crier UI regression — the composer's destination control
// and the two POST /ingress/messages modes it names, tag chips and their
// client-side grammar, the ?tag= transcript filter, "seen by N/M" from the
// read-cursor route, and the harness badge — driven in real Chromium against a
// scratch router (scripts/chat-courier-ui.cjs).
//
// A missing browser prerequisite is the script's exit 77, which this test
// reports as a SKIP naming the cause: a green here means the assertions ran,
// never that they were skipped.
func TestChatCrierUIBrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the Crier UI browser regression")
	}
	// Unsigned attribution is the posture this page is designed for: the script
	// seeds its transcript through the public API, and the page's own sends are
	// browser-attributed (a signature-required router refuses them by design).
	//
	// The session root is a TEMP dir: this regression creates sessions and (in
	// its last step) a mode=inbox push, which under the JSONL backend writes
	// ingress-routes.jsonl into the session root — a file the JSONL store then
	// lists as a session bundle (the reported CRIER-INGRESS defect). Pointing
	// the test at the default relative "crier-sessions" directory would leave
	// that file in the checkout and poison the NEXT run's GET /sessions.
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  filepath.Join(t.TempDir(), "sessions"),
		"CR_GROUP_ROOT":        filepath.Join(t.TempDir(), "groups"),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, filepath.Join("..", "..", "scripts", "chat-courier-ui.cjs"))
	cmd.Env = append(os.Environ(), "CHAT_BASE="+base, "UI_EVIDENCE="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 77 {
		t.Skipf("browser prerequisite unavailable: %s", out)
	}
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("Crier UI browser regression: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS: CRIER-UI-1") {
		t.Fatal("Crier UI browser regression did not complete its assertions")
	}
}
