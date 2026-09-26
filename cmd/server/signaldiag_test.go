package main

// CI-018 — attributable exits for the in-process test server.
//
// THE FAILURE THIS FILE EXISTS FOR. TestCRFEAT029NamespaceDocumentCanComeFromAFile
// (and its siblings) failed intermittently, in 0.03s, reporting that the server
// exited before answering the /health readiness probe on port 46429 (last
// error: connect: connection refused) — with a captured server log holding ONLY
// the normal startup lines: no fatal, no panic. Two things were missing from
// that report and both are fixed here:
//
//  1. the harness discarded run()'s RETURN VALUE, so a failure never said
//     whether the boot died on a bind failure (exit 1), a rejected config
//     (exit 1) or a signal-triggered graceful shutdown (exit 0) — the exit
//     CODE is the missing attribution, and every exit site now carries it
//     through runExitCode (see TestEveryHarnessExitSiteNamesTheRunExitCode).
//     The fingerprintable wording every site and every board fingerprint keys
//     on ("server exited before answering " + "/health", spelled as two pieces
//     here so this file is not a false hit for a repo-wide grep of it) is
//     preserved verbatim;
//  2. the graceful-shutdown path was the ONLY silent early exit of run(): the
//     wait goroutine closed the listener and run() returned 0 with no line of
//     its own, so a SIGTERM delivered while a server was still booting produced
//     exactly the observed signature. logShutdownSignal now names the signal,
//     the port and the build on that path (see
//     TestShutdownSignalLogNamesTheSignal).
//
// The tests below are deliberately split: the log shape and the message shape
// are asserted directly, and the exit-code plumbing is driven through a REAL
// deterministic boot failure (an occupied port) instead of waiting for the
// flake to reappear.

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/buildinfo"
)

// exitCodeUnknown is reported by runExitCode when the boot goroutine has not
// recorded a code yet. It is a can't-happen on the harness paths — those sites
// only read the code after observing the CLOSED done channel, which the boot
// goroutine closes only after the send — and exists so a failure report can
// never hang the suite waiting for a value that will not arrive.
const exitCodeUnknown = -1

// runExitCode reads the exit code a harness's run() goroutine recorded, without
// blocking. The send happens BEFORE the deferred close(done) the waiter
// observes, so a site that has just seen done closed always finds the code
// buffered here.
func runExitCode(code <-chan int) int {
	select {
	case c := <-code:
		return c
	default:
		return exitCodeUnknown
	}
}

// TestRunExitCodeReadsTheBufferedCodeAndNeverBlocks pins the plumbing contract
// every harness exit site depends on: the code is recorded before done closes,
// and a read with nothing buffered returns the sentinel instead of blocking.
func TestRunExitCodeReadsTheBufferedCodeAndNeverBlocks(t *testing.T) {
	// Empty channel: never blocks.
	if got := runExitCode(make(chan int, 1)); got != exitCodeUnknown {
		t.Errorf("runExitCode on an empty channel = %d, want %d", got, exitCodeUnknown)
	}

	// The ordering contract: a waiter that observed done closed always finds
	// the code already buffered (reordering the two statements inside the boot
	// goroutine breaks exactly this).
	for _, want := range []int{0, 1} {
		done := make(chan struct{})
		exitCode := make(chan int, 1)
		go func() {
			defer close(done)
			exitCode <- want
		}()
		<-done
		if got := runExitCode(exitCode); got != want {
			t.Errorf("runExitCode after done closed = %d, want %d", got, want)
		}
	}
}

