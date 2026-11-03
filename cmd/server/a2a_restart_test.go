package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	parallelBoot(t) // subprocess/snapshot boot: parent-side env-free (INT-CI-014)
	plan := planChildBoot(t)
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
					return replaySendTask(t, plan, base, key)
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
				require.Len(t, replayListTasks(t, plan, base), 1)
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
					require.Len(t, replayListTasks(t, plan, base), 2)
				}
				stop()
			}
		})
	}
}

func TestA2AReplayWindowReportedAfterExpiry(t *testing.T) {
	parallelBoot(t) // subprocess/snapshot boot: parent-side env-free (INT-CI-014)
	plan := planChildBoot(t)
	for _, backend := range []string{"sqlite", "memory"} {
		t.Run(backend, func(t *testing.T) {
			base, _, stop := bootA2AReplayChild(t, t.TempDir(), backend, "window", "1")
			defer stop()
			registerA2AAgent(t, base, "restart-a2a", true, nil)
			first := replaySendTask(t, plan, base, "expired-message")
			// Real production config and wall clock: no fake store or replay decision.
			time.Sleep(2 * time.Second)
			second := replaySendTask(t, plan, base, "expired-message")
			require.NotEqual(t, first["id"], second["id"])
			require.Len(t, replayListTasks(t, plan, base), 2)
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

func replaySendTask(t *testing.T, plan childBootPlan, base, key string) map[string]any {
	t.Helper()
	// INT-CI-023: the request budget is derived, not the old flat 5s. The
	// recorded INT-CI-023 a2a failure was this call's client timing out at
	// exactly 5s against a LIVE server at loadavg 33.34 — a saturated host
	// delaying a request, not a boot that never happened.
	client := &http.Client{Timeout: plan.Request}
	code, body, _ := a2aRPC(t, client, base, sendMessageBody("restart-a2a", a2a.MethodSendMessage, key))
	require.Equal(t, 200, code, "%s", body)
	t.Logf("messageId=%s actual_body=%s", key, body)
	task, ok := rpcResult(t, body)["task"].(map[string]any)
	require.True(t, ok, "%s", body)
	return task
}

func replayListTasks(t *testing.T, plan childBootPlan, base string) []any {
	t.Helper()
	_, raw, _ := a2aRPC(t, &http.Client{Timeout: plan.Request}, base,
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

// The child boots the production server from a cold start (exec + test-binary
// startup + run()). The budget is no longer a hand-picked constant: it comes from
// planChildBoot (childboot_test.go), which derives every wait from ONE measured
// cold boot (childBootMeasuredCost: measured here as n=8 min 193ms / median 425ms
// / max 512ms at loadavg 23.75) scaled by the host's runnable queue per CPU. The
// previous fixed 25s Eventually window failed on 2026-10-19 (DF-CRIER-425) at
// loadavg 19-25; the replacement 400ms x10 derivation then used a measurement of
// the wrong phase and its log line reported the /health poll (sub-millisecond) as
// "bound its port". Both are fixed: the wait is one shared attempt budget, and the
// log line reports the real exec-to-port time.

// bootA2AReplayChild boots the production server child and returns its base URL,
// its pid and a stop function that asserts the shutdown contract.
func bootA2AReplayChild(t *testing.T, root, backend, phase, window string) (string, int, func()) {
	t.Helper()
	// INT-CI-022: the child binds its OWN ephemeral port (CRIER_PORT=0) and
	// reports it in the pidfile written only after that bind. The pre-fix
	// shape handed the child a freePort() number and asserted against it, so
	// a squatter that won the close-then-bind window made this helper's
	// /health poll succeed against a STRANGER — measured on this host as
	// "a2a_restart_test.go:110 expected 200 actual 404 {\"error\":\"not
	// found\"}": the squatter was another crier test server with no A2A
	// route. Port identity now comes from the pidfile, which names the pid.
	//
	// INT-CI-023: the boot is retried up to plan.Attempts times, each attempt a
	// FRESH child process with its own pidfile and log (attempt N of 3 is named
	// in every failure), the failed attempt is killed before the retry starts,
	// and a child that dies mid-boot is reported immediately with its exit code
	// and its own log tail instead of burning the rest of the budget.
	plan := planChildBoot(t)
	child, base := bootChildWithRetry(t, plan, func(attempt int) childSpawnSpec {
		spec := childSpawnSpec{
			Attempt:    attempt,
			Root:       root,
			Name:       fmt.Sprintf("%s-attempt-%d", phase, attempt),
			RunPattern: "^TestA2AReplayServerChild$",
			Timeout:    plan.Timeout,
		}
		spec.Env = []string{
			"CRIER390_ROOT=" + root,
			"CRIER390_PORT=0",
			"CRIER390_PIDFILE=" + spec.pidPath(),
			"CRIER390_BACKEND=" + backend,
			"CRIER390_WINDOW=" + window,
		}
		return spec
	})

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		// Signal the explicit child PID, never the package test process.
		require.NotEqual(t, os.Getpid(), child.cmd.Process.Pid)
		require.NoError(t, syscall.Kill(child.cmd.Process.Pid, syscall.SIGTERM))
		require.NoError(t, child.awaitExit(plan.Ctx), "child did not exit within %s of SIGTERM", plan.Ctx)
		require.NoError(t, child.waitErrValue())
		logs, err := os.ReadFile(child.logPath)
		require.NoError(t, err)
		require.Contains(t, string(logs), "signal=terminated")
		t.Logf("backend=%s boot=%s pid=%d SIGTERM -> exit 0 (attempt budget %s)",
			backend, phase, child.cmd.Process.Pid, plan.Attempt)
	}
	return base, child.cmd.Process.Pid, stop
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
		"CRIER_PORT": os.Getenv("CRIER390_PORT"), "CR_PIDFILE": os.Getenv("CRIER390_PIDFILE"),
		"CR_REGISTRY_BACKEND": backend, "CR_REGISTRY_SQLITE_PATH": filepath.Join(root, "registry.db"),
		"CR_IDEMPOTENCY_WINDOW_S": os.Getenv("CRIER390_WINDOW"),
		"CR_DATABASE_URL":         "", "DATABASE_URL": "", "CRIER_DATABASE_URL": "", "CR_AUTH_TOKEN": "", "CR_GUARD_ENABLED": "false", "CR_REQUIRE_AGENT_SIG": "false", "CR_A2A_ENABLED": "true",
		"CR_SESSION_BACKEND": "sqlite", "CR_SQLITE_PATH": filepath.Join(root, "sessions.db"), "CR_GROUP_ROOT": filepath.Join(root, "groups"),
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
