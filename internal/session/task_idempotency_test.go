package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

// These exercise the real task routes and durable stores, including a fresh
// handle on retry: an HTTP-handler-local cache cannot satisfy the contract.
func TestTaskRequestSharedLock(t *testing.T) {
	backends := append(localBackends(), localBackend{
		name: "sqlite_memory", pathFn: func(*testing.T) string { return ":memory:" },
		open: func(t *testing.T, path string) Repository {
			s, err := NewSQLiteStore(path)
			require.NoError(t, err)
			return s
		},
	})
	runTaskRequestSharedLock(t, backends)
}

func runTaskRequestSharedLock(t *testing.T, backends []localBackend) {
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t, backend.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			// A blocking message send and a non-waiting task send must contend
			// on exactly the same key, in BOTH directions.
			release, err := store.LockMessage(t.Context(), "room", "key")
			require.NoError(t, err)
			_, busy := store.TryLockMessage(t.Context(), "room", "key")
			release()
			require.ErrorIs(t, busy, ErrMessageInProgress)
			release, err = store.TryLockMessage(t.Context(), "room", "key")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			_, busy = store.LockMessage(ctx, "room", "key")
			cancel()
			release()
			require.ErrorIs(t, busy, context.DeadlineExceeded)
			for _, pair := range [][2]string{{"room", "other"}, {"other", "key"}, {"room", "key"}} {
				unlock, err := store.TryLockMessage(t.Context(), pair[0], pair[1])
				require.NoError(t, err)
				unlock()
			}
		})
	}
}

func TestTaskRequestIdempotencyReplaysCreationAfterClaim(t *testing.T) {
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t, backend.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			room := h.createRoom(t)
			var first, replay taskCreateResponse
			req := keyedTaskRequest("creation")
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &first, nil))
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks/"+first.TaskID+"/claim",
				taskTransitionRequest{Actor: "nimbus"}, nil, nil))
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &replay, nil))
			require.Equal(t, first, replay)
			var current taskView
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+room+"/tasks/"+first.TaskID, nil, &current, nil))
			require.Equal(t, "claimed", current.State)
			require.Equal(t, 2, current.Versions)
		})
	}
}

func TestTaskRequestIdempotencyRefusalReplaysWithoutWrite(t *testing.T) {
	h := newTriggerHarness(t)
	room := h.createRoom(t)
	req := keyedTaskRequest("refused")
	req.Targets[0].ID = "missing"
	var first, replay taskCreateResponse
	require.Equal(t, http.StatusCreated, h.srv.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &first, nil))
	require.Equal(t, "refused", first.Outcomes[0].Outcome)
	// Make every post-lookup side effect fail loudly if replay reaches it.
	h.handler.newID = func() (string, error) { return "", errors.New("replay minted an id") }
	h.handler.store = &noTaskReplayWrites{Repository: h.handler.store}
	require.Equal(t, http.StatusCreated, h.srv.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &replay, nil))
	require.Equal(t, first, replay)
}

type noTaskReplayWrites struct{ Repository }

func (*noTaskReplayWrites) NextSeq(context.Context, string) (int64, error) {
	return 0, errors.New("replay allocated a sequence")
}

func (*noTaskReplayWrites) PostMessage(context.Context, *Message) error {
	return errors.New("replay wrote a message")
}