// TestShutdownSignalLogNamesTheSignal is the CI-018 gate for the log line that
// makes the graceful exit attributable: it must name the triggering SIGNAL (the
// one piece of information the captured logs never held), the port and the
// build. SIGTERM and SIGINT are the two signals run() registers for.
func TestShutdownSignalLogNamesTheSignal(t *testing.T) {
	const port = 42715

	for _, tc := range []struct {
		name string
		sig  os.Signal
		want string
	}{
		{"SIGTERM", syscall.SIGTERM, "signal=terminated"},
		{"SIGINT", syscall.SIGINT, "signal=interrupt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			logShutdownSignal(tc.sig, port)

			out := buf.String()
			for _, want := range []string{
				"shutdown signal received",
				tc.want,
				"port=42715",
				"version=" + buildinfo.String(),
			} {
				if !strings.Contains(out, want) {
					t.Errorf("shutdown signal log does not contain %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestServerBootExitCodeIsCapturedOnFailedBind drives the exit-code capture
// through a REAL boot failure instead of waiting for the flake: the port is
// held by a listener for the whole test, so run()'s bind fails deterministically
// (the DF-CRIER-154 path) and it returns 1. The assertion is on the value the
// harness sites interpolate — an exit code of 1 is what turns the /health
// boot-failure message into a diagnosable report.
func TestServerBootExitCodeIsCapturedOnFailedBind(t *testing.T) {
	// Deterministic environment: no DB, no auth token, no guard.
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CR_GUARD_ENABLED", "false")

	// The listener stays open for the whole test: closing it would race run()'s
	// bind attempt and could flake green. run() binds ":<port>", so a holder on
	// 127.0.0.1:<port> is enough to make the bind fail (the same shape
	// captureFailedBind uses).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	t.Setenv("CRIER_PORT", strconv.Itoa(port))

	done := make(chan struct{})
	// exitCode carries run()'s return value (CI-018). The send happens BEFORE
	// the deferred close(done) a waiter observes, so whenever this server has
	// exited, its code is already buffered here.
	exitCode := make(chan int, 1)
	go func() {
		defer close(done)
		exitCode <- run(nil)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("run() against an occupied port did not return within 20s")
	}

	if got := runExitCode(exitCode); got != 1 {
		t.Fatalf("run() against an occupied port = %d, want 1 (the code the harness failure text reports)", got)
	}
}

// TestSignalShutdownReturnsZeroAndLogsTheSignal exercises the whole CI-018
// plumbing on the path that produced the silent exit: a server booted by the
// normal harness shape is shut down by a process SIGTERM, run() must still
// return 0 (the pre-existing contract, unchanged), and the shutdown line must
// reach the process logger so the next occurrence of the flake is attributable
// from the captured log alone.
func TestSignalShutdownReturnsZeroAndLogsTheSignal(t *testing.T) {
	// Deterministic environment: no DB, no auth token, no guard.
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CR_GUARD_ENABLED", "false")

	port := freePort(t)
	t.Setenv("CRIER_PORT", strconv.Itoa(port))

	done := make(chan struct{})
	// exitCode carries run()'s return value (CI-018). The send happens BEFORE
	// the deferred close(done) a waiter observes, so whenever this server has
	// exited, its code is already buffered here.
	exitCode := make(chan int, 1)
	go func() {
		defer close(done)
		exitCode <- run(nil)
	}()

	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find own process: %v", err)
	}
	t.Cleanup(func() {
		_ = self.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("server did not shut down within 10s of SIGTERM")
		}
	})

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start within 20s: %v", err)
		}
		select {
		case <-done:
			t.Fatalf("server exited before answering /health on port %d: run() exit code %d (last error: %v)", port, runExitCode(exitCode), err)
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Install the log capture AFTER the boot: run() calls initLogger (which
	// repoints slog's default), so a buffer installed earlier would be replaced
	// before the shutdown line is written. slog resolves the default handler at
	// call time, which is why a late swap still captures it.
	logs := captureServerLogs(t)

	if err := self.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not shut down within 10s of SIGTERM")
	}

	if got := runExitCode(exitCode); got != 0 {
		t.Errorf("run() after SIGTERM = %d, want 0 (graceful shutdown is not a failure)", got)
	}
	out := logs.String()
	for _, want := range []string{"shutdown signal received", "signal=terminated", "port=" + strconv.Itoa(port)} {
		if !strings.Contains(out, want) {
			t.Errorf("shutdown log does not contain %q:\n%s", want, out)
		}
	}
}

// TestEveryHarnessExitSiteNamesTheRunExitCode is the automated form of CI-018's
// first acceptance criterion. The harness sites are scanned as TEXT because the
// property is about the message a human reads on a red CI run: every place that
// can print the /health boot-failure message must both read the captured exit
// code and render it. Each site's own format string is then rendered with a
// code of 1, so the check is on the SHIPPED text rather than on a copy of it —
// a site that lost the value would fail here instead of on the next flake.
func TestEveryHarnessExitSiteNamesTheRunExitCode(t *testing.T) {
	// The prefix is assembled from two pieces ON PURPOSE: a repo-wide grep for
	// the whole string must keep finding exactly the harness exit sites, and
	// this file (which mentions the failure text in prose) must not add an
	// unattributed-looking hit of its own.
	prefix := "server exited before answering " + "/health"

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob harness sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no *_test.go in this directory — the scan would be vacuous")
	}

	sites := 0
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			// A SITE is a line that PRINTS the failure — prose that mentions it
			// is not one, so the classifier is the call, not the phrase.
			if !strings.Contains(line, prefix) || !strings.Contains(line, "t.Fatalf(") {
				continue
			}
			sites++
			where := fmt.Sprintf("%s:%d", name, i+1)

			if !strings.Contains(line, "run() exit code %d") {
				t.Errorf("%s: exit site does not name run()'s exit code: %s", where, strings.TrimSpace(line))
				continue
			}
			if !strings.Contains(line, "runExitCode(exitCode)") {
				t.Errorf("%s: exit site does not read the captured run() exit code: %s", where, strings.TrimSpace(line))
				continue
			}

			format, ok := firstQuotedLiteral(line)
			if !ok {
				t.Errorf("%s: exit site has no format literal to render: %s", where, strings.TrimSpace(line))
				continue
			}
			msg := fmt.Sprintf(format, 42715, 1, errors.New("dial tcp 127.0.0.1:42715: connect: connection refused"))
			if !strings.Contains(msg, prefix) {
				t.Errorf("%s: rendered failure text lost the fingerprintable prefix %q: %s", where, prefix, msg)
			}
			if !strings.Contains(msg, "exit code 1") {
				t.Errorf("%s: rendered failure text does not name the exit code: %s", where, msg)
			}
		}
	}

	// A census, not a count to be maintained by hand: the package boots a server
	// in 9 places today (main_test.go x4, and crfeat030/docsclaims/observability/
	// status/signaldiag x1 each). Adding a site is fine — this scan covers it
	// automatically — but LOSING one means a boot no longer reports its exit
	// code, and that is what this floor catches.
	const want = 9
	if sites < want {
		t.Fatalf("found %d exit site(s), want at least %d: a boot stopped reporting run()'s exit code, or moved out of this scan", sites, want)
	}
}

// firstQuotedLiteral returns the contents of the first double-quoted Go string
// literal on a source line. The exit sites pass their format string as the
// first argument of t.Fatalf, so this is what the failing test would print.
func firstQuotedLiteral(line string) (string, bool) {
	start := strings.Index(line, "\"")
	if start < 0 {
		return "", false
	}
	end := strings.Index(line[start+1:], "\"")
	if end < 0 {
		return "", false
	}
	return line[start+1 : start+1+end], true
}
