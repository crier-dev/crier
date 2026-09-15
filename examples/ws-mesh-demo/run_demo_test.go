package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// These tests pin the DF-CRIER-152 regressions in the demo's shell driver and
// docs, so the failure mode cannot come back silently:
//
//   * every demo client must be spawned with `-url "$BASE"` — without it the
//     client falls back to the compiled-in default and a DEMO_PORT run measures
//     whatever else owns that port (a docker-published crier was live on :18767);
//   * the relay port must be pre-checked and the answering server proven to be
//     ours before any assertion is made against it;
//   * the transcript must be written OUTSIDE the repo (it used to dirty the
//     tracked examples dir);
//   * the two peers must be HTTP-registered BEFORE their mesh connections.

func runDemoScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("run-demo.sh")
	if err != nil {
		t.Fatalf("read run-demo.sh: %v", err)
	}
	return string(b)
}

var (
	demoPortDefault = regexp.MustCompile(`(?m)^DEMO_PORT="\$\{DEMO_PORT:-([0-9]+)\}"`)
	clientCall      = regexp.MustCompile(`(?m)^[ \t]*"\$WORKDIR/ws-mesh-demo"[ \t]+(.*)$`)
)

// TestDefaultBaseURLUsesRunDemoScratchPort keeps the client's compiled-in
// default and the script's default in lockstep, on a scratch port that does not
// collide with the fleet's long-lived listeners.
func TestDefaultBaseURLUsesRunDemoScratchPort(t *testing.T) {
	src := runDemoScript(t)
	m := demoPortDefault.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("run-demo.sh: `DEMO_PORT=\"${DEMO_PORT:-<port>}\"` default not found")
	}
	want := "http://127.0.0.1:" + m[1]
	if defaultBaseURL != want {
		t.Errorf("defaultBaseURL = %q, run-demo.sh default port implies %q", defaultBaseURL, want)
	}
	for _, colliding := range []string{":8767", ":18767"} {
		if strings.HasSuffix(defaultBaseURL, colliding) {
			t.Errorf("defaultBaseURL %q collides with long-lived port %s", defaultBaseURL, colliding)
		}
	}
}

// TestEveryDemoClientInvocationIsPinnedToTheDemoPort is the core regression: a
// client spawned without -url ignores DEMO_PORT entirely.
func TestEveryDemoClientInvocationIsPinnedToTheDemoPort(t *testing.T) {
	src := runDemoScript(t)
	matches := clientCall.FindAllStringSubmatch(src, -1)
	if len(matches) < 3 {
		t.Fatalf("found %d demo-client invocations in run-demo.sh, want >= 3 (subscriber + 2 peers)", len(matches))
	}
	for _, m := range matches {
		args := strings.TrimSpace(m[1])
		if !strings.HasPrefix(args, `-url "$BASE"`) {
			t.Errorf("demo client invoked without -url \"$BASE\": %q", args)
		}
	}
}

// TestTranscriptIsWrittenOutsideTheRepo covers the "dirtying git status" half of
// the ticket: the transcript is mktemp-derived under ${TMPDIR:-/tmp}, its path is
// printed, and nothing writes TRANSCRIPT-<date>.md back into examples/.
func TestTranscriptIsWrittenOutsideTheRepo(t *testing.T) {
	src := runDemoScript(t)

	if !strings.Contains(src, `TRANSCRIPT_DIR="${TMPDIR:-/tmp}"`) {
		t.Error(`run-demo.sh must derive the transcript dir from ${TMPDIR:-/tmp}`)
	}
	if !strings.Contains(src, `mktemp "$TRANSCRIPT_DIR/ws-mesh-demo-TRANSCRIPT-`) {
		t.Error("run-demo.sh must create the transcript with mktemp under $TRANSCRIPT_DIR")
	}
	for _, bad := range []string{`"$DEMO_DIR/TRANSCRIPT`, `$DEMO_DIR/TRANSCRIPT-`} {
		if strings.Contains(src, bad) {
			t.Errorf("run-demo.sh still writes a transcript inside the repo (%s)", bad)
		}
	}
	if !strings.Contains(src, "transcript: ${TRANSCRIPT}") {
		t.Error("run-demo.sh must print the transcript location at the end of the run")
	}
}

