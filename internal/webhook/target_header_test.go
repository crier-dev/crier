package webhook

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// DF-CRIER-175 — the outbound identity pair (spec §3).
//
// X-Crier-Agent has always carried the SENDER of the delivery; the spec
// documented it as the target, so a sink author could not tell which agent a
// delivery was FOR from any part of the wire (no header, no envelope field).
// The fix is additive: X-Crier-Agent keeps its sender meaning, a new
// X-Crier-Target names the endpoint's own agent, the envelope carries
// crier.target next to crier.sender, and an unknown sender omits the header
// entirely instead of sending it blank.

// headerKeys returns the raw header map keys, so a test can distinguish
// "header absent" from "header present with an empty value" — Get() reports
// "" for both.
func headerKeys(h http.Header, key string) ([]string, bool) {
	v, ok := h[http.CanonicalHeaderKey(key)]
	return v, ok
}

// envelopeKeys decodes a captured POST body and returns the crier object's
// keys, so an omitted (omitempty) field is distinguishable from an empty one.
func envelopeKeys(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var wire struct {
		Crier map[string]any `json:"crier"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode envelope body %q: %v", body, err)
	}
	return wire.Crier
}

// TestClientPost_IdentityHeaders: a delivery carries BOTH identities — the
// sender on X-Crier-Agent and the target on X-Crier-Target — and the envelope
// body carries crier.sender + crier.target.
func TestClientPost_IdentityHeaders(t *testing.T) {
	cs := newCaptureServer(t)
	c := NewClient(2*time.Second, nil)

	env := &Envelope{
		Crier: EnvelopeMeta{
			Version: 1, MessageID: "m1", Kind: "message",
			Sender: "agent-a", Target: "agent-b",
		},
		Payload: json.RawMessage(`{"hello":"world"}`),
	}
	res := c.Post(testConfig(cs.server.URL), env, 0)
	if res.Err != nil || res.StatusCode != 200 {
		t.Fatalf("post failed: %+v", res)
	}

	post := cs.last()
	if got := post.headers.Get("X-Crier-Agent"); got != "agent-a" {
		t.Errorf("X-Crier-Agent = %q, want the SENDER agent-a", got)
	}
	if got := post.headers.Get("X-Crier-Target"); got != "agent-b" {
		t.Errorf("X-Crier-Target = %q, want the TARGET agent-b", got)
	}
	crier := envelopeKeys(t, post.body)
	if crier["sender"] != "agent-a" {
		t.Errorf("body crier.sender = %v, want agent-a", crier["sender"])
	}
	if crier["target"] != "agent-b" {
		t.Errorf("body crier.target = %v, want agent-b", crier["target"])
	}
}

// TestClientPost_EmptySenderOmitsAgentHeader: an unknown sender must NOT be
// sent as an empty header value (a blank X-Crier-Agent reads downstream as a
// real-but-empty identity); the target identity and crier.target survive.
func TestClientPost_EmptySenderOmitsAgentHeader(t *testing.T) {
	cs := newCaptureServer(t)
	c := NewClient(2*time.Second, nil)

	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m1", Kind: "message", Target: "agent-b"},
		Payload: json.RawMessage(`{"hello":"world"}`),
	}
	res := c.Post(testConfig(cs.server.URL), env, 0)
	if res.Err != nil || res.StatusCode != 200 {
		t.Fatalf("post failed: %+v", res)
	}

	post := cs.last()
	if v, present := headerKeys(post.headers, "X-Crier-Agent"); present {
		t.Errorf("X-Crier-Agent header present (%q) for an empty sender, want the key ABSENT", v)
	}
	if got := post.headers.Get("X-Crier-Target"); got != "agent-b" {
		t.Errorf("X-Crier-Target = %q, want agent-b (the target is independent of the sender)", got)
	}
	crier := envelopeKeys(t, post.body)
	if _, present := crier["sender"]; present {
		t.Errorf("body crier.sender present (%v) for an empty sender, want the key absent (omitempty)", crier["sender"])
	}
	if crier["target"] != "agent-b" {
		t.Errorf("body crier.target = %v, want agent-b", crier["target"])
	}
}

// TestClientPostBatch_IdentityOnCoalescedPOST: one coalesced batch POST names
// its endpoint as the target (header) and repeats its own target in every
// inner envelope.
func TestClientPostBatch_IdentityOnCoalescedPOST(t *testing.T) {
	cs := newCaptureServer(t)
	c := NewClient(2*time.Second, nil)
	cfg := batchTestConfig(cs.server.URL)

	envs := []*Envelope{
		{Crier: EnvelopeMeta{Version: 1, MessageID: "m1", Kind: "message", Sender: "agent-a", Target: "agent-b"}, Payload: json.RawMessage(`{"n":1}`)},
		{Crier: EnvelopeMeta{Version: 1, MessageID: "m2", Kind: "message", Sender: "agent-a", Target: "agent-b"}, Payload: json.RawMessage(`{"n":2}`)},
	}
	res := c.PostBatch(cfg, envs, 0)
	if res.Err != nil || res.StatusCode != 200 {
		t.Fatalf("batch post failed: %+v", res)
	}

	post := cs.last()
	if got := post.headers.Get("X-Crier-Event"); got != "batch" {
		t.Fatalf("X-Crier-Event = %q, want batch", got)
	}
	if got := post.headers.Get("X-Crier-Target"); got != "agent-b" {
		t.Errorf("X-Crier-Target on batch POST = %q, want agent-b", got)
	}
	if got := post.headers.Get("X-Crier-Agent"); got != "agent-a" {
		t.Errorf("X-Crier-Agent on batch POST = %q, want the sender agent-a", got)
	}
	var body struct {
		Messages []Envelope `json:"messages"`
	}
	if err := json.Unmarshal(post.body, &body); err != nil {
		t.Fatalf("decode batch body: %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("batch messages = %d, want 2", len(body.Messages))
	}
	for i, env := range body.Messages {
		if env.Crier.Target != "agent-b" {
			t.Errorf("inner envelope %d crier.target = %q, want agent-b", i, env.Crier.Target)
		}
		if env.Crier.Sender != "agent-a" {
			t.Errorf("inner envelope %d crier.sender = %q, want agent-a", i, env.Crier.Sender)
		}
	}
}

// TestDriver_BatchFlushStampsTarget: the driver — not the caller — decides the
// target of a coalesced flush, so a batch whose envelopes carry no target
// (an embedding caller, a hand-built item) still names the endpoint's agent.
func TestDriver_BatchFlushStampsTarget(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Second, ProbeEvery: time.Second,
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return batchTestConfig(cs.server.URL), nil })

	cfg := batchTestConfig(cs.server.URL)
	cfg.Batch = &BatchConfig{MaxMessages: 2, FlushIntervalS: 60}
	for _, id := range []string{"m01", "m02"} {
		// No Target on the envelope: the driver must supply it.
		if _, err := d.Deliver("agent-b", cfg, &Envelope{
			Crier:   EnvelopeMeta{Version: 1, MessageID: id, Kind: "message"},
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "coalesced batch POST", func() bool {
		posts, _ := cs.batchRequests()
		return posts == 1
	})
	post := cs.last()
	if got := post.headers.Get("X-Crier-Target"); got != "agent-b" {
		t.Errorf("X-Crier-Target on flushed batch = %q, want agent-b", got)
	}
	var body struct {
		Messages []Envelope `json:"messages"`
	}
	if err := json.Unmarshal(post.body, &body); err != nil {
		t.Fatalf("decode batch body: %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("batch messages = %d, want 2", len(body.Messages))
	}
	for i, env := range body.Messages {
		if env.Crier.Target != "agent-b" {
			t.Errorf("inner envelope %d crier.target = %q, want agent-b", i, env.Crier.Target)
		}
	}
}

// TestDriver_QueuedBatchRedeliveryNamesItsTarget: a batch that reaches the
// drain loop through the durable queue (degraded requeue / retry) still names
// its target — the stamp is applied at the POST site, not only at enqueue.
func TestDriver_QueuedBatchRedeliveryNamesItsTarget(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})
	d.SetConfigResolver(func(id string) (*Config, error) { return batchTestConfig(cs.server.URL), nil })

	// A hand-built queue item whose envelopes never went through a flush —
	// the shape an older queue snapshot or an embedding caller produces.
	_ = d.queue.Push(&QueueItem{
		AgentID: "agent-b",
		Batch: []*Envelope{
			{Crier: EnvelopeMeta{Version: 1, MessageID: "m1", Kind: "message"}, Payload: json.RawMessage(`{}`)},
			{Crier: EnvelopeMeta{Version: 1, MessageID: "m2", Kind: "message"}, Payload: json.RawMessage(`{}`)},
		},
		CreatedAt: time.Now(),
	})
	d.drainQueue()

	posts, envs := cs.batchRequests()
	if posts != 1 || len(envs) != 2 {
		t.Fatalf("batch POSTs = %d with %d messages, want 1 POST with 2", posts, len(envs))
	}
	if got := cs.last().headers.Get("X-Crier-Target"); got != "agent-b" {
		t.Errorf("X-Crier-Target on redelivered batch = %q, want agent-b", got)
	}
	for i, env := range envs {
		if env.Crier.Target != "agent-b" {
			t.Errorf("redelivered inner envelope %d crier.target = %q, want agent-b", i, env.Crier.Target)
		}
	}
}

// TestDriver_ProbeNamesTheProbedAgent: the degraded-endpoint probe is
// delivered TO the agent being probed, so it carries that agent as its target
// (and keeps the server's own "crier" sender).
func TestDriver_ProbeNamesTheProbedAgent(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})
	d.SetConfigResolver(func(id string) (*Config, error) { return testConfig(cs.server.URL), nil })

	d.mu.Lock()
	d.degraded["agent-b"] = time.Now()
	d.mu.Unlock()
	d.probeDegraded()

	post := cs.last()
	if got := post.headers.Get("X-Crier-Event"); got != "probe" {
		t.Fatalf("X-Crier-Event = %q, want probe (no probe POST captured)", got)
	}
	if got := post.headers.Get("X-Crier-Target"); got != "agent-b" {
		t.Errorf("X-Crier-Target on probe = %q, want agent-b", got)
	}
	if got := post.headers.Get("X-Crier-Agent"); got != "crier" {
		t.Errorf("X-Crier-Agent on probe = %q, want crier (unchanged)", got)
	}
	crier := envelopeKeys(t, post.body)
	if crier["target"] != "agent-b" {
		t.Errorf("probe body crier.target = %v, want agent-b", crier["target"])
	}
}
