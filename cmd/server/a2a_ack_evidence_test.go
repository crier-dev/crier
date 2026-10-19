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
			assertA2AAckEvidence(t, base, id, accepted)
			assertA2AAckEvidence(t, reloadedA2AServer(t, store), id, accepted)
		})
	}
}

func assertA2AAckEvidence(t *testing.T, base, id string, accepted map[string]any) {
	t.Helper()
	got := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
	require.Equal(t, string(a2a.TaskStateCompleted), rpcTaskState(t, got))
	require.Equal(t, accepted["history"], got["history"], "original parts, metadata and context survive Ack")
	require.Equal(t, accepted["contextId"], got["contextId"])
	require.Equal(t, a2a.StateBasisAcknowledged, got["metadata"].(map[string]any)["crier"].(map[string]any)["state_basis"])
	require.Equal(t, "the message was acknowledged: the inbox entry is closed and can no longer be retrieved", terminalStatusText(t, got))
	listing := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, listTasksBody("terminal-agent", "")))
	matches := 0
	for _, raw := range listing["tasks"].([]any) {
		task := raw.(map[string]any)
		if task["id"] == id {
			matches++
			require.Equal(t, string(a2a.TaskStateCompleted), rpcTaskState(t, task))
		}
	}
	require.Equal(t, 1, matches, "ListTasks must contain one COMPLETED, never immortal UNSPECIFIED")
}

func TestA2AAckEvidenceMemory(t *testing.T) {
	testA2AAckEvidence(t, registry.NewMemoryStore())
}
