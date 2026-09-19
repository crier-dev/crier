package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/mesh"
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
	// The first candidate of the script's default rotation, and the fixed default
	// port the script must no longer carry (QA-CRIER-10).
	demoPortBase    = regexp.MustCompile(`(?m)^DEMO_PORT_BASE="\$\{DEMO_PORT_BASE:-([0-9]+)\}"`)
	demoPortDefault = regexp.MustCompile(`(?m)^DEMO_PORT="\$\{DEMO_PORT:-([0-9]+)\}"`)
	clientCall      = regexp.MustCompile(`(?m)^[ 	]*"\$WORKDIR/ws-mesh-demo"[ 	]+(.*)$`)
	// `port-guard: selected :<port> for the ws-mesh-demo relay — …`
	guardSelection = regexp.MustCompile(`(?m)^port-guard: selected :([0-9]+) for the ws-mesh-demo relay`)
	// A port literal in the script body outside the candidate-base definition: a
	// component that would be measured on a port nothing selected. (RE2 has no
	// lookahead, so the comment/base-definition lines are filtered in Go.)
	portLiteral = regexp.MustCompile(`(?m)^.*(1[89][0-9]{3}).*$`)
	baseLine    = regexp.MustCompile(`^\s*DEMO_PORT_BASE=`)
)

