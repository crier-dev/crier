package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise run(), not a hand-built handler or just the config resolver.
//
// INT-CI-023: the child used to carry a 40s -test.timeout inside a 45s parent
// context while its own boot path was allowed 3 attempts x 2 waits x 20s = 120s.
// The numbers were picked independently, so the child died of its own timeout
// while it still had retry budget and the operator read a Go panic instead of a
// diagnosis. Both deadlines now come from planChildBoot, which derives them from
// the measured cold boot so `child -test.timeout > internal retry envelope` and
// `parent context > child timeout` hold at every load level.
func TestSQLiteRegistryProductionRestart(t *testing.T) {
	plan := planChildBoot(t)

	seedRoot := ""
	for _, phase := range []string{"seed", "reopen"} {
		out, dir := runRegistryPhaseChild(t, plan, phase, seedRoot)
		if phase == "seed" {
			// The reopen phase must read the state the SUCCESSFUL seed wrote,
			// which is why the seed's retry uses a fresh directory per attempt
			// and the winner is what gets carried forward.
			seedRoot = dir
		}
		require.Contains(t, string(out), "type=sqlite durable=true")
		require.NotContains(t, string(out), "DEMO-ONLY")
		require.Contains(t, string(out), "/health HTTP 200")
		if phase == "seed" {
			require.Contains(t, string(out), "agent a HTTP 201")
		} else {
			require.Contains(t, string(out), "agent a after restart HTTP 200")
		}
	}
}

// runRegistryPhaseChild runs one phase of the production-restart fixture in a real
// child process and returns its transcript plus the state directory it used.
//
// Each attempt gets its OWN log file, and a boot-budget failure is retried up to
// plan.Attempts times. The retry never reuses the failed attempt's state: a seed
// attempt is given a fresh directory (a child that already registered agents must
// not be retried against its own half-written SQLite registry — POST /agents on an
// existing id answers 409), while the reopen phase legitimately re-reads the
// directory the successful seed wrote. A failure that is NOT a boot-budget failure
// is never retried: a product verdict must not be re-rolled until it passes.
func runRegistryPhaseChild(t *testing.T, plan childBootPlan, phase, seedRoot string) ([]byte, string) {
	t.Helper()

	var failures []string
	for attempt := 1; attempt <= plan.Attempts; attempt++ {
		dir := seedRoot
		if phase == "seed" {
			dir = t.TempDir()
		}
		logPath := filepath.Join(t.TempDir(), fmt.Sprintf("%s-attempt-%d.log", phase, attempt))
		logFile, err := os.Create(logPath)
		require.NoError(t, err)

		ctx, cancel := context.WithTimeout(t.Context(), plan.Ctx)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteRegistryProductionChild$", "-test.v",
			fmt.Sprintf("-test.timeout=%s", plan.Timeout))
		cmd.Env = append(os.Environ(), "CRIER364_PHASE="+phase, "CRIER364_ROOT="+dir)
		cmd.Stdout, cmd.Stderr = logFile, logFile

		start := time.Now()
		runErr := cmd.Run()
		elapsed := time.Since(start)
		cancel()
		require.NoError(t, logFile.Close())

		out, err := os.ReadFile(logPath)
		require.NoError(t, err)

		if runErr == nil {
			t.Logf("%s production boot transcript (attempt %d/%d, %s, log %s):\n%s",
				phase, attempt, plan.Attempts, elapsed.Round(time.Millisecond), logPath, out)
			return out, dir
		}

		// A failed attempt must be diagnosable from its own words: the exit
		// status, which deadline (if any) fired, and the child's log tail — never
		// a bare "the child failed".
		failures = append(failures, fmt.Sprintf("attempt %d/%d (%s, log %s): %s\nchild log tail:\n%s",
			attempt, plan.Attempts, elapsed.Round(time.Millisecond), logPath,
			describeRegistryChildExit(cmd, ctx, runErr, elapsed, plan), fileTail(logPath, 2048)))
		if !retryableRegistryChildFailure(out) {
			break
		}
	}
	t.Fatalf("the %s child did not complete in %d attempt(s) "+
		"(child -test.timeout %s, parent context %s, both derived from a measured %s cold boot x%d at loadavg %.2f over %d CPUs):\n\n%s",
		phase, plan.Attempts, plan.Timeout, plan.Ctx, childBootMeasuredCost, childBootHeadroom,
		plan.Load, plan.CPUs, strings.Join(failures, "\n\n"))
	return nil, ""
}

// describeRegistryChildExit names which budget killed the child, so the next
// operator does not have to infer it from a generic "signal: killed".
func describeRegistryChildExit(cmd *exec.Cmd, ctx context.Context, runErr error, elapsed time.Duration, plan childBootPlan) string {
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return fmt.Sprintf("the parent context (%s) expired after %s: the child outlived its own %s timeout",
			plan.Ctx, elapsed.Round(time.Millisecond), plan.Timeout)
	case cmd.ProcessState != nil:
		return fmt.Sprintf("exit code %d after %s (%v)", cmd.ProcessState.ExitCode(),
			elapsed.Round(time.Millisecond), runErr)
	default:
		return fmt.Sprintf("%v after %s", runErr, elapsed.Round(time.Millisecond))
	}
}

