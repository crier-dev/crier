package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/buildinfo"
	"github.com/crier-dev/crier/internal/pidfile"
	"gopkg.in/yaml.v3"
)

// TestServerHealth is an entrypoint smoke test: it runs run(nil) on a random
// free port, hits /health, verifies a 200 response, then triggers graceful
// shutdown via SIGTERM. If someone breaks the wiring in main.go (router,
// middleware, listener), this test fails immediately. run(nil) is used
// instead of main() so the test binary's os.Args (e.g. -test.timeout) never
// reaches flag parsing, and so the server never calls os.Exit.
func TestServerHealth(t *testing.T) {
	// Skip on Go 1.25 — go test catches the process-level SIGTERM before
	// the signal goroutine in main(), producing "signal: terminated" (CI-012).
	if strings.HasPrefix(runtime.Version(), "go1.25") {
		t.Skip("skipping on Go 1.25: SIGTERM handling in go test differs from 1.26")
	}

	// Bypass auth and force the in-memory store regardless of the dev env.
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")

	port := freePort(t)
	t.Setenv("CRIER_PORT", fmt.Sprintf("%d", port))

	done := make(chan struct{})
	go func() {
		defer close(done)
		run(nil)
	}()

	// Graceful shutdown: main() installs a SIGINT/SIGTERM handler that calls
	// srv.Shutdown. Send SIGTERM to our own process and wait for main to return.
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

	// Wait for the server to come up (bounded).
	var resp *http.Response
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err = client.Get(baseURL + "/health")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start within 10s: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health: got status %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /health body: %v", err)
	}
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("GET /health: body %q does not contain %q", body, "ok")
	}
}

// freePort finds a random free TCP port by listening on :0 and releasing.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestBindFailureDiagnostic is the DF-CRIER-154 regression gate. On this
// shared host the documented default run path is routinely blocked by
// leftover servers holding the port, and the pre-fix failure path printed
// only the raw errno — no port holder to look for, no way to run elsewhere,
// no build identity on the failing line. The test occupies a port
// in-process, starts the server on it via run() (the testable entrypoint),
// and asserts the failure log carries the actionable diagnostic. The
// listener is kept open for the whole test: closing it would race run()'s
// bind attempt and could flake green.
func TestBindFailureDiagnostic(t *testing.T) {
	// Deterministic environment: no DB, no auth token, no inherited port.
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CRIER_PORT", "1") // overridden by -port below; never a real target

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// Capture slog output: run() re-installs the default logger itself
	// (initLogger writes to os.Stderr), so redirect os.Stderr for the call
	// via a pipe and restore afterwards.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	savedStderr := os.Stderr
	os.Stderr = w

	done := make(chan int)
	go func() {
		done <- run([]string{"-port", strconv.Itoa(port)})
	}()

	// Drain the pipe in the background so a full pipe buffer cannot block
	// run()'s logging.
	var buf bytes.Buffer
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(&buf, r)
	}()

	code := <-done
	_ = w.Close()
	os.Stderr = savedStderr
	<-drained
	_ = r.Close()

	out := buf.String()

	// (a) non-zero exit code (expected exactly 1).
	if code == 0 {
		t.Fatalf("run(-port %d) on an occupied port = 0, want non-zero\nstderr:\n%s", port, out)
	}
	if code != 1 {
		t.Errorf("run(-port %d) = %d, want 1", port, code)
	}

	// (b) the port number appears on the failure path.
	if !strings.Contains(out, strconv.Itoa(port)) {
		t.Errorf("failure output does not name port %d:\n%s", port, out)
	}

	// (c) the actionable guidance: holder-check command and -port alternative.
	if !strings.Contains(out, "ss -tlnp | grep :"+strconv.Itoa(port)) {
		t.Errorf("failure output does not carry the holder-check command (ss -tlnp | grep :%d):\n%s", port, out)
	}
	if !strings.Contains(out, "-port <n>") || !strings.Contains(out, "CRIER_PORT") {
		t.Errorf("failure output does not carry the run-elsewhere guidance (-port <n> / CRIER_PORT):\n%s", out)
	}
	if !strings.Contains(out, "another process already holds this port") {
		t.Errorf("failure output does not state that another process already holds this port:\n%s", out)
	}

	// (d) the build identity is repeated on the failure path (String()
	// already carries the leading "v", so the log shows version=v…).
	if !strings.Contains(out, "version="+buildinfo.String()) {
		t.Errorf("failure output does not repeat the build identity (version=%s):\n%s", buildinfo.String(), out)
	}

	// Constraint: the raw errno text must keep matching — log greppers and
	// the existing "bind: address already in use" expectations stay intact.
	if !strings.Contains(out, "bind: address already in use") {
		t.Errorf("failure output dropped the raw error text (bind: address already in use):\n%s", out)
	}
}

