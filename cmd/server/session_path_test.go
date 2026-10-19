package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

func TestStatusSessionStorePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	cfg := config.Config{Session: config.SessionConfig{Backend: "sqlite", SQLitePath: path}}
	var body map[string]any
	require.NoError(t, json.Unmarshal(httptestRecorder(t, cfg, "memory"), &body))
	require.Equal(t, path, body["session_store_path"], "posture must name the resolved session file, not only the backend")
	require.NotEmpty(t, body["session_store_mount"])
}

// Distinct production processes, one operator-selected path, no shared Go state.
// A different legacy fallback on each boot also catches an ignored alias even
// if the path assertion is accidentally removed in a later edit.
func TestSessionSQLiteAliasProductionRestart(t *testing.T) {
	root := t.TempDir()
	for _, phase := range []string{"seed", "reopen"} {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionSQLiteAliasChild$", "-test.v", "-test.timeout=40s")
		cmd.Env = append(os.Environ(), "CRIER413_PHASE="+phase, "CRIER413_ROOT="+root)
		out, err := cmd.CombinedOutput()
		cancel()
		require.NoError(t, err, "%s production process:\n%s", phase, out)
		require.Contains(t, string(out), "--- PASS: TestSessionSQLiteAliasChild")
		t.Logf("%s production process:\n%s", phase, out)
	}
}

func TestSessionSQLiteAliasChild(t *testing.T) {
	phase := os.Getenv("CRIER413_PHASE")
	if phase == "" {
		return
	}
	root := os.Getenv("CRIER413_ROOT")
	path := filepath.Join(root, "sessions.db")
	base := startTestServerWithEnv(t, map[string]string{
		"CR_REGISTRY_BACKEND": "memory", "CR_REGISTRY_SQLITE_PATH": "",
		"CR_SESSION_BACKEND": "sqlite", "CR_SESSION_SQLITE_PATH": path,
		"CR_SQLITE_PATH": filepath.Join(root, "wrong-"+phase+".db"),
		"CR_GROUP_ROOT":  filepath.Join(root, "groups"), "CR_REQUIRE_AGENT_SIG": "false",
		"CR_PIDFILE": "",
	})
	client := &http.Client{Timeout: 5 * time.Second}
	if phase == "seed" {
		code, raw := sessionDo(t, client, "POST", base+"/sessions", map[string]any{
			"title": "operator-selected-file", "created_by": map[string]string{"principal": "operator"},
		}, nil)
		require.Equal(t, http.StatusCreated, code, raw)
	}
	_, err := os.Stat(path)
	require.NoError(t, err, "session store must actually create the configured alias path %s", path)
	_, err = os.Stat(filepath.Join(root, "wrong-"+phase+".db"))
	require.True(t, os.IsNotExist(err), "legacy fallback must not be opened when alias is set")
	var posture map[string]any
	code, raw := sessionDo(t, client, "GET", base+"/status", nil, &posture)
	require.Equal(t, http.StatusOK, code, raw)
	require.Equal(t, path, posture["session_store_path"])
	t.Logf("/status session store: %s (%v)", posture["session_store_path"], posture["session_store_mount"])
	code, raw = sessionDo(t, client, "GET", base+"/sessions", nil, nil)
	require.Equal(t, http.StatusOK, code, raw)
	require.Contains(t, raw, "operator-selected-file", "the same session must remain listed after process replacement")
}
