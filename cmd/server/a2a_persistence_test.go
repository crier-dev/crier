package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/registry"
)

func reloadedA2AServer(t *testing.T, store registry.Store) string {
	t.Helper()
	serving := registry.NewAuthorityStore(store, nil, "")
	handler := registry.NewHandler(serving)
	router := mux.NewRouter()
	registerA2ARoute(router, serving, handler.HandleDeliver, handler.HandleUpdateAgent, nil, a2aOptions{streamBudget: time.Second, streamPoll: time.Millisecond})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server.URL
}

func TestA2APersistence_AckAfterStreamAndHandlerClose(t *testing.T) {
	base, store := terminalHarness(t)
	id, stream := terminalSubscription(t, base)
	entries, lease, err := store.Retrieve("terminal-agent", time.Minute, 1)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, store.Ack("terminal-agent", lease, []string{id}))
	stream.nextStatus(t, string(a2a.TaskStateWorking))
	require.NoError(t, stream.body.Body.Close())
	stream.waitClosed(t)
	assertStoredTask(t, reloadedA2AServer(t, store), id, a2a.TaskStateWorking)
}

func TestA2APersistence_CancelAfterStreamAndHandlerClose(t *testing.T) {
	base, store := terminalHarness(t)
	id, stream := terminalSubscription(t, base)
	rpcResult(t, mustA2AOK(t, http.DefaultClient, base, cancelTaskBody("terminal-agent", id)))
	stream.nextStatus(t, string(a2a.TaskStateCanceled))
	stream.waitClosed(t)
	assertStoredTask(t, reloadedA2AServer(t, store), id, a2a.TaskStateCanceled)
}

func assertStoredTask(t *testing.T, base, id string, state a2a.TaskState) {
	t.Helper()
	task := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
	require.Equal(t, string(state), rpcTaskState(t, task))
	listing := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, listTasksBody("terminal-agent", "")))
	tasks := listing["tasks"].([]any)
	require.Len(t, tasks, 1)
	require.Equal(t, id, tasks[0].(map[string]any)["id"])
	require.Equal(t, string(state), rpcTaskState(t, tasks[0].(map[string]any)))
}

func TestA2APersistence_UnobservedRemovalIsUnknown(t *testing.T) {
	base, store := terminalHarness(t)
	id, stream := terminalSubscription(t, base)
	require.NoError(t, store.CloseEntry("terminal-agent", id))
	stream.nextStatus(t, string(a2a.TaskStateUnspecified))
	// Force the stream to finish before reading through a new handler.
	stream.waitClosed(t)
	base = reloadedA2AServer(t, store)
	assertStoredTask(t, base, id, a2a.TaskStateUnspecified)
	task := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
	require.Equal(t, "the inbox entry was removed; no acknowledgement or cancellation was observed, so the task outcome is unknown", terminalStatusText(t, task))
}

func TestA2APersistence_MemoryRestartHasNoEvidence(t *testing.T) {
	base, store := terminalHarness(t)
	id, stream := terminalSubscription(t, base)
	rpcResult(t, mustA2AOK(t, http.DefaultClient, base, cancelTaskBody("terminal-agent", id)))
	stream.nextStatus(t, string(a2a.TaskStateCanceled))
	stream.waitClosed(t)
	assertStoredTask(t, reloadedA2AServer(t, store), id, a2a.TaskStateCanceled)
	fresh := registry.NewMemoryStore()
	registerStoreAgent(t, fresh, "terminal-agent", true)
	_, raw, _ := a2aRPC(t, http.DefaultClient, reloadedA2AServer(t, fresh), getTaskBody("terminal-agent", id, ""))
	code, _ := rpcError(t, raw)
	require.Equal(t, a2a.CodeTaskNotFoundError, code)
}

// A retained terminal frame is a storage promise, not merely a live watch bit.
type failingObservationStore struct{ *registry.MemoryStore }

func (s *failingObservationStore) RecordTaskObservation(string, registry.TaskObservation) error {
	return errors.New("injected observation write failure")
}
func TestA2APersistence_AckDoesNotDependOnStreamWrite(t *testing.T) {
	store := &failingObservationStore{registry.NewMemoryStore()}
	registerStoreAgent(t, store, "terminal-agent", true)
	base := reloadedA2AServer(t, store)
	id, stream := terminalSubscription(t, base)
	_, lease, err := store.Retrieve("terminal-agent", time.Minute, 1)
	require.NoError(t, err)
	require.NoError(t, store.Ack("terminal-agent", lease, []string{id}))
	stream.nextStatus(t, string(a2a.TaskStateWorking))
	require.NoError(t, stream.body.Body.Close())
	stream.waitClosed(t)
	assertStoredTask(t, reloadedA2AServer(t, store), id, a2a.TaskStateWorking)
}

func TestA2APersistence_StreamWriteFailureIsNotUnknown(t *testing.T) {
	store := &failingObservationStore{registry.NewMemoryStore()}
	registerStoreAgent(t, store, "terminal-agent", true)
	base := reloadedA2AServer(t, store)
	id, stream := terminalSubscription(t, base)
	// This is disappearance, not Ack: Ack now commits its own evidence and
	// correctly bypasses the stream's fallible RecordTaskObservation hook.
	require.NoError(t, store.CloseEntry("terminal-agent", id))
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
observing:
	for {
		select {
		case event := <-stream.events:
			if errorText, ok := event["__error"]; ok {
				require.Contains(t, errorText, "injected observation write failure")
				break observing
			}
			if update, ok := event["statusUpdate"].(map[string]any); ok {
				require.NotEqual(t, string(a2a.TaskStateUnspecified), update["status"].(map[string]any)["state"], "a failed persistence step must not emit a retained unknown state")
			}
		case <-deadline.C:
			t.Fatal("no explicit persistence error")
		}
	}
	stream.waitClosed(t)
	_, found, err := store.TaskObservation("terminal-agent", id)
	require.NoError(t, err)
	require.False(t, found)
}
