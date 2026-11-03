package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

func ackEvidenceServer(t *testing.T, store registry.Store) string {
	t.Helper()
	wrapped := registry.NewAuthorityStore(store, nil, "")
	h := registry.NewHandler(wrapped)
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)
	registerA2ARoute(r, wrapped, h.HandleDeliver, h.HandleUpdateAgent, nil,
		a2aOptions{streamBudget: time.Second, streamPoll: time.Millisecond})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

func testA2AAckEvidence(t *testing.T, store registry.Store) {
	t.Helper()
	registerStoreAgent(t, store, "terminal-agent", true)
	base := ackEvidenceServer(t, store)
	backend := store.(registry.TaskObservationStore)
	for _, mode := range []string{"no-stream", "between-streams", "close-race"} {
		t.Run(mode, func(t *testing.T) {
			accepted := sentTask(t, rpcResult(t, mustA2AOK(t, http.DefaultClient, base,
				sendMessageBody("terminal-agent", a2a.MethodSendMessage, mode))))
			id := taskIDOf(t, accepted)
			entries, lease, err := store.Retrieve("terminal-agent", time.Minute, 1)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			var stream *sseStream
			if mode != "no-stream" {
				stream = openSSE(t, base, subscribeBody("terminal-agent", id))
				stream.next(t, "opening task")
			}
			if mode == "between-streams" {
				require.NoError(t, stream.body.Body.Close())
				stream.waitClosed(t)
			}
			closed := make(chan struct{})
			go func() {
				defer close(closed)
				if mode == "close-race" {
					_ = stream.body.Body.Close()
				}
			}()
			body := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, lease, id)
			resp, err := http.Post(base+"/agents/terminal-agent/inbox/ack", "application/json", strings.NewReader(body))
			require.NoError(t, err)
			require.Equal(t, http.StatusNoContent, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			<-closed
			if mode == "close-race" {
				stream.waitClosed(t)
			}
			o, found, err := backend.TaskObservation("terminal-agent", id)
			require.NoError(t, err)
			require.True(t, found)
			var ev a2a.TaskEvidence
			require.NoError(t, json.Unmarshal(o.Evidence, &ev))
			require.NotNil(t, ev.Entry)
			require.JSONEq(t, string(entries[0].Payload), string(ev.Entry.Payload))
			// A stale observer finishing after Ack cannot install unknown over it.
			stale := o
			stale.Outcome = "unknown"
			ev.AcknowledgedAt = time.Time{}
			ev.RemovedAt = time.Now().UTC()
			stale.Evidence, err = json.Marshal(ev)
			require.NoError(t, err)
			require.NoError(t, backend.RecordTaskObservation("terminal-agent", stale))
			assertA2AAckEvidence(t, base, id)
			assertA2AAckEvidence(t, reloadedA2AServer(t, store), id)
		})
	}
}

// assertA2AAckEvidence pins what a read of an acknowledged task answers: the
// acknowledgement is retained as lifecycle EVIDENCE (the stream uses it to keep
// waiting for an answer), but the task itself is gone from the store. Crier keeps
// no tombstone for a released lease, so GetTask and ListTasks answer
// TaskNotFoundError (-32001) naming TASK_NOT_FOUND — never a live WORKING task
// state, and never an invented COMPLETED.
func assertA2AAckEvidence(t *testing.T, base, id string) {
	t.Helper()
	_, raw, _ := a2aRPC(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, ""))
	code, message := rpcError(t, raw)
	require.Equal(t, a2a.CodeTaskNotFoundError, code,
		"an acknowledged task with no answer is purged, so a read is -32001: %s", message)
	require.Contains(t, string(raw), `"TASK_NOT_FOUND"`, "the refusal names the spec's own reason")
	require.NotContains(t, string(raw), "TASK_STATE_WORKING", "a purged task never reports a live state")
	listing := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, listTasksBody("terminal-agent", "")))
	require.Empty(t, rpcTasks(t, listing), "a purged task is not in the listing")
}

func TestA2AAckEvidenceMemory(t *testing.T) {
	testA2AAckEvidence(t, registry.NewMemoryStore())
}
