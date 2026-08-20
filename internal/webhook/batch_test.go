package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// batchRequests returns (count of X-Crier-Event: batch POSTs, every envelope
// coalesced across those POSTs).
func (cs *captureServer) batchRequests() (int, []Envelope) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := 0
	var envs []Envelope
	for _, r := range cs.requests {
		if r.headers.Get("X-Crier-Event") != "batch" {
			continue
		}
		n++
		var body struct {
			Messages []Envelope `json:"messages"`
		}
		if err := json.Unmarshal(r.body, &body); err != nil {
			continue
		}
		envs = append(envs, body.Messages...)
	}
	return n, envs
}

func batchTestConfig(url string) *Config {
	return &Config{
		URL:          url,
		DeliveryMode: "batch",
		Batch:        &BatchConfig{MaxMessages: 4, FlushIntervalS: 1},
		Retries:      5,
		TimeoutMs:    5000,
	}
}

// TestBatch_CoalescesToFewerPosts is the CR-FEAT-005 proof test: 10 rapid
// deliveries to one endpoint with max_messages=4 arrive as <=3 batched POSTs
// (4 + 4 + 2 — the trailing 2 flushed by flush_interval_s), and every
// envelope survives the coalescing with correct fields.
func TestBatch_CoalescesToFewerPosts(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Second, ProbeEvery: time.Second,
	})
	d.Start()
	defer d.Stop()
	cfg := batchTestConfig(cs.server.URL)

	const n = 10
	for i := 0; i < n; i++ {
		delivered, err := d.Deliver("agent-b", cfg, &Envelope{
			Crier: EnvelopeMeta{
				Version: 1, MessageID: fmt.Sprintf("m%02d", i), Kind: "message",
				Sender: "agent-a", SessionID: "sess-1",
			},
			Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
		})
		if err != nil || delivered {
			t.Fatalf("deliver %d: delivered=%v err=%v (want accepted, no err)", i, delivered, err)
		}
	}

	// End-state: all 10 messages arrive, coalesced into <=3 batch POSTs.
	waitFor(t, "all 10 messages delivered", func() bool {
		_, envs := cs.batchRequests()
		return len(envs) >= n
	})
	posts, envs := cs.batchRequests()
	if posts == 0 {
		t.Fatal("no batch POSTs observed")
	}
	if posts > 3 {
		t.Fatalf("batch POSTs = %d, want <= 3 (10 messages at max_messages=4 + interval flush)", posts)
	}
	if len(envs) != n {
		t.Fatalf("messages across batch POSTs = %d, want %d", len(envs), n)
	}
	seen := make(map[string]bool, n)
	for _, e := range envs {
		if e.Crier.Version != 1 || e.Crier.Kind != "message" || e.Crier.Sender != "agent-a" || e.Crier.SessionID != "sess-1" {
			t.Errorf("envelope fields corrupted: %+v", e.Crier)
		}
		seen[e.Crier.MessageID] = true
	}
	for i := 0; i < n; i++ {
		if !seen[fmt.Sprintf("m%02d", i)] {
			t.Errorf("message m%02d missing from batch bodies", i)
		}
	}
}

