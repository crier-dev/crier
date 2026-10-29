package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// DF-CRIER-335: GET /status/completed states the relay's completion definition
// once — completed = (pending == 0) AND (leased == 0) — and the background
// observer logs the TRANSITIONS. These tests pin the truth table (including the
// arm that matters most: an unmeasured queue is never "complete"), the wire
// shape a panel renders, and the transition logic the loop logs on.

// depthReader returns a fixed measurement, and lets a test change it between
// samples — the watcher must notice the change, not re-report a cached verdict.
type depthReader struct {
	mu    sync.Mutex
	depth registry.QueueDepth
	ok    bool
	reads int
}

func (r *depthReader) read() (registry.QueueDepth, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	return r.depth, r.ok
}

func (r *depthReader) set(pending, leased int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.depth = registry.QueueDepth{Pending: pending, Leased: leased}
	r.ok = true
}

func (r *depthReader) unmeasure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ok = false
}

func (r *depthReader) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// TestDeriveCompletionTruthTable pins the definition. Every row names the two
// conditions it is derived from, so a change to either comparison fails here
// rather than in a browser.
func TestDeriveCompletionTruthTable(t *testing.T) {
	tests := []struct {
		name     string
		depth    *statusQueueDepth
		want     bool
		inboxOK  bool
		leasesOK bool
		reason   string
	}{
		{name: "empty inbox, no leases", depth: &statusQueueDepth{},
			want: true, inboxOK: true, leasesOK: true},
		{name: "one pending message is not complete",
			depth: &statusQueueDepth{Pending: 1},
			want:  false, inboxOK: false, leasesOK: true, reason: completionReasonInboxPending},
		{name: "a live lease is not complete",
			depth: &statusQueueDepth{Pending: 1, Leased: 1},
			want:  false, inboxOK: false, leasesOK: false, reason: completionReasonPendingAndLeased},
		{name: "a lease reported without queued messages is still not complete",
			depth: &statusQueueDepth{Pending: 0, Leased: 1},
			want:  false, inboxOK: true, leasesOK: false, reason: completionReasonLeasesOutstanding},
		{name: "an age alone does not block completion",
			depth: &statusQueueDepth{OldestAgeS: 640},
			want:  true, inboxOK: true, leasesOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveCompletion(tc.depth)
			assert.Equal(t, tc.want, got.Completed, "completed")
			assert.Equal(t, tc.inboxOK, got.InboxEmpty, "inbox_empty")
			assert.Equal(t, tc.leasesOK, got.NoPendingLeases, "no_pending_leases")
			assert.Equal(t, tc.reason, got.Reason, "reason")
			// The verdict carries the measurement it was derived from, never a
			// re-rendered copy with different numbers.
			assert.Same(t, tc.depth, got.QueueDepth)
		})
	}
}

// TestDeriveCompletionUnmeasuredIsNeverComplete is the honesty arm: a store that
// cannot report a depth must not be able to answer "done". Both conditions are
// false, the measurement is null, and the reason names the absence.
func TestDeriveCompletionUnmeasuredIsNeverComplete(t *testing.T) {
	got := deriveCompletion(nil)
	assert.False(t, got.Completed, "an unmeasured queue must never be complete")
	assert.False(t, got.InboxEmpty)
	assert.False(t, got.NoPendingLeases)
	assert.Nil(t, got.QueueDepth)
	assert.Equal(t, completionReasonUnmeasured, got.Reason)
}

