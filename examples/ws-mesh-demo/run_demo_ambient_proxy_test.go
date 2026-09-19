package main

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// QA-CRIER-21 — loopback demo traffic must never ride an ambient proxy, and a
// demo that cannot start must fail fast with a readable error instead of
// hanging.
//
// The failure these arms exist for (measured on a bunker box, reproduced twice):
// with HTTP_PROXY/HTTPS_PROXY/ALL_PROXY pointing at a dead loopback port,
// `go test ./...` hung until the 120s cap (rc=124). The demo script itself had
// already FAILED — `curl -sfS "$BASE/health"` was sent to the dead proxy, so
// [2/10] aborted with "relay never became healthy" — but the Go E2E arm waited
// for a readiness line that could never be written and only ever surfaced as
// `panic: test timed out`. curl has no built-in loopback exemption; Go's
// ProxyFromEnvironment does, which is why only the demo's own shell leg hung.
//
// Three properties are pinned here, and each one is asserted in both directions
// (the fixture must be shown to be hostile/incapable, so a green cannot be
// vacuous):
//
//   - TestDemoIsReachableUnderADeadAmbientProxy: the SHIPPED script, run with
//     those three proxy variables pointing at a dead port, reaches DEMO PASS —
//     while a bare curl under the very same environment cannot reach a loopback
//     listener this test owns.
//   - TestDemoBoundedWaitFailsFast: the bounded readiness wait reports a NAMED
//     failure, carrying the child's own output tail, within seconds — for a
//     child that exits immediately and for one that stalls forever. Both run
//     through the real harness (runDemoCommand) against a sandboxed stub script,
//     in a child test binary, so the arm asserts on the failure text this code
//     really produces rather than on a mock of it.
//   - TestEveryExampleHarnessKeepsLoopbackOffTheProxy: every shipped runner that
//     talks to loopback calls the shared guard BEFORE its first curl, and the
//     ws-mesh runner pins each of its loopback probes with --noproxy '*'.
//   - TestProxyForDemoExemptsLoopbackOnly: the demo client's own dials exempt
//     loopback structurally (demoDialer carries the rule) while genuinely
//     external hosts keep the standard proxy lookup.

// demoWaitHelperMode selects an arm of TestDemoBoundedWaitFailsFast. It is only
// set in the environment of the CHILD test binary that test spawns.
const demoWaitHelperMode = "DEMO_WAIT_ARM_MODE"

// deadAmbientProxyEnv is the ambient environment with every proxy spelling
// REMOVED (an inherited no_proxy=127.0.0.1 would quietly exempt loopback and
// turn every arm below into a no-op) and the three proxy variables pointed at a
// loopback port where nothing listens, so any request that rides the proxy fails
// at once with ECONNREFUSED.
func deadAmbientProxyEnv(deadPort int) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range demoEnvStripped() {
		key, _, _ := strings.Cut(kv, "=")
		switch strings.ToLower(key) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			continue
		}
		env = append(env, kv)
	}
	dead := "http://127.0.0.1:" + strconv.Itoa(deadPort)
	return append(env, "HTTP_PROXY="+dead, "HTTPS_PROXY="+dead, "ALL_PROXY="+dead)
}

// envValue reads one key out of an env slice.
func envValue(env []string, key string) (string, bool) {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == key {
			return v, true
		}
	}
	return "", false
}

