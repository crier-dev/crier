package webhook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeliverBlockingGateWaitUsesBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	d := NewDriver(NewClient(time.Second, nil), NewMemoryQueue(), DefaultDriverConfig())
	// Hold the same target/session lane, exactly as an earlier slow request
	// does. The tested call must refuse without waiting for this lock to lift.
	release := d.gate.acquire("5:agentsession")
	defer release()
	start := time.Now()
	_, err := d.DeliverBlocking(context.Background(), "agent", &Config{URL: srv.URL},
		&Envelope{Crier: EnvelopeMeta{SessionID: "session"}, Payload: []byte(`{}`)}, 40*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "timeout")
	require.Less(t, time.Since(start), time.Second)
	require.Zero(t, calls.Load(), "expired gate wait must not POST")
}

func TestSessionGateCanceledWaitCompletes(t *testing.T) {
	g := newSessionGate()
	release := g.acquire("held")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		unlock, err := g.acquireContext(ctx, "held")
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		require.True(t, errors.Is(err, context.Canceled))
	case <-time.After(time.Second):
		t.Fatal("canceled gate waiter leaked")
	}
	release()
	unlock, err := g.acquireContext(context.Background(), "held")
	require.NoError(t, err, "cancellation must not strand the lock")
	unlock()
}
