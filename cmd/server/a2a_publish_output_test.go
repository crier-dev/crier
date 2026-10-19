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
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/relay"
)

func publishOutputAs(t *testing.T, base, agent, realm, topic string, event any) int {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"topic": topic, "event": event})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, base+"/relay/publish", strings.NewReader(string(raw)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", agent)
	req.Header.Set(namespace.HeaderNamespace, realm)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

func outputEnvelope(taskID, text string) map[string]any {
	return map[string]any{"task_id": taskID, "role": a2a.RoleAgent, "parts": []any{map[string]any{"type": "text", "text": text}}}
}

// This is the acceptance measurement against run()'s production composition,
// with real registration, signed retrieve/ack, and no stream until step three.
func TestA2APublishOutputWithoutStreamHTTP(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	const agent = "publish-answer-361"
	priv := registerA2AAgent(t, base, agent, true, nil)
	start := time.Now()
	task := sentTask(t, rpcResult(t, mustA2AOK(t, client, base, sendMessageBody(agent, a2a.MethodSendMessage, "request-361"))))
	id := taskIDOf(t, task)
	code, raw := signedAs(t, client, http.MethodGet, base, "/agents/"+agent+"/inbox", agent, priv, nil)
	require.Equal(t, http.StatusOK, code, string(raw))
	var inbox struct {
		LeaseID string `json:"lease_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &inbox))
	code, raw = signedAs(t, client, http.MethodPost, base, "/agents/"+agent+"/inbox/ack", agent, priv, []byte(fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, inbox.LeaseID, id)))
	require.Equal(t, http.StatusNoContent, code, string(raw))
	get := func() map[string]any {
		return rpcResult(t, mustA2AOK(t, client, base, getTaskBody(agent, id, `"historyLength":0`)))
	}
	got := get()
	require.Equal(t, string(a2a.TaskStateWorking), rpcTaskState(t, got))
	require.Equal(t, false, got["metadata"].(map[string]any)["crier"].(map[string]any)["answered"])
	t.Logf("step 1 wall=%s SendMessage -> signed retrieve -> ack(204) -> GetTask: %s", time.Since(start), mustJSON361(t, got))

	start = time.Now()
	topic, ok := a2a.TaskTopic(id)
	require.True(t, ok)
	env := outputEnvelope(id, "answer")
	require.Equal(t, http.StatusAccepted, publishOutputAs(t, base, agent, "", topic, env))
	got = get() // Immediate read after 202, not an eventual/poll-until-green test.
	require.Equal(t, string(a2a.TaskStateCompleted), rpcTaskState(t, got))
	require.Equal(t, true, got["metadata"].(map[string]any)["crier"].(map[string]any)["answered"])
	require.Len(t, got["artifacts"], 1)
	require.Equal(t, "answer", got["artifacts"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"])
	listing := rpcResult(t, mustA2AOK(t, client, base, listTasksBody(agent, `"includeArtifacts":true,"historyLength":0`)))
	require.Equal(t, []map[string]any{got}, rpcTasks(t, listing))
	t.Logf("step 2 wall=%s publish(202), no stream -> GetTask and ListTasks agree: %s", time.Since(start), mustJSON361(t, got))

	// Completed tasks refuse new subscriptions. Replay on the completed task
	// first (first evidence must stay byte-identical), then measure the same
	// ack/publish sequence on a fresh task with its own subscription open.
	start = time.Now()
	require.Equal(t, http.StatusAccepted, publishOutputAs(t, base, agent, "", topic, env))
	require.Equal(t, got, get())
	other := sentTask(t, rpcResult(t, mustA2AOK(t, client, base, sendMessageBody(agent, a2a.MethodSendMessage, "stream-361"))))
	otherID := taskIDOf(t, other)
	code, raw = signedAs(t, client, http.MethodGet, base, "/agents/"+agent+"/inbox", agent, priv, nil)
	require.Equal(t, http.StatusOK, code, string(raw))
	require.NoError(t, json.Unmarshal(raw, &inbox))
	code, raw = signedAs(t, client, http.MethodPost, base, "/agents/"+agent+"/inbox/ack", agent, priv, []byte(fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, inbox.LeaseID, otherID)))
	require.Equal(t, http.StatusNoContent, code, string(raw))
	stream := openSSE(t, base, subscribeBody(agent, otherID))
	opening := stream.next(t, "opening task")["task"].(map[string]any)
	require.Equal(t, string(a2a.TaskStateWorking), rpcTaskState(t, opening))
	otherTopic, ok := a2a.TaskTopic(otherID)
	require.True(t, ok)
	require.Equal(t, http.StatusAccepted, publishOutputAs(t, base, agent, "", otherTopic, outputEnvelope(otherID, "answer")))
	stream.next(t, "answer artifact")
	stream.nextStatus(t, string(a2a.TaskStateCompleted))
	stream.waitClosed(t)
	streamed := rpcResult(t, mustA2AOK(t, client, base, getTaskBody(agent, otherID, `"historyLength":0`)))
	require.Equal(t, string(a2a.TaskStateCompleted), rpcTaskState(t, streamed))
	require.Equal(t, true, streamed["metadata"].(map[string]any)["crier"].(map[string]any)["answered"])
	t.Logf("step 3 wall=%s same-task stream open on fresh acked task -> publish(202) -> artifact + COMPLETED; GetTask: %s", time.Since(start), mustJSON361(t, streamed))
}

func mustJSON361(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

func TestA2APublishOutputScope(t *testing.T) {
	for _, tc := range []struct {
		name, publisher, realm, taskField, text string
		stream, want                            bool
	}{
		{name: "owner after ack", publisher: "owner", text: "answer", want: true},
		{name: "camel taskId", publisher: "owner", taskField: "taskId", text: "answer", want: true},
		{name: "unknown task", publisher: "owner", taskField: "missing", text: "answer"},
		{name: "foreign agent", publisher: "other", text: "answer"},
		{name: "foreign agent with stream", publisher: "other", text: "answer", stream: true},
		{name: "anonymous", text: "answer"},
		{name: "anonymous with stream", text: "answer", stream: true},
		{name: "wrong realm", publisher: "owner", realm: "elsewhere", text: "answer"},
		{name: "empty output", publisher: "owner"},
		{name: "whitespace output", publisher: "owner", text: "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := registry.NewMemoryStore()
			registerStoreAgent(t, store, "owner", true)
			registerStoreAgent(t, store, "other", true)
			serving := registry.NewAuthorityStore(store, nil, "")
			handler := registry.NewHandler(serving)
			bus := relay.New(0) // Anonymous publishes are accepted, but cannot be evidence.
			policies, err := namespace.Parse(`{"namespaces":[{"name":"elsewhere"}]}`, nil)
			require.NoError(t, err)
			bus.SetNamespacePolicies(policies)
			r := mux.NewRouter()
			r.HandleFunc("/relay/publish", bus.HandlePublish).Methods(http.MethodPost)
			registerA2ARoute(r, serving, handler.HandleDeliver, handler.HandleUpdateAgent, bus, a2aOptions{streamPoll: time.Millisecond, streamBudget: time.Second})
			srv := httptest.NewServer(r)
			t.Cleanup(srv.Close)
			task := sentTask(t, rpcResult(t, mustA2AOK(t, http.DefaultClient, srv.URL, sendMessageBody("owner", a2a.MethodSendMessage, "scope-361"))))
			id := taskIDOf(t, task)
			var stream *sseStream
			if tc.stream {
				stream = openSSE(t, srv.URL, subscribeBody("owner", id))
				stream.next(t, "opening task")
				t.Cleanup(func() { require.NoError(t, stream.body.Body.Close()); stream.waitClosed(t) })
			}
			_, lease, err := store.Retrieve("owner", time.Minute, 1)
			require.NoError(t, err)
			require.NoError(t, store.Ack("owner", lease, []string{id}))
			before, found, err := store.TaskObservation("owner", id)
			require.NoError(t, err)
			require.True(t, found)
			eventID := id
			if tc.taskField == "missing" {
				eventID = "nonexistent-task"
			}
			topic, ok := a2a.TaskTopic(eventID)
			require.True(t, ok)
			env := outputEnvelope(eventID, tc.text)
			if tc.taskField == "taskId" {
				delete(env, "task_id")
				env["taskId"] = id
			}
			require.Equal(t, http.StatusAccepted, publishOutputAs(t, srv.URL, tc.publisher, tc.realm, topic, env))
			if tc.stream {
				// Drain the actual event: proves the old SSE persistence path
				// cannot turn a foreign or anonymous publish into an answer.
				for {
					if _, ok := stream.next(t, "published event")["artifactUpdate"]; ok {
						break
					}
				}
			}
			_, recorded, err := store.TaskOutput("owner", eventID)
			require.NoError(t, err)
			require.Equal(t, tc.want, recorded)
			_, recorded, err = store.TaskOutput("other", eventID)
			require.NoError(t, err)
			require.False(t, recorded)
			got := rpcResult(t, mustA2AOK(t, http.DefaultClient, srv.URL, getTaskBody("owner", id, "")))
			wantState := a2a.TaskStateWorking
			if tc.want {
				wantState = a2a.TaskStateCompleted
			}
			require.Equal(t, string(wantState), rpcTaskState(t, got))
			require.Equal(t, tc.want, got["metadata"].(map[string]any)["crier"].(map[string]any)["answered"])
			after, found, err := store.TaskObservation("owner", id)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, before, after, "output never rewrites lifecycle evidence")
			if tc.want {
				first, _, err := store.TaskOutput("owner", id)
				require.NoError(t, err)
				require.Equal(t, http.StatusAccepted, publishOutputAs(t, srv.URL, "owner", "", topic, outputEnvelope(id, "replacement")))
				next, _, err := store.TaskOutput("owner", id)
				require.NoError(t, err)
				require.Equal(t, first, next, "first qualifying nonempty output wins")
			}
		})
	}
}