// TestDemoIsReachableUnderADeadAmbientProxy runs the SHIPPED demo with the three
// proxy variables pointing at a dead loopback port. The premise is asserted
// first: a bare curl to a loopback listener this test owns must FAIL under that
// very environment — otherwise nothing here would be proven, because a curl that
// ignored HTTP_PROXY would pass this arm with or without the fix.
func TestDemoIsReachableUnderADeadAmbientProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("this arm runs the shipped script end to end")
	}
	requireDemoTools(t)

	dead := pickFreePort(t)
	env := deadAmbientProxyEnv(dead)

	// PREMISE 1: the environment really is hostile to a proxied loopback request.
	ping := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind a loopback listener: %v", err)
	}
	go func() { _ = ping.Serve(ln) }()
	defer func() { _ = ping.Close() }()
	pingURL := "http://" + ln.Addr().String() + "/"

	bare := exec.Command("curl", "-sfS", "-m", "5", "-o", "/dev/null", pingURL)
	bare.Env = env
	if out, err := bare.CombinedOutput(); err == nil {
		t.Fatalf("premise broken: a bare curl reached the loopback listener %s while HTTP_PROXY pointed at the dead port :%d — this arm cannot prove anything (output: %s)",
			pingURL, dead, out)
	}

	// PREMISE 2: the arm's environment carries the dead proxy in all three
	// spellings and no no_proxy that could exempt loopback.
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		v, ok := envValue(env, key)
		if !ok || !strings.Contains(v, strconv.Itoa(dead)) {
			t.Fatalf("premise broken: %s is %q (present=%v), want the dead proxy :%d", key, v, ok, dead)
		}
	}
	if v, ok := envValue(env, "no_proxy"); ok {
		t.Fatalf("premise broken: no_proxy=%q leaked into the arm's environment", v)
	}

	// THE ARM: the shipped script, same hostile environment, must run to its own
	// DEMO PASS — every one of its probes targets 127.0.0.1 and must go direct.
	port := pickFreePort(t)
	transcript := filepath.Join(t.TempDir(), "transcript.md")
	runEnv := append(env,
		"DEMO_PORT="+strconv.Itoa(port),
		"DEMO_KEEPALIVE_WAIT=0", // the live keepalive wait costs the server's 30s interval
		"DEMO_TRANSCRIPT="+transcript,
	)
	res := runShippedDemoEnv(t, runEnv, transcript, nil)

	if res.code != 0 {
		t.Fatalf("run-demo.sh exited %d under HTTP_PROXY/HTTPS_PROXY/ALL_PROXY=http://127.0.0.1:%d, want 0 — loopback traffic is riding the ambient proxy.\n--- output ---\n%s\n--- transcript ---\n%s",
			res.code, dead, res.output, res.transcript)
	}
	for _, want := range []string{
		"relay healthy (our pid",
		// The fix's own line, with the loopback names it merged in. Asserting the
		// VALUE (not just that some no_proxy exists) is what ties the green to
		// the guard rather than to an environment that happened to be friendly.
		"- loopback proxy bypass: no_proxy=127.0.0.1,localhost,::1 ",
		"DEMO PASS",
	} {
		if !strings.Contains(res.transcript, want) {
			t.Errorf("the transcript never shows %q:\n%s", want, res.transcript)
		}
	}
	if strings.Contains(res.transcript, "FAIL:") {
		t.Errorf("the transcript carries a FAIL: line although the script exited 0:\n%s", res.transcript)
	}
}

// TestDemoBoundedWaitFailsFast proves the bounded readiness wait fails FAST, with
// a named error and the child's own output tail, when the demo genuinely cannot
// start. Each arm spawns THIS test binary as a child, which drives a sandboxed
// stub script through runDemoCommand — the same harness the shipped demo uses —
// and asserts on the child's failure text.
//
// Neither arm may end in the go-test timeout alarm: that alarm is exactly what
// the bug produced (a hang with no cause in it), and it is not a pass/fail
// condition this package may depend on.
func TestDemoBoundedWaitFailsFast(t *testing.T) {
	arms := []struct {
		mode        string
		wantNamed   string
		wantStub    string
		maxDuration time.Duration
	}{
		{
			mode:        "child-exits",
			wantNamed:   demoChildExitedBeforeReady,
			wantStub:    "stub-run-demo: relay never became healthy",
			maxDuration: 30 * time.Second,
		},
		{
			mode:        "child-stalls",
			wantNamed:   demoReadinessDeadlineMissed,
			wantStub:    "stub-run-demo: started and never wrote a readiness line",
			maxDuration: 45 * time.Second,
		},
	}

	for _, arm := range arms {
		t.Run(arm.mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDemoWaitHelperArm$", "-test.v")
			cmd.Env = append(demoEnvStripped(), demoWaitHelperMode+"="+arm.mode)

			started := time.Now()
			out, err := cmd.CombinedOutput()
			elapsed := time.Since(started)
			text := string(out)

			if err == nil {
				t.Fatalf("the helper arm exited 0 although the sandboxed fixture cannot report readiness — the wait cannot fail, so it is not evidence:\n%s", text)
			}
			if elapsed > arm.maxDuration {
				t.Errorf("the arm took %s, want under %s — it must fail on its own verdict, not by running out the readiness budget", elapsed, arm.maxDuration)
			}
			if !strings.Contains(text, arm.wantNamed) {
				t.Errorf("the failure does not carry the named verdict %q:\n%s", arm.wantNamed, text)
			}
			if !strings.Contains(text, arm.wantStub) {
				t.Errorf("the failure does not carry the child script's own output tail (%q), so the cause is not in it:\n%s", arm.wantStub, text)
			}
			if strings.Contains(text, "tail of ") && !strings.Contains(text, "run-demo.out") {
				t.Errorf("the failure names a tail but not the child's captured output file:\n%s", text)
			}
			for _, alarm := range []string{"test timed out after", "panic: test timed out"} {
				if strings.Contains(text, alarm) {
					t.Errorf("the arm ended in the go-test alarm (%q), not in the bounded wait's own verdict:\n%s", alarm, text)
				}
			}
		})
	}
}

