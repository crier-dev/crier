package main

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// DF-CRIER-459: run the same selectable-file harness used for RED evidence in CI.
func TestChatRefusalTruth(t *testing.T) {
	node, err := exec.LookPath("node")
	require.NoError(t, err, "node is required to exercise the shipped client")
	out, err := exec.Command(node, filepath.Join("..", "..", "scripts", "chat-refusal-truth.cjs")).CombinedOutput()
	require.NoError(t, err, "%s", out)
	t.Log(string(out))
}

// DF-CRIER-458: fetch the real protected page through the production server,
// extract its declared public paths, and verify each one without a credential.
// This tests both the prose's scope and the actual middleware/route composition.
func TestBrowserRefusalPublicExceptions(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN":              df444Token,
		"CR_PERMISSIONS_ENABLED":     "false",
		"CR_PERMISSIONS_ADMIN_TOKEN": "",
	})
	status, ct, body := df444Get(t, base+"/chat", "text/html")
	require.Equal(t, http.StatusUnauthorized, status)
	require.Contains(t, ct, "text/html")
	require.Contains(t, body, "every request except the public paths listed below must carry a <code>Bearer</code> credential")
	require.NotContains(t, body, "every request must carry")
	paths := regexp.MustCompile(`<code>(/[^<]+)</code>`).FindAllStringSubmatch(body, -1)
	var named []string
	for _, match := range paths {
		named = append(named, match[1])
	}
	require.Equal(t, []string{"/health", "/version", "/openapi.json", "/openapi.yaml", "/docs"}, named)
	t.Log("GET /chat without token: 401; Bearer rule explicitly excludes the listed public paths")
	for _, path := range named {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			status, _, body := df444Get(t, base+path, "")
			require.Equal(t, http.StatusOK, status, "%s: %s", path, body)
			require.NotEmpty(t, body)
			t.Logf("GET %s without token: %d", path, status)
		})
	}
}
