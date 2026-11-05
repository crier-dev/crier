package claudecode_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/claudecode"
)

// TestLiveTurnAgainstTheInstalledBinary is the measurement-ladder leg for
// CRIER-HARNESS-11. It is skipped unless CRIER_TEST_LIVE_CLAUDE=1, and it
// reports the receipt verbatim so a blocked host shows WHY, not just that it
// failed.
func TestLiveTurnAgainstTheInstalledBinary(t *testing.T) {
	if os.Getenv("CRIER_TEST_LIVE_CLAUDE") != "1" {
		t.Skip("set CRIER_TEST_LIVE_CLAUDE=1 to run a real turn")
	}
	bin := os.Getenv("CLAUDE_BIN")
	if bin == "" {
		bin = "/home/kara/.local/harness/npm/bin/claude"
	}
	a := claudecode.New(claudecode.Config{Binary: bin, Workdir: os.TempDir(), Timeout: 90 * time.Second})
	rc, err := a.Deliver(context.Background(), harness.SessionRef{},
		harness.Message{Text: "Reply with the single word CRIER-CLAUDE-OK"})
	fmt.Printf("RECEIPT: %+v err=%v\n", rc, err)
	if rc.State != harness.StateDispatched {
		t.Fatalf("live turn did not dispatch: state=%s error=%s tail=%s", rc.State, rc.Error, rc.Tail)
	}
	if rc.SessionID == "" {
		t.Fatal("a dispatched create must carry the session id the CLI minted")
	}
}