// TestDemoWaitHelperArm is the child half of TestDemoBoundedWaitFailsFast. It is
// inert without demoWaitHelperMode in the environment, so `go test` on its own
// never runs a fixture: the arms that matter run it as a subprocess.
func TestDemoWaitHelperArm(t *testing.T) {
	mode := os.Getenv(demoWaitHelperMode)
	if mode == "" {
		t.Skip("child half of TestDemoBoundedWaitFailsFast — it runs in a subprocess with " + demoWaitHelperMode + " set")
	}

	stub := ""
	switch mode {
	case "child-exits":
		// The dead-proxy shape: the script gives up early and says why.
		stub = "#!/usr/bin/env bash\n" +
			"echo 'stub-run-demo: relay never became healthy at http://127.0.0.1:18961' >&2\n" +
			"exit 3\n"
	case "child-stalls":
		// The classic hang: the child is alive, writes nothing that answers the
		// readiness question, and would outlive any caller. The budget is the
		// only thing that can end this arm, so it is shortened here (the var is
		// package state; the shipped value is 120s).
		demoReadyBudget = 3 * time.Second
		stub = "#!/usr/bin/env bash\n" +
			"echo 'stub-run-demo: started and never wrote a readiness line'\n" +
			"exec sleep 300\n"
	default:
		t.Fatalf("unknown arm mode %q", mode)
	}

	stubPath := filepath.Join(t.TempDir(), "stub-run-demo.sh")
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatalf("write the stub script: %v", err)
	}

	transcript := filepath.Join(t.TempDir(), "transcript.md")
	// This MUST fail, from inside the bounded wait, which exits the test here.
	_ = runDemoCommand(t, stubPath, demoEnvStripped(), transcript, func(pid string) {
		t.Fatalf("premise broken: the wait reported relay pid %q although the stub never writes a readiness line", pid)
	})
	t.Fatalf("the sandboxed fixture (%s) did not make the bounded wait fail — the wait cannot fail, so it is not evidence", mode)
}

// demoCurlInvocation matches a line in a harness that really INVOKES curl (a
// flag right after the command). Prose mentions curl ("Requirements: … curl …")
// and comments do too, so neither may count when the call ORDER is asserted.
var demoCurlInvocation = regexp.MustCompile(`(?m)^[ \t]*[^#\s][^\n]*\bcurl\s+-`)

// TestEveryExampleHarnessKeepsLoopbackOffTheProxy pins the wiring for every
// shipped runner that starts a service on loopback: it calls the SHARED guard
// (a local copy of a guard is a guard that drifts) BEFORE its first curl, and
// the ws-mesh runner — whose every curl targets $BASE, loopback by construction
// — additionally pins each probe with --noproxy '*'. Dropping either half is a
// regression to the hang this row is about.
func TestEveryExampleHarnessKeepsLoopbackOffTheProxy(t *testing.T) {
	harnesses := []string{
		"ws-mesh-demo/run-demo.sh",
		"federation-demo/run-demo.sh",
		"hermes-gateway-demo/run-demo.sh",
		"llm-mesh/run-demo.sh",
		"llm-mesh/mesh/run-demo.sh",
	}
	for _, rel := range harnesses {
		t.Run(rel, func(t *testing.T) {
			path := filepath.Join("..", filepath.FromSlash(rel))
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			src := string(b)

			if !strings.Contains(src, `scripts/lib/port-guard.sh`) {
				t.Errorf("%s does not source the shared port-guard library", rel)
			}
			call := strings.Index(src, "\nguard_loopback_off_proxy")
			if call < 0 {
				t.Fatalf("%s never calls guard_loopback_off_proxy: a loopback poll would be sent to an ambient proxy (QA-CRIER-21)", rel)
			}
			if first := demoCurlInvocation.FindStringIndex(src); first != nil && first[0] < call {
				t.Errorf("%s calls guard_loopback_off_proxy AFTER its first curl invocation (offset %d) — the exemption must be in place before any probe", rel, first[0])
			}
			if strings.Contains(src, "\nunset HTTP_PROXY") || strings.Contains(src, "\nunset http_proxy") {
				t.Errorf("%s unsets the proxy instead of exempting loopback: a genuinely external host must still honour it", rel)
			}
		})
	}

	// The ws-mesh runner's own probes are loopback by construction ($BASE is
	// built from 127.0.0.1 and the selected port), so each one is pinned per
	// call as well as covered by the exported exemption.
	b, err := os.ReadFile("run-demo.sh")
	if err != nil {
		t.Fatalf("read run-demo.sh: %v", err)
	}
	lines := strings.Split(string(b), "\n")
	probes := 0
	for i, line := range lines {
		if !demoCurlInvocation.MatchString(line) {
			continue
		}
		probes++
		if !strings.Contains(line, "--noproxy '*'") {
			t.Errorf("run-demo.sh line %d invokes curl without --noproxy '*': %s", i+1, strings.TrimSpace(line))
		}
	}
	if probes < 5 {
		t.Errorf("found only %d curl invocations in run-demo.sh — this invariant is not reading the script", probes)
	}
}

