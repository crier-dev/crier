package webhook

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// DF-CRIER-11 — the captured outbound sender token.
//
// The envelope's `sender` is a SCALAR on the wire (EnvelopeMeta.Sender is a
// `string` with `json:"sender,omitempty"`), while specs/WEBHOOK-DELIVERY.md §3
// drew it as an OBJECT next to a scalar `target`. The existing identity tests
// (target_header_test.go) compare `crier["sender"]` against the STRING
// "agent-a", so a JSON OBJECT fails them with a decode error rather than a
// clear message; nothing pinned the JSON TOKEN TYPE, which is the part the doc
// got wrong. This test captures the raw body of a real POST and asserts the
// token type directly, plus the absence contract for an unset sender.
//
// It is the internal/webhook half of the DF-CRIER-11 pin; the docs/claims.yaml
// claim WEBHOOK-OUTBOUND-SENDER-SCALAR pins the spec LINE (via make docs-check)
// and re-measures the same two facts end-to-end through the live registry
// handler, where the deliver request's `sender` is typed.
func TestClientPost_SenderIsAScalarToken(t *testing.T) {
	cs := newCaptureServer(t)
	c := NewClient(2*time.Second, nil)

	env := &Envelope{
		Crier: EnvelopeMeta{
			Version: 1, MessageID: "m1", Kind: "message",
			Sender: "agent-x", Target: "agent-b",
		},
		Payload: json.RawMessage(`{"hello":"world"}`),
	}
	if res := c.Post(testConfig(cs.server.URL), env, 0); res.Err != nil || res.StatusCode != 200 {
		t.Fatalf("post failed: %+v", res)
	}
	body := cs.last().body
	t.Logf("captured outbound body (sender set): %s", body)

	var wire struct {
		Crier map[string]json.RawMessage `json:"crier"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode outbound body %q: %v", body, err)
	}
	raw, present := wire.Crier["sender"]
	if !present {
		t.Fatalf("crier.sender key ABSENT from the outbound body %s", body)
	}
	if got := string(raw); got != `"agent-x"` {
		t.Errorf("crier.sender token = %s, want the JSON STRING \"agent-x\" (not an object)", got)
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		t.Errorf("crier.sender does not decode as a JSON string: token=%s err=%v", raw, err)
	} else if asString != "agent-x" {
		t.Errorf("crier.sender = %q, want agent-x", asString)
	}
	// The sibling scalar the spec's sample printed next to it: if one is a
	// string the other must be too (the contradiction DF-CRIER-11 records).
	if got := string(wire.Crier["target"]); got != `"agent-b"` {
		t.Errorf("crier.target token = %s, want \"agent-b\"", got)
	}
}

// TestClientPost_NoSenderOmitsTheKey is the second half of the pin: a delivery
// that names no sender must carry NO `sender` key at all (omitempty), matching
// the spec's "omitted from the body when the delivery names no sender" — an
// empty string is a different, wrong state.
func TestClientPost_NoSenderOmitsTheKey(t *testing.T) {
	cs := newCaptureServer(t)
	c := NewClient(2*time.Second, nil)

	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m2", Kind: "message", Target: "agent-b"},
		Payload: json.RawMessage(`{"hello":"world"}`),
	}
	if res := c.Post(testConfig(cs.server.URL), env, 0); res.Err != nil || res.StatusCode != 200 {
		t.Fatalf("post failed: %+v", res)
	}
	body := cs.last().body
	t.Logf("captured outbound body (no sender): %s", body)

	var wire struct {
		Crier map[string]json.RawMessage `json:"crier"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode outbound body %q: %v", body, err)
	}
	if raw, present := wire.Crier["sender"]; present {
		t.Errorf("crier.sender present (%s) with no sender set, want the key ABSENT", raw)
	}
}

// TestSendCapture_PrintsRawBodies is a probe, not an assertion: it prints the
// raw crier object of the two bodies above in the form the summary quotes. It
// fails only if the capture itself breaks, so it cannot rot into a vacuous
// green.
func TestSendCapture_PrintsRawBodies(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		io.WriteString(w, `{"echo":true}`)
	}))
	defer sink.Close()

	c := NewClient(2*time.Second, nil)
	cfg := testConfig(sink.URL)
	for _, env := range []*Envelope{
		{Crier: EnvelopeMeta{Version: 1, MessageID: "cap-1", Kind: "message", Sender: "agent-x", Target: "agent-b"}, Payload: json.RawMessage(`{"hello":"world"}`)},
		{Crier: EnvelopeMeta{Version: 1, MessageID: "cap-2", Kind: "message", Target: "agent-b"}, Payload: json.RawMessage(`{"hello":"world"}`)},
	} {
		if res := c.Post(cfg, env, 0); res.Err != nil || res.StatusCode != 200 {
			t.Fatalf("post failed: %+v", res)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("captured %d bodies, want 2", len(bodies))
	}
	for i, b := range bodies {
		var wire struct {
			Crier json.RawMessage `json:"crier"`
		}
		if err := json.Unmarshal([]byte(b), &wire); err != nil {
			t.Fatalf("body %d is not JSON: %v (%s)", i, err, b)
		}
		t.Logf("raw crier object %d: %s", i, wire.Crier)
	}
}
