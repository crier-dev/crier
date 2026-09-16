package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// INT-CI-004 — the context plumbing that lets DeliverBlocking bound an
// attempt. These tests pin the two properties the driver depends on:
//
//  1. the ctx really reaches the outbound request (cancelling it aborts the
//     attempt instead of letting it run to the client-global timeout), and
//  2. a cancelled/deadline-exceeded attempt is classified RETRYABLE — the
//     same mapping as a network timeout — so DeliverBlocking backs off and
//     exhausts its budget (deliver API 504) instead of short-circuiting on
//     ErrPermanent (502).
//
// Post/PostBatch keep their no-context signatures and delegate with
// context.Background(), so existing callers and tests compile unchanged —
// asserted by the delegation-equivalence test at the end.

func ctxTestConfig(t *testing.T) *Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok-body"))
	}))
	t.Cleanup(srv.Close)
	return &Config{URL: srv.URL, DeliveryMode: "blocking", TimeoutMs: 100}
}

// TestPostContext_CancelledContextIsRetryableTransportFailure drives the ctx
// into the request path: an already-cancelled context must make the POST fail
// as a retryable transport error (never ErrPermanent), without waiting for
// the endpoint.
func TestPostContext_CancelledContextIsRetryableTransportFailure(t *testing.T) {
	cfg := ctxTestConfig(t)
	c := NewClient(10*time.Second, nil) // far larger than the test's patience

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	start := time.Now()
	res := c.PostContext(ctx, cfg, budgetTestEnv("msg-ctx"), 0)
	elapsed := time.Since(start)

	if res.Err == nil {
		t.Fatalf("cancelled ctx: Err = nil, status = %d — the context is not wired into the request", res.StatusCode)
	}
	if !res.Retryable {
		t.Fatalf("cancelled ctx: result = %+v, want Retryable true (a ctx deadline must map like a network timeout, so blocking delivery backs off rather than reporting a permanent failure)", res)
	}
	if res.StatusCode != 0 {
		t.Fatalf("cancelled ctx: status = %d, want 0 (no response)", res.StatusCode)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("cancelled ctx: err = %v, want it to wrap context.Canceled", res.Err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cancelled ctx took %s — the request was not aborted by the context", elapsed)
	}

	// A deadline (the shape DeliverBlocking uses) classifies identically.
	dctx, dcancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer dcancel()
	dres := c.PostContext(dctx, cfg, budgetTestEnv("msg-ctx"), 0)
	if dres.Err == nil || !dres.Retryable {
		t.Fatalf("expired deadline: result = %+v, want Err != nil and Retryable true", dres)
	}
	if !errors.Is(dres.Err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline: err = %v, want it to wrap context.DeadlineExceeded", dres.Err)
	}
}

// TestPostBatchContext_CancelledContextIsRetryableTransportFailure is the
// batch surface of the same contract (PostBatchContext shares postBody).
func TestPostBatchContext_CancelledContextIsRetryableTransportFailure(t *testing.T) {
	cfg := ctxTestConfig(t)
	c := NewClient(10*time.Second, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := c.PostBatchContext(ctx, cfg, []*Envelope{budgetTestEnv("msg-batch")}, 0)
	if res.Err == nil {
		t.Fatalf("cancelled ctx: Err = nil, status = %d — the batch path is not context-aware", res.StatusCode)
	}
	if !res.Retryable {
		t.Fatalf("cancelled ctx: result = %+v, want Retryable true", res)
	}

	// The empty-slice guard still short-circuits before any request, and is
	// NOT retryable (a local misuse, not a transport failure).
	if res := c.PostBatchContext(context.Background(), cfg, nil, 0); res.Err == nil || res.Retryable {
		t.Fatalf("no envelopes: result = %+v, want a non-retryable local error", res)
	}
}

// TestPost_DelegatesToPostContext proves the additive refactor left the
// existing, context-free entry points behaviorally identical: Post and Post
// against a background context produce the same status and body, and
// PostBatch still delivers a batch.
func TestPost_DelegatesToPostContext(t *testing.T) {
	cfg := ctxTestConfig(t)
	c := NewClient(10*time.Second, nil)

	viaPost := c.Post(cfg, budgetTestEnv("msg-legacy"), 0)
	viaCtx := c.PostContext(context.Background(), cfg, budgetTestEnv("msg-legacy"), 0)
	if viaPost.Err != nil || viaCtx.Err != nil {
		t.Fatalf("Post = %+v, PostContext = %+v; want both to succeed against a live endpoint", viaPost, viaCtx)
	}
	if viaPost.StatusCode != viaCtx.StatusCode || string(viaPost.Body) != string(viaCtx.Body) {
		t.Fatalf("Post = (%d, %q), PostContext = (%d, %q); want identical results",
			viaPost.StatusCode, viaPost.Body, viaCtx.StatusCode, viaCtx.Body)
	}

	batch := c.PostBatch(cfg, []*Envelope{budgetTestEnv("msg-legacy-batch")}, 0)
	if batch.Err != nil || batch.StatusCode != http.StatusOK {
		t.Fatalf("PostBatch = %+v, want 200 with no error", batch)
	}
}
