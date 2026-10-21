package main

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// DF-CRIER-460: the same selectable-HTML harness supplies reproducible RED
// evidence and exercises the embedded client's refresh, click and render paths.
func TestChatLifecycleTruth(t *testing.T) {
	node, err := exec.LookPath("node")
	require.NoError(t, err, "node is required to exercise the shipped client")
	out, err := exec.Command(node, filepath.Join("..", "..", "scripts", "chat-lifecycle-truth.cjs")).CombinedOutput()
	require.NoError(t, err, "%s", out)
	t.Log(string(out))
}
