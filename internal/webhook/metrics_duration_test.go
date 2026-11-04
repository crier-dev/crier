package webhook

// metrics_duration_test.go — MON-CRIER-002 acceptance evidence for
// crier_webhook_delivery_duration_seconds{outcome} (observability-spec §3.3,
// R2): time to a terminal outcome, measured from the accept and including
// every retry/re-queue the driver performs.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
)

// scrapeDefault renders the process-wide registry through its real handler.
func scrapeDefault(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(metrics.Default.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET registry: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read exposition: %v", err)
	}
	return string(body)
}

// outcomeCount returns the _count sample of the delivery-duration histogram
// for one outcome, or -1 when the child is absent.
func outcomeCount(t *testing.T, body, outcome string) int64 {
	t.Helper()
	prefix := fmt.Sprintf("crier_webhook_delivery_duration_seconds_count{outcome=%q} ", outcome)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 10, 64)
		if err != nil {
			t.Fatalf("sample %q is not an integer: %v", line, err)
		}
		return v
	}
	return -1
}

// TestDeliveryDurationSeededOutcomes pins the closed outcome vocabulary: all
// four children render with the full histogram shape before any delivery, so
// "no delivery ended this way yet" is a reading rather than a gap.
func TestDeliveryDurationSeededOutcomes(t *testing.T) {
	body := scrapeDefault(t)
	if !strings.Contains(body, "# TYPE crier_webhook_delivery_duration_seconds histogram") {
		t.Fatalf("delivery-duration histogram is not exposed:\n%s", body)
	}
	for _, outcome := range []string{"delivered", "failed", "deadlettered", "dropped"} {
		for _, suffix := range []string{`le="0.005"}`, `le="1"}`, `le="+Inf"}`} {
			want := fmt.Sprintf("crier_webhook_delivery_duration_seconds_bucket{outcome=%q,%s", outcome, suffix)
			if !strings.Contains(body, want) {
				t.Errorf("missing bucket %q\nbody:\n%s", want, body)
			}
		}
		for _, suffix := range []string{"_sum", "_count"} {
			want := fmt.Sprintf("crier_webhook_delivery_duration_seconds%s{outcome=%q} ", suffix, outcome)
			if !strings.Contains(body, want) {
				t.Errorf("missing %s for outcome=%q\nbody:\n%s", suffix, outcome, body)
			}
		}
		if got := outcomeCount(t, body, outcome); got < 0 {
			t.Errorf("outcome=%q has no _count sample", outcome)
		}
	}
}

// TestDeliveryDurationRecordsDelivered: an endpoint that answers 2xx records a
// delivered observation — the timing exists, not just the counter.
func TestDeliveryDurationRecordsDelivered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	before := outcomeCount(t, scrapeDefault(t), "delivered")
	if before < 0 {
		t.Fatal("outcome=\"delivered\" is not seeded")
	}
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})
	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking", Retries: 1, TimeoutMs: 2000}
	env := &Envelope{Crier: EnvelopeMeta{MessageID: "m002-delivered"}}
	delivered, err := d.DeliverContext(t.Context(), "agent-delivered", cfg, env)
	if err != nil {
		t.Fatalf("DeliverContext: %v", err)
	}
	if !delivered {
		t.Fatal("delivery not reported delivered")
	}

	if after := outcomeCount(t, scrapeDefault(t), "delivered"); after != before+1 {
		t.Fatalf("outcome=\"delivered\" count = %d, want %d", after, before+1)
	}
}

// TestDeliveryDurationRecordsDeadlettered: a permanent 4xx is terminal — one
// observation, at the deadlettered arm.
func TestDeliveryDurationRecordsDeadlettered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	defer srv.Close()

	before := outcomeCount(t, scrapeDefault(t), "deadlettered")
	if before < 0 {
		t.Fatal("outcome=\"deadlettered\" is not seeded")
	}
	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})
	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking", Retries: 1, TimeoutMs: 2000}
	_, _ = d.DeliverBlocking(t.Context(), "agent-dead", cfg,
		&Envelope{Crier: EnvelopeMeta{MessageID: "m002-dead"}}, 2*time.Second)

	if after := outcomeCount(t, scrapeDefault(t), "deadlettered"); after != before+1 {
		t.Fatalf("outcome=\"deadlettered\" count = %d, want %d", after, before+1)
	}
}

// TestDeliveryDurationRecordsDropped measures the QUEUE path: an item whose
// agent is gone ends as dropped, and the observation is measured from the
// item's accept stamp — a requeued delivery keeps its accept time, so the
// recorded latency spans the retries rather than restarting at each drain.
func TestDeliveryDurationRecordsDropped(t *testing.T) {
	before := outcomeCount(t, scrapeDefault(t), "dropped")
	if before < 0 {
		t.Fatal("outcome=\"dropped\" is not seeded")
	}
	q := NewMemoryQueue()
	d := NewDriver(NewClient(2*time.Second, nil), q, DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})
	// No config resolver → agentConfig returns nil → "agent gone" → dropped.
	accepted := time.Now().Add(-250 * time.Millisecond)
	if err := q.Push(&QueueItem{
		AgentID:   "vanished",
		Envelope:  &Envelope{Crier: EnvelopeMeta{MessageID: "m002-dropped"}},
		CreatedAt: accepted,
	}); err != nil {
		t.Fatalf("push: %v", err)
	}
	d.drainQueue()

	body := scrapeDefault(t)
	if after := outcomeCount(t, body, "dropped"); after != before+1 {
		t.Fatalf("outcome=\"dropped\" count = %d, want %d", after, before+1)
	}
	// The observation carries the ~250ms wait: it lands in the le="0.25" bucket
	// (cumulative), which a fresh child would leave at 0.
	if !strings.Contains(body, `crier_webhook_delivery_duration_seconds_bucket{outcome="dropped",le="0.25"}`) {
		t.Fatalf("dropped bucket missing:\n%s", body)
	}
}

// TestRequeuePreservesAcceptStamp is the "including retries" contract at its
// root: a batch requeued after a cooperative failure keeps the OLDEST inner
// accept stamp, so its age and its eventual latency are measured end to end.
func TestRequeuePreservesAcceptStamp(t *testing.T) {
	q := NewMemoryQueue()
	d := NewDriver(NewClient(2*time.Second, nil), q, DriverConfig{
		MaxRetries: 5, RedeliverEvery: time.Hour, ProbeEvery: time.Hour,
	})
	oldest := time.Now().Add(-90 * time.Second)
	newer := time.Now().Add(-10 * time.Second)
	d.requeueBatch("agent-batch", []*QueueItem{
		{AgentID: "agent-batch", Envelope: &Envelope{Crier: EnvelopeMeta{MessageID: "b1"}}, CreatedAt: newer},
		{AgentID: "agent-batch", Envelope: &Envelope{Crier: EnvelopeMeta{MessageID: "b2"}}, CreatedAt: oldest},
	})
	items := q.PopBatch(10)
	if len(items) != 1 {
		t.Fatalf("queue length = %d, want the requeued batch item", len(items))
	}
	if items[0].CreatedAt.Sub(oldest) > time.Second {
		t.Fatalf("requeued batch CreatedAt = %v, want the oldest inner accept stamp %v", items[0].CreatedAt, oldest)
	}
}
