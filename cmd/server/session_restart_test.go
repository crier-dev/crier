package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// DF-CRIER-357: boot the production composition in distinct OS processes, so
// neither the session handler nor the registry's receipt cache can mask a lost
// durable request key. Only the remote harness endpoint is a test double.
func TestSessionRequestReplayAcrossProcesses(t *testing.T) {
	for _, backend := range []string{"sqlite", "jsonl"} {
		for _, route := range []string{"messages", "tasks"} {
			t.Run(backend+"/"+route, func(t *testing.T) {
				var calls atomic.Int32
				hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					_, _ = w.Write([]byte(`{"live":true,"reply":"durable restart answer"}`))
				}))
				t.Cleanup(hook.Close)
				root := t.TempDir()
				runChild := func(phase string) {
					t.Helper()
					ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionRequestRestartChild$", "-test.v", "-test.timeout=40s")
					cmd.Env = append(os.Environ(), "CRIER_REQUEST_RESTART_CHILD="+phase,
						"CRIER_REQUEST_RESTART_ROOT="+root, "CRIER_REQUEST_RESTART_BACKEND="+backend,
						"CRIER_REQUEST_RESTART_ROUTE="+route, "CRIER_REQUEST_RESTART_HOOK="+hook.URL)
					out, err := cmd.CombinedOutput()
					require.NoError(t, err, "%s child (target invocations=%d):\n%s", phase, calls.Load(), out)
					require.Contains(t, string(out), "--- PASS: TestSessionRequestRestartChild")
				}
				runChild("send") // also retries inside that process
				require.EqualValues(t, 1, calls.Load(), "same-process retry dispatched twice")
				runChild("replay")
				t.Logf("target invocations before restart=1 after restart replay=%d", calls.Load())
				require.EqualValues(t, 1, calls.Load(), "restart replay must not invoke the harness")
				runChild("independent") // different key, then same key in another room
				require.EqualValues(t, 3, calls.Load(), "different keys/sessions must execute")
			})
		}
	}
}

type restartRequestEvidence struct {
	Room       string                     `json:"room"`
	Response   map[string]json.RawMessage `json:"response"`
	Transcript json.RawMessage            `json:"transcript"`
}

// Helper process: no package-level state from the sender survives a replay.
func TestSessionRequestRestartChild(t *testing.T) {
	parallelBoot(t)
	phase := os.Getenv("CRIER_REQUEST_RESTART_CHILD")
	if phase == "" {
		return
	}
	root := os.Getenv("CRIER_REQUEST_RESTART_ROOT")
	route := os.Getenv("CRIER_REQUEST_RESTART_ROUTE")
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":   os.Getenv("CRIER_REQUEST_RESTART_BACKEND"),
		"CR_SQLITE_PATH":       filepath.Join(root, "sessions.sqlite"),
		"CR_SESSION_LOG_ROOT":  filepath.Join(root, "sessions"),
		"CR_GROUP_LOG_ROOT":    filepath.Join(root, "groups"),
		"CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 10 * time.Second}
	// The registry is deliberately fresh on every boot: even its target and
	// webhook registration must be restored, ruling out inbox dedup as a proxy.
	code, raw := sessionDo(t, client, http.MethodPost, base+"/agents", map[string]any{
		"id": "restart-target", "public_key": strings.Repeat("ab", 32),
		"webhook": map[string]string{"url": os.Getenv("CRIER_REQUEST_RESTART_HOOK"), "delivery_mode": "blocking"},
	}, nil)
	require.Equal(t, http.StatusCreated, code, "%s", raw)
	createRoom := func() string {
		t.Helper()
		var room struct {
			ID string `json:"id"`
		}
		code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions", map[string]any{
			"title": "restart", "created_by": map[string]string{"principal": "operator"},
			"members": []map[string]string{{"member_type": "agent", "member_id": "restart-target", "role": "member"}},
		}, &room)
		require.Equal(t, http.StatusCreated, code, "%s", raw)
		return room.ID
	}
	post := func(room, key string) map[string]json.RawMessage {
		t.Helper()
		var out map[string]json.RawMessage
		code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions/"+room+"/"+route, map[string]any{
			"sender": "operator-agent", "payload": map[string]string{"text": "do work"},
			"idempotency_key": key, "targets": []map[string]string{{"kind": "agent", "id": "restart-target"}},
		}, &out)
		require.Equal(t, http.StatusCreated, code, "%s", raw)
		var outcomes []struct {
			Outcome string `json:"outcome"`
		}
		require.NoError(t, json.Unmarshal(out["outcomes"], &outcomes))
		require.Len(t, outcomes, 1)
		require.Equal(t, "delivered", outcomes[0].Outcome, "the test must execute, not silently refuse")
		// The existing replay marker is the sole intentional response delta.
		delete(out, "replay_action")
		return out
	}
	transcript := func(room string) json.RawMessage {
		t.Helper()
		code, raw := sessionDo(t, client, http.MethodGet, base+"/sessions/"+room+"/messages", nil, nil)
		require.Equal(t, http.StatusOK, code, "%s", raw)
		return json.RawMessage(raw)
	}
	path := filepath.Join(root, "evidence.json")
	if phase == "send" {
		room := createRoom()
		first := post(room, "persistent-key")
		before := transcript(room)
		require.Equal(t, first, post(room, "persistent-key"))
		require.JSONEq(t, string(before), string(transcript(room)))
		evidence, err := json.Marshal(restartRequestEvidence{Room: room, Response: first, Transcript: before})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, evidence, 0o600))
		return
	}
	rawEvidence, err := os.ReadFile(path)
	require.NoError(t, err)
	var first restartRequestEvidence
	require.NoError(t, json.Unmarshal(rawEvidence, &first))
	switch phase {
	case "replay":
		// Compare the entire original result, including seq, task id, outcome
		// detail and timestamps; an equal message id alone is insufficient.
		require.Equal(t, first.Response, post(first.Room, "persistent-key"))
		require.JSONEq(t, string(first.Transcript), string(transcript(first.Room)))
	case "independent":
		differentKey := post(first.Room, "another-key")
		require.NotEqual(t, first.Response["id"], differentKey["id"])
		differentRoom := post(createRoom(), "persistent-key")
		require.NotEqual(t, first.Response["id"], differentRoom["id"])
	default:
		t.Fatalf("unknown helper phase %q", phase)
	}
}
