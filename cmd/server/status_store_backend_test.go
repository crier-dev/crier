// status_store_backend_test.go — DF-CRIER-428: a SQLite path is not a backend
// selection, and /status must never let one read as the store in use.
//
// The measured shape: a container booted with CR_SESSION_SQLITE_PATH=/data/sessions.db
// and no CR_SESSION_BACKEND logged `backend=jsonl` while nothing anywhere said
// the file was ignored; CR_REGISTRY_SQLITE_PATH alone left the in-memory
// (DEMO-ONLY) registry serving. The path is legitimate to set — a default must
// not silently relocate existing sessions — so the fix is to NAME the mismatch,
// beside the path and backend actually in use, and to name it in the boot log
// too. These tests pin both postures:
//
//  1. matching — the path is the live store, nothing is named as ignored;
//  2. mismatched — the ignored key is named, the reported path is the one the
//     store really uses, and (live) the named file is never created.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

// matchingStorePostureConfig is the durable posture: both backends explicitly
// sqlite, both files present. The session path is spelled as a file: URI on
// purpose — /status must report the resolved file, not the DSN.
func matchingStorePostureConfig(t *testing.T, root string) (config.Config, string, string) {
	t.Helper()
	sessionPath := filepath.Join(root, "sessions.db")
	registryPath := filepath.Join(root, "registry.db")
	require.NoError(t, os.WriteFile(sessionPath, nil, 0o600))
	require.NoError(t, os.WriteFile(registryPath, nil, 0o600))
	return config.Config{
		Session: config.SessionConfig{
			Backend:          config.SessionBackendSQLite,
			SQLitePath:       "file:" + sessionPath + "?cache=shared",
			SQLitePathSource: "CR_SESSION_SQLITE_PATH",
			LogRoot:          filepath.Join(root, "sessions-log"),
		},
		Registry: config.RegistryConfig{
			Backend:          "sqlite",
			SQLitePath:       registryPath,
			SQLitePathSource: "CR_REGISTRY_SQLITE_PATH",
		},
	}, sessionPath, registryPath
}

func statusBody(t *testing.T, cfg config.Config, registryBackend string) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(httptestRecorder(t, cfg, registryBackend), &body))
	return body
}

// Posture 1: path WITH its matching backend — the path is the live store, it
// reads durable, and nothing is named as ignored.
func TestStatusStorePostureMatchingBackendReadsDurable(t *testing.T) {
	root := t.TempDir()
	cfg, sessionPath, registryPath := matchingStorePostureConfig(t, root)
	body := statusBody(t, cfg, "sqlite")

	require.Equal(t, "sqlite", body["session_backend"])
	require.Equal(t, sessionPath, body["session_store_path"],
		"the sqlite backend must name the resolved file it opened")
	require.Equal(t, "", body["session_store_ignored"],
		"a path the resolved backend opens is not ignored")

	require.Equal(t, "sqlite", body["registry_backend"])
	require.Equal(t, registryPath, body["registry_store_path"],
		"registry_backend must be reported beside the durable file it opened")
	require.Equal(t, "", body["registry_store_ignored"])
	require.Equal(t, true, body["registry_durable"])
}

// Posture 2: path WITHOUT its backend — the resolved backend is jsonl/memory,
// so /status must name the ignored key and must name the store actually in use,
// never the file no process opens.
func TestStatusStorePostureMismatchedBackendNamesTheIgnore(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "sessions.db")
	registryPath := filepath.Join(root, "registry.db")
	logRoot := filepath.Join(root, "sessions-log")
	// Exactly the measured deployment: only the two path keys are set. No
	// database URL and no backend, so the session store stays jsonl and the
	// registry stays the in-memory store.
	cfg := config.Config{
		Session: config.SessionConfig{
			SQLitePath:       sessionPath,
			SQLitePathSource: "CR_SESSION_SQLITE_PATH",
			LogRoot:          logRoot,
		},
		Registry: config.RegistryConfig{
			SQLitePath:       registryPath,
			SQLitePathSource: "CR_REGISTRY_SQLITE_PATH",
		},
	}
	body := statusBody(t, cfg, "memory")

	require.Equal(t, "jsonl", body["session_backend"])
	require.Equal(t, logRoot, body["session_store_path"],
		"session_store_path must name the jsonl directory the process really writes")
	require.NotEqual(t, sessionPath, body["session_store_path"],
		"the sqlite file no backend opened must never be reported as the session path")
	require.Equal(t, "ignored: CR_SESSION_SQLITE_PATH (backend is jsonl)", body["session_store_ignored"],
		"the ignored session key must be named beside the path in use, not silently dropped")

	require.Equal(t, "memory", body["registry_backend"])
	require.Nil(t, body["registry_store_path"],
		"the in-memory registry has no file; reporting the ignored one would imply durability")
	require.Equal(t, "ignored: CR_REGISTRY_SQLITE_PATH (backend is memory)", body["registry_store_ignored"])
	require.Equal(t, false, body["registry_durable"],
		"a named-but-ignored path must never read durable")
}

