package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
)

// DF-CRIER-370: two real production processes share only the registry SQLite
// file. SIGTERM, process exit and a distinct PID rule out a handler-local map.
func TestA2AReplayAcrossProcessRestart(t *testing.T) {
	root := t.TempDir()
	var firstID string
	var firstTask map[string]any
	var firstPID int
	for _, phase := range []string{"send", "replay"} {
		port := freePort(t)
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestA2AReplayServerChild$", "-test.v", "-test.timeout=55s")
		cmd.Env = append(os.Environ(), "CRIER370_ROOT="+root, "CRIER370_PORT="+strconv.Itoa(port))
		logPath := filepath.Join(root, phase+".log")
		log, err := os.Create(logPath)
		require.NoError(t, err)
		defer func() { _ = log.Close() }()
		cmd.Stdout, cmd.Stderr = log, log
		require.NoError(t, cmd.Start())
		stopped := false
		defer func() {
			if !stopped {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()
		require.NotEqual(t, firstPID, cmd.Process.Pid)
		firstPID = cmd.Process.Pid
		base := fmt.Sprintf("http://127.0.0.1:%d", port)
		client := &http.Client{Timeout: 5 * time.Second}
		require.Eventually(t, func() bool {
			resp, err := client.Get(base + "/health")
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == 200
		}, 25*time.Second, 25*time.Millisecond)
		if phase == "send" {
			registerA2AAgent(t, base, "restart-a2a", true, nil)
		}
		post := func(key string) map[string]any {
			code, body, _ := a2aRPC(t, client, base, sendMessageBody("restart-a2a", a2a.MethodSendMessage, key))
			require.Equal(t, 200, code, "%s", body)
			result := rpcResult(t, body)
			task, ok := result["task"].(map[string]any)
			require.True(t, ok, "%s", body)
			return task
		}
		task := post("restart-message")
		id := task["id"].(string)
		t.Logf("boot=%s pid=%d messageId=restart-message task_id=%s replay=%v", phase, cmd.Process.Pid, id, task["metadata"])
		listBody := `{"jsonrpc":"2.0","id":2,"method":"ListTasks","params":{"tenant":"restart-a2a"}}`
		_, raw, _ := a2aRPC(t, client, base, listBody)
		tasks := rpcResult(t, raw)["tasks"].([]any)
		t.Logf("boot=%s tasks_total=%d", phase, len(tasks))
		require.Len(t, tasks, 1, "restart replay must not mint a second task: %s", raw)
		if phase == "send" {
			firstID, firstTask = id, task
			require.Equal(t, firstID, post("restart-message")["id"])
		} else {
			require.Equal(t, firstID, id)
			meta := task["metadata"].(map[string]any)["crier"].(map[string]any)
			require.Equal(t, true, meta["idempotent_replay"])
			delete(meta, "idempotent_replay")
			require.Equal(t, firstTask, task, "replay must retain the original task, history and timestamps")
			require.NotEqual(t, firstID, post("another-message")["id"])
			_, raw, _ = a2aRPC(t, client, base, listBody)
			require.Len(t, rpcResult(t, raw)["tasks"].([]any), 2)
		}
		// Signal the child PID, never the package test process or an in-process
		// server. This is the subprocess signal-contract pattern.
		require.NotEqual(t, os.Getpid(), cmd.Process.Pid)
		require.NoError(t, syscall.Kill(cmd.Process.Pid, syscall.SIGTERM))
		err = cmd.Wait()
		stopped = true
		require.NoError(t, err)
		logs, err := os.ReadFile(logPath)
		require.NoError(t, err)
		require.Contains(t, string(logs), "signal=terminated")
		t.Logf("boot=%s SIGTERM -> exit 0", phase)
	}
}

func TestA2AReplayServerChild(t *testing.T) {
	root := os.Getenv("CRIER370_ROOT")
	if root == "" {
		return
	}
	for key, value := range map[string]string{
		"CRIER_PORT": os.Getenv("CRIER370_PORT"), "CR_REGISTRY_BACKEND": "sqlite", "CR_REGISTRY_SQLITE_PATH": filepath.Join(root, "registry.db"),
		"CR_DATABASE_URL": "", "DATABASE_URL": "", "CRIER_DATABASE_URL": "", "CR_AUTH_TOKEN": "", "CR_GUARD_ENABLED": "false", "CR_REQUIRE_AGENT_SIG": "false", "CR_A2A_ENABLED": "true",
		"CR_SESSION_BACKEND": "sqlite", "CR_SQLITE_PATH": filepath.Join(root, "sessions.db"), "CR_GROUP_ROOT": filepath.Join(root, "groups"), "CR_PIDFILE": "",
	} {
		t.Setenv(key, value)
	}
	require.Equal(t, 0, run(nil))
}

// The canonical response marker is already part of the A2A metadata contract.
func TestA2AReplayTaskMetadataJSON(t *testing.T) {
	raw, err := json.Marshal(a2a.CrierMeta{IdempotentReplay: true})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"idempotent_replay":true`)
}
