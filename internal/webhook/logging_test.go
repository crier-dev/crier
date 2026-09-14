package webhook

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/middleware"
)

// captureLogs redirects the default slog logger into a buffer for the duration
// of the test so driver log lines can be asserted (DF-CRIER-141).
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
	})

	return &buf
}

func deliverTestEnv() *Envelope {
	return &Envelope{
		Crier: EnvelopeMeta{
			Version:   1,
			MessageID: "msg-9",
			Kind:      KindMessage,
			Sender:    "agent-sender",
		},
		Payload: []byte(`{"secret":"payload-body"}`),
	}
}

// TestDeliverContextLogsDispatchAndOutcome proves the HTTP deliver path emits
// a dispatch and an outcome line carrying the agent id, the endpoint
// (host+path) and the request correlation id — and that neither the endpoint
// query string nor the payload body is logged.
func TestDeliverContextLogsDispatchAndOutcome(t *testing.T) {
	logs := captureLogs(t)

	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())
	// delivery_mode=blocking takes the immediate-attempt path, so the log
	// lines are emitted synchronously inside this call.
	cfg := &Config{URL: sink.URL + "/hook?token=super-secret", DeliveryMode: "blocking"}

	ctx := middleware.WithRequestID(context.Background(), "req-abc123")
	delivered, err := d.DeliverContext(ctx, "agent-x", cfg, deliverTestEnv())
	if err != nil {
		t.Fatalf("DeliverContext: %v", err)
	}
	if !delivered {
		t.Fatal("delivered = false, want true")
	}

	out := logs.String()
	for _, want := range []string{
		"webhook: delivery dispatched",
		"webhook: delivery delivered",
		"agent=agent-x",
		"endpoint=127.0.0.1:",
		"message_id=msg-9",
		"request_id=req-abc123",
		"status=200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
	for _, leak := range []string{"super-secret", "token=", "payload-body"} {
		if strings.Contains(out, leak) {
			t.Errorf("log %q leaked %q — endpoint queries and payload bodies must never be logged", out, leak)
		}
	}
}

// TestDeliverBlockingLogsRetryAttempt proves a failed attempt is a warn line
// naming the error, and that an in-budget retry is logged with its attempt
// number (the driver backs off ~1s before the second try, so the budget has
// to exceed that for the retry to actually fire).
func TestDeliverBlockingLogsRetryAttempt(t *testing.T) {
	logs := captureLogs(t)

	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sink.Close()

	d := NewDriver(NewClient(2*time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())
	cfg := &Config{URL: sink.URL + "/hook"}

	ctx := middleware.WithRequestID(context.Background(), "req-fail")
	_, err := d.DeliverBlocking(ctx, "agent-y", cfg, deliverTestEnv(), 1200*time.Millisecond)
	if err == nil {
		t.Fatal("DeliverBlocking error = nil, want a timeout error")
	}

	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "webhook: delivery failed") {
		t.Errorf("log %q: want a warn failure line", out)
	}
	if !strings.Contains(out, "agent=agent-y") || !strings.Contains(out, "status=500") {
		t.Errorf("log %q: want agent id and status code", out)
	}
	if !strings.Contains(out, "retry=1") {
		t.Errorf("log %q: want the retry attempt number", out)
	}
	if !strings.Contains(out, "request_id=req-fail") {
		t.Errorf("log %q: want request_id=req-fail", out)
	}
}

// TestDispatchArgsCarryRetryAndHideSecrets pins the shared log attribute set:
// the retry attempt rides only on redeliveries, and the endpoint is rendered
// host+path with no query.
func TestDispatchArgsCarryRetryAndHideSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	logger.Info("dispatch", dispatchArgs("agent-z", &Config{URL: "https://hooks.example.com/a/b?token=zzz"}, "m-1", 1, 2, "req-1")...)

	out := buf.String()
	for _, want := range []string{"retry=2", "message_id=m-1", "endpoint=hooks.example.com/a/b", "messages=1", "request_id=req-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("line %q does not contain %q", out, want)
		}
	}
	if strings.Contains(out, "token") || strings.Contains(out, "zzz") {
		t.Errorf("line %q leaked the endpoint query", out)
	}

	// First attempt: no retry attribute, so the common case stays short.
	buf.Reset()
	logger.Info("dispatch", dispatchArgs("agent-z", &Config{URL: "https://hooks.example.com/a/b"}, "m-1", 1, 0, "")...)
	if strings.Contains(buf.String(), "retry=") {
		t.Errorf("first-attempt line %q carries a retry attribute", buf.String())
	}
}

// TestEndpointLabelDropsQuery proves the log-safe endpoint rendering.
func TestEndpointLabelDropsQuery(t *testing.T) {
	cases := map[string]string{
		"https://hooks.example.com/a/b?token=zzz": "hooks.example.com/a/b",
		"http://127.0.0.1:9/hook":                 "127.0.0.1:9/hook",
		"not-a-url":                               "",
	}
	for in, want := range cases {
		if got := endpointLabel(in); got != want {
			t.Errorf("endpointLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
