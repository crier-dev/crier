package ci_test

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// lookPath is a seam, not indirection for its own sake: the missing-tool path has to be
// exercisable on a developer or CI host that happens to have every tool installed, and the
// only honest way to do that is to stub the lookup instead of depending on the host.
var lookPath = exec.LookPath

// requireTools skips the calling test with a reason that NAMES every external tool the bash
// fixtures need and this host does not have. A host missing bash used to surface as an
// unattributed failure of the whole package (DF-CRIER-497-R2); a named skip keeps the
// failing cause in the output and keeps a bare host from looking like a red product.
func requireTools(t *testing.T, names ...string) {
	t.Helper()
	var missing []string
	for _, name := range names {
		if _, err := lookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Skipf("SKIP: bash fixtures need %s, which is not on PATH on this host", strings.Join(missing, ", "))
	}
}

// TestRequireToolsNamesMissingTool is the regression test for the missing-tool path. It runs
// the preflight in a child test process (so the real skip is observed, not just a recorded
// call) and asserts the skip reason names the tool that was missing.
func TestRequireToolsNamesMissingTool(t *testing.T) {
	t.Run("missing chosen tool", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRequireToolsNamesMissingToolChild$", "-test.v")
		cmd.Env = append(os.Environ(), "CI_REQUIRE_TOOLS_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("child output %q, error %v; want a passing skipped test", out, err)
		}
		if !strings.Contains(string(out), "SKIP: bash fixtures need fixture-missing-tool") {
			t.Fatalf("child output %q does not name the missing tool", out)
		}
	})
}

// TestRequireToolsNamesMissingToolChild is only meaningful inside the child process started by
// TestRequireToolsNamesMissingTool: outside it, it returns immediately so a normal package run
// stays green.
func TestRequireToolsNamesMissingToolChild(t *testing.T) {
	if os.Getenv("CI_REQUIRE_TOOLS_CHILD") == "" {
		return
	}

	const missingTool = "fixture-missing-tool"
	previous := lookPath
	t.Cleanup(func() { lookPath = previous })
	lookPath = func(name string) (string, error) {
		if name == missingTool {
			return "", errors.New("fixture missing tool")
		}
		return exec.LookPath(name)
	}

	requireTools(t, missingTool)
}