// The legacy alias is named as ITSELF: telling an operator to unset
// CR_SESSION_SQLITE_PATH when they set CR_SQLITE_PATH sends them to a variable
// that is not in their deployment.
func TestStatusStorePostureNamesTheLegacyAliasItIgnores(t *testing.T) {
	cfg := config.Config{
		Session: config.SessionConfig{SQLitePath: "/data/sessions.db", SQLitePathSource: "CR_SQLITE_PATH"},
		Registry: config.RegistryConfig{
			SQLitePath:       "/data/registry.db",
			SQLitePathSource: "CR_REGISTRY_SQLITE_PATH",
		},
	}
	body := statusBody(t, cfg, "memory")
	require.Equal(t, "ignored: CR_SQLITE_PATH (backend is jsonl)", body["session_store_ignored"])
	require.Equal(t, "ignored: CR_REGISTRY_SQLITE_PATH (backend is memory)", body["registry_store_ignored"])
}

// Live proof, not a hand-built config: boot run() with ONLY the path keys set,
// create a session, and require that (a) /status names the mismatch, (b) the
// ignored files were never created, and (c) the session really landed in the
// jsonl store. This is the "never reads durable" half of the acceptance
// criteria — a store that is not opened cannot have written the file.
func TestMismatchedStorePostureLiveNeverOpensTheNamedFiles(t *testing.T) {
	parallelBoot(t)
	root := t.TempDir()
	sessionPath := filepath.Join(root, "sessions.db")
	registryPath := filepath.Join(root, "registry.db")
	logRoot := filepath.Join(root, "sessions-log")
	base := startTestServerWithEnv(t, map[string]string{
		// The measured deployment: paths set, backends unset.
		"CR_SESSION_SQLITE_PATH":  sessionPath,
		"CR_SESSION_BACKEND":      "",
		"CR_SQLITE_PATH":          "",
		"CR_REGISTRY_SQLITE_PATH": registryPath,
		"CR_REGISTRY_BACKEND":     "",
		// Keep the jsonl store and the group roster inside the fixture.
		"CR_SESSION_LOG_ROOT":  logRoot,
		"CR_GROUP_ROOT":        filepath.Join(root, "groups"),
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_PIDFILE":           "",
	})
	client := &http.Client{Timeout: 5 * time.Second}

	var posture map[string]any
	code, raw := sessionDo(t, client, "GET", base+"/status", nil, &posture)
	require.Equal(t, http.StatusOK, code, raw)
	require.Equal(t, "jsonl", posture["session_backend"])
	require.Equal(t, "memory", posture["registry_backend"])
	require.Equal(t, "ignored: CR_SESSION_SQLITE_PATH (backend is jsonl)", posture["session_store_ignored"])
	require.Equal(t, "ignored: CR_REGISTRY_SQLITE_PATH (backend is memory)", posture["registry_store_ignored"])
	require.NotEqual(t, sessionPath, posture["session_store_path"])
	require.Nil(t, posture["registry_store_path"])

	code, raw = sessionDo(t, client, "POST", base+"/sessions", map[string]any{
		"title":      "mismatch-posture",
		"created_by": map[string]string{"principal": "operator"},
	}, nil)
	require.Equal(t, http.StatusCreated, code, raw)

	_, err := os.Stat(sessionPath)
	require.True(t, os.IsNotExist(err), "the ignored session file %s must never be created", sessionPath)
	_, err = os.Stat(registryPath)
	require.True(t, os.IsNotExist(err), "the ignored registry file %s must never be created", registryPath)

	entries, err := os.ReadDir(logRoot)
	require.NoError(t, err, "the jsonl store named by /status must exist")
	require.NotEmpty(t, entries, "the created session must have been written to the jsonl store /status named")
}
