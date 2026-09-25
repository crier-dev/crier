package main

// CR-FEAT-034: the durable path is the documented DEFAULT, and the non-durable
// one is never presented as a recommendation. The docs half of that lives in
// README.md / docs/integration-guide.md and is gated by docs/claims.yaml (the
// COUNT-DOCS-* claims in cmd/server/docsclaims_test.go re-measure those docs).
//
// This file covers the runtime half (the CR-FEAT-031 pairing the row offers):
// a server started with NO CR_DATABASE_URL must say out loud that it is running
// the demo-only, non-durable store. An INFO line carrying a hint was measured to
// be overlookable (DISPATCH · CRI-001: the reviewer's first-run experience was
// "a demo of non-durable durability"), so the claim here is a WARN naming the
// consequence and the fix — asserted from the REAL run() wiring, not from a
// re-implementation of the string.

import (
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// (syncBuffer, the goroutine-safe capture buffer, lives in fallback_test.go —
// the access-log capture helper — so this file reuses it rather than declaring
// a second one.)

// capturedServer is an in-process server booted by run() with os.Stderr
// redirected into a pipe, so a test can assert on what STARTUP logged.
type capturedServer struct {
	port      int
	buf       *syncBuffer
	done      chan int
	r, w      *os.File
	saved     *os.File
	drainDone chan struct{}
	stopOne   sync.Once
}

// bootCapturedServer starts run(nil) on its own free port with stderr captured
// and waits until /health answers. Env must already be set by the caller.
func bootCapturedServer(t *testing.T, port int) *capturedServer {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w

	cs := &capturedServer{port: port, buf: &syncBuffer{}, done: make(chan int, 1), r: r, w: w, saved: saved}
	// Registered BEFORE the server starts: a t.Fatalf below must still restore
	// os.Stderr and reap the server, or every later test in this package logs
	// into a pipe nobody drains.
	t.Cleanup(cs.Stop)

	go func() { cs.done <- run(nil) }()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(cs.buf, r)
	}()
	cs.drainDone = drained

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health")
		if err == nil {
			resp.Body.Close()
			return cs
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start within 10s: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Stop is idempotent: one SIGTERM to this test process (the same graceful path
// make stop uses), then the pipe is closed, drained and os.Stderr restored.
func (c *capturedServer) Stop() {
	c.stopOne.Do(func() {
		if self, err := os.FindProcess(os.Getpid()); err == nil {
			_ = self.Signal(syscall.SIGTERM)
		}
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
		}
		_ = c.w.Close()
		if c.drainDone != nil {
			<-c.drainDone
		}
		_ = c.r.Close()
		os.Stderr = c.saved
	})
}

// Logs returns everything startup wrote to stderr so far.
func (c *capturedServer) Logs() string { return c.buf.String() }

// TestMemoryBackendWarningIsLoud: with no database URL the server runs the
// in-memory store — demo-only, process-lifetime — and startup must say so in a
// way a reader cannot mistake for a normal line. The claim is the WARN level,
// the consequence, the fix and the machine-readable durable=false field, all
// measured on what the REAL run() wrote.
func TestMemoryBackendWarningIsLoud(t *testing.T) {
	if strings.HasPrefix(runtime.Version(), "go1.25") {
		t.Skip("skipping on Go 1.25: SIGTERM handling in go test differs from 1.26")
	}
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("CR_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	port := freePort(t)
	t.Setenv("CRIER_PORT", strconv.Itoa(port))

	cs := bootCapturedServer(t, port)

	// The posture is readable at the endpoint that exists for exactly this
	// question: GET /status reports the backend actually serving.
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/status")
	if err != nil {
		t.Fatalf("GET /status: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"registry_backend":"memory"`) {
		t.Errorf("/status = %s, want it to report the in-memory backend", string(body))
	}

	// The server is up, so startup has logged everything it is going to.
	cs.Stop()
	logs := cs.Logs()

	wants := []string{
		"level=WARN",
		"DEMO-ONLY",
		"non-durable",
		"CR_DATABASE_URL",
		"durable=false",
	}
	for _, want := range wants {
		if !strings.Contains(logs, want) {
			t.Errorf("startup log on the in-memory backend is missing %q — the non-durable store is the product's most surprising default and must announce itself\nstderr:\n%s", want, logs)
		}
	}
	// It must still be findable by a machine: the structured field survives.
	if !strings.Contains(logs, `type=memory`) {
		t.Errorf("startup log no longer names the backend it selected (type=memory)\nstderr:\n%s", logs)
	}
}

// TestPostgresBackendStartupDoesNotWarnAboutMemory is the NEGATIVE CONTROL for
// the claim above: the same startup with a database URL configured must NOT
// print the in-memory warning, and must not fall back to the memory store when
// the URL is unusable — it fails to start instead. Without this, a warning
// printed unconditionally would satisfy the test above and prove nothing about
// the branch.
func TestPostgresBackendStartupDoesNotWarnAboutMemory(t *testing.T) {
	t.Setenv("CR_AUTH_TOKEN", "")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("CRIER_DATABASE_URL", "")
	// A connection refused instantly: nothing listens on port 1, and this must
	// stay a fast failure (no dial timeout to wait out).
	t.Setenv("CR_DATABASE_URL", "postgres://crier:crier@127.0.0.1:1/crier?sslmode=disable")
	t.Setenv("CRIER_PORT", strconv.Itoa(freePort(t)))

	var code int
	out := captureStderr(t, func() {
		code = run(nil)
	})

	if code == 0 {
		t.Fatalf("run() with an unreachable CR_DATABASE_URL = 0, want a non-zero exit (no silent fallback to the memory store)\nstderr:\n%s", out)
	}
	if !strings.Contains(out, "initialize PostgreSQL registry store") {
		t.Errorf("startup did not name the PostgreSQL failure it hit\nstderr:\n%s", out)
	}
	if strings.Contains(out, "DEMO-ONLY") || strings.Contains(out, "durable=false") {
		t.Errorf("the in-memory warning printed on the PostgreSQL path — it is not gated on the backend actually selected\nstderr:\n%s", out)
	}
}