// TestParseArgs exercises the CLI flag parsing directly (no exec, no server
// startup). The --help and --version paths are the CR-GAP-005 hard gate.
func TestParseArgs(t *testing.T) {
	envVars := []string{
		"CRIER_PORT",
		"CR_PIDFILE",
		"CR_DATABASE_URL",
		"CR_REQUIRE_AGENT_SIG",
		"CR_AUTH_TOKEN",
		"CR_LOG_LEVEL",
		"CR_LOG_FORMAT",
		"CR_RATE_LIMIT_PER_MINUTE",
		"CR_WS_ALLOWED_ORIGINS",
		"CR_FED_MAX_HOLD_S",
		"CR_FED_QUEUE_FILE",
	}

	t.Run("help exits 0 and documents env vars", func(t *testing.T) {
		for _, args := range [][]string{{"--help"}, {"-help"}, {"-h"}} {
			var out strings.Builder
			help, showVersion, _, port, dbURL, pf, err := parseArgs(args, &out)
			if err != nil {
				t.Fatalf("parseArgs(%v) error: %v", args, err)
			}
			if !help {
				t.Fatalf("parseArgs(%v): help = false, want true", args)
			}
			if showVersion {
				t.Fatalf("parseArgs(%v): version = true, want false", args)
			}
			if port != 0 || dbURL != "" || pf != "" {
				t.Fatalf("parseArgs(%v): unexpected overrides port=%d dbURL=%q pidfile=%q", args, port, dbURL, pf)
			}
			usage := out.String()
			for _, want := range append([]string{"Usage:", "crier [flags]", "-port", "-db-url", "-version", "-stop", "-pidfile"}, envVars...) {
				if !strings.Contains(usage, want) {
					t.Errorf("parseArgs(%v): usage output missing %q", args, want)
				}
			}
		}
	})

	t.Run("version", func(t *testing.T) {
		for _, args := range [][]string{{"--version"}, {"-version"}} {
			var out strings.Builder
			help, showVersion, _, port, dbURL, pf, err := parseArgs(args, &out)
			if err != nil {
				t.Fatalf("parseArgs(%v) error: %v", args, err)
			}
			if help {
				t.Fatalf("parseArgs(%v): help = true, want false", args)
			}
			if !showVersion {
				t.Fatalf("parseArgs(%v): version = false, want true", args)
			}
			if port != 0 || dbURL != "" || pf != "" {
				t.Fatalf("parseArgs(%v): unexpected overrides port=%d dbURL=%q pidfile=%q", args, port, dbURL, pf)
			}
		}
	})

	t.Run("port and db-url overrides", func(t *testing.T) {
		var out strings.Builder
		help, showVersion, _, port, dbURL, _, err := parseArgs([]string{"-port", "9999", "-db-url", "postgres://override"}, &out)
		if err != nil {
			t.Fatalf("parseArgs error: %v", err)
		}
		if help || showVersion {
			t.Fatalf("parseArgs: help=%v version=%v, want both false", help, showVersion)
		}
		if port != 9999 {
			t.Fatalf("parseArgs: port = %d, want 9999", port)
		}
		if dbURL != "postgres://override" {
			t.Fatalf("parseArgs: dbURL = %q, want %q", dbURL, "postgres://override")
		}
	})

	t.Run("unset flags leave env alone", func(t *testing.T) {
		var out strings.Builder
		help, showVersion, stop, port, dbURL, pf, err := parseArgs(nil, &out)
		if err != nil {
			t.Fatalf("parseArgs(nil) error: %v", err)
		}
		if help || showVersion || stop {
			t.Fatalf("parseArgs(nil): help=%v version=%v stop=%v, want all false", help, showVersion, stop)
		}
		if port != 0 || dbURL != "" || pf != "" {
			t.Fatalf("parseArgs(nil): unexpected overrides port=%d dbURL=%q pidfile=%q", port, dbURL, pf)
		}
	})

	t.Run("stop and pidfile flags", func(t *testing.T) {
		var out strings.Builder
		_, _, stop, _, _, pf, err := parseArgs([]string{"-stop", "-pidfile", "/tmp/crier.pid"}, &out)
		if err != nil {
			t.Fatalf("parseArgs error: %v", err)
		}
		if !stop {
			t.Fatal("parseArgs: stop = false, want true")
		}
		if pf != "/tmp/crier.pid" {
			t.Fatalf("parseArgs: pidfile = %q, want /tmp/crier.pid", pf)
		}
	})

	t.Run("unknown flag errors", func(t *testing.T) {
		var out strings.Builder
		_, _, _, _, _, _, err := parseArgs([]string{"--bogus"}, &out)
		if err == nil {
			t.Fatal("parseArgs(--bogus): err = nil, want error")
		}
		if !strings.Contains(out.String(), "bogus") {
			t.Errorf("parseArgs(--bogus): output %q does not mention the unknown flag", out.String())
		}
	})
}

