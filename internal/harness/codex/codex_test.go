package codex_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/codex"
)

// ---------------------------------------------------------------------------
// Test doubles
//
// The adapter's Exec seam is the documented way to run the CLI through
// something else (a sandbox, a wrapper, an in-process stand-in). These tests
// use it to answer with output codex really produced (the retry line, the
// reqwest error class, the thread handshake) without needing the binary, a
// model, or a network.
// ---------------------------------------------------------------------------

// stubExec records every spec it is handed and answers with one canned result.
type stubExec struct {
	res   harness.ExecResult
	specs []harness.ExecSpec
}

func (s *stubExec) run(_ context.Context, spec harness.ExecSpec, _ time.Duration) harness.ExecResult {
	s.specs = append(s.specs, spec)
	return s.res
}

// binary writes an executable placeholder. The Exec seam never runs it, so the
// only thing it must be is present: LookupBinary refuses a missing path.
func binary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	return path
}

// closedEndpoint returns a loopback URL that was listening and is not anymore:
// a connect to it is refused immediately, which is what a dead router looks
// like from this host without any egress.
func closedEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return "http://" + addr + "/v1"
}

// liveEndpoint returns a URL whose host accepts TCP connections (so the
// adapter's diagnostic connect succeeds) and answers any request with 200.
func liveEndpoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

// deliver runs one delivery and fails the test on a returned error: all
// harness-side outcomes belong in the receipt.
func deliver(t *testing.T, a *codex.Adapter, id string) harness.Receipt {
	t.Helper()
	rc, err := a.Deliver(context.Background(), harness.SessionRef{ID: id},
		harness.Message{ID: "m_1", SessionID: "s_1", Text: "hello"})
	if err != nil {
		t.Fatalf("deliver returned an error: %v", err)
	}
	return rc
}

// argvPairs asserts a `-c key="value"` pair is present exactly as codex parses
// it, and returns the argv for further assertions.
func argvPairs(t *testing.T, argv []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for i, a := range argv {
			if a == "-c" && i+1 < len(argv) && argv[i+1] == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("argv is missing -c %s:\n%q", w, argv)
		}
	}
}

// ---------------------------------------------------------------------------
// The provider knob
// ---------------------------------------------------------------------------

// TestBaseURLKnobBecomesACompleteProviderOverride pins the shape codex actually
// accepts: a custom provider definition, because 0.166.0 refuses to override
// the reserved built-in id `openai`.
func TestBaseURLKnobBecomesACompleteProviderOverride(t *testing.T) {
	const base = "http://127.0.0.1:18080/v1"
	stub := &stubExec{res: harness.ExecResult{
		Stdout: []byte(`{"type":"thread.started","thread_id":"t-1"}`),
	}}
	a := codex.New(codex.Config{Binary: binary(t), BaseURL: base, Exec: stub.run})

	rc := deliver(t, a, "")
	if rc.State != harness.StateDispatched {
		t.Fatalf("state = %q, want dispatched (error %q tail %q)", rc.State, rc.Error, rc.Tail)
	}
	if rc.SessionID != "t-1" {
		t.Fatalf("receipt must carry the minted thread id, got %q", rc.SessionID)
	}
	if len(stub.specs) != 1 {
		t.Fatalf("want 1 invocation, got %d", len(stub.specs))
	}
	argvPairs(t, stub.specs[0].Args,
		`model_provider="crier"`,
		`model_providers.crier.name="Crier"`,
		`model_providers.crier.base_url="`+base+`"`,
		`model_providers.crier.wire_api="responses"`,
		`model_providers.crier.env_key="OPENAI_API_KEY"`,
	)
	// The create path still carries its explicit sandbox (-s, not -c).
	idx := indexOf(stub.specs[0].Args, "-s")
	if idx < 0 || idx+1 >= len(stub.specs[0].Args) || stub.specs[0].Args[idx+1] != "workspace-write" {
		t.Fatalf("create argv must carry -s workspace-write: %q", stub.specs[0].Args)
	}
	if !a.Features()["base_url"] {
		t.Fatalf("features must advertise the configured provider override: %v", a.Features())
	}
}