// TestDefaultBaseURLUsesRunDemoScratchPort keeps the client's compiled-in
// default and the FIRST candidate of the script's rotation in lockstep, on a
// scratch port that does not collide with the fleet's long-lived listeners — and
// pins the QA-CRIER-10 change: the runner no longer hard-codes a single port
// (`DEMO_PORT="${DEMO_PORT:-18961}"`), it SELECTS one with the shared guard.
func TestDefaultBaseURLUsesRunDemoScratchPort(t *testing.T) {
	src := runDemoScript(t)
	m := demoPortBase.FindStringSubmatch(src)
	if m == nil {
		t.Fatal(`run-demo.sh: DEMO_PORT_BASE="${DEMO_PORT_BASE:-<port>}" (the first rotation candidate) not found`)
	}
	want := "http://127.0.0.1:" + m[1]
	if defaultBaseURL != want {
		t.Errorf("defaultBaseURL = %q, run-demo.sh first rotation candidate implies %q", defaultBaseURL, want)
	}
	if demoPortDefault.MatchString(src) {
		t.Error("run-demo.sh still defaults DEMO_PORT to ONE fixed port — the port must be selected, not hard-coded (QA-CRIER-10)")
	}
	if !strings.Contains(src, `select_scratch_port "${DEMO_PORT:-}" "$DEMO_PORT_BASE"`) {
		t.Error("run-demo.sh must choose its port with the shared select_scratch_port")
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

// TestPortIsPreCheckedAndMeasuredServerIsOurs covers the failure mode seen live:
// the port was owned by a foreign crier, the script measured that server, and the
// failure surfaced as a bogus "/mesh/peers count != 2". The mechanism is now the
// shared guard library (the detailed ordering invariants live in
// TestRunDemoUsesTheSharedGuardsAndProvesOwnership); what this test keeps honest
// is that the guards are still wired to THIS script's port and pid.
func TestPortIsPreCheckedAndMeasuredServerIsOurs(t *testing.T) {
	src := runDemoScript(t)

	if !strings.Contains(src, `. "$REPO_ROOT/scripts/lib/port-guard.sh"`) {
		t.Error("run-demo.sh must source scripts/lib/port-guard.sh (the shared guards)")
	}
	if !strings.Contains(src, `select_scratch_port "${DEMO_PORT:-}" "$DEMO_PORT_BASE"`) {
		t.Error("run-demo.sh must CHOOSE its port with the shared select_scratch_port (QA-CRIER-10)")
	}
	if !strings.Contains(src, `assert_port_owned "$DEMO_PORT" "$SERVER_PID"`) {
		t.Error("run-demo.sh must assert the pid holding $DEMO_PORT is the relay it started")
	}
	if !strings.Contains(src, `kill -0 "$SERVER_PID" 2>/dev/null`) {
		t.Error("run-demo.sh must fail fast when its own relay process exits during startup")
	}
	if !strings.Contains(src, `'"count":0'`) {
		t.Error("run-demo.sh must assert the relay it talks to starts with an EMPTY peer list")
	}
}

// TestRunDemoSelectsItsPortInsteadOfHardCodingOne pins the QA-CRIER-10 wiring as
// source invariants: the port is chosen before anything is built, the caller
// override is passed through (checked, never rotated), the candidate budget is
// threaded, and no port literal survives anywhere else in the body — a component
// left on a literal would be measured on a port nothing selected.
func TestRunDemoSelectsItsPortInsteadOfHardCodingOne(t *testing.T) {
	src := runDemoScript(t)

	selectIdx := strings.Index(src, `select_scratch_port "${DEMO_PORT:-}" "$DEMO_PORT_BASE"`)
	if selectIdx < 0 {
		t.Fatal("run-demo.sh must call select_scratch_port with its override and candidate base")
	}
	buildIdx := strings.Index(src, `go build -o "$WORKDIR/crier" ./cmd/server`)
	transcriptIdx := strings.Index(src, `mktemp "$TRANSCRIPT_DIR/ws-mesh-demo-TRANSCRIPT-`)
	if buildIdx < 0 || transcriptIdx < 0 {
		t.Fatalf("run-demo.sh must have a build step and a transcript step (build=%d transcript=%d)", buildIdx, transcriptIdx)
	}
	if selectIdx > buildIdx {
		t.Error("the port must be settled BEFORE anything is built")
	}
	if selectIdx > transcriptIdx {
		t.Error("the port must be settled BEFORE a refusal can look like a started run (transcript opened first)")
	}
	for _, want := range []string{
		`"$DEMO_PORT_CANDIDATES"`,
		`DEMO_PORT="$PORT_GUARD_SELECTED"`,
		`BASE="http://127.0.0.1:${DEMO_PORT}"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("run-demo.sh is missing the rotation wiring %q", want)
		}
	}

	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") || baseLine.MatchString(line) {
			continue
		}
		if m := portLiteral.FindStringSubmatch(line); m != nil {
			t.Errorf("run-demo.sh line %d carries the port literal %s outside the candidate-base definition: %s",
				i+1, m[1], strings.TrimSpace(line))
		}
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

	for _, want := range []string{"POST /agents", "mesh/connect", "18961", "ws-mesh-demo-TRANSCRIPT", "DEMO_PORT",
		// DF-CRIER-3: the demo README must document the exchange it now runs.
		"roundtrip -agent AGENT -target AGENT", "-respond", "request_id", "KEEPALIVE"} {
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

// --- DF-CRIER-3: the REQUEST → RESPONSE leg -------------------------------

// TestRunDemoDrivesTheExchangeLeg pins the shell driver's new assertions as
// source invariants. Each entry is a way the leg could pass vacuously:
//
//   - no roundtrip invocation at all (the exchange never runs);
//   - an assertion that only checks "a RESPONSE arrived" without comparing
//     request_id to the REQUEST's message_id;
//   - comparing a field to itself (request_id vs the RESPONSE's own
//     message_id) so the run cannot tell the two apart;
//   - a KEEPALIVE observed but never checked to be classified as ignored;
//   - the responder's side never cross-checked, so a client that matched on
//     something other than the echoed message_id could still go green.
func TestRunDemoDrivesTheExchangeLeg(t *testing.T) {
	src := runDemoScript(t)

	var roundtripArgs, responderArgs string
	for _, m := range clientCall.FindAllStringSubmatch(src, -1) {
		args := strings.TrimSpace(m[1])
		if strings.Contains(args, "roundtrip ") {
			roundtripArgs = args
		}
		if strings.Contains(args, "peer -agent demo-agent-b") && strings.Contains(args, "-respond") {
			responderArgs = args
		}
	}
	if roundtripArgs == "" {
		t.Fatal(`run-demo.sh has no "$WORKDIR/ws-mesh-demo" roundtrip invocation`)
	}
	for _, want := range []string{"-agent demo-agent-a", "-target demo-agent-b", "-expect-status 200", "-keepalive-wait", `-url "$BASE"`} {
		if !strings.Contains(roundtripArgs, want) {
			t.Errorf("roundtrip invocation is missing %q: %q", want, roundtripArgs)
		}
	}
	if responderArgs == "" {
		t.Fatal("run-demo.sh must start demo-agent-b with -respond so the REQUEST can be answered")
	}
	if !strings.Contains(responderArgs, "-status 200") {
		t.Errorf("the responder must be told which status to answer with: %q", responderArgs)
	}

	for _, want := range []string{
		// The requester must have connected as a mesh peer of its own first.
		`mesh/connect/`,
		// The correlation assertion itself, and the proof the run can tell the
		// two fields apart.
		`[ "$RESP_REQUEST_ID" = "$REQ_ID" ]`,
		`[ "$RESP_MSG_ID" != "$REQ_ID" ]`,
		`[ "${#REQ_ID}" = "24" ]`,
		// The status code the responder sent.
		`[ "$RESP_STATUS" = "200" ]`,
		// The client's own exit status, so a timeout cannot pass.
		`[ "$ROUNDTRIP_RC" = "0" ]`,
		// The frame the requester accepted was a RESPONSE correlated with the
		// REQUEST's message_id, and its body arrived verbatim (as an object).
		`grep -q "^RESPONSE RECEIVED request_id=${REQ_ID} " "$ROUNDTRIP_OUT"`,
		`body={"pong":true,"from":"demo-agent-b"}`,
		// KEEPALIVE frames counted by the shared classification, and the live
		// one required when the wait is enabled.
		`^KEEPALIVE (IGNORED|OBSERVED) `,
		`grep -m1 '^KEEPALIVE OBSERVED ' "$ROUNDTRIP_OUT"`,
		// The responder's own log: it saw that message_id and echoed it.
		`[ "$B_REQ_ID" = "$REQ_ID" ]`,
		`[ "$B_SENT_ID" = "$REQ_ID" ]`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("run-demo.sh is missing the exchange assertion %q", want)
		}
	}

	// The requester reconnects as demo-agent-a, so the holding socket must be
	// closed (and the mesh must have dropped it) BEFORE the roundtrip starts:
	// a second socket under one agent id replaces the first in the server's
	// connection map, and the old socket's late OnClose would then delete the
	// new entry and drop the RESPONSE.
	killIdx := strings.Index(src, `kill "$PEER_A_PID"`)
	waitIdx := strings.Index(src, `wait "$PEER_A_PID"`)
	roundtripIdx := strings.Index(src, `roundtrip -agent demo-agent-a`)
	for _, idx := range []int{killIdx, waitIdx, roundtripIdx} {
		if idx < 0 {
			t.Fatalf("run-demo.sh must close peer A's holding socket before the roundtrip (kill=%d wait=%d roundtrip=%d)", killIdx, waitIdx, roundtripIdx)
		}
	}
	if killIdx > roundtripIdx || waitIdx > roundtripIdx {
		t.Error("peer A's holding socket must be killed and reaped BEFORE the roundtrip connects as demo-agent-a")
	}
	if !strings.Contains(src, "is still listed after its socket closed") {
		t.Error("run-demo.sh must fail when demo-agent-a is still on the mesh after its socket closed")
	}
}

// TestClassifyInboundFiltersKeepaliveAndForeignReplies drives the client-side
// filter directly. It is the deterministic half of the KEEPALIVE proof: the
// socket carries KEEPALIVE frames (and other agents' replies) interleaved with
// the answer, so "the next frame" is not "the reply".
func TestClassifyInboundFiltersKeepaliveAndForeignReplies(t *testing.T) {
	const ours = "9cb0fb27f650afead3734c03"

	cases := []struct {
		name      string
		typ       mesh.MessageType
		requestID string
		want      frameKind
	}{
		{"KEEPALIVE carries no request_id", mesh.TypeKeepalive, "", frameIgnore},
		{"KEEPALIVE carrying an id is still not a reply", mesh.TypeKeepalive, ours, frameIgnore},
		{"REGISTER is not a reply", mesh.TypeRegister, "", frameIgnore},
		{"REGISTER_ACK is not a reply", mesh.TypeRegisterAck, "", frameIgnore},
		{"RESPONSE to our REQUEST", mesh.TypeResponse, ours, frameReply},
		{"RESPONSE to another REQUEST", mesh.TypeResponse, "116bdfb85f15c004180f65b3", frameIgnore},
		{"RESPONSE with no correlation field", mesh.TypeResponse, "", frameIgnore},
		{"ERROR to our REQUEST", mesh.TypeError, ours, frameReply},
		{"ERROR to another REQUEST", mesh.TypeError, "a084c5f8fe35491bb4791a46", frameIgnore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyInbound(tc.typ, tc.requestID, ours); got != tc.want {
				t.Errorf("classifyInbound(%s, %q, %q) = %v, want %v", tc.typ, tc.requestID, ours, got, tc.want)
			}
		})
	}
}

// meshPeerCount reads /mesh/peers off the test's in-process server.
func meshPeerCount(t *testing.T, base, agentID string) bool {
	t.Helper()
	resp, err := http.Get(base + "/mesh/peers")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return strings.Contains(string(buf[:n]), `"agent_id":"`+agentID+`"`)
}

// TestRoundtripIgnoresKeepaliveFramesMidAwait is the live half of the KEEPALIVE
// proof, driven against a REAL in-process mesh: the same handler cmd/server
// mounts (/mesh/connect/{agentID}) served from a real listener, with the mesh
// keepalive interval far below the responder's answer delay. KEEPALIVE frames
// therefore reach the requester's socket WHILE its RESPONSE is still pending.
//
// The assertion is the client's exit status: cmdRoundtrip exits 0 only when the
// frame that satisfied classifyInbound carried the expected status_code AND
// (-require-keepalive-before-reply) at least one KEEPALIVE was ignored while
// that RESPONSE was pending. A client that treated the next frame as the answer
// would decode a KEEPALIVE as a RESPONSE, read status_code 0, and exit 1; a run
// in which no KEEPALIVE interleaved at all cannot go green either.
//
// The wait for the RESPONSE is bounded twice (DF-CRIER-252): -timeout is a
// wall-clock HANG GUARD — a host stall of more than the old 5s was measured
// here once a full `go test ./...` run puts ~16 test binaries, two of them
// compiling the tree, on the same box — and -max-keepalives is a PROGRESS
// bound, which advances only while the server keeps ticking, so it stretches
// with the host instead of against it. Neither one is the pass condition: a
// mis-correlated or KEEPALIVE-shaped reply is rejected the moment it is read.
func TestRoundtripIgnoresKeepaliveFramesMidAwait(t *testing.T) {
	if rc := liveRoundtrip(t, startTestMesh(t, 120*time.Millisecond), "test-agent-a"); rc != 0 {
		t.Fatalf("roundtrip exited %d, want 0: a frame that was not the correlated RESPONSE was accepted as the reply (a KEEPALIVE mistaken for the answer), the reply did not echo the REQUEST's message_id, or no KEEPALIVE was ignored while it was pending", rc)
	}
}

// liveRoundtrip is the exchange both the test above and the stall fixture drive:
// a real in-process mesh (keepalive tick 120ms), the demo's own `peer -respond`
// client answering 900ms later, and one roundtrip whose exit status is the
// assertion. Sharing it keeps the fixture from drifting away from the flow it is
// supposed to guard.
func liveRoundtrip(t *testing.T, tm *testMesh, requesterID string) int {
	t.Helper()

	responderDone := make(chan int, 1)
	go func() {
		responderDone <- cmdPeer(tm.wsBase, []string{
			"-agent", "test-agent-b",
			"-respond",
			"-status", "200",
			"-body", `{"pong":true,"from":"test-agent-b"}`,
			"-respond-delay", "900ms",
		})
	}()

	// The responder must be on the mesh before the REQUEST is sent, or the
	// server answers CONTROLLER_OFFLINE and the wrong path is measured.
	waitForPeer(t, tm.httpBase, "test-agent-b", 20*time.Second)

	rc := cmdRoundtrip(tm.wsBase, []string{
		"-agent", requesterID,
		"-target", "test-agent-b",
		"-method", "GET",
		"-path", "/ping",
		"-body", `{"hello":"world"}`,
		"-expect-status", "200",
		"-timeout", "30s",
		"-max-keepalives", "128",
		"-require-keepalive-before-reply",
		"-keepalive-wait", "10s",
	})

	// Unblock the responder's read loop so its goroutine ends here.
	tm.stop()
	select {
	case <-responderDone:
	case <-time.After(10 * time.Second):
		t.Error("the responder never exited after the mesh stopped")
	}
	return rc
}

// TestRoundtripRequiresAnInterleavedKeepaliveOnTheLiveMesh is the DIFFERENTIAL
// proof of the pass condition above, end to end (real listener, real
// /mesh/connect handler, the demo's own `peer -respond` client): with the
// server's own 30s keepalive interval and a responder that answers at once, the
// correlated RESPONSE is the FIRST frame on the requester's socket.
// -require-keepalive-before-reply must then refuse to exit 0, and the identical
// run without the flag must exit 0 — so it is the flag, not the setup, that
// demands an ignored KEEPALIVE mid-await, and a "reply was the only frame"
// green cannot pass for an interleaving proof.
//
// The two runs use different requester ids on purpose: a second socket under one
// agent id replaces the first in the server's connection map, and the old
// socket's late OnClose would then delete the new entry and drop the RESPONSE
// (the race run-demo.sh documents and works around).
func TestRoundtripRequiresAnInterleavedKeepaliveOnTheLiveMesh(t *testing.T) {
	tm := startTestMesh(t, 30*time.Second) // the server's default: no tick before the reply

	responderDone := make(chan int, 1)
	go func() {
		responderDone <- cmdPeer(tm.wsBase, []string{
			"-agent", "test-agent-b",
			"-respond",
			"-status", "200",
			"-body", `{"pong":true,"from":"test-agent-b"}`,
		})
	}()
	defer func() {
		tm.stop()
		select {
		case <-responderDone:
		case <-time.After(10 * time.Second):
			t.Error("the responder never exited after the mesh stopped")
		}
	}()
	waitForPeer(t, tm.httpBase, "test-agent-b", 20*time.Second)

	base := []string{
		"-target", "test-agent-b",
		"-method", "GET",
		"-path", "/ping",
		"-body", `{"hello":"world"}`,
		"-expect-status", "200",
		"-timeout", "20s",
		"-max-keepalives", "128",
	}

	// Half 1: no KEEPALIVE interleaves, and the flag is not asked for — the reply
	// correlates on its own, so this run must pass. Without this half a refusal
	// below would prove nothing about the flag.
	if rc := cmdRoundtrip(tm.wsBase, append([]string{"-agent", "test-agent-a1"}, base...)); rc != 0 {
		t.Fatalf("roundtrip exited %d without -require-keepalive-before-reply, want 0: the differential needs this half green", rc)
	}

	// Half 2: same flow, same socket shape, one flag more.
	if rc := cmdRoundtrip(tm.wsBase, append([]string{"-agent", "test-agent-a2", "-require-keepalive-before-reply"}, base...)); rc == 0 {
		t.Fatal("a correlated RESPONSE that arrived before any KEEPALIVE was ignored exited 0 with -require-keepalive-before-reply set: the interleaving proof this test exists for was vacuous")
	}
}

// --- the await loop, driven deterministically (DF-CRIER-252) ---------------
//
// The live test above is the integration half of the proof; these are the
// deterministic half. awaitReply is the loop the demo client actually runs, so
// the interleaving rule and the request_id correlation can be pinned frame by
// frame, with no listener, no clock and no load average — a run of this file
// cannot go green because the box happened to be fast.

// testMesh is an in-process mesh the tests drive: a real listener serving the
// same handler cmd/server mounts, plus the handle that stops it exactly once.
type testMesh struct {
	wsBase   string
	httpBase string
	m        *mesh.Mesh
	stop     func()
}

func startTestMesh(t *testing.T, keepalive time.Duration) *testMesh {
	t.Helper()
	cfg := mesh.DefaultMeshConfig("test-mesh")
	cfg.KeepaliveInterval = keepalive
	m := mesh.NewMesh(cfg)

	r := mux.NewRouter()
	r.HandleFunc("/mesh/connect/{agentID}", mesh.HandleConnect(m))
	r.HandleFunc("/mesh/peers", mesh.HandlePeers(m)).Methods("GET")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: r}
	go func() { _ = srv.Serve(ln) }()

	// mesh.Stop closes m.stopCh, so it must run exactly once on every path —
	// including the tests that stop the mesh mid-test and again at cleanup.
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = srv.Close()
			m.Stop()
		})
	}
	t.Cleanup(stop)
	return &testMesh{
		wsBase:   "ws://" + ln.Addr().String(),
		httpBase: "http://" + ln.Addr().String(),
		m:        m,
		stop:     stop,
	}
}

// waitForPeer blocks until agentID appears in /mesh/peers (the precondition
// both roundtrip tests need, or the server answers CONTROLLER_OFFLINE and the
// wrong path is measured). within is a hang guard, not a timing assertion.
func waitForPeer(t *testing.T, httpBase, agentID string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !meshPeerCount(t, httpBase, agentID) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared on /mesh/peers within %s", agentID, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// scriptedConn is a frameConn replaying a scripted sequence. Once the script is
// exhausted it returns a read-deadline error — exactly what a real socket does
// when the await's hang guard passes — so an await that never correlates still
// terminates and a test never sleeps.
type scriptedConn struct {
	script    []scriptFrame
	pos       int
	deadlines []time.Time
}

type scriptFrame struct {
	frame []byte
	err   error
}

// scriptTimeout is a net.Error whose Timeout() is true, standing in for the
// os.ErrDeadlineExceeded a gorilla connection returns after SetReadDeadline.
type scriptTimeout struct{}

func (scriptTimeout) Error() string   { return "read tcp 127.0.0.1:0->127.0.0.1:0: i/o timeout" }
func (scriptTimeout) Timeout() bool   { return true }
func (scriptTimeout) Temporary() bool { return true }

func (c *scriptedConn) SetReadDeadline(deadline time.Time) error {
	c.deadlines = append(c.deadlines, deadline)
	return nil
}

func (c *scriptedConn) ReadMessage() (int, []byte, error) {
	if c.pos >= len(c.script) {
		return 0, nil, scriptTimeout{}
	}
	f := c.script[c.pos]
	c.pos++
	if f.err != nil {
		return 0, nil, f.err
	}
	return 1, f.frame, nil // 1 == websocket.TextMessage
}

func frames(items ...scriptFrame) *scriptedConn {
	return &scriptedConn{script: items}
}

func wireFrame(t *testing.T, v any) []byte {
	t.Helper()
	data, err := mesh.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return data
}

func keepaliveFrame(t *testing.T, messageID string) scriptFrame {
	t.Helper()
	return scriptFrame{frame: wireFrame(t, &mesh.Keepalive{
		Envelope: mesh.Envelope{Type: mesh.TypeKeepalive, Version: 1, MessageID: messageID, Timestamp: time.Now()},
		AgentID:  "test-mesh",
	})}
}

func responseFrame(t *testing.T, requestID, messageID string, statusCode int) scriptFrame {
	t.Helper()
	return scriptFrame{frame: wireFrame(t, &mesh.Response{
		Envelope:   mesh.Envelope{Type: mesh.TypeResponse, Version: 1, MessageID: messageID, Timestamp: time.Now()},
		RequestID:  requestID,
		StatusCode: statusCode,
		Body:       json.RawMessage(`{"pong":true,"from":"test-agent-b"}`),
	})}
}

// TestAwaitReplyIgnoresInterleavedKeepalivesAndCorrelatesByRequestID pins both
// halves of the contract in one pass: a KEEPALIVE is not the answer, another
// agent's RESPONSE is not my answer, and my answer is recognised by
// request_id — never by being the next frame on the socket.
func TestAwaitReplyIgnoresInterleavedKeepalivesAndCorrelatesByRequestID(t *testing.T) {
	const (
		ours      = "9cb0fb27f650afead3734c03"
		foreignID = "116bdfb85f15c004180f65b3"
	)

	conn := frames(
		keepaliveFrame(t, "ka-1"),
		keepaliveFrame(t, "ka-2"),
		responseFrame(t, foreignID, "resp-foreign", 200), // another REQUEST's reply
		keepaliveFrame(t, "ka-3"),
		responseFrame(t, ours, "resp-ours", 200),
	)
	var out bytes.Buffer
	resp, keepalives, err := awaitReply(conn, &out, ours, awaitLimits{Budget: time.Second})
	if err != nil {
		t.Fatalf("awaitReply: %v (the correlated RESPONSE was the 5th frame and must be found)", err)
	}
	if resp.MessageID != "resp-ours" || resp.RequestID != ours {
		t.Errorf("accepted RESPONSE message_id=%s request_id=%s, want the one correlated with our REQUEST (resp-ours/%s)",
			resp.MessageID, resp.RequestID, ours)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status_code = %d, want 200", resp.StatusCode)
	}
	if keepalives != 3 {
		t.Errorf("keepalives_ignored = %d, want 3 (one per KEEPALIVE before the correlated reply)", keepalives)
	}
	if got := strings.Count(out.String(), "KEEPALIVE IGNORED "); got != 3 {
		t.Errorf("transcript carries %d KEEPALIVE IGNORED lines, want 3:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "FRAME IGNORED type=RESPONSE ") {
		t.Errorf("the other agent's RESPONSE was not reported as ignored:\n%s", out.String())
	}
	if len(conn.deadlines) != 1 {
		t.Errorf("SetReadDeadline called %d times, want 1 (the hang guard is armed once)", len(conn.deadlines))
	}
}

// TestAwaitReplyNeverAcceptsAKEEPALIVEAsTheResponse: a socket that only ever
// carries KEEPALIVEs must end in a failure, never in a reply.
func TestAwaitReplyNeverAcceptsAKEEPALIVEAsTheResponse(t *testing.T) {
	const ours = "9cb0fb27f650afead3734c03"

	conn := frames(keepaliveFrame(t, "ka-1"), keepaliveFrame(t, "ka-2"))
	var out bytes.Buffer
	resp, keepalives, err := awaitReply(conn, &out, ours, awaitLimits{Budget: time.Second})
	if err == nil {
		t.Fatalf("a KEEPALIVE-only stream was accepted as the reply (status_code=%d)", resp.StatusCode)
	}
	if resp.MessageID != "" || resp.RequestID != "" || resp.StatusCode != 0 {
		t.Errorf("a refused await returned a populated RESPONSE: %+v", resp)
	}
	if keepalives != 2 {
		t.Errorf("keepalives_ignored = %d, want 2", keepalives)
	}
	if !strings.Contains(err.Error(), "no RESPONSE for message_id="+ours) {
		t.Errorf("error does not name the REQUEST it waited for: %v", err)
	}
}

// TestAwaitReplyIgnoresAResponseForAnotherRequestID is the correlation
// assertion on its own: a well-formed RESPONSE carrying someone else's
// request_id is not this REQUEST's answer, however long we wait.
func TestAwaitReplyIgnoresAResponseForAnotherRequestID(t *testing.T) {
	const (
		ours    = "9cb0fb27f650afead3734c03"
		someone = "116bdfb85f15c004180f65b3"
	)

	conn := frames(responseFrame(t, someone, "resp-foreign", 200))
	var out bytes.Buffer
	resp, _, err := awaitReply(conn, &out, ours, awaitLimits{Budget: time.Second})
	if err == nil {
		t.Fatalf("a RESPONSE with request_id=%s was accepted as the reply to %s (status_code=%d)",
			someone, ours, resp.StatusCode)
	}
	if resp.RequestID == someone {
		t.Errorf("the refused await still returned the foreign RESPONSE: %+v", resp)
	}
	if !strings.Contains(out.String(), "FRAME IGNORED type=RESPONSE ") {
		t.Errorf("the foreign RESPONSE was not reported as ignored:\n%s", out.String())
	}
}

// TestAwaitReplyInterleavingRequirement is the flag's whole meaning: exit 0
// must prove an ignored KEEPALIVE, not assume one.
func TestAwaitReplyInterleavingRequirement(t *testing.T) {
	const (
		ours     = "9cb0fb27f650afead3734c03"
		pending  = "arrived before any KEEPALIVE was ignored"
		frameSet = "the reply was the only frame on the socket"
	)

	cases := []struct {
		name            string
		require         bool
		keepaliveFirst  bool
		wantErrContains string
	}{
		{"correlated reply after an ignored KEEPALIVE passes", true, true, ""},
		{"correlated reply with no KEEPALIVE fails when required", true, false, pending},
		{"same socket without the requirement passes", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := []scriptFrame{}
			if tc.keepaliveFirst {
				script = append(script, keepaliveFrame(t, "ka-1"))
			}
			script = append(script, responseFrame(t, ours, "resp-ours", 200))

			var out bytes.Buffer
			resp, keepalives, err := awaitReply(frames(script...), &out, ours, awaitLimits{
				Budget:           time.Second,
				RequireKeepalive: tc.require,
			})
			if tc.wantErrContains == "" {
				if err != nil {
					t.Fatalf("awaitReply: %v", err)
				}
				if resp.RequestID != ours {
					t.Errorf("accepted RESPONSE request_id=%s, want %s", resp.RequestID, ours)
				}
				return
			}
			if err == nil {
				t.Fatalf("awaitReply passed with keepalives_ignored=%d although an interleaved KEEPALIVE is required", keepalives)
			}
			if !strings.Contains(err.Error(), tc.wantErrContains) || !strings.Contains(err.Error(), frameSet) {
				t.Errorf("error %q does not explain the missing interleaving", err)
			}
			if resp.RequestID != ours {
				t.Errorf("the diagnostic RESPONSE was not returned to the caller: %+v", resp)
			}
		})
	}
}

// TestAwaitReplyProgressBoundEndsAFruitlessWaitWithoutAClock: the give-up
// decision can be driven by the peer's own progress. With a budget of an hour
// (which must NOT be what ends it) and a bound of 3 ignored KEEPALIVEs, the
// await stops after the third — and with the bound above the scripted count the
// same socket ends on the clock instead, so the knob is the bound and not
// something else.
func TestAwaitReplyProgressBoundEndsAFruitlessWaitWithoutAClock(t *testing.T) {
	const ours = "9cb0fb27f650afead3734c03"

	script := []scriptFrame{
		keepaliveFrame(t, "ka-1"), keepaliveFrame(t, "ka-2"), keepaliveFrame(t, "ka-3"),
		keepaliveFrame(t, "ka-4"), keepaliveFrame(t, "ka-5"), keepaliveFrame(t, "ka-6"),
	}

	var out bytes.Buffer
	_, keepalives, err := awaitReply(frames(script...), &out, ours, awaitLimits{
		Budget:        time.Hour,
		MaxKeepalives: 3,
	})
	if err == nil {
		t.Fatal("six ignored KEEPALIVEs with a bound of three did not end the await")
	}
	if keepalives != 3 {
		t.Errorf("keepalives_ignored = %d, want 3 (the bound, not the script)", keepalives)
	}
	if !strings.Contains(err.Error(), "progress bound") {
		t.Errorf("error %q does not name the bound that fired", err)
	}
	if got := strings.Count(out.String(), "KEEPALIVE IGNORED "); got != 3 {
		t.Errorf("transcript carries %d KEEPALIVE IGNORED lines, want 3:\n%s", got, out.String())
	}

	var out2 bytes.Buffer
	_, keepalives, err = awaitReply(frames(script...), &out2, ours, awaitLimits{
		Budget:        time.Second,
		MaxKeepalives: 7, // above the scripted count: the clock has to end this one
	})
	if err == nil {
		t.Fatal("the await did not end after the scripted frames ran out")
	}
	if keepalives != 6 {
		t.Errorf("keepalives_ignored = %d, want 6", keepalives)
	}
	if !strings.Contains(err.Error(), "within 1s") {
		t.Errorf("error %q is not the hang-guard message", err)
	}
}

// TestAwaitKeepaliveObservesALiveFrameAndCountsIt pins the second leg: it
// ignores everything that is not a KEEPALIVE, returns the running count
// (including the frames the reply await had already ignored), and ends on the
// hang guard when no KEEPALIVE ever arrives.
func TestAwaitKeepaliveObservesALiveFrameAndCountsIt(t *testing.T) {
	const ours = "9cb0fb27f650afead3734c03"

	var out bytes.Buffer
	n, err := awaitKeepalive(frames(
		responseFrame(t, ours, "resp-ours", 200), // a stray reply is not a KEEPALIVE
		keepaliveFrame(t, "ka-live"),
	), &out, time.Second, 3)
	if err != nil {
		t.Fatalf("awaitKeepalive: %v", err)
	}
	if n != 4 {
		t.Errorf("keepalives_ignored = %d, want 4 (3 already ignored + the live one)", n)
	}
	if !strings.Contains(out.String(), "FRAME IGNORED type=RESPONSE message_id=resp-ours (waiting for KEEPALIVE)") {
		t.Errorf("the stray frame was not reported:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "KEEPALIVE OBSERVED message_id=ka-live") {
		t.Errorf("the live KEEPALIVE was not reported:\n%s", out.String())
	}

	var out2 bytes.Buffer
	n, err = awaitKeepalive(frames(responseFrame(t, ours, "resp-ours", 200)), &out2, time.Second, 2)
	if err == nil {
		t.Fatal("a socket with no KEEPALIVE frame passed the live-KEEPALIVE leg")
	}
	if n != 2 {
		t.Errorf("keepalives_ignored = %d, want the prior count 2", n)
	}
	if !strings.Contains(err.Error(), "no KEEPALIVE within 1s") {
		t.Errorf("error %q does not name the bound", err)
	}
}

// --- DF-CRIER-252: the crash-in-slow-motion fixture -------------------------

const (
	// df252ChildEnv marks the CHILD half of the stall fixture below.
	df252ChildEnv = "CRIER_DF252_STALL_CHILD"
	// df252ChildRun is load-bearing: the child must run ONLY the fixture test.
	// A child that ran the package would report several PASS lines, and the
	// parent's "exactly one test, and it is this one" check could not tell a
	// suspended child from a suite that merely finished slowly.
	df252ChildRun = "^TestRoundtripSurvivesAStalledProcess$"
	// df252StallFor is how long the parent suspends the child. It has to exceed
	// the 5s wall-clock budget the pre-fix test armed (DF-CRIER-252: `-timeout
	// 5s`), or the fixture could not go red on the code it exists to guard; 6s
	// is that budget plus a second, and ~7x the 900ms the reply is due after.
	df252StallFor = 6 * time.Second
	// df252Awaiting is the child's own transcript line that proves its read
	// deadline is armed and its RESPONSE is still pending: the requester reads
	// nothing before the await, so the first frame it reports is a KEEPALIVE it
	// ignored while waiting. The parent suspends the child on this line, so the
	// stall lands inside the await by construction rather than by sleeping and
	// hoping.
	df252Awaiting = "KEEPALIVE IGNORED "
	// df252Answered is the line that proves the exchange completed after the
	// stall — the reply correlated and its status matched.
	df252Answered = "ROUNDTRIP OK "
)

var df252PassLine = regexp.MustCompile(`--- PASS: TestRoundtripSurvivesAStalledProcess \(([0-9.]+)s\)`)

// TestRoundtripSurvivesAStalledProcess is the DF-CRIER-252 gate, and it is the
// same live exchange as TestRoundtripIgnoresKeepaliveFramesMidAwait with one
// thing added: the test PROCESS is suspended for 6s in the middle of it.
//
// WHY IT EXISTS. The flake this ticket is about was never a protocol bug. QA
// measured 4 failures in 24 full-suite runs of the live test at loadavg ~19,
// each one reporting
//
//	ROUNDTRIP FAIL no RESPONSE ... within 5s
//
// with a healthy server, a healthy client, and a host that had simply not run
// the test process for the better part of five seconds. The test's whole
// pass/fail decision rested on that one constant. A stop-the-world stall is the
// smallest deterministic model of that shape, and unlike a load average it can be
// injected on demand: pre-fix the suspended child reads nothing before its 5s
// deadline, prints exactly the QA message, and fails; the fixed client keeps a
// wall-clock hang guard (30s) that is not the pass condition and a PROGRESS bound
// that only advances while the peer actually ticks, so it survives the stall and
// still correlates the reply.
//
// WHY A CHILD PROCESS. The failure is that the process does not run, which cannot
// be observed from inside it — something outside has to suspend and resume it.
// The child is spawned with -test.run pinned to this one test, so the parent's
// assertions ("exactly one PASS, no FAIL/SKIP, and that PASS names this test")
// cannot be satisfied by an unrelated run.
//
// It costs ~7s of wall time: the stall is real time, not a mock.
func TestRoundtripSurvivesAStalledProcess(t *testing.T) {
	if os.Getenv(df252ChildEnv) == "1" {
		df252StalledChild(t)
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run="+df252ChildRun, "-test.v", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), df252ChildEnv+"=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe for the child test binary: %v", err)
	}

	started := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the child test binary: %v", err)
	}

	// Suspend the child the moment it proves the await is live, hold it past the
	// budget the pre-fix test used, then let it finish. Reading its stdout line by
	// line is what makes the injection point deterministic.
	var transcript strings.Builder
	suspended := false
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		transcript.WriteString(line)
		transcript.WriteString("\n")
		if suspended || !strings.HasPrefix(line, df252Awaiting) {
			continue
		}
		suspended = true
		if err := cmd.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatalf("suspend the child (SIGSTOP): %v", err)
		}
		time.Sleep(df252StallFor)
		if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
			t.Fatalf("resume the child (SIGCONT): %v", err)
		}
	}
	waitErr := cmd.Wait()
	elapsed := time.Since(started)
	out := transcript.String()

	if !suspended {
		t.Fatalf("the child never reported %q, so no stall was injected and this fixture proved nothing after %s:\n%s",
			df252Awaiting, elapsed.Round(time.Millisecond), out)
	}
	if waitErr != nil {
		t.Fatalf("the child failed after a %s suspension (%v): the await could not survive the process not running, which is the DF-CRIER-252 failure mode — a wall clock was the pass condition:\n%s",
			df252StallFor, waitErr, out)
	}

	// Non-vacuity, in three parts: the child's OWN test clock has to include the
	// suspension (this flow takes ~1s when it is not stopped), it has to have
	// correlated the reply afterwards, and it has to have done so having ignored a
	// KEEPALIVE first — in that order, which is the interleaving the test is named
	// for.
	m := df252PassLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the child never reported its own PASS duration, so the suspension cannot be proven from this transcript:\n%s", out)
	}
	secs, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("unparseable child duration %q: %v", m[1], err)
	}
	if secs < df252StallFor.Seconds() {
		t.Fatalf("the child's test took %.2fs, less than the %s suspension it was supposed to suffer: it was not stopped, so this fixture proved nothing",
			secs, df252StallFor)
	}
	if !strings.Contains(out, df252Answered) {
		t.Fatalf("the child exited 0 but never printed %q, so the exchange did not complete after the stall:\n%s", df252Answered, out)
	}
	if passes := strings.Count(out, "--- PASS: "); passes != 1 {
		t.Fatalf("the child ran %d tests, wanted exactly the fixture test: an unrelated PASS would make this fixture vacuous:\n%s", passes, out)
	}
	if strings.Contains(out, "--- FAIL: ") || strings.Contains(out, "--- SKIP: ") {
		t.Fatalf("the child reported a FAIL/SKIP line, so this fixture proves nothing:\n%s", out)
	}
	kept := strings.Index(out, "KEEPALIVE IGNORED ")
	replied := strings.Index(out, "RESPONSE RECEIVED ")
	if kept < 0 || replied < 0 || kept > replied {
		t.Fatalf("the transcript does not show an ignored KEEPALIVE before the correlated RESPONSE (KEEPALIVE at %d, RESPONSE at %d):\n%s",
			kept, replied, out)
	}
}

// df252StalledChild is the child half: one live roundtrip, no stall injection of
// its own — the parent owns the suspension, because a stopped process cannot
// resume itself.
func df252StalledChild(t *testing.T) {
	t.Helper()
	tm := startTestMesh(t, 120*time.Millisecond)
	if rc := liveRoundtrip(t, tm, "test-agent-a"); rc != 0 {
		t.Fatalf("roundtrip exited %d, want 0: the await did not survive the suspension (a frame that was not the correlated RESPONSE was accepted as the reply, the reply did not echo the REQUEST's message_id, or no KEEPALIVE was ignored while it was pending)", rc)
	}
}
