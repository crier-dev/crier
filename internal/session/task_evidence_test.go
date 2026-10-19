package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise real routes on both persistent backends, including reopen. Refusals
// must not append a lifecycle event or change the owner's task.
func TestTaskCompletionRequiresOwnerAndEvidence(t *testing.T) {
	for _, b := range localBackends() {
		t.Run(b.name, func(t *testing.T) {
			path := b.pathFn(t)
			store := b.open(t, path)
			h := newAPIHarness(t, store)
			room := h.createRoom(t)
			var created taskCreateView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/tasks", map[string]any{
				"sender": fixAgentA, "targets": []AudienceTarget{{Kind: TargetAgent, ID: fixAgentB}},
				"payload": map[string]string{"text": "produce an artifact"},
			}, &created, nil))
			url := "/sessions/" + room + "/tasks/" + created.TaskID
			read := func() map[string]any {
				t.Helper()
				var v map[string]any
				require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, url, nil, &v, nil))
				return v
			}
			open := read()
			require.Equal(t, "open", open["state"])
			require.Equal(t, "unclaimed", open["claim_status"])
			require.Equal(t, url+"/claim", open["claim_url"])
			require.Empty(t, open["owner"])
			require.Equal(t, float64(1), open["versions"])
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, url+"/claim", map[string]any{"actor": fixAgentB}, nil, nil))
			claimed := read()
			for _, state := range []string{"done", "failed"} {
				for _, actor := range []map[string]any{
					{"actor": fixAgentA}, {"actor": "typo"},
					{"actor": fixAgentB, "actor_kind": "principal"},
				} {
					actor["new_state"] = state
					actor["payload"] = map[string]string{"artifact": "file:///result.json"}
					var refused map[string]any
					require.Equal(t, http.StatusConflict, h.do(t, http.MethodPost, url+"/complete", actor, &refused, nil))
					require.Equal(t, "TASK_OWNER_MISMATCH", refused["error"])
					require.Contains(t, refused["detail"], fixAgentB)
					require.Contains(t, refused["detail"], actor["actor"])
					require.Equal(t, claimed, read(), "refusal changed task history")
				}
			}
			for _, payload := range []any{nil, map[string]any{}, []any{}, "  ", map[string]any{"text": "", "artifacts": []any{nil}}} {
				var refused map[string]any
				require.Equal(t, http.StatusConflict, h.do(t, http.MethodPost, url+"/complete", map[string]any{"actor": fixAgentB, "payload": payload}, &refused, nil))
				require.Equal(t, "TASK_EVIDENCE_REQUIRED", refused["error"])
				require.Equal(t, claimed, read())
			}
			evidence := map[string]any{"artifact": "file:///result.json", "rows": float64(0)}
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, url+"/complete", map[string]any{"actor": fixAgentB, "payload": evidence}, nil, nil))
			done := read()
			require.Equal(t, "done", done["state"])
			require.Equal(t, evidence, done["evidence"])
			require.Equal(t, float64(3), done["versions"])
			var list struct {
				Tasks []map[string]any `json:"tasks"`
			}
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+room+"/tasks", nil, &list, nil))
			require.Equal(t, done, list.Tasks[0])
			h.srv.Close()
			require.NoError(t, store.Close())
			reopened := b.open(t, path)
			t.Cleanup(func() { _ = reopened.Close() })
			h = newAPIHarness(t, reopened)
			require.Equal(t, done, read(), "completion evidence must survive reopen")
		})
	}
}

