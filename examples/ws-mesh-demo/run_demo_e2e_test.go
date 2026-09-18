package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests RUN the demo rather than only reading its source (the
// source-invariant tests in run_demo_test.go pin the script's shape; these pin
// its behaviour):
//
//   - TestRunDemoEndToEndOnAScratchPort executes `bash run-demo.sh` on a port
//     the test itself proved free, and asserts from the transcript that the
//     connected agent was listed by /mesh/peers, that the relay event was
//     received on the exact topic, and that the pid the script started is the
//     pid HOLDING the port (ownership, not mere presence);
//   - TestRunDemoRefusesAForeignServerOnItsPort is the negative control: a
//     squatter that answers /health with 200 and /mesh/peers with an EMPTY list —
//     i.e. a server that would yield a full green — must make the run abort
//     loudly, before it builds or starts anything.
//
// Neither bound is a fixed wall-clock pass condition: every assertion is on a
// transcript line the script writes only after the host really did the step,
// and the only clocks are generous hang guards.

const (
	// demoScript is the shipped command under test, run from this directory.
	demoScript = "run-demo.sh"
	// demoRunBudget is a HANG GUARD for the whole script, not a timing
	// assertion: the script's own steps poll until they succeed (a cold Go
	// build cache plus a loaded box is the slow case), and it is killed with
	// its process group if this expires.
	demoRunBudget = 150 * time.Second
	// demoReadyBudget bounds the wait for the script's "relay is up" line so the
	// ownership assertion happens while the server is still running.
	demoReadyBudget = 120 * time.Second
)

var (
	// ssPortPID is the holder pid inside an `ss -tlnp` line: users:(("crier",pid=123,fd=3))
	ssPortPID = regexp.MustCompile(`pid=([0-9]+)`)
	// demoServerPID is the script's own readiness line naming the relay pid.
	demoServerPID = regexp.MustCompile(`relay healthy \(our pid ([0-9]+)\)`)
)

// requireDemoTools skips (with a reason) when the demo's prerequisites are not
// on PATH, so a minimal container fails as "not runnable here" instead of red.
func requireDemoTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "go", "curl", "sha256sum", "ss"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("run-demo.sh needs %q on PATH: %v", tool, err)
		}
	}
}

// pickFreePort binds and releases an ephemeral port, so the test starts the demo
// on a port it verified free itself — never the script's default scratch port,
// which another process on a shared box may hold.
func pickFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind an ephemeral port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release the ephemeral port: %v", err)
	}
	return port
}

// portHolderPID reports the pid listening on port, read from `ss -tlnp` — the
// same source the demo's own guard uses, but consulted independently here.
func portHolderPID(t *testing.T, port int) string {
	t.Helper()
	out, err := exec.Command("ss", "-tlnp").Output()
	if err != nil {
		t.Fatalf("ss -tlnp: %v", err)
	}
	return parsePortHolder(string(out), port)
}

// parsePortHolder returns the pid holding :port in `ss -tlnp` output, or "" when
// nothing listens on it (or the holder's pid is not visible to this user).
func parsePortHolder(ssOut string, port int) string {
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(ssOut, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != "LISTEN" || !strings.HasSuffix(f[3], suffix) {
			continue
		}
		if m := ssPortPID.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		return ""
	}
	return ""
}

// demoEnv is the environment the script runs under: the ambient one minus every
// CR_*/CRIER_*/DEMO_* setting (so an inherited CR_RATE_LIMIT_PER_MINUTE=0 or
// CRIER_PORT cannot silently change what the run proves) plus the test's own.
func demoEnv(port int, transcript string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "CR_") || strings.HasPrefix(key, "CRIER_") || strings.HasPrefix(key, "DEMO_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"DEMO_PORT="+strconv.Itoa(port),
		// The live KEEPALIVE wait costs ~30s (the server's own interval); the
		// frame-classification rule is proven by the in-process tests instead.
		"DEMO_KEEPALIVE_WAIT=0",
		"DEMO_TRANSCRIPT="+transcript,
	)
}

// demoResult is what a run of the script left behind.
type demoResult struct {
	code       int
	output     string // combined stdout+stderr
	transcript string // the tee'd transcript, "" when the run never created one
}

