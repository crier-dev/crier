package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestServerHealth is an entrypoint smoke test: it runs main() on a random
// free port, hits /health, verifies a 200 response, then triggers graceful
// shutdown via SIGTERM. If someone breaks the wiring in main.go (router,
// middleware, listener), this test fails immediately.
func TestServerHealth(t *testing.T) {
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
		main()
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