// resolvePidfile: an explicit flag wins over the env var; both empty means
// no pidfile (DF-CRIER-194 default-off).
func TestResolvePidfile(t *testing.T) {
	t.Setenv("CR_PIDFILE", "/from/env")
	if got := resolvePidfile(""); got != "/from/env" {
		t.Errorf("resolvePidfile(\"\") = %q, want the env value /from/env", got)
	}
	if got := resolvePidfile("/from/flag"); got != "/from/flag" {
		t.Errorf("resolvePidfile(flag) = %q, want /from/flag (flag wins)", got)
	}
	t.Setenv("CR_PIDFILE", "")
	if got := resolvePidfile(""); got != "" {
		t.Errorf("resolvePidfile with nothing set = %q, want empty", got)
	}
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns
// everything fn wrote there — the TestBindFailureDiagnostic pattern,
// factored out for the -stop refusal tests (the refusal path reports on
// stderr because it is an error outcome).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	return <-done
}

// TestStopNoPidfileIsNoOp: -stop with no pidfile on disk is an idempotent
// success with a clear message (DF-CRIER-194 AC: "nothing to stop" must
// be exit 0 — make stop pairs with make run).
func TestStopNoPidfileIsNoOp(t *testing.T) {
	var out bytes.Buffer
	path := filepath.Join(t.TempDir(), "absent.pid")
	code := stopServer(&out, path)
	if code != 0 {
		t.Fatalf("stopServer(missing pidfile) = %d, want 0\noutput: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "nothing to stop") {
		t.Errorf("output %q does not say \"nothing to stop\"", out.String())
	}

	// The run() wiring: -stop must reach the same code path and exit 0
	// without ever starting the server.
	t.Setenv("CR_PIDFILE", path)
	if code := run([]string{"-stop"}); code != 0 {
		t.Errorf("run(-stop) with no pidfile = %d, want 0", code)
	}
}

// TestStopWithoutPidfilePath: -stop with no path from either flag or env
// is a usage error, not a silent success.
func TestStopWithoutPidfilePath(t *testing.T) {
	t.Setenv("CR_PIDFILE", "")
	var out bytes.Buffer
	code := stopServer(&out, "")
	if code != 2 {
		t.Fatalf("stopServer(no path) = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "-pidfile") {
		t.Errorf("output %q does not mention -pidfile", out.String())
	}
}

// TestStopStalePidfileRemoved: a pidfile naming a dead pid is stale state;
// -stop removes it and succeeds.
func TestStopStalePidfileRemoved(t *testing.T) {
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	path := filepath.Join(t.TempDir(), "crier.pid")
	if err := pidfile.Write(path, pidfile.Record{PID: dead.ProcessState.Pid(), Port: 18995, Binary: "/opt/crier/bin/crier"}); err != nil {
		t.Fatalf("seed stale pidfile: %v", err)
	}

	var out bytes.Buffer
	code := stopServer(&out, path)
	if code != 0 {
		t.Fatalf("stopServer(stale pidfile) = %d, want 0\noutput: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "stale") {
		t.Errorf("output %q does not say the pidfile was stale", out.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale pidfile still present after -stop: %v", err)
	}
}

// TestStopRefusesForeignProcess is the DF-CRIER-194 safety gate: a pidfile
// naming a LIVE process that is not the recorded crier binary must cause a
// non-zero exit, a message naming both binaries, and NO signal delivered —
// the process must still be alive afterwards. The stop command must never
// be able to kill an unrelated process.
func TestStopRefusesForeignProcess(t *testing.T) {
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	defer func() {
		_ = sleep.Process.Kill()
		_, _ = sleep.Process.Wait()
	}()

	path := filepath.Join(t.TempDir(), "crier.pid")
	if err := pidfile.Write(path, pidfile.Record{PID: sleep.Process.Pid, Port: 8767, Binary: "/opt/crier/bin/crier"}); err != nil {
		t.Fatalf("seed foreign pidfile: %v", err)
	}

	var out bytes.Buffer
	var code int
	stderr := captureStderr(t, func() {
		code = stopServer(&out, path)
	})
	if code == 0 {
		t.Fatalf("stopServer(foreign pid) = 0, want non-zero refusal\nstdout: %s\nstderr: %s", out.String(), stderr)
	}
	for _, want := range []string{"REFUSING", "/opt/crier/bin/crier", strconv.Itoa(sleep.Process.Pid)} {
		if !strings.Contains(stderr, want) {
			t.Errorf("refusal output missing %q:\nstderr: %s", want, stderr)
		}
	}
	if !strings.Contains(stderr, "sleep") {
		t.Errorf("refusal output does not name the live binary (sleep):\nstderr: %s", stderr)
	}
	// The load-bearing safety assertion: nothing was signalled.
	if !pidfile.Alive(sleep.Process.Pid) {
		t.Fatal("foreign process was killed; -stop must never signal on mismatch")
	}
	// And the pidfile is left in place for the operator to inspect.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("pidfile removed despite refusal: %v", err)
	}
}

// TestPidfileLifecycle pins the write-side contract: the pidfile exists
// ONLY after a successful bind (pid + port + binary path recorded), and a
// failed bind leaves no pidfile behind.
func TestPidfileLifecycle(t *testing.T) {
	// Deterministic environment: no DB, no auth token, no inherited port.
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	t.Setenv("CR_PIDFILE", "")

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	t.Run("written after successful bind", func(t *testing.T) {
		port := freePort(t)
		pf := filepath.Join(t.TempDir(), "crier.pid")

		done := make(chan int)
		go func() {
			done <- run([]string{"-port", strconv.Itoa(port), "-pidfile", pf})
		}()
		selfProc, _ := os.FindProcess(os.Getpid())
		t.Cleanup(func() {
			_ = selfProc.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Errorf("server did not shut down within 10s of SIGTERM")
			}
		})

		// Wait for the pidfile (bounded). Its existence implies the bind
		// succeeded — it is written only after net.Listen returns.
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(pf); err == nil {
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stat pidfile: %v", err)
			}
			if time.Now().After(deadline) {
				t.Fatal("pidfile not written within 10s")
			}
			time.Sleep(25 * time.Millisecond)
		}

		rec, err := pidfile.Read(pf)
		if err != nil {
			t.Fatalf("read pidfile: %v", err)
		}
		if rec.PID != os.Getpid() {
			t.Errorf("pidfile pid = %d, want own pid %d (in-process run)", rec.PID, os.Getpid())
		}
		if rec.Port != port {
			t.Errorf("pidfile port = %d, want %d", rec.Port, port)
		}
		if rec.Binary != self {
			t.Errorf("pidfile binary = %q, want %q", rec.Binary, self)
		}

		// The listener is up: the port answers.
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err != nil {
			t.Fatalf("dial :%d after pidfile appeared: %v", port, err)
		}
		conn.Close()
	})

	t.Run("failed bind leaves no pidfile", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("occupy port: %v", err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		pf := filepath.Join(t.TempDir(), "should-not-exist.pid")
		code := run([]string{"-port", strconv.Itoa(port), "-pidfile", pf})
		if code != 1 {
			t.Fatalf("run on occupied port = %d, want 1", code)
		}
		if _, err := os.Stat(pf); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("pidfile exists after a failed bind (%v); it must be written only after the listener binds", err)
		}
	})
}