// TestProxyForDemoExemptsLoopbackOnly pins the Go half of the same rule: the demo
// client's own connections must not consult the proxy environment for a loopback
// host, while every other host keeps the standard lookup.
//
// Scope, measured and stated plainly: on this toolchain (go1.26)
// http.ProxyFromEnvironment ALREADY exempts 127.0.0.1/localhost/::1, so the
// loopback rows pin the demo's own contract rather than catching a live
// regression — what they catch is a toolchain or URL shape that does not, which
// is exactly why the rule is installed on the dialer instead of being left to
// the environment. The rows that DO catch a regression are proven by mutation
// (both measured; this test failed on each): returning nil for every host fails
// the two external rows, and dialing through websocket.DefaultDialer again fails
// the wiring rows at the bottom.
func TestProxyForDemoExemptsLoopbackOnly(t *testing.T) {
	const dead = "http://127.0.0.1:9"
	t.Setenv("HTTP_PROXY", dead)
	t.Setenv("HTTPS_PROXY", dead)
	t.Setenv("ALL_PROXY", dead)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	loopback := []string{
		"http://127.0.0.1:18961/health",
		"http://127.0.0.1:18961/mesh/peers",
		"http://localhost:18961/health",
		"http://[::1]:18961/health",
		"ws://127.0.0.1:18961/mesh/connect/demo-agent-a",
	}
	for _, raw := range loopback {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		got, err := proxyForDemo(&http.Request{URL: u})
		if err != nil {
			t.Errorf("proxyForDemo(%q): %v", raw, err)
			continue
		}
		if got != nil {
			t.Errorf("proxyForDemo(%q) = %v, want nil — a loopback dial must never ride the ambient proxy", raw, got)
		}
		if !isLoopbackHost(u.Hostname()) {
			t.Errorf("isLoopbackHost(%q) = false, want true", u.Hostname())
		}
	}

	// Gorilla rewrites ws://→http:// (client.go DialContext, before it consults
	// the dialer's Proxy func), so these are the shapes proxyForDemo really sees:
	// a loopback ws dial arrives as http://127.0.0.1:<port>/…, and that is
	// exactly the dial that must not be proxied.
	for _, raw := range []string{"http://example.com/x", "http://192.0.2.10:1234/relay"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		got, err := proxyForDemo(&http.Request{URL: u})
		if err != nil {
			t.Errorf("proxyForDemo(%q): %v", raw, err)
			continue
		}
		if got == nil {
			t.Errorf("proxyForDemo(%q) = nil, want the ambient proxy %s — proxying must stay intact for genuinely external hosts", raw, dead)
		}
	}

	// The dialer every demo subcommand uses must be the one carrying that rule:
	// a subcommand left on websocket.DefaultDialer would silently go back to the
	// environment for a loopback dial on a toolchain that proxies loopback.
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if n := strings.Count(string(src), "websocket.DefaultDialer.Dial("); n != 0 {
		t.Errorf("main.go still dials through websocket.DefaultDialer in %d place(s) — those dials consult the proxy environment", n)
	}
	if n := strings.Count(string(src), "demoDialer.Dial("); n < 2 {
		t.Errorf("main.go dials through demoDialer %d time(s), want the mesh and subscribe paths both", n)
	}
	if demoDialer.Proxy == nil {
		t.Error("demoDialer.Proxy is nil: the loopback rule is not installed on the dialer")
	}
}
