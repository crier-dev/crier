package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// stderrTailBytes is the spec's §3 receipt tail: the last 2 KiB of stderr.
const stderrTailBytes = 2048

// maxCaptureBytes bounds what one delivery keeps in memory for each stream.
// A harness that floods stdout must not grow the relay's heap without bound;
// the byte COUNTS stay exact, only the retained prefix is capped.
const maxCaptureBytes = 1 << 20

// execSpec is one child process. Args is an argument vector, never a command
// line: no shell is involved anywhere in this package.
type execSpec struct {
	Path string
	Args []string
	Dir  string
	Env  []string
	// Stdin is sent on the child's stdin when non-empty. It is the escape
	// hatch for messages over ARG_MAX (§6).
	Stdin string
}

// execResult is everything a caller needs to build a receipt.
type execResult struct {
	ExitCode   int
	Stdout     []byte
	Stderr     []byte
	Tail       string
	StdoutSize int
	TimedOut   bool
	// StartError is set when the process could not be started at all (missing
	// binary, permission denied). ExitCode is -1 in that case.
	StartError string
	Duration   time.Duration
}

// runCommand runs one child under a per-delivery deadline and kills the whole
// process GROUP on expiry — a harness that spawns helpers must not leave them
// behind (§5 HARNESS_TIMEOUT).
func runCommand(ctx context.Context, spec execSpec, timeout time.Duration) execResult {
	start := time.Now()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if spec.Env != nil {
		cmd.Env = spec.Env
	}
	out := newCappedBuffer(maxCaptureBytes)
	errBuf := newCappedBuffer(maxCaptureBytes)
	cmd.Stdout = out
	cmd.Stderr = errBuf
	if spec.Stdin != "" {
		cmd.Stdin = bytes.NewReader([]byte(spec.Stdin))
	}
	// Cancel runs on ctx expiry. Killing the process group (not just the pid)
	// is the difference between "the harness stopped" and "the harness and the
	// three agents it started kept running".
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }
	// WaitDelay bounds the wait for inherited pipes to close after the kill,
	// so a grandchild holding stdout open cannot stall the delivery forever.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	res := execResult{
		ExitCode:   exitCode(cmd),
		Stdout:     out.Bytes(),
		Stderr:     errBuf.Bytes(),
		StdoutSize: out.Total(),
		Duration:   time.Since(start),
	}
	res.Tail = tailString(res.Stderr, stderrTailBytes)
	if err != nil {
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) && !res.TimedOut {
			res.StartError = err.Error()
			res.ExitCode = -1
		}
	}
	return res
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

// tailString returns at most n trailing bytes of b, trimmed of surrounding
// whitespace so a receipt reads cleanly in JSON.
func tailString(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(bytes.TrimSpace(b))
}

// cappedBuffer keeps the first limit bytes and counts everything written.
type cappedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
	total int
}

func newCappedBuffer(limit int) *cappedBuffer { return &cappedBuffer{limit: limit} }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total += len(p)
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) <= room {
			b.buf.Write(p)
		} else {
			b.buf.Write(p[:room])
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, b.buf.Len())
	copy(out, b.buf.Bytes())
	return out
}

func (b *cappedBuffer) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// lookupBinary resolves a configured binary to an executable path. A name with
// no separator goes through PATH; a path is checked as-is. Probe must not run
// anything first: an absent binary is reported by name, not by an exec error.
func lookupBinary(path string) (string, error) {
	if path == "" {
		return "", errors.New("binary path is empty")
	}
	if !hasPathSeparator(path) {
		found, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		return found, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errors.New(path + " is a directory")
	}
	return path, nil
}

func hasPathSeparator(p string) bool {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' || p[i] == os.PathSeparator {
			return true
		}
	}
	return false
}

// workdirError validates a delivery working directory. It is never created: a
// wrong cwd silently changes what the agent does (§6).
func workdirError(dir string) error {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New(dir + " is not a directory")
	}
	return nil
}
