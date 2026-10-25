package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

func TestMessageRequestIdempotency(t *testing.T) {
	t.Parallel()
	runMessageRequestIdempotency(t, localBackends())
}

// Count the delivery seam itself, not just transcript rows: a duplicate harness
// invocation can be hidden by downstream inbox deduplication (DF-CRIER-357).
type requestExecutionCounter struct {
	*registry.MemoryStore
	calls *atomic.Int32
}

func (d requestExecutionCounter) DispatchDelivery(ctx context.Context, target string, req registry.DeliveryRequest) (registry.DeliveryResult, error) {
	d.calls.Add(1)
	return registry.NewSessionDeliverer(registry.NewHandler(d.MemoryStore)).DispatchDelivery(ctx, target, req)
}

func newRequestExecutionHarness(t *testing.T, store Repository, calls *atomic.Int32) *apiHarness {
	t.Helper()
	reg := registry.NewMemoryStore()
	for _, id := range []string{"atlas", "nimbus"} {
		require.NoError(t, reg.Register(&registry.Agent{ID: id}))
	}
	router := mux.NewRouter()
	registerSessionRoutes(router, NewHTTPHandler(store, HTTPOptions{
		Deliverer: requestExecutionCounter{MemoryStore: reg, calls: calls}, Agents: reg,
	}))
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &apiHarness{srv: srv, reg: reg}
}

func runMessageRequestIdempotency(t *testing.T, backends []localBackend) {
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, first, second string
				want                int
			}{
				{"same_key", "retry", "retry", 1},
				{"different_keys", "first", "second", 2},
				{"no_key", "", "", 2},
			} {
				t.Run(tc.name, func(t *testing.T) {
					path := backend.pathFn(t)
					store := backend.open(t, path)
					var calls atomic.Int32
					h := newRequestExecutionHarness(t, store, &calls)
					room := h.createRoom(t)
					req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"hello"}`), IdempotencyKey: tc.first}
					first := h.postTo(t, room, req)
					require.EqualValues(t, 1, calls.Load())
					if tc.first != "" {
						require.Equal(t, first.ID, h.postTo(t, room, req).ID)
						require.EqualValues(t, 1, calls.Load(), "same-process retry must not execute")
					}
					require.NoError(t, store.Close())
					// A fresh store AND handler must recover the key and outcomes.
					reopened := backend.open(t, path)
					t.Cleanup(func() { _ = reopened.Close() })
					h2 := newRequestExecutionHarness(t, reopened, &calls)
					req.IdempotencyKey = tc.second
					second := h2.postTo(t, room, req)
					require.EqualValues(t, tc.want, calls.Load(), "reopened store must recover the request before dispatch")
					t.Logf("target invocations before reopen=1 after=%d", calls.Load())
					st, err := reopened.Load(context.Background(), room)
					require.NoError(t, err)
					require.Len(t, st.Messages, tc.want)
					if tc.want == 1 {
						require.Equal(t, first.ID, second.ID)
						require.Equal(t, first.Seq, second.Seq)
						require.Equal(t, first.Outcomes, second.Outcomes)
						entries, _, err := h2.reg.Retrieve("nimbus", time.Minute, 100)
						require.NoError(t, err)
						require.Empty(t, entries, "replay must not fan out into the fresh registry")
					} else {
						require.NotEqual(t, first.ID, second.ID)
					}
				})
			}
		})
	}
}

func TestMessageRequestIdempotencySessionScope(t *testing.T) {
	t.Parallel()
	runMessageRequestIdempotencySessionScope(t, localBackends())
}

func runMessageRequestIdempotencySessionScope(t *testing.T, backends []localBackend) {
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t, backend.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			var ids []string
			for range 2 {
				room := h.createRoom(t)
				req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"hello"}`), IdempotencyKey: "shared"}
				first := h.postTo(t, room, req)
				second := h.postTo(t, room, req)
				require.Equal(t, first.ID, second.ID)
				st, err := store.Load(context.Background(), room)
				require.NoError(t, err)
				require.Len(t, st.Messages, 1)
				ids = append(ids, first.ID)
			}
			require.NotEqual(t, ids[0], ids[1])
		})
	}
}

type heldRequestDeliverer struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (d *heldRequestDeliverer) Deliver(string, *registry.InboxEntry) error {
	panic("fan-out must use the guarded dispatcher")
}

func (d *heldRequestDeliverer) DispatchDelivery(context.Context, string, registry.DeliveryRequest) (registry.DeliveryResult, error) {
	d.calls.Add(1)
	d.once.Do(func() { close(d.started) })
	<-d.release
	return registry.DeliveryResult{}, nil
}

func TestMessageRequestIdempotencyConcurrent(t *testing.T) {
	t.Parallel()
	runMessageRequestIdempotencyConcurrent(t, localBackends())
}

func runMessageRequestIdempotencyConcurrent(t *testing.T, backends []localBackend) {
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			path := backend.pathFn(t)
			firstStore := backend.open(t, path)
			t.Cleanup(func() { _ = firstStore.Close() })
			h := newAPIHarness(t, firstStore)
			room := h.createRoom(t)
			// Independent handles/handlers rule out a handler-local mutex.
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
			responses := make(chan *httptest.ResponseRecorder, 2)
			post := func(router *mux.Router) {
				r := httptest.NewRequest(http.MethodPost, "/sessions/"+room+"/messages", strings.NewReader(`{"sender":"atlas","payload":{"text":"hello"},"idempotency_key":"simultaneous"}`))
				w := httptest.NewRecorder()
				router.ServeHTTP(w, r)
				responses <- w
			}
			go post(routers[0])
			select {
			case <-d.started:
			case <-time.After(5 * time.Second):
				t.Fatal("first send never reached delivery")
			}
			go post(routers[1])
			select {
			case w := <-responses:
				t.Fatalf("send completed before delivery was released: %s", w.Body.String())
			case <-time.After(100 * time.Millisecond):
			}
			release.Do(func() { close(d.release) })
			var messages []transcriptMessage
			for range 2 {
				select {
				case w := <-responses:
					require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
					var m transcriptMessage
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
					messages = append(messages, m)
				case <-time.After(5 * time.Second):
					t.Fatal("send did not finish")
				}
			}
			require.Equal(t, messages[0].ID, messages[1].ID)
			require.Equal(t, messages[0].Seq, messages[1].Seq)
			require.Equal(t, messages[0].Outcomes, messages[1].Outcomes)
			require.Equal(t, int32(1), d.calls.Load())
			st, err := firstStore.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Messages, 1)
		})
	}
}
