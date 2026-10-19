package main

import (
	"context"
	"encoding/json"
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
func TestSQLiteRegistryProductionRestart(t *testing.T) {
	root := t.TempDir()
	for _, phase := range []string{"seed", "reopen"} {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteRegistryProductionChild$", "-test.v", "-test.timeout=40s")
		cmd.Env = append(os.Environ(), "CRIER364_PHASE="+phase, "CRIER364_ROOT="+root)
		out, err := cmd.CombinedOutput()
		cancel()
		require.NoError(t, err, "%s child:\n%s", phase, out)
		require.Contains(t, string(out), "type=sqlite durable=true")
		require.NotContains(t, string(out), "DEMO-ONLY")
		t.Logf("%s production boot: SQLite durable=true, no DEMO-ONLY warning", phase)
	}
}
func TestSQLiteRegistryProductionChild(t *testing.T) {
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
	if phase == "seed" {
		for _, id := range []string{"a", "b"} {
			code, raw := sessionDo(t, client, "POST", base+"/agents", map[string]any{"id": id}, nil)
			require.Equal(t, 201, code, "%s", raw)
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
		}
	}
	var stats struct {
		QueueDepth  int `json:"queue_depth"`
		LeasedCount int `json:"leased_count"`
	}
	code, raw := sessionDo(t, client, "GET", base+"/agents/a/inbox/stats", nil, &stats)
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
