package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/buildinfo"
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

// TestParseArgs exercises the CLI flag parsing directly (no exec, no server
// startup). The --help and --version paths are the CR-GAP-005 hard gate.
func TestParseArgs(t *testing.T) {
	envVars := []string{
		"CRIER_PORT",
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
			help, showVersion, port, dbURL, err := parseArgs(args, &out)
			if err != nil {
				t.Fatalf("parseArgs(%v) error: %v", args, err)
			}
			if !help {
				t.Fatalf("parseArgs(%v): help = false, want true", args)
			}
			if showVersion {
				t.Fatalf("parseArgs(%v): version = true, want false", args)
			}
			if port != 0 || dbURL != "" {
				t.Fatalf("parseArgs(%v): unexpected overrides port=%d dbURL=%q", args, port, dbURL)
			}
			usage := out.String()
			for _, want := range append([]string{"Usage:", "crier [flags]", "-port", "-db-url", "-version"}, envVars...) {
				if !strings.Contains(usage, want) {
					t.Errorf("parseArgs(%v): usage output missing %q", args, want)
				}
			}
		}
	})

	t.Run("version", func(t *testing.T) {
		for _, args := range [][]string{{"--version"}, {"-version"}} {
			var out strings.Builder
			help, showVersion, port, dbURL, err := parseArgs(args, &out)
			if err != nil {
				t.Fatalf("parseArgs(%v) error: %v", args, err)
			}
			if help {
				t.Fatalf("parseArgs(%v): help = true, want false", args)
			}
			if !showVersion {
				t.Fatalf("parseArgs(%v): version = false, want true", args)
			}
			if port != 0 || dbURL != "" {
				t.Fatalf("parseArgs(%v): unexpected overrides port=%d dbURL=%q", args, port, dbURL)
			}
		}
	})

	t.Run("port and db-url overrides", func(t *testing.T) {
		var out strings.Builder
		help, showVersion, port, dbURL, err := parseArgs([]string{"-port", "9999", "-db-url", "postgres://override"}, &out)
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
		help, showVersion, port, dbURL, err := parseArgs(nil, &out)
		if err != nil {
			t.Fatalf("parseArgs(nil) error: %v", err)
		}
		if help || showVersion {
			t.Fatalf("parseArgs(nil): help=%v version=%v, want both false", help, showVersion)
		}
		if port != 0 || dbURL != "" {
			t.Fatalf("parseArgs(nil): unexpected overrides port=%d dbURL=%q", port, dbURL)
		}
	})

	t.Run("unknown flag errors", func(t *testing.T) {
		var out strings.Builder
		_, _, _, _, err := parseArgs([]string{"--bogus"}, &out)
		if err == nil {
			t.Fatal("parseArgs(--bogus): err = nil, want error")
		}
		if !strings.Contains(out.String(), "bogus") {
			t.Errorf("parseArgs(--bogus): output %q does not mention the unknown flag", out.String())
		}
	})
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
		if !strings.HasPrefix(identity, "v") {
			t.Fatalf("-version output %q is not the canonical identity (want %q)", out, "crier v<version>-<commit>")
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
