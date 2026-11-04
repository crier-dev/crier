package registry

// metrics_deliver_test.go — MON-CRIER-002 acceptance evidence for
// crier_inbox_deliver_duration_seconds{transport} (observability-spec §3.3,
// R2): the store-write vs webhook-accept cost split, recorded at the Dispatch
// choke point that both delivery callers share.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/webhook"
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

// deliverCount returns the _count sample of the transport histogram for one
// transport, or -1 when the child is absent.
func deliverCount(t *testing.T, body, transport string) int64 {
	t.Helper()
	prefix := fmt.Sprintf("crier_inbox_deliver_duration_seconds_count{transport=%q} ", transport)
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

// TestDeliverDurationSeededTransports: both arms of the closed transport enum
// render with the full histogram shape before any delivery happens.
func TestDeliverDurationSeededTransports(t *testing.T) {
	body := scrapeDefault(t)
	if !strings.Contains(body, "# TYPE crier_inbox_deliver_duration_seconds histogram") {
		t.Fatalf("deliver-duration histogram is not exposed:\n%s", body)
	}
	for _, transport := range []string{"inbox", "webhook"} {
		for _, suffix := range []string{`le="0.005"}`, `le="30"}`, `le="+Inf"}`} {
			want := fmt.Sprintf("crier_inbox_deliver_duration_seconds_bucket{transport=%q,%s", transport, suffix)
			if !strings.Contains(body, want) {
				t.Errorf("missing bucket %q\nbody:\n%s", want, body)
			}
		}
		for _, suffix := range []string{"_sum", "_count"} {
			want := fmt.Sprintf("crier_inbox_deliver_duration_seconds%s{transport=%q} ", suffix, transport)
			if !strings.Contains(body, want) {
				t.Errorf("missing %s for transport=%q\nbody:\n%s", suffix, transport, body)
			}
		}
		if got := deliverCount(t, body, transport); got < 0 {
			t.Errorf("transport=%q has no _count sample", transport)
		}
	}
}

// TestDeliverDurationRecordsInboxTransport drives a real DeliverComposed (the
// in-process delivery path, which funnels through Dispatch) and proves the
// store-write arm advances.
func TestDeliverDurationRecordsInboxTransport(t *testing.T) {
	before := deliverCount(t, scrapeDefault(t), "inbox")
	if before < 0 {
		t.Fatal("transport=\"inbox\" is not seeded")
	}

	store := NewMemoryStore()
	if err := store.Register(&Agent{ID: "m002-inbox", PublicKey: HexKey("aa")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := NewHandler(store)
	res, err := h.DeliverComposed(context.Background(), "m002-inbox", "", &InboxEntry{Payload: []byte(`{"text":"hi"}`)})
	if err != nil {
		t.Fatalf("DeliverComposed: %v", err)
	}
	if res.Transport != "inbox" {
		t.Fatalf("result transport = %q, want inbox", res.Transport)
	}

	if after := deliverCount(t, scrapeDefault(t), "inbox"); after != before+1 {
		t.Fatalf("crier_inbox_deliver_duration_seconds_count{transport=\"inbox\"} = %d, want %d", after, before+1)
	}
}

// TestDeliverDurationRecordsWebhookTransport: the same choke point labels a
// push accept as transport="webhook" — the split the spec asks for ("store
// write vs webhook accept") only means something if both arms are real.
func TestDeliverDurationRecordsWebhookTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	before := deliverCount(t, scrapeDefault(t), "webhook")
	if before < 0 {
		t.Fatal("transport=\"webhook\" is not seeded")
	}

	store := NewMemoryStore()
	if err := store.Register(&Agent{
		ID:        "m002-hook",
		PublicKey: HexKey("bb"),
		Webhook:   &webhook.Config{URL: srv.URL, DeliveryMode: "async", Retries: 1, TimeoutMs: 1000},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	h := NewHandler(store)
	h.SetWebhookDriver(webhook.NewDriver(webhook.NewClient(time.Second, nil), webhook.NewMemoryQueue(), webhook.DefaultDriverConfig()))

	res, err := h.DeliverComposed(context.Background(), "m002-hook", "", &InboxEntry{Payload: []byte(`{"text":"push"}`)})
	if err != nil {
		t.Fatalf("DeliverComposed: %v", err)
	}
	if res.Transport != "webhook" {
		t.Fatalf("result transport = %q, want webhook", res.Transport)
	}

	if after := deliverCount(t, scrapeDefault(t), "webhook"); after != before+1 {
		t.Fatalf("crier_inbox_deliver_duration_seconds_count{transport=\"webhook\"} = %d, want %d", after, before+1)
	}
}
