package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

func TestSessionMountKind(t *testing.T) {
	root := "20 1 0:20 / / rw - overlay overlay rw\n"
	for _, tc := range []struct{ name, path, mounts, want string }{
		{"root layer", "/sessions.db", root, "overlay-layer"},
		{"volume", "/data/sessions.db", root + "21 20 8:1 /vol /data rw - ext4 /dev/sda rw\n", "mounted-filesystem"},
		{"mounted file", "/data/sessions.db", root + "21 20 8:1 /vol/file /data/sessions.db rw - ext4 /dev/sda rw\n", "mounted-filesystem"},
		{"prefix boundary", "/database/sessions.db", root + "21 20 8:1 /vol /data rw - ext4 /dev/sda rw\n", "overlay-layer"},
		{"nested volatile mount", "/data/tmp/sessions.db", root + "22 21 0:21 / /data/tmp rw - tmpfs tmpfs rw\n21 20 8:1 /vol /data rw - ext4 /dev/sda rw\n", "memory"},
		{"escaped mount", "/session data/sessions.db", root + "21 20 8:1 /vol /session\\040data rw shared:1 - ext4 /dev/sda rw\n", "mounted-filesystem"},
		{"non-overlay root cannot prove container", "/data/sessions.db", "20 1 8:1 / / rw - ext4 /dev/sda rw\n", "unknown"},
		{"root memory", "/sessions.db", "20 1 0:1 / / rw - ramfs ramfs rw\n", "memory"},
		{"no matching mount", "/sessions.db", "20 1 8:1 / /elsewhere rw - ext4 /dev/sda rw\n", "unknown"},
		{"stacked mount is ambiguous", "/data/sessions.db", root + "21 20 8:1 /vol /data rw - ext4 /dev/sda rw\n22 21 0:21 / /data rw - tmpfs tmpfs rw\n", "unknown"},
		{"malformed", "/sessions.db", "garbage\n20 1 8:1 / / rw -\n", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, sessionMountKind(tc.path, tc.mounts)) })
	}
}

func TestSessionStoreLocation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sessions.db")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	link := filepath.Join(root, "alias.db")
	require.NoError(t, os.Symlink(path, link))
	cwd, err := os.Getwd()
	require.NoError(t, err)
	relative, err := filepath.Rel(cwd, path)
	require.NoError(t, err)
	for _, input := range []string{path, link, relative, "file:" + path + "?cache=shared"} {
		cfg := config.Config{Session: config.SessionConfig{Backend: "sqlite", SQLitePath: input}}
		got, _ := sessionStoreLocation(cfg)
		require.NotNil(t, got)
		require.Equal(t, path, *got, "resolve actual file, not URI options or symlink spelling")
	}
	for _, input := range []string{":memory:", "file::memory:?cache=shared", "file:volatile?mode=memory&cache=shared"} {
		got, kind := sessionStoreLocation(config.Config{Session: config.SessionConfig{Backend: "sqlite", SQLitePath: input}})
		require.Equal(t, ":memory:", *got)
		require.Equal(t, "memory", kind)
	}
	for _, input := range []string{"file:?cache=shared", "file://other-host/sessions.db", "file:%ZZ"} {
		got, kind := sessionStoreLocation(config.Config{Session: config.SessionConfig{Backend: "sqlite", SQLitePath: input}})
		require.Nil(t, got)
		require.Equal(t, "unknown", kind)
	}
	got, kind := sessionStoreLocation(config.Config{Session: config.SessionConfig{Backend: "postgres"}, Database: config.DatabaseConfig{URL: "postgres://user:secret@host/db"}})
	require.Nil(t, got, "never expose a credential-bearing database URL")
	require.Equal(t, "external", kind)
	got, _ = sessionStoreLocation(config.Config{Session: config.SessionConfig{Backend: "jsonl", LogRoot: root}})
	require.Equal(t, root, *got)
	got, kind = sessionStoreLocation(config.Config{Session: config.SessionConfig{Backend: "sqlite", SQLitePath: filepath.Join(root, "missing.db")}})
	require.Equal(t, filepath.Join(root, "missing.db"), *got)
	require.Equal(t, "unknown", kind, "a missing file cannot supply mount evidence")
}

// Execute the shipped renderer and assert its sentence, not a source substring.
func TestSessionStorePostureRenderer(t *testing.T) {
	node, err := exec.LookPath("node")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "operator sessions.db")
	cfg := config.Config{Session: config.SessionConfig{Backend: "sqlite", SQLitePath: path}}
	body := httptestRecorder(t, cfg, "memory")
	var live map[string]any
	require.NoError(t, json.Unmarshal(body, &live))
	source := string(chatClientHTML)
	a, b := strings.Index(source, "function renderPosture("), strings.Index(source, "async function refresh()")
	require.Greater(t, b, a)
	cmd := exec.Command(node, "--input-type=commonjs")
	cmd.Stdin = strings.NewReader(`
const assert = require('node:assert/strict');
const lines = {};
const notice = (id, text) => { lines[id] = text; };
` + source[a:b] + "\nconst live = " + string(body) + `;
renderPosture(live);
assert.ok(lines.posture.includes('Session store: sqlite '+live.session_store_path+' ('), lines.posture);
const cases = {
  'mounted-filesystem':'volume/mount observed — persistence not verified',
  'overlay-layer':'overlay writable layer — mount this path to retain sessions on container replacement',
  'memory':'memory-backed — not persistent storage',
  'external':'external database — storage location not observable',
  'unknown':'volume vs writable layer unknown — verify the storage mount'
};
for (const [mount, words] of Object.entries(cases)) {
  renderPosture({...live,session_store_mount:mount});
  assert.ok(lines.posture.includes('Session store: sqlite '+live.session_store_path+' ('+words+')'),lines.posture);
}
renderPosture({});
assert.ok(lines.posture.includes('volume vs writable layer unknown'));
console.log('session path rendered PASS');
`)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "session path rendered PASS")
}
