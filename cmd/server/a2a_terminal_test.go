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

func terminalHarness(t *testing.T, custom ...a2aOptions) (string, *registry.MemoryStore) {
	t.Helper()
	store := registry.NewMemoryStore()
	registerStoreAgent(t, store, "terminal-agent", true)
	// Exercise the production service-policy wrapper as well as the backend;
	// optional observation capabilities must survive the composition root.
	servingStore := registry.NewAuthorityStore(store, nil, "")
	handler := registry.NewHandler(servingStore)
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox/ack", handler.HandleAck).Methods(http.MethodPost)
	opts := a2aOptions{streamBudget: 3 * time.Second, streamPoll: 5 * time.Millisecond}
	if len(custom) > 0 {
		opts = custom[0]
	}
	registerA2ARoute(r, servingStore, handler.HandleDeliver, handler.HandleUpdateAgent, nil, opts)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL, store
}

func terminalSubscription(t *testing.T, base string) (string, *sseStream) {
	t.Helper()
	task := sentTask(t, rpcResult(t, mustA2AOK(t, http.DefaultClient, base,
		sendMessageBody("terminal-agent", a2a.MethodSendMessage, "terminal-message"))))
	id := taskIDOf(t, task)
	stream := openSSE(t, base, subscribeBody("terminal-agent", id))
	require.Equal(t, string(a2a.TaskStateSubmitted), rpcTaskState(t, stream.next(t, "opening task")["task"].(map[string]any)))
	return id, stream
}

func terminalStatusText(t *testing.T, update map[string]any) string {
	t.Helper()
	status := update["status"].(map[string]any)
	message := status["message"].(map[string]any)
	return message["parts"].([]any)[0].(map[string]any)["text"].(string)
}

func TestA2ATerminal_CancelDuringStream(t *testing.T) {
	for _, budgetFirst := range []bool{false, true} {
		name := "poll"
		opts := a2aOptions{streamBudget: 3 * time.Second, streamPoll: 5 * time.Millisecond}
		if budgetFirst {
			name = "budget-before-poll"
			opts.streamBudget = time.Second
			opts.streamPoll = time.Minute
		}
		t.Run(name, func(t *testing.T) {
			base, _ := terminalHarness(t, opts)
			id, stream := terminalSubscription(t, base)
			second := openSSE(t, base, subscribeBody("terminal-agent", id))
			second.next(t, "second observer opening task")
			canceled := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, cancelTaskBody("terminal-agent", id)))
			require.Equal(t, string(a2a.TaskStateCanceled), rpcTaskState(t, canceled))
			for _, observer := range []*sseStream{stream, second} {
				update := observer.next(t, "cancel status")["statusUpdate"].(map[string]any)
				require.Equal(t, string(a2a.TaskStateCanceled), update["status"].(map[string]any)["state"])
				require.Contains(t, terminalStatusText(t, update), "canceled by request")
				observer.waitClosed(t)
			}
			read := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "")))
			require.Equal(t, string(a2a.TaskStateCanceled), rpcTaskState(t, read))
			_, raw, _ := a2aRPC(t, http.DefaultClient, base, subscribeBody("terminal-agent", id))
			code, _ := rpcError(t, raw)
			require.Equal(t, a2a.CodeUnsupportedOperationError, code)
		})
	}
}

func TestA2ATerminal_RetrieveWithoutAck(t *testing.T) {
	base, store := terminalHarness(t)
	id, stream := terminalSubscription(t, base)
	entries, _, err := store.Retrieve("terminal-agent", 1, 60)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, id, entries[0].ID)
	working := stream.next(t, "retrieve status")["statusUpdate"].(map[string]any)
	require.Equal(t, string(a2a.TaskStateWorking), working["status"].(map[string]any)["state"])
	// A removal that is NOT an Ack must never be reported as success, even
	// after the observer saw retrieval. CloseEntry also models a transfer/purge.
	require.NoError(t, store.CloseEntry("terminal-agent", id))
	unknown := stream.next(t, "unexplained removal")["statusUpdate"].(map[string]any)
	require.Equal(t, string(a2a.TaskStateUnspecified), unknown["status"].(map[string]any)["state"])
	require.Equal(t, "the inbox entry was removed; no acknowledgement or cancellation was observed, so the task outcome is unknown", terminalStatusText(t, unknown))
	// Unknown is non-terminal. The stream remains available for later evidence.
	select {
	case <-stream.closed:
		t.Fatal("unknown outcome closed the stream as if it were terminal")
	default:
	}
}

func TestA2ATerminal_RetrieveWithAck(t *testing.T) {
	base, store := terminalHarness(t)
	id, stream := terminalSubscription(t, base)
	entries, lease, err := store.Retrieve("terminal-agent", 1, 60)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	stream.nextStatus(t, string(a2a.TaskStateWorking))
	body := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, lease, id)
	resp, err := http.Post(base+"/agents/terminal-agent/inbox/ack", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	completed := stream.next(t, "ack status")["statusUpdate"].(map[string]any)
	require.Equal(t, string(a2a.TaskStateWorking), completed["status"].(map[string]any)["state"])
	require.Equal(t, "the receiving agent acknowledged the message; no answer observed yet", terminalStatusText(t, completed))
	require.NoError(t, stream.body.Body.Close())
	stream.waitClosed(t)
}

func TestA2ATerminal_Expiry(t *testing.T) {
	base, store := terminalHarness(t)
	entry := &registry.InboxEntry{ID: "expiring-task", Payload: json.RawMessage(`{"text":"expire"}`), ExpiresAt: time.Now().Add(time.Second)}
	require.NoError(t, store.Deliver("terminal-agent", entry))
	stream := openSSE(t, base, subscribeBody("terminal-agent", entry.ID))
	stream.next(t, "opening expiring task")
	failed := stream.next(t, "expiry status")["statusUpdate"].(map[string]any)
	require.Equal(t, string(a2a.TaskStateFailed), failed["status"].(map[string]any)["state"])
	require.Contains(t, terminalStatusText(t, failed), "expired unacknowledged")
	stream.waitClosed(t)
}
