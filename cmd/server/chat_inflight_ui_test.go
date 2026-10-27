// DF-CRIER-414 — the composer's in-flight surface, pinned in the embedded page.
//
// Two levels, both cheap enough for the guard's suite:
//
//  1. the served page carries the panel, the ticket the watcher keys on and the
//     server fields it renders — no client-side counter of its own;
//  2. the watcher's SHIPPED functions are executed (not copied) against scripted
//     server responses: an in-flight pass with an unreachable target, the
//     recorded terminal outcomes, and the settled-with-no-record case. The
//     assertions are the exact text an operator reads and the spinner's state,
//     because "the indicator must not be left spinning" is a behaviour, not a
//     string.
package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChatClientInFlightProgressSurface(t *testing.T) {
	html := string(chatClientHTML)
	for _, marker := range []string{
		// The panel and its two children: the spinner is decoration beside the
		// server's text, never the text itself.
		`id="send-progress" class="gate" role="status" aria-live="polite" hidden`,
		`id="send-progress-spinner" class="spinner" aria-hidden="true"`,
		`id="send-progress-text"`,
		// The ticket the send publishes, and the watcher that consumes it.
		`state.sendWatch = { session:id, key:payload.idempotency_key || ""`,
		`state.sendWatch.settled = true`,
		`setInterval(watchSendProgress, 1000)`,
		// DF-CRIER-467: the panel matches the request's OWN key only. A keyless
		// ticket is named and refused instead of borrowing a transcript message,
		// and every send — the explicit-task path included — carries a key.
		`if (!ticket.key) {`,
		`No per-request delivery record exists for this send yet: it was sent without an idempotency key`,
		`const recorded = messages.find(m => m.idempotency_key === ticket.key);`,
		`payload.idempotency_key = pendingMessageRequest.key;`,
		`const fingerprint = JSON.stringify({id, namespace:state.namespace, payload, task:!!task});`,
		`if (task && !inProgress) pendingMessageRequest = null;`,
		// Everything rendered is a SERVER value.
		`progressSeconds(pass.elapsed_ms)`,
		`progressSeconds(pass.budget_ms)`,
		`recorded.dispatch_live`,
		`outcomes.map(outcomeLine)`,
		// The spinner stops when the panel is no longer in the in-flight state.
		`$("send-progress-spinner").hidden = !spinning`,
		// Server text is displayed verbatim through textContent, never as HTML.
		`$("send-progress-text").textContent = text`,
		`panel.className = error ? "note error" : "gate"`,
		// Reduced motion is respected for the decoration.
		`@media(prefers-reduced-motion:reduce) { #send-progress .spinner { animation:none; } }`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded chat missing in-flight contract marker %q", marker)
		}
	}
	// No second clock: the page must not render an elapsed time of its own, and
	// must not carry a decorative countdown that runs regardless of the server.
	// DF-CRIER-467: a positional match is exactly the defect — the newest
	// transcript message is not this request's record.
	for _, forbidden := range []string{`countdown`, `elapsed++`, `elapsedMs++`, `setInterval(() => { $("send-progress-text")`, `messages[messages.length - 1]`} {
		if strings.Contains(html, forbidden) {
			t.Errorf("embedded chat must not build an in-flight claim the server has not measured: %q", forbidden)
		}
	}
}

// TestChatClientInFlightProgressBrowser drives the shipped page against a real
// fixture server with a dead and a blackholed target, over the DEFAULT 30s
// delivery budget, and asserts what the operator reads at ~2s, ~5s and after
// the budget. It is the row's live evidence; the script also writes the
// screenshots. Playwright must be on NODE_PATH (as for the other browser
// wrappers); a missing prerequisite is SKIP, never PASS. One delivery budget of
// wall time is inherent to the assertion — the budget is deliberate and is not
// shortened to make the test cheaper.
func TestChatClientInFlightProgressBrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the in-flight progress browser evidence")
	}
	binary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is required to build the scratch server for the browser evidence")
	}
	server := filepath.Join(t.TempDir(), "crier-inflight-server")
	build := exec.Command(binary, "build", "-o", server, "./cmd/server")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build scratch server: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "../../scripts/chat-inflight-progress.cjs")
	cmd.Env = append(os.Environ(), "CRIER_TEST_BINARY="+server, "CHAT_EVIDENCE="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 77 {
		t.Skipf("browser prerequisite unavailable: %s", out)
	}
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("in-flight progress browser evidence: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS: in-flight elapsed/budget, early unreachable naming the target") {
		t.Fatal("in-flight progress browser evidence did not complete its assertions")
	}
}

func TestChatClientInFlightProgressRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to execute the embedded chat client")
	}
	source := string(chatClientHTML)
	extract := func(start, end string) string {
		a, b := strings.Index(source, start), strings.Index(source, end)
		require.Greater(t, b, a, "page region %q..%q", start, end)
		return source[a:b]
	}
	harness := `
const assert = require('node:assert/strict');
const elements = new Map();
const $ = id => { if (!elements.has(id)) elements.set(id,{value:'',textContent:'',className:'',innerHTML:'',hidden:false,scrollHeight:0,scrollTop:0,clientHeight:100}); return elements.get(id); };
const esc = value => String(value);
const stamp = value => String(value);
const sessionPath = (id, suffix) => '/sessions/' + id + '/' + suffix;
const state = { current:'room', sendWatch:null, sending:false, busy:false, messages:[] };
const refresh = async () => {};
let scripted = null;
const call = async () => scripted;
` + extract("function outcomeLabel(", "async function send(event)") + extract("const PROGRESS_READ_GRACE_MS", "setInterval(watchSendProgress, 1000);") + `
const panel = () => $('send-progress');
const text = () => $('send-progress-text').textContent;
const spinning = () => $('send-progress-spinner').hidden === false;
const ticket = () => (state.sendWatch = { session:'room', key:'k', startedAt:Date.now(), settled:false, done:false, budgetMs:null, pendingPolls:0 });

(async () => {
  // 1. In flight, with a target the runtime already knows is unreachable and a
  //    target that has produced no attempt yet. Elapsed and budget are the
  //    SERVER's numbers, passed through untouched.
  scripted = {messages:[{idempotency_key:'k', dispatch_live:{
    attempt:1, started_at:'2026-10-19T12:00:00Z', elapsed_ms:2100, budget_ms:30000,
    targets:[
      {target:'dead-bot', attempts:2, last_attempt_at:'2026-10-19T12:00:02Z', class:'unreachable', reason:'dial tcp 127.0.0.1:39999: connect: connection refused'},
      {target:'slow-bot', delivery_class:'blocking_webhook', attempts:0}
    ]}}]};
  ticket(); await watchSendProgress();
  assert.equal(panel().hidden, false, 'in flight: the panel is visible');
  assert.equal(text(),
    'Delivery in flight — 2.1s of the 30.0s delivery budget\n' +
    'dead-bot: unreachable — dial tcp 127.0.0.1:39999: connect: connection refused (2 attempts · last attempt 2026-10-19T12:00:02Z)\n' +
    'slow-bot: no attempt has returned yet (server reported 0; blocking_webhook delivery)',
    'in flight: elapsed/budget and the per-target reason come from the server reading');
  assert.equal(spinning(), true, 'in flight: the spinner is running');

  // 2. The pass ended: the panel is REPLACED by the recorded terminal outcome
  //    and the spinner stops. The vocabulary is the existing outcomeLine.
  scripted = {messages:[{idempotency_key:'k', outcomes:[
    {target:'dead-bot', outcome:'refused', detail:'2 attempts, 30s delivery budget exhausted: dial tcp 127.0.0.1:39999: connect: connection refused'},
    {target:'blackhole-bot', outcome:'budget_cut', detail:'answer did not arrive inside the budget: 1 attempt, 30s delivery budget exhausted'}
  ]}]};
  await watchSendProgress();
  assert.equal(panel().hidden, false, 'terminal: the panel reports the outcome');
  assert.equal(spinning(), false, 'terminal: nothing is left spinning');
  assert.equal(text(),
    'Delivery finished — dead-bot: delivery refused — 2 attempts, 30s delivery budget exhausted: dial tcp 127.0.0.1:39999: connect: connection refused; ' +
    'blackhole-bot: budget_cut — answer did not arrive inside the budget: 1 attempt, 30s delivery budget exhausted. Recorded outcome; the transcript row keeps it.\n' +
    'dead-bot: retry state not reported; blackhole-bot: retry state not reported',
    'terminal: the recorded outcome, in the existing vocabulary');
  assert.ok(panel().className.includes('error'), 'terminal: a negative outcome is not styled as progress');
  assert.equal(state.sendWatch.done, true, 'terminal: watching stops');

  // 3. A settled send that recorded nothing leaves no indicator behind.
  state.sendWatch = { session:'room', key:'missing', startedAt:Date.now(), settled:true, done:false, budgetMs:null, pendingPolls:0 };
  scripted = {messages:[{idempotency_key:'other'}]};
  await watchSendProgress();
  assert.equal(panel().hidden, true, 'no record: the panel is cleared, not left claiming progress');

  // 4. A message whose fan-out finished but whose outcome write-back has not
  //    committed keeps the bounded "being written" state, and stops.
  state.sendWatch = { session:'room', key:'k', startedAt:Date.now(), settled:true, done:false, budgetMs:1000, pendingPolls:0 };
  scripted = {messages:[{idempotency_key:'k', outcomes_incomplete:true}]};
  await watchSendProgress();
  assert.match(text(), /server reports incomplete outcomes/);
  assert.equal(spinning(), true, 'write-back pending: state is still moving');
  state.sendWatch.pendingPolls = 99;
  await watchSendProgress();
  assert.match(text(), /not readable yet/);
  assert.equal(spinning(), false, 'bounded: a pending write-back never spins forever');
  assert.equal(state.sendWatch.done, true);

  console.log('PASS: in-flight, unreachable, terminal and settled states all report server values');
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(harness)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("in-flight progress rendering: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS: in-flight, unreachable, terminal and settled states") {
		t.Fatal("in-flight progress rendering did not complete its assertions")
	}
}