// TestPeersAreHTTPRegisteredBeforeMeshConnect pins the ordering the ticket asks
// for: POST /agents (201, both ids) strictly before the mesh peer spawns.
func TestPeersAreHTTPRegisteredBeforeMeshConnect(t *testing.T) {
	src := runDemoScript(t)

	registerIdx := strings.Index(src, `-X POST "$BASE/agents"`)
	if registerIdx < 0 {
		t.Fatal(`run-demo.sh has no HTTP registration call (POST "$BASE/agents")`)
	}
	peerIdx := -1
	for _, m := range clientCall.FindAllStringSubmatchIndex(src, -1) {
		if strings.Contains(src[m[2]:m[3]], "peer -agent demo-agent-a") {
			peerIdx = m[0]
			break
		}
	}
	if peerIdx < 0 {
		t.Fatal("run-demo.sh has no `peer -agent demo-agent-a` invocation")
	}
	if registerIdx > peerIdx {
		t.Error("HTTP registration must happen BEFORE the mesh peers are spawned")
	}
	if !strings.Contains(src, "for AGENT in demo-agent-a demo-agent-b; do") {
		t.Error("run-demo.sh must register both demo-agent-a and demo-agent-b")
	}
	if !strings.Contains(src, `[ "$REG_CODE" = "201" ]`) {
		t.Error("run-demo.sh must assert HTTP 201 from POST /agents")
	}
}

// TestPortIsPreCheckedAndMeasuredServerIsOurs covers the second failure mode seen
// live: the port was owned by a foreign crier, the script measured that server,
// and the failure surfaced as a bogus "/mesh/peers count != 2".
func TestPortIsPreCheckedAndMeasuredServerIsOurs(t *testing.T) {
	src := runDemoScript(t)

	if !strings.Contains(src, "port_in_use() {") {
		t.Error("run-demo.sh must define a port_in_use probe")
	}
	portCheckIdx := strings.Index(src, `if port_in_use "$DEMO_PORT"; then`)
	if portCheckIdx < 0 {
		t.Fatal("run-demo.sh must abort when $DEMO_PORT is already in use")
	}
	serverStartIdx := strings.Index(src, `CRIER_PORT="$DEMO_PORT" CR_LOG_LEVEL=warn "$WORKDIR/crier"`)
	if serverStartIdx < 0 {
		t.Fatal("run-demo.sh must start its own relay with CRIER_PORT=$DEMO_PORT")
	}
	if portCheckIdx > serverStartIdx {
		t.Error("the port pre-check must run BEFORE the relay is started")
	}
	if !strings.Contains(src, `kill -0 "$SERVER_PID" 2>/dev/null`) {
		t.Error("run-demo.sh must fail fast when its own relay process exits during startup")
	}
	if !strings.Contains(src, `'"count":0'`) {
		t.Error("run-demo.sh must assert the relay it talks to starts with an EMPTY peer list")
	}
}

// TestUsageTextStatesTheRegistrationPrecondition documents the precondition in
// the script's own help output (the ticket's stale-claim grep).
func TestUsageTextStatesTheRegistrationPrecondition(t *testing.T) {
	src := runDemoScript(t)

	start := strings.Index(src, "usage() {")
	if start < 0 {
		t.Fatal("run-demo.sh must define usage()")
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not locate the end of usage()")
	}
	usage := src[start : start+end]

	for _, want := range []string{"POST /agents", "mesh/connect", "ws-mesh-demo-TRANSCRIPT", "DEMO_PORT", "DEMO_TRANSCRIPT"} {
		if !strings.Contains(usage, want) {
			t.Errorf("-h usage text must mention %q", want)
		}
	}
	if strings.Index(usage, "POST /agents") > strings.Index(usage, "mesh/connect") {
		t.Error("-h usage text must describe the /agents registration before the /mesh/connect peers")
	}
	// `-h` must be handled, and unknown arguments must not be ignored.
	if !strings.Contains(src, "-h|--help)") {
		t.Error("run-demo.sh must handle -h/--help")
	}
	if !strings.Contains(src, "unknown argument") {
		t.Error("run-demo.sh must reject unknown arguments instead of ignoring them")
	}
}

// TestReadmeClaimsMatchLiveBehavior keeps the docs honest: the registration step,
// the scratch port and the outside-the-repo transcript are all documented, and
// the old "transcript in this directory" claim is gone.
func TestReadmeClaimsMatchLiveBehavior(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(b)

	for _, want := range []string{"POST /agents", "mesh/connect", "18961", "ws-mesh-demo-TRANSCRIPT", "DEMO_PORT"} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md must document %q", want)
		}
	}
	for _, stale := range []string{"TRANSCRIPT-<date>.md` in this directory", "teed into `TRANSCRIPT-<date>.md` in this directory"} {
		if strings.Contains(readme, stale) {
			t.Errorf("README.md still claims the transcript lands in this directory: %q", stale)
		}
	}
}