func TestTaskRequestIdempotencyNonTaskKeyConflict(t *testing.T) {
	h := newAPIHarness(t, newLocalJSONLStore(t))
	room := h.createRoom(t)
	h.postTo(t, room, postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"hello"}`), IdempotencyKey: "message-key"})
	var refusal map[string]any
	require.Equal(t, http.StatusConflict, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", keyedTaskRequest("message-key"), &refusal, nil))
	require.Equal(t, "IDEMPOTENCY_KEY_CONFLICT", refusal["error"])
}

func TestTaskRequestIdempotency(t *testing.T) {
	runTaskRequestIdempotency(t, localBackends())
}

func runTaskRequestIdempotency(t *testing.T, backends []localBackend) {
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, first, second string
				want                int
			}{
				{"same_key", "retry", "retry", 1},
				{"different_keys", "first", "second", 2},
				// Absence means a fresh task, never a server-invented key.
				{"no_key", "", "", 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					path := backend.pathFn(t)
					store := backend.open(t, path)
					var calls atomic.Int32
					h := newRequestExecutionHarness(t, store, &calls)
					room := h.createRoom(t)
					req := keyedTaskRequest(tc.first)
					var first, second taskCreateResponse
					require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &first, nil))
					require.NotEmpty(t, first.TaskID)
					require.Equal(t, "open", first.State)
					require.EqualValues(t, 1, calls.Load())
					if tc.first != "" {
						require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &second, nil))
						require.Equal(t, first, second)
						require.EqualValues(t, 1, calls.Load(), "same-process task retry must not execute")
					}
					require.NoError(t, store.Close())
					reopened := backend.open(t, path)
					t.Cleanup(func() { _ = reopened.Close() })
					h2 := newRequestExecutionHarness(t, reopened, &calls)
					req.IdempotencyKey = tc.second
					require.Equal(t, http.StatusCreated, h2.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &second, nil))
					require.EqualValues(t, tc.want, calls.Load(), "reopened task request must not dispatch twice")
					t.Logf("target invocations before reopen=1 after=%d", calls.Load())
					st, err := reopened.Load(t.Context(), room)
					require.NoError(t, err)
					require.Len(t, st.Messages, tc.want)
					require.Len(t, tasksInState(st), tc.want)
					if tc.want == 1 {
						require.Equal(t, first, second, "replay must return the recorded creation response")
						entries, _, err := h2.reg.Retrieve("nimbus", time.Minute, 100)
						require.NoError(t, err)
						require.Empty(t, entries, "replay must not deliver into the fresh registry")
					} else {
						require.NotEqual(t, first.TaskID, second.TaskID)
						require.NotEqual(t, first.ID, second.ID)
					}
				})
			}
		})
	}
}

func keyedTaskRequest(key string) postTaskRequest {
	return postTaskRequest{Sender: "atlas", Payload: json.RawMessage(`{"work":"sweep"}`),
		Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}, IdempotencyKey: key}
}

func TestTaskRequestIdempotencyTriggerOnce(t *testing.T) {
	h := newTriggerHarness(t)
	room := h.createRoom(t)
	fired := make(chan string, 2)
	h.handler.SetTaskTrigger(func(taskID, _, _ string) { fired <- taskID })
	var first, second taskCreateResponse
	req := keyedTaskRequest("once")
	require.Equal(t, http.StatusCreated, h.srv.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &first, nil))
	select {
	case id := <-fired:
		require.Equal(t, first.TaskID, id)
	case <-time.After(5 * time.Second):
		t.Fatal("first task did not trigger")
	}
	// A changed retry cannot create work for a new payload or audience.
	req.Payload = json.RawMessage(`{"work":"different"}`)
	req.Targets = []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}
	require.Equal(t, http.StatusCreated, h.srv.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &second, nil))
	require.Equal(t, first, second)
	select {
	case id := <-fired:
		t.Fatalf("replay fired a second trigger: %s", id)
	case <-time.After(100 * time.Millisecond):
	}
	entries, _, err := h.srv.reg.Retrieve("nimbus", time.Minute, 100)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	entries, _, err = h.srv.reg.Retrieve("atlas", time.Minute, 100)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestTaskRequestIdempotencyValidationDoesNotBurnKey(t *testing.T) {
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			for _, invalid := range []string{"payload", "author", "targets", "thread"} {
				t.Run(invalid, func(t *testing.T) {
					store := backend.open(t, backend.pathFn(t))
					t.Cleanup(func() { _ = store.Close() })
					h := newAPIHarness(t, store)
					room := h.createRoom(t)
					req := keyedTaskRequest("correctable")
					switch invalid {
					case "payload":
						req.Payload = nil
					case "author":
						req.Sender = ""
					case "targets":
						req.Targets = nil
					case "thread":
						req.ThreadID = "not-the-root"
					}
					require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, nil, nil))
					st, err := store.Load(t.Context(), room)
					require.NoError(t, err)
					require.Empty(t, st.Messages)
					var first, second taskCreateResponse
					req = keyedTaskRequest("correctable")
					require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &first, nil))
					require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &second, nil))
					require.Equal(t, first, second)
				})
			}
		})
	}
}

func TestTaskRequestIdempotencyConcurrent(t *testing.T) {
	runTaskRequestIdempotencyConcurrent(t, localBackends())
}

func runTaskRequestIdempotencyConcurrent(t *testing.T, backends []localBackend) {
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			path := backend.pathFn(t)
			firstStore := backend.open(t, path)
			t.Cleanup(func() { _ = firstStore.Close() })
			h := newAPIHarness(t, firstStore)
			room := h.createRoom(t)
			secondStore := backend.open(t, path)
			t.Cleanup(func() { _ = secondStore.Close() })
			d := &heldRequestDeliverer{started: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(d.release) }) })
			routers := make([]*mux.Router, 2)
			for i, store := range []Repository{firstStore, secondStore} {
				routers[i] = mux.NewRouter()
				registerSessionRoutes(routers[i], NewHTTPHandler(store, HTTPOptions{Deliverer: d}))
			}
			post := func(router *mux.Router) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/sessions/"+room+"/tasks", strings.NewReader(`{"sender":"atlas","payload":{"work":"sweep"},"targets":[{"kind":"agent","id":"nimbus"}],"idempotency_key":"inflight"}`))
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				return w
			}
			firstDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { firstDone <- post(routers[0]) }()
			select {
			case <-d.started:
			case <-time.After(5 * time.Second):
				t.Fatal("first task never reached delivery")
			}
			secondDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { secondDone <- post(routers[1]) }()
			select {
			case w := <-secondDone:
				require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
				require.JSONEq(t, `{"error":"a delivery with this idempotency_key is already in progress"}`, w.Body.String())
			case <-time.After(5 * time.Second):
				t.Fatal("in-flight retry must return 409 without waiting for delivery")
			}
			require.Equal(t, int32(1), d.calls.Load())
			release.Do(func() { close(d.release) })
			var first taskCreateResponse
			select {
			case w := <-firstDone:
				require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &first))
			case <-time.After(5 * time.Second):
				t.Fatal("first task did not finish")
			}
			w := post(routers[1])
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var replay taskCreateResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &replay))
			require.Equal(t, first, replay)
			require.Equal(t, int32(1), d.calls.Load())
			st, err := firstStore.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Messages, 1)
			require.Len(t, tasksInState(st), 1)
		})
	}
}

func TestTaskRequestIdempotencySessionScope(t *testing.T) {
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t, backend.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			var ids []string
			for range 2 {
				room := h.createRoom(t)
				var first, replay taskCreateResponse
				req := keyedTaskRequest("shared")
				require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &first, nil))
				require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", req, &replay, nil))
				require.Equal(t, first, replay)
				ids = append(ids, first.TaskID)
			}
			require.NotEqual(t, ids[0], ids[1])
		})
	}
}