func TestTaskFailureByOwnerDoesNotRequireSuccessEvidence(t *testing.T) {
	h := newTriggerHarness(t)
	room := h.createRoom(t)
	code, created := h.createTask(t, room)
	require.Equal(t, http.StatusCreated, code)
	url := "/sessions/" + room + "/tasks/" + created.Task["task_id"].(string)
	require.Equal(t, http.StatusCreated, h.srv.do(t, http.MethodPost, url+"/claim", map[string]any{"actor": fixAgentB}, nil, nil))
	require.Equal(t, http.StatusCreated, h.srv.do(t, http.MethodPost, url+"/complete", map[string]any{"actor": fixAgentB, "new_state": "failed"}, nil, nil))
	var v taskReadView
	require.Equal(t, http.StatusOK, h.srv.do(t, http.MethodGet, url, nil, &v, nil))
	require.Equal(t, "failed", v.State)
	require.Equal(t, fixAgentB, v.Owner)
	require.Equal(t, 3, v.Versions)
}

// Each refusal is independent, so a negative control tests owner and evidence
// enforcement directly rather than stopping first on an additive view field.
func TestTaskCompletionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, actor, state, code string
	}{
		{"wrong owner done", fixAgentA, "done", "TASK_OWNER_MISMATCH"},
		{"wrong owner failed", fixAgentA, "failed", "TASK_OWNER_MISMATCH"},
		{"owner no evidence", fixAgentB, "done", "TASK_EVIDENCE_REQUIRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTriggerHarness(t)
			room := h.createRoom(t)
			status, created := h.createTask(t, room)
			require.Equal(t, http.StatusCreated, status)
			url := "/sessions/" + room + "/tasks/" + created.Task["task_id"].(string)
			require.Equal(t, http.StatusCreated, h.srv.do(t, http.MethodPost, url+"/claim", map[string]any{"actor": fixAgentB}, nil, nil))
			var refused map[string]any
			require.Equal(t, http.StatusConflict, h.srv.do(t, http.MethodPost, url+"/complete", map[string]any{"actor": tc.actor, "new_state": tc.state}, &refused, nil))
			require.Equal(t, tc.code, refused["error"])
			var got taskReadView
			require.Equal(t, http.StatusOK, h.srv.do(t, http.MethodGet, url, nil, &got, nil))
			require.Equal(t, "claimed", got.State)
			require.Equal(t, fixAgentB, got.Owner)
			require.Equal(t, 2, got.Versions)
		})
	}
}

func TestTaskCompletionEvidenceReaderRefusesUnavailableOrEmpty(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    json.RawMessage
		err    error
		status int
		code   string
	}{
		{"unavailable", nil, errors.New("journal read failed"), http.StatusServiceUnavailable, "TASK_EVIDENCE_UNAVAILABLE"},
		{"empty reader", json.RawMessage(`{}`), nil, http.StatusConflict, "TASK_EVIDENCE_REQUIRED"},
		{"malformed reader", json.RawMessage(`{`), nil, http.StatusConflict, "TASK_EVIDENCE_REQUIRED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTriggerHarness(t)
			var called bool
			h.handler.SetTaskEvidence(func(_ context.Context, sessionID, taskID string) (json.RawMessage, error) {
				called = true
				require.NotEmpty(t, sessionID)
				require.NotEmpty(t, taskID)
				return tc.raw, tc.err
			})
			room := h.createRoom(t)
			status, created := h.createTask(t, room)
			require.Equal(t, http.StatusCreated, status)
			url := "/sessions/" + room + "/tasks/" + created.Task["task_id"].(string)
			require.Equal(t, http.StatusCreated, h.srv.do(t, http.MethodPost, url+"/claim", map[string]any{"actor": fixAgentB}, nil, nil))
			var refused map[string]any
			require.Equal(t, tc.status, h.srv.do(t, http.MethodPost, url+"/complete", map[string]any{"actor": fixAgentB}, &refused, nil))
			require.Equal(t, tc.code, refused["error"])
			require.True(t, called)
			var got taskReadView
			require.Equal(t, http.StatusOK, h.srv.do(t, http.MethodGet, url, nil, &got, nil))
			require.Equal(t, "claimed", got.State)
			require.Equal(t, 2, got.Versions)
		})
	}
}