// TestStatusCompletedHandlerWire pins the served body field by field — the panel
// reads these names — and that the verdict is live (no caching) and honest for a
// server with no depth reader.
func TestStatusCompletedHandlerWire(t *testing.T) {
	t.Run("empty queue", func(t *testing.T) {
		reader := &depthReader{}
		reader.set(0, 0)
		rec := httptest.NewRecorder()
		newStatusCompletedHandler(reader.read)(rec, httptest.NewRequest(http.MethodGet, "/status/completed", nil))

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, true, body["completed"])
		assert.Equal(t, true, body["inbox_empty"])
		assert.Equal(t, true, body["no_pending_leases"])
		_, hasReason := body["reason"]
		assert.False(t, hasReason, "a complete queue explains nothing: reason must be omitted")
		depth, ok := body["queue_depth"].(map[string]any)
		require.True(t, ok, "queue_depth must be the measurement object: %s", rec.Body.String())
		assert.EqualValues(t, 0, depth["pending"])
		assert.EqualValues(t, 0, depth["leased"])
		assert.EqualValues(t, 0, depth["oldest_age_s"])
	})

	t.Run("work outstanding", func(t *testing.T) {
		reader := &depthReader{}
		reader.set(3, 2)
		rec := httptest.NewRecorder()
		newStatusCompletedHandler(reader.read)(rec, httptest.NewRequest(http.MethodGet, "/status/completed", nil))

		var body map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, false, body["completed"])
		assert.Equal(t, false, body["inbox_empty"])
		assert.Equal(t, false, body["no_pending_leases"])
		assert.Equal(t, completionReasonPendingAndLeased, body["reason"])
		depth, ok := body["queue_depth"].(map[string]any)
		require.True(t, ok)
		assert.EqualValues(t, 3, depth["pending"])
		assert.EqualValues(t, 2, depth["leased"])
	})

	t.Run("no reader wired", func(t *testing.T) {
		rec := httptest.NewRecorder()
		newStatusCompletedHandler(nil)(rec, httptest.NewRequest(http.MethodGet, "/status/completed", nil))

		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
		assert.Equal(t, "false", string(raw["completed"]))
		assert.Equal(t, "null", string(raw["queue_depth"]), "an unmeasured depth is null, never 0")
		assert.Equal(t, `"`+completionReasonUnmeasured+`"`, string(raw["reason"]))
	})

	t.Run("verdict is read per request", func(t *testing.T) {
		reader := &depthReader{}
		reader.set(0, 0)
		handler := newStatusCompletedHandler(reader.read)
		for _, want := range []bool{true, false, true} {
			if want {
				reader.set(0, 0)
			} else {
				reader.set(1, 0)
			}
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodGet, "/status/completed", nil))
			var body statusCompletion
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, want, body.Completed)
		}
		assert.Equal(t, 3, reader.readCount())
	})
}

// TestStatusCompletedRouteIsGetOnly mounts the handler exactly as cmd/server
// does, so a non-GET is refused by the router instead of reaching the handler.
func TestStatusCompletedRouteIsGetOnly(t *testing.T) {
	reader := &depthReader{}
	reader.set(0, 0)
	r := mux.NewRouter()
	r.HandleFunc("/status/completed", newStatusCompletedHandler(reader.read)).Methods(http.MethodGet)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status/completed", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/status/completed", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// logBuffer is a slog sink that records the messages the watcher emits.
type logBuffer struct {
	mu      sync.Mutex
	records []slog.Record
}

func (b *logBuffer) Enabled(context.Context, slog.Level) bool { return true }

func (b *logBuffer) Handle(_ context.Context, r slog.Record) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records = append(b.records, r.Clone())
	return nil
}

func (b *logBuffer) WithAttrs([]slog.Attr) slog.Handler { return b }

func (b *logBuffer) WithGroup(string) slog.Handler { return b }

func (b *logBuffer) messages() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.records))
	for _, r := range b.records {
		out = append(out, r.Message)
	}
	return out
}

// recordsCopy returns the captured records so a test can assert on the FIELDS a
// line carries, not only its text.
func (b *logBuffer) recordsCopy() []slog.Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]slog.Record(nil), b.records...)
}

// TestCompletionWatcherLogsTransitions drives the observer through the real
// sequence a relay takes: drained → a delivery → drained again. It pins that
// ENTERING completion is logged once (not once per interval), that LEAVING it is
// logged, and that a repeated unchanged verdict stays silent.
func TestCompletionWatcherLogsTransitions(t *testing.T) {
	reader := &depthReader{}
	reader.set(0, 0)
	sink := &logBuffer{}
	watcher := newCompletionWatcher(reader.read, slog.New(sink))

	// First observation already complete: logged (an operator wants "nothing
	// queued" as soon as the process can say it).
	state, event := watcher.sample()
	assert.True(t, state.Completed)
	assert.Equal(t, "complete", event)

	// Same verdict again: silent.
	_, event = watcher.sample()
	assert.Empty(t, event, "an unchanged verdict must not re-log")

	// A delivery arrives: the queue leaves completion.
	reader.set(2, 0)
	state, event = watcher.sample()
	assert.False(t, state.Completed)
	assert.Equal(t, "incomplete", event)

	// Leased but still queued: still incomplete, still silent (no flapping).
	reader.set(2, 2)
	_, event = watcher.sample()
	assert.Empty(t, event)

	// Drained again: completion is re-entered and logged.
	reader.set(0, 0)
	state, event = watcher.sample()
	assert.True(t, state.Completed)
	assert.Equal(t, "complete", event)

	msgs := sink.messages()
	require.Len(t, msgs, 3, "want enter/leave/enter: %v", msgs)
	assert.True(t, strings.Contains(msgs[0], "work queue complete"), msgs[0])
	assert.True(t, strings.Contains(msgs[1], "no longer complete"), msgs[1])
	assert.True(t, strings.Contains(msgs[2], "work queue complete"), msgs[2])
}

