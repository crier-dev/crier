package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
	parallelBoot(t) // subprocess/snapshot boot: parent-side env-free (INT-CI-014)
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

// The child boots the production server from a cold start (exec + test-binary
// startup + run()); MEASURED on this host (16 CPUs) at one-minute loadavg
// 13-15, GET /health first answers ~0.3-0.4 s after exec, so a healthy boot
// costs a few hundred milliseconds. A FIXED 25 s Eventually window failed on
// 2026-10-19 (DF-CRIER-425) at one-minute loadavg 19-25: the child's boot
// exceeded the window under host contention and a scheduling delay presented
// exactly like a behavioural regression, with only "Condition never satisfied"
// and none of the child's output.
//
// The window is therefore DERIVED from the measurement instead of picked: 10x
// the measured boot cost, scaled by the host's runnable queue per CPU above
// one queue per CPU (a cold process boot is mostly scheduling, and its wall
// clock grows with the queue), floored at 5 s so an idle host keeps real
// headroom, and capped at 50 s to stay clear of the child's own
// -test.timeout=55s, after which the child would die under us and the poll
// would report a harness limit as a boot failure. The derivation is logged and
// a timeout failure carries the child's log tail, so a future failure states
// its own conditions instead of repeating an unexplained number.
const (
	a2aBootMeasuredCost   = 400 * time.Millisecond // measured healthy child boot on this host
	a2aBootBudgetMultiple = 10                     // headroom over that measurement (=> 4s unloaded)
	a2aBootFloor          = 5 * time.Second        // idle-host headroom
	a2aBootCeiling        = 50 * time.Second       // under the child's own -test.timeout=55s
)

// a2aBootWindow derives this run's child-boot window from the measured cost
// and the host's current load, and LOGS the derivation.
func a2aBootWindow(t *testing.T) time.Duration {
	t.Helper()
	load, factor := -1.0, 1.0
	cpus := runtime.NumCPU()
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) > 0 {
			if parsed, perr := strconv.ParseFloat(fields[0], 64); perr == nil {
				load = parsed
			}
		}
	}
	// A boot whose wall clock is mostly scheduling grows with the runnable
	// queue per CPU; at or below one queue per CPU the host is not contended
	// and the factor stays 1.
	if load >= 0 && cpus > 0 && load > float64(cpus) {
		factor = load / float64(cpus)
	}
	window := time.Duration(float64(a2aBootMeasuredCost) * float64(a2aBootBudgetMultiple) * factor)
	if window < a2aBootFloor {
		window = a2aBootFloor
	}
	if window > a2aBootCeiling {
		window = a2aBootCeiling
	}
	if load >= 0 {
		t.Logf("a2a child boot window: %s = measured %s x%d, loadavg=%.2f over %d CPUs (factor %.2f, floor %s, ceiling %s)",
			window, a2aBootMeasuredCost, a2aBootBudgetMultiple, load, cpus, factor, a2aBootFloor, a2aBootCeiling)
	} else {
		t.Logf("a2a child boot window: %s = measured %s x%d (loadavg unavailable, factor 1.00, floor %s, ceiling %s)",
			window, a2aBootMeasuredCost, a2aBootBudgetMultiple, a2aBootFloor, a2aBootCeiling)
	}
	return window
}

// a2aChildLogTail returns the last ~2KB of the child's combined stdout/stderr
// log, so a boot failure names what the child actually printed instead of
// "Condition never satisfied".
func a2aChildLogTail(t *testing.T, logPath string) string {
	t.Helper()
	logs, err := os.ReadFile(logPath)
	if err != nil {
		return fmt.Sprintf("(child log %s unreadable: %v)", logPath, err)
	}
	s := string(logs)
	if len(s) > 2048 {
		s = s[len(s)-2048:]
	}
	if strings.TrimSpace(s) == "" {
		return "(child log empty — the child produced no output before the window expired)"
	}
	return s
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
	bootWindow := a2aBootWindow(t)
	bootStart := time.Now()
	// Poll manually instead of require.Eventually so the timeout failure can
	// carry the child's log tail, read at failure time.
	booted := false
	for deadline := time.Now().Add(bootWindow); time.Now().Before(deadline); {
		resp, err := client.Get(base + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				booted = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !booted {
		t.Fatalf("child did not boot within %s (backend=%s phase=%s); child log tail:\n%s",
			bootWindow, backend, phase, a2aChildLogTail(t, logPath))
	}
	t.Logf("backend=%s %s phase: child booted (pid %d) in %s of the %s window",
		backend, phase, cmd.Process.Pid, time.Since(bootStart), bootWindow)
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
