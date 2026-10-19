package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

// Exercise the production router/config path, including both partially armed
// configurations: neither switch nor token alone enables management authority.
func TestStatusManagementSurface(t *testing.T) {
	const adminToken = "management-posture-test-only-secret"
	for _, tc := range []struct {
		name, enabled, token, want string
	}{
		{"disabled", "false", "", "disabled"},
		{"token-only", "false", adminToken, "disabled"},
		{"enabled-without-token", "true", "", "disabled"},
		{"admin-gated", "true", adminToken, "admin-token-gated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := bootStatusServer(t, map[string]string{
				"CR_PERMISSIONS_ENABLED":     tc.enabled,
				"CR_PERMISSIONS_ADMIN_TOKEN": tc.token,
				"CR_PERMISSIONS_DIR":         filepath.Join(t.TempDir(), "permissions"),
			})
			code, _, raw := getStatus(t, base, "")
			require.Equal(t, 200, code)
			body := assertStatusSchema(t, raw)
			require.Equal(t, tc.want, body["management_surface"])
			require.NotContains(t, string(raw), adminToken)
			require.NotRegexp(t, regexp.MustCompile(`(?i)[0-9a-f]{64}`), string(raw))
			t.Logf("GET /status (%s): %s", tc.name, raw)
		})
	}
}

// Execute the actual embedded renderer with actual /status handler bodies,
// not a reimplementation of either the backend predicate or the UI label.
func TestChatManagementPosture(t *testing.T) {
	node, err := exec.LookPath("node")
	require.NoError(t, err, "node is required to exercise the embedded renderer")
	source := string(chatClientHTML)
	start := strings.Index(source, "function renderPosture(")
	end := strings.Index(source, "async function refresh()")
	require.GreaterOrEqual(t, start, 0)
	require.Greater(t, end, start)
	for _, enabled := range []bool{false, true} {
		cfg := config.Config{Session: config.SessionConfig{Backend: "sqlite"}, Permissions: config.PermissionsConfig{Enabled: enabled, AdminToken: "management-posture-test-only-secret"}}
		body := httptestRecorder(t, cfg, "sqlite")
		want := "disabled"
		if enabled {
			want = "admin-token gated"
		}
		t.Run(want, func(t *testing.T) {
			encodedWant, err := json.Marshal("Management: " + want)
			require.NoError(t, err)
			cmd := exec.Command(node, "--input-type=commonjs")
			cmd.Stdin = strings.NewReader(`
const assert = require('node:assert/strict');
const notices = new Map();
const notice = (id, text) => notices.set(id, text);
` + source[start:end] + `
renderPosture(` + string(body) + `);
const line = notices.get('posture');
assert.ok(line.includes(` + string(encodedWant) + `), 'posture must state '+` + string(encodedWant) + `+'; got: '+line);
assert.ok(!line.includes('management-posture-test-only-secret'));
assert.doesNotMatch(line, /[0-9a-f]{64}/i);
console.log(line);
renderPosture({});
assert.ok(notices.get('posture').includes('Management: unknown'), 'missing posture must remain unknown, not disabled');
`)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			t.Logf("%s", out)
		})
	}
}
