package main

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

type observedA2AReplayStore struct {
	*registry.MemoryStore
	pending chan struct{}
}

func (s *observedA2AReplayStore) AcquireA2AReplay(agent, key string, now time.Time, window time.Duration) (string, []byte, error) {
	token, raw, err := s.MemoryStore.AcquireA2AReplay(agent, key, now, window)
	if err == registry.ErrA2AReplayPending {
		select {
		case s.pending <- struct{}{}:
		default:
		}
	}
	return token, raw, err
}

func TestA2AConcurrentReplayWaitsForOriginalAccept(t *testing.T) {
	store := &observedA2AReplayStore{MemoryStore: registry.NewMemoryStore(), pending: make(chan struct{}, 1)}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h := &a2aHandler{store: store, opts: a2aOptions{now: time.Now}, deliver: func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"id":"one-task","transport":"inbox"}`))
	}}
	first, second := replayTranslation(t, "same-key", "work"), replayTranslation(t, "same-key", "work")
	type result struct {
		receipt *a2a.Replay
		err     *a2a.RPCError
	}
	done := make(chan result, 2)
	go func() { r, e := h.sendOnce("target", first, t.Context()); done <- result{r, e} }()
	<-entered
	go func() { r, e := h.sendOnce("target", second, t.Context()); done <- result{r, e} }()
	// Release only after the second request has observed the committed pending
	// reservation. No socket timing or sleeps are used to arrange the race.
	<-store.pending
	close(release)
	replayed := 0
	for range 2 {
		r := <-done
		require.Nil(t, r.err)
		require.Equal(t, "one-task", r.receipt.Accept.ID)
		if r.receipt.Accept.IdempotentReplay {
			replayed++
		}
	}
	require.Equal(t, 1, replayed)
	require.EqualValues(t, 1, calls.Load())
}
