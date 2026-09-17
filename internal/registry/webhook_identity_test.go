package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crier-dev/crier/internal/webhook"
)

// DF-CRIER-175 — the outbound identity pair end-to-end through the REAL
// deliver handler.
//
// A sibling worker test (internal/webhook) proves the client API carries the
// two identities; these tests prove the PLUMBING: the target agent id the
// deliver handler already knows (the {id} in the path) reaches the wire as
// X-Crier-Target + crier.target without anything being derived from the
// webhook URL, and that the guard verdict of an INBOX delivery rides in the
// response BODY — never in an X-Crier-Guard-* response header, which exists
// only on the outbound webhook POST.
//
// Harnesses reused from siblings: webhookRecorder (guard_deliver_test.go)
// captures outbound POSTs, deliverHarness (deliver_transport_test.go) wires a
// real Handler + webhook driver.

// capturedEnvelope decodes a captured POST body's crier object as raw keys, so
// an omitted (omitempty) field is distinguishable from an empty one.
type capturedEnvelope struct {
	Crier map[string]any `json:"crier"`
}

func decodeCapturedEnvelope(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var wire capturedEnvelope
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode outbound POST body %q: %v", body, err)
	}
	return wire.Crier
}

// TestDeliver_WebhookPOSTNamesItsTarget: a delivery to a webhook agent goes
// out naming BOTH identities — X-Crier-Target is the agent in the request
// path (the delivery's target), X-Crier-Agent is the deliver body's sender.
func TestDeliver_WebhookPOSTNamesItsTarget(t *testing.T) {
	wh := &webhookRecorder{}
	whSrv := httptest.NewServer(wh)
	t.Cleanup(whSrv.Close)

	const target = "target-agent"
	h, _ := deliverHarness(t, target, &webhook.Config{URL: whSrv.URL, DeliveryMode: "blocking"})

	rec, _ := postDeliver(t, h, target, `{"payload":{"x":1},"sender":"agent-a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("blocking deliver: %d %s", rec.Code, rec.Body.String())
	}
	if wh.count() != 1 {
		t.Fatalf("endpoint POSTs = %d, want 1", wh.count())
	}
	post := wh.last()
	if got := post.headers.Get("X-Crier-Target"); got != target {
		t.Errorf("X-Crier-Target = %q, want the agent id in the path (%q)", got, target)
	}
	if got := post.headers.Get("X-Crier-Agent"); got != "agent-a" {
		t.Errorf("X-Crier-Agent = %q, want the SENDER agent-a (meaning unchanged)", got)
	}
	crier := decodeCapturedEnvelope(t, post.body)
	if crier["target"] != target {
		t.Errorf("body crier.target = %v, want %q", crier["target"], target)
	}
	if crier["sender"] != "agent-a" {
		t.Errorf("body crier.sender = %v, want agent-a", crier["sender"])
	}
}

// TestDeliver_WebhookPOSTSenderOmitted: when the deliver body names no sender,
// the outbound POST must omit X-Crier-Agent entirely (not send it blank) while
// still naming the target in the header and the body.
func TestDeliver_WebhookPOSTSenderOmitted(t *testing.T) {
	wh := &webhookRecorder{}
	whSrv := httptest.NewServer(wh)
	t.Cleanup(whSrv.Close)

	const target = "target-agent"
	h, _ := deliverHarness(t, target, &webhook.Config{URL: whSrv.URL, DeliveryMode: "async"})

	rec, _ := postDeliver(t, h, target, `{"payload":{"x":1}}`) // no "sender" key
	if rec.Code != http.StatusAccepted {
		t.Fatalf("async deliver: %d %s", rec.Code, rec.Body.String())
	}
	waitForPosts(t, wh, 1)
	post := wh.last()

	if v, present := post.headers[http.CanonicalHeaderKey("X-Crier-Agent")]; present {
		t.Errorf("X-Crier-Agent header present (%q) for a senderless delivery, want the key ABSENT", v)
	}
	if got := post.headers.Get("X-Crier-Target"); got != target {
		t.Errorf("X-Crier-Target = %q, want %q (the target does not depend on the sender)", got, target)
	}
	crier := decodeCapturedEnvelope(t, post.body)
	if _, present := crier["sender"]; present {
		t.Errorf("body crier.sender present (%v) for a senderless delivery, want the key absent (omitempty)", crier["sender"])
	}
	if crier["target"] != target {
		t.Errorf("body crier.target = %v, want %q", crier["target"], target)
	}
}

// TestDeliver_BatchPOSTNamesItsTarget: a batch-mode agent's coalesced POST
// carries the target header once, and every inner envelope repeats its own
// crier.target.
func TestDeliver_BatchPOSTNamesItsTarget(t *testing.T) {
	wh := &webhookRecorder{}
	whSrv := httptest.NewServer(wh)
	t.Cleanup(whSrv.Close)

	const target = "target-agent"
	cfg := &webhook.Config{
		URL:          whSrv.URL,
		DeliveryMode: "batch",
		Batch:        &webhook.BatchConfig{MaxMessages: 2, FlushIntervalS: 1},
	}
	h, _ := deliverHarness(t, target, cfg)

	for _, n := range []string{"1", "2"} {
		rec, _ := postDeliver(t, h, target, `{"payload":{"n":`+n+`},"sender":"agent-a"}`)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("batch deliver %s: %d %s", n, rec.Code, rec.Body.String())
		}
	}
	waitForPosts(t, wh, 1)
	post := wh.last()
	if got := post.headers.Get("X-Crier-Event"); got != "batch" {
		t.Fatalf("X-Crier-Event = %q, want batch", got)
	}
	if got := post.headers.Get("X-Crier-Target"); got != target {
		t.Errorf("X-Crier-Target on coalesced POST = %q, want %q", got, target)
	}
	var body struct {
		Messages []webhook.Envelope `json:"messages"`
	}
	if err := json.Unmarshal(post.body, &body); err != nil {
		t.Fatalf("decode batch body: %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("coalesced messages = %d, want 2", len(body.Messages))
	}
	for i, env := range body.Messages {
		if env.Crier.Target != target {
			t.Errorf("inner envelope %d crier.target = %q, want %q", i, env.Crier.Target, target)
		}
	}
}

// TestDeliver_InboxGuardErrorRidesInTheBody: a guard failure on an INBOX
// delivery fails open with 201 and the verdict in the response BODY
// (guard.errored) — the seven X-Crier-Guard-* headers are an outbound-webhook
// artifact and never appear on a deliver RESPONSE. This pins the sentence the
// corrected docs now state (specs/WEBHOOK-DELIVERY.md §3, examples/demo.sh,
// internal/registry/handler.go HandleDeliver).
func TestDeliver_InboxGuardErrorRidesInTheBody(t *testing.T) {
	f := newGuardFixture(t, guardVerdictAllow)
	f.llm.status = http.StatusInternalServerError // provider down → fail open

	if code := f.registerAgent(t, "inbox-agent", f.guardPolicy(false)); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	rec := f.deliver(t, "inbox-agent", `{"text":"x"}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("fail-open inbox deliver: %d %s", rec.Code, rec.Body.String())
	}

	var resp deliverResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode deliver response: %v", err)
	}
	if resp.Guard == nil {
		t.Fatalf("deliver response carries no guard object: %s", rec.Body.String())
	}
	if !resp.Guard.Errored {
		t.Errorf("deliver response guard.errored = false, want true (the verdict of an errored run must be visible)")
	}
	if resp.Guard.Decision != "allow" {
		t.Errorf("deliver response guard.decision = %q, want allow (fail-open)", resp.Guard.Decision)
	}
	for k, v := range rec.Result().Header {
		if strings.HasPrefix(k, "X-Crier-Guard-") {
			t.Errorf("inbox delivery answered a guard RESPONSE header %s: %q — the X-Crier-Guard-* headers exist only on the outbound webhook POST", k, v)
		}
	}
	if v := rec.Header().Get("X-Crier-Guard-Error"); v != "" {
		t.Errorf("X-Crier-Guard-Error response header = %q, want absent", v)
	}
}
