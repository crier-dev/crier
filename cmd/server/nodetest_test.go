package main

import (
	"os/exec"
	"testing"
)

// nodeOrSkip returns the node interpreter used to execute the shipped chat
// client, or skips the calling test when Node.js is not installed.
//
// QA-CRIER-54: the clean-machine CI battery runs on freshly provisioned JIT
// agents that may carry no Node.js at all. Before this guard, a failing
// exec.LookPath("node") was reported through require.NoError/t.Fatal and the
// environment gap was graded as a code failure ("exec: node: executable file
// not found in $PATH"), turning the whole ci-pass cell red. A host without
// node cannot exercise the client, so the test reports SKIP (UNVERIFIED)
// instead of a pass: the client behaviour was never proved, and a silent pass
// would claim evidence that does not exist. Everything not depending on node
// keeps running.
func nodeOrSkip(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("Node.js not available - skipping test that executes the shipped chat client (%v)", err)
	}
	return node
}