// TestStopRunningServer is the full lifecycle against the REAL binary
// (both halves exec'd, exactly the operator UX): a server started with
// -pidfile, then `<bin> -stop -pidfile …`. In-process run() cannot be
// used here — the recorded pid would be the test process itself, which
// SIGTERM shuts down gracefully but which obviously never exits, so the
// stop poll could only ever time out. Asserts: stop exits 0, the server
// PROCESS is gone, the pidfile is removed, the port is bindable again.
func TestStopRunningServer(t *testing.T) {
	// Same env discipline as TestServerHealth, passed to BOTH children.
	env := append(os.Environ(),
		"CR_AUTH_TOKEN=",
		"CR_DATABASE_URL=",
		"DATABASE_URL=",
		"CRIER_DATABASE_URL=",
		"CR_GUARD_ENABLED=false",
		"CR_PIDFILE=",
	)

	dir := t.TempDir()
	bin := filepath.Join(dir, "crier-server")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}

	port := freePort(t)
	pf := filepath.Join(dir, "crier.pid")

	server := exec.Command(bin, "-port", strconv.Itoa(port), "-pidfile", pf)
	server.Env = env
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		_ = server.Process.Kill()
		_, _ = server.Process.Wait()
	})

	// Wait for the pidfile = the listener is bound (written only after
	// a successful bind).
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(pf); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not write its pidfile within 15s")
		}
		time.Sleep(25 * time.Millisecond)
	}
	rec, err := pidfile.Read(pf)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if rec.PID != server.Process.Pid {
		t.Fatalf("pidfile pid = %d, want the server child's pid %d", rec.PID, server.Process.Pid)
	}
	if rec.Port != port {
		t.Errorf("pidfile port = %d, want %d", rec.Port, port)
	}

	// The stop half: a separate process, exactly what an operator runs.
	stop := exec.Command(bin, "-stop", "-pidfile", pf)
	stop.Env = env
	stopOut, err := stop.CombinedOutput()
	if err != nil {
		t.Fatalf("-stop exited with error: %v\n%s", err, stopOut)
	}
	if !strings.Contains(string(stopOut), "stopped") {
		t.Errorf("-stop output %q does not report the stop", stopOut)
	}

	// The server process is gone.
	waitDone := make(chan error, 1)
	go func() { waitDone <- server.Wait() }()
	select {
	case <-waitDone: // reaped: definitively exited
	case <-time.After(10 * time.Second):
		t.Fatal("server process still alive 10s after -stop")
	}

	// The pidfile is removed.
	if _, err := os.Stat(pf); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pidfile still present after stop: %v", err)
	}

	// The port is provably free: we can bind it ourselves.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d still occupied after stop: %v", port, err)
	}
	ln.Close()
}

