package dispatchlive

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A live pass reports the caller's boundaries — planned targets, budget and the
// pass index — plus the attempts the runtime actually reported, with elapsed
// measured by the registry's own clock.
func TestLivePassReportsBoundariesAndAttempts(t *testing.T) {
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry()
	reg.now = func() time.Time { return base.Add(2100 * time.Millisecond) }
	reg.Begin("room", "msg-1", 1, []string{"dead-bot", "live-bot", "dead-bot"}, 30000, base)

	// Nothing reported yet: every PLANNED target is present with no attempt
	// evidence, and the pass is still live.
	pass := reg.Live("room", "msg-1")
	require.NotNil(t, pass)
	require.Equal(t, 1, pass.Attempt)
	require.Equal(t, base, pass.StartedAt)
	require.EqualValues(t, 2100, pass.ElapsedMs)
	require.Equal(t, 30000, pass.BudgetMs)
	require.Equal(t, []Target{{Target: "dead-bot"}, {Target: "live-bot"}}, pass.Targets)

	attempted := base.Add(150 * time.Millisecond)
	reg.Observe(Attempt{SessionID: "room", MessageID: "msg-1", AgentID: "dead-bot", Attempt: 2,
		At: attempted, Status: 0, Class: ClassUnreachable,
		Reason: "dial tcp 127.0.0.1:39999: connect: connection refused"})

	pass = reg.Live("room", "msg-1")
	require.Len(t, pass.Targets, 2)
	require.Equal(t, Target{Target: "dead-bot", Attempts: 2, LastAttemptAt: &attempted,
		Class:  ClassUnreachable,
		Reason: "dial tcp 127.0.0.1:39999: connect: connection refused"}, pass.Targets[0])
	require.Equal(t, Target{Target: "live-bot"}, pass.Targets[1], "an unreported target keeps its planned slot")

	// A target the live pass is not delivering to is never invented.
	reg.Observe(Attempt{SessionID: "room", MessageID: "msg-1", AgentID: "stranger", Attempt: 1, Class: ClassError})
	require.Equal(t, []Target{{Target: "dead-bot", Attempts: 2, LastAttemptAt: &attempted,
		Class:  ClassUnreachable,
		Reason: "dial tcp 127.0.0.1:39999: connect: connection refused"}, {Target: "live-bot"}}, reg.Live("room", "msg-1").Targets)

	// End removes the pass: after the budget a reader gets the durable record,
	// never a stale indicator.
	reg.End("room", "msg-1")
	require.Nil(t, reg.Live("room", "msg-1"))
}

// The registry only ever describes passes this process is running: an attempt
// for an unknown pass, an unknown session or a nil registry is discarded rather
// than materialising in-flight state.
func TestObserveWithoutAPassIsDiscarded(t *testing.T) {
	reg := NewRegistry()
	reg.Observe(Attempt{SessionID: "room", MessageID: "msg-1", AgentID: "bot", Attempt: 1, Class: ClassUnreachable})
	require.Nil(t, reg.Live("room", "msg-1"))

	reg.Begin("room", "msg-1", 1, []string{"bot"}, 30000, time.Now())
	reg.Observe(Attempt{MessageID: "msg-1", AgentID: "bot", Attempt: 1})
	reg.Observe(Attempt{SessionID: "room", AgentID: "bot", Attempt: 1})
	reg.Observe(Attempt{SessionID: "other", MessageID: "msg-1", AgentID: "bot", Attempt: 1})
	require.Equal(t, []Target{{Target: "bot"}}, reg.Live("room", "msg-1").Targets)

	var nilReg *Registry
	nilReg.Begin("room", "msg-1", 1, []string{"bot"}, 1, time.Time{})
	nilReg.Observe(Attempt{SessionID: "room", MessageID: "msg-1", AgentID: "bot"})
	nilReg.End("room", "msg-1")
	require.Nil(t, nilReg.Live("room", "msg-1"))
}

// A pass is keyed by (session, message): two sessions delivering the same
// message id do not share a live view.
func TestLiveIsScopedToSessionAndMessage(t *testing.T) {
	reg := NewRegistry()
	reg.Begin("room-a", "msg-1", 1, []string{"bot"}, 1000, time.Now())
	reg.Begin("room-b", "msg-1", 1, []string{"bot"}, 2000, time.Now())
	reg.Observe(Attempt{SessionID: "room-b", MessageID: "msg-1", AgentID: "bot", Attempt: 1, Class: ClassTimeout})

	require.EqualValues(t, 1000, reg.Live("room-a", "msg-1").BudgetMs)
	require.Equal(t, []Target{{Target: "bot"}}, reg.Live("room-a", "msg-1").Targets)
	require.Equal(t, ClassTimeout, reg.Live("room-b", "msg-1").Targets[0].Class)
	require.Nil(t, reg.Live("room-a", "msg-2"))
}

// The published shape is the contract the transcript's `dispatch_live` field and
// the operator panel read; pin the wire names, not just the Go fields.
func TestPassJSONShape(t *testing.T) {
	base := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry()
	reg.now = func() time.Time { return base.Add(2 * time.Second) }
	reg.Begin("room", "msg-1", 1, []string{"dead-bot"}, 30000, base)
	at := base.Add(100 * time.Millisecond)
	reg.Observe(Attempt{SessionID: "room", MessageID: "msg-1", AgentID: "dead-bot", Attempt: 1, At: at,
		Class: ClassUnreachable, Reason: "dial tcp 127.0.0.1:39999: connect: connection refused"})

	raw, err := json.Marshal(reg.Live("room", "msg-1"))
	require.NoError(t, err)
	require.JSONEq(t, `{
		"attempt": 1,
		"started_at": "2026-10-19T12:00:00Z",
		"elapsed_ms": 2000,
		"budget_ms": 30000,
		"targets": [{
			"target": "dead-bot",
			"attempts": 1,
			"last_attempt_at": "2026-10-19T12:00:00.1Z",
			"class": "unreachable",
			"reason": "dial tcp 127.0.0.1:39999: connect: connection refused"
		}]
	}`, string(raw), "an answered attempt omits class/reason/status; an unreported target omits everything but its name")
}

// The registry is read by HTTP handlers and written by delivery goroutines, so
// the snapshot must be race-free (exercised under -race in the guard's suite).
func TestRegistryIsConcurrencySafe(t *testing.T) {
	reg := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				reg.Begin("room", "msg", n+1, []string{"a", "b"}, 30000, time.Now())
				reg.Observe(Attempt{SessionID: "room", MessageID: "msg", AgentID: "a", Attempt: n + 1, Class: ClassTimeout})
				_ = reg.Live("room", "msg")
				reg.End("room", "msg")
			}
		}(i)
	}
	wg.Wait()
	require.Nil(t, reg.Live("room", "msg"))
}
