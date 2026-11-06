package codex_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/harness"
	"github.com/crier-dev/crier/internal/harness/codex"
)

// ---------------------------------------------------------------------------
// The live leg (CRIER-HARNESS-13)
//
// Both arms drive the INSTALLED codex CLI through the adapter's ordinary
// delivery path — the argv the adapter builds, the JSONL it parses, the receipt
// it returns. The only substitution is the far end of the wire: an in-process
// OpenAI-compatible Responses server in one arm, and a loopback port that was
// listening and is not anymore in the other.
//
// Nothing here reaches a model or leaves the host: the provider is a stub, it
// is authenticated with a placeholder key, and CODEX_HOME is a temp directory so
// the operator's own ~/.codex/config.toml and rollouts are untouched.
// ---------------------------------------------------------------------------

// stubProvider is a minimal OpenAI Responses API endpoint. It records the
// request that arrived (so a test can prove the message text travelled) and
// streams one assistant turn back as server-sent events, which is the protocol
// codex's wire_api "responses" speaks.
type stubProvider struct {
	srv *httptest.Server

	mu       sync.Mutex
	paths    []string
	bodies   []string
	authHead []string
}

func newStubProvider(t *testing.T, reply string) *stubProvider {
	t.Helper()
	p := &stubProvider{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.bodies = append(p.bodies, string(body))
		p.authHead = append(p.authHead, r.Header.Get("Authorization"))
		p.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		send := func(event string, data string) {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		item := `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + reply + `","annotations":[]}]}`
		send("response.created", `{"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1762400000,"status":"in_progress","model":"crier-stub","output":[]}}`)
		send("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}`)
		send("response.content_part.added", `{"type":"response.content_part.added","item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`)
		send("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"`+reply+`"}`)
		send("response.output_text.done", `{"type":"response.output_text.done","item_id":"msg_1","output_index":0,"content_index":0,"text":"`+reply+`"}`)
		send("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":`+item+`}`)
		send("response.completed", `{"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1762400000,"status":"completed","model":"crier-stub","output":[`+item+`],"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens":5,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":15}}}`)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// requests returns what the provider saw.
func (p *stubProvider) requests() (paths, bodies, auth []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...), append([]string(nil), p.bodies...), append([]string(nil), p.authHead...)
}

// liveRun is the real process runner plus a record of what the child printed,
// so a test can assert on the turn the adapter parsed instead of guessing from
// the receipt.
type liveRun struct {
	codexHome string

	mu   sync.Mutex
	spec []harness.ExecSpec
	res  []harness.ExecResult
}

func newLiveRun(t *testing.T) *liveRun {
	return &liveRun{codexHome: t.TempDir()}
}

// exec is the Exec seam: it runs the real binary with a hermetic CODEX_HOME and
// a placeholder credential (never a real one), and keeps the output.
func (l *liveRun) exec(ctx context.Context, spec harness.ExecSpec, timeout time.Duration) harness.ExecResult {
	spec.Env = append(os.Environ(),
		"CODEX_HOME="+l.codexHome,
		"OPENAI_API_KEY=dummy-crier-harness-13",
	)
	res := harness.RunCommand(ctx, spec, timeout)
	l.mu.Lock()
	l.spec = append(l.spec, spec)
	l.res = append(l.res, res)
	l.mu.Unlock()
	return res
}

func (l *liveRun) last() (harness.ExecSpec, harness.ExecResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.res) == 0 {
		return harness.ExecSpec{}, harness.ExecResult{}
	}
	return l.spec[len(l.spec)-1], l.res[len(l.res)-1]
}

// TestLiveCodexDeliveryAgainstTheConfiguredProvider is the delivery leg for
// CRIER-HARNESS-13, gated on CRIER_TEST_LIVE_HARNESS=1 the way the pi joint and
// the claude-code probe are.
//
// Arm 1 points the adapter's CR_CODEX_BASE_URL knob at a stub server: a real
// assistant turn must come back through the installed CLI and the receipt must
// report the delivered state. Arm 2 points the same knob at nothing: the
// delivery must fail with HARNESS_PROVIDER_UNREACHABLE quoting the target URL
// and the dial error, in seconds, with no egress and no model spend.
//
// With the owner's routing (CRIER-HARNESS-8) all arm 1 needs is that URL.
func TestLiveCodexDeliveryAgainstTheConfiguredProvider(t *testing.T) {
	if os.Getenv("CRIER_TEST_LIVE_HARNESS") != "1" {
		t.Skip("set CRIER_TEST_LIVE_HARNESS=1 to run the codex delivery legs (needs the codex CLI; no model spend, no egress)")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed on this host; the naming logic is covered by the unit tests")
	}
	// codex refuses to run outside a trusted directory, so the delivery cwd is
	// this package's directory — inside the repository, read-only sandbox.
	workdir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	t.Run("stub provider returns a real assistant turn", func(t *testing.T) {
		const prompt = "CRIER-HARNESS-13-PROMPT"
		const reply = "CRIER-HARNESS-13-REPLY"
		prov := newStubProvider(t, reply)
		run := newLiveRun(t)

		a := codex.New(codex.Config{
			Binary:  bin,
			Workdir: workdir,
			BaseURL: prov.srv.URL + "/v1",
			Timeout: 120 * time.Second,
			Exec:    run.exec,
		})

		start := time.Now()
		rc, err := a.Deliver(context.Background(), harness.SessionRef{},
			harness.Message{ID: "m_live_1", SessionID: "s_live", Text: "Reply with exactly: " + prompt})
		elapsed := time.Since(start)
		spec, res := run.last()
		stdout := string(res.Stdout)
		t.Logf("receipt=%+v elapsed=%s exit=%d stdout_bytes=%d", rc, elapsed.Round(time.Millisecond), res.ExitCode, res.StdoutSize)
		t.Logf("argv=%q", spec.Args)
		t.Logf("child stdout:\n%s", strings.TrimSpace(stdout))

		if err != nil {
			t.Fatalf("deliver returned an error: %v", err)
		}
		if rc.State != harness.StateDispatched || rc.Error != "" {
			t.Fatalf("want dispatched with no error code, got %+v (child stderr: %s)", rc, strings.TrimSpace(string(res.Stderr)))
		}
		if !rc.Created {
			t.Fatalf("a delivery with no session id is a create: %+v", rc)
		}
		if rc.SessionID == "" {
			t.Fatalf("a dispatched create must carry the thread id codex minted: %+v", rc)
		}
		if tid := codex.ThreadID(res.Stdout); tid != rc.SessionID {
			t.Fatalf("receipt thread id %q does not match the child's handshake %q", rc.SessionID, tid)
		}
		if rc.StdoutBytes != len(res.Stdout) {
			t.Fatalf("receipt stdout_bytes=%d does not match the observed %d bytes", rc.StdoutBytes, len(res.Stdout))
		}

		// The assistant turn came back: codex parsed the stub's streamed
		// response and printed it as its own agent message.
		if !strings.Contains(stdout, `"agent_message"`) {
			t.Fatalf("no agent_message event in the child's JSONL; the turn did not complete:\n%s", stdout)
		}
		if !strings.Contains(stdout, reply) {
			t.Fatalf("the assistant text the provider streamed is not in the child's turn:\n%s", stdout)
		}
		if !strings.Contains(stdout, `"turn.completed"`) {
			t.Fatalf("no turn.completed event; the delivery is not a finished turn:\n%s", stdout)
		}

		// The message travelled out through the knob: one request, at the
		// Responses path, carrying the prompt.
		paths, bodies, auth := prov.requests()
		if len(bodies) != 1 {
			t.Fatalf("provider saw %d requests, want 1 (paths %q)", len(bodies), paths)
		}
		if !strings.HasSuffix(paths[0], "/responses") {
			t.Fatalf("codex did not use the Responses wire API: %q", paths[0])
		}
		if !strings.Contains(bodies[0], prompt) {
			t.Fatalf("the delivered message never reached the provider endpoint")
		}
		if !strings.HasPrefix(auth[0], "Bearer ") {
			t.Fatalf("the provider override must still authenticate: %q", auth[0])
		}
		if elapsed > 120*time.Second {
			t.Fatalf("the stub turn took %s", elapsed)
		}
	})

	t.Run("router down is a named failure", func(t *testing.T) {
		dead := closedEndpoint(t)
		run := newLiveRun(t)

		a := codex.New(codex.Config{
			Binary:  bin,
			Workdir: workdir,
			BaseURL: dead,
			Timeout: 5 * time.Second,
			Exec:    run.exec,
		})

		start := time.Now()
		rc, err := a.Deliver(context.Background(), harness.SessionRef{},
			harness.Message{ID: "m_live_2", SessionID: "s_live", Text: "hello"})
		elapsed := time.Since(start)
		_, res := run.last()
		t.Logf("receipt=%+v elapsed=%s exit=%d timed_out=%v", rc, elapsed.Round(time.Millisecond), res.ExitCode, res.TimedOut)
		t.Logf("child stdout:\n%s", strings.TrimSpace(string(res.Stdout)))

		if err != nil {
			t.Fatalf("deliver returned an error: %v", err)
		}
		if rc.State != harness.StateFailed {
			t.Fatalf("a dead router must never be reported as success: %+v", rc)
		}
		if rc.Error != codex.CodeProviderUnreachable {
			t.Fatalf("want %s, got %q (tail %q)", codex.CodeProviderUnreachable, rc.Error, rc.Tail)
		}
		if !strings.Contains(rc.Tail, dead) {
			t.Fatalf("the receipt must quote the target URL %q: %q", dead, rc.Tail)
		}
		if !strings.Contains(rc.Tail, "connection refused") {
			t.Fatalf("the receipt must quote the dial error: %q", rc.Tail)
		}
		if strings.HasPrefix(rc.Tail, "delivery exceeded") {
			t.Fatalf("a refused endpoint must not come back as a bare timeout: %q", rc.Tail)
		}
		if elapsed > 30*time.Second {
			t.Fatalf("the unreachable arm must be bounded, took %s", elapsed)
		}
		t.Logf("named: %s", rc.Tail)
	})
}
