package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

func replayTranslation(t *testing.T, key, text string) *a2a.Translation {
	t.Helper()
	tr, err := a2a.Translate(&a2a.SendMessageParams{Message: &a2a.Message{MessageID: key, Role: a2a.RoleUser, Parts: []a2a.Part{a2a.TextPart(text)}}}, nil, "", false)
	require.NoError(t, err)
	return tr
}

func TestA2AReplayDoesNotDispatchAgain(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, blocking := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/blocking=%v", backend, blocking), func(t *testing.T) {
				var store registry.Store = registry.NewMemoryStore()
				path := filepath.Join(t.TempDir(), "registry.db")
				if backend == "sqlite" {
					s, err := registry.NewSQLiteStore(path)
					require.NoError(t, err)
					store = s
				}
				now := time.Now().UTC()
				calls := 0
				deliver := func(w http.ResponseWriter, _ *http.Request) {
					calls++
					reply := ""
					if blocking {
						reply = `,"reply":{"answer":"original answer"}`
					}
					_, _ = fmt.Fprintf(w, `{"id":"task-%d","transport":"inbox"%s}`, calls, reply)
				}
				handler := func() *a2aHandler {
					return &a2aHandler{store: registry.NewAuthorityStore(store, nil, ""), deliver: deliver, opts: a2aOptions{now: func() time.Time { return now }}}
				}
				first, rpcErr := handler().sendOnce("target", replayTranslation(t, "same", "original"), t.Context())
				require.Nil(t, rpcErr)
				require.Equal(t, 1, calls)
				if backend == "sqlite" {
					require.NoError(t, store.(*registry.SQLiteStore).Close())
					s, err := registry.NewSQLiteStore(path)
					require.NoError(t, err)
					store = s
					defer func() { _ = s.Close() }()
				}
				now = now.Add(time.Minute)
				replay, rpcErr := handler().sendOnce("target", replayTranslation(t, "same", "changed retry payload"), t.Context())
				require.Nil(t, rpcErr)
				require.Equal(t, 1, calls, "replay must not even call the registry delivery handler")
				require.True(t, replay.Accept.IdempotentReplay)
				replay.Accept.IdempotentReplay = false
				originalJSON, err := json.Marshal(first)
				require.NoError(t, err)
				replayJSON, err := json.Marshal(replay)
				require.NoError(t, err)
				require.JSONEq(t, string(originalJSON), string(replayJSON))
				independent, rpcErr := handler().sendOnce("target", replayTranslation(t, "different", "new work"), t.Context())
				require.Nil(t, rpcErr)
				require.Equal(t, 2, calls)
				require.NotEqual(t, first.Accept.ID, independent.Accept.ID)
			})
		}
	}
}

type failingA2AReplayStore struct {
	*registry.MemoryStore
	failAcquire, failFinish bool
}

func (s *failingA2AReplayStore) AcquireA2AReplay(agent, key string, now time.Time, window time.Duration) (string, []byte, error) {
	if s.failAcquire {
		return "", nil, errors.New("injected replay read failure")
	}
	return s.MemoryStore.AcquireA2AReplay(agent, key, now, window)
}
func (s *failingA2AReplayStore) FinishA2AReplay(agent, key, token string, record []byte) error {
	if s.failFinish {
		return errors.New("injected receipt write failure")
	}
	return s.MemoryStore.FinishA2AReplay(agent, key, token, record)
}

func TestA2AReplayFailsClosed(t *testing.T) {
	parallelBoot(t) // parent-side env-free boot path (INT-CI-014)
	for _, mode := range []string{"acquire", "finish", "reject", "ambiguous", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			store := &failingA2AReplayStore{MemoryStore: registry.NewMemoryStore(), failAcquire: mode == "acquire", failFinish: mode == "finish"}
			calls := 0
			h := &a2aHandler{store: store, opts: a2aOptions{now: time.Now}, deliver: func(w http.ResponseWriter, _ *http.Request) {
				calls++
				switch mode {
				case "reject":
					w.WriteHeader(400)
				case "ambiguous":
					w.WriteHeader(500)
				default:
					_, _ = w.Write([]byte(`{"id":"original","transport":"inbox"}`))
				}
			}}
			if mode == "corrupt" {
				token, _, err := store.AcquireA2AReplay("target", "key", time.Now(), time.Hour)
				require.NoError(t, err)
				require.NoError(t, store.FinishA2AReplay("target", "key", token, []byte(`{"translation":null}`)))
			}
			_, rpcErr := h.sendOnce("target", replayTranslation(t, "key", "work"), context.Background())
			require.NotNil(t, rpcErr)
			if mode == "acquire" || mode == "corrupt" {
				require.Zero(t, calls)
				return
			}
			store.failFinish = false
			_, rpcErr = h.sendOnce("target", replayTranslation(t, "key", "work"), context.Background())
			require.NotNil(t, rpcErr)
			if mode == "reject" {
				require.Equal(t, 2, calls)
			} else {
				require.Equal(t, 1, calls)
				require.Contains(t, rpcErr.Message, "pending or unconfirmed")
			}
		})
	}
}

func TestA2AReplayConcurrentHandlers(t *testing.T) {
	parallelBoot(t) // parent-side env-free boot path (INT-CI-014)
	store := registry.NewMemoryStore()
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	deliver := func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"id":"only-task","transport":"inbox"}`))
	}
	handler := func() *a2aHandler {
		return &a2aHandler{store: store, deliver: deliver, opts: a2aOptions{now: time.Now}}
	}
	done := make(chan *a2a.RPCError, 1)
	go func() {
		_, err := handler().sendOnce("target", replayTranslation(t, "concurrent", "first"), t.Context())
		done <- err
	}()
	<-entered
	_, err := handler().sendOnce("target", replayTranslation(t, "concurrent", "retry"), t.Context())
	require.NotNil(t, err)
	require.Contains(t, err.Message, "pending or unconfirmed")
	require.EqualValues(t, 1, calls.Load())
	close(release)
	require.Nil(t, <-done)
	replay, err := handler().sendOnce("target", replayTranslation(t, "concurrent", "retry"), t.Context())
	require.Nil(t, err)
	require.Equal(t, "only-task", replay.Accept.ID)
	require.True(t, replay.Accept.IdempotentReplay)
	require.EqualValues(t, 1, calls.Load())
}