// TestOpenAPIServed is the CR-GAP-049 end-to-end gate: on a running server,
// GET /openapi.json returns HTTP 200 with the spec as valid JSON (openapi ==
// "3.1.0"), GET /openapi.yaml returns the raw embedded spec, and GET /docs
// returns a self-contained HTML page linking to both. Auth is ENABLED on
// purpose — the spec endpoints must stay reachable without a token (exempt
// from middleware.Auth like /health) while the rest of the API stays locked.
func TestOpenAPIServed(t *testing.T) {
	// Skip on Go 1.25 — same SIGTERM-in-go-test caveat as TestServerHealth.
	if strings.HasPrefix(runtime.Version(), "go1.25") {
		t.Skip("skipping on Go 1.25: SIGTERM handling in go test differs from 1.26")
	}

	t.Setenv("CR_AUTH_TOKEN", "test-token")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")

	port := freePort(t)
	t.Setenv("CRIER_PORT", fmt.Sprintf("%d", port))

	done := make(chan struct{})
	go func() {
		defer close(done)
		run(nil)
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

	// Wait for the server to come up (bounded). /health is public even with
	// auth enabled, so it is a safe readiness probe.
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start within 10s: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	t.Run("openapi.json is valid 3.1.0 JSON without auth", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/openapi.json")
		if err != nil {
			t.Fatalf("GET /openapi.json: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /openapi.json: status %d, want %d", resp.StatusCode, http.StatusOK)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET /openapi.json: Content-Type %q, want %q", ct, "application/json")
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /openapi.json: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("GET /openapi.json: body is not valid JSON: %v", err)
		}
		if doc["openapi"] != "3.1.0" {
			t.Fatalf("GET /openapi.json: openapi = %v, want %q", doc["openapi"], "3.1.0")
		}
	})

	t.Run("openapi.yaml matches the embedded spec without auth", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/openapi.yaml")
		if err != nil {
			t.Fatalf("GET /openapi.yaml: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /openapi.yaml: status %d, want %d", resp.StatusCode, http.StatusOK)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /openapi.yaml: %v", err)
		}
		if string(body) != string(openapiYAML) {
			t.Errorf("GET /openapi.yaml: body (%d bytes) differs from the embedded spec (%d bytes)", len(body), len(openapiYAML))
		}
	})

	t.Run("docs links to both endpoints without auth", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/docs")
		if err != nil {
			t.Fatalf("GET /docs: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /docs: status %d, want %d", resp.StatusCode, http.StatusOK)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /docs: %v", err)
		}
		html := string(body)
		if !strings.Contains(html, "/openapi.json") || !strings.Contains(html, "/openapi.yaml") {
			t.Error("GET /docs: page does not link to both /openapi.json and /openapi.yaml")
		}
	})

	t.Run("protected routes still require auth", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/agents")
		if err != nil {
			t.Fatalf("GET /agents: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /agents without token: status %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})
}

// TestOpenAPIDocsSpec validates docs/openapi.yaml itself (the source of
// truth): it parses as YAML, declares openapi 3.1.0, and is byte-identical
// to the embedded copy generated from it. This is the test the CI
// openapi-spec-validator job runs, so spec-vs-code drift breaks CI instead
// of hiding.
func TestOpenAPIDocsSpec(t *testing.T) {
	docsSpec, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("read docs/openapi.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(docsSpec, &doc); err != nil {
		t.Fatalf("docs/openapi.yaml is not valid YAML: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("docs/openapi.yaml: openapi = %v, want %q", doc["openapi"], "3.1.0")
	}
	if string(docsSpec) != string(openapiYAML) {
		t.Errorf("docs/openapi.yaml (%d bytes) differs from the embedded cmd/server/openapi.yaml (%d bytes) — run `go generate ./cmd/server`", len(docsSpec), len(openapiYAML))
	}
}

// TestVersionEndpointServed is the DF-CRIER-101 end-to-end gate: on a running
// server, GET /version returns the RESOLVED build identity as JSON (not a
// hardcoded string), and it is reachable without a token while auth is
// enabled — exempt from middleware.Auth exactly like /health.
func TestVersionEndpointServed(t *testing.T) {
	// Skip on Go 1.25 — same SIGTERM-in-go-test caveat as TestServerHealth.
	if strings.HasPrefix(runtime.Version(), "go1.25") {
		t.Skip("skipping on Go 1.25: SIGTERM handling in go test differs from 1.26")
	}

	t.Setenv("CR_AUTH_TOKEN", "test-token")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")

	port := freePort(t)
	t.Setenv("CRIER_PORT", fmt.Sprintf("%d", port))

	done := make(chan struct{})
	go func() {
		defer close(done)
		run(nil)
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

	// /health is public even with auth on, so it is the readiness probe.
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start within 10s: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}

	resp, err := client.Get(baseURL + "/version")
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /version without a token: status %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /version: Content-Type %q, want %q", ct, "application/json")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /version body: %v", err)
	}

	var served map[string]any
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("GET /version: body %q is not valid JSON: %v", body, err)
	}

	// The handler must serve the package's resolved identity — anything
	// else (a stale var, a hardcoded string) is the bug this closes.
	want := buildinfo.Resolve()
	for key, wantValue := range map[string]any{
		"version":    want.Version,
		"commit":     want.Commit,
		"build_time": want.BuildTime,
		"modified":   want.Modified,
	} {
		if got := served[key]; got != wantValue {
			t.Errorf("GET /version: %s = %#v, want %#v (buildinfo.Resolve)", key, got, wantValue)
		}
	}

	if len(served) != 4 {
		t.Errorf("GET /version: %d keys %v, want exactly version/commit/build_time/modified", len(served), served)
	}
	// The test binary is built by `go test`, which may or may not stamp VCS
	// metadata; when it did, the served identity must carry a real commit
	// rather than the "unknown" sentinel.
	if want.Commit == buildinfo.DefaultCommit {
		t.Logf("test binary has no VCS metadata — served identity %q (the exec test covers the fallback)", want.String())
	} else if got := served["commit"]; got == buildinfo.DefaultCommit {
		t.Errorf("GET /version: commit = %q, want %q (vcs.revision was available)", got, want.Commit)
	}
}