// TestCompletionWatcherLosingTheMeasurementIsNotSticky: a queue that WAS complete
// and then becomes unreadable must not keep reporting the old verdict — the
// observer logs the change and the served state is false with the unmeasured
// reason, because "we cannot see the queue" is not "the queue is drained".
func TestCompletionWatcherLosingTheMeasurementIsNotSticky(t *testing.T) {
	reader := &depthReader{}
	reader.set(0, 0)
	sink := &logBuffer{}
	watcher := newCompletionWatcher(reader.read, slog.New(sink))

	state, event := watcher.sample()
	require.True(t, state.Completed)
	require.Equal(t, "complete", event)

	reader.unmeasure()
	state, event = watcher.sample()
	assert.False(t, state.Completed, "an unreadable queue must not inherit the last verdict")
	assert.False(t, state.InboxEmpty)
	assert.False(t, state.NoPendingLeases)
	assert.Nil(t, state.QueueDepth)
	assert.Equal(t, completionReasonUnmeasured, state.Reason)
	assert.Equal(t, "incomplete", event, "losing the measurement changes the verdict and is logged")

	// And it stays silent while it remains unmeasured.
	_, event = watcher.sample()
	assert.Empty(t, event)
	require.Len(t, sink.messages(), 2)

	// The leaving-completion line must not invent counts for a queue nobody
	// read: it carries the reason and NO pending/leased attribute.
	recs := sink.recordsCopy()
	require.Len(t, recs, 2)
	attrs := map[string]string{}
	recs[1].Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	assert.NotContains(t, attrs, "pending", "an unmeasured queue must not log a fabricated pending count")
	assert.NotContains(t, attrs, "leased")
	assert.Equal(t, completionReasonUnmeasured, attrs["reason"])
}

// TestCompletionWatcherFirstObservationIncompleteIsSilent: a server that boots
// with work outstanding is the normal case, not a transition — the loop must not
// write a line for it.
func TestCompletionWatcherFirstObservationIncompleteIsSilent(t *testing.T) {
	reader := &depthReader{}
	reader.set(1, 0)
	sink := &logBuffer{}
	watcher := newCompletionWatcher(reader.read, slog.New(sink))

	state, event := watcher.sample()
	assert.False(t, state.Completed)
	assert.Empty(t, event)
	assert.Empty(t, sink.messages())
}

// TestCompletionWatcherUnmeasuredIsSilent: no depth reader (or a failing one) is
// never a completion transition, so the loop cannot log a state it did not
// measure.
func TestCompletionWatcherUnmeasuredIsSilent(t *testing.T) {
	for name, reader := range map[string]queueDepthReader{
		"no reader": nil,
		"store cannot report": func() (registry.QueueDepth, bool) {
			return registry.QueueDepth{}, false
		},
	} {
		t.Run(name, func(t *testing.T) {
			sink := &logBuffer{}
			watcher := newCompletionWatcher(reader, slog.New(sink))
			for i := 0; i < 3; i++ {
				state, event := watcher.sample()
				assert.False(t, state.Completed)
				assert.Equal(t, completionReasonUnmeasured, state.Reason)
				assert.Empty(t, event)
			}
			assert.Empty(t, sink.messages())
		})
	}
}

// TestCompletionWatcherRunSamplesAndStops pins the loop itself: it samples while
// the context is live and returns promptly once it is cancelled (the graceful
// shutdown path cancels it beside the purge loop).
func TestCompletionWatcherRunSamplesAndStops(t *testing.T) {
	reader := &depthReader{}
	reader.set(0, 0)
	sink := &logBuffer{}
	watcher := newCompletionWatcher(reader.read, slog.New(sink))
	watcher.interval = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.run(ctx)
	}()

	// Wait for the loop to sample (bounded: 2ms ticks, and this asserts, not
	// sleeps-forever).
	deadline := time.Now().Add(2 * time.Second)
	for reader.readCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Positive(t, reader.readCount(), "the loop must sample on its interval")

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher.run did not return after its context was cancelled")
	}
	// The interval is per-sample, so the first complete observation is exactly
	// one line no matter how many ticks fired.
	require.NotEmpty(t, sink.messages())
	for _, msg := range sink.messages() {
		assert.True(t, strings.Contains(msg, "work queue complete"), msg)
	}
	assert.Equal(t, 1, bytes.Count([]byte(strings.Join(sink.messages(), "\n")), []byte("work queue complete")),
		"a drained queue must be one line, not one per tick")
}

// TestCompletionWatcherDefaultInterval pins the documented poll cadence: the
// constant is the interval a server actually runs with, and run() falls back to
// it rather than to a zero ticker (which would panic).
func TestCompletionWatcherDefaultInterval(t *testing.T) {
	watcher := newCompletionWatcher(nil, nil)
	assert.Equal(t, completionPollInterval, watcher.interval)
	assert.NotNil(t, watcher.log, "a nil logger must fall back to the default one")

	watcher.interval = 0
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watcher.run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run must return immediately on an already-cancelled context")
	}
}