// TestBatch_IntervalFlush: well under max_messages, flush_interval_s alone
// triggers the batch POST.
func TestBatch_IntervalFlush(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Second, ProbeEvery: time.Second,
	})
	d.Start()
	defer d.Stop()
	cfg := batchTestConfig(cs.server.URL)
	cfg.Batch = &BatchConfig{MaxMessages: 100, FlushIntervalS: 1}

	for _, id := range []string{"m01", "m02"} {
		if _, err := d.Deliver("agent-b", cfg, &Envelope{
			Crier:   EnvelopeMeta{Version: 1, MessageID: id, Kind: "message"},
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "interval flush", func() bool {
		posts, envs := cs.batchRequests()
		return posts == 1 && len(envs) == 2
	})
}

// TestBatch_FlushFailureQueuesBatchForRetry: a failed batch flush re-queues
// the WHOLE batch as one durable queue item (spec §4 "queue flush retry");
// redelivery POSTs it as a batch again and drains after recovery.
func TestBatch_FlushFailureQueuesBatchForRetry(t *testing.T) {
	cs := newCaptureServer(t, 500)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: 40 * time.Millisecond,
		ProbeEvery: 40 * time.Millisecond, CircuitThreshold: 10,
	})
	d.Start()
	defer d.Stop()
	cfg := batchTestConfig(cs.server.URL)
	cfg.Batch = &BatchConfig{MaxMessages: 2, FlushIntervalS: 60}
	d.SetConfigResolver(func(id string) (*Config, error) { return cfg, nil })

	for _, id := range []string{"m01", "m02"} {
		if _, err := d.Deliver("agent-b", cfg, &Envelope{
			Crier:   EnvelopeMeta{Version: 1, MessageID: id, Kind: "message"},
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The flush fails (500) and the batch is retried through the queue.
	waitFor(t, "batch retried while endpoint down", func() bool {
		posts, _ := cs.batchRequests()
		return posts >= 2
	})

	cs.status.Store(200)
	waitFor(t, "queue drained after recovery", func() bool { return d.queue.Len() == 0 })
	waitFor(t, "successful batch POST after recovery", func() bool {
		posts, _ := cs.batchRequests()
		return posts >= 3
	})

	// Every batch POST preserved the coalescing: 2 envelopes per request.
	cs.mu.Lock()
	var lastBatch []Envelope
	for _, r := range cs.requests {
		if r.headers.Get("X-Crier-Event") != "batch" {
			continue
		}
		var body struct {
			Messages []Envelope `json:"messages"`
		}
		if json.Unmarshal(r.body, &body) == nil {
			lastBatch = body.Messages
		}
	}
	cs.mu.Unlock()
	if len(lastBatch) != 2 {
		t.Fatalf("messages in last batch POST = %d, want 2 (batch preserved across retries)", len(lastBatch))
	}
}

// TestAsync_FireAndForget: async mode accepts immediately (NOT synchronously
// delivered); the POST happens in the background through the queue drain.
func TestAsync_FireAndForget(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})
	d.Start()
	defer d.Stop()
	cfg := testConfig(cs.server.URL) // DeliveryMode: async
	d.SetConfigResolver(func(id string) (*Config, error) { return cfg, nil })

	delivered, err := d.Deliver("agent-b", cfg, &Envelope{
		Crier: EnvelopeMeta{
			Version: 1, MessageID: "m1", Kind: "message",
			Sender: "agent-a", SessionID: "sess-1",
		},
		Payload: json.RawMessage(`{"hello":"world"}`),
	})
	if err != nil || delivered {
		t.Fatalf("deliver: delivered=%v err=%v (want accepted, no err)", delivered, err)
	}
	// The redelivery interval is 1h, so the POST can only come from the wake
	// path — proving the background drain fires promptly.
	waitFor(t, "background POST", func() bool { return cs.messageCount() >= 1 })
	got := cs.last()
	if got.headers.Get("X-Crier-Event") != "message" {
		t.Fatalf("X-Crier-Event = %q, want message", got.headers.Get("X-Crier-Event"))
	}
	var env Envelope
	if err := json.Unmarshal(got.body, &env); err != nil {
		t.Fatalf("bad envelope json: %v", err)
	}
	if env.Crier.MessageID != "m1" || env.Crier.Sender != "agent-a" || env.Crier.SessionID != "sess-1" {
		t.Fatalf("envelope mismatch: %+v", env.Crier)
	}
}

// TestClientPostBatch_WireContract: a batch POST carries X-Crier-Event: batch
// and the spec §4 body {"messages":[...]} with every envelope intact, signed
// over the raw batch body.
func TestClientPostBatch_WireContract(t *testing.T) {
	cs := newCaptureServer(t)
	secret := []byte("test-secret")
	c := NewClient(2*time.Second, secret)
	cfg := batchTestConfig(cs.server.URL)
	envs := []*Envelope{
		{Crier: EnvelopeMeta{Version: 1, MessageID: "m1", Kind: "message", Sender: "agent-a"}, Payload: json.RawMessage(`{"a":1}`)},
		{Crier: EnvelopeMeta{Version: 1, MessageID: "m2", Kind: "message", Sender: "agent-a"}, Payload: json.RawMessage(`{"a":2}`)},
	}
	res := c.PostBatch(cfg, envs, 0)
	if res.Err != nil || res.StatusCode != 200 {
		t.Fatalf("batch post failed: %+v", res)
	}

	got := cs.last()
	if got.headers.Get("X-Crier-Event") != "batch" {
		t.Fatalf("X-Crier-Event = %q, want batch", got.headers.Get("X-Crier-Event"))
	}
	if got.headers.Get("X-Crier-Retry") != "0" {
		t.Fatalf("X-Crier-Retry = %q, want 0", got.headers.Get("X-Crier-Retry"))
	}
	var body struct {
		Messages []Envelope `json:"messages"`
	}
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("bad batch body %q: %v", got.body, err)
	}
	if len(body.Messages) != 2 || body.Messages[0].Crier.MessageID != "m1" || body.Messages[1].Crier.MessageID != "m2" {
		t.Fatalf("batch messages wrong: %+v", body.Messages)
	}
	sig := got.headers.Get("X-Crier-Signature")
	if sig == "" {
		t.Fatal("missing X-Crier-Signature on batch POST")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(got.body)
	if !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		t.Fatal("batch signature mismatch")
	}
}

