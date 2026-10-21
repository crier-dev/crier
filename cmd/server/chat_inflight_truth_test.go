package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The same script supports a pinned pre-change --html negative control. Keep
// this in the ordinary Chat/Progress test gate, not just a manual browser run.
func TestChatInFlightTruth(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the shipped progress client")
	}
	out, err := exec.Command(node, "../../scripts/chat-inflight-truth.cjs").CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("in-flight truth harness: %v", err)
	}
	if !strings.Contains(string(out), "0 failed (stub HTTP; shipped client functions)") {
		t.Fatal("in-flight truth harness did not complete its assertions")
	}
}
