package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
)

func publishAgentAnswer(t *testing.T, base, agentID, taskID string) {
	t.Helper()
	topic, ok := a2a.TaskTopic(taskID)
	require.True(t, ok)
	env, err := a2a.EnvelopeFromMessage(&a2a.Message{MessageID: "answer-355", TaskID: taskID, Role: a2a.RoleAgent, Parts: []a2a.Part{a2a.TextPart("canary deployed")}})
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, publishOutputAs(t, base, agentID, "", topic, env))
}

// This drives the production listener and real publish/ack routes, not an
// injected TaskEvidence. It proves output after consumption still reaches
// polling clients, ListTasks agrees, and only explicit task output counts.
func TestA2AAnswerObservedHTTP(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_A2A_ENABLED": "true"})
	client := &http.Client{Timeout: 5 * time.Second}
	const agent = "answer-agent-355"
	priv := registerA2AAgent(t, base, agent, true, nil)
	stream := openSSE(t, base, sendMessageBody(agent, a2a.MethodSendStreamingMessage, "request-355"))
	opened := stream.next(t, "opening task")["task"].(map[string]any)
	id := taskIDOf(t, opened)
	code, raw := signedAs(t, client, http.MethodGet, base, "/agents/"+agent+"/inbox", agent, priv, nil)
	require.Equal(t, http.StatusOK, code)
	var inbox struct {
		LeaseID string `json:"lease_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &inbox))
	stream.nextStatus(t, string(a2a.TaskStateWorking))
	ack := fmt.Sprintf(`{"lease_id":%q,"message_ids":[%q]}`, inbox.LeaseID, id)
	code, raw = signedAs(t, client, http.MethodPost, base, "/agents/"+agent+"/inbox/ack", agent, priv, []byte(ack))
	require.Equal(t, http.StatusNoContent, code, string(raw))
	consumed := stream.next(t, "consumed but unanswered")["statusUpdate"].(map[string]any)
	require.Equal(t, string(a2a.TaskStateWorking), rpcTaskState(t, consumed))
	require.Contains(t, terminalStatusText(t, consumed), "no answer observed yet")
	var lastGetRaw []byte
	get := func() map[string]any {
		lastGetRaw = mustA2AOK(t, client, base, getTaskBody(agent, id, `"historyLength":0`))
		return rpcResult(t, lastGetRaw)
	}
	assertList := func(want map[string]any) {
		listing := rpcResult(t, mustA2AOK(t, client, base, listTasksBody(agent, `"includeArtifacts":true,"historyLength":0`)))
		tasks := rpcTasks(t, listing)
		require.Len(t, tasks, 1)
		// includeArtifacts=true deliberately makes absence an explicit empty array.
		if _, present := want["artifacts"]; !present {
			want["artifacts"] = []any{}
		}
		require.Equal(t, want, tasks[0])
	}
	noAnswer := get()
	noAnswerRaw := append([]byte(nil), lastGetRaw...)
	require.Equal(t, string(a2a.TaskStateWorking), rpcTaskState(t, noAnswer))
	meta := noAnswer["metadata"].(map[string]any)["crier"].(map[string]any)
	require.Equal(t, false, meta["answered"])
	require.Equal(t, a2a.StateBasisAcknowledged, meta["state_basis"])
	require.NotContains(t, meta, "answer_observed_at")
	assertList(noAnswer)
	workingOnly := rpcResult(t, mustA2AOK(t, client, base, listTasksBody(agent, `"status":"TASK_STATE_WORKING"`)))
	require.Len(t, rpcTasks(t, workingOnly), 1)
	completedOnly := rpcResult(t, mustA2AOK(t, client, base, listTasksBody(agent, `"status":"TASK_STATE_COMPLETED"`)))
	require.Empty(t, rpcTasks(t, completedOnly))

	topic, ok := a2a.TaskTopic(id)
	require.True(t, ok)
	for _, event := range []any{
		map[string]any{"note": "progress is not an answer"},
		map[string]any{"parts": []any{map[string]any{"type": "text", "text": "another request"}}, "role": a2a.RoleUser, "task_id": id},
		map[string]any{"parts": []any{map[string]any{"type": "text", "text": "foreign output"}}, "role": a2a.RoleAgent, "task_id": "other-task"},
	} {
		body, err := json.Marshal(map[string]any{"topic": topic, "event": event})
		require.NoError(t, err)
		resp, err := publishToRelay(base, string(body))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		stream.next(t, "non-answer artifact")
		require.Equal(t, string(a2a.TaskStateWorking), rpcTaskState(t, get()))
	}
	publishAgentAnswer(t, base, agent, id)
	stream.next(t, "agent answer artifact")
	completed := stream.nextStatus(t, string(a2a.TaskStateCompleted))
	require.Contains(t, terminalStatusText(t, completed), "an answer was observed")
	stream.waitClosed(t)
	answered := get()
	answeredRaw := append([]byte(nil), lastGetRaw...)
	require.Equal(t, string(a2a.TaskStateCompleted), rpcTaskState(t, answered))
	meta = answered["metadata"].(map[string]any)["crier"].(map[string]any)
	require.Equal(t, true, meta["answered"])
	require.NotEmpty(t, meta["answer_observed_at"])
	artifact := answered["artifacts"].([]any)[0].(map[string]any)
	require.Equal(t, "canary deployed", artifact["parts"].([]any)[0].(map[string]any)["text"])
	assertList(answered)
	completedOnly = rpcResult(t, mustA2AOK(t, client, base, listTasksBody(agent, `"status":"TASK_STATE_COMPLETED"`)))
	require.Len(t, rpcTasks(t, completedOnly), 1)
	workingOnly = rpcResult(t, mustA2AOK(t, client, base, listTasksBody(agent, `"status":"TASK_STATE_WORKING"`)))
	require.Empty(t, rpcTasks(t, workingOnly))
	// A normal list still honors includeArtifacts=false even when output exists.
	listing := rpcResult(t, mustA2AOK(t, client, base, listTasksBody(agent, "")))
	require.NotContains(t, rpcTasks(t, listing)[0], "artifacts")
	if dir := os.Getenv("DF355_EVIDENCE_DIR"); dir != "" {
		require.NoError(t, os.MkdirAll(dir, 0700))
		for name, raw := range map[string][]byte{"acked-no-answer": noAnswerRaw, "answered": answeredRaw} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name+".json"), raw, 0600))
		}
	}
}

func TestA2AAnswerNotInCreatingHistory(t *testing.T) {
	base, store := terminalHarness(t)
	id, stream := terminalSubscription(t, base)
	_, lease, err := store.Retrieve("terminal-agent", time.Minute, 1)
	require.NoError(t, err)
	require.NoError(t, store.Ack("terminal-agent", lease, []string{id}))
	task := rpcResult(t, mustA2AOK(t, http.DefaultClient, base, getTaskBody("terminal-agent", id, "\"historyLength\":0")))
	require.Equal(t, string(a2a.TaskStateWorking), rpcTaskState(t, task))
	require.NotContains(t, task, "history")
	require.Contains(t, terminalStatusText(t, task), "no answer observed yet")
	require.NoError(t, stream.body.Body.Close())
	stream.waitClosed(t)
}