// retryableRegistryChildFailure reports whether a failed child is worth another
// attempt. Exactly one class qualifies: the child's own boot-budget marker, i.e.
// its in-process retry envelope was exhausted by a slow host.
//
// Two classes are deliberately NOT retried, because retrying them would either
// mask a defect or triple the wall clock for the same answer:
//   - a product assertion failure (the child ran and verdicts were formed);
//   - the child's own -test.timeout panic or a parent-context kill. The derived
//     hierarchy makes ChildTimeout exceed the retry envelope and ParentCtx exceed
//     ChildTimeout, so either one firing means the boot HUNG rather than raced.
func retryableRegistryChildFailure(out []byte) bool {
	return strings.Contains(string(out), childBootBudgetMarker)
}

func TestSQLiteRegistryProductionChild(t *testing.T) {
	parallelBoot(t)
	phase := os.Getenv("CRIER364_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("CRIER364_ROOT")
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REGISTRY_BACKEND": "sqlite", "CR_REGISTRY_SQLITE_PATH": filepath.Join(root, "registry.db"),
		"CR_SESSION_BACKEND": "sqlite", "CR_SQLITE_PATH": filepath.Join(root, "sessions.db"),
		"CR_GROUP_ROOT": filepath.Join(root, "groups"), "CR_REQUIRE_AGENT_SIG": "false",
	})
	client := &http.Client{Timeout: 5 * time.Second}
	code, raw := sessionDo(t, client, "GET", base+"/health", nil, nil)
	require.Equal(t, http.StatusOK, code, "%s", raw)
	t.Logf("%s /health HTTP %d: %s", phase, code, raw)
	if phase == "seed" {
		for _, id := range []string{"a", "b"} {
			code, raw := sessionDo(t, client, "POST", base+"/agents", map[string]any{"id": id}, nil)
			require.Equal(t, 201, code, "%s", raw)
			t.Logf("agent %s HTTP %d: %s", id, code, raw)
		}
		code, raw := sessionDo(t, client, "POST", base+"/agents/a/inbox", map[string]any{"payload": map[string]string{"text": "survive restart"}}, nil)
		require.Equal(t, 201, code, "%s", raw)
		var retrieved struct {
			Messages []struct {
				Payload []byte `json:"payload"`
			} `json:"messages"`
		}
		code, raw = sessionDo(t, client, "GET", base+"/agents/a/inbox?lease_seconds=3600", nil, &retrieved)
		require.Equal(t, 200, code, "%s", raw)
		require.Len(t, retrieved.Messages, 1)
		require.Contains(t, string(retrieved.Messages[0].Payload), "survive restart")
	} else {
		for _, id := range []string{"a", "b"} {
			code, raw := sessionDo(t, client, "GET", base+"/agents/"+id, nil, nil)
			require.Equal(t, 200, code, "%s", raw)
			require.Contains(t, raw, `"id":"`+id+`"`)
			t.Logf("agent %s after restart HTTP %d: %s", id, code, raw)
		}
	}
	var stats struct {
		QueueDepth  int `json:"queue_depth"`
		LeasedCount int `json:"leased_count"`
	}
	code, raw = sessionDo(t, client, "GET", base+"/agents/a/inbox/stats", nil, &stats)
	require.Equal(t, 200, code, "%s", raw)
	require.Equal(t, 1, stats.QueueDepth)
	require.Equal(t, 1, stats.LeasedCount)
	var status map[string]json.RawMessage
	code, raw = sessionDo(t, client, "GET", base+"/status", nil, &status)
	require.Equal(t, 200, code, "%s", raw)
	require.Equal(t, `"sqlite"`, strings.TrimSpace(string(status["registry_backend"])))
}

func TestRegistryBackendInvalidBoot(t *testing.T) {
	for _, tc := range []struct{ backend, path, fragment string }{{"sqlite", "", "CR_REGISTRY_SQLITE_PATH"}, {"typo", "", "want memory, sqlite or postgres"}, {"sqlite", filepath.Join(t.TempDir(), "missing", "registry.db"), "initialize SQLite registry store"}} {
		t.Run(tc.backend+tc.fragment, func(t *testing.T) {
			t.Setenv("CR_REGISTRY_BACKEND", tc.backend)
			t.Setenv("CR_REGISTRY_SQLITE_PATH", tc.path)
			t.Setenv("CR_DATABASE_URL", "")
			t.Setenv("DATABASE_URL", "")
			t.Setenv("CRIER_DATABASE_URL", "")
			t.Setenv("CR_GUARD_ENABLED", "false")
			t.Setenv("CR_SESSION_BACKEND", "jsonl")
			out := captureStderr(t, func() { require.Equal(t, 1, run(nil)) })
			require.Contains(t, out, tc.fragment)
		})
	}
}