// runShippedDemo executes the shipped command (run-demo.sh). onRunning, when non-nil, is called
// once the script has reported its relay healthy and is still running, so the
// caller can prove ownership of the port at that moment.
func runShippedDemo(t *testing.T, port int, transcript string, onRunning func(pid string)) demoResult {
	t.Helper()

	outPath := filepath.Join(t.TempDir(), "run-demo.out")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create %s: %v", outPath, err)
	}
	defer out.Close()

	ctx, cancel := context.WithTimeout(context.Background(), demoRunBudget)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", demoScript)
	cmd.Dir = "."
	cmd.Env = demoEnv(port, transcript)
	cmd.Stdout = out
	cmd.Stderr = out
	// The script starts a relay, two mesh peers and a subscriber; killing only
	// the shell on a hang guard would orphan them, so put the run in its own
	// process group and signal the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start `bash %s`: %v", demoScript, err)
	}

	if onRunning != nil {
		pid := waitForDemoRelayPID(t, transcript)
		onRunning(pid)
	}

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("`bash %s` did not finish within %s (hang guard) — see %s", demoScript, demoRunBudget, outPath)
	}

	code := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("`bash %s`: %v", demoScript, waitErr)
		}
		code = exitErr.ExitCode()
	}

	body, readErr := os.ReadFile(outPath)
	if readErr != nil {
		t.Fatalf("read %s: %v", outPath, readErr)
	}
	tsp := ""
	if b, err := os.ReadFile(transcript); err == nil {
		tsp = string(b)
	}
	return demoResult{code: code, output: string(body), transcript: tsp}
}

