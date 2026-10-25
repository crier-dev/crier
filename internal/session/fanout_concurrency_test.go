package session

import (
	"context"
	"encoding/json"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/stretchr/testify/require"
)

// This dispatcher models the real dispatcher's per-target deadline without
// network I/O. All goroutines, channels and timers belong to the synctest bubble:
// host scheduling cannot consume any of the simulated delivery budget.
type joinedBudgetDeliverer struct {
	started, completed atomic.Int32
	wrongBudget        atomic.Bool
	release            chan struct{}
	allHang            bool
}

func (*joinedBudgetDeliverer) Deliver(string, *registry.InboxEntry) error {
	panic("fan-out must use the guarded dispatcher")
}

func (d *joinedBudgetDeliverer) DispatchDelivery(ctx context.Context, target string, req registry.DeliveryRequest) (registry.DeliveryResult, error) {
	if req.TimeoutMs != 400 {
		d.wrongBudget.Store(true)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	defer cancel()
	d.started.Add(1)
	defer d.completed.Add(1)
	select {
	case <-d.release:
	case <-ctx.Done():
	}
	if d.allHang || target == "b-hang" {
		<-ctx.Done()
		return registry.DeliveryResult{}, &webhook.BlockingDeliveryError{
			Attempts: 1, Budget: 400 * time.Millisecond, BudgetExhausted: true,
			Cut: true, Cause: ctx.Err(),
		}
	}
	return registry.DeliveryResult{Transport: "webhook", DeliveryMode: "blocking", Reply: json.RawMessage(`{"reply":"fast answer"}`)}, nil
}

// The HTTP fixtures in cmd/server retain the real boot/budget/persistence checks.
// Concurrency and join ordering are proven here without an HTTP round-trip SLA:
// every attempt starts before any is released, and neither outcomes nor answers
// are published while even one attempt is still waiting for its budget.
func TestFanoutBudgetJoinsConcurrentAttempts(t *testing.T) {
	t.Parallel()
	for _, allHang := range []bool{false, true} {
		t.Run("all_hang="+strconv.FormatBool(allHang), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const budget = 400 * time.Millisecond
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel() // Also unblocks a deliberately broken serialized fan-out.
				d := &joinedBudgetDeliverer{release: make(chan struct{}), allHang: allHang}
				msg := &Message{ID: "message", SessionID: "session", ThreadID: "thread", Payload: json.RawMessage(`{}`)}
				var replies []string
				var replyCalls atomic.Int32
				var prematureReply bool
				done := make(chan error, 1)
				start := time.Now()
				go func() {
					_, err := fanoutWithReplies(ctx, d, msg, []string{"c-fast", "b-hang", "a-fast"}, nil, 400, "", func(target string, _ json.RawMessage) error {
						prematureReply = prematureReply || d.completed.Load() != 3
						replyCalls.Add(1)
						replies = append(replies, target)
						return nil
					})
					done <- err
				}()
				synctest.Wait()
				require.EqualValues(t, 3, d.started.Load(), "all attempts must start before any attempt is released")
				require.Zero(t, d.completed.Load())
				require.Zero(t, replyCalls.Load())
				require.Empty(t, done, "fan-out cannot publish outcomes before attempts finish")
				close(d.release)
				synctest.Wait()
				wantFast := int32(2)
				if allHang {
					wantFast = 0
				}
				require.Equal(t, wantFast, d.completed.Load())
				require.Zero(t, replyCalls.Load(), "even fast answers must wait for the join")
				require.Empty(t, done, "a pending budget must hold the outcome return")

				// Observe only atomics/channels until done: advancing fake time
				// is not a happens-before edge for reading msg or the reply slice.
				// Advance FAKE time, not wall time. A serialized implementation has
				// already failed the start barrier; an early return fails above.
				time.Sleep(budget)
				synctest.Wait()
				require.Len(t, done, 1, "fan-out must settle when every attempt has finished")
				require.NoError(t, <-done)
				require.EqualValues(t, 3, d.completed.Load())
				require.False(t, d.wrongBudget.Load())
				require.False(t, prematureReply, "answers are recorded only after all attempts finish")
				require.Len(t, msg.Outcomes, 3)
				for i, target := range []string{"a-fast", "b-hang", "c-fast"} {
					require.Equal(t, target, msg.Outcomes[i].Target)
					want := OutcomeDelivered
					if allHang || target == "b-hang" {
						want = OutcomeBudgetCut
					}
					require.Equal(t, want, msg.Outcomes[i].Outcome)
				}
				if allHang {
					require.Empty(t, replies)
				} else {
					require.Equal(t, []string{"a-fast", "c-fast"}, replies)
				}
				t.Logf("started=%d completed=%d replies=%d simulated_elapsed=%s all_hang=%t", d.started.Load(), d.completed.Load(), len(replies), time.Since(start), allHang)
			})
		})
	}
}