// TestBatch_PermanentFailureDeadLetters: a permanent 4xx on the batch POST
// dead-letters the batch — no queue retry (spec §3 response contract).
func TestBatch_PermanentFailureDeadLetters(t *testing.T) {
	cs := newCaptureServer(t, 400)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: 40 * time.Millisecond, ProbeEvery: time.Second,
	})
	d.Start()
	defer d.Stop()
	cfg := batchTestConfig(cs.server.URL)
	cfg.Batch = &BatchConfig{MaxMessages: 2, FlushIntervalS: 60}
	d.SetConfigResolver(func(id string) (*Config, error) { return cfg, nil })

	for _, id := range []string{"m01", "m02"} {
		if _, err := d.Deliver("agent-b", cfg, &Envelope{
			Crier:   EnvelopeMeta{Version: 1, MessageID: id, Kind: "message"},
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}

	waitFor(t, "permanent batch POST", func() bool {
		posts, _ := cs.batchRequests()
		return posts == 1
	})
	// Give any (wrong) retry time to fire: a dead-lettered batch must NOT be
	// re-POSTed, and nothing may sit in the durable queue.
	time.Sleep(200 * time.Millisecond)
	if posts, _ := cs.batchRequests(); posts != 1 {
		t.Fatalf("batch POSTs = %d after permanent failure, want 1 (no retry)", posts)
	}
	if d.queue.Len() != 0 {
		t.Fatalf("queue len = %d after dead-letter, want 0", d.queue.Len())
	}
}

// TestBatch_SenderOverrideEnvelopeMode: the envelope's delivery_mode
// (sender override, spec §4) wins over the agent default — an async-default
// agent receiving batch-flagged messages coalesces them into batch POSTs.
func TestBatch_SenderOverrideEnvelopeMode(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Second, ProbeEvery: time.Second,
		// cfg.Batch is nil here (override test) — driver-level flush
		// controls apply, so keep the interval short for the wait.
		BatchFlushInterval: time.Second,
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return batchTestConfig(cs.server.URL), nil })

	cfg := testConfig(cs.server.URL) // agent default: async
	for i := 0; i < 3; i++ {
		if _, err := d.Deliver("agent-b", cfg, &Envelope{
			Crier:   EnvelopeMeta{Version: 1, MessageID: fmt.Sprintf("m%d", i), Kind: "message", DeliveryMode: "batch"},
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "override routed to batch", func() bool {
		_, envs := cs.batchRequests()
		return len(envs) == 3
	})
	if n := cs.messageCount(); n != 0 {
		t.Fatalf("message POSTs = %d, want 0 (batch-flagged messages must not POST individually)", n)
	}
}

// TestAsync_SenderOverrideEnvelopeMode: the reverse override — a batch-default
// agent receiving an async-flagged message fires a single POST via the queue
// instead of buffering.
func TestAsync_SenderOverrideEnvelopeMode(t *testing.T) {
	cs := newCaptureServer(t)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return batchTestConfig(cs.server.URL), nil })

	cfg := batchTestConfig(cs.server.URL) // agent default: batch
	if _, err := d.Deliver("agent-b", cfg, &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "m1", Kind: "message", DeliveryMode: "async"},
		Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "async override POST", func() bool { return cs.messageCount() >= 1 })
	if posts, _ := cs.batchRequests(); posts != 0 {
		t.Fatalf("batch POSTs = %d, want 0 (async-flagged message must bypass the buffer)", posts)
	}
}

// TestBatchConfig_Validate: batch registration validation.
func TestBatchConfig_Validate(t *testing.T) {
	good := &Config{URL: "http://x:1/h", DeliveryMode: "batch",
		Batch: &BatchConfig{MaxMessages: 10, FlushIntervalS: 5}, Retries: 5, TimeoutMs: 30000}
	if err := good.Validate(); err != nil {
		t.Fatalf("good batch config rejected: %v", err)
	}
	for _, bad := range []*Config{
		{URL: "http://x", DeliveryMode: "batch", Batch: &BatchConfig{MaxMessages: -1}},
		{URL: "http://x", DeliveryMode: "batch", Batch: &BatchConfig{FlushIntervalS: -2}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("invalid batch config accepted: %+v", bad.Batch)
		}
	}
}