// TestBaseURLKnobSurvivesTheResumePath: the override belongs on BOTH paths, or
// a continued thread would fall back to the host's own routing.
func TestBaseURLKnobSurvivesTheResumePath(t *testing.T) {
	const base = "http://127.0.0.1:18080/v1"
	stub := &stubExec{res: harness.ExecResult{}}
	a := codex.New(codex.Config{Binary: binary(t), BaseURL: base, Exec: stub.run})

	rc := deliver(t, a, "01a1a422-dad4-78e3-99b0-6c6b3bfd2c1d")
	if rc.State != harness.StateDispatched {
		t.Fatalf("state = %q, want dispatched", rc.State)
	}
	argv := stub.specs[0].Args
	if len(argv) < 2 || argv[0] != "exec" || argv[1] != "resume" {
		t.Fatalf("want `exec resume`, got %q", argv)
	}
	argvPairs(t, argv, `model_providers.crier.base_url="`+base+`"`)
	if indexOf(argv, "01a1a422-dad4-78e3-99b0-6c6b3bfd2c1d") < 0 {
		t.Fatalf("resume argv must carry the thread id: %q", argv)
	}
}

// TestEnvBaseURLIsTheDefaultKnob covers CR_CODEX_BASE_URL and the precedence
// rule: an explicit Config.BaseURL wins over the environment.
func TestEnvBaseURLIsTheDefaultKnob(t *testing.T) {
	const fromEnv = "http://127.0.0.1:18081/v1"
	const fromCfg = "http://127.0.0.1:18082/v1"

	t.Run("environment", func(t *testing.T) {
		t.Setenv(codex.EnvBaseURL, fromEnv)
		stub := &stubExec{}
		a := codex.New(codex.Config{Binary: binary(t), Exec: stub.run})
		deliver(t, a, "")
		argvPairs(t, stub.specs[0].Args, `model_providers.crier.base_url="`+fromEnv+`"`)
		if !a.Features()["base_url"] {
			t.Fatalf("features must advertise the env-provided override: %v", a.Features())
		}
	})

	t.Run("config wins", func(t *testing.T) {
		t.Setenv(codex.EnvBaseURL, fromEnv)
		stub := &stubExec{}
		a := codex.New(codex.Config{Binary: binary(t), BaseURL: fromCfg, Exec: stub.run})
		deliver(t, a, "")
		argvPairs(t, stub.specs[0].Args, `model_providers.crier.base_url="`+fromCfg+`"`)
	})
}

// TestNoKnobLeavesTheArgvAlone keeps the shipped behaviour: without the knob
// nothing about the invocation changes and the CLI keeps the host's routing.
func TestNoKnobLeavesTheArgvAlone(t *testing.T) {
	t.Setenv(codex.EnvBaseURL, "")
	stub := &stubExec{}
	a := codex.New(codex.Config{Binary: binary(t), Exec: stub.run})
	deliver(t, a, "")

	for _, arg := range stub.specs[0].Args {
		if strings.Contains(arg, "model_provider") {
			t.Fatalf("no base URL was configured, so argv must not name a provider: %q", stub.specs[0].Args)
		}
	}
	if a.Features()["base_url"] {
		t.Fatalf("features must not advertise a provider override that is not configured: %v", a.Features())
	}
}

// ---------------------------------------------------------------------------
// The named failure
// ---------------------------------------------------------------------------

// TestRouterDownIsANamedFailure is AC(1) in miniature: codex's real retry line
// is what a dead endpoint produces, the delivery times out with it, and the
// receipt must name the cause with the target URL and the operating system's
// dial error rather than a bare timeout.
func TestRouterDownIsANamedFailure(t *testing.T) {
	dead := closedEndpoint(t)
	stub := &stubExec{res: harness.ExecResult{
		ExitCode: -1,
		TimedOut: true,
		// Verbatim from codex 0.166.0 against a refused connection.
		Stdout: []byte(`{"type":"error","message":"Reconnecting... waiting for network (Connection failed: error sending request)"}`),
	}}
	a := codex.New(codex.Config{Binary: binary(t), BaseURL: dead, Timeout: 2 * time.Second, Exec: stub.run})

	start := time.Now()
	rc := deliver(t, a, "")
	elapsed := time.Since(start)

	if rc.State != harness.StateFailed || rc.Error != codex.CodeProviderUnreachable {
		t.Fatalf("want failed/%s, got %+v", codex.CodeProviderUnreachable, rc)
	}
	if !strings.Contains(rc.Tail, dead) {
		t.Fatalf("tail must quote the target URL %q: %q", dead, rc.Tail)
	}
	if !strings.Contains(rc.Tail, "connection refused") {
		t.Fatalf("tail must quote the dial error: %q", rc.Tail)
	}
	if !strings.Contains(rc.Tail, "Reconnecting") {
		t.Fatalf("tail must keep codex's own words: %q", rc.Tail)
	}
	if strings.HasPrefix(rc.Tail, "delivery exceeded") {
		t.Fatalf("a refused endpoint must not be reported as a bare timeout: %q", rc.Tail)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the diagnostic connect must be bounded, took %s", elapsed)
	}
}