// TestServerVersionCLIFlags is the DF-CRIER-106/116/127 acceptance gate on the
// real binary. The unstamped case is the load-bearing one: a bare `go build`
// with NO ldflags must still report the commit the binary was built from, via
// the VCS metadata the Go toolchain embeds. The second build proves the
// Makefile's -X target path is wired correctly — the linker SILENTLY ignores
// an -X flag naming a symbol it cannot find, so only an assertion on the
// output can catch a typo'd path.
func TestServerVersionCLIFlags(t *testing.T) {
	dir := t.TempDir()
	plainBin := filepath.Join(dir, "crier-plain")
	injectedBin := filepath.Join(dir, "crier-injected")

	// A sibling worker may commit while this test builds; accept either HEAD
	// observed around the build.
	headBefore := gitHead(t, ".")

	if out, err := exec.Command("go", "build", "-o", plainBin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build unstamped crier: %v\n%s", err, out)
	}
	injectedLDFlags := "-X github.com/crier-dev/crier/internal/buildinfo.Version=9.9.9"
	if out, err := exec.Command("go", "build", "-ldflags", injectedLDFlags, "-o", injectedBin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build stamped crier: %v\n%s", err, out)
	}

	headAfter := gitHead(t, ".")

	t.Run("unstamped build reports the commit it was built from", func(t *testing.T) {
		out, err := exec.Command(plainBin, "-version").CombinedOutput()
		if err != nil {
			t.Fatalf("-version exited with error: %v\n%s", err, out)
		}
		identity := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "crier"))
		// DF-CRIER-171: an unstamped build renders the version sentinel BARE
		// ("dev-<commit>"), never glued to a "v" ("vdev-<commit>"). A stamped
		// version keeps its prefix ("v9.9.9-<commit>").
		if strings.Contains(identity, "v"+buildinfo.DefaultVersion) {
			t.Fatalf("-version output %q glues the version prefix onto the %q sentinel (want %q)", out, buildinfo.DefaultVersion, "dev-<commit>[-dirty]")
		}
		if !strings.HasPrefix(identity, "v") && !strings.HasPrefix(identity, buildinfo.DefaultVersion+"-") {
			t.Fatalf("-version output %q is neither canonical form: %q or %q", out, "crier v<version>-<commit>", "crier dev-<commit>")
		}
		if identity == "v"+buildinfo.DefaultVersion {
			t.Fatalf("-version printed %q — the placeholder version with no commit (DF-CRIER-127)", strings.TrimSpace(string(out)))
		}

		commit := identityCommit(identity)
		if commit == "" {
			t.Fatalf("-version identity %q carries no commit segment", identity)
		}
		if !isShortHex(commit) {
			t.Fatalf("-version identity %q: commit segment %q is not a short git revision", identity, commit)
		}

		switch {
		case headBefore == "" && headAfter == "":
			t.Logf("git unavailable: cannot tie commit %q to a repo revision", commit)
		case commit != shortSha(headBefore) && commit != shortSha(headAfter):
			t.Errorf("-version reports commit %q, which is neither HEAD (%s) nor the revision HEAD moved to (%s)",
				commit, shortSha(headBefore), shortSha(headAfter))
		}
	})

	t.Run("ldflags-stamped version wins, commit still resolved", func(t *testing.T) {
		out, err := exec.Command(injectedBin, "-version").CombinedOutput()
		if err != nil {
			t.Fatalf("-version exited with error: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "v9.9.9-") {
			t.Errorf("-version output %q does not carry the injected version %q — check the -X target path in the Makefile", out, "v9.9.9-")
		}
		if got := identityCommit(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "crier"))); !isShortHex(got) {
			t.Errorf("-version output %q does not carry a resolved commit", out)
		}
	})

	t.Run("Makefile stamps the symbols the binary reads", func(t *testing.T) {
		// The linker SILENTLY ignores an -X flag naming a symbol it cannot
		// find, so a typo'd package path in the Makefile would keep every
		// build green while the identity stayed unstamped. `make -n` prints
		// the fully expanded command without running it — that text is the
		// only proof the release wiring points at the symbols
		// internal/buildinfo actually reads.
		out, err := exec.Command("make", "-C", "../..", "-n", "build").CombinedOutput()
		if err != nil {
			t.Fatalf("make -C ../.. -n build: %v\n%s", err, out)
		}
		expanded := string(out)
		const pkg = "github.com/crier-dev/crier/internal/buildinfo"
		for _, symbol := range []string{".Version", ".Commit", ".BuildTime"} {
			if !strings.Contains(expanded, "-X "+pkg+symbol+"=") {
				t.Errorf("`make -n build` does not stamp %s%s:\n%s", pkg, symbol, expanded)
			}
		}
		if head := gitHead(t, "."); head != "" && !strings.Contains(expanded, "-X "+pkg+".Commit="+head) {
			t.Errorf("`make -n build` does not stamp HEAD (%s) as the commit:\n%s", head, expanded)
		}
	})
}

// gitHead returns the current HEAD sha of the repository containing dir, or ""
// when git is unavailable (the caller then cannot tie a reported commit to a
// revision and says so instead of failing).
func gitHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// shortSha returns the 8 characters a build identity carries.
func shortSha(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// identityCommit extracts the trailing commit segment of a canonical identity
// ("v<version>-<commit>[-dirty]", where the version may itself contain dashes,
// e.g. a git-describe string): "v1.2.3-1a2b3c4d-dirty" → "1a2b3c4d", "vdev" →
// "".
func identityCommit(identity string) string {
	trimmed := strings.TrimSuffix(identity, "-dirty")
	idx := strings.LastIndex(trimmed, "-")
	if idx < 0 {
		return ""
	}
	return trimmed[idx+1:]
}

// isShortHex reports whether s is 1-8 hex digits, i.e. a shortened git
// revision rather than a word like "unknown".
func isShortHex(s string) bool {
	if s == "" || len(s) > 8 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
