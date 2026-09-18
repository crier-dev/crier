package main

import (
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
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
// The assertion is the client's exit status: cmdRoundtrip writes the frame that
// satisfied classifyInbound as the reply and exits 0 only when it carried the
// expected status_code, so an exit of 0 with a delayed responder is only
// possible if every interleaved KEEPALIVE was ignored. A client that treated
// the next frame as the answer would decode a KEEPALIVE as a RESPONSE, read
// status_code 0, and exit 1.
func TestRoundtripIgnoresKeepaliveFramesMidAwait(t *testing.T) {
	cfg := mesh.DefaultMeshConfig("test-mesh")
	cfg.KeepaliveInterval = 120 * time.Millisecond // ticks 4x before the reply
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
	defer srv.Close()
	// mesh.Stop closes m.stopCh, so it must run exactly once on every path.
	var stopOnce sync.Once
	stopMesh := func() { stopOnce.Do(m.Stop) }
	defer stopMesh()

	wsBase := "ws://" + ln.Addr().String()
	httpBase := "http://" + ln.Addr().String()

	responderDone := make(chan int, 1)
	go func() {
		responderDone <- cmdPeer(wsBase, []string{
			"-agent", "test-agent-b",
			"-respond",
			"-status", "200",
			"-body", `{"pong":true,"from":"test-agent-b"}`,
			"-respond-delay", "900ms",
		})
	}()

	// The responder must be on the mesh before the REQUEST is sent, or the
	// server answers CONTROLLER_OFFLINE and this test would measure the wrong
	// path.
	deadline := time.Now().Add(5 * time.Second)
	for !meshPeerCount(t, httpBase, "test-agent-b") {
		if time.Now().After(deadline) {
			t.Fatal("responder never appeared on /mesh/peers")
		}
		time.Sleep(20 * time.Millisecond)
	}

	rc := cmdRoundtrip(wsBase, []string{
		"-agent", "test-agent-a",
		"-target", "test-agent-b",
		"-method", "GET",
		"-path", "/ping",
		"-body", `{"hello":"world"}`,
		"-expect-status", "200",
		"-timeout", "5s",
		"-keepalive-wait", "3s",
	})
	if rc != 0 {
		t.Fatalf("roundtrip exited %d, want 0: a frame that was not the correlated RESPONSE was accepted as the reply (a KEEPALIVE mistaken for the answer, or the reply did not echo the REQUEST's message_id)", rc)
	}

	// Unblock the responder's read loop so its goroutine ends here.
	stopMesh()
	select {
	case <-responderDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the responder never exited after the mesh stopped")
	}
}
