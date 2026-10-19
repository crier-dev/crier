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
	"github.com/crier-dev/crier/internal/registry"
)

// DF-CRIER-390: the default memory registry loses its replay state on restart;
// SQLite retains it. Both must state their actual guarantee on the send wire.
func TestA2AReplayAcrossProcessRestart(t *testing.T) {
	for _, backend := range []string{"sqlite", "memory"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			var firstID string
			var firstTask map[string]any
			var firstPID int
			for _, phase := range []string{"send", "replay"} {
				base, pid, stop := bootA2AReplayChild(t, root, backend, phase, "")
				require.NotEqual(t, firstPID, pid)
				firstPID = pid
				if phase == "send" || backend == "memory" {
					registerA2AAgent(t, base, "restart-a2a", true, nil)
				}
				post := func(key string) map[string]any {
					return replaySendTask(t, base, key)
				}
				task := post("restart-message")
				id := task["id"].(string)
				replay, durability := "new", "process"
				if backend == "sqlite" {
					durability = "durable"
					if phase == "replay" {
						replay = "replayed"
					}
				}
				assertReplayCard(t, base, durability, registry.DefaultIdempotencyWindow.Seconds())
				assertReplayLabel(t, task, durability, replay, registry.DefaultIdempotencyWindow.Seconds())
				require.Len(t, replayListTasks(t, base), 1)
				if phase == "send" {
					firstID, firstTask = id, task
					retry := post("restart-message")
					require.Equal(t, firstID, retry["id"])
					assertReplayLabel(t, retry, durability, "replayed", registry.DefaultIdempotencyWindow.Seconds())
				} else if backend == "sqlite" {
					require.Equal(t, firstID, id)
					meta := task["metadata"].(map[string]any)["crier"].(map[string]any)
					require.Equal(t, true, meta["idempotent_replay"])
					delete(meta, "idempotent_replay")
					meta["idempotency"].(map[string]any)["replay"] = "new"
					require.Equal(t, firstTask, task, "replay must retain the original task, history and timestamps")
				} else {
					require.NotEqual(t, firstID, id, "two memory boots must mint two tasks, not claim restart replay")
					t.Logf("two memory boots minted task_ids=%s,%s (first inbox was lost)", firstID, id)
				}
				if phase == "replay" {
					require.NotEqual(t, id, post("another-message")["id"])
					require.Len(t, replayListTasks(t, base), 2)
				}
				stop()
			}
		})
	}
}

func TestA2AReplayWindowReportedAfterExpiry(t *testing.T) {
	for _, backend := range []string{"sqlite", "memory"} {
		t.Run(backend, func(t *testing.T) {
			base, _, stop := bootA2AReplayChild(t, t.TempDir(), backend, "window", "1")
			defer stop()
			registerA2AAgent(t, base, "restart-a2a", true, nil)
			first := replaySendTask(t, base, "expired-message")
			// Real production config and wall clock: no fake store or replay decision.
			time.Sleep(2 * time.Second)
			second := replaySendTask(t, base, "expired-message")
			require.NotEqual(t, first["id"], second["id"])
			require.Len(t, replayListTasks(t, base), 2)
			durability := "process"
			if backend == "sqlite" {
				durability = "durable"
			}
			assertReplayCard(t, base, durability, 1)
			assertReplayLabel(t, first, durability, "new", 1)
			assertReplayLabel(t, second, durability, "new", 1)
		})
	}
}

func replaySendTask(t *testing.T, base, key string) map[string]any {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	code, body, _ := a2aRPC(t, client, base, sendMessageBody("restart-a2a", a2a.MethodSendMessage, key))
	require.Equal(t, 200, code, "%s", body)
	t.Logf("messageId=%s actual_body=%s", key, body)
	task, ok := rpcResult(t, body)["task"].(map[string]any)
	require.True(t, ok, "%s", body)
	return task
}

