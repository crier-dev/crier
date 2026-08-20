package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capturedRequest records what a test endpoint received.
type capturedRequest struct {
	body       []byte
	headers    http.Header
	statusCode int
}

type captureServer struct {
	mu       sync.Mutex
	requests []capturedRequest
	status   atomic.Int32 // status to return (default 200)
	server   *httptest.Server
}

func newCaptureServer(t *testing.T, status ...int) *captureServer {
	cs := &captureServer{}
	cs.status.Store(200)
	if len(status) > 0 {
		cs.status.Store(int32(status[0]))
	}
	cs.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cs.mu.Lock()
		cs.requests = append(cs.requests, capturedRequest{body: body, headers: r.Header.Clone()})
		cs.mu.Unlock()
		if cs.status.Load() >= 400 {
			w.WriteHeader(int(cs.status.Load()))
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"reply":"ok"}`))
	}))
	t.Cleanup(cs.server.Close)
	return cs
}

func (cs *captureServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.requests)
}

// messageCount counts only real delivery POSTs (excludes health probes,
// which carry X-Crier-Event: probe).
func (cs *captureServer) messageCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	n := 0
	for _, r := range cs.requests {
		if r.headers.Get("X-Crier-Event") == "message" {
			n++
		}
	}
	return n
}

func (cs *captureServer) last() capturedRequest {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.requests) == 0 {
		return capturedRequest{}
	}
	return cs.requests[len(cs.requests)-1]
}

func testConfig(url string) *Config {
	return &Config{URL: url, DeliveryMode: "async", Retries: 5, TimeoutMs: 5000}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// TestClientPost_SignatureAndEnvelope: a POST carries the exact envelope and
// a valid HMAC signature when a secret is configured (spec §3).
func TestClientPost_SignatureAndEnvelope(t *testing.T) {
	cs := newCaptureServer(t)
	secret := []byte("test-secret")
	c := NewClient(2*time.Second, secret)

	env := &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: "abc123", Kind: "message", Sender: "agent-a", SessionID: "sess-1"},
		Payload: json.RawMessage(`{"hello":"world"}`),
	}
	res := c.Post(testConfig(cs.server.URL), env, 0)
	if res.Err != nil || res.StatusCode != 200 {
		t.Fatalf("post failed: %+v", res)
	}

	got := cs.last()
	// Envelope body
	var gotEnv Envelope
	if err := json.Unmarshal(got.body, &gotEnv); err != nil {
		t.Fatalf("bad envelope json: %v", err)
	}
	if gotEnv.Crier.MessageID != "abc123" || gotEnv.Crier.Sender != "agent-a" || gotEnv.Crier.SessionID != "sess-1" {
		t.Fatalf("envelope mismatch: %+v", gotEnv.Crier)
	}
	// Headers
	if got.headers.Get("X-Crier-Event") != "message" {
		t.Fatalf("X-Crier-Event = %q", got.headers.Get("X-Crier-Event"))
	}
	if got.headers.Get("X-Crier-Retry") != "0" {
		t.Fatalf("X-Crier-Retry = %q", got.headers.Get("X-Crier-Retry"))
	}
	// Signature verification
	sig := got.headers.Get("X-Crier-Signature")
	if sig == "" {
		t.Fatal("missing X-Crier-Signature")
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(got.body)
	if !hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil)))) {
		t.Fatal("signature mismatch")
	}
}

// TestClientPost_StatusClasses: 4xx = permanent, 5xx = retryable, 408/429 retryable.
func TestClientPost_StatusClasses(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{200, false}, {201, false}, {400, false}, {404, false},
		{408, true}, {429, true}, {500, true}, {502, true}, {503, true},
	} {
		cs := newCaptureServer(t, tc.status)
		c := NewClient(2*time.Second, nil)
		res := c.Post(testConfig(cs.server.URL), &Envelope{Crier: EnvelopeMeta{Version: 1, Kind: "message"}}, 0)
		if res.Retryable != tc.retryable {
			t.Errorf("status %d: retryable = %v, want %v", tc.status, res.Retryable, tc.retryable)
		}
	}
}

// TestDriver_DeliverQueuesThenRetries: a failing endpoint gets the initial
// POST, the item queues, and redelivery succeeds after recovery (2 message
// POSTs total), then the queue drains.
func TestDriver_DeliverQueuesThenRetries(t *testing.T) {
	cs := newCaptureServer(t, 500)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: 50 * time.Millisecond, ProbeEvery: time.Second,
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return testConfig(cs.server.URL), nil })
	cfg := testConfig(cs.server.URL)

	delivered, err := d.Deliver("agent-b", cfg, &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "m1", Kind: "message"}})
	if err != nil || delivered {
		t.Fatalf("deliver: delivered=%v err=%v (want queued, no err)", delivered, err)
	}
	// Queue length is transient (the redelivery loop drains concurrently) —
	// assert observable end-states instead.

	// Redelivery keeps failing while the endpoint is down: at least 2
	// attempts happen while the item is still queued.
	waitFor(t, "2 failed attempts", func() bool { return cs.messageCount() >= 2 })

	cs.status.Store(200)
	waitFor(t, "successful redelivery", func() bool { return cs.messageCount() >= 3 })
	waitFor(t, "queue drain", func() bool { return d.queue.Len() == 0 })
}

// TestDriver_CircuitBreaker: threshold failures degrade the endpoint; queued
// items are held while degraded; a probe recovers it and the queue drains.
func TestDriver_CircuitBreaker(t *testing.T) {
	cs := newCaptureServer(t, 500)
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: 40 * time.Millisecond,
		ProbeEvery: 40 * time.Millisecond, CircuitThreshold: 4,
	})
	d.Start()
	defer d.Stop()
	d.SetConfigResolver(func(id string) (*Config, error) { return testConfig(cs.server.URL), nil })
	cfg := testConfig(cs.server.URL)

	for i := 0; i < 4; i++ {
		_, _ = d.Deliver("agent-b", cfg, &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "m", Kind: "message"}})
	}
	// Async mode is enqueue-first (fire-and-forget), so the POSTs and their
	// failures accrue in the background drain loop — wait for the circuit to
	// open as an observable end-state.
	waitFor(t, "circuit open after threshold failures", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, deg := d.degraded["agent-b"]
		return deg
	})
	countAtDegrade := cs.messageCount()

	// While degraded: deliveries queue, no NEW message POSTs (probes are
	// the only traffic, and they are not deliveries).
	_, _ = d.Deliver("agent-b", cfg, &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "m2", Kind: "message"}})
	time.Sleep(150 * time.Millisecond)
	if cs.messageCount() != countAtDegrade {
		t.Fatalf("message POSTs while degraded: %d -> %d (want unchanged)", countAtDegrade, cs.messageCount())
	}

	// Recovery: probe succeeds -> drain.
	cs.status.Store(200)
	waitFor(t, "recovery", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, stillDeg := d.degraded["agent-b"]
		return !stillDeg
	})
	waitFor(t, "queue drained after recovery", func() bool { return d.queue.Len() == 0 })
}

// TestClient_BearerAuth: bearer token from env ref is attached.
func TestClient_BearerAuth(t *testing.T) {
	cs := newCaptureServer(t)
	old := lookupEnv
	lookupEnv = func(k string) string { return "tok-123" }
	defer func() { lookupEnv = old }()

	c := NewClient(2*time.Second, nil)
	cfg := testConfig(cs.server.URL)
	cfg.AuthType = AuthBearer
	cfg.AuthValueRef = "env:CR_TEST_TOKEN"
	_ = c.Post(cfg, &Envelope{Crier: EnvelopeMeta{Version: 1, Kind: "message"}}, 0)

	if got := cs.last().headers.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", got)
	}
}

// TestConfig_Validate: registration-time validation rejects bad configs.
func TestConfig_Validate(t *testing.T) {
	good := &Config{URL: "http://x:1/h", DeliveryMode: "async", Retries: 5, TimeoutMs: 30000}
	if err := good.Validate(); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}
	for _, bad := range []*Config{
		{URL: ""},
		{URL: "ftp://x"},
		{URL: "http://x", AuthType: "apikey"},
		{URL: "http://x", AuthType: AuthBearer, AuthValueRef: ""},
		{URL: "http://x", DeliveryMode: "sync-ish"},
		{URL: "http://x", Retries: 11},
		{URL: "http://x", TimeoutMs: 200000},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("invalid config accepted: %+v", bad)
		}
	}
}

// TestEnvelopeBody_NoTrailingGarbage: the envelope marshals as a single clean
// JSON object (a regression guard for the ndjson style bugs of the mesh lane).
func TestEnvelopeBody_NoTrailingGarbage(t *testing.T) {
	env := &Envelope{Crier: EnvelopeMeta{Version: 1, MessageID: "x", Kind: "message"}, Payload: json.RawMessage(`{"a":1}`)}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\n") {
		t.Fatalf("envelope contains newline: %q", b)
	}
}