// waitForDemoRelayPID blocks until the transcript carries the readiness line the
// script writes only after its relay answered /health — the moment the
// ownership question ("who holds the port?") is answerable. The bound is a hang
// guard; the signal is the script's real progress.
func waitForDemoRelayPID(t *testing.T, transcript string) string {
	t.Helper()
	deadline := time.Now().Add(demoReadyBudget)
	for {
		if body, err := os.ReadFile(transcript); err == nil {
			if m := demoServerPID.FindSubmatch(body); m != nil {
				return string(m[1])
			}
		}
		if time.Now().After(deadline) {
			body, _ := os.ReadFile(transcript)
			t.Fatalf("the demo never reported its relay as healthy within %s — transcript so far:\n%s", demoReadyBudget, body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunDemoEndToEndOnAScratchPort runs the shipped command for real and holds
// it to the CR-GAP-050 acceptance criteria: no external install, the connected
// agent listed by /mesh/peers while its socket is open, the relay event received
// on the exact topic, and a run that measured the server IT started (the pid
// holding the test's own scratch port is the pid the script started).
func TestRunDemoEndToEndOnAScratchPort(t *testing.T) {
	if testing.Short() {
		t.Skip("run-demo.sh builds the server and drives four live clients")
	}
	requireDemoTools(t)

	port := pickFreePort(t)
	if holder := portHolderPID(t, port); holder != "" {
		t.Fatalf("premise broken: :%d is already held by pid %q", port, holder)
	}

	transcript := filepath.Join(t.TempDir(), "transcript.md")
	res := runShippedDemo(t, port, transcript, func(pid string) {
		// Ownership, proven by the TEST (not by the script's own words): the
		// process holding the scratch port must be the pid the script started,
		// and that pid must be the demo's own relay binary.
		if holder := portHolderPID(t, port); holder != pid {
			t.Errorf("pid holding :%d is %q, want the demo's own relay pid %q — the run measured a server it did not start", port, holder, pid)
		}
		cmdline, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if err != nil {
			t.Errorf("read /proc/%s/cmdline: %v", pid, err)
		} else if !strings.Contains(string(cmdline), "crier") {
			t.Errorf("/proc/%s/cmdline = %q, want the demo's relay binary (built into its own mktemp dir)", pid, string(cmdline))
		}
	})

	if res.code != 0 {
		t.Fatalf("run-demo.sh exited %d, want 0.\n--- output ---\n%s\n--- transcript ---\n%s", res.code, res.output, res.transcript)
	}
	if res.transcript == "" {
		t.Fatalf("run-demo.sh exited 0 but wrote no transcript at %s — output:\n%s", transcript, res.output)
	}

	// The two CR-GAP-050 criteria, read off the run's own transcript.
	if !strings.Contains(res.transcript, `"count":2`) || !strings.Contains(res.transcript, `"agent_id":"demo-agent-a"`) {
		t.Errorf("the transcript never shows the connected agent in /mesh/peers:\n%s", res.transcript)
	}
	if !strings.Contains(res.transcript, `EVENT {"topic":"demo",`) || !strings.Contains(res.transcript, "hello from run-demo") {
		t.Errorf("the transcript never shows the subscriber receiving the published event on the exact topic:\n%s", res.transcript)
	}
	if !strings.Contains(res.transcript, "PASS: exactly one event, on the exact subscribed topic demo") {
		t.Errorf("the transcript does not assert a single exact-topic delivery:\n%s", res.transcript)
	}
	if !strings.Contains(res.transcript, "POST /relay/publish without X-Agent-ID -> HTTP 401") {
		t.Errorf("the transcript does not prove the X-Agent-ID requirement negatively:\n%s", res.transcript)
	}
	if !strings.Contains(res.transcript, "is held by pid ") {
		t.Errorf("the transcript does not show the port-ownership guard passing:\n%s", res.transcript)
	}
	if !strings.Contains(res.transcript, "DEMO PASS") {
		t.Errorf("the transcript does not end in DEMO PASS:\n%s", res.transcript)
	}
	if strings.Contains(res.transcript, "FAIL:") {
		t.Errorf("the transcript carries a FAIL: line although the script exited 0:\n%s", res.transcript)
	}
}

// TestRunDemoRefusesAForeignServerOnItsPort is the negative control for the
// isolation criterion. The squatter is not an inert listener: it answers
// /health with 200 and /mesh/peers with an EMPTY list, so a demo missing its
// port guards would run its whole script against it and could report success for
// a binary it never built. The run must abort instead — loudly, before it builds
// or starts anything.
func TestRunDemoRefusesAForeignServerOnItsPort(t *testing.T) {
	if testing.Short() {
		t.Skip("this control runs the shipped script")
	}
	requireDemoTools(t)

	port := pickFreePort(t)
	foreign := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/mesh/peers":
			_, _ = w.Write([]byte(`{"count":0,"peers":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("start the foreign server on :%d: %v", port, err)
	}
	go func() { _ = foreign.Serve(ln) }()
	defer func() { _ = foreign.Close() }()

	// Premise: the foreign server really holds the port and really answers the
	// two probes the demo would otherwise trust.
	if holder := portHolderPID(t, port); holder != strconv.Itoa(os.Getpid()) {
		t.Fatalf("premise broken: :%d is held by pid %q, want this test process %d", port, holder, os.Getpid())
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/health")
	if err != nil {
		t.Fatalf("premise broken: the foreign server does not answer /health: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("premise broken: the foreign /health answered %d, want 200", resp.StatusCode)
	}

	transcript := filepath.Join(t.TempDir(), "transcript.md")
	res := runShippedDemo(t, port, transcript, nil)

	if res.code == 0 {
		t.Fatalf("run-demo.sh exited 0 against a FOREIGN server holding its port — the phantom green this guard exists for.\n--- output ---\n%s\n--- transcript ---\n%s", res.output, res.transcript)
	}
	if !strings.Contains(res.output, ":"+strconv.Itoa(port)+" is already in use") {
		t.Errorf("the refusal does not name the occupied port :%d:\n%s", port, res.output)
	}
	if !strings.Contains(res.output, "holder pid") {
		t.Errorf("the refusal does not name the holder's pid:\n%s", res.output)
	}
	if strings.Contains(res.output, "DEMO PASS") {
		t.Errorf("the refused run still printed DEMO PASS:\n%s", res.output)
	}
	if res.transcript != "" {
		t.Errorf("the run created a transcript (%d bytes) although it should have aborted before starting:\n%s", len(res.transcript), res.transcript)
	}
}

// TestEveryExampleHarnessSourcesThePortGuardLib pins the README claim ("The three
// guards live in scripts/lib/port-guard.sh") for all three example harnesses: a
// locally re-implemented guard is a guard that drifts.
func TestEveryExampleHarnessSourcesThePortGuardLib(t *testing.T) {
	lib := filepath.Join("..", "..", "scripts", "lib", "port-guard.sh")
	if _, err := os.Stat(lib); err != nil {
		t.Fatalf("the shared port-guard library is missing: %v", err)
	}
	for _, harness := range []string{"ws-mesh-demo", "federation-demo", "hermes-gateway-demo"} {
		path := filepath.Join("..", harness, "run-demo.sh")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(b), `scripts/lib/port-guard.sh`) {
			t.Errorf("%s does not source scripts/lib/port-guard.sh, so its port guards are its own copy", path)
		}
	}
}

// TestRunDemoUsesTheSharedGuardsAndProvesOwnership pins the three guards as
// source invariants, in the order they must run: refuse on an occupied port
// BEFORE building or starting anything, prove the holder of the port is our own
// relay pid AFTER it answered /health, and require an empty peer list.
func TestRunDemoUsesTheSharedGuardsAndProvesOwnership(t *testing.T) {
	src := runDemoScript(t)

	if !strings.Contains(src, `. "$REPO_ROOT/scripts/lib/port-guard.sh"`) {
		t.Error("run-demo.sh must source the shared port-guard library")
	}

	refuseIdx := strings.Index(src, `require_free_port "$DEMO_PORT"`)
	if refuseIdx < 0 {
		t.Fatal("run-demo.sh must call require_free_port on $DEMO_PORT")
	}
	buildIdx := strings.Index(src, `go build -o "$WORKDIR/crier" ./cmd/server`)
	if buildIdx < 0 {
		t.Fatal("run-demo.sh has no server build step")
	}
	startIdx := strings.Index(src, `CRIER_PORT="$DEMO_PORT" CR_LOG_LEVEL=warn "$WORKDIR/crier"`)
	if startIdx < 0 {
		t.Fatal("run-demo.sh must start its own relay with CRIER_PORT=$DEMO_PORT")
	}
	ownIdx := strings.Index(src, `assert_port_owned "$DEMO_PORT" "$SERVER_PID" "the ws-mesh-demo relay"`)
	if ownIdx < 0 {
		t.Fatal("run-demo.sh must assert that the pid holding $DEMO_PORT is the relay it started")
	}
	if refuseIdx > buildIdx || refuseIdx > startIdx {
		t.Error("the port refusal must run BEFORE the server is built and started")
	}
	if ownIdx < startIdx {
		t.Error("assert_port_owned must run after the relay was started (it proves the port's holder is that pid)")
	}
	if !strings.Contains(src, `kill -0 "$SERVER_PID" 2>/dev/null`) {
		t.Error("run-demo.sh must fail fast when its own relay process exits during startup")
	}
	if !strings.Contains(src, `CR_RATE_LIMIT_PER_MINUTE=100`) {
		t.Error("run-demo.sh must pin CR_RATE_LIMIT_PER_MINUTE for its own relay, or the X-Agent-ID 401 it asserts could be inherited away")
	}
	if !strings.Contains(src, `grep -q '"count":0'`) {
		t.Error("run-demo.sh must assert the relay it talks to starts with an EMPTY peer list")
	}
}

// TestRunDemoProvesThePublishHeaderAndExactTopic pins the relay-leg assertions as
// source invariants: the 401 without X-Agent-ID, the cross-topic publish, and the
// single-EVENT requirement. Each is a way the relay leg could pass vacuously.
func TestRunDemoProvesThePublishHeaderAndExactTopic(t *testing.T) {
	src := runDemoScript(t)

	noHeaderIdx := strings.Index(src, `PUB_NO_HEADER=`)
	matchingIdx := strings.Index(src, `PUB=$(curl`)
	otherIdx := strings.Index(src, `PUB_OTHER=`)
	for name, idx := range map[string]int{"PUB_NO_HEADER": noHeaderIdx, "PUB_OTHER": otherIdx, "PUB": matchingIdx} {
		if idx < 0 {
			t.Fatalf("run-demo.sh is missing the %s publish probe", name)
		}
	}
	// The header requirement must be checked, in the exact direction it is
	// documented (401 without it), and the cross-topic publish must happen BEFORE
	// the event under test so the subscriber (-once) would fail on a fan-out.
	if !strings.Contains(src, `[ "$PUB_NO_HEADER" = "401" ]`) {
		t.Error("run-demo.sh must require 401 for a publish without X-Agent-ID")
	}
	if otherIdx > matchingIdx {
		t.Error("the cross-topic publish must be issued BEFORE the event under test")
	}
	if !strings.Contains(src, `"topic":"demo-other"`) {
		t.Error("run-demo.sh must publish to a topic the subscriber did not subscribe to")
	}
	for _, want := range []string{
		`EVENT_LINES=$(grep -c '^EVENT ' "$WORKDIR/sub.out"`,
		`[ "$EVENT_LINES" = "1" ]`,
		`grep -q '"topic":"demo"' "$WORKDIR/sub.out"`,
		`-H 'X-Agent-ID: demo-publisher'`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("run-demo.sh is missing the relay-leg assertion %q", want)
		}
	}
}

// TestDocsPointAtTheShippedNoInstallCommand keeps the docs honest about how a
// cold reader runs this: the top-level README and the integration guide must both
// name the repo-shipped command, and the guide must say plainly that no external
// WebSocket client is required (the premise of CR-GAP-050 was a README whose
// "try it" path was `websocat` / `npx wscat`).
func TestDocsPointAtTheShippedNoInstallCommand(t *testing.T) {
	readDoc := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}

	const command = "examples/ws-mesh-demo/run-demo.sh"

	readme := readDoc("README.md")
	if !strings.Contains(readme, command) {
		t.Errorf("README.md never names the runnable command %q", command)
	}
	if !strings.Contains(readme, "No external WebSocket client is needed") {
		t.Error("README.md must say the mesh/relay demo needs no external WebSocket client")
	}

	guide := readDoc("docs/integration-guide.md")
	if n := strings.Count(guide, command); n < 2 {
		t.Errorf("docs/integration-guide.md names %q %d time(s), want the relay AND mesh sections to point at it", command, n)
	}
	if !strings.Contains(guide, "no external WebSocket client is required") {
		t.Error("docs/integration-guide.md must state that no external WebSocket client is required")
	}
	if !strings.Contains(guide, "Zero-install path") {
		t.Error("docs/integration-guide.md must lead its WebSocket sections with the zero-install path")
	}

	demoReadme := readDoc(filepath.Join("examples", "ws-mesh-demo", "README.md"))
	for _, want := range []string{
		command, // run from the repo root
		"no `websocat`, no `wscat`",
		"X-Agent-ID",
		"require_free_port",
		"assert_port_owned",
		"DEMO_PORT",
		"DEMO PASS",
	} {
		if !strings.Contains(demoReadme, want) {
			t.Errorf("examples/ws-mesh-demo/README.md must document %q", want)
		}
	}
}

// TestDemoEnvDropsInheritedOverrides is the premise check for the runs above: an
// inherited CR_RATE_LIMIT_PER_MINUTE=0 (or CRIER_PORT, or DEMO_PORT) would change
// what the demo proves, so the harness must not pass any of them through.
func TestDemoEnvDropsInheritedOverrides(t *testing.T) {
	t.Setenv("CR_RATE_LIMIT_PER_MINUTE", "0")
	t.Setenv("CRIER_PORT", "9999")
	t.Setenv("DEMO_PORT", "1234")

	env := demoEnv(4242, "/tmp/t.md")
	var buf bytes.Buffer
	for _, kv := range env {
		buf.WriteString(kv)
		buf.WriteString("\n")
	}
	joined := buf.String()
	for _, forbidden := range []string{"CR_RATE_LIMIT_PER_MINUTE=0", "CRIER_PORT=9999", "DEMO_PORT=1234"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("demoEnv passed %q through to the script", forbidden)
		}
	}
	if !strings.Contains(joined, fmt.Sprintf("DEMO_PORT=%d", 4242)) {
		t.Error("demoEnv must set the port under test")
	}
}