func replayListTasks(t *testing.T, base string) []any {
	t.Helper()
	_, raw, _ := a2aRPC(t, &http.Client{Timeout: 5 * time.Second}, base,
		`{"jsonrpc":"2.0","id":2,"method":"ListTasks","params":{"tenant":"restart-a2a"}}`)
	return rpcResult(t, raw)["tasks"].([]any)
}

func assertReplayLabel(t *testing.T, task map[string]any, durability, replay string, seconds float64) {
	t.Helper()
	meta := task["metadata"].(map[string]any)["crier"].(map[string]any)
	label, ok := meta["idempotency"].(map[string]any)
	require.True(t, ok, "send response lacks metadata.crier.idempotency: %v", meta)
	require.Equal(t, durability, label["durability"])
	require.Equal(t, replay, label["replay"])
	require.Equal(t, seconds, label["window_seconds"])
	if durability == "process" {
		require.Contains(t, label["description"], "life of the process only")
		require.Contains(t, label["description"], "restart may mint a second task for the same messageId")
	}
}

func bootA2AReplayChild(t *testing.T, root, backend, phase, window string) (string, int, func()) {
	t.Helper()
	port := freePort(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestA2AReplayServerChild$", "-test.v", "-test.timeout=55s")
	cmd.Env = append(os.Environ(), "CRIER390_ROOT="+root, "CRIER390_PORT="+strconv.Itoa(port), "CRIER390_BACKEND="+backend, "CRIER390_WINDOW="+window)
	logPath := filepath.Join(root, phase+".log")
	log, err := os.Create(logPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	cmd.Stdout, cmd.Stderr = log, log
	require.NoError(t, cmd.Start())
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
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
	return base, cmd.Process.Pid, func() {
		if stopped {
			return
		}
		// Signal the explicit child PID, never the package test process.
		require.NotEqual(t, os.Getpid(), cmd.Process.Pid)
		require.NoError(t, syscall.Kill(cmd.Process.Pid, syscall.SIGTERM))
		err := cmd.Wait()
		stopped = true
		require.NoError(t, err)
		logs, err := os.ReadFile(logPath)
		require.NoError(t, err)
		require.Contains(t, string(logs), "signal=terminated")
		t.Logf("backend=%s boot=%s pid=%d SIGTERM -> exit 0", backend, phase, cmd.Process.Pid)
	}
}

func TestA2AReplayServerChild(t *testing.T) {
	root := os.Getenv("CRIER390_ROOT")
	if root == "" {
		return
	}
	backend := os.Getenv("CRIER390_BACKEND")
	if backend == "memory" {
		backend = "" // Exercise the default deployment, not an explicit memory override.
	}
	for key, value := range map[string]string{
		"CRIER_PORT": os.Getenv("CRIER390_PORT"), "CR_REGISTRY_BACKEND": backend, "CR_REGISTRY_SQLITE_PATH": filepath.Join(root, "registry.db"),
		"CR_IDEMPOTENCY_WINDOW_S": os.Getenv("CRIER390_WINDOW"),
		"CR_DATABASE_URL":         "", "DATABASE_URL": "", "CRIER_DATABASE_URL": "", "CR_AUTH_TOKEN": "", "CR_GUARD_ENABLED": "false", "CR_REQUIRE_AGENT_SIG": "false", "CR_A2A_ENABLED": "true",
		"CR_SESSION_BACKEND": "sqlite", "CR_SQLITE_PATH": filepath.Join(root, "sessions.db"), "CR_GROUP_ROOT": filepath.Join(root, "groups"), "CR_PIDFILE": "",
	} {
		t.Setenv(key, value)
	}
	require.Equal(t, 0, run(nil))
}

// The canonical response marker remains part of the A2A metadata contract.
func TestA2AReplayTaskMetadataJSON(t *testing.T) {
	raw, err := json.Marshal(a2a.CrierMeta{IdempotentReplay: true})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"idempotent_replay":true`)
}