// TestChildReportedURLIsUsedWithoutTheKnob: a CLI that names the endpoint it
// could not reach is still diagnosable, so the knob is an improvement and not
// a requirement.
func TestChildReportedURLIsUsedWithoutTheKnob(t *testing.T) {
	dead := closedEndpoint(t)
	stub := &stubExec{res: harness.ExecResult{
		ExitCode: 1,
		Stderr: []byte(`■ stream error: error sending request for url (` + dead + `/responses): ` +
			`client error (Connect): tcp connect error: Connection refused (os error 111)` + "\n"),
	}}
	a := codex.New(codex.Config{Binary: binary(t), Timeout: 2 * time.Second, Exec: stub.run})

	rc := deliver(t, a, "")
	if rc.Error != codex.CodeProviderUnreachable {
		t.Fatalf("want %s, got %+v", codex.CodeProviderUnreachable, rc)
	}
	if !strings.Contains(rc.Tail, dead) {
		t.Fatalf("tail must quote the URL the CLI named %q: %q", dead, rc.Tail)
	}
	if !strings.Contains(rc.Tail, "connection refused") {
		t.Fatalf("tail must quote the dial error: %q", rc.Tail)
	}
}

// TestReachableEndpointIsNotCalledUnreachable is the false-positive guard: the
// endpoint answered a TCP connect, so the failure is a slow turn and keeps
// HARNESS_TIMEOUT — with the target named, because a timeout with no context is
// the bug this row exists to fix.
func TestReachableEndpointIsNotCalledUnreachable(t *testing.T) {
	live := liveEndpoint(t)
	stub := &stubExec{res: harness.ExecResult{
		ExitCode: -1,
		TimedOut: true,
		Stdout:   []byte(`{"type":"error","message":"Reconnecting... waiting for network (Connection failed: error sending request)"}`),
	}}
	a := codex.New(codex.Config{Binary: binary(t), BaseURL: live, Timeout: 2 * time.Second, Exec: stub.run})

	rc := deliver(t, a, "")
	if rc.Error != harness.CodeTimeout {
		t.Fatalf("a reachable endpoint must stay a timeout, got %+v", rc)
	}
	if strings.Contains(rc.Tail, "unreachable") {
		t.Fatalf("a reachable endpoint must never be reported as unreachable: %q", rc.Tail)
	}
	if !strings.Contains(rc.Tail, live) {
		t.Fatalf("even a plain timeout must name the target: %q", rc.Tail)
	}
}

// TestAuthFailureIsNotRelabelled: an exit-non-zero with no connection class is
// left alone (and costs no diagnostic connect).
func TestAuthFailureIsNotRelabelled(t *testing.T) {
	stub := &stubExec{res: harness.ExecResult{
		ExitCode: 3,
		Stderr:   []byte("provider authentication failed\n"),
		Tail:     "provider authentication failed",
	}}
	a := codex.New(codex.Config{Binary: binary(t), BaseURL: liveEndpoint(t), Exec: stub.run})

	rc := deliver(t, a, "")
	if rc.Error != harness.CodeExitNonZero {
		t.Fatalf("want %s, got %+v", harness.CodeExitNonZero, rc)
	}
	if !strings.Contains(rc.Tail, "authentication failed") {
		t.Fatalf("the CLI's own tail must survive: %q", rc.Tail)
	}
}

// TestTimeoutWithoutATargetKeepsItsShape: with no knob and no URL in the
// child's output there is nothing to quote, so the legacy tail stands.
func TestTimeoutWithoutATargetKeepsItsShape(t *testing.T) {
	t.Setenv(codex.EnvBaseURL, "")
	stub := &stubExec{res: harness.ExecResult{ExitCode: -1, TimedOut: true}}
	a := codex.New(codex.Config{Binary: binary(t), Timeout: 2 * time.Second, Exec: stub.run})

	rc := deliver(t, a, "")
	if rc.Error != harness.CodeTimeout {
		t.Fatalf("want %s, got %+v", harness.CodeTimeout, rc)
	}
	if rc.Tail != "delivery exceeded 2s" {
		t.Fatalf("tail = %q, want the unchanged shape %q", rc.Tail, "delivery exceeded 2s")
	}
}

func indexOf(argv []string, want string) int {
	for i, a := range argv {
		if a == want {
			return i
		}
	}
	return -1
}
