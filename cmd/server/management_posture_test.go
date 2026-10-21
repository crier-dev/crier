package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crier-dev/crier/config"
	"github.com/stretchr/testify/require"
)

const managementTestToken = "management-posture-test-only-secret"

const managementDisabledSentence = "disabled — delivery ACL off (CR_PERMISSIONS_ENABLED=false). Not armed on this instance — a configured posture, not a failure. Management arming requires CR_PERMISSIONS_ADMIN_TOKEN; its configuration is not reported here."
const managementUnarmedSentence = "enabled — delivery ACL not armed. Configuration reported by /status; read results are shown per route."
const managementGatedSentence = "enabled and admin-token gated — management gate present. Configuration reported by /status; read results are shown per route."

// Exercise the production config, router, admin gate and embedded renderer.
// Every posture comes from /status and is checked against actual GET reads.
func TestStatusManagementSurface(t *testing.T) {
	var lines []string
	for _, tc := range []struct {
		name, enabled, token, bearer, reach, want, sentence string
		readCode                                            int
	}{
		{"disabled", "false", "", "", "", "disabled", managementDisabledSentence, 403},
		{"token-only", "false", managementTestToken, managementTestToken, "", "disabled", managementDisabledSentence, 403},
		{"enabled-without-token", "true", "", "", "", "enabled-unarmed", managementUnarmedSentence, 403},
		{"enabled-token-not-supplied", "true", managementTestToken, "", "", "enabled-unarmed", managementUnarmedSentence, 403},
		{"enabled-wrong-token", "true", managementTestToken, "wrong-viewer-token", "", "enabled-unarmed", managementUnarmedSentence, 403},
		{"admin-gated", "true", managementTestToken, managementTestToken, "", "admin-token-gated", managementGatedSentence, 200},
		{"reach-is-not-admin", "true", managementTestToken, "ordinary-reach-token", "ordinary-reach-token", "enabled-unarmed", managementUnarmedSentence, 403},
		{"admin-with-reach-gate", "true", managementTestToken, managementTestToken, "ordinary-reach-token", "admin-token-gated", managementGatedSentence, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := bootStatusServer(t, map[string]string{
				"CR_AUTH_TOKEN":              tc.reach,
				"CR_PERMISSIONS_ENABLED":     tc.enabled,
				"CR_PERMISSIONS_ADMIN_TOKEN": tc.token,
				"CR_PERMISSIONS_DIR":         filepath.Join(t.TempDir(), "permissions"),
			})
			code, _, raw := getStatus(t, base, tc.bearer)
			require.Equal(t, http.StatusOK, code, "%s", raw)
			body := assertStatusSchema(t, raw)
			require.Equal(t, tc.want, body["management_surface"])
			require.NotContains(t, string(raw), managementTestToken)
			require.NotRegexp(t, regexp.MustCompile(`(?i)[0-9a-f]{64}`), string(raw))
			for _, route := range []string{"/principals", "/bindings", "/grants"} {
				status, read := df297Request(t, http.DefaultClient, http.MethodGet, base+route, tc.bearer, "", "")
				require.Equal(t, tc.readCode, status, "%s: %s", route, read)
				if tc.enabled == "true" && tc.readCode == 403 {
					require.Contains(t, read, "CR_PERMISSIONS_ADMIN_TOKEN")
				}
			}
			line := renderManagementPosture(t, raw, tc.sentence)
			lines = append(lines, tc.name+": "+line)
		})
	}
	t.Logf("Management postures side by side:\n%s", strings.Join(lines, "\n"))
}

// Keep both original wire values readable by newer clients; missing and future
// values are unknown, never silently disabled. This is compatibility input only,
// not product data or a replacement for the live configuration table above.
func TestChatManagementPosture(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"disabled", managementDisabledSentence},
		{"enabled-unarmed", managementUnarmedSentence},
		{"admin-token-gated", managementGatedSentence},
		{"", "unknown"},
		{"future-posture", "unknown"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"management_surface": tc.value})
			require.NoError(t, err)
			renderManagementPosture(t, body, tc.want)
		})
	}
}

func TestManagementPostureUnwiredIsNotAuthorizedOrCacheable(t *testing.T) {
	cfg := config.Config{Permissions: config.PermissionsConfig{Enabled: true, AdminToken: managementTestToken}}
	request := httptest.NewRequest(http.MethodGet, "/status", nil)
	request.Header.Set("Authorization", "Bearer "+managementTestToken)
	response := httptest.NewRecorder()
	// No live registry gate was wired: even the correct configured token must
	// not manufacture read authority in this configuration-only handler.
	newStatusHandler(cfg, "memory")(response, request)
	body := assertStatusSchema(t, response.Body.Bytes())
	require.Equal(t, "enabled-unarmed", body["management_surface"])
	require.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	require.Contains(t, response.Header().Values("Vary"), "Authorization")
}

func renderManagementPosture(t *testing.T, body []byte, want string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	require.NoError(t, err, "node is required to exercise the embedded renderer")
	source := string(chatClientHTML)
	start := strings.Index(source, "function renderPosture(")
	end := strings.Index(source, "async function refresh()")
	require.GreaterOrEqual(t, start, 0)
	require.Greater(t, end, start)
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
assert.doesNotMatch(line, /refused|succeeded|authorized for this viewer/i, 'posture must state configuration only');
assert.ok(line.includes(` + string(encodedWant) + `), 'posture must state '+` + string(encodedWant) + `+'; got: '+line);
assert.ok(!line.includes('management-posture-test-only-secret'));
assert.doesNotMatch(line, /[0-9a-f]{64}/i);
console.log(line.split(' · Management: ')[1].split(' · Federation: ')[0]);
renderPosture({});
assert.ok(notices.get('posture').includes('Management: unknown'), 'missing posture must remain unknown, not disabled');
`)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	line := strings.TrimSpace(string(out))
	require.Equal(t, want, line, fmt.Sprintf("rendered /status: %s", body))
	return line
}
