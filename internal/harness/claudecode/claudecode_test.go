package claudecode_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/claudecode"
)

// fakeCLI stands in for the Claude Code binary. It records its argv NUL
// separated and its stdin, then answers whatever JSON the mode selects, so the
// test proves the ARGUMENT VECTOR the adapter builds — not a re-implementation
// of it.
const fakeCLI = `#!/bin/bash
set -u
dir="${HARNESS_FAKE_DIR:?}"
mode="${HARNESS_FAKE_MODE:-ok}"
printf '%s\0' "$@" > "$dir/argv"
cat > "$dir/stdin" 2>/dev/null || true
case "$mode" in
  ok)       echo '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"cc-minted-1"}' ;;
  noid)     echo '{"type":"result","subtype":"success","is_error":false,"result":"done"}' ;;
  iserror)  echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"router unreachable","session_id":"cc-minted-1"}' ;;
  fail)     echo 'boom' >&2; exit 1 ;;
esac
`

func newFake(t *testing.T) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(fakeCLI), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	t.Setenv("HARNESS_FAKE_DIR", dir)
	return bin, dir
}

func argvOf(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "argv"))
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	parts := strings.Split(string(raw), "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func contains(parts []string, want string) bool {
	for _, p := range parts {
		if p == want {
			return true
		}
	}
	return false
}

// TestCreateMintsAndBindsTheSessionID is the create path: one `-p` turn with a
// JSON answer, and the id the CLI minted lands on the receipt so the next
// delivery can continue THAT session instead of forking a second one.
func TestCreateMintsAndBindsTheSessionID(t *testing.T) {
	bin, dir := newFake(t)
	t.Setenv("HARNESS_FAKE_MODE", "ok")
	a := claudecode.New(claudecode.Config{Binary: bin, Workdir: dir, Timeout: 30 * time.Second})

	rc, err := a.Deliver(context.Background(), harness.SessionRef{}, harness.Message{Text: "hello agent"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if rc.State != harness.StateDispatched {
		t.Fatalf("state = %q, want %q (tail %q)", rc.State, harness.StateDispatched, rc.Tail)
	}
	if !rc.Created {
		t.Fatal("receipt must report Created for an empty session id")
	}
	if rc.SessionID != "cc-minted-1" {
		t.Fatalf("session id = %q, want the id the CLI minted", rc.SessionID)
	}
	argv := argvOf(t, dir)
	for _, want := range []string{"-p", "--output-format", "json", "hello agent"} {
		if !contains(argv, want) {
			t.Fatalf("argv %q missing %q", argv, want)
		}
	}
	if contains(argv, "--resume") {
		t.Fatalf("create path must not carry --resume: %q", argv)
	}
}

// TestContinueResumesTheNamedSession is the continue path: the id travels as
// `--resume <id>` and the receipt never claims to have created anything.
func TestContinueResumesTheNamedSession(t *testing.T) {
	bin, dir := newFake(t)
	t.Setenv("HARNESS_FAKE_MODE", "ok")
	a := claudecode.New(claudecode.Config{Binary: bin, Workdir: dir, Timeout: 30 * time.Second})

	rc, err := a.Deliver(context.Background(),
		harness.SessionRef{ID: "cc-existing"},
		harness.Message{Text: "second turn"})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if rc.Created {
		t.Fatal("continue must not report Created")
	}
	if rc.SessionID != "cc-existing" {
		t.Fatalf("session id = %q, want the resumed id", rc.SessionID)
	}
	argv := argvOf(t, dir)
	i := -1
	for n, p := range argv {
		if p == "--resume" {
			i = n
			break
		}
	}
	if i < 0 || i+1 >= len(argv) || argv[i+1] != "cc-existing" {
		t.Fatalf("argv %q must carry --resume cc-existing", argv)
	}
}

// TestDeliveredButUnboundDowngradesToOutputInvalid: the CLI answered a turn but
// named no session, so the receipt cannot certify a binding (§5).
func TestDeliveredButUnboundDowngradesToOutputInvalid(t *testing.T) {
	bin, dir := newFake(t)
	t.Setenv("HARNESS_FAKE_MODE", "noid")
	a := claudecode.New(claudecode.Config{Binary: bin, Workdir: dir, Timeout: 30 * time.Second})

	rc, _ := a.Deliver(context.Background(), harness.SessionRef{}, harness.Message{Text: "x"})
	if rc.SessionID != "" {
		t.Fatalf("no id was minted, receipt must not carry one: %q", rc.SessionID)
	}
	if rc.Error != harness.CodeOutputInvalid {
		t.Fatalf("error = %q, want %q", rc.Error, harness.CodeOutputInvalid)
	}
}

// TestExitNonZeroIsAFailedReceipt: the harness's own stderr tail is the
// evidence, and the state must never be dispatched.
func TestExitNonZeroIsAFailedReceipt(t *testing.T) {
	bin, dir := newFake(t)
	t.Setenv("HARNESS_FAKE_MODE", "fail")
	a := claudecode.New(claudecode.Config{Binary: bin, Workdir: dir, Timeout: 30 * time.Second})

	rc, _ := a.Deliver(context.Background(), harness.SessionRef{ID: "cc-1"}, harness.Message{Text: "x"})
	if rc.State != harness.StateFailed {
		t.Fatalf("state = %q, want failed", rc.State)
	}
	if rc.Error == "" {
		t.Fatal("a failed receipt must name a code")
	}
	if !strings.Contains(rc.Tail, "boom") {
		t.Fatalf("tail = %q, want the harness's own stderr", rc.Tail)
	}
}

// TestExitZeroWithIsErrorIsAFailedReceipt: exit 0 with is_error must not be
// reported as success — that is the difference between "the process ran" and
// "the agent answered".
func TestExitZeroWithIsErrorIsAFailedReceipt(t *testing.T) {
	bin, dir := newFake(t)
	t.Setenv("HARNESS_FAKE_MODE", "iserror")
	a := claudecode.New(claudecode.Config{Binary: bin, Workdir: dir, Timeout: 30 * time.Second})

	rc, _ := a.Deliver(context.Background(), harness.SessionRef{ID: "cc-1"}, harness.Message{Text: "x"})
	if rc.State != harness.StateFailed {
		t.Fatalf("state = %q, want failed for is_error=true", rc.State)
	}
}

// TestLargeMessageTravelsOnStdin: ARG_MAX must never cap a Crier message (§6).
func TestLargeMessageTravelsOnStdin(t *testing.T) {
	bin, dir := newFake(t)
	t.Setenv("HARNESS_FAKE_MODE", "ok")
	a := claudecode.New(claudecode.Config{Binary: bin, Workdir: dir, Timeout: 30 * time.Second})

	big := strings.Repeat("A", 70<<10)
	if _, err := a.Deliver(context.Background(), harness.SessionRef{ID: "cc-1"}, harness.Message{Text: big}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if len(stdin) != len(big) {
		t.Fatalf("stdin carried %d bytes, want %d", len(stdin), len(big))
	}
	if contains(argvOf(t, dir), big) {
		t.Fatal("a 70 KiB message must not ride in the argument vector")
	}
}

// TestParseResultReadsTheSessionID pins the one field the adapter depends on.
func TestParseResultReadsTheSessionID(t *testing.T) {
	got := claudecode.ParseResult([]byte(`{"session_id":"abc","is_error":false}`))
	if got.SessionID != "abc" {
		t.Fatalf("session id = %q", got.SessionID)
	}
	if claudecode.ParseResult([]byte("not json")).SessionID != "" {
		t.Fatal("unparseable output must yield no id")
	}
}
