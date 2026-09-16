package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// INT-CI-004 — the blocking-delivery budget must bound the ATTEMPT, not just
// the window between attempts.
//
// DeliverBlocking enforces the caller's budget with a pre-attempt deadline
// check (driver.go) and a budget-clipped backoff, but the attempt itself was
// bounded ONLY by the driver-global client timeout (the per-agent
// Config.TimeoutMs never reaches the client). With a global timeout LARGER
// than the endpoint's hang, a delivery carrying timeout_ms=100 overran its
// budget and the endpoint's late 2xx was reported back to the caller as a
// SUCCESS — the 504-vs-200 photo finish that TestDeliver_Blocking_TimeoutIs504
// loses under parallel-package load (make test-short / CI).
//
// These tests pin both halves: a late 2xx must NOT become a reply, and the
// resulting error must stay a TIMEOUT (never ErrPermanent, which the deliver
// API maps to 502 instead of 504).

// lateSuccessEndpoint answers only after delay, then writes 200 (and body
// when non-empty) — the endpoint that overruns the caller's budget but
// eventually "succeeds".
func lateSuccessEndpoint(t *testing.T, delay time.Duration, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// budgetTestEnv is a minimal blocking-mode envelope for the budget tests.
func budgetTestEnv(messageID string) *Envelope {
	return &Envelope{
		Crier:   EnvelopeMeta{Version: 1, MessageID: messageID, Kind: KindMessage, Sender: "agent-sender"},
		Payload: json.RawMessage(`{"ping":1}`),
	}
}

// TestDeliverBlocking_AttemptBoundedByRemainingBudget is the INT-CI-004
// detector. The driver-global client timeout (10s) is deliberately LARGER
// than the endpoint's hang (2s), so the ONLY thing that can stop the attempt
// at the caller's 100ms budget is the budget itself.
//
// Unfixed: the call waits out the 2s hang and returns the endpoint's 2xx as a
// successful reply. Fixed: it returns a timeout error and no reply, well
// inside the hang.
func TestDeliverBlocking_AttemptBoundedByRemainingBudget(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"late 200 with a reply body", "late-reply"},
		{"late 200 with an empty body", ""}, // the shape TestDeliver_Blocking_TimeoutIs504 races on
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := lateSuccessEndpoint(t, 2*time.Second, tc.body)
			cfg := &Config{URL: srv.URL, DeliveryMode: "blocking", TimeoutMs: 100}
			d := NewDriver(NewClient(10*time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())

			start := time.Now()
			reply, err := d.DeliverBlocking(context.Background(), "agent-hang", cfg, budgetTestEnv("msg-budget"), 100*time.Millisecond)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("100ms budget against a 2s hang: err = nil, reply = %q after %s — the attempt was bounded only by the 10s driver-global client timeout, so an over-budget endpoint 2xx was reported as a SUCCESS",
					reply, elapsed.Round(time.Millisecond))
			}
			if reply != nil {
				t.Fatalf("budget overrun returned reply %q; want no reply on a timeout", reply)
			}
			if !strings.Contains(err.Error(), "timed out") {
				t.Fatalf("err = %v, want a timeout/budget-exhaustion message", err)
			}
			if errors.Is(err, ErrPermanent) {
				t.Fatalf("err = %v wraps ErrPermanent; a budget overrun must stay a TIMEOUT (deliver API 504), not a permanent failure (502)", err)
			}
			// The attempt itself must be bounded: waiting out the 2s hang
			// would mean the budget never reached the request path.
			if elapsed > time.Second {
				t.Fatalf("call took %s with a 100ms budget — the attempt is not bounded by the remaining budget", elapsed.Round(time.Millisecond))
			}
		})
	}
}

// TestDeliverBlocking_FastSuccessStillReturnsReply is the control: an endpoint
// that answers inside the budget must be unaffected — reply returned, no
// error. A budget bound that also broke the happy path would be a regression,
// not a fix.
func TestDeliverBlocking_FastSuccessStillReturnsReply(t *testing.T) {
	srv := lateSuccessEndpoint(t, 0, "quick-reply")
	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking", TimeoutMs: 100}
	d := NewDriver(NewClient(10*time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())

	reply, err := d.DeliverBlocking(context.Background(), "agent-fast", cfg, budgetTestEnv("msg-fast"), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("in-budget delivery: err = %v, want nil", err)
	}
	if string(reply) != `"quick-reply"` {
		t.Fatalf("reply = %q, want %q", reply, `"quick-reply"`)
	}
}

// TestDeliverBlocking_PermanentRejectionStaysPermanent guards the other side
// of the classification: a non-retryable 4xx answered INSIDE the budget is
// still ErrPermanent (deliver API 502), so the new attempt bound cannot turn
// a permanent rejection into a timeout.
func TestDeliverBlocking_PermanentRejectionStaysPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking", TimeoutMs: 100}
	d := NewDriver(NewClient(10*time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())

	reply, err := d.DeliverBlocking(context.Background(), "agent-4xx", cfg, budgetTestEnv("msg-4xx"), 100*time.Millisecond)
	if err == nil {
		t.Fatalf("400 rejection: err = nil, reply = %q; want ErrPermanent", reply)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent (deliver API 502)", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v; a 400 answered inside the budget must not be reported as a timeout", err)
	}
}

// TestDeliverBlocking_Unmappable2xxStaysPermanent is the second permanent
// case: a 2xx whose body the agent's reply schema cannot map is permanent
// (502), not a timeout — unchanged by the attempt bound.
func TestDeliverBlocking_Unmappable2xxStaysPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &Config{URL: srv.URL, DeliveryMode: "blocking", SchemaTemplate: "openai-compatible", TimeoutMs: 100}
	d := NewDriver(NewClient(10*time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())

	reply, err := d.DeliverBlocking(context.Background(), "agent-schema", cfg, budgetTestEnv("msg-schema"), 100*time.Millisecond)
	if err == nil {
		t.Fatalf("unmappable 2xx: err = nil, reply = %q; want ErrPermanent", reply)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent (deliver API 502)", err)
	}
}
